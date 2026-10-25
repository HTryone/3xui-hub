package ws

import (
	"context"

	"controlhub-server/internal/nodeapi"
)

// Backend 是 WS 通道需要的落库能力。*nodeapi.Repo 直接满足本接口 ——
// 指令认领 / 回执 / 流量落账 / driver_config 键级合并都**复用 HTTP 侧同一套实现**
// （NodeWsServer.php 注释里的「两条通道永远一致」），本包不再抄一份 SQL。
//
// 测试用 fake 实现同一接口即可，不需要 MySQL。
type Backend interface {
	// Authenticate 按 X-Node-Secret 找节点（与 HTTP NodeAuth 同口径）。
	Authenticate(ctx context.Context, secret string, key []byte) (nodeapi.Node, error)
	// NodeByID 重读节点（hello/report 取最新 driver_config）。
	NodeByID(ctx context.Context, id int64) (nodeapi.Node, error)
	// MergeDriverConfig 行锁 + 键级合并（禁止整块回写，会覆盖并发 bump 的 config_version）。
	MergeDriverConfig(ctx context.Context, nodeID int64, patch nodeapi.Object) error
	// MergeDriverConfigAndMarkOnline = merge + status=online/last_check_at（hello/report）。
	MergeDriverConfigAndMarkOnline(ctx context.Context, nodeID int64, patch nodeapi.Object) error
	// ClaimPending 原子认领 pending 指令（behavior-contract.md §6）。
	ClaimPending(ctx context.Context, nodeID int64, limit int) ([]nodeapi.Command, error)
	// ApplyAcks 落指令回执（与 HTTP /ack 共用）。
	ApplyAcks(ctx context.Context, nodeID int64, results []nodeapi.AckResult) (int, error)
	// SyncNodeFromPush 流量落账（与 HTTP /push 共用 TrafficSyncService 口径）。
	SyncNodeFromPush(ctx context.Context, node nodeapi.Node, stats []nodeapi.PushStats) (nodeapi.SyncResult, error)
	// PullInterval 配置拉取周期（秒）：admin 设置项 xray_pull_interval，缺省 30。
	PullInterval(ctx context.Context) int
}

// OutboundStore 是出站流量落表（xray_outbound_traffic，S4 出站管理的数据源）。
// 与 Backend 分开：这张表未建时要**静默跳过**，不牵连主路径。
type OutboundStore interface {
	// StoreOutbound 按 (node_id, tag) upsert 覆盖写；表不存在时返回 nil（静默跳过）。
	StoreOutbound(ctx context.Context, nodeID int64, rows []OutboundRow) error
}

// OutboundRow 是一条出站流量（tag 已按 mb_substr(0,128) 截断）。
type OutboundRow struct {
	Tag      string
	Upload   int64
	Download int64
}
