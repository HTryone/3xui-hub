package xray

import (
	"regexp"
	"strconv"
	"strings"

	"controlhub-server/internal/driver/internal/phpjson"
	"controlhub-server/internal/driver/internal/phpval"
)

// 逐条复刻 backend/app/Drivers/Xray/XrayRelayService.php 的**纯计算**部分：
//
//	出站 tag 决策（resolveTag/uniqueTag/validateCustomTag/tagTaken）
//	出站 settings / streamSettings 组装（outboundSettings/outboundStream/outboundStreamFromThreeX）
//	reality 公钥取值（realityPublicKey：顶层 → 客户端段 → 私钥推导）
//
// **不做**的部分（Go 侧属于业务层，不在驱动层）：
//   - 写 xray_outbounds / xray_relay_clients 表（P8b 管理端负责，需要行锁与 bump）
//   - 调 3x-ui addClient/attach 建承接凭据（由 threexui 包 + 业务层组合）
//
// 这样切分的理由：驱动层只做「给定目标入站，算出该写什么」，可完整单测；
// 落库与跨节点副作用留在业务层，避免驱动层持有数据库连接。

// 系统保留 tag（与内置出站一致，不可占用）。
const (
	TagPrefix = "rl-"
)

var reservedTags = map[string]bool{"direct": true, "blocked": true, "api": true}

// RelayError 是配置非法（对应 PHP 的 ValidationException，字段名走 Field）。
type RelayError struct {
	Field   string
	Message string
}

func (e *RelayError) Error() string { return e.Message }

// RelayPlan 是「源节点 × 目标入站」应写入的中转出站与目标侧凭据。
type RelayPlan struct {
	// Tag 是源节点上的出站 tag。
	Tag string
	// Protocol 是出站协议（vless/vmess/trojan）。
	Protocol string
	// Port 是目标入站端口。
	Port int64
	// Settings / StreamSettings 是出站片段。
	Settings       phpjson.Object
	StreamSettings phpjson.Object
	// ClientID 是目标侧应具备的凭据（vless/vmess 是 uuid，trojan 是 password）。
	// 空串表示目标侧已有凭据可复用（由调用方查询后传入）。
	ClientID string
	// Remark 是自定义备注（留空即空字符串，不自动填三段式）。
	Remark string
}

// RelayOptions 是 ensureRelay 的入参。
type RelayOptions struct {
	// SourceName / SourceTags 用于生成默认 tag 与查重：
	// SourceName 是源节点名；SourceTags 是**该源节点已有的全部出站 tag**（含自定义）。
	SourceName string
	SourceTags []string
	// TargetNodeID / TargetHost 是目标节点身份与连接地址。
	TargetNodeID int64
	TargetHost   string
	// TargetName 用于默认 tag（「节点名-端口-入站名」）。
	TargetName string
	// TargetPort 是默认 tag 冲突时 rl- 生成用的端口段（0 → 'x'）。
	TargetPort int64
	// CustomTag / CustomRemark 留空则用默认值。
	CustomTag    string
	CustomRemark string
	// RelayExists 为 true 表示「源节点 × 目标入站」已有中转出站（幂等命中：
	// 此时 tag 不变，也不做自定义标识的重名/保留字校验）。
	RelayExists bool
}

