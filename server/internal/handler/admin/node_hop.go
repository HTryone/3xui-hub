package admin

// HY2 端口跳跃的节点侧联动（issue #94 R1–R3）。
//
// 面板不自己动防火墙：四个接口都只把**白名单载荷**入队成一条 node_api_commands，
// 再同步等回执 —— 与 reality_scan / outbound_test 同一条指令通道，不另造队列。
//
//	POST /nodes/:node/hop/probe    → port_probe            （只读，挑空闲区间）
//	POST /nodes/:node/hop/apply    → firewall_hop_apply    （写 nat 表 + 自验 + 持久化）
//	POST /nodes/:node/hop/remove   → firewall_hop_remove
//	POST /nodes/:node/hop/status   → firewall_hop_status   （只读，真实运行态）
//
// agent ≤ 1.0.6 不认这些 action，只会回 `unknown action: …`。所以入队前先按节点上报的
// agent_version 卡门槛：不够就 4xx 直接拒绝，**不产生任何指令记录**（issue #94 的验收硬指标）。

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"controlhub-server/internal/nodeapi"

	"github.com/gin-gonic/gin"
)

const (
	// minHopAgentVersion 是端口跳跃所需的最低 agent 版本（这些 action 从该版本起注册）。
	minHopAgentVersion = "1.0.8"
	// defaultHopProbePool 是「自动分配区间」的默认搜索池（与 issue #94 的示例一致）。
	defaultHopProbePool = "20000-50000"
	// defaultHopProbeCount 是默认申请的连续端口数。
	defaultHopProbeCount = 200
	// hopProbeMaxCount 与 agent 侧 hopCountMax 对齐，超了不必打扰节点。
	hopProbeMaxCount = 4096
)

// hopProbe / hopApply / hopRemove / hopStatus 是四个转发入口。

// hopProbe 转发 port_probe：在 pool 里找 count 个连续空闲端口。
//
// 传入 inbound_id 时，面板会把该入站的主端口与同节点其它启用入站的端口一起塞进
// exclude —— agent 只认 /proc/net 的占用，看不到「面板已经分配但还没监听」的端口。
func (d *NodeDomain) hopProbe(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	if !d.requireHopAgent(c, node) {
		return
	}

	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("inbound_id", "nullable", "integer", "min:1", "max:2000000000")
	v.field("pool", "nullable", "string", "max:32")
	v.field("count", "nullable", "integer", "min:1", "max:4096")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	pool := strings.TrimSpace(nodeStringField(input, "pool"))
	if pool == "" {
		pool = defaultHopProbePool
	}
	if _, _, ok := parsePortSpan(pool); !ok {
		d.abort422(c, "搜索区间格式错误：应形如 20000-50000（端口 1-65535，左端不大于右端）")
		return
	}

	count := int64(defaultHopProbeCount)
	if raw, present := input["count"]; present && raw != nil {
		if n := inputInt(raw); n > 0 {
			count = n
		}
	}

	payload := jo(
		"pool", pool,
		"count", count,
		"exclude", jsArr{},
	)
	if exclude := d.hopExcludePorts(c, node, inputInt(input["inbound_id"])); len(exclude) > 0 {
		payload.put("exclude", hopPortsJSArr(exclude))
	}

	d.runHopCommand(c, node, nodeapi.ActionPortProbe, payload)
}

// hopApply 转发 firewall_hop_apply。
//
// 主端口永远取自库里的入站记录（不接受前端传值）：主端口一旦被伪造，agent 侧
// REDIRECT 会把别的端口流量绕进这个入站。
func (d *NodeDomain) hopApply(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	if !d.requireHopAgent(c, node) {
		return
	}

	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("inbound_id", "required", "integer", "min:1", "max:2000000000")
	v.field("ports", "nullable", "string", "max:64")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	inbound, err := d.store.XrayInboundByID(ctxOf(c), node.ID, uint64(inputInt(input["inbound_id"])))
	if err != nil {
		if d.noRowsToNotFound(c, err) {
			return
		}
		d.failServer(c, "查询入站", err)
		return
	}
	if inbound.Protocol != "hysteria" {
		d.abort422(c, "端口跳跃只支持 hysteria（HY2）入站")
		return
	}

	ports := strings.TrimSpace(nodeStringField(input, "ports"))
	if ports == "" {
		ports = hopPortsOf(inbound)
	}
	if ports == "" {
		d.abort422(c, "该入站未配置跳跃区间：请先在入站表单里填「跳跃区间」，或探测后指定区间")
		return
	}
	lo, hi, ok := parsePortSpan(ports)
	if !ok {
		d.abort422(c, "跳跃区间格式错误：应形如 20000-20199（端口 1-65535，左端不大于右端）")
		return
	}
	// 与入站表单同一条硬约束：主端口落在区间内会被自己的 REDIRECT 绕回去。
	if inbound.Port >= lo && inbound.Port <= hi {
		d.abort422(c, "主端口不能落在跳跃区间内（"+ports+"）：主端口自身的流量会被转发规则误绕，连接会断")
		return
	}

	d.runHopCommand(c, node, nodeapi.ActionFirewallHopApply, jo(
		"inbound_id", int64(inbound.ID),
		"main_port", inbound.Port,
		"ports", ports,
		"proto", "udp",
	))
}

