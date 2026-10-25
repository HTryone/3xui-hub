package admin

// Webshare 上游 API 客户端（对齐 backend/app/Services/Webshare/WebshareClient.php）。
//
// 只做「一次请求 + 把失败讲清楚」，不含业务语义：
//   - 鉴权：Authorization: Token <api_key>
//   - 分页：/subuser/ 返回 {count,next,previous,results:[...]}，这里拆出 results
//   - 429 限流 / 403 未开通子用户权限 → 抛 WebshareError（带状态码）
//
// 文档：https://proxy.webshare.io/api/v2

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WebshareBaseURL 是上游 v2 地址。
const WebshareBaseURL = "https://proxy.webshare.io/api/v2"

const (
	webshareTimeout        = 15 * time.Second
	webshareConnectTimeout = 6 * time.Second
)

// WebshareError 对齐 WebshareException：带 HTTP 状态码（0 = 网络层失败）。
type WebshareError struct {
	Msg    string
	Status int
}

func (e *WebshareError) Error() string { return e.Msg }

// UpstreamClient 是访问 Webshare 的接缝（测试注入假实现，包内单测不发外网请求）。
type UpstreamClient interface {
	ListPlans(ctx context.Context, token string) ([]map[string]any, error)
	ListSubusers(ctx context.Context, token string) ([]map[string]any, error)
	ListProxies(ctx context.Context, token string, query map[string]string) (map[string]any, error)
	GetSubuser(ctx context.Context, token, upstreamID string) (map[string]any, error)
	CreateSubuser(ctx context.Context, token string, attrs map[string]any) (map[string]any, error)
	UpdateSubuser(ctx context.Context, token, upstreamID string, attrs map[string]any) (map[string]any, error)
	DeleteSubuser(ctx context.Context, token, upstreamID string) error
}

// HTTPWebshareClient 是 UpstreamClient 的真实实现。
type HTTPWebshareClient struct {
	// BaseURL 为空时用 WebshareBaseURL（自建反代可覆盖）。
	BaseURL string
	// Proxy 是出口代理（socks5h://host:port / http://host:port），空 = 直连。
	Proxy string
	// CABundle 是自定义 CA bundle 路径，空 = 系统 CA。
	CABundle string
	HTTP     *http.Client
}