// EnsureRelayForXrayInbound 复刻 ensureRelay() 的 xray 目标分支：
// 目标入站（xray_inbounds 行）→ 中转出站片段 + 目标凭据要求。
//
// key 是 APP_KEY（用于 reality 私钥推导的兜底路径，本函数不涉私钥；
// 保留参数位以便将来需要时不必改签名）。
func EnsureRelayForXrayInbound(target Inbound, clientID string, opts RelayOptions) (RelayPlan, error) {
	if target.Protocol == "shadowsocks" {
		return RelayPlan{}, &RelayError{Field: "outbound_tag", Message: "目标入站协议（shadowsocks）暂不支持自动中转"}
	}
	if target.Protocol != "vless" && target.Protocol != "vmess" && target.Protocol != "trojan" {
		return RelayPlan{}, &RelayError{Field: "outbound_tag", Message: "目标入站协议暂不支持自动中转"}
	}

	port := target.Port
	if port == 0 {
		port = 443
	}

	flow := relayFlowFor(target)
	settings := outboundSettings(target.Protocol, clientID, opts.TargetHost, port, flow)
	stream, err := outboundStream(target, opts.TargetHost)
	if err != nil {
		return RelayPlan{}, err
	}

	remark := strings.TrimSpace(opts.CustomRemark)

	defaultTag := opts.TargetName + "-" + relayInboundLabel(target)
	tag, err := resolveTag(opts, defaultTag)
	if err != nil {
		return RelayPlan{}, err
	}

	return RelayPlan{
		Tag:            tag,
		Protocol:       target.Protocol,
		Port:           port,
		Settings:       settings,
		StreamSettings: stream,
		ClientID:       clientID,
		Remark:         remark,
	}, nil
}

// EnsureRelayForThreeXInbound 复刻 ensureRelay() 的 3x-ui 目标分支。
//
// stream 是 3x-ui listInbounds 里已解析的 streamSettings（面板格式），
// port/tag 来自同一条入站记录；拿不到时按 port=443、tag="" 处理。
func EnsureRelayForThreeXInbound(protocol string, inboundID int64, port int64, inboundTag string, stream map[string]any, clientID string, opts RelayOptions) (RelayPlan, error) {
	if protocol == "shadowsocks" {
		return RelayPlan{}, &RelayError{Field: "outbound_tag", Message: "目标入站协议（shadowsocks）暂不支持自动中转"}
	}
	if protocol != "vless" && protocol != "vmess" && protocol != "trojan" {
		return RelayPlan{}, &RelayError{Field: "outbound_tag", Message: "目标入站协议暂不支持自动中转"}
	}
	if port <= 0 {
		port = 443
	}

	// 3x-ui 目标用 xhttp 时 vless 出站用户**不带 flow**（vision 仅限 tcp）。
	flow := ""
	if protocol == "vless" && threeXNetwork(stream) == "tcp" &&
		(threeXSecurity(stream) == "reality" || threeXSecurity(stream) == "tls") {
		flow = Flow
	}

	settings := outboundSettings(protocol, clientID, opts.TargetHost, port, flow)
	outStream, err := outboundStreamFromThreeX(stream, opts.TargetHost)
	if err != nil {
		return RelayPlan{}, err
	}

	label := inboundTag
	if !strings.Contains(label, strconv.FormatInt(port, 10)) {
		label = strconv.FormatInt(port, 10) + "-" + label
	}
	opts.TargetPort = port

	tag, err := resolveTag(opts, opts.TargetName+"-"+label)
	if err != nil {
		return RelayPlan{}, err
	}

	return RelayPlan{
		Tag:            tag,
		Protocol:       protocol,
		Port:           port,
		Settings:       settings,
		StreamSettings: outStream,
		ClientID:       clientID,
		Remark:         strings.TrimSpace(opts.CustomRemark),
	}, nil
}

// relayInboundLabel 复刻 PHP 的默认 tag 中段：
//
//	inbound->tag 已含端口 → 直接用 tag；否则用「端口-tag」。
func relayInboundLabel(ib Inbound) string {
	portStr := strconv.FormatInt(ib.Port, 10)
	if strings.Contains(ib.Tag, portStr) {
		return ib.Tag
	}
	return portStr + "-" + ib.Tag
}

// resolveTag 复刻 resolveTag()：
//   - 自定义：校验通过后原样使用（允许中文；重名直接报错，不自动加后缀、不静默覆盖）
//   - 默认：用 $defaultTag；被占用时回退 uniqueTag 生成 rl- 形态并自增后缀
func resolveTag(opts RelayOptions, defaultTag string) (string, error) {
	custom := strings.TrimSpace(opts.CustomTag)
	isCustom := custom != ""

	// 自定义标识先校验（保留字/长度/与本节点已有出站重名）——在动对方服务器凭据之前拒绝，
	// 避免「重名报错却已在对方建了承接凭据」的脏状态。
	if isCustom && !opts.RelayExists {
		if err := validateCustomTag(custom); err != nil {
			return "", err
		}
		if tagTaken(opts.SourceTags, custom) {
			return "", &RelayError{Field: "outbound_tag", Message: "标识 " + custom + " 已存在"}
		}
	}

	if isCustom {
		return custom, nil
	}

	tag := strings.TrimSpace(defaultTag)
	if tag != "" && !tagTaken(opts.SourceTags, tag) {
		return tag, nil
	}
	return uniqueTag(opts.SourceTags, opts.TargetNodeID, opts.TargetPort), nil
}