// hopRemove 转发 firewall_hop_remove（只删该入站自己的 HY2HOP-<id> 链）。
//
// 不校验入站在库：入站被删掉后链可能还留在节点上，这里正是清理它的入口。
func (d *NodeDomain) hopRemove(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	if !d.requireHopAgent(c, node) {
		return
	}

	input := jsonInput(c)
	v := newNodeValidator(ctxOf(c), input)
	v.field("inbound_id", "required", "integer", "min:1", "max:2000000000")
	if !v.passes() {
		d.failNodeValidation(c, v)
		return
	}

	d.runHopCommand(c, node, nodeapi.ActionFirewallHopRemove,
		jo("inbound_id", inputInt(input["inbound_id"])))
}

// hopStatus 转发 firewall_hop_status：列出节点上所有 HY2HOP-* 链的真实运行态。
//
// 面板「端口跳跃」列显示的是**实际状态**，不是数据库里的意图 —— 节点重启丢了规则、
// 手工改过链，这里立刻能看出来。
func (d *NodeDomain) hopStatus(c *gin.Context) {
	node, ok := d.loadXrayNode(c)
	if !ok {
		return
	}
	if !d.requireHopAgent(c, node) {
		return
	}
	d.runHopCommand(c, node, nodeapi.ActionFirewallHopStatus, jo())
}

// ---- 内部 ----

// requireHopAgent 在入队前拦掉 agent 版本不足的节点。
//
// 返回 false 时已经写好 4xx 响应，且**没有**产生 node_api_commands 记录。
func (d *NodeDomain) requireHopAgent(c *gin.Context, node *NodeRecord) bool {
	return d.requireAgentAction(c, node, minHopAgentVersion, "端口跳跃")
}

// requireAgentAction 是「某个 action 需要的最低 agent 版本」通用门槛。
//
// 返回 false 时已经写好 4xx 响应，且**没有**产生 node_api_commands 记录 ——
// 老 agent 收到不认识的 action 只会回 unknown action，留一条垃圾指令毫无意义。
func (d *NodeDomain) requireAgentAction(c *gin.Context, node *NodeRecord, minVersion, feature string) bool {
	if agentSupports(node, minVersion) {
		return true
	}
	have := strings.TrimSpace(nodeScalarString(nodeDriverConfigString(node, "agent_version")))
	msg := "需升级 agent：" + feature + "要求 agent ≥ " + minVersion + "（当前未上报版本，请先在「节点」页升级 agent）"
	if have != "" {
		msg = "需升级 agent：" + feature + "要求 agent ≥ " + minVersion + "（当前 " + have + "，请先在「节点」页升级 agent）"
	}
	c.Data(http.StatusPreconditionFailed, "application/json; charset=utf-8",
		phpEncode(jo("code", http.StatusPreconditionFailed, "msg", msg, "data", nil)))
	return false
}

// agentSupportsHop 判断节点上报的 agent_version 是否够端口跳跃。
// 未上报 / 解析不了都算不够：无法确认就不下发。
func agentSupportsHop(node *NodeRecord) bool {
	return agentSupports(node, minHopAgentVersion)
}

// agentSupports 判断节点上报的 agent_version 是否达到 minVersion。
func agentSupports(node *NodeRecord, minVersion string) bool {
	return versionAtLeast(nodeScalarString(nodeDriverConfigString(node, "agent_version")), minVersion)
}

// versionAtLeast 比较点分版本（忽略前导 v 与 -beta/+build 后缀）。
// 解析不了返回 false（宁可拒发，也不把不认识的 action 丢给老 agent）。
func versionAtLeast(have, need string) bool {
	h, ok := parseVersionTriple(have)
	if !ok {
		return false
	}
	n, ok := parseVersionTriple(need)
	if !ok {
		return false
	}
	for i := 0; i < len(h); i++ {
		if h[i] != n[i] {
			return h[i] > n[i]
		}
	}
	return true
}

