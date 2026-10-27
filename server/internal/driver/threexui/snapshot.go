package threexui

import (
	"context"
	"errors"
	"sort"
	"strings"

	"controlhub-server/internal/driver/internal/phpjson"
)

// Snapshot 是驱动层「全量读一次节点状况」的入口：归一化后的 client 集合 +
// 本轮确实探测过的 email 集合。
//
// Probed 与 Clients 必须分开，两者语义相反：
//   - email 在 Probed 里、不在 Clients 里 → **确认不存在**（可以据此补注册）；
//   - email 不在 Probed 里                  → **本轮没探测**（什么都不能推断）。
//
// 把「没看」当成「缺人」，或者把「查询失败」当成「没有」，就是重复建号 / 误停用户的来源。
//
// Complete 回答的是另一个问题：这份 Clients 是否**枚举了节点上的全部 client**。
// 只有它为 true 时，「不在 Clients 里」才等价于「确认不存在」；
// 逐用户探测（Complete=false）只能对 Probed 里的 email 下结论。
type Snapshot struct {
	// Clients 按 email 索引的归一化 client 状态。
	Clients map[string]ClientState
	// Probed 是本轮确实得到结论的 email 集合（存在，或确认不存在）。
	Probed map[string]bool
	// ProbeFailed 是退化路径里「探测本身失败」的次数（既不算存在也不算不存在）。
	ProbeFailed int
	// Source 是本次数据的来源：SnapshotInbounds（批量读）或 SnapshotProbe（逐用户退化）。
	Source string
	// Complete=true 表示 Clients 是节点的全量枚举（inbounds/list 成功）：
	// 任何没出现的 email 都是确认不存在。false 表示只是抽样探测，只能看 Probed。
	Complete bool
}

// 快照来源标识。
const (
	SnapshotInbounds = "inbounds"
	SnapshotProbe    = "probe"
)

// ErrSnapshotNoSource 表示批量读与逐用户退化两条路径都没拿到数据。
// 调用方必须把它当「未知」处理：不能据此认定节点上一个 client 都没有。
var ErrSnapshotNoSource = errors.New("threexui: snapshot unavailable (inbounds/list and per-client probe both failed)")

// Snapshot 全量读入口（无候选名单）：
//  1. 先走 inbounds/list —— 一次请求拿到全部入站 + clientStats + settings.clients；
//  2. inbounds/list 失败（网络/业务错）时退化到 clients/list 列出的 email 逐个 GetClientStrict。
//
// 无候选名单时退化路径只能读到 clients/list 返回的 email：API 实测它只回少量非本站 email，
// 不是全量快照，因此这条路径的 Coverage 是**不完整**的（Source 会标 probe，Probed 里
// 只有真探测过的那些）。需要按已知用户全量探测时用 SnapshotFor。
//
// 两条路径都拿不到「可枚举的名单」时返回包装了 ErrSnapshotNoSource 的错误。
//
// 注意：只要求**名单**拿得到。名单拿到了、但逐个探测全失败的情况不报错 ——
// 此时 Probed 为空，调用方无论如何都推不出「谁不存在」（见 Snapshot.Probed 的约定），
// 用 ProbeFailed 计数即可；报错反而会让调用方丢掉这个计数。
func (c *Client) Snapshot(ctx context.Context) (Snapshot, error) {
	snap, err := c.snapshotFromInbounds(ctx)
	if err == nil {
		return snap, nil
	}
	// 批量读失败：只要不是 ctx 取消，就退化逐用户探测（节点偶发 5xx 时不必整轮放弃）。
	// ctx 已取消时继续打面板没有意义，原样上抛。
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Snapshot{}, err
	}

	probed, probeErr := c.snapshotFromClientsList(ctx)
	if probeErr != nil {
		return Snapshot{}, errors.Join(ErrSnapshotNoSource, err, probeErr)
	}
	return probed, nil
}

// SnapshotFor 全量读入口（带候选名单）：inbounds/list 成功时直接归一化全量入站数据；
// 失败时**只对 emails 逐个** GetClientStrict 退化探测。
//
// emails 为空时等价于 Snapshot(ctx)。退化路径的轮转/预算/跨轮补齐属于调用方策略
// （见 collector/reconcile.go），驱动层这里只负责「按名单探测 + 忠实标注 Probed」。
func (c *Client) SnapshotFor(ctx context.Context, emails []string) (Snapshot, error) {
	snap, err := c.snapshotFromInbounds(ctx)
	if err == nil {
		return snap, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Snapshot{}, err
	}
	if len(emails) == 0 {
		// 批量读已失败，这里不能再走 Snapshot()（那会重试 inbounds/list 一遍），
		// 直接落到 clients/list 退化路径。
		probed, probeErr := c.snapshotFromClientsList(ctx)
		if probeErr != nil {
			return Snapshot{}, errors.Join(ErrSnapshotNoSource, err, probeErr)
		}
		return probed, nil
	}

	out := Snapshot{
		Clients: map[string]ClientState{},
		Probed:  map[string]bool{},
		Source:  SnapshotProbe,
	}
	for _, email := range emails {
		email = strings.TrimSpace(email)
		if email == "" || out.Probed[email] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		obj, err := c.GetClientStrict(ctx, email)
		if err != nil {
			if IsClientNotFound(err) {
				out.Probed[email] = true // 面板明确说没有：可以据此补注册
				continue
			}
			// 探测失败（超时/5xx/登录态被顶）：**不算探测过**，留待下一轮。
			out.ProbeFailed++
			continue
		}
		if obj == nil {
			out.Probed[email] = true
			continue
		}
		st := NormalizeClientState(obj)
		if st.Email == "" {
			st.Email = email
		}
		out.Clients[email] = st
		out.Probed[email] = true
	}

	// 全部探测失败时也不报错：Probed 为空 + ProbeFailed 计数，调用方据此判定「本轮没看到人」。
	// 按名单探测的语义是「名单已知、看它们在不在」，单个探测失败不是整轮失败。
	return out, nil
}