// validateCustomTag 复刻 validateCustomTag()：系统保留字拒绝；长度与库列（varchar 64）一致。
// 允许中文等任意非空字符（实测 xray 支持中文 tag）。
func validateCustomTag(tag string) error {
	if reservedTags[strings.ToLower(tag)] {
		return &RelayError{Field: "outbound_tag", Message: "标识 " + tag + " 为系统保留"}
	}
	// PHP 用 strlen（字节数）；中文一个字 3 字节，故 21 个汉字就超 64。
	if len(tag) > 64 {
		return &RelayError{Field: "outbound_tag", Message: "标识最长 64 个字符"}
	}
	return nil
}

func tagTaken(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}

var tagSanitizer = regexp.MustCompile(`[^A-Za-z0-9_.-]`)

// uniqueTag 复刻 uniqueTag()：rl-{目标节点}-{port}，冲突自动加序号。
func uniqueTag(existing []string, targetNodeID int64, port int64) string {
	portPart := "x"
	if port > 0 {
		portPart = strconv.FormatInt(port, 10)
	}
	base := TagPrefix + strconv.FormatInt(targetNodeID, 10) + "-" + portPart
	base = tagSanitizer.ReplaceAllString(base, "")
	if base == "" {
		base = TagPrefix + "relay"
	}

	tag := base
	for i := 2; tagTaken(existing, tag); i++ {
		tag = base + "-" + strconv.Itoa(i)
	}
	return tag
}

// relayFlowFor 复刻 flowFor()：目标入站若为 vless + tcp + reality/tls（vision 流控适用），
// 中转出站用户须带 flow，与渲染层 / 订阅链接同源；否则对端按 vision 处理而客户端没带 → 握手失败。
func relayFlowFor(ib Inbound) string {
	if ib.Protocol != "vless" {
		return ""
	}
	stream := streamObject(ib)
	if networkOf(stream) == "tcp" && (securityOf(stream) == "reality" || securityOf(stream) == "tls") {
		return Flow
	}
	return ""
}

// outboundSettings 复刻 outboundSettings()：凭据 + 目标地址/端口 + vless flow。
func outboundSettings(protocol, clientID, address string, port int64, flow string) phpjson.Object {
	if port <= 0 {
		port = 443
	}

	switch protocol {
	case "vmess":
		return phpjson.Object{
			{Key: "vnext", Value: phpjson.Array{phpjson.Object{
				{Key: "address", Value: address},
				{Key: "port", Value: port},
				{Key: "users", Value: phpjson.Array{phpjson.Object{
					{Key: "id", Value: clientID},
					{Key: "alterId", Value: 0},
					{Key: "security", Value: "auto"},
				}}},
			}}},
		}
	case "trojan":
		return phpjson.Object{
			{Key: "servers", Value: phpjson.Array{phpjson.Object{
				{Key: "address", Value: address},
				{Key: "port", Value: port},
				{Key: "password", Value: clientID},
			}}},
		}
	default: // vless
		user := phpjson.Object{
			{Key: "id", Value: clientID},
			{Key: "encryption", Value: "none"},
		}
		if flow != "" {
			// PHP 用 array_filter 去掉空 flow，且 id/encryption 恒在，故顺序是 id → encryption → flow。
			user = append(user, phpjson.Member{Key: "flow", Value: flow})
		}
		return phpjson.Object{
			{Key: "vnext", Value: phpjson.Array{phpjson.Object{
				{Key: "address", Value: address},
				{Key: "port", Value: port},
				{Key: "users", Value: phpjson.Array{user}},
			}}},
		}
	}
}

