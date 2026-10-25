package admin

// monitor.go 对齐 backend/app/Services/MonitorService.php 的只读查询（summary / series）。
//
// 逐行对齐 PHP，口径要点（与 MonitorService 头注释一致）：
//   - 差分：每条样本的 delta = max(0, cum - 前一条 cum)，负差钳 0；「前一条」允许
//     落在窗口外，取不到按 0 起算（不把首次采样的累计值当成一段流量）。
//   - 没有数据就是 null，不兜底成 0（否则「节点没上报」会被前端画成「流量 0」）。
//   - 峰值速率是「单个区间」的双向速率，不是窗口平均（平均值会把突发抹平）。
//   - 时区：month 日界与 day 桶用 business_timezone，hour 桶用 app.timezone=UTC。

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// rawBucketSeconds 对齐 MonitorService::RAW_BUCKET_SECONDS（5 分钟采样周期）。
const rawBucketSeconds = 300

// monitorSampleRow 是 monitor_samples 的一行（累计值 + 采样时刻）。
type monitorSampleRow struct {
	sent      int64
	recv      int64
	sampledAt time.Time
}

// monitorPoint 是 points() 的一个增量点。
type monitorPoint struct {
	t    int64
	up   int64
	down int64
}

// MonitorSummary 对齐 GET /admin-api/monitor/summary。
func (h *AdminExtras) MonitorSummary(c *gin.Context) {
	data, err := h.monitorSummary(c.Request.Context(), c.Query("window"))
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data", data)))
}

// MonitorSeries 对齐 GET /admin-api/monitor/series。
func (h *AdminExtras) MonitorSeries(c *gin.Context) {
	data, err := h.monitorSeries(c.Request.Context(), c.Query("window"))
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data", data)))
}

// monitorWindow 对齐 MonitorService::window：非法 window 回退 24h。
// 返回的 start/end 保留原始精度；SQL 与 ISO 输出都按秒截断（Laravel 绑定
// DateTime 用 'Y-m-d H:i:s'，Carbon::toIso8601String 也不含微秒）。
func (h *AdminExtras) monitorWindow(window string) (start, end time.Time, key string) {
	key = window
	switch window {
	case "24h", "7d", "30d", "month":
	default:
		key = "24h"
	}

	end = h.now().In(time.UTC)

	switch key {
	case "7d":
		start = end.Add(-7 * 24 * time.Hour)
	case "30d":
		start = end.Add(-30 * 24 * time.Hour)
	case "month":
		start = h.monthStart()
	default: // 24h
		start = end.Add(-24 * time.Hour)
	}
	return start, end, key
}

// monthStart 对齐 MonitorService::monthStart：
// 本月 1 日 00:00（business_timezone），换算成 app.timezone（UTC）下的同一时刻。
func (h *AdminExtras) monthStart() time.Time {
	biz := businessLocation()
	local := h.now().In(biz)
	startLocal := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, biz)
	return startLocal.In(time.UTC)
}

// monitorBucket 对齐 MonitorService::series 的粒度映射：
// 24h→raw、7d→hour、30d/month→day。
func monitorBucket(key string) string {
	switch key {
	case "7d":
		return "hour"
	case "30d", "month":
		return "day"
	default:
		return "raw"
	}
}

// bucketStart 对齐 MonitorService::bucketStart：采样时刻归并到的粒度起点。
// hour → app tz（UTC）整点；day → business tz 当天 0 点；raw → 原样。
func bucketStart(at time.Time, bucket string) time.Time {
	switch bucket {
	case "hour":
		u := at.In(time.UTC)
		return time.Date(u.Year(), u.Month(), u.Day(), u.Hour(), 0, 0, 0, time.UTC)
	case "day":
		biz := businessLocation()
		local := at.In(biz)
		return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, biz)
	default:
		return at
	}
}

// monitorDeltas 对齐 MonitorService::deltas：prev 为 nil 整段按 0 起算，负差钳 0。
func monitorDeltas(cur monitorSampleRow, prev *monitorSampleRow) (int64, int64) {
	if prev == nil {
		return 0, 0
	}
	dSent := cur.sent - prev.sent
	if dSent < 0 {
		dSent = 0
	}
	dRecv := cur.recv - prev.recv
	if dRecv < 0 {
		dRecv = 0
	}
	return dSent, dRecv
}

// windowDeltas 对齐 MonitorService::windowDeltas：窗口内增量合计。
// 同窗口内一条样本都没有 → (nil, nil)（与「有采样但都是首条」的 (0,0) 区分）。
func (h *AdminExtras) windowDeltas(ctx context.Context, nodeID int64, start, end time.Time) (*int64, *int64, error) {
	points, err := h.points(ctx, nodeID, start, end, "raw")
	if err != nil {
		return nil, nil, err
	}
	if len(points) == 0 {
		return nil, nil, nil
	}
	var up, down int64
	for _, p := range points {
		up += p.up
		down += p.down
	}
	return &up, &down, nil
}

