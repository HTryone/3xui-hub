package nodeapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 数据访问：与 Laravel 侧（Eloquent / Query Builder）逐条 SQL 对齐。
//
// 原则（contracts/data-contract.md §6 + MIGRATION.md §3.6）：
//   - driver_config 是 JSON 列，写入必须行锁 + 只 merge 自家键；
//     config_version 只由 bump 推进，不能被心跳回写覆盖；
//   - 时间列一律按库内字面量读写（DSN loc=UTC 打标签、不换算）；
//   - 不改 schema、不读 Laravel 的 jobs 表。

// Repo 是 nodeapi 模块的数据访问入口（*sql.DB 并发安全，可被所有请求共享）。
type Repo struct {
	DB *sql.DB
	// Now 可注入（测试固定时间用）；为 nil 时用 time.Now().UTC()。
	Now func() time.Time
}

// NewRepo 构造 Repo。
func NewRepo(db *sql.DB) *Repo { return &Repo{DB: db} }

func (r *Repo) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

// Node 是一个 xray 节点（鉴权与六端点需要的字段）。
type Node struct {
	ID           int64
	Name         string
	Host         string
	DriverType   string
	Status       string
	DriverConfig json.RawMessage // 原始 JSON 文本（NULL → nil）
	Enabled      bool
}

// ConfigObject 解析 driver_config 为保序对象。
func (n Node) ConfigObject() Object {
	o, err := ParseObject(n.DriverConfig)
	if err != nil {
		return Object{}
	}
	return o
}

// ConfigVersion 是 driver_config.config_version（缺省 0）。
func (n Node) ConfigVersion() int64 {
	v, ok := n.ConfigObject().Get("config_version")
	if !ok {
		return 0
	}
	return intOf(v)
}

// RoutingOptions 读 driver_config.xray_routing（缺省 AsIs / 空）。
func (n Node) RoutingOptions() RoutingOptions {
	s := toObject(mustGet(n.ConfigObject(), "xray_routing"))
	opts := RoutingOptions{}
	strategy, ok := s.Get("domainStrategy")
	if ok && stringOf(strategy) != "" {
		opts.DomainStrategy = stringOf(strategy)
	} else {
		opts.DomainStrategy = "AsIs"
	}
	matcher, ok := s.Get("domainMatcher")
	if ok {
		opts.DomainMatcher = stringOf(matcher)
	}
	return opts
}

// XrayNodes 返回全部 driver_type='xray' 的节点（NodeAuth 逐个比对密钥）。
//
// 对齐 Node::where('driver_type','xray')->get()：不按 enabled 过滤——
// PHP 侧同样不过滤，禁用节点仍能用旧密钥调接口（会在心跳里被写成 online，
// 这是现状，不在本次迁移中改变）。
func (r *Repo) XrayNodes(ctx context.Context) ([]Node, error) {
	rows, err := r.DB.QueryContext(ctx,
		"SELECT id, name, host, driver_type, status, driver_config, enabled FROM nodes WHERE driver_type = 'xray'")
	if err != nil {
		return nil, fmt.Errorf("查询 xray 节点失败: %w", err)
	}
	defer rows.Close()

	var out []Node
	for rows.Next() {
		var (
			n       Node
			cfg     sql.NullString
			enabled int64
		)
		if err := rows.Scan(&n.ID, &n.Name, &n.Host, &n.DriverType, &n.Status, &cfg, &enabled); err != nil {
			return nil, fmt.Errorf("读取 xray 节点失败: %w", err)
		}
		if cfg.Valid {
			n.DriverConfig = json.RawMessage(cfg.String)
		}
		n.Enabled = enabled != 0
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 xray 节点失败: %w", err)
	}
	return out, nil
}

// NodeByID 重新读取节点（鉴权后取最新状态）。
func (r *Repo) NodeByID(ctx context.Context, id int64) (Node, error) {
	var (
		n       Node
		cfg     sql.NullString
		enabled int64
	)
	err := r.DB.QueryRowContext(ctx,
		"SELECT id, name, host, driver_type, status, driver_config, enabled FROM nodes WHERE id = ?", id).
		Scan(&n.ID, &n.Name, &n.Host, &n.DriverType, &n.Status, &cfg, &enabled)
	if err != nil {
		return Node{}, fmt.Errorf("读取节点 #%d 失败: %w", id, err)
	}
	if cfg.Valid {
		n.DriverConfig = json.RawMessage(cfg.String)
	}
	n.Enabled = enabled != 0
	return n, nil
}

