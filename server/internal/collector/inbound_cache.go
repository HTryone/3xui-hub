package collector

// 节点级 inbounds/list 共享快照（TTL + singleflight）。
//
// 为什么要有这一层：traffic-sync（RunTrafficSync）与 reconcile（ReconcileNodes）都要
// 每台 3x-ui 节点的 `GET /panel/api/inbounds/list`，两个任务又同频（每 5 分钟），
// 于是同一份大 JSON 每节点每轮被拉两遍、各解析一遍。这里按 nodeID 缓存**解析后**的结果，
// 两个任务共用一份：
//
//   - 缓存键 = nodeID；值 = 一次解析同时产出的两个视图：
//     Stats   （sync 视图：clientStats 的 up/down/enable，同 email 跨入站只取首次）
//     Entries （reconcile 视图：clientsOfInbound + entryFromClientObject，带 Incomplete 标记）
//   - TTL 内直接命中；过期重新拉；**失败不写缓存**（连不上/超时/业务错都不会留下脏快照，
//     下一轮立刻重试，不会被错误结果糊住 60 秒）。
//   - 单飞：同一节点同时有多个调用方时只放一个真去拉，其余等它的结果（8 个并发 = 1 次 HTTP）。
//   - 对账在节点上写完 enable/补注册后调用 invalidateInboundSnapshot：否则 TTL 内
//     流量同步会读到「写入前」的 enable，把刚修好的人误报成漂移（只影响 75 秒窗口，
//     但假告警会写 drift 记录，没必要留）。
//
// 口径说明（与驱动层逐条对齐，不可「顺手简化」）：
//   - Stats 只吃 `clientStats`（snake_case，本版实测），字段缺失的 enable 按 true
//     —— 与 threexui.enableOr(stat, true) 一致：老版本不返回 enable 时不能误判成关闭；
//   - up/down 用本包的 asInt64（json.Number 优先），与驱动层 asInt64 的主路径同口径；
//   - Entries 完全复用 reconcile.go 里既有的 clientsOfInbound / entryFromClientObject，
//     避免两处解析出现漂移（对账的字段读不全判定比 sync 严格，不能混用）。

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"controlhub-server/internal/driver/threexui"
)

// defaultInboundSnapshotTTL 是共享快照的存活时长。
//
// 60-90 秒：两个任务的最长错峰间隔远小于它，同频触发时必然命中同一次拉取；
// 又不能太长 —— 节点上的 enable/total 变化要能在一分钟内被下一轮看到。
const defaultInboundSnapshotTTL = 75 * time.Second

// inboundSnapshot 是一次 inbounds/list 解析后的两个视图（只读共享，调用方不得改写）。
type inboundSnapshot struct {
	Stats      map[string]ClientStat        // sync 视图
	Entries    map[string]nodeSnapshotEntry // reconcile 视图
	KeysSample []string                     // 「字段读不全」的键名样本（每种读法最多一条）
}

// inboundCacheEntry 是缓存里的一格：解析结果 + 过期时刻。
type inboundCacheEntry struct {
	snap      *inboundSnapshot
	expiresAt time.Time
}

// inboundFetch 是一次进行中的拉取（singleflight 的等待对象）。
type inboundFetch struct {
	done chan struct{}
	snap *inboundSnapshot
	err  error
}

// inboundSnapshotCache 是节点级共享快照缓存。
//
// 零值可用（ttl==0 = 不缓存，直接透传每次拉取）——测试里直接构造 &Collector{} 的老用例
// 因此保持原有行为；生产路径由 New() 写入 defaultInboundSnapshotTTL。
type inboundSnapshotCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	entries  map[int64]inboundCacheEntry
	inflight map[int64]*inboundFetch
}