// snapshotFromInbounds 一次 inbounds/list 归一化全量 client。
func (c *Client) snapshotFromInbounds(ctx context.Context) (Snapshot, error) {
	inbounds, err := c.ListInbounds(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	out := Snapshot{
		Clients:  map[string]ClientState{},
		Probed:   map[string]bool{},
		Source:   SnapshotInbounds,
		Complete: true,
	}
	for _, inbound := range inbounds {
		inboundID := readInboundID(inbound)
		for _, obj := range clientObjectsOfInbound(inbound) {
			st := NormalizeClientState(obj)
			if st.Email == "" {
				continue // 没有 email 的行不是我们的 client（面板侧脏数据），无法索引
			}
			if inboundID > 0 {
				st.Inbounds = addInboundID(st.Inbounds, inboundID)
			}
			next := mergeClientState(existingState(out.Clients, st.Email), st)
			out.Clients[st.Email] = next
			out.Probed[st.Email] = true
		}
	}
	return out, nil
}

// snapshotFromClientsList 退化路径：clients/list 的 email 逐个 GetClientStrict。
//
// 只用于 Snapshot(ctx) 无名单的场景；clients/list 实测不是全量快照，
// 所以这里拿到的 Probed 集合只代表「这些 email 探测过」，不代表「节点上只有这些人」。
func (c *Client) snapshotFromClientsList(ctx context.Context) (Snapshot, error) {
	raw, err := c.ListClients(ctx)
	if err != nil {
		return Snapshot{}, err
	}

	out := Snapshot{
		Clients: map[string]ClientState{},
		Probed:  map[string]bool{},
		Source:  SnapshotProbe,
	}
	// 名单已拿到：逐个探测全部失败也不报错（同 SnapshotFor），Probed 为空即代表本轮没看到人。
	for _, item := range raw {
		email, _ := item["email"].(string)
		email = strings.TrimSpace(email)
		if email == "" || out.Probed[email] {
			continue
		}
		if err := ctx.Err(); err != nil {
			return Snapshot{}, err
		}
		obj, err := c.GetClientStrict(ctx, email)
		if err != nil {
			if IsClientNotFound(err) {
				out.Probed[email] = true
				continue
			}
			out.ProbeFailed++
			continue
		}
		if obj == nil {
			out.Probed[email] = true
			continue
		}
		st := NormalizeClientState(obj)
		if st.Email == "" {
			st.Email = email
		}
		if prev := existingState(out.Clients, email); prev != nil {
			st = mergeClientState(prev, st)
		}
		out.Clients[email] = st
		out.Probed[email] = true
	}
	return out, nil
}

// Emails 返回快照里所有存在的 email（升序，便于日志与测试比对）。
func (s Snapshot) Emails() []string {
	out := make([]string, 0, len(s.Clients))
	for email := range s.Clients {
		out = append(out, email)
	}
	sort.Strings(out)
	return out
}

// existingState 取已归一化的状态（mergeClientState 用；不存在返回 nil）。
func existingState(m map[string]ClientState, email string) *ClientState {
	if st, ok := m[email]; ok {
		return &st
	}
	return nil
}

// readInboundID 读入站对象的 id（个别版本给数字字符串）。
func readInboundID(inbound map[string]any) int64 {
	return readIntValue(inbound, "id")
}

// clientObjectsOfInbound 从入站对象里取出它的 client 列表。两种形态都要吃：
//
//   - clientStats（本版实测存在）：client_traffics 行，snake_case（total / expiry_time）；
//   - settings.clients（老形态）：字符串或已解析对象都可能，camelCase（totalGB / expiryTime）。
//
// 同一 client 通常两种都有（clientStats 带流量、settings 带入站级配置），
// 交给 mergeClientState 合并，而不是二选一丢信息。
//
// 顺序很关键：clientStats 先输出，mergeClientState 保留先到的 LimitBytes/ExpiryMs，
// 于是取的是 client_traffics 的运行态值（xray 实际执行的限额），
// settings.clients 只在 clientStats 没给出该字段时补齐。
func clientObjectsOfInbound(inbound map[string]any) []map[string]any {
	var out []map[string]any
	for _, item := range asObjectList(inbound["clientStats"]) {
		out = append(out, item)
	}
	for _, item := range asObjectList(settingsClients(inbound["settings"])) {
		out = append(out, item)
	}
	return out
}

// settingsClients 取出 settings.clients 数组。settings 可能是 JSON 字符串（面板原样返回）
// 或已解析对象；解析失败 / 结构不符时返回 nil（不影响 clientStats 形态）。
//
// 用 phpjson.Decode 而不是 encoding/json：与 ListInbounds 的解析口径一致（json.Number 保原文），
// 避免同一条 expiryTime 在两种读法下变成不同类型。
func settingsClients(settings any) any {
	switch s := settings.(type) {
	case string:
		if strings.TrimSpace(s) == "" {
			return nil
		}
		parsed, err := phpjson.DecodeObject([]byte(s))
		if err != nil {
			return nil
		}
		return parsed["clients"]
	case map[string]any:
		return s["clients"]
	}
	return nil
}