// outboundStream 复刻 outboundStream()：传输层取目标入站；reality/tls 用目标入站的**公开**参数
// （publicKey / serverNames / shortIds），与订阅链接生成同源（不涉私钥）。
func outboundStream(ib Inbound, host string) (phpjson.Object, error) {
	stream := streamObject(ib)
	security := securityOf(stream)
	network := networkOf(stream)

	out := phpjson.Object{
		{Key: "network", Value: network},
		{Key: "security", Value: security},
	}

	switch security {
	case "reality":
		reality := phpval.ToObject(mustGet(stream, "realitySettings"))
		client := phpval.ToObject(mustGet(reality, "settings"))

		publicKey, err := realityPublicKey(reality)
		if err != nil {
			return nil, err
		}
		fingerprint := phpval.String(mustGet(client, "fingerprint"))
		if fingerprint == "" {
			fingerprint = "chrome"
		}
		setting := phpjson.Object{
			{Key: "serverName", Value: firstStringValueOrDefault(mustGet(reality, "serverNames"), DefaultSNI)},
			{Key: "publicKey", Value: publicKey},
			{Key: "fingerprint", Value: fingerprint},
		}
		if shortID := firstStringValue(mustGet(reality, "shortIds")); shortID != "" {
			setting = append(setting, phpjson.Member{Key: "shortId", Value: shortID})
		}
		if spider := phpval.String(mustGet(client, "spiderX")); spider != "" {
			setting = append(setting, phpjson.Member{Key: "spiderX", Value: spider})
		}
		out = append(out, phpjson.Member{Key: "realitySettings", Value: setting})
	case "tls":
		sni := jsonFieldString(stream, "tlsSettings", "serverName")
		if sni == "" {
			sni = host
		}
		out = append(out, phpjson.Member{Key: "tlsSettings", Value: phpjson.Object{
			{Key: "serverName", Value: sni},
		}})
	}

	switch network {
	case "ws":
		ws := phpval.ToObject(mustGet(stream, "wsSettings"))
		block := phpjson.Object{{Key: "path", Value: coalesceString(mustGet(ws, "path"), "/")}}
		if wsHost := jsonFieldString(ws, "host"); wsHost != "" {
			block = append(block, phpjson.Member{Key: "headers", Value: phpjson.Object{
				{Key: "Host", Value: wsHost},
			}})
		}
		out = append(out, phpjson.Member{Key: "wsSettings", Value: block})
	case "xhttp":
		xhttp := phpval.ToObject(mustGet(stream, "xhttpSettings"))
		block := phpjson.Object{
			{Key: "path", Value: coalesceString(mustGet(xhttp, "path"), "/")},
		}
		if h := jsonFieldString(xhttp, "host"); h != "" {
			block = append(block, phpjson.Member{Key: "host", Value: h})
		}
		mode := jsonFieldString(xhttp, "mode")
		if mode == "" {
			mode = "auto"
		}
		block = append(block, phpjson.Member{Key: "mode", Value: mode})
		out = append(out, phpjson.Member{Key: "xhttpSettings", Value: block})
	case "httpupgrade":
		hu := phpval.ToObject(mustGet(stream, "httpupgradeSettings"))
		block := phpjson.Object{{Key: "path", Value: coalesceString(mustGet(hu, "path"), "/")}}
		if h := jsonFieldString(hu, "host"); h != "" {
			block = append(block, phpjson.Member{Key: "host", Value: h})
		}
		out = append(out, phpjson.Member{Key: "httpupgradeSettings", Value: block})
	}

	// TCP header 伪装：目标入站带 http 伪装时原样转发（缺了中转会握手失败）
	if network == "tcp" && jsonFieldString(stream, "tcpSettings", "header", "type") == "http" {
		if tcp, ok := mustGet(stream, "tcpSettings").(phpjson.Object); ok {
			out = append(out, phpjson.Member{Key: "tcpSettings", Value: tcp.Clone()})
		}
	}

	return out, nil
}

