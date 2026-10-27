package nodeapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// 指令队列：逐条对齐 app/Models/NodeApiCommand.php + app/Services/NodeApiCommandService.php。
//
// 核心不变量（behavior-contract.md §6）：认领必须是
//   UPDATE ... SET status='acknowledged' WHERE id=? AND status='pending'
// 只有 RowsAffected==1 才算抢到；WS 通道与 HTTP 长轮询并发时不会重复下发。

// 指令状态常量（与 PHP 一致）。
const (
	StatusPending      = "pending"
	StatusAcknowledged = "acknowledged"
	StatusSuccess      = "success"
	StatusFailed       = "failed"
)

// 指令动作常量（与 NodeApiCommand 一致）。
const (
	ActionAddUser      = "adu"
	ActionRemoveUser   = "rmu"
	ActionUpgradeAgent = "upgrade"
	ActionRealityScan  = "reality_scan"
	ActionGeodataSync  = "geodata_sync"
	// ActionOutboundTest 是「出站连通性延时测试」（面板侧路由规则表的「延时」列）。
	//
	// 协议（面板 → agent）：
	//
	//	action  = "outbound_test"
	//	payload = {"targets":[{"tag":"hk-1","server":"1.2.3.4","port":443}, ...]}
	//
	// agent 逐个对 server:port 做 TCP 握手，回执 success 时 result 是 JSON 数组：
	//
	//	[{"tag":"hk-1","ok":true,"latency_ms":12,"error":""}, ...]
	//
	// 内置 direct/blocked、负载均衡 tag、UDP 出站（wireguard/warp）由面板侧过滤，
	// 不会出现在 payload 里。agent 侧实现不在本仓（本任务只定义协议）。
	ActionOutboundTest = "outbound_test"
	// 以上与 app/Models/NodeApiCommand.php 的常量逐字一致；本模块只用
	// ActionRealityScan / ActionOutboundTest（决定回执截断上限），其余导出供后续
	// P9（WS 通道）复用。
)

const (
	// maxCommandsPerClaim 是单次认领上限（PHP claimPending 默认 5）。
	maxCommandsPerClaim = 5

	// commandRetainDays 是终态回执保留天数（config/xray.php command_retain_days）。
	commandRetainDays = 30

	// outputLimitDefault / outputLimitRealityScan 是回执落库截断长度（按 action 放宽）。
	outputLimitDefault     = 2000
	outputLimitRealityScan = 30000
	// outputLimitOutboundTest 是出站延时回执的截断长度：回执是每条出站一个对象的
	// JSON 数组（约 60 字节/条），默认 2000 只够 ~30 条，批量测试会截断成非法 JSON。
	outputLimitOutboundTest = 30000
)

// Command 是下发给 agent 的一条指令。
type Command struct {
	ID      int64
	Action  string
	Payload json.RawMessage
}

// ClaimPending 复刻 NodeApiCommandService::claimPending()：
// 先按 (node_id, status='pending') 取前 N 条，再逐条原子 UPDATE 认领。
//
// 为什么不是一条 UPDATE ... LIMIT：需要把 payload 一起返回，且必须逐条确认
// 「确实是我抢到的」（并发通道可能先一步），所以沿用 PHP 的两步式。
func (r *Repo) ClaimPending(ctx context.Context, nodeID int64, limit int) ([]Command, error) {
	if limit <= 0 {
		limit = maxCommandsPerClaim
	}
	rows, err := r.DB.QueryContext(ctx,
		"SELECT id, action, payload FROM node_api_commands "+
			"WHERE node_id = ? AND status = 'pending' ORDER BY id LIMIT ?", nodeID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询 pending 指令失败: %w", err)
	}

	type candidate struct {
		id      int64
		action  string
		payload sql.NullString
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.action, &c.payload); err != nil {
			rows.Close()
			return nil, fmt.Errorf("读取 pending 指令失败: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 pending 指令失败: %w", err)
	}

	out := make([]Command, 0, len(cands))
	for _, c := range cands {
		claimed, err := r.acknowledge(ctx, c.id)
		if err != nil {
			return nil, err
		}
		if !claimed {
			continue // 并发下被另一条通道抢走
		}
		cmd := Command{ID: c.id, Action: c.action}
		if c.payload.Valid && c.payload.String != "" {
			cmd.Payload = json.RawMessage(c.payload.String)
		}
		out = append(out, cmd)
	}
	return out, nil
}

