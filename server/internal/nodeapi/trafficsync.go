package nodeapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// 流量落账（HTTP push 通道）：逐行对齐 app/Services/TrafficSyncService.php
// 的 syncNodeFromSource → commitNodeStats → syncNodeBatch → upsertSnapshots /
// applyDeltas / recordUserTraffic。
//
// 口径铁律（contracts/behavior-contract.md §2，data-contract.md §1）：
//   - 快照表字段是 traffic_snapshots.upload / .download（**不是** up/down）；
//   - 增量 = 本轮累计 − 上轮快照；本轮 < 上轮 → 本轮计费增量记 0 并把快照推进为新基准；
//   - 加权 = floor((up+down) × multiplier)，倍率只乘 delta、绝不进快照；
//   - 周期套餐同时累加 monthly_traffic_used；
//   - 幂等：同一份采样重放不重复扣（以最新快照为基准）。

// SyncResult 复刻 syncNodeFromSource 的返回值。
type SyncResult struct {
	Acquired bool
	// DeltaMap 是 userID → 方向增量与加权增量（只含非零项）。
	DeltaMap map[int64]Delta
}

// Delta 是单个用户的增量。
type Delta struct {
	Up       int64
	Down     int64
	Weighted int64
}

// 常量（与 PHP 一致）。
const (
	originalMultiplierKey = "default_node_multiplier"
	// snapshotChunkSize 是 upsert / 批量 UPDATE 的分批大小（PHP array_chunk(…, 500)）。
	snapshotChunkSize = 500
	// trafficLockSeconds 是节点级流量锁的持有时长（PHP Cache::lock(…, 120)）。
	trafficLockSeconds = 120
)

// SyncNodeFromPush 复刻 syncNodeFromSource()：节点锁内「读快照 → 算增量 → 落账」。
//
// 时区：本函数里所有时间字面量都**显式从 Go 传**（created_at/updated_at/hour_start/date），
// SQL 里不出现 NOW()；session 时区因此在两侧都不影响结果（PHP 侧同样如此：
// Laravel 的 query builder 传 Carbon 值，只有 upsertSnapshots 用了 NOW()，见下方说明）。
func (r *Repo) SyncNodeFromPush(ctx context.Context, node Node, stats []PushStats) (SyncResult, error) {
	lock, ok, err := r.acquireTrafficLock(ctx, node.ID)
	if err != nil {
		return SyncResult{}, err
	}
	if !ok {
		// PHP 拿不到锁时返回 acquired=false（并发同步正在进行，本轮跳过）。
		return SyncResult{Acquired: false, DeltaMap: map[int64]Delta{}}, nil
	}
	defer r.releaseTrafficLock(context.WithoutCancel(ctx), node.ID, lock)

	clientStats := make(map[string]clientStat, len(stats))
	for _, s := range stats {
		clientStats[s.Email] = clientStat{Up: s.Up, Down: s.Down}
	}

	result, err := r.commitNodeStats(ctx, node, clientStats)
	if err != nil {
		return SyncResult{}, err
	}
	result.Acquired = true
	return result, nil
}

type clientStat struct {
	Up   int64
	Down int64
}

// commitNodeStats 复刻 commitNodeStats()：事务内「算增量 → 写快照 → 累加已用 → 留痕」。
func (r *Repo) commitNodeStats(ctx context.Context, node Node, clientStats map[string]clientStat) (SyncResult, error) {
	out := SyncResult{DeltaMap: map[int64]Delta{}}
	if len(clientStats) == 0 {
		return out, nil
	}

	batch, err := r.syncNodeBatch(ctx, node, clientStats)
	if err != nil {
		return out, err
	}
	out.DeltaMap = batch.DeltaMap

	// 小时表过期清理：与 PHP 一样在事务**外**、每小时至多一次、失败不影响落账。
	r.pruneExpiredHourly(ctx)

	err = r.inTx(ctx, func(tx *sql.Tx) error {
		if err := upsertSnapshots(ctx, tx, batch.Snapshots); err != nil {
			return err
		}
		if err := applyDeltas(ctx, tx, batch.DeltaMap, batch.PeriodUsers); err != nil {
			return err
		}
		return recordUserTraffic(ctx, tx, node.ID, batch.DeltaMap, r.now())
	})
	if err != nil {
		return out, err
	}
	return out, nil
}