// points 对齐 MonitorService::points：差分 + 分桶归并（核心）。
// 窗口首条的「前一条」取窗口外最近一条，取不到按 0 起算。
func (h *AdminExtras) points(ctx context.Context, nodeID int64, start, end time.Time, bucket string) ([]monitorPoint, error) {
	prev, err := h.previousSample(ctx, nodeID, start)
	if err != nil {
		return nil, err
	}
	rows, err := h.windowSamples(ctx, nodeID, start, end)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}

	buckets := map[int64]*monitorPoint{}
	for i := range rows {
		cur := rows[i]
		dSent, dRecv := monitorDeltas(cur, prev)
		p := cur
		prev = &p

		t := bucketStart(cur.sampledAt, bucket).Unix()
		bp, ok := buckets[t]
		if !ok {
			bp = &monitorPoint{t: t}
			buckets[t] = bp
		}
		bp.up += dSent
		bp.down += dRecv
	}

	return sortPoints(buckets), nil
}

// sortPoints 对齐 PHP ksort + array_values：按桶起点升序。
func sortPoints(buckets map[int64]*monitorPoint) []monitorPoint {
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	out := make([]monitorPoint, 0, len(keys))
	for _, k := range keys {
		out = append(out, *buckets[k])
	}
	return out
}

// previousSample 取窗口外最近一条（sampled_at < start，取不到 → nil）。
func (h *AdminExtras) previousSample(ctx context.Context, nodeID int64, start time.Time) (*monitorSampleRow, error) {
	var (
		sent, recv int64
		sampledAt  time.Time
	)
	err := h.db.QueryRowContext(ctx,
		`SELECT sent, recv, sampled_at FROM monitor_samples
		 WHERE node_id = ? AND sampled_at < ?
		 ORDER BY sampled_at DESC LIMIT 1`,
		nodeID, formatDBTime(start)).Scan(&sent, &recv, &sampledAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &monitorSampleRow{sent: sent, recv: recv, sampledAt: sampledAt}, nil
}

// windowSamples 取窗口内该节点的样本（sampled_at 升序，whereBetween 含两端）。
func (h *AdminExtras) windowSamples(ctx context.Context, nodeID int64, start, end time.Time) ([]monitorSampleRow, error) {
	rows, err := h.db.QueryContext(ctx,
		`SELECT sent, recv, sampled_at FROM monitor_samples
		 WHERE node_id = ? AND sampled_at BETWEEN ? AND ?
		 ORDER BY sampled_at ASC`,
		nodeID, formatDBTime(start), formatDBTime(end))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []monitorSampleRow{}
	for rows.Next() {
		var r monitorSampleRow
		if err := rows.Scan(&r.sent, &r.recv, &r.sampledAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// formatDBTime 对齐 Laravel 查询绑定：DateTime 格式化为 'Y-m-d H:i:s'（截断微秒）。
// 表列是 datetime（秒精度），比较也按秒精度做。
func formatDBTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// peakRate 对齐 MonitorService::peakRate：窗口内单个区间最大双向速率（B/s）及发生时刻。
// 并列取最早（严格大于才替换）；窗口内无样本 → (nil, nil)。
func (h *AdminExtras) peakRate(ctx context.Context, nodeID int64, start, end time.Time) (*float64, *time.Time, error) {
	prev, err := h.previousSample(ctx, nodeID, start)
	if err != nil {
		return nil, nil, err
	}
	rows, err := h.windowSamples(ctx, nodeID, start, end)
	if err != nil {
		return nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, nil
	}

	var peakBytes int64
	var peakAt *time.Time
	for i := range rows {
		cur := rows[i]
		dSent, dRecv := monitorDeltas(cur, prev)
		p := cur
		prev = &p

		bytes := dSent + dRecv
		// 严格大于才替换 → 并列取最早。首条无条件入选（peakAt 为 nil），
		// 所以「窗口内全为零流量」也会得到一个真实的峰值时刻（不是 null）。
		if peakAt == nil || bytes > peakBytes {
			peakBytes = bytes
			at := cur.sampledAt
			peakAt = &at
		}
	}

	rate := phpRound(float64(peakBytes)/float64(rawBucketSeconds), 2)
	return &rate, peakAt, nil
}

// peakAt 对齐 MonitorService::peakAt：某列最大值所在行的采样时刻，并列取最早。
// 无有效值 → nil。
func (h *AdminExtras) peakAt(ctx context.Context, nodeID int64, start, end time.Time, column string) (*time.Time, error) {
	if column != "cpu" && column != "mem" {
		return nil, nil
	}
	var sampledAt time.Time
	err := h.db.QueryRowContext(ctx,
		`SELECT sampled_at FROM monitor_samples
		 WHERE node_id = ? AND sampled_at BETWEEN ? AND ? AND `+column+` IS NOT NULL
		 ORDER BY `+column+` DESC, sampled_at ASC LIMIT 1`,
		nodeID, formatDBTime(start), formatDBTime(end)).Scan(&sampledAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sampledAt, nil
}

// monitorSummary 对齐 MonitorService::summary。
func (h *AdminExtras) monitorSummary(ctx context.Context, window string) (*jsObj, error) {
	start, end, key := h.monitorWindow(window)

	nodes, err := h.enabledNodes(ctx)
	if err != nil {
		return nil, err
	}

	nodeRows := make(jsArr, 0, len(nodes))
	var totalUp, totalDown int64

	for _, node := range nodes {
		upPtr, downPtr, err := h.windowDeltas(ctx, node.id, start, end)
		if err != nil {
			return nil, err
		}

		// 窗口内没有任何样本 → 流量、cpu/mem 与峰值时刻全 null
		if upPtr == nil && downPtr == nil {
			nodeRows = append(nodeRows, jo(
				"id", node.id,
				"name", node.name,
				"up", nil,
				"down", nil,
				"total", nil,
				"cpu_avg", nil,
				"cpu_max", nil,
				"mem_avg", nil,
				"mem_max", nil,
				"peak_rate", nil,
				"peak_rate_at", nil,
				"cpu_max_at", nil,
				"mem_max_at", nil,
			))
			continue
		}

		up, down := *upPtr, *downPtr
		stats, err := h.nodeStats(ctx, node.id, start, end)
		if err != nil {
			return nil, err
		}
		peakRate, peakRateAt, err := h.peakRate(ctx, node.id, start, end)
		if err != nil {
			return nil, err
		}
		cpuMaxAt, err := h.peakAt(ctx, node.id, start, end, "cpu")
		if err != nil {
			return nil, err
		}
		memMaxAt, err := h.peakAt(ctx, node.id, start, end, "mem")
		if err != nil {
			return nil, err
		}

		nodeRows = append(nodeRows, jo(
			"id", node.id,
			"name", node.name,
			"up", up,
			"down", down,
			"total", up+down,
			"cpu_avg", nullablePHPFloat(stats.cpuAvg),
			"cpu_max", nullablePHPFloat(stats.cpuMax),
			"mem_avg", nullablePHPFloat(stats.memAvg),
			"mem_max", nullablePHPFloat(stats.memMax),
			"peak_rate", nullablePHPFloat(peakRate),
			"peak_rate_at", nullableISO(peakRateAt),
			"cpu_max_at", nullableISO(cpuMaxAt),
			"mem_max_at", nullableISO(memMaxAt),
		))

		// 合计只累加有数据的节点（没有数据的节点是 null，跳过而非加 0）
		totalUp += up
		totalDown += down
	}

	return jo(
		"totals", jo("up", totalUp, "down", totalDown, "total", totalUp+totalDown),
		"window", jo("key", key, "start", isoTime(start), "end", isoTime(end)),
		"nodes", nodeRows,
	), nil
}

// monitorSeries 对齐 MonitorService::series：逐节点点列 + 末尾 aggregate 合计。
// 不改成 {nodes, aggregate} 对象：消费方按列表索引取（firstWhere('node_id', …)）。
func (h *AdminExtras) monitorSeries(ctx context.Context, window string) (jsArr, error) {
	start, end, key := h.monitorWindow(window)
	bucket := monitorBucket(key)

	nodes, err := h.enabledNodes(ctx)
	if err != nil {
		return nil, err
	}

	out := make(jsArr, 0, len(nodes)+1)
	totals := map[int64]*monitorPoint{}

	for _, node := range nodes {
		points, err := h.points(ctx, node.id, start, end, bucket)
		if err != nil {
			return nil, err
		}

		out = append(out, jo(
			"node_id", node.id,
			"name", node.name,
			"points", pointsArr(points),
		))

		// 汇总在后端做：各节点采样时刻有秒级差，前端按时间轴叠加会错位成假峰谷。
		// raw 模式先把 t 归到 5 分钟桶再加；hour/day 的 t 本就是桶起点。
		// 没有采样的节点不贡献任何桶（缺数据 ≠ 0）。
		for _, p := range points {
			t := p.t
			if bucket == "raw" {
				t = (t / rawBucketSeconds) * rawBucketSeconds
			}
			bp, ok := totals[t]
			if !ok {
				bp = &monitorPoint{t: t}
				totals[t] = bp
			}
			bp.up += p.up
			bp.down += p.down
		}
	}

	out = append(out, jo(
		"node_id", nil,
		"name", "合计",
		"aggregate", true,
		"points", pointsArr(sortPoints(totals)),
	))
	return out, nil
}

// nodeMeta 是启用节点的最小元信息。
type nodeMeta struct {
	id   int64
	name string
}

// enabledNodes 对齐 Node::where('enabled', true)->orderBy('id')。
func (h *AdminExtras) enabledNodes(ctx context.Context) ([]nodeMeta, error) {
	rows, err := h.db.QueryContext(ctx,
		"SELECT id, name FROM nodes WHERE enabled = 1 ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []nodeMeta{}
	for rows.Next() {
		var n nodeMeta
		if err := rows.Scan(&n.id, &n.name); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// nodeStats 对齐 summary 里的 selectRaw('AVG(cpu), MAX(cpu), AVG(mem), MAX(mem)')。
// 聚合值 round 到 2 位；没有有效值 → nil。
type nodeStatsResult struct {
	cpuAvg, cpuMax, memAvg, memMax *float64
}

func (h *AdminExtras) nodeStats(ctx context.Context, nodeID int64, start, end time.Time) (nodeStatsResult, error) {
	var cpuAvg, cpuMax, memAvg, memMax sql.NullFloat64
	err := h.db.QueryRowContext(ctx,
		`SELECT AVG(cpu), MAX(cpu), AVG(mem), MAX(mem) FROM monitor_samples
		 WHERE node_id = ? AND sampled_at BETWEEN ? AND ?`,
		nodeID, formatDBTime(start), formatDBTime(end)).
		Scan(&cpuAvg, &cpuMax, &memAvg, &memMax)
	if err != nil {
		return nodeStatsResult{}, err
	}

	roundPtr := func(n sql.NullFloat64) *float64 {
		if !n.Valid {
			return nil
		}
		v := phpRound(n.Float64, 2)
		return &v
	}
	return nodeStatsResult{
		cpuAvg: roundPtr(cpuAvg),
		cpuMax: roundPtr(cpuMax),
		memAvg: roundPtr(memAvg),
		memMax: roundPtr(memMax),
	}, nil
}

// pointsArr 把点列转成 PHP 的 [['t'=>int,'up'=>int,'down'=>int], …]。
func pointsArr(points []monitorPoint) jsArr {
	out := make(jsArr, 0, len(points))
	for _, p := range points {
		out = append(out, jo("t", p.t, "up", p.up, "down", p.down))
	}
	return out
}

// nullablePHPFloat 对齐 PHP 可空 (float) 字段：null 原样，否则按 phpFloat 口径输出。
func nullablePHPFloat(p *float64) any {
	if p == nil {
		return nil
	}
	return phpFloat(*p)
}

// phpRound 对齐 PHP round($v, $precision)（半数远离零）。
//
// PHP 的 round 按**十进制字面量**语义处理 .xx5 边界（round(10.005,2)=10.01），
// 而 float64 的 10.005 实际是 10.004999…，直接 math.Floor(x*100+0.5) 会得到
// 10.00。这里先取最短往返十进制表示（与 PHP echo $v 一致），再做十进制四舍五入。
func phpRound(v float64, precision int) float64 {
	if precision < 0 {
		precision = 0
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}

	dot := strings.IndexByte(s, '.')
	ip, fp := s, ""
	if dot >= 0 {
		ip, fp = s[:dot], s[dot+1:]
	}
	if ip == "" {
		ip = "0"
	}

	// 补齐到 precision+1 位（多留一位决定进舍）
	for len(fp) < precision+1 {
		fp += "0"
	}

	keep, rest := fp[:precision], fp[precision:]
	if rest[0] >= '5' {
		// keep + 1，十进制进位
		b := []byte(keep)
		i := len(b) - 1
		for ; i >= 0; i-- {
			if b[i] < '9' {
				b[i]++
				break
			}
			b[i] = '0'
		}
		if i < 0 {
			ib := []byte(ip)
			j := len(ib) - 1
			for ; j >= 0; j-- {
				if ib[j] < '9' {
					ib[j]++
					break
				}
				ib[j] = '0'
			}
			if j < 0 {
				ip = "1" + string(ib)
			} else {
				ip = string(ib)
			}
		}
		keep = string(b)
	}

	keep = strings.TrimRight(keep, "0")
	out := ip
	if keep != "" {
		out = ip + "." + keep
	}
	if neg && out != "0" {
		out = "-" + out
	}

	if f, err := strconv.ParseFloat(out, 64); err == nil {
		return f
	}
	// 兜底：极长小数等罕见情况退化为标量运算
	sh := math.Pow(10, float64(precision))
	abs := math.Floor(math.Abs(v)*sh+0.5) / sh
	if v < 0 {
		return -abs
	}
	return abs
}
