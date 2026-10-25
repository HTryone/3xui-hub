package nodeapi

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"controlhub-server/internal/crypto"
)

// 全量配置渲染：逐行对齐 backend/app/Drivers/Xray/XrayConfigService.php。
//
// 输出必须与 PHP 的 renderJson() 逐字节一致（同一份数据、同一套顺序），
// 否则节点每轮拉取都会因为指纹/内容变化重启内核。因此这里刻意不用 map，
// 一律用保序 Object（见 orderedjson.go）。

const (
	// API_PORT 是 xray 内核 API 的本地回环端口（v26 CLI 必须显式 --server）。
	API_PORT = 10085

	// FLOW 是 VLESS+Reality 的传输流控（v26 校验要求该值）。
	FLOW = "xtls-rprx-vision"

	// DEFAULT_DEST / DEFAULT_SNI 是 Reality 缺省目标与 SNI。
	DEFAULT_DEST = "www.cloudflare.com:443"
	DEFAULT_SNI  = "www.cloudflare.com"
)

// RenderData 是 render() 的全部输入（一次查询集齐，避免渲染期间数据漂移）。
//
// 切片顺序就是数据库返回顺序，必须与 PHP 侧一致：
//   - Users           eligibleUserRows() 无 orderBy → 由 MySQL 决定（两级查询同源即同序）
//   - Inbounds/Outbounds/Balancers/Rules  enabledFor() 的 sort, id
//   - Subscriptions   enabledFor() 的 priority, id
//   - RelayClients    clientsForInbound() 的 id
type RenderData struct {
	Node          RenderNode
	Users         []RenderUser
	Inbounds      []RenderInbound
	Outbounds     []RenderOutbound
	Balancers     []RenderBalancer
	RoutingRules  []RenderRoutingRule
	Subscriptions []RenderSubscription
	RelayClients  []RenderRelayClient
}

// RenderNode 是渲染用到的节点字段。
type RenderNode struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Host        string `json:"host"`
	DriverType  string `json:"driver_type"`
	ConfigRaw   []byte `json:"driver_config_raw"` // driver_config 原始 JSON 文本（nil = NULL）
	ConfigVer   int64  `json:"-"`
	RoutingOpts RoutingOptions
}

// RoutingOptions 是 driver_config.xray_routing 的口径（缺省 AsIs / 空）。
type RoutingOptions struct {
	DomainStrategy string
	DomainMatcher  string
}

// RenderUser 是 config 实际注入的用户列（与 eligibleUserRows() 完全同源）。
type RenderUser struct {
	ID           int64      `json:"id"`
	UUID         string     `json:"uuid"`
	Protocol     string     `json:"protocol"`
	TrafficLimit int64      `json:"traffic_limit"`
	ExpiredAt    *time.Time `json:"expired_at"`
}

// RenderInbound 是启用入站的一行。
//
// ID 只用于把中转凭据（xray_relay_clients.inbound_id）挂回对应入站，不参与渲染输出。
type RenderInbound struct {
	ID                int64           `json:"id"`
	Tag               string          `json:"tag"`
	Remark            *string         `json:"remark"`
	Protocol          string          `json:"protocol"`
	Port              int64           `json:"port"`
	Listen            string          `json:"listen"`
	Settings          json.RawMessage `json:"settings"`
	StreamSettings    json.RawMessage `json:"stream_settings"`
	Sniffing          json.RawMessage `json:"sniffing"`
	RealityPrivateKey *string         `json:"reality_private_key"`
}

// RenderOutbound 是启用出站的一行。
type RenderOutbound struct {
	Tag            string          `json:"tag"`
	Protocol       string          `json:"protocol"`
	Settings       json.RawMessage `json:"settings"`
	StreamSettings json.RawMessage `json:"stream_settings"`
	SecretKey      *string         `json:"secret_key"`
}