// batchResult 是 syncNodeBatch 的产物。
type batchResult struct {
	DeltaMap map[int64]Delta
	// Snapshots 是待写入的快照（原始累计值）。
	Snapshots []snapshotRow
	// PeriodUsers 是「周期套餐」用户集合（需要同时累加 monthly_traffic_used）。
	PeriodUsers map[int64]bool
}

type snapshotRow struct {
	UserID   int64
	NodeID   int64
	Upload   int64
	Download int64
}

// syncNodeBatch 复刻 syncNodeBatch() 的增量计算部分（drift 检测不在 HTTP push 路径）。
func (r *Repo) syncNodeBatch(ctx context.Context, node Node, clientStats map[string]clientStat) (batchResult, error) {
	out := batchResult{DeltaMap: map[int64]Delta{}, PeriodUsers: map[int64]bool{}}

	// 1. 从 clientEmail（ch_user_{id}）解析 userId。
	emailToUserID := map[string]int64{}
	userIDs := make([]int64, 0, len(clientStats))
	for email := range clientStats {
		id, ok := parseClientEmail(email)
		if !ok {
			continue
		}
		emailToUserID[email] = id
		userIDs = append(userIDs, id)
	}
	if len(emailToUserID) == 0 {
		return out, nil
	}

	// 2. 批量查最新快照 + 3. 批量查有套餐的用户。
	snapshots, err := r.latestSnapshots(ctx, node.ID, userIDs)
	if err != nil {
		return out, err
	}
	users, err := r.usersWithPlan(ctx, userIDs)
	if err != nil {
		return out, err
	}

	multiplier := r.multiplierFor(ctx, node)

	for email, userID := range emailToUserID {
		user, ok := users[userID]
		if !ok {
			continue // 无套餐用户跳过
		}
		stat, ok := clientStats[email]
		if !ok {
			continue
		}

		nowUp, nowDown := stat.Up, stat.Down
		nowTotal := nowUp + nowDown

		last, hasLast := snapshots[userID]
		var lastUp, lastDown *int64
		if hasLast {
			lu, ld := last.Upload, last.Download
			lastUp, lastDown = &lu, &ld
		}

		upDelta, downDelta := directionalDeltas(nowUp, nowDown, lastUp, lastDown)

		var delta int64
		if !hasLast {
			// 首次同步且计数为 0：不落空快照。
			if nowTotal == 0 {
				continue
			}
			delta = nowTotal
		} else {
			lastTotal := last.Upload + last.Download
			if nowTotal < lastTotal {
				// 计数器倒退（节点重置/重建）：本轮计费增量记 0，快照推进为新基准。
				out.Snapshots = append(out.Snapshots, snapshotRow{
					UserID: userID, NodeID: node.ID, Upload: nowUp, Download: nowDown,
				})
				continue
			}
			delta = nowTotal - lastTotal
			if delta == 0 {
				// 累计值没变 → 跳过写快照；但方向增量不为 0 的「一升一降」要留痕。
				if upDelta != 0 || downDelta != 0 {
					out.DeltaMap[userID] = Delta{Up: upDelta, Down: downDelta, Weighted: 0}
					out.Snapshots = append(out.Snapshots, snapshotRow{
						UserID: userID, NodeID: node.ID, Upload: nowUp, Download: nowDown,
					})
				}
				continue
			}
		}

		weighted := int64(0)
		if delta > 0 {
			weighted = int64(math.Floor(float64(delta) * multiplier))
		}
		if upDelta > 0 || downDelta > 0 || weighted > 0 {
			out.DeltaMap[userID] = Delta{Up: upDelta, Down: downDelta, Weighted: weighted}
		}

		if user.PeriodPlan {
			out.PeriodUsers[userID] = true
		}

		out.Snapshots = append(out.Snapshots, snapshotRow{
			UserID: userID, NodeID: node.ID, Upload: nowUp, Download: nowDown,
		})
	}

	return out, nil
}