// outboundStreamFromThreeX 复刻 outboundStreamFromThreeX()：3x-ui 面板格式 → 出站客户端 streamSettings。
//
// 与 xray 目标的关键差异（已按实测线上数据核对）：
//   - reality 的 **publicKey 在 realitySettings 顶层**（不在客户端段 settings 里），
//     客户端段 settings 只放 fingerprint / spiderX / mldsa65Verify；而 xray 目标的
//     publicKey 恰在 settings 里。故 publicKey 必须顶层优先、兼容 settings 回退。
//   - 网络类型在 `network`（auto→tcp）；xhttp 参数在 `xhttpSettings`（path/host/mode）。
//   - serverNames/shortIds 在顶层；取值口径与订阅链接同源（各取第 0 个，不涉私钥）。
func outboundStreamFromThreeX(stream map[string]any, host string) (phpjson.Object, error) {
	if len(stream) == 0 {
		return phpjson.Object{}, nil
	}

	security := threeXSecurity(stream)
	network := threeXNetwork(stream)

	out := phpjson.Object{
		{Key: "network", Value: network},
		{Key: "security", Value: security},
	}

	switch security {
	case "reality":
		reality := objectFromMap(stream["realitySettings"])
		client := phpval.ToObject(mustGet(reality, "settings"))

		publicKey, err := realityPublicKey(reality)
		if err != nil {
			return nil, err
		}
		fingerprint := phpval.String(mustGet(client, "fingerprint"))
		if fingerprint == "" {
			fingerprint = "chrome"
		}
		setting := phpjson.Object{
			{Key: "serverName", Value: firstStringValueOrDefault(mustGet(reality, "serverNames"), DefaultSNI)},
			{Key: "publicKey", Value: publicKey},
			{Key: "fingerprint", Value: fingerprint},
		}
		if shortID := firstStringValue(mustGet(reality, "shortIds")); shortID != "" {
			setting = append(setting, phpjson.Member{Key: "shortId", Value: shortID})
		}
		if spider := phpval.String(mustGet(client, "spiderX")); spider != "" {
			setting = append(setting, phpjson.Member{Key: "spiderX", Value: spider})
		}
		out = append(out, phpjson.Member{Key: "realitySettings", Value: setting})
	case "tls":
		tls := objectFromMap(stream["tlsSettings"])
		sni := phpval.String(mustGet(tls, "serverName"))
		if sni == "" {
			sni = host
		}
		if sni != "" {
			out = append(out, phpjson.Member{Key: "tlsSettings", Value: phpjson.Object{
				{Key: "serverName", Value: sni},
			}})
		}
	}

	switch network {
	case "ws":
		ws := objectFromMap(stream["wsSettings"])
		block := phpjson.Object{{Key: "path", Value: coalesceString(mustGet(ws, "path"), "/")}}
		if wsHost := phpval.String(mustGet(ws, "host")); wsHost != "" {
			block = append(block, phpjson.Member{Key: "headers", Value: phpjson.Object{
				{Key: "Host", Value: wsHost},
			}})
		}
		out = append(out, phpjson.Member{Key: "wsSettings", Value: block})
	case "xhttp":
		xhttp := objectFromMap(stream["xhttpSettings"])
		block := phpjson.Object{{Key: "path", Value: coalesceString(mustGet(xhttp, "path"), "/")}}
		if h := phpval.String(mustGet(xhttp, "host")); h != "" {
			block = append(block, phpjson.Member{Key: "host", Value: h})
		}
		mode := phpval.String(mustGet(xhttp, "mode"))
		if mode == "" {
			mode = "auto"
		}
		block = append(block, phpjson.Member{Key: "mode", Value: mode})
		out = append(out, phpjson.Member{Key: "xhttpSettings", Value: block})
	case "httpupgrade":
		hu := objectFromMap(stream["httpupgradeSettings"])
		block := phpjson.Object{{Key: "path", Value: coalesceString(mustGet(hu, "path"), "/")}}
		if h := phpval.String(mustGet(hu, "host")); h != "" {
			block = append(block, phpjson.Member{Key: "host", Value: h})
		}
		out = append(out, phpjson.Member{Key: "httpupgradeSettings", Value: block})
	case "grpc":
		grpc := objectFromMap(stream["grpcSettings"])
		block := phpjson.Object{}
		if v := phpval.String(mustGet(grpc, "serviceName")); v != "" {
			block = append(block, phpjson.Member{Key: "serviceName", Value: v})
		}
		if v := phpval.String(mustGet(grpc, "authority")); v != "" {
			block = append(block, phpjson.Member{Key: "authority", Value: v})
		}
		out = append(out, phpjson.Member{Key: "grpcSettings", Value: block})
	}

	// TCP header 伪装：目标入站带 http 伪装时原样转发
	if network == "tcp" && jsonFieldString(objectFromMap(stream), "tcpSettings", "header", "type") == "http" {
		if tcp, ok := stream["tcpSettings"]; ok {
			if obj, ok := toOrdered(tcp).(phpjson.Object); ok {
				out = append(out, phpjson.Member{Key: "tcpSettings", Value: obj})
			}
		}
	}

	return out, nil
}