// RenderBalancer 是启用负载均衡器。
type RenderBalancer struct {
	Tag         string          `json:"tag"`
	Selectors   json.RawMessage `json:"selectors"`
	Strategy    string          `json:"strategy"`
	FallbackTag *string         `json:"fallback_tag"`
}

// RenderRoutingRule 是启用路由规则。
type RenderRoutingRule struct {
	Domains     json.RawMessage `json:"domains"`
	IPs         json.RawMessage `json:"ips"`
	Port        *string         `json:"port"`
	Network     *string         `json:"network"`
	Protocol    *string         `json:"protocol"`
	InboundTag  *string         `json:"inbound_tag"`
	OutboundTag string          `json:"outbound_tag"`
}

// RenderSubscription 是启用出站订阅（只需要 prepend 与最近一次拉取结果）。
type RenderSubscription struct {
	Prepend bool            `json:"prepend"`
	Fetched json.RawMessage `json:"fetched"`
}

// RenderRelayClient 是中转凭据（按 id 排序，只用于向对应入站注入 client）。
type RenderRelayClient struct {
	InboundID int64  `json:"inbound_id"`
	ClientID  string `json:"client_id"`
}

// realitySettings 的高级字段缺省语（对齐 3x-ui reality.ts 与内核默认值）。
var realityAdvDefaults = Object{
	{Key: "xver", Value: 0},
	{Key: "show", Value: false},
	{Key: "minClientVer", Value: ""},
	{Key: "maxClientVer", Value: ""},
	{Key: "maxTimediff", Value: 0},
	{Key: "mldsa65Seed", Value: ""},
}

// streamEmptyObjectKeys 是必须输出 {} 的结构体字段（PHP [] 会被 xray 拒绝）。
var streamEmptyObjectKeys = []string{
	"tlsSettings", "realitySettings", "wsSettings", "xhttpSettings",
	"httpupgradeSettings", "grpcSettings", "tcpSettings", "sockopt",
}

var defaultSniffing = Object{
	{Key: "enabled", Value: true},
	{Key: "destOverride", Value: Array{"http", "tls", "quic"}},
}

// Render 复刻 XrayConfigService::render()（数组形态，保序）。
func Render(d *RenderData, key []byte) (Object, error) {
	outbounds, err := renderOutbounds(d, key)
	if err != nil {
		return nil, err
	}
	routing, err := renderRouting(d)
	if err != nil {
		return nil, err
	}
	inbounds, err := renderInbounds(d, key)
	if err != nil {
		return nil, err
	}

	out := Object{
		{Key: "log", Value: Object{{Key: "loglevel", Value: "warning"}}},
		{Key: "api", Value: Object{
			{Key: "tag", Value: "api"},
			{Key: "listen", Value: "127.0.0.1:10085"},
			{Key: "services", Value: Array{"HandlerService", "StatsService"}},
		}},
		{Key: "stats", Value: Object{}},
		{Key: "policy", Value: Object{
			{Key: "levels", Value: Object{{Key: "0", Value: Object{
				{Key: "statsUserUplink", Value: true},
				{Key: "statsUserDownlink", Value: true},
			}}}},
			{Key: "system", Value: Object{
				{Key: "statsInboundUplink", Value: true},
				{Key: "statsInboundDownlink", Value: true},
				{Key: "statsOutboundUplink", Value: true},
				{Key: "statsOutboundDownlink", Value: true},
			}},
		}},
		{Key: "outbounds", Value: outbounds},
		{Key: "routing", Value: routing},
		{Key: "inbounds", Value: inbounds},
	}
	for _, m := range renderObservatory(d) {
		out = append(out, m)
	}
	return out, nil
}

// RenderJSON 复刻 renderJson()：JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE。
func RenderJSON(d *RenderData, key []byte) ([]byte, error) {
	o, err := Render(d, key)
	if err != nil {
		return nil, err
	}
	return MarshalValue(o)
}

// ── 入站 ─────────────────────────────────────────────────

