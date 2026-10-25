// Package panel 定义驱动层共用的「面板驱动」契约与能力常量。
//
// 对应 PHP 侧 backend/app/Drivers/{Capability.php,Contracts/DriverInterface.php,
// Contracts/PanelDriverInterface.php}：业务层只判断能力，不判断驱动类型；
// 每个方法对应一个 Capability，调用方先 Supports() 再调。
//
// 放在 internal/driver/internal/ 下：只有 driver 下的包能引用，
// 避免把驱动契约散布到 handler / collector 之外的层次。
package panel

import "context"

// 能力常量（与 PHP Capability enum 的 value 逐字一致）。
const (
	CapClientCreate      = "client.create"
	CapClientRead        = "client.read"
	CapClientUpdate      = "client.update"
	CapClientDelete      = "client.delete"
	CapClientList        = "client.list"
	CapClientToggle      = "client.toggle"
	CapTrafficSync       = "traffic.sync"
	CapTrafficReset      = "traffic.reset"
	CapInboundList       = "inbound.list"
	CapInboundGet        = "inbound.get"
	CapProtocolAttach    = "protocol.attach"
	CapProtocolDetach    = "protocol.detach"
	CapSubscriptionLinks = "subscription.links"
	CapSubscriptionGen   = "subscription.generate"
	CapHealthCheck       = "health.check"
	CapPaymentPay        = "payment.pay"
	CapPaymentCallback   = "payment.callback"
	CapPaymentQuery      = "payment.query"
	CapPaymentRefund     = "payment.refund"
)

// ClientTraffic 是该节点上「一个 email」的累计流量（原始值，不含倍率）。
//
// 字段名与 3x-ui 上报口径一致（up/down）；落库时的 upload/download 由调用方映射
// （contracts/data-contract.md §1 的字段名陷阱）。
type ClientTraffic struct {
	Up     int64
	Down   int64
	Enable bool
}

// Health 是健康检查的归一化结果（对应 ThreeXUiClient::healthCheck() 的返回数组）。
//
// 失败分支同样带齐 cpu/mem/xrayState/netSent/netRecv 键（值为 nil）——
// 调用方按统一形状取值，不必判 isset；nil 表示「面板没上报」，采样层据此跳过，
// 绝不能写成 0（会被差分当成「累计归零」，凭空造出一段流量）。
type Health struct {
	OK        bool     `json:"ok"`
	LatencyMs int64    `json:"latencyMs"`
	CPU       any      `json:"cpu"`
	Mem       *float64 `json:"mem"`
	XrayState *string  `json:"xrayState"`
	NetSent   *int64   `json:"netSent"`
	NetRecv   *int64   `json:"netRecv"`
	Error     string   `json:"error,omitempty"`
}

// Driver 是面板驱动的 Go 侧契约（对齐 PanelDriverInterface + DriverInterface）。
//
// 与 PHP 的差异（有意，写在这里以免后续会话再纠结）：
//   - 全部方法带 ctx（Go 侧的超时取消）；
//   - 没有 async 变体：Go 侧用 goroutine + ctx 并发；PHP 的异步版只是绕开
//     Guzzle curl_multi 的串行 wait，Bearer 模式本就无状态；
//   - deleteClient 只保留 keepTraffic：PHP 的 inboundId 参数在 3x-ui 侧被忽略
//     （见 ThreeXUiClientTest::test_delete_client_ignores_unsupported_inbound_id_query）。
type Driver interface {
	Name() string
	Version() string
	Supports(capability string) bool
	Capabilities() []string

	// 客户管理
	ListClients(ctx context.Context) ([]map[string]any, error)
	GetClient(ctx context.Context, identifier string) (map[string]any, error)
	CreateClient(ctx context.Context, data map[string]any, inboundIDs []int64) (map[string]any, error)
	UpdateClient(ctx context.Context, identifier string, data map[string]any, inboundID *int64) error
	DeleteClient(ctx context.Context, identifier string, keepTraffic bool) error
	ToggleClient(ctx context.Context, identifier string, enable bool) error

	// 流量
	GetClientTraffic(ctx context.Context, identifier string) (map[string]any, error)
	ResetClientTraffic(ctx context.Context, identifier string) error

	// 入站
	ListInbounds(ctx context.Context) ([]map[string]any, error)
	GetInbound(ctx context.Context, id int64) (map[string]any, error)

	// 批量流量同步
	GetClientStatsByEmail(ctx context.Context) (map[string]ClientTraffic, error)
	GetClientStatsGroupedByInbound(ctx context.Context) (map[int64]map[string]ClientTraffic, error)

	// 订阅链接
	GetClientLinks(ctx context.Context, identifier string) ([]string, error)

	// 协议切换
	AttachClient(ctx context.Context, identifier string, inboundIDs []int64) error
	DetachClient(ctx context.Context, identifier string, inboundIDs []int64) error

	// 健康检查
	HealthCheck(ctx context.Context) (Health, error)
}