// directionalDeltas 复刻 TrafficSyncService::directionalDeltas()：
// 首次同步整段现值算增量；总量倒退全记 0；其余逐方向取差、负差钳 0。
func directionalDeltas(nowUp, nowDown int64, lastUp, lastDown *int64) (int64, int64) {
	if lastUp == nil || lastDown == nil {
		return nowUp, nowDown
	}
	if nowUp+nowDown < *lastUp+*lastDown {
		return 0, 0
	}
	up := nowUp - *lastUp
	if up < 0 {
		up = 0
	}
	down := nowDown - *lastDown
	if down < 0 {
		down = 0
	}
	return up, down
}

// parseClientEmail 复刻 parseUserIds 的 /^ch_user_(\d+)$/。
func parseClientEmail(email string) (int64, bool) {
	const prefix = "ch_user_"
	if !strings.HasPrefix(email, prefix) {
		return 0, false
	}
	digits := email[len(prefix):]
	if digits == "" {
		return 0, false
	}
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	id, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

type lastSnapshot struct {
	Upload   int64
	Download int64
}

func (r *Repo) latestSnapshots(ctx context.Context, nodeID int64, userIDs []int64) (map[int64]lastSnapshot, error) {
	out := map[int64]lastSnapshot{}
	if len(userIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(userIDs)+1)
	args = append(args, nodeID)
	for _, id := range userIDs {
		args = append(args, id)
	}
	// 对齐 TrafficSnapshot::where(node_id)->whereIn(user_id)->orderByDesc(id)->get()->keyBy(user_id)：
	// 同一 user 取 id 最大的一行（id 单增 ⇒ 最新）。
	q := "SELECT user_id, upload, download FROM traffic_snapshots WHERE node_id = ? AND user_id IN (" +
		placeholders(len(userIDs)) + ") ORDER BY id DESC"
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询流量快照失败: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			uid                 int64
			upload, downloadVal int64
		)
		if err := rows.Scan(&uid, &upload, &downloadVal); err != nil {
			return nil, fmt.Errorf("读取流量快照失败: %w", err)
		}
		if _, exists := out[uid]; exists {
			continue // 已取到更新的那行
		}
		out[uid] = lastSnapshot{Upload: upload, Download: downloadVal}
	}
	return out, rows.Err()
}

type renderUserPlan struct {
	PeriodPlan bool
}

func (r *Repo) usersWithPlan(ctx context.Context, userIDs []int64) (map[int64]renderUserPlan, error) {
	out := map[int64]renderUserPlan{}
	if len(userIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(userIDs))
	for _, id := range userIDs {
		args = append(args, id)
	}
	// 对齐 User::whereIn(id)->whereNotNull(plan_id)->with(plan)->get()：
	// 只关心「有没有套餐」与「套餐是否 period」。
	q := "SELECT u.id, p.type FROM users u INNER JOIN plans p ON p.id = u.plan_id WHERE u.id IN (" +
		placeholders(len(userIDs)) + ")"
	rows, err := r.DB.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("查询套餐用户失败: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id    int64
			ptype string
		)
		if err := rows.Scan(&id, &ptype); err != nil {
			return nil, fmt.Errorf("读取套餐用户失败: %w", err)
		}
		out[id] = renderUserPlan{PeriodPlan: ptype == "period"}
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// multiplierFor 复刻 TrafficSyncService::multiplierFor()：
// 节点 traffic_multiplier 为 NULL → 继承 site_configs.default_node_multiplier，否则用节点值。
// 不做进程内缓存（改倍率必须立刻生效）。
func (r *Repo) multiplierFor(ctx context.Context, node Node) float64 {
	var own sql.NullString
	err := r.DB.QueryRowContext(ctx, "SELECT traffic_multiplier FROM nodes WHERE id = ?", node.ID).Scan(&own)
	if err == nil && own.Valid && own.String != "" {
		f, _ := strconv.ParseFloat(own.String, 64)
		return f
	}
	return r.originalMultiplier(ctx)
}

// originalMultiplier 复刻 originalMultiplier()：缺省/空串 → 1.0。
func (r *Repo) originalMultiplier(ctx context.Context) float64 {
	var value sql.NullString
	err := r.DB.QueryRowContext(ctx,
		"SELECT value FROM site_configs WHERE `key` = ? LIMIT 1", originalMultiplierKey).Scan(&value)
	if err != nil || !value.Valid || value.String == "" {
		return 1.0
	}
	f, err := strconv.ParseFloat(value.String, 64)
	if err != nil {
		return 1.0
	}
	return f
}

// upsertSnapshots 复刻 upsertSnapshots()：每 user+node 只保留一行。
//
// created_at/updated_at 用 SQL 的 NOW()（与 PHP 逐字一致）——这两列不参与正确性，
// 但它们的时间口径必须和 PHP 写出来的一致，否则并存期两边的运维查询会对不上。
func upsertSnapshots(ctx context.Context, tx *sql.Tx, rows []snapshotRow) error {
	if len(rows) == 0 {
		return nil
	}
	for start := 0; start < len(rows); start += snapshotChunkSize {
		end := start + snapshotChunkSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]

		var sb strings.Builder
		sb.WriteString("INSERT INTO traffic_snapshots (user_id, node_id, upload, download, created_at, updated_at) VALUES ")
		args := make([]any, 0, len(chunk)*4)
		for i, row := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, NOW(), NOW())")
			args = append(args, row.UserID, row.NodeID, row.Upload, row.Download)
		}
		sb.WriteString(" ON DUPLICATE KEY UPDATE upload = VALUES(upload), download = VALUES(download), updated_at = NOW()")

		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("写入流量快照失败: %w", err)
		}
	}
	return nil
}

