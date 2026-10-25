package threexui

import (
	"context"
	"fmt"

	"controlhub-server/internal/driver/internal/panel"
)

// Driver 把 Client 包进 panel.Driver（对齐 backend/app/Drivers/ThreeXUi/ThreeXUiDriver.php）。
//
// 业务层不直接依赖 *Client：能力判定走 Supports()，具体实现可替换。
type Driver struct {
	client *Client
}

// capabilityList 与 PHP ThreeXUiDriver::CAPABILITIES 逐条一致、顺序也一致
// （capabilities() 的返回值会被管理端直接展示）。
var capabilityList = []string{
	panel.CapClientCreate,
	panel.CapClientRead,
	panel.CapClientUpdate,
	panel.CapClientDelete,
	panel.CapClientList,
	panel.CapClientToggle,
	panel.CapTrafficSync,
	panel.CapTrafficReset,
	panel.CapInboundList,
	panel.CapInboundGet,
	panel.CapProtocolAttach,
	panel.CapProtocolDetach,
	panel.CapSubscriptionLinks,
	panel.CapHealthCheck,
}

var capabilitySet = func() map[string]bool {
	m := make(map[string]bool, len(capabilityList))
	for _, c := range capabilityList {
		m[c] = true
	}
	return m
}()

// NewDriver 由配置构造驱动。
func NewDriver(cfg Config) *Driver { return &Driver{client: New(cfg)} }

// NewDriverWithClient 注入已构造好的 Client（测试 / 复用连接池时用）。
func NewDriverWithClient(c *Client) *Driver { return &Driver{client: c} }

// Client 返回底层 HTTP 客户端（诊断 / 直接调面板方法时用）。
func (d *Driver) Client() *Client { return d.client }

// Name 对应 name()。
func (d *Driver) Name() string { return "3x-ui" }

// Version 对应 version()。
func (d *Driver) Version() string { return "3.x" }

// Supports 对应 supports()：只认已知能力常量（PHP 的 Capability::tryFrom 对未知值返回 null）。
func (d *Driver) Supports(capability string) bool { return capabilitySet[capability] }

// Capabilities 对应 capabilities()。
func (d *Driver) Capabilities() []string {
	out := make([]string, len(capabilityList))
	copy(out, capabilityList)
	return out
}

// ListClients 对应 listClients()。
func (d *Driver) ListClients(ctx context.Context) ([]map[string]any, error) {
	return d.client.ListClients(ctx)
}

// GetClient 对应 getClient()。
func (d *Driver) GetClient(ctx context.Context, identifier string) (map[string]any, error) {
	return d.client.GetClient(ctx, identifier)
}

// CreateClient 对应 createClient()：email 是唯一必填项（面板主键）。
func (d *Driver) CreateClient(ctx context.Context, data map[string]any, inboundIDs []int64) (map[string]any, error) {
	return d.client.AddClient(ctx, data, inboundIDs)
}

// UpdateClient 对应 updateClient()：body 是完整 client（替换非 patch）。
func (d *Driver) UpdateClient(ctx context.Context, identifier string, data map[string]any, inboundID *int64) error {
	return d.client.UpdateClient(ctx, identifier, data, inboundID)
}

// DeleteClient 对应 deleteClient()。
func (d *Driver) DeleteClient(ctx context.Context, identifier string, keepTraffic bool) error {
	return d.client.DeleteClient(ctx, identifier, keepTraffic)
}

// ToggleClient 对应 toggleClient()：走 update 只带 {enable}。
//
// 注意与 PHP 的差异：PHP 把 ['enable' => $enable] 原样 POST 给 /update/{email}，
// 而 3x-ui 的 update 是**整条替换**语义 —— 只带 enable 的 body 会把该 client 的
// uuid/limit/expiry 全部清空。这是 PHP 现状（驱动层照抄），修复属于业务层
// （先 GetClient 拿到完整对象再改 enable），Go 侧在 README 的风险清单里标注。
func (d *Driver) ToggleClient(ctx context.Context, identifier string, enable bool) error {
	return d.client.UpdateClient(ctx, identifier, map[string]any{"enable": enable}, nil)
}

// GetClientTraffic 对应 getClientTraffic()。
func (d *Driver) GetClientTraffic(ctx context.Context, identifier string) (map[string]any, error) {
	return d.client.GetClientTraffic(ctx, identifier)
}

// ResetClientTraffic 对应 resetClientTraffic()。
func (d *Driver) ResetClientTraffic(ctx context.Context, identifier string) error {
	return d.client.ResetClientTraffic(ctx, identifier)
}

// ListInbounds 对应 listInbounds()。
func (d *Driver) ListInbounds(ctx context.Context) ([]map[string]any, error) {
	return d.client.ListInbounds(ctx)
}

// GetInbound 对应 getInbound()。
func (d *Driver) GetInbound(ctx context.Context, id int64) (map[string]any, error) {
	return d.client.GetInbound(ctx, id)
}

// GetClientStatsByEmail 对应 getClientStatsByEmail()。
func (d *Driver) GetClientStatsByEmail(ctx context.Context) (map[string]panel.ClientTraffic, error) {
	return d.client.GetClientStatsByEmail(ctx)
}

// GetClientStatsGroupedByInbound 对应 getClientStatsGroupedByInbound()。
func (d *Driver) GetClientStatsGroupedByInbound(ctx context.Context) (map[int64]map[string]panel.ClientTraffic, error) {
	return d.client.GetClientStatsGroupedByInbound(ctx)
}

// GetClientLinks 对应 getClientLinks()。
func (d *Driver) GetClientLinks(ctx context.Context, identifier string) ([]string, error) {
	return d.client.GetClientLinks(ctx, identifier)
}

// AttachClient 对应 attachClient()。
func (d *Driver) AttachClient(ctx context.Context, identifier string, inboundIDs []int64) error {
	return d.client.AttachClient(ctx, identifier, inboundIDs)
}

// DetachClient 对应 detachClient()。
func (d *Driver) DetachClient(ctx context.Context, identifier string, inboundIDs []int64) error {
	return d.client.DetachClient(ctx, identifier, inboundIDs)
}

// HealthCheck 对应 healthCheck()。
func (d *Driver) HealthCheck(ctx context.Context) (panel.Health, error) {
	return d.client.HealthCheck(ctx)
}

// 编译期断言：Driver 必须实现 panel.Driver。
var _ panel.Driver = (*Driver)(nil)

// ClientEmail 是 ControlHub 在 3x-ui 上的 client 主键口径（data-contract.md §1）。
//
// 单独导出：订阅、流量同步、建号三条链路都必须用同一个口径，散落拼接最容易写歪。
func ClientEmail(userID int64) string { return fmt.Sprintf("ch_user_%d", userID) }