// parseVersionTriple 取 major.minor.patch；缺的段按 0 补齐。
func parseVersionTriple(raw string) ([3]int, bool) {
	var out [3]int
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return out, false
	}
	parts := strings.Split(s, ".")
	if len(parts) > len(out) {
		parts = parts[:len(out)]
	}
	for i, part := range parts {
		digits := 0
		for digits < len(part) && part[digits] >= '0' && part[digits] <= '9' {
			digits++
		}
		if digits == 0 {
			return out, false
		}
		n, err := strconv.Atoi(part[:digits])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// hopPortsOf 读入站已配置的跳跃区间（streamSettings.finalmask.quicParams.udpHop.ports）。
func hopPortsOf(ib *XrayInboundRecord) string {
	stream, err := parseOrderedJS(ib.StreamSettings)
	if err != nil || stream == nil {
		return ""
	}
	return strings.TrimSpace(jsonGetString(stream, "finalmask", "quicParams", "udpHop", "ports"))
}

// hopExcludePorts 收集「面板已分配」的端口，避免探测结果与现有入站撞车。
//
// agent 只看 /proc/net 的监听表，主端口可能还没监听（入站刚建、内核未起），只靠它会
// 挑到已有入站的端口。
func (d *NodeDomain) hopExcludePorts(c *gin.Context, node *NodeRecord, focusInboundID int64) []int64 {
	inbounds, err := d.store.XrayInboundsByNode(ctxOf(c), node.ID)
	if err != nil {
		// 读不到就只排掉主端口，不阻断探测（agent 侧仍会避开真实占用）。
		d.logger.Printf("admin 端口跳跃：查询入站失败（exclude 仅含主端口）: %v", err)
		inbounds = nil
	}
	seen := map[int64]bool{}
	out := make([]int64, 0, len(inbounds)+1)
	add := func(p int64) {
		if p < 1 || p > 65535 || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for i := range inbounds {
		// 停用入站不占端口，但「本次要配跳跃的那个」无论启停都排掉（主端口绝不能进区间）。
		if !inbounds[i].Enabled && int64(inbounds[i].ID) != focusInboundID {
			continue
		}
		add(inbounds[i].Port)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func hopPortsJSArr(ports []int64) jsArr {
	arr := make(jsArr, 0, len(ports))
	for _, p := range ports {
		arr = append(arr, p)
	}
	return arr
}

// runHopCommand 入队一条端口跳跃指令并同步等回执（形状与 realityScan 一致：
// result / error / pending 三选一）。
func (d *NodeDomain) runHopCommand(c *gin.Context, node *NodeRecord, action string, payload any) {
	d.runNodeCommand(c, node, action, payload, d.cfg.HopWait)
}

// runNodeCommand 入队一条节点指令并同步等回执（端口跳跃 + sysctl 共用）。
//
// 返回体固定三态：result（成功，agent 回执原样转发）/ error（失败，agent 原文）/
// pending（等不到回执，节点可能离线）。
func (d *NodeDomain) runNodeCommand(c *gin.Context, node *NodeRecord, action string, payload any, wait int) {
	command, err := d.store.CreateCommand(ctxOf(c), node.ID, action, payload)
	if err != nil {
		d.failServer(c, "入队节点指令", err)
		return
	}

	deadline := d.now().Add(timeDurationSeconds(wait))
	for d.now().Before(deadline) {
		fresh, err := d.store.CommandByID(ctxOf(c), command.ID)
		if err != nil {
			d.failServer(c, "回读指令状态", err)
			return
		}
		switch fresh.Status {
		case "failed":
			output := ""
			if fresh.Result != nil {
				output = *fresh.Result
			}
			// agent 的失败回执本身就是结果 JSON（带 reason），能解就一起给前端。
			if obj, ok := parseHopResult(output); ok {
				d.success(c, jo("result", obj, "error", hopResultReason(obj, "节点执行失败")))
				return
			}
			if strings.Contains(output, "unknown action") {
				output = "该节点的 agent 版本过旧，请先在「节点」页升级 agent 后重试"
			} else if output == "" {
				output = "节点执行失败"
			}
			d.success(c, jo("result", nil, "error", output))
			return
		case "success":
			raw := ""
			if fresh.Result != nil {
				raw = *fresh.Result
			}
			obj, ok := parseHopResult(raw)
			if !ok {
				d.success(c, jo("result", nil, "error", "节点回执解析失败"))
				return
			}
			d.success(c, jo("result", obj))
			return
		}
		d.sleep(d.pollInterval)
	}

	d.success(c, jo(
		"pending", true,
		"message", "节点未在 "+strconv.Itoa(wait)+" 秒内回执（可能离线），稍后可重试",
	))
}

// parseHopResult 把 agent 回执解析成保序对象（四个 action 都回单个 JSON 对象）。
func parseHopResult(raw string) (*jsObj, bool) {
	parsed, err := parseOrderedJS([]byte(raw))
	if err != nil {
		return nil, false
	}
	obj, ok := parsed.(*jsObj)
	return obj, ok
}

// hopResultReason 取回执里的 reason（缺失时用兜底文案）。
func hopResultReason(obj *jsObj, fallback string) string {
	if s := strings.TrimSpace(jsonGetString(obj, "reason")); s != "" {
		return s
	}
	return fallback
}