func renderInbounds(d *RenderData, key []byte) (Array, error) {
	out := Array{}
	for _, ib := range d.Inbounds {
		frag, err := renderInbound(d, ib, key, true)
		if err != nil {
			return nil, err
		}
		out = append(out, frag)
	}
	return out, nil
}

func renderInbound(d *RenderData, ib RenderInbound, key []byte, withStream bool) (Object, error) {
	settingsVal, err := ParseJSON(ib.Settings)
	if err != nil {
		return nil, err
	}
	settings := toObject(settingsVal)
	if settings == nil {
		settings = Object{}
	}

	clients, hasClients, err := clientsFor(d, ib)
	if err != nil {
		return nil, err
	}
	if hasClients {
		settings = settings.Clone()
		settings.Set("clients", clients)
	}

	frag := Object{
		{Key: "tag", Value: ib.Tag},
		{Key: "listen", Value: listenOr(ib.Listen)},
		{Key: "port", Value: ib.Port},
		{Key: "protocol", Value: ib.Protocol},
	}
	if len(settings) == 0 {
		frag = append(frag, Member{Key: "settings", Value: Object{}})
	} else {
		frag = append(frag, Member{Key: "settings", Value: settings})
	}

	if withStream {
		streamVal, err := ParseJSON(ib.StreamSettings)
		if err != nil {
			return nil, err
		}
		stream := toObject(streamVal)
		if stream == nil {
			stream = Object{}
		}
		stream = stream.Clone()

		if stringOf(mustGet(stream, "security")) == "reality" {
			rs, err := realitySettingsFor(ib, key)
			if err != nil {
				return nil, err
			}
			stream.Set("realitySettings", rs)
		}
		for _, subKey := range streamEmptyObjectKeys {
			if v, ok := stream.Get(subKey); ok {
				if arr, isArr := v.(Array); isArr && len(arr) == 0 {
					stream.Set(subKey, Object{})
				}
			}
		}
		if len(stream) > 0 {
			frag = append(frag, Member{Key: "streamSettings", Value: stream})
		}

		sniffVal, err := ParseJSON(ib.Sniffing)
		if err != nil {
			return nil, err
		}
		if phpEmpty(sniffVal) {
			frag = append(frag, Member{Key: "sniffing", Value: defaultSniffing.Clone()})
		} else {
			frag = append(frag, Member{Key: "sniffing", Value: sniffVal})
		}
	}

	return frag, nil
}

// mustGet 复刻 `$x['k'] ?? null` 里「键不存在」的语义。
func mustGet(o Object, k string) any {
	v, ok := o.Get(k)
	if !ok {
		return nil
	}
	return v
}

// listenOr 复刻 `$inbound->listen ?: '0.0.0.0'`。
func listenOr(listen string) string {
	if phpEmpty(listen) {
		return "0.0.0.0"
	}
	return listen
}