// NewHTTPWebshareClient 按配置构造客户端。
//
// 代理配置对齐 config/panel.php 的 webshare 段：国内机房访问 proxy.webshare.io
// 通常需要出口代理；不走代理时直连。
func NewHTTPWebshareClient(cfg Config) *HTTPWebshareClient {
	transport := &http.Transport{
		ResponseHeaderTimeout: webshareTimeout,
		DisableKeepAlives:     false,
	}
	if cfg.WebshareProxy != "" {
		if u, err := url.Parse(cfg.WebshareProxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	// 自定义 CA bundle 走 TLSClientConfig（略：本机 .env 未配置时留空即系统 CA）。
	return &HTTPWebshareClient{
		Proxy:    cfg.WebshareProxy,
		CABundle: cfg.WebshareCABundle,
		HTTP: &http.Client{
			Transport: transport,
			Timeout:   webshareTimeout,
		},
	}
}

// ListPlans 对齐 WebshareClient::listPlans（拆 results）。
func (c *HTTPWebshareClient) ListPlans(ctx context.Context, token string) ([]map[string]any, error) {
	data, err := c.request(ctx, token, http.MethodGet, "/subscription/plan/", nil, nil)
	if err != nil {
		return nil, err
	}
	return resultsOf(data), nil
}

// ListSubusers 对齐 WebshareClient::listSubusers。
func (c *HTTPWebshareClient) ListSubusers(ctx context.Context, token string) ([]map[string]any, error) {
	data, err := c.request(ctx, token, http.MethodGet, "/subuser/", nil, nil)
	if err != nil {
		return nil, err
	}
	return resultsOf(data), nil
}

// ListProxies 对齐 WebshareClient::listProxies（query 为 RFC3986 编码）。
func (c *HTTPWebshareClient) ListProxies(ctx context.Context, token string, query map[string]string) (map[string]any, error) {
	return c.request(ctx, token, http.MethodGet, "/proxy/list/", query, nil)
}

// GetSubuser 对齐 WebshareClient::getSubuser（只读详情，不含凭据）。
func (c *HTTPWebshareClient) GetSubuser(ctx context.Context, token, upstreamID string) (map[string]any, error) {
	return c.request(ctx, token, http.MethodGet, "/subuser/"+url.PathEscape(upstreamID)+"/", nil, nil)
}

// CreateSubuser 对齐 WebshareClient::createSubuser。
func (c *HTTPWebshareClient) CreateSubuser(ctx context.Context, token string, attrs map[string]any) (map[string]any, error) {
	return c.request(ctx, token, http.MethodPost, "/subuser/", nil, attrs)
}

// UpdateSubuser 对齐 WebshareClient::updateSubuser（PATCH）。
func (c *HTTPWebshareClient) UpdateSubuser(ctx context.Context, token, upstreamID string, attrs map[string]any) (map[string]any, error) {
	return c.request(ctx, token, http.MethodPatch, "/subuser/"+url.PathEscape(upstreamID)+"/", nil, attrs)
}

// DeleteSubuser 对齐 WebshareClient::deleteSubuser（上游返回 204 无响应体）。
func (c *HTTPWebshareClient) DeleteSubuser(ctx context.Context, token, upstreamID string) error {
	_, err := c.request(ctx, token, http.MethodDelete, "/subuser/"+url.PathEscape(upstreamID)+"/", nil, nil)
	return err
}

// request 发一次请求并解包 JSON；非 2xx 一律转 WebshareError。
func (c *HTTPWebshareClient) request(ctx context.Context, token, method, path string, query map[string]string, payload map[string]any) (map[string]any, error) {
	base := c.BaseURL
	if base == "" {
		base = WebshareBaseURL
	}
	full := strings.TrimRight(base, "/") + path
	if len(query) > 0 {
		q := url.Values{}
		for k, v := range query {
			if v == "" {
				continue
			}
			q.Set(k, v)
		}
		if enc := q.Encode(); enc != "" {
			full += "?" + enc
		}
	}

	var body io.Reader
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			return nil, &WebshareError{Msg: "构造上游请求失败", Status: 0}
		}
		body = bytes.NewReader(raw)
	}

	reqCtx, cancel := context.WithTimeout(ctx, webshareTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, method, full, body)
	if err != nil {
		return nil, &WebshareError{Msg: "构造上游请求失败", Status: 0}
	}
	req.Header.Set("Authorization", "Token "+token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: webshareTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		// 网络层失败翻成管理员能看懂的话（对齐 PHP 的 ConnectionException 分支）。
		raw := err.Error()
		reason := "失败"
		if strings.Contains(raw, "timeout") || strings.Contains(raw, "deadline exceeded") {
			reason = "超时"
		}
		via := "当前是直连，国内网络通常出不去（可在 config/panel.php 配置 webshare.proxy）"
		if c.Proxy != "" {
			via = "出口代理 " + c.Proxy + " 不可用（代理客户端可能没连上节点 / 节点已失效）"
		}
		return nil, &WebshareError{Msg: "连接 Webshare " + reason + "：" + via + "。", Status: 0}
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, &WebshareError{Msg: "读取 Webshare 响应失败", Status: resp.StatusCode}
	}

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &WebshareError{Msg: "Webshare 请求过于频繁（429），请稍后再试", Status: 429}
	case resp.StatusCode == http.StatusForbidden:
		return nil, &WebshareError{
			Msg:    "Webshare 拒绝访问（403）：请在 proxy.webshare.io/subuser/ 开通子用户 API 权限",
			Status: 403,
		}
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return nil, &WebshareError{Msg: errorMessageOf(raw, resp.StatusCode), Status: resp.StatusCode}
	}

	if strings.TrimSpace(string(raw)) == "" {
		return map[string]any{}, nil
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, &WebshareError{Msg: "Webshare 返回了无法解析的响应", Status: resp.StatusCode}
	}
	return data, nil
}

// resultsOf 拆出分页信封里的 results（对齐 PHP 的 $data['results'] ?? $data）。
func resultsOf(data map[string]any) []map[string]any {
	raw, ok := data["results"]
	if !ok {
		// 上游直接返回单个对象时按一项处理（PHP 里 $data['results'] ?? $data 会得到整个对象）。
		return []map[string]any{data}
	}
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// errorMessageOf 对齐 WebshareClient::errorMessage + firstErrorText：
// 优先取 detail / message / non_field_errors / error / label 里第一段可读文本。
func errorMessageOf(raw []byte, status int) string {
	var body any
	if err := json.Unmarshal(raw, &body); err != nil {
		return "Webshare 请求失败（HTTP " + itoaInt(status) + "）"
	}
	if text := firstErrorText(body, 0); text != "" {
		return truncateRunes(text, 250)
	}
	return "Webshare 请求失败（HTTP " + itoaInt(status) + "）"
}

// firstErrorText 深度上限 3 层，防止异常结构把栈撑爆。
func firstErrorText(value any, depth int) string {
	if depth > 3 {
		return ""
	}
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	obj, ok := value.(map[string]any)
	if !ok {
		if list, isList := value.([]any); isList {
			parts := make([]string, 0, 3)
			for _, item := range list {
				if found := firstErrorText(item, depth+1); found != "" {
					parts = append(parts, found)
					if len(parts) >= 3 {
						break
					}
				}
			}
			return strings.Join(parts, "；")
		}
		return ""
	}

	for _, key := range []string{"detail", "message", "non_field_errors", "error", "label"} {
		if v, ok := obj[key]; ok {
			if found := firstErrorText(v, depth+1); found != "" {
				return found
			}
		}
	}

	parts := make([]string, 0, 3)
	for _, v := range obj {
		if found := firstErrorText(v, depth+1); found != "" {
			parts = append(parts, found)
			if len(parts) >= 3 {
				break
			}
		}
	}
	return strings.Join(parts, "；")
}

// truncateRunes 按字符截断（对齐 mb_substr）。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// itoaInt 是 int → 十进制。
func itoaInt(v int) string { return fmt.Sprintf("%d", v) }