// cachedInbounds 取该节点的共享快照：命中缓存直接返回，否则（单飞地）拉一次并解析。
//
// 失败一律不写缓存；等待中的调用方拿到 leader 的结果（含错误），leader 的 ctx 被取消时
// 跟随返回错误 —— 下一轮因没有缓存会重新拉取，不会永久卡住。
func (c *Collector) cachedInbounds(ctx context.Context, nodeID int64, client *threexui.Client) (*inboundSnapshot, error) {
	return c.cachedInboundsWith(ctx, nodeID, func(ctx context.Context) (*inboundSnapshot, error) {
		inbounds, err := client.ListInbounds(ctx)
		if err != nil {
			return nil, err
		}
		return parseInboundSnapshot(inbounds), nil
	})
}

// cachedInboundsWith 是 cachedInbounds 的可注入版本（fetch=实际拉取+解析）。
//
// 单独抽出来是为了让缓存自身的行为（TTL/单飞/失败不落盘）能被独立测试，
// 不必为此改 driver 包 —— 上一个任务已经验证过「新建驱动代码没人接」的教训。
func (c *Collector) cachedInboundsWith(ctx context.Context, nodeID int64,
	fetch func(context.Context) (*inboundSnapshot, error)) (*inboundSnapshot, error) {

	cache := &c.inbounds
	now := c.now()

	cache.mu.Lock()
	if cache.ttl > 0 {
		if e, ok := cache.entries[nodeID]; ok && now.Before(e.expiresAt) {
			cache.mu.Unlock()
			return e.snap, nil
		}
	}
	if f, ok := cache.inflight[nodeID]; ok {
		cache.mu.Unlock()
		// 等 leader：结果（含错误）是它刚刚真拉回来的那份，与写进缓存的是同一指针。
		// 等完不重捞缓存 —— 单飞窗口内不会有第二个 leader，缓存内容只可能来自这次拉取。
		select {
		case <-f.done:
			return f.snap, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f := &inboundFetch{done: make(chan struct{})}
	if cache.inflight == nil {
		cache.inflight = map[int64]*inboundFetch{}
	}
	cache.inflight[nodeID] = f
	cache.mu.Unlock()

	snap, err := fetch(ctx)

	cache.mu.Lock()
	delete(cache.inflight, nodeID)
	if err == nil && cache.ttl > 0 {
		if cache.entries == nil {
			cache.entries = map[int64]inboundCacheEntry{}
		}
		cache.entries[nodeID] = inboundCacheEntry{snap: snap, expiresAt: c.now().Add(cache.ttl)}
	}
	cache.mu.Unlock()
	f.snap, f.err = snap, err
	close(f.done)

	return snap, err
}

// invalidateInboundSnapshot 丢掉某节点的缓存（节点上刚写过 client 状态时调用）。
func (c *Collector) invalidateInboundSnapshot(nodeID int64) {
	cache := &c.inbounds
	cache.mu.Lock()
	delete(cache.entries, nodeID)
	cache.mu.Unlock()
}

// clearInboundSnapshots 丢掉全部节点的缓存（一轮对账收尾有过跨节点写入时调用）。
func (c *Collector) clearInboundSnapshots() {
	cache := &c.inbounds
	cache.mu.Lock()
	cache.entries = nil
	cache.mu.Unlock()
}

// parseInboundSnapshot 把一次 inbounds/list 的结果解析成两个视图（纯函数，便于单测）。
func parseInboundSnapshot(inbounds []map[string]any) *inboundSnapshot {
	snap := &inboundSnapshot{
		Stats:   make(map[string]ClientStat, len(inbounds)),
		Entries: make(map[string]nodeSnapshotEntry),
	}

	// ── sync 视图：只吃 clientStats，同 email 跨入站只取首次（不跨入站累加）──
	// 依据与驱动层 GetClientStatsByEmail 一致：3x-ui 的 client_traffics 表 Email 唯一，
	// 同一 email 出现在多个入站下时携带的是同一份全局累计值。
	for _, ib := range inbounds {
		for _, stat := range inboundClientStats(ib) {
			email, _ := stat["email"].(string)
			if email == "" {
				continue
			}
			if _, seen := snap.Stats[email]; seen {
				continue // 同 email 跨入站重复，只取首次
			}
			up, _ := asInt64(stat["up"])
			down, _ := asInt64(stat["down"])
			snap.Stats[email] = ClientStat{Up: up, Down: down, Enable: statEnable(stat)}
		}
	}

	// ── reconcile 视图：与 snapshotFromInbounds 原逻辑逐字一致 ──
	// clientStats 优先、settings.clients 兜底；enable 读不到才标 Incomplete
	// （total/expiry 缺失按 0 = 不限/不过期，是合法值）。
	// 同 email 跨入站出现时，**入站归属取并集**（对账要用它判断新入站有没有挂上）。
	for _, ib := range inbounds {
		ibID := inboundIDOf(ib)
		for _, obj := range clientsOfInbound(ib) {
			email := strings.TrimSpace(stringOf(obj["email"]))
			if email == "" {
				continue
			}
			e := entryFromClientObject(obj)
			if e.Incomplete && snap.KeysSample == nil {
				snap.KeysSample = objKeyNames(obj)
			}
			if prev, ok := snap.Entries[email]; ok {
				prev.Inbounds = appendUniqueID(prev.Inbounds, ibID)
				switch {
				case !prev.Incomplete && !e.Incomplete:
					// 已经读全：只并入站归属，字段保持首条（同 email 是同一份全局定义）
					snap.Entries[email] = prev
				case prev.Incomplete && e.Incomplete:
					snap.Entries[email] = prev
				default:
					// prev 不全、本条读全 → 用本条的字段覆盖，但保留合并后的入站归属
					e.Inbounds = prev.Inbounds
					snap.Entries[email] = e
				}
				continue
			}
			e.Inbounds = appendUniqueID(nil, ibID)
			snap.Entries[email] = e
		}
	}
	return snap
}

// inboundIDOf 读入站对象的 id（个别版本给数字字符串；读不到返回 0）。
func inboundIDOf(ib map[string]any) int64 {
	if v, ok := asInt64(ib["id"]); ok {
		return v
	}
	return 0
}

// appendUniqueID 追加一个入站 id（去重、忽略非正数、保持升序）。
func appendUniqueID(ids []int64, id int64) []int64 {
	if id <= 0 {
		return ids
	}
	for _, have := range ids {
		if have == id {
			return ids
		}
	}
	ids = append(ids, id)
	sortIDs(ids)
	return ids
}

// sortIDs 就地升序（入站数量个位数，插入排序足够）。
func sortIDs(ids []int64) {
	for i := 1; i < len(ids); i++ {
		for j := i; j > 0 && ids[j] < ids[j-1]; j-- {
			ids[j], ids[j-1] = ids[j-1], ids[j]
		}
	}
}

// missingInbounds 返回 want 里 have 没有的入站 id（升序）。
func missingInbounds(want, have []int64) []int64 {
	if len(want) == 0 {
		return nil
	}
	set := make(map[int64]bool, len(have))
	for _, id := range have {
		set[id] = true
	}
	var out []int64
	for _, id := range want {
		if !set[id] {
			out = append(out, id)
		}
	}
	return out
}

// inboundClientStats 取入站的 clientStats 数组（本版 3x-ui 实测存在，snake_case）。
func inboundClientStats(ib map[string]any) []map[string]any {
	arr, ok := ib["clientStats"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// statEnable 复刻驱动层 enableOr(stat, true)：字段缺失按 **true**（不能误判成关闭），
// 其余按 truthy 口径（bool 取本身；字符串/JSON 数字按 "非空且非 0"）。
//
// ⚠️ 数字必须单独认 json.Number：驱动层 Decode 用 UseNumber（保大整数原文），
// 真实节点上的 enable 可能是 1/0 而不是 true/false。
func statEnable(stat map[string]any) bool {
	v, ok := stat["enable"]
	if !ok || v == nil {
		return true
	}
	switch t := v.(type) {
	case bool:
		return t
	case json.Number:
		s := t.String()
		return s != "" && s != "0"
	case string:
		return t != "" && t != "0"
	default:
		// 对齐驱动层 truthy 的 default 分支：其余类型（含 float64）按真值处理。
		return true
	}
}