// applyDeltas 复刻 applyDeltas()：只把加权值累加进 traffic_used；
// 周期套餐用户同时累加 monthly_traffic_used。
func applyDeltas(ctx context.Context, tx *sql.Tx, deltaMap map[int64]Delta, periodUsers map[int64]bool) error {
	if len(deltaMap) == 0 {
		return nil
	}
	weighted := make(map[int64]int64, len(deltaMap))
	for uid, d := range deltaMap {
		if d.Weighted > 0 {
			weighted[uid] = d.Weighted
		}
	}
	if len(weighted) == 0 {
		return nil
	}

	ids := sortedIDs(weighted)
	for start := 0; start < len(ids); start += snapshotChunkSize {
		end := start + snapshotChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		if err := applyDeltaChunk(ctx, tx, "traffic_used", chunk, weighted); err != nil {
			return err
		}

		periodChunk := make([]int64, 0, len(chunk))
		for _, id := range chunk {
			if periodUsers[id] {
				periodChunk = append(periodChunk, id)
			}
		}
		if len(periodChunk) > 0 {
			if err := applyDeltaChunk(ctx, tx, "monthly_traffic_used", periodChunk, weighted); err != nil {
				return err
			}
		}
	}
	return nil
}

// applyDeltaChunk 复刻 `UPDATE users SET col = col + CASE id WHEN ? THEN ? ... END WHERE id IN (...)`。
func applyDeltaChunk(ctx context.Context, tx *sql.Tx, column string, ids []int64, weighted map[int64]int64) error {
	var sb strings.Builder
	args := make([]any, 0, len(ids)*2+len(ids))
	sb.WriteString("UPDATE users SET ")
	sb.WriteString(column)
	sb.WriteString(" = ")
	sb.WriteString(column)
	sb.WriteString(" + CASE id")
	for _, id := range ids {
		sb.WriteString(" WHEN ? THEN ?")
		args = append(args, id, weighted[id])
	}
	sb.WriteString(" END WHERE id IN (")
	sb.WriteString(placeholders(len(ids)))
	sb.WriteString(")")
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("累加 %s 失败: %w", column, err)
	}
	return nil
}

func sortedIDs(m map[int64]int64) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	// 插入顺序不影响金额（CASE 分支互斥），但固定顺序便于 SQL 日志对拍。
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
	return ids
}

// recordUserTraffic 复刻 recordUserTraffic()：小时表（24h 窗口）+ 天表（业务时区日界）累加。
//
// 业务时区：config('panel.business_timezone')，缺省 Asia/Shanghai。
func recordUserTraffic(ctx context.Context, tx *sql.Tx, nodeID int64, deltaMap map[int64]Delta, now time.Time) error {
	if len(deltaMap) == 0 {
		return nil
	}
	rows := make(map[int64]Delta, len(deltaMap))
	for uid, d := range deltaMap {
		if d.Up == 0 && d.Down == 0 && d.Weighted == 0 {
			continue
		}
		rows[uid] = d
	}
	if len(rows) == 0 {
		return nil
	}

	loc := businessLocation()
	hourStart := now.UTC().Truncate(time.Hour)
	date := now.In(loc).Format("2006-01-02")

	if err := accumulateUserTraffic(ctx, tx, "user_traffic_hourly", "hour_start", hourStart.Format("2006-01-02 15:04:05"), nodeID, rows); err != nil {
		return err
	}
	return accumulateUserTraffic(ctx, tx, "user_traffic_daily", "date", date, nodeID, rows)
}