// realityPublicKey 复刻 realityPublicKey()：顶层 → 客户端段 → **私钥推导**；全部失败则报错。
//
// 3x-ui v3.10 的入站数据只存 privateKey、不存 publicKey，直接读顶层会得到空串，
// 进而生成 REALITY 缺公钥的坏出站（xray 校验报 `empty "password"`、节点整份配置无法应用）。
// 此处以推导兜底，并在确实取不到时显式报错，不允许再产出无效配置。
func realityPublicKey(reality phpjson.Object) (string, error) {
	client := phpval.ToObject(mustGet(reality, "settings"))
	publicKey := phpval.String(mustGet(reality, "publicKey"))
	if publicKey == "" {
		publicKey = phpval.String(mustGet(client, "publicKey"))
	}
	if publicKey == "" {
		publicKey = PublicKeyFromPrivateKey(phpval.String(mustGet(reality, "privateKey")))
	}
	if publicKey == "" {
		return "", &RelayError{
			Field:   "outbound_tag",
			Message: "目标入站 REALITY 公钥缺失且无法从私钥推导，已阻止生成无效中转配置",
		}
	}
	return publicKey, nil
}

// ── 3x-ui 面板格式读取小工具 ────────────────────────────────

func threeXSecurity(stream map[string]any) string {
	v := phpval.String(stream["security"])
	if v == "" {
		return "none"
	}
	return v
}

func threeXNetwork(stream map[string]any) string {
	v := phpval.String(stream["network"])
	if v == "" || v == "auto" {
		return "tcp"
	}
	return v
}

// objectFromMap 把 map[string]any（3x-ui JSON 解码结果）转成保序 Object。
// 键序按 map 迭代顺序（3x-ui 只按语义解析，中转出站不是对拍路径）。
func objectFromMap(v any) phpjson.Object {
	m, ok := v.(map[string]any)
	if !ok {
		return phpjson.Object{}
	}
	obj := make(phpjson.Object, 0, len(m))
	for k, val := range m {
		obj = append(obj, phpjson.Member{Key: k, Value: toOrdered(val)})
	}
	return obj
}

// toOrdered 把标准库解码的 map/slice 递归转成保序结构。
func toOrdered(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return objectFromMap(t)
	case []any:
		arr := make(phpjson.Array, 0, len(t))
		for _, e := range t {
			arr = append(arr, toOrdered(e))
		}
		return arr
	}
	return v
}

// firstStringValueOrDefault 取数组第 0 个字符串；取不到（或为空串）时用默认值。
//
// 与 PHP `$reality['serverNames'][0] ?? DEFAULT_SNI` 的差异：PHP 的 `??` 只在
// 「键不存在 / 值为 null」时回落，空串会原样传下去。这里为了不产出空 serverName
// （空值的 realitySettings 会被内核静默拒绝）按「空串也用默认值」处理，
// 与 PHP 侧实测行为一致（serverNames 为空数组时 PHP 同样得到 DEFAULT_SNI）。
func firstStringValueOrDefault(v any, def string) string {
	s := firstStringValue(v)
	if s == "" {
		return def
	}
	return s
}

// 编译期断言：RelayError 实现 error。
var _ error = (*RelayError)(nil)