// MergeDriverConfig 复刻 Node::mergeDriverConfig()：
// 事务内 SELECT ... FOR UPDATE 重读最新值，再 array_replace_recursive 合并。
//
// 为什么必须行锁内重读：alive/register 与 bumpXrayConfigVersion 并发时，
// 用内存快照整块回写会把刚递增的 config_version 覆盖回旧值。
func (r *Repo) MergeDriverConfig(ctx context.Context, nodeID int64, patch Object) error {
	return r.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := lockDriverConfig(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		merged := mergeRecursive(cur, patch)
		return writeDriverConfig(ctx, tx, nodeID, merged)
	})
}

// MergeDriverConfigAndMarkOnline 复刻 `mergeDriverConfig($cfg)` + `forceFill([
// 'status'=>'online','last_check_at'=>now()])->save()`：同一事务内完成，
// 避免中途失败时只写了一半。
func (r *Repo) MergeDriverConfigAndMarkOnline(ctx context.Context, nodeID int64, patch Object) error {
	return r.inTx(ctx, func(tx *sql.Tx) error {
		cur, err := lockDriverConfig(ctx, tx, nodeID)
		if err != nil {
			return err
		}
		merged := mergeRecursive(cur, patch)
		if err := writeDriverConfig(ctx, tx, nodeID, merged); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx,
			"UPDATE nodes SET status = 'online', last_check_at = ?, updated_at = ? WHERE id = ?",
			dbDateTime(r.now()), dbDateTime(r.now()), nodeID)
		if err != nil {
			return fmt.Errorf("更新节点在线状态失败: %w", err)
		}
		return nil
	})
}

// MarkOnline 复刻 register 的 `forceFill(['status'=>'online','last_check_at'=>now()])->save()`。
func (r *Repo) MarkOnline(ctx context.Context, nodeID int64) error {
	now := dbDateTime(r.now())
	if _, err := r.DB.ExecContext(ctx,
		"UPDATE nodes SET status = 'online', last_check_at = ?, updated_at = ? WHERE id = ?",
		now, now, nodeID); err != nil {
		return fmt.Errorf("更新节点在线状态失败: %w", err)
	}
	return nil
}