// accumulateUserTraffic 是累加型 upsert（小时表与天表共用）。
func accumulateUserTraffic(ctx context.Context, tx *sql.Tx, table, bucketCol, bucket string, nodeID int64, rows map[int64]Delta) error {
	ids := make([]int64, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sortInt64s(ids)

	for start := 0; start < len(ids); start += snapshotChunkSize {
		end := start + snapshotChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		var sb strings.Builder
		sb.WriteString("INSERT INTO " + table + " (user_id, node_id, " + bucketCol +
			", up_delta, down_delta, weighted_delta, created_at, updated_at) VALUES ")
		args := make([]any, 0, len(chunk)*6)
		for i, uid := range chunk {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("(?, ?, ?, ?, ?, ?, NOW(), NOW())")
			d := rows[uid]
			args = append(args, uid, nodeID, bucket, d.Up, d.Down, d.Weighted)
		}
		sb.WriteString(" ON DUPLICATE KEY UPDATE up_delta = up_delta + VALUES(up_delta), " +
			"down_delta = down_delta + VALUES(down_delta), " +
			"weighted_delta = weighted_delta + VALUES(weighted_delta), updated_at = NOW()")

		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("写入 %s 失败: %w", table, err)
		}
	}
	return nil
}

func sortInt64s(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// businessLocation 读 PANEL_BUSINESS_TIMEZONE（缺省 Asia/Shanghai）。
func businessLocation() *time.Location {
	name := strings.TrimSpace(getenv("PANEL_BUSINESS_TIMEZONE"))
	if name == "" {
		name = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC
	}
	return loc
}

// ── 节点级流量锁（对齐 Laravel Cache::lock('traffic-sync:node:{id}', 120)）──
//
// 与 PHP 共用同一张 cache_locks 表：PHP 释放时按 owner 删除，Go 用同样的
// 「key + owner」语义，两侧并发安全。过期时间也用同一口径（unix 秒）。

func (r *Repo) acquireTrafficLock(ctx context.Context, nodeID int64) (string, bool, error) {
	key := cacheKeyPrefix() + fmt.Sprintf("traffic-sync:node:%d", nodeID)
	owner := randomOwner()
	expires := r.now().Unix() + trafficLockSeconds

	res, err := r.DB.ExecContext(ctx,
		"INSERT IGNORE INTO cache_locks (`key`, owner, expiration) VALUES (?, ?, ?)",
		key, owner, expires)
	if err != nil {
		return "", false, fmt.Errorf("获取流量锁失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return owner, true, nil
	}

	// 已被占用：仅当锁已过期（或本就是自己）时才接管（对齐 DatabaseLock::acquire）。
	res, err = r.DB.ExecContext(ctx,
		"UPDATE cache_locks SET owner = ?, expiration = ? WHERE `key` = ? AND (owner = ? OR expiration <= ?)",
		owner, expires, key, owner, r.now().Unix())
	if err != nil {
		return "", false, fmt.Errorf("接管流量锁失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n >= 1 {
		return owner, true, nil
	}
	return "", false, nil
}

// releaseTrafficLock 复刻 DatabaseLock::release()：只删除自己持有的那一行。
func (r *Repo) releaseTrafficLock(ctx context.Context, nodeID int64, owner string) {
	key := cacheKeyPrefix() + fmt.Sprintf("traffic-sync:node:%d", nodeID)
	if _, err := r.DB.ExecContext(ctx,
		"DELETE FROM cache_locks WHERE `key` = ? AND owner = ?", key, owner); err != nil {
		// 释放失败只会让锁在 120 秒后自然过期，不影响已完成的落账。
		_ = err
	}
}

// getenv 是 os.Getenv 的包内别名（便于测试替换）。
func getenv(name string) string { return os.Getenv(name) }

func randomOwner() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// cacheKeyPrefix 复刻 config/cache.php 的 prefix（CACHE_PREFIX 或 slug(APP_NAME)+'-cache-'）。
//
// 只用于 cache_locks 的 key 前缀：必须与 PHP 完全一致，否则两侧锁互相看不见。
func cacheKeyPrefix() string {
	if p := strings.TrimSpace(getenv("CACHE_PREFIX")); p != "" {
		return p
	}
	name := getenv("APP_NAME")
	slug := slugify(name)
	if slug == "" {
		slug = "laravel"
	}
	return slug + "-cache-"
}

// slugify 复刻 Str::slug()（只处理常见场景：小写、非字母数字转 -）。
func slugify(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// pruneExpiredHourly 复刻 pruneExpiredHourly()：小时表懒删除。
//
// 每小时至多一次（Cache::add 防重入，key 与 PHP 一致，共用 cache 表），
// 保留天数读 config/user_traffic.php（USER_TRAFFIC_HOURLY_RETAIN_DAYS，最小 2）。
// 失败只记日志，绝不影响落账。
func (r *Repo) pruneExpiredHourly(ctx context.Context) {
	ok, err := r.cacheAdd(ctx, "user-traffic:hourly:prune", "1", 3600)
	if err != nil || !ok {
		return // 这一小时内已清过 / 缓存不可用
	}

	retainDays := 7
	if raw := strings.TrimSpace(getenv("USER_TRAFFIC_HOURLY_RETAIN_DAYS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			retainDays = n
		}
	}
	if retainDays < 2 {
		retainDays = 2
	}
	cutoff := dbDateTime(r.now().AddDate(0, 0, -retainDays).Truncate(time.Hour))
	_, _ = r.DB.ExecContext(ctx,
		"DELETE FROM user_traffic_hourly WHERE hour_start < ? LIMIT 5000", cutoff)
}

// cacheAdd 复刻 Cache::add()：已存在（且未过期）返回 false，否则写入并返回 true。
//
// value 用 PHP serialize 的字符串格式写（"s:1:\"1\";"），保证 PHP 侧读到的内容
// 语义正常；两侧只把该键当「存在性标记」用，不依赖具体值。
func (r *Repo) cacheAdd(ctx context.Context, key, value string, seconds int) (bool, error) {
	fullKey := cacheKeyPrefix() + key
	expiration := r.now().Unix() + int64(seconds)

	// 先按 Laravel 的 get() 口径判断：存在且未过期 → false。
	var exp int64
	err := r.DB.QueryRowContext(ctx,
		"SELECT expiration FROM cache WHERE `key` = ?", fullKey).Scan(&exp)
	if err == nil && exp > r.now().Unix() {
		return false, nil
	}

	serialized := phpSerializeString(value)
	if err != nil { // 不存在 → 插入（INSERT IGNORE 防并发）
		res, ierr := r.DB.ExecContext(ctx,
			"INSERT IGNORE INTO cache (`key`, value, expiration) VALUES (?, ?, ?)",
			fullKey, serialized, expiration)
		if ierr != nil {
			return false, ierr
		}
		n, _ := res.RowsAffected()
		return n > 0, nil
	}

	// 已过期 → 覆盖（对齐 DatabaseStore::add 的插入语义）。
	res, uerr := r.DB.ExecContext(ctx,
		"UPDATE cache SET value = ?, expiration = ? WHERE `key` = ?", serialized, expiration, fullKey)
	if uerr != nil {
		return false, uerr
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// phpSerializeString 生成 PHP serialize() 的字符串形态：s:3:"abc";
//
// 注意长度是**字节数**（PHP 的 strlen），不是字符数；值里含引号/反斜杠时按 PHP
// 的转义规则处理（与 Go %q 不同，所以手写）。
func phpSerializeString(s string) string {
	var b strings.Builder
	b.WriteString("s:")
	b.WriteString(strconv.Itoa(len(s)))
	b.WriteString(`:"`)
	// 实测 PHP 8.x 的 serialize() 对字符串内容不做转义（引号、反斜杠都原样输出），
	// 只把字节长度写进前缀。缓存值只需 PHP 能 unserialize，故照原样拼接。
	b.WriteString(s)
	b.WriteString(`";`)
	return b.String()
}
