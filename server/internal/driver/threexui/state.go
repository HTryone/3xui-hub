package threexui

import "strings"

// ClientState 是 3x-ui client 在驱动层的归一化视图。
//
// 为什么需要它：同一个 client 在面板上被三种读法返回，字段命名互不一致（实测）——
//
//	GET /panel/api/inbounds/list → obj[].clientStats[]   email/enable/total/expiry_time   (snake_case)
//	GET /panel/api/inbounds/list → obj[].settings.clients[]  email/enable/totalGB/expiryTime (camelCase)
//	GET /panel/api/clients/get/{email} → client{}        email/enable/totalGB/expiryTime (camelCase)
//
// 调用方若各自 firstIntField(total,totalGB) 兜底（reconcile.go 现在就是这么干的），
// 每加一条读链就要再抄一遍命名兼容，且「null 到底是什么」散落在各处。这里统一收口。
//
// 归一化规则（与线上实测口径一致，见 collector/reconcile.go entryFromClientObject）：
//
//   - LimitBytes ← totalGB / total（名字叫 GB，值其实是**字节**；3x-ui 一直这么存）。
//   - ExpiryMs   ← expiryTime / expiry_time（毫秒时间戳；0 = 不过期）。
//   - 上面两个字段为 null 或键缺失 → 0。**0 是合法值（不限流量 / 不过期），不是「读不到」**，
//     早期版本把 total=null 当读不全，导致整轮 unknown、一条都没修（线上踩过）。
//   - Enable ← enable。只有它读不到才算未知：EnableKnown=false。未知 ≠ 关闭，
//     调用方不得据此改写面板（那是凭猜测停用用户）。
type ClientState struct {
	Email string
	// Enable 是面板给的开关；EnableKnown=false 时该值无意义（恒 false）。
	Enable bool
	// EnableKnown=false 表示 enable 字段缺失/为 null/不可判定 —— 未知，不是「关闭」。
	EnableKnown bool
	// LimitBytes 是流量上限（字节）；0 = 不限。
	LimitBytes int64
	// ExpiryMs 是到期时间（毫秒时间戳）；0 = 不过期。
	ExpiryMs int64
	// Inbounds 是该 client 挂载的入站 id（升序去重）。NormalizeClientState 只能从
	// 对象自带的 inboundId/inboundIds 读；批量快照由 Snapshot 按入站归属补齐。
	Inbounds []int64
}

// Known 报告这条 client 的关键字段是否读全（目前只有 enable 读不到算未知）。
func (s ClientState) Known() bool { return s.EnableKnown }

// NormalizeClientState 把任一种面板读法的 client 对象归一化成 ClientState。
//
// 兼容外层包一层 {"client": {...}} 的形态（clients/get 实测如此），email 前后空白会去掉。
func NormalizeClientState(obj map[string]any) ClientState {
	if inner, ok := obj["client"].(map[string]any); ok {
		obj = inner
	}

	var st ClientState
	if email, ok := obj["email"].(string); ok {
		st.Email = strings.TrimSpace(email)
	}

	st.Enable, st.EnableKnown = readEnable(obj)
	// 两种命名都要吃：clientStats 用 total/expiry_time，settings.clients 与 clients/get 用 totalGB/expiryTime。
	st.LimitBytes = readBytesValue(obj, "totalGB", "total")
	st.ExpiryMs = readIntValue(obj, "expiryTime", "expiry_time")
	st.Inbounds = inboundIDsFromObject(obj)
	return st
}

// readEnable 读 enable；ok=false 表示读不到（键缺失 / null / 无法判定的取值）。
//
// 必须同时吃 bool、json.Number（phpjson.Decode 用 UseNumber，`1` 会变成 Number("1")）、
// 数字字符串：只认 bool 会让真实节点上的 enable 全部「未知」。
func readEnable(obj map[string]any) (value bool, known bool) {
	v, ok := obj["enable"]
	if !ok || v == nil {
		return false, false
	}
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		switch strings.ToLower(strings.TrimSpace(t)) {
		case "true", "1":
			return true, true
		case "false", "0", "":
			return false, true
		}
		return false, false
	}
	if isNumeric(v) {
		return asFloat64(v) != 0, true
	}
	return false, false
}