// acknowledge 复刻 NodeApiCommand::acknowledge()：pending → acknowledged。
func (r *Repo) acknowledge(ctx context.Context, id int64) (bool, error) {
	res, err := r.DB.ExecContext(ctx,
		"UPDATE node_api_commands SET status = 'acknowledged', acknowledged_at = ?, updated_at = ? "+
			"WHERE id = ? AND status = 'pending'",
		dbDateTime(r.now()), dbDateTime(r.now()), id)
	if err != nil {
		return false, fmt.Errorf("认领指令 #%d 失败: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("认领指令 #%d 失败: %w", id, err)
	}
	return n > 0, nil
}

// ApplyAcks 复刻 NodeApiCommandService::applyAck()：
// 逐条按 (id, node_id) 定位 → 写终态 + 截断后的输出 → 计数。
//
// 越权保护：回执只能改自己节点的指令（where node_id = ?）。
func (r *Repo) ApplyAcks(ctx context.Context, nodeID int64, results []AckResult) (int, error) {
	updated := 0
	for _, row := range results {
		ok, err := r.applyAck(ctx, nodeID, row)
		if err != nil {
			return updated, err
		}
		if ok {
			updated++
		}
	}
	// 顺带清理过期终态（懒删除，失败只影响清理本身）。
	_ = r.pruneExpired(ctx)
	return updated, nil
}

func (r *Repo) applyAck(ctx context.Context, nodeID int64, row AckResult) (bool, error) {
	var (
		action  string
		current sql.NullString
	)
	err := r.DB.QueryRowContext(ctx,
		"SELECT action, status FROM node_api_commands WHERE id = ? AND node_id = ?",
		row.ID, nodeID).Scan(&action, &current)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("查询指令 #%d 失败: %w", row.ID, err)
	}

	limit := outputLimitFor(action)
	// PHP：isset($row['output']) ? mb_substr(...) : null；空串会写成 ""（不是 null）。
	var result any
	if row.Output != "" || row.OutputSet {
		result = runeTruncate(row.Output, limit)
	}

	status := StatusFailed
	if row.Success {
		status = StatusSuccess
	}
	_, err = r.DB.ExecContext(ctx,
		"UPDATE node_api_commands SET status = ?, result = ?, executed_at = ?, updated_at = ? WHERE id = ? AND node_id = ?",
		status, result, dbDateTime(r.now()), dbDateTime(r.now()), row.ID, nodeID)
	if err != nil {
		return false, fmt.Errorf("写入指令 #%d 回执失败: %w", row.ID, err)
	}
	return true, nil
}

// outputLimitFor 返回某 action 回执的落库截断长度。
//
// 默认 2000 字符是给 xray 命令输出用的；reality_scan 与 outbound_test 的回执是
// JSON（逐目标一个对象），批量时会远超默认上限，必须单独放宽，否则落库即截断成
// 非法 JSON，面板侧只能报「回执解析失败」。
func outputLimitFor(action string) int {
	switch action {
	case ActionRealityScan:
		return outputLimitRealityScan
	case ActionOutboundTest:
		return outputLimitOutboundTest
	}
	return outputLimitDefault
}

// pruneExpired 复刻 pruneExpired()：只删 success/failed 且「上次写入超过保留期」的行。
func (r *Repo) pruneExpired(ctx context.Context) error {
	cutoff := dbDateTime(r.now().AddDate(0, 0, -commandRetainDays))
	_, err := r.DB.ExecContext(ctx,
		"DELETE FROM node_api_commands WHERE status IN ('success','failed') AND updated_at < ? LIMIT 5000", cutoff)
	return err
}

// InFlightCount 是某节点未终态的指令数（0 = agent 可安全全量兜底同步）。
func (r *Repo) InFlightCount(ctx context.Context, nodeID int64) (int, error) {
	var n int
	err := r.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM node_api_commands WHERE node_id = ? AND status IN ('pending','acknowledged')",
		nodeID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("统计在途指令失败: %w", err)
	}
	return n, nil
}

// commandWaitEnqueued 在长轮询里用于等待新指令的轮询间隔（PHP sleep(2)）。
const commandPollInterval = 2 * time.Second