// clientsFor 复刻 clientsFor()：shadowsocks 单凭据模式不注入 clients。
// 第二个返回值表示是否注入（对应 PHP 的 null）。
func clientsFor(d *RenderData, ib RenderInbound) (Array, bool, error) {
	if ib.Protocol == "shadowsocks" {
		return nil, false, nil
	}

	clients := Array{}
	for _, u := range d.Users {
		if u.Protocol != ib.Protocol {
			continue
		}
		var client Object
		switch ib.Protocol {
		case "vless":
			client = Object{
				{Key: "id", Value: u.UUID},
				{Key: "email", Value: clientEmailOf(u)},
				{Key: "totalGB", Value: u.TrafficLimit},
				{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
			}
			if flowApplies(ib) {
				client = append(client, Member{Key: "flow", Value: FLOW})
			}
		case "vmess":
			client = Object{
				{Key: "id", Value: u.UUID},
				{Key: "email", Value: clientEmailOf(u)},
				{Key: "totalGB", Value: u.TrafficLimit},
				{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
			}
		case "trojan":
			// trojan 以密码定位用户：统一用 uuid 作密码（面板侧保证唯一）。
			client = Object{
				{Key: "password", Value: u.UUID},
				{Key: "email", Value: clientEmailOf(u)},
				{Key: "totalGB", Value: u.TrafficLimit},
				{Key: "expiryTime", Value: expiryZulu(u.ExpiredAt)},
			}
		default:
			// 未知协议：PHP 的 match 返回 null，不注入该用户。
			continue
		}
		clients = append(clients, client)
	}

	// 中转凭据：面板在目标入站上为来源节点自动生成的 client。
	for _, rc := range d.RelayClients {
		if rc.InboundID != ib.ID {
			continue
		}
		if ib.Protocol == "trojan" {
			clients = append(clients, Object{{Key: "password", Value: rc.ClientID}})
		} else {
			clients = append(clients, Object{{Key: "id", Value: rc.ClientID}})
		}
	}

	return clients, true, nil
}

// clientEmailOf 复刻 clientEmail(): 'ch_user_{id}'。
func clientEmailOf(u RenderUser) string {
	return "ch_user_" + itoa(u.ID)
}

// expiryZulu 复刻 expiryZulu(): null/” → 0，否则 toIso8601ZuluString。
func expiryZulu(t *time.Time) any {
	if t == nil {
		return 0
	}
	if t.IsZero() {
		return 0
	}
	return iso8601Zulu(*t)
}

// flowApplies 复刻 flowApplies()：仅 vless + tcp + reality/tls。
func flowApplies(ib RenderInbound) bool {
	if ib.Protocol != "vless" {
		return false
	}
	streamVal, err := ParseJSON(ib.StreamSettings)
	if err != nil {
		return false
	}
	stream := toObject(streamVal)
	network := "tcp"
	if v, ok := stream.Get("network"); ok && stringOf(v) != "" {
		network = stringOf(v)
	}
	security := stringOf(mustGet(stream, "security"))
	if security == "" {
		security = "none"
	}
	return network == "tcp" && (security == "reality" || security == "tls")
}

// realitySettingsFor 复刻 realitySettingsFor()：公开参数来自入站，私钥解密注入。
func realitySettingsFor(ib RenderInbound, key []byte) (Object, error) {
	streamVal, err := ParseJSON(ib.StreamSettings)
	if err != nil {
		return nil, err
	}
	stream := toObject(streamVal)
	public := toObject(mustGet(stream, "realitySettings"))

	dest := stringOf(mustGet(public, "dest"))
	if dest == "" {
		dest = DEFAULT_DEST
	}

	serverNames := Array{}
	if raw, ok := public.Get("serverNames"); ok {
		for _, v := range toArrayValues(raw) {
			if s, isStr := v.(string); isStr {
				serverNames = append(serverNames, s)
			}
		}
	} else {
		serverNames = append(serverNames, DEFAULT_SNI)
	}

	shortIDs := Array{}
	if raw, ok := public.Get("shortIds"); ok {
		for _, v := range toArrayValues(raw) {
			if s, isStr := v.(string); isStr {
				shortIDs = append(shortIDs, s)
			}
		}
	}

	out := Object{
		{Key: "dest", Value: dest},
		{Key: "serverNames", Value: serverNames},
		{Key: "shortIds", Value: shortIDs},
	}
	if pk := realityPrivateKey(ib, key); pk != "" {
		out = append(out, Member{Key: "privateKey", Value: pk})
	}

	// 高级字段：仅写「显式设置且非内核默认」的值（空值会让内核静默拒绝握手）。
	for _, def := range realityAdvDefaults {
		v, ok := public.Get(def.Key)
		if !ok || v == nil || stringOf(v) == "" || equalScalar(v, def.Value) {
			continue
		}
		out = append(out, Member{Key: def.Key, Value: v})
	}

	if mkl := strings.TrimSpace(stringOf(mustGet(public, "masterKeyLog"))); mkl != "" {
		out = append(out, Member{Key: "masterKeyLog", Value: mkl})
	}

	for _, k := range []string{"limitFallbackUpload", "limitFallbackDownload"} {
		blockRaw, ok := public.Get(k)
		if !ok {
			continue
		}
		block, isObj := blockRaw.(Object)
		if !isObj {
			continue
		}
		out = append(out, Member{Key: k, Value: Object{
			{Key: "afterBytes", Value: intOf(mustGet(block, "afterBytes"))},
			{Key: "bytesPerSec", Value: intOf(mustGet(block, "bytesPerSec"))},
			{Key: "burstBytesPerSec", Value: intOf(mustGet(block, "burstBytesPerSec"))},
		}})
	}

	return out, nil
}

// equalScalar 比较 JSON 标量与 PHP 默认值（用于跳过 reality 高级字段）。
func equalScalar(a, b any) bool {
	switch bv := b.(type) {
	case int:
		return numericEquals(a, float64(bv))
	case float64:
		return numericEquals(a, bv)
	case bool:
		av, ok := a.(bool)
		return ok && av == bv
	case string:
		return stringOf(a) == bv && isStringish(a)
	}
	return false
}

func isStringish(v any) bool {
	_, ok := v.(string)
	return ok
}

func numericEquals(a any, b float64) bool {
	switch a.(type) {
	case json.Number, int, int64, float64:
		return floatOf(a) == b
	}
	return false
}

// realityPrivateKey 复刻 realityPrivateKey()：解密失败时退回原始值（兼容明文存量）。
func realityPrivateKey(ib RenderInbound, key []byte) string {
	if ib.RealityPrivateKey == nil || *ib.RealityPrivateKey == "" {
		return ""
	}
	plain, err := crypto.DecryptString(*ib.RealityPrivateKey, key)
	if err != nil {
		return *ib.RealityPrivateKey
	}
	return plain
}

// ── 出站 ─────────────────────────────────────────────────

func renderOutbounds(d *RenderData, key []byte) (Array, error) {
	system := Array{
		Object{
			{Key: "tag", Value: "direct"},
			{Key: "protocol", Value: "freedom"},
			{Key: "settings", Value: Object{{Key: "finalRules", Value: Array{
				Object{{Key: "action", Value: "block"}, {Key: "ip", Value: Array{"geoip:private"}}},
				Object{{Key: "action", Value: "allow"}},
			}}}},
		},
		Object{
			{Key: "tag", Value: "blocked"},
			{Key: "protocol", Value: "blackhole"},
			{Key: "settings", Value: Object{}},
		},
	}

	seen := map[string]bool{"direct": true, "blocked": true, "api": true}
	custom := Array{}
	for _, ob := range d.Outbounds {
		if seen[ob.Tag] {
			continue // 系统保留 tag / 重复 tag 不可占用
		}
		seen[ob.Tag] = true

		frag, err := renderOutbound(ob, key)
		if err != nil {
			return nil, err
		}
		custom = append(custom, frag)
	}

	prependRaw, appendRaw, err := activeOutboundsSplit(d)
	if err != nil {
		return nil, err
	}
	prepend := Array{}
	for _, ob := range prependRaw {
		tag := outboundTag(ob.(Object))
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		prepend = append(prepend, ob)
	}
	appendList := Array{}
	for _, ob := range appendRaw {
		tag := outboundTag(ob.(Object))
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		appendList = append(appendList, ob)
	}

	return append(append(append(system, prepend...), custom...), appendList...), nil
}

func renderOutbound(ob RenderOutbound, key []byte) (Object, error) {
	settingsVal, err := ParseJSON(ob.Settings)
	if err != nil {
		return nil, err
	}
	settings := toObject(settingsVal)
	if ob.Protocol == "wireguard" {
		if sk := outboundSecretKey(ob, key); sk != "" {
			settings = settings.Clone()
			settings.Set("secretKey", sk)
		}
	}

	frag := Object{
		{Key: "tag", Value: ob.Tag},
		{Key: "protocol", Value: ob.Protocol},
	}
	if len(settings) == 0 {
		frag = append(frag, Member{Key: "settings", Value: Object{}})
	} else {
		frag = append(frag, Member{Key: "settings", Value: settings})
	}

	streamVal, err := ParseJSON(ob.StreamSettings)
	if err != nil {
		return nil, err
	}
	stream := toObject(streamVal)
	if len(stream) > 0 {
		stream = stream.Clone()
		for _, subKey := range streamEmptyObjectKeys {
			if v, ok := stream.Get(subKey); ok {
				if arr, isArr := v.(Array); isArr && len(arr) == 0 {
					stream.Set(subKey, Object{})
				}
			}
		}
		frag = append(frag, Member{Key: "streamSettings", Value: stream})
	}

	return frag, nil
}

// outboundSecretKey 复刻 secretKey()：解密失败退回原始值。
func outboundSecretKey(ob RenderOutbound, key []byte) string {
	if ob.SecretKey == nil || *ob.SecretKey == "" {
		return ""
	}
	plain, err := crypto.DecryptString(*ob.SecretKey, key)
	if err != nil {
		return *ob.SecretKey
	}
	return plain
}

// activeOutboundsSplit 复刻 activeOutboundsSplit()：按订阅顺序展开 fetched 出站，
// 丢弃无 tag 与保留 tag 的条目。
func activeOutboundsSplit(d *RenderData) (Array, Array, error) {
	prepend := Array{}
	appendList := Array{}
	for _, sub := range d.Subscriptions {
		val, err := ParseJSON(sub.Fetched)
		if err != nil {
			return nil, nil, err
		}
		var items Array
		switch t := val.(type) {
		case Array:
			items = t
		case nil:
			continue
		default:
			continue // 损坏/非数组 → 视作空
		}
		for _, item := range items {
			ob, ok := item.(Object)
			if !ok {
				continue
			}
			tag := outboundTag(ob)
			if tag == "" || tag == "direct" || tag == "blocked" || tag == "api" {
				continue
			}
			if sub.Prepend {
				prepend = append(prepend, ob)
			} else {
				appendList = append(appendList, ob)
			}
		}
	}
	return prepend, appendList, nil
}

func outboundTag(ob Object) string {
	if v, ok := ob.Get("tag"); ok {
		return stringOf(v)
	}
	return ""
}

// ── 路由 ─────────────────────────────────────────────────

func renderRouting(d *RenderData) (Object, error) {
	rules := Array{
		Object{
			{Key: "type", Value: "field"},
			{Key: "ip", Value: Array{"geoip:private"}},
			{Key: "outboundTag", Value: "blocked"},
		},
		Object{
			{Key: "type", Value: "field"},
			{Key: "protocol", Value: Array{"bittorrent"}},
			{Key: "outboundTag", Value: "blocked"},
		},
	}

	balancerTags := map[string]bool{}
	for _, b := range d.Balancers {
		if !hasUsableSelectors(b) {
			continue // 无 selector 的 balancer 选不出任何出站，跳过防误配
		}
		balancerTags[b.Tag] = true
	}

	tcpOnlyOutboundTags := map[string]bool{}
	for _, ob := range d.Outbounds {
		if ob.Protocol == "socks" || ob.Protocol == "http" {
			tcpOnlyOutboundTags[ob.Tag] = true
		}
	}

	for _, rule := range d.RoutingRules {
		frag := Object{{Key: "type", Value: "field"}}
		if domains, err := stringArray(rule.Domains); err != nil {
			return nil, err
		} else if len(domains) > 0 {
			frag = append(frag, Member{Key: "domain", Value: domains})
		}
		if ips, err := stringArray(rule.IPs); err != nil {
			return nil, err
		} else if len(ips) > 0 {
			frag = append(frag, Member{Key: "ip", Value: ips})
		}
		if rule.Port != nil && *rule.Port != "" {
			frag = append(frag, Member{Key: "port", Value: *rule.Port})
		}
		if rule.Network != nil && *rule.Network != "" {
			frag = append(frag, Member{Key: "network", Value: *rule.Network})
		}
		if rule.Protocol != nil && *rule.Protocol != "" {
			frag = append(frag, Member{Key: "protocol", Value: Array{*rule.Protocol}})
		}
		if rule.InboundTag != nil && *rule.InboundTag != "" {
			frag = append(frag, Member{Key: "inboundTag", Value: Array{*rule.InboundTag}})
		}
		// 空规则（无任何匹配条件）会命中全部流量，跳过防误配。
		if len(frag) <= 1 {
			continue
		}

		key := "outboundTag"
		if balancerTags[rule.OutboundTag] {
			key = "balancerTag"
		}
		frag = append(frag, Member{Key: key, Value: rule.OutboundTag})

		// UDP 兼容护栏：TCP-only 出站且该规则会吃掉 UDP 时，先出一条同条件 udp→direct。
		if key == "outboundTag" && tcpOnlyOutboundTags[rule.OutboundTag] &&
			(rule.Network == nil || *rule.Network == "udp") {
			udpFrag := frag.Clone()
			udpFrag.Set("network", "udp")
			udpFrag.Set("outboundTag", "direct")
			rules = append(rules, udpFrag)

			if rule.Network == nil {
				frag.Set("network", "tcp")
			}
		}

		rules = append(rules, frag)
	}

	opts := d.Node.RoutingOpts
	routing := Object{{Key: "domainStrategy", Value: opts.DomainStrategy}}
	if opts.DomainMatcher != "" {
		routing = append(routing, Member{Key: "domainMatcher", Value: opts.DomainMatcher})
	}

	if balancers := renderBalancers(d); len(balancers) > 0 {
		routing = append(routing, Member{Key: "balancers", Value: balancers})
	}

	routing = append(routing, Member{Key: "rules", Value: rules})
	return routing, nil
}

// stringArray 复刻 cast 'array' + array_filter(is_string) + array_values。
func stringArray(raw json.RawMessage) (Array, error) {
	val, err := ParseJSON(raw)
	if err != nil {
		return nil, err
	}
	out := Array{}
	for _, v := range toArrayValues(val) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func hasUsableSelectors(b RenderBalancer) bool {
	sel, err := stringArray(b.Selectors)
	return err == nil && len(sel) > 0
}

func renderBalancers(d *RenderData) Array {
	out := Array{}
	for _, b := range d.Balancers {
		selectors, err := stringArray(b.Selectors)
		if err != nil || len(selectors) == 0 {
			continue // 无 selector 的 balancer 选不出任何出站，跳过防误配
		}
		frag := Object{
			{Key: "tag", Value: b.Tag},
			{Key: "selector", Value: selectors},
			{Key: "strategy", Value: Object{{Key: "type", Value: b.Strategy}}},
		}
		if b.FallbackTag != nil && *b.FallbackTag != "" {
			frag = append(frag, Member{Key: "fallbackTag", Value: *b.FallbackTag})
		}
		out = append(out, frag)
	}
	return out
}

// renderObservatory 复刻 renderObservatory()：按 balancer 策略决定 observatory /
// burstObservatory；有 burst 需求时把 leastPing 的 selector 一并并入。
func renderObservatory(d *RenderData) Object {
	observatorySelectors := Array{}
	burstSelectors := Array{}

	for _, b := range d.Balancers {
		selectors, err := stringArray(b.Selectors)
		if err != nil || len(selectors) == 0 {
			continue
		}
		switch {
		case b.Strategy == "leastPing":
			observatorySelectors = append(observatorySelectors, selectors...)
		case b.Strategy == "leastLoad" ||
			((b.Strategy == "random" || b.Strategy == "roundRobin") &&
				b.FallbackTag != nil && *b.FallbackTag != ""):
			burstSelectors = append(burstSelectors, selectors...)
		}
	}

	if len(burstSelectors) > 0 {
		merged := uniqueStrings(append(append(Array{}, burstSelectors...), observatorySelectors...))
		return Object{{Key: "burstObservatory", Value: Object{
			{Key: "subjectSelector", Value: merged},
			{Key: "pingConfig", Value: Object{
				{Key: "destination", Value: "https://www.google.com/generate_204"},
				{Key: "interval", Value: "1m"},
				{Key: "connectivity", Value: ""},
				{Key: "timeout", Value: "5s"},
				{Key: "sampling", Value: 2},
				{Key: "httpMethod", Value: "HEAD"},
			}},
		}}}
	}
	if len(observatorySelectors) > 0 {
		return Object{{Key: "observatory", Value: Object{
			{Key: "subjectSelector", Value: uniqueStrings(observatorySelectors)},
			{Key: "probeURL", Value: "https://www.google.com/generate_204"},
			{Key: "probeInterval", Value: "1m"},
			{Key: "enableConcurrency", Value: true},
		}}}
	}
	return nil
}

// uniqueStrings 复刻 array_unique（保持首次出现顺序）。
func uniqueStrings(in Array) Array {
	seen := map[string]bool{}
	out := Array{}
	for _, v := range in {
		s := stringOf(v)
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// ── 指纹 ─────────────────────────────────────────────────

// FingerprintData 是 fingerprint() 的全部输入（SQL 侧已聚合好的标量）。
type FingerprintData struct {
	ConfigVersion int64
	// Stamps 按 PHP 的表顺序给出：「table:max_updated_at:count」原始三段。
	Stamps []Stamp
	// Relay 是 xray_relay_clients 的聚合（node_id = id OR source_node_id = id）。
	Relay Stamp
	// Users / Plans 是内容摘要的聚合结果。
	UsersCount     int64
	UsersSum       int64
	PlansCount     int64
	PlansSum       int64
	RoutingOptions RoutingOptions
}

// Stamp 是一张表在节点维度的指纹片段。
type Stamp struct {
	Label        string // 表名（PHP 用表名做前缀）
	MaxUpdatedAt string // 库内原文；无行时为 ""
	Count        int64
}

// fingerprintTables 是参与指纹的表与列，顺序即 PHP 的拼接顺序。
var fingerprintTables = []struct {
	Table string
	Col   string
}{
	{"xray_inbounds", "node_id"},
	{"xray_outbounds", "node_id"},
	{"xray_outbound_subscriptions", "node_id"},
	{"xray_balancers", "node_id"},
	{"xray_routing_rules", "node_id"},
}

// Fingerprint 复刻 fingerprint()：sha1(「|」拼接)。
func Fingerprint(d *FingerprintData) (string, error) {
	parts := []string{itoa(d.ConfigVersion)}

	for _, s := range d.Stamps {
		parts = append(parts, s.Label+":"+orDash(s.MaxUpdatedAt)+":"+itoa(s.Count))
	}
	parts = append(parts, "relay:"+orDash(d.Relay.MaxUpdatedAt)+":"+itoa(d.Relay.Count))
	parts = append(parts, "users:"+itoa(d.UsersCount)+":"+itoa(d.UsersSum))
	parts = append(parts, "plans:"+itoa(d.PlansCount)+":"+itoa(d.PlansSum))

	// 节点自身设置：只取真正影响渲染的部分（路由基础设置）。
	// 绝不能含 updated_at / driver_config 全量摘要：心跳每 5 秒写一次，
	// 那会让指纹永不相等、节点每轮都重启内核。
	routingJSON, err := phpEncodeDefault(Object{
		{Key: "domainStrategy", Value: d.RoutingOptions.DomainStrategy},
		{Key: "domainMatcher", Value: d.RoutingOptions.DomainMatcher},
	})
	if err != nil {
		return "", err
	}
	parts = append(parts, "nodecfg:"+sha1Hex(routingJSON))

	return sha1Hex(strings.Join(parts, "|")), nil
}

func sha1Hex(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:])
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