// SetHostIfEmpty 复刻 register 里的「host 为空则用来源公网 IP 回填」。
//
// 只回填公网地址：内网/回环无法用于客户端连接。返回是否写入。
func (r *Repo) SetHostIfEmpty(ctx context.Context, nodeID int64, host string) (bool, error) {
	res, err := r.DB.ExecContext(ctx,
		"UPDATE nodes SET host = ?, updated_at = ? WHERE id = ? AND (host = '' OR host IS NULL)",
		host, dbDateTime(r.now()), nodeID)
	if err != nil {
		return false, fmt.Errorf("回填节点主机地址失败: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func lockDriverConfig(ctx context.Context, tx *sql.Tx, nodeID int64) (Object, error) {
	var cfg sql.NullString
	err := tx.QueryRowContext(ctx,
		"SELECT driver_config FROM nodes WHERE id = ? FOR UPDATE", nodeID).Scan(&cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("节点 #%d 不存在", nodeID)
	}
	if err != nil {
		return nil, fmt.Errorf("锁定节点 #%d 的 driver_config 失败: %w", nodeID, err)
	}
	if !cfg.Valid || cfg.String == "" {
		return Object{}, nil
	}
	o, err := ParseObject([]byte(cfg.String))
	if err != nil {
		// 库里的 JSON 损坏：按空对象起步（与 PHP 的 ?? [] 一致），不吞掉原始错误源。
		return Object{}, nil
	}
	return o, nil
}

func writeDriverConfig(ctx context.Context, tx *sql.Tx, nodeID int64, cfg Object) error {
	raw, err := MarshalValue(cfg)
	if err != nil {
		return fmt.Errorf("序列化 driver_config 失败: %w", err)
	}
	_, err = tx.ExecContext(ctx,
		"UPDATE nodes SET driver_config = ?, updated_at = ? WHERE id = ?",
		string(raw), dbDateTime(time.Now().UTC()), nodeID)
	if err != nil {
		return fmt.Errorf("写入 driver_config 失败: %w", err)
	}
	return nil
}

func (r *Repo) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// PullInterval 读 site_configs.xray_pull_interval（缺省 30，<5 也用 30）。
func (r *Repo) PullInterval(ctx context.Context) int {
	var value sql.NullString
	err := r.DB.QueryRowContext(ctx,
		"SELECT value FROM site_configs WHERE `key` = 'xray_pull_interval' LIMIT 1").Scan(&value)
	if err != nil || !value.Valid {
		return 30
	}
	v := intOf(value.String)
	if v >= 5 {
		return int(v)
	}
	return 30
}

// ── 渲染数据装配 ─────────────────────────────────────────────

// LoadRenderData 一次取齐 render() 所需的全部数据。
//
// SQL 与 PHP 侧逐条对应：
//   - eligibleUserRows(): plan_id 非空 + enabled + traffic_disabled_at 为空，**无 ORDER BY**
//   - XrayInbound/Outbound/Balancer/RoutingRule::enabledFor(): sort, id
//   - XrayOutboundSubscription::enabledFor(): priority, id
//   - XrayRelayClient::forInbound(): id
func (r *Repo) LoadRenderData(ctx context.Context, node Node) (*RenderData, error) {
	d := &RenderData{
		Node: RenderNode{
			ID:          node.ID,
			Name:        node.Name,
			Host:        node.Host,
			DriverType:  node.DriverType,
			ConfigRaw:   node.DriverConfig,
			ConfigVer:   node.ConfigVersion(),
			RoutingOpts: node.RoutingOptions(),
		},
	}

	users, err := r.eligibleUsers(ctx)
	if err != nil {
		return nil, err
	}
	d.Users = users

	inbounds, err := r.enabledInbounds(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	d.Inbounds = inbounds

	outbounds, err := r.enabledOutbounds(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	d.Outbounds = outbounds

	balancers, err := r.enabledBalancers(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	d.Balancers = balancers

	rules, err := r.enabledRoutingRules(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	d.RoutingRules = rules

	subs, err := r.enabledSubscriptions(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	d.Subscriptions = subs

	relays, err := r.relayClientsForInbounds(ctx, inboundIDs(inbounds))
	if err != nil {
		return nil, err
	}
	d.RelayClients = relays

	return d, nil
}

func inboundIDs(inbounds []RenderInbound) []int64 {
	ids := make([]int64, 0, len(inbounds))
	for _, ib := range inbounds {
		ids = append(ids, ib.ID)
	}
	return ids
}

func (r *Repo) eligibleUsers(ctx context.Context) ([]RenderUser, error) {
	rows, err := r.DB.QueryContext(ctx,
		"SELECT id, uuid, protocol, traffic_limit, expired_at FROM users "+
			"WHERE plan_id IS NOT NULL AND enabled = 1 AND traffic_disabled_at IS NULL")
	if err != nil {
		return nil, fmt.Errorf("查询渲染用户失败: %w", err)
	}
	defer rows.Close()

	var out []RenderUser
	for rows.Next() {
		var (
			u  RenderUser
			ex sql.NullTime
		)
		if err := rows.Scan(&u.ID, &u.UUID, &u.Protocol, &u.TrafficLimit, &ex); err != nil {
			return nil, fmt.Errorf("读取渲染用户失败: %w", err)
		}
		if ex.Valid {
			t := ex.Time
			u.ExpiredAt = &t
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r *Repo) enabledInbounds(ctx context.Context, nodeID int64) ([]RenderInbound, error) {
	rows, err := r.DB.QueryContext(ctx,
		"SELECT id, tag, remark, protocol, port, listen, settings, stream_settings, sniffing, reality_private_key "+
			"FROM xray_inbounds WHERE node_id = ? AND enabled = 1 ORDER BY sort, id", nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询入站失败: %w", err)
	}
	defer rows.Close()

	var out []RenderInbound
	for rows.Next() {
		var (
			ib                              RenderInbound
			remark, settings, stream, sniff sql.NullString
			privKey                         sql.NullString
		)
		if err := rows.Scan(&ib.ID, &ib.Tag, &remark, &ib.Protocol, &ib.Port, &ib.Listen,
			&settings, &stream, &sniff, &privKey); err != nil {
			return nil, fmt.Errorf("读入站失败: %w", err)
		}
		if remark.Valid {
			v := remark.String
			ib.Remark = &v
		}
		ib.Settings = rawOrNilString(settings)
		ib.StreamSettings = rawOrNilString(stream)
		ib.Sniffing = rawOrNilString(sniff)
		if privKey.Valid {
			v := privKey.String
			ib.RealityPrivateKey = &v
		}
		out = append(out, ib)
	}
	return out, rows.Err()
}

func rawOrNilString(s sql.NullString) json.RawMessage {
	if !s.Valid {
		return nil
	}
	return json.RawMessage(s.String)
}

func (r *Repo) enabledOutbounds(ctx context.Context, nodeID int64) ([]RenderOutbound, error) {
	rows, err := r.DB.QueryContext(ctx,
		"SELECT tag, protocol, settings, stream_settings, secret_key FROM xray_outbounds "+
			"WHERE node_id = ? AND enabled = 1 ORDER BY sort, id", nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询出站失败: %w", err)
	}
	defer rows.Close()

	var out []RenderOutbound
	for rows.Next() {
		var (
			ob                  RenderOutbound
			settings, stream, s sql.NullString
		)
		if err := rows.Scan(&ob.Tag, &ob.Protocol, &settings, &stream, &s); err != nil {
			return nil, fmt.Errorf("读取出站失败: %w", err)
		}
		ob.Settings = rawOrNilString(settings)
		ob.StreamSettings = rawOrNilString(stream)
		if s.Valid {
			v := s.String
			ob.SecretKey = &v
		}
		out = append(out, ob)
	}
	return out, rows.Err()
}

func (r *Repo) enabledBalancers(ctx context.Context, nodeID int64) ([]RenderBalancer, error) {
	rows, err := r.DB.QueryContext(ctx,
		"SELECT tag, selectors, strategy, fallback_tag FROM xray_balancers "+
			"WHERE node_id = ? AND enabled = 1 ORDER BY sort, id", nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询负载均衡失败: %w", err)
	}
	defer rows.Close()

	var out []RenderBalancer
	for rows.Next() {
		var (
			b         RenderBalancer
			selectors sql.NullString
			fallback  sql.NullString
		)
		if err := rows.Scan(&b.Tag, &selectors, &b.Strategy, &fallback); err != nil {
			return nil, fmt.Errorf("读取负载均衡失败: %w", err)
		}
		b.Selectors = rawOrNilString(selectors)
		if fallback.Valid {
			v := fallback.String
			b.FallbackTag = &v
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (r *Repo) enabledRoutingRules(ctx context.Context, nodeID int64) ([]RenderRoutingRule, error) {
	rows, err := r.DB.QueryContext(ctx,
		"SELECT domains, ips, port, network, protocol, inbound_tag, outbound_tag "+
			"FROM xray_routing_rules WHERE node_id = ? AND enabled = 1 ORDER BY sort, id", nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询路由规则失败: %w", err)
	}
	defer rows.Close()

	var out []RenderRoutingRule
	for rows.Next() {
		var (
			rule                          RenderRoutingRule
			domains, ips                  sql.NullString
			port, network, proto, inbound sql.NullString
		)
		if err := rows.Scan(&domains, &ips, &port, &network, &proto, &inbound, &rule.OutboundTag); err != nil {
			return nil, fmt.Errorf("读取路由规则失败: %w", err)
		}
		rule.Domains = rawOrNilString(domains)
		rule.IPs = rawOrNilString(ips)
		rule.Port = nullStringPtr(port)
		rule.Network = nullStringPtr(network)
		rule.Protocol = nullStringPtr(proto)
		rule.InboundTag = nullStringPtr(inbound)
		out = append(out, rule)
	}
	return out, rows.Err()
}

func nullStringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

func (r *Repo) enabledSubscriptions(ctx context.Context, nodeID int64) ([]RenderSubscription, error) {
	rows, err := r.DB.QueryContext(ctx,
		"SELECT prepend, fetched FROM xray_outbound_subscriptions "+
			"WHERE node_id = ? AND enabled = 1 ORDER BY priority, id", nodeID)
	if err != nil {
		return nil, fmt.Errorf("查询出站订阅失败: %w", err)
	}
	defer rows.Close()

	var out []RenderSubscription
	for rows.Next() {
		var (
			sub     RenderSubscription
			prepend int64
			fetched sql.NullString
		)
		if err := rows.Scan(&prepend, &fetched); err != nil {
			return nil, fmt.Errorf("读取出站订阅失败: %w", err)
		}
		sub.Prepend = prepend != 0
		sub.Fetched = rawOrNilString(fetched)
		out = append(out, sub)
	}
	return out, rows.Err()
}

func (r *Repo) relayClientsForInbounds(ctx context.Context, ids []int64) ([]RenderRelayClient, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := r.DB.QueryContext(ctx,
		"SELECT inbound_id, client_id FROM xray_relay_clients WHERE inbound_id IN ("+placeholders+") ORDER BY id", args...)
	if err != nil {
		return nil, fmt.Errorf("查询中转凭据失败: %w", err)
	}
	defer rows.Close()

	var out []RenderRelayClient
	for rows.Next() {
		var rc RenderRelayClient
		if err := rows.Scan(&rc.InboundID, &rc.ClientID); err != nil {
			return nil, fmt.Errorf("读取中转凭据失败: %w", err)
		}
		out = append(out, rc)
	}
	return out, rows.Err()
}

// ── 指纹 ─────────────────────────────────────────────────────

// LoadFingerprint 复刻 XrayConfigService::fingerprint() 的全部查询。
func (r *Repo) LoadFingerprint(ctx context.Context, node Node) (*FingerprintData, error) {
	fd := &FingerprintData{
		ConfigVersion:  node.ConfigVersion(),
		RoutingOptions: node.RoutingOptions(),
	}

	for _, t := range fingerprintTables {
		stamp, err := r.tableStamp(ctx, t.Table, t.Col, node.ID)
		if err != nil {
			return nil, err
		}
		fd.Stamps = append(fd.Stamps, stamp)
	}

	relay, err := r.relayStamp(ctx, node.ID)
	if err != nil {
		return nil, err
	}
	fd.Relay = relay

	if err := r.usersPlansSummary(ctx, fd); err != nil {
		return nil, err
	}
	return fd, nil
}

func (r *Repo) tableStamp(ctx context.Context, table, col string, nodeID int64) (Stamp, error) {
	var (
		maxUpdated sql.NullString
		count      int64
	)
	q := fmt.Sprintf("SELECT MAX(updated_at), COUNT(*) FROM `%s` WHERE `%s` = ?", table, col)
	if err := r.DB.QueryRowContext(ctx, q, nodeID).Scan(&maxUpdated, &count); err != nil {
		return Stamp{}, fmt.Errorf("指纹查询 %s 失败: %w", table, err)
	}
	stamp := Stamp{Label: table, Count: count}
	if maxUpdated.Valid {
		stamp.MaxUpdatedAt = maxUpdated.String
	}
	return stamp, nil
}

func (r *Repo) relayStamp(ctx context.Context, nodeID int64) (Stamp, error) {
	var (
		maxUpdated sql.NullString
		count      int64
	)
	err := r.DB.QueryRowContext(ctx,
		"SELECT MAX(updated_at), COUNT(*) FROM xray_relay_clients WHERE node_id = ? OR source_node_id = ?",
		nodeID, nodeID).Scan(&maxUpdated, &count)
	if err != nil {
		return Stamp{}, fmt.Errorf("指纹查询 xray_relay_clients 失败: %w", err)
	}
	stamp := Stamp{Label: "relay", Count: count}
	if maxUpdated.Valid {
		stamp.MaxUpdatedAt = maxUpdated.String
	}
	return stamp, nil
}

// usersPlansSummary 复刻 fingerprint() 里 users / plans 的内容摘要（CRC32 累加）。
func (r *Repo) usersPlansSummary(ctx context.Context, fd *FingerprintData) error {
	usersSQL := "SELECT COUNT(*), COALESCE(SUM(CRC32(CONCAT_WS('|', id, uuid, protocol, enabled, " +
		"COALESCE(plan_id, '-'), COALESCE(traffic_limit, '-'), " +
		"COALESCE(DATE_FORMAT(expired_at, '%Y-%m-%d %H:%i:%s'), '-'), " +
		"COALESCE(traffic_disabled_at, '-')))), 0) FROM users"
	if err := r.DB.QueryRowContext(ctx, usersSQL).Scan(&fd.UsersCount, &fd.UsersSum); err != nil {
		return fmt.Errorf("指纹查询 users 失败: %w", err)
	}

	plansSQL := "SELECT COUNT(*), COALESCE(SUM(CRC32(CONCAT_WS('|', id, type, " +
		"COALESCE(months, '-'), COALESCE(monthly_traffic, '-'), " +
		"COALESCE(period_traffic, '-'), COALESCE(total_traffic, '-')))), 0) FROM plans"
	if err := r.DB.QueryRowContext(ctx, plansSQL).Scan(&fd.PlansCount, &fd.PlansSum); err != nil {
		return fmt.Errorf("指纹查询 plans 失败: %w", err)
	}
	return nil
}

// ConfigChangedAt 读 driver_config.config_changed_at（RFC3339 字符串）。
func (n Node) ConfigChangedAt() (time.Time, bool) {
	v, ok := n.ConfigObject().Get("config_changed_at")
	if !ok {
		return time.Time{}, false
	}
	s := stringOf(v)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