// readBytesValue 按顺序取第一个「键存在且是数值」的字节数字段；都没有则 0（= 不限）。
//
// null 与缺失等价：3x-ui 用 null 表示「没有设置上限」，语义就是不限。
func readBytesValue(obj map[string]any, keys ...string) int64 {
	n := readIntValue(obj, keys...)
	if n < 0 {
		// 负的字节上限不是合法状态（计数器/上限不可能为负），按不限处理而不是当成巨大值。
		return 0
	}
	return n
}

// readIntValue 按顺序取第一个存在且是数值的整数/毫秒字段；都没有则 0。
//
// 字符串数字（"1048576"）与 json.Number 都收：面板不同版本给的类型不一样。
// 非数值（对象、数组、乱码字符串）一律跳过，不 panic 也不当成 0 覆盖掉后面的键。
func readIntValue(obj map[string]any, keys ...string) int64 {
	for _, key := range keys {
		v, ok := obj[key]
		if !ok || v == nil || !isNumeric(v) {
			continue
		}
		return asInt64(v)
	}
	return 0
}

// inboundIDsFromObject 读对象自带的入站归属（个别版本 clients/get 会带 inboundId/inboundIds）。
func inboundIDsFromObject(obj map[string]any) []int64 {
	var out []int64
	for _, key := range []string{"inboundId", "inbound_id"} {
		v, ok := obj[key]
		if !ok || v == nil || !isNumeric(v) {
			continue
		}
		out = addInboundID(out, asInt64(v))
	}
	for _, key := range []string{"inboundIds", "inbound_ids"} {
		arr, ok := obj[key].([]any)
		if !ok {
			continue
		}
		for _, item := range arr {
			if item == nil || !isNumeric(item) {
				continue
			}
			out = addInboundID(out, asInt64(item))
		}
	}
	return out
}

// addInboundID 追加一个入站 id（去重、忽略非正数、保持升序）。
func addInboundID(ids []int64, id int64) []int64 {
	if id <= 0 {
		return ids
	}
	for _, existing := range ids {
		if existing == id {
			return ids
		}
	}
	ids = append(ids, id)
	// 入站数量极少（个位数），插入排序比每次 sort.Slice 更省事也更容易读。
	for i := len(ids) - 1; i > 0 && ids[i] < ids[i-1]; i-- {
		ids[i], ids[i-1] = ids[i-1], ids[i]
	}
	return ids
}

// mergeClientState 把同一 email 的多次读法并起来（跨入站、跨 clientStats/settings）。
// prev == nil 表示这个 email 第一次出现，直接收下 next。
//
// 规则：
//   - LimitBytes / ExpiryMs：保留先到的值。调用方按 clientStats → settings.clients 的顺序喂，
//     即**运行态优先**：clientStats 是 client_traffics 行，就是 xray 实际执行的限额；
//     settings.clients 只是面板侧配置副本，两者打架时以运行态为准。
//     0（不限 / 不过期）是合法值，不能因为「看起来空」就被后面的副本覆盖；
//   - Enable：**已知压过未知**。同一 email 在 clientStats 里可能不带 enable 而 settings 带，
//     丢掉可知的信息没有意义；两条都已知时保留先到的；
//   - Inbounds：并集（升序去重）。
func mergeClientState(prev *ClientState, next ClientState) ClientState {
	if prev == nil {
		return next
	}
	out := *prev
	if out.Email == "" {
		out.Email = next.Email
	}
	if !out.EnableKnown && next.EnableKnown {
		out.Enable = next.Enable
		out.EnableKnown = true
	}
	for _, id := range next.Inbounds {
		out.Inbounds = addInboundID(out.Inbounds, id)
	}
	return out
}
