// Package payment 是支付网关驱动层（backend/app/Drivers/Payments/ 的 Go 复刻）。
//
// 一个驱动 = 一套网关协议：下单拿支付链接、回调验签、查单、应答文本。
// 驱动本身**不碰数据库、不碰 session、不判业务状态**（金额是否匹配、订单该不该置 paid
// 都由上层 PaymentService 决定），这样每个协议都能用 httptest 单独跑通。
//
// 金额一律用 decimal(10,2) 的字符串表示（contracts/data-contract.md §3），
// 需要运算时转成分（见 money.go），全程不出现 float64。
package payment

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DriverType 是 payment_configs.driver_type 的取值。
//
// 注意 wwspay 是**网关品牌**，协议实现与 payindex 完全相同：老数据里 driver_type
// 的列默认值就是 'wwspay'（见迁移 2026_06_28_000200），线上存量行大量是它。
// PHP 的 PaymentService::driverFor 对「空 / null / 未注册」一律回退 payindex，
// 这里用同一套回退，保证两边对同一行配置的行为一致。
const (
	DriverPayindex = "payindex"
	DriverEpayV1   = "epay_v1"
)

// Config 是 payment_configs 的一行（驱动需要的列）。
//
// DriverConfig 对应 driver_config JSON（新格式，优先级最高）；Gateway/QueryGateway/
// MemberID/APIKey/BankCode 是旧列（老数据，driver_config 为空时零影响）。
type Config struct {
	ID           uint64
	Name         string
	DriverType   string
	DriverConfig map[string]string

	Gateway      string
	QueryGateway string
	MemberID     string
	APIKey       string
	BankCode     string

	// NotifyURL 是库里的 notify_url 列原值（可能是空串或非 http 开头，见 NotifyURL()）。
	NotifyURL string
}

// legacyColumns 是允许回落到旧列的键（与 PHP AbstractPaymentDriver::LEGACY_COLUMNS 一致）。
var legacyColumns = map[string]bool{
	"gateway": true, "query_gateway": true, "member_id": true, "api_key": true, "bank_code": true,
}

// Cfg 读取配置项：driver_config 优先（非空才算），空则回退旧列，再回退默认值。
func (c Config) Cfg(key, def string) string {
	if v, ok := c.DriverConfig[key]; ok && v != "" {
		return v
	}
	if legacyColumns[key] {
		if v := c.legacy(key); v != "" {
			return v
		}
	}
	return def
}

func (c Config) legacy(key string) string {
	switch key {
	case "gateway":
		return c.Gateway
	case "query_gateway":
		return c.QueryGateway
	case "member_id":
		return c.MemberID
	case "api_key":
		return c.APIKey
	case "bank_code":
		return c.BankCode
	}
	return ""
}

// Order 是驱动下单所需的最小订单视图。
type Order struct {
	OrderNo     string
	Amount      string // decimal(10,2) 字符串，如 "98.50"
	ProductName string // 已拼好的商品名（「套餐购买-月付」）
	PayIP       string // 下单请求的客户端 IP（付款人 IP，网关风控用）
}

// PayRequest 是一次下单的完整入参。
//
// NotifyURL / ReturnURL 由上层解析后传入：驱动层不查 domains 表，也不关心当前请求
// 走的是哪个域名 —— 那是 HTTP 层的事。
type PayRequest struct {
	Order     Order
	NotifyURL string // 服务端异步通知地址（绝对 URL）
	ReturnURL string // 页面跳转回跳地址（当前请求的 scheme://host/）
}

// PayResult 是下单结果，语义与 PHP PaymentResult 一一对应：
//
//	Success          → PayURL 有值
//	!Success, Err==nil → 网关明确拒绝，FailMsg 是原因（调用方可据此作废被判重的旧单号）
//	!Success, Err!=nil → 网络/解析异常（临时故障，调用方**不得**作废订单：用户可能已在支付）
type PayResult struct {
	Success bool
	PayURL  string
	FailMsg string
	Err     error
}

// CallbackResult 是回调验签结果。Amount 保持网关给的原始字符串，由上层按分比较。
type CallbackResult struct {
	Success bool
	OrderNo string
	TradeNo string
	Amount  string
	Err     string
}

// QueryResult 是查单结果。Status 只取 "paid" / "pending"（对齐 PHP 的两态）。
type QueryResult struct {
	Status     string
	TradeState string
	TradeNo    string
}

// Definition 是协议的静态描述（后台动态表单用，对应各 Driver::definition()）。
type Definition struct {
	Key           string
	Label         string
	Description   string
	SupportsQuery bool
	Fields        []FieldDef
}

// FieldDef 是一个配置字段的表单描述。
type FieldDef struct {
	Name        string
	Label       string
	Placeholder string
	Required    bool
	Secret      bool
}

// Driver 是一套网关协议。
type Driver interface {
	Name() string
	SupportsQuery() bool

	// Pay 下单。返回的错误只表示「调用方需要知道的技术故障」，网关拒绝走 FailMsg。
	Pay(ctx context.Context, req PayRequest, cfg Config) PayResult

	// VerifySignature 校验回调签名。
	VerifySignature(data map[string]string, cfg Config) bool

	// HandleCallback 验签 + 判状态（金额比对由上层统一做）。
	HandleCallback(data map[string]string, cfg Config) CallbackResult

	// Query 查单。不支持 / 查询失败返回 nil（上层据此保持订单状态不变）。
	Query(ctx context.Context, order Order, cfg Config) *QueryResult

	// NotifyResponse 是回调应答文本（网关要求各不相同）。
	NotifyResponse(success bool) string

	Definition() Definition
}

// Registry 是驱动注册中心（对齐 PHP DriverRegistry，但只含支付域）。
type Registry struct {
	drivers map[string]Driver
}

// NewRegistry 注册内置协议。
func NewRegistry() *Registry {
	r := &Registry{drivers: map[string]Driver{}}
	r.Register(&PayindexDriver{})
	r.Register(&EpayV1Driver{})
	return r
}

// Register 注册一个驱动（同名覆盖）。
func (r *Registry) Register(d Driver) { r.drivers[d.Name()] = d }

// Has 判断某协议是否已注册。
func (r *Registry) Has(name string) bool { _, ok := r.drivers[name]; return ok }

// For 取配置对应的驱动：driver_type 为空 / 未注册 → 回退 payindex（与 PHP 逐字一致）。
func (r *Registry) For(cfg Config) Driver {
	if t := cfg.DriverType; t != "" {
		if d, ok := r.drivers[t]; ok {
			return d
		}
	}
	return r.drivers[DriverPayindex]
}

// ErrUnknownDriver 保留给显式查询未注册协议的场景（For 本身是回退语义，不返回错误）。
var ErrUnknownDriver = errors.New("payment: 未注册的支付协议")

// Get 按名字取驱动（后台协议清单等场景用），未注册即报错。
func (r *Registry) Get(name string) (Driver, error) {
	if d, ok := r.drivers[name]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrUnknownDriver, name)
}

// Definitions 按注册顺序输出协议清单。顺序固定（payindex 在前），前端列表才稳定。
func (r *Registry) Definitions() []Definition {
	return []Definition{
		r.drivers[DriverPayindex].Definition(),
		r.drivers[DriverEpayV1].Definition(),
	}
}

// HTTPDoer 是驱动的 HTTP 依赖（测试注入 httptest 客户端）。
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// DefaultTimeout 对齐 PHP 侧的 ->timeout(10)。
const DefaultTimeout = 10 * time.Second

// postForm 以表单 POST 发送，返回响应体字节。
//
// 参数用有序切片（不是 url.Values）：签名串与请求体的键序都要与 PHP 一致，
// 而且网关侧的「按 key 排序后校验」实现细节各不相同，保持原序最稳。
func postForm(ctx context.Context, client HTTPDoer, url string, pairs [][2]string) ([]byte, error) {
	form := make([]byte, 0, 128)
	for i, p := range pairs {
		if i > 0 {
			form = append(form, '&')
		}
		form = append(form, phpURLEncode(p[0])...)
		form = append(form, '=')
		form = append(form, phpURLEncode(p[1])...)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(form))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// 网关响应是 JSON 或短文本；1 MiB 上限防止异常网关把内存打满。
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
