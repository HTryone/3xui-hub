package xray

import (
	"encoding/json"

	"controlhub-server/internal/driver/internal/phpjson"
	"controlhub-server/internal/driver/internal/phpval"
	"controlhub-server/internal/driver/internal/secrets"
)

// ── 出站 ────────────────────────────────────────────────────

// renderOutbounds 复刻 renderOutbounds()：
//
// 内置 direct（freedom + finalRules 阻私有网段）+ blocked（blackhole），
// 其后按 sort 追加节点自定义出站，并合并该节点的「出站订阅」出站。
//
// 顺序（对齐 3x-ui mergeSubscriptionOutbounds）：
//
//	内置 direct/blocked → 订阅 prepend → 自定义出站 → 订阅 append
//
// 内置出站恒在最前，保证 direct 作为默认兜底出口不被订阅顶掉；
// 订阅 tag 与已有 tag 冲突时丢弃（自定义/内置为准），避免 xray 报重复 tag。
func renderOutbounds(d *Data, key []byte) (phpjson.Array, error) {
	system := systemOutbounds()
	seen := map[string]bool{"direct": true, "blocked": true, "api": true}

	custom := phpjson.Array{}
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
	prepend := phpjson.Array{}
	for _, raw := range prependRaw {
		ob, ok := raw.(phpjson.Object)
		if !ok {
			continue
		}
		tag := outboundTag(ob)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		prepend = append(prepend, ob)
	}
	appendList := phpjson.Array{}
	for _, raw := range appendRaw {
		ob, ok := raw.(phpjson.Object)
		if !ok {
			continue
		}
		tag := outboundTag(ob)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		appendList = append(appendList, ob)
	}

	return append(append(append(system, prepend...), custom...), appendList...), nil
}

// systemOutbounds 是内置的 direct / blocked。
//
// freedom 挂 finalRules（block geoip:private + allow）：不依赖 dat 的私网出站兜底。
func systemOutbounds() phpjson.Array {
	return phpjson.Array{
		phpjson.Object{
			{Key: "tag", Value: "direct"},
			{Key: "protocol", Value: "freedom"},
			{Key: "settings", Value: phpjson.Object{{Key: "finalRules", Value: phpjson.Array{
				phpjson.Object{{Key: "action", Value: "block"}, {Key: "ip", Value: phpjson.Array{"geoip:private"}}},
				phpjson.Object{{Key: "action", Value: "allow"}},
			}}}},
		},
		phpjson.Object{
			{Key: "tag", Value: "blocked"},
			{Key: "protocol", Value: "blackhole"},
			{Key: "settings", Value: phpjson.Object{}},
		},
	}
}

func renderOutbound(ob Outbound, key []byte) (phpjson.Object, error) {
	settingsVal, err := phpjson.ParseOrdered(ob.Settings)
	if err != nil {
		return nil, err
	}
	settings := phpval.ToObject(settingsVal)
	if ob.Protocol == "wireguard" {
		if sk := outboundSecretKey(ob, key); sk != "" {
			settings = settings.Clone()
			settings.Set("secretKey", sk)
		}
	}

	frag := phpjson.Object{
		{Key: "tag", Value: ob.Tag},
		{Key: "protocol", Value: ob.Protocol},
	}
	if len(settings) == 0 {
		frag = append(frag, phpjson.Member{Key: "settings", Value: phpjson.Object{}})
	} else {
		frag = append(frag, phpjson.Member{Key: "settings", Value: settings})
	}

	streamVal, err := phpjson.ParseOrdered(ob.StreamSettings)
	if err != nil {
		return nil, err
	}
	stream := phpval.ToObject(streamVal)
	if len(stream) > 0 {
		stream = stream.Clone()
		// 与入站渲染同守：空数组子对象必须输出 {}
		for _, subKey := range streamEmptyObjectKeys {
			if v, ok := stream.Get(subKey); ok {
				if arr, isArr := v.(phpjson.Array); isArr && len(arr) == 0 {
					stream.Set(subKey, phpjson.Object{})
				}
			}
		}
		frag = append(frag, phpjson.Member{Key: "streamSettings", Value: stream})
	}

	return frag, nil
}

// outboundSecretKey 复刻 secretKey()：解密失败退回原始值（兼容明文存量）。
func outboundSecretKey(ob Outbound, key []byte) string {
	if ob.SecretKey == nil || *ob.SecretKey == "" {
		return ""
	}
	if len(key) != 32 {
		return *ob.SecretKey
	}
	return secrets.DecryptOrRaw(*ob.SecretKey, key)
}

// activeOutboundsSplit 复刻 activeOutboundsSplit()：按订阅顺序展开 fetched 出站，
// 丢弃无 tag 与保留 tag 的条目。
func activeOutboundsSplit(d *Data) (phpjson.Array, phpjson.Array, error) {
	prepend := phpjson.Array{}
	appendList := phpjson.Array{}
	for _, sub := range d.Subscriptions {
		val, err := phpjson.ParseOrdered(sub.Fetched)
		if err != nil {
			return nil, nil, err
		}
		items, ok := val.(phpjson.Array)
		if !ok {
			continue // 损坏/非数组 → 视作空
		}
		for _, item := range items {
			ob, ok := item.(phpjson.Object)
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

func outboundTag(ob phpjson.Object) string {
	if v, ok := ob.Get("tag"); ok {
		return phpval.String(v)
	}
	return ""
}

// ── 路由 ────────────────────────────────────────────────────

// renderRouting 复刻 renderRouting()：内置基础规则（与 3x-ui 默认一致）打头，
// 其后是节点自定义规则。
func renderRouting(d *Data) (phpjson.Object, error) {
	rules := phpjson.Array{
		phpjson.Object{
			{Key: "type", Value: "field"},
			{Key: "ip", Value: phpjson.Array{"geoip:private"}},
			{Key: "outboundTag", Value: "blocked"},
		},
		phpjson.Object{
			{Key: "type", Value: "field"},
			{Key: "protocol", Value: phpjson.Array{"bittorrent"}},
			{Key: "outboundTag", Value: "blocked"},
		},
	}

	// balancer tag 集合：规则目标命中 balancer 时渲染成 balancerTag（而非 outboundTag）
	balancerTags := map[string]bool{}
	for _, b := range d.Balancers {
		if !hasUsableSelectors(b) {
			continue // 无 selector 的 balancer 选不出任何出站，跳过防误配
		}
		balancerTags[b.Tag] = true
	}

	// UDP 兼容护栏：xray 内核的 socks / http 出站未实现 UDP（SOCKS5 UDP ASSOCIATE），
	// 被路由进这些出站的 UDP 流量会静默黑洞（实测：DNS 全部超时，客户端
	// 「测延迟正常但没网」）。凡自定义规则会把 UDP 送进 TCP-only 出站时，
	// 在该规则前注入一条「同匹配条件 + network:udp → direct」的护栏。
	tcpOnlyOutboundTags := map[string]bool{}
	for _, ob := range d.Outbounds {
		if ob.Protocol == "socks" || ob.Protocol == "http" {
			tcpOnlyOutboundTags[ob.Tag] = true
		}
	}

	for _, rule := range d.RoutingRules {
		frag := phpjson.Object{{Key: "type", Value: "field"}}
		if domains, err := stringArray(rule.Domains); err != nil {
			return nil, err
		} else if len(domains) > 0 {
			frag = append(frag, phpjson.Member{Key: "domain", Value: domains})
		}
		if ips, err := stringArray(rule.IPs); err != nil {
			return nil, err
		} else if len(ips) > 0 {
			frag = append(frag, phpjson.Member{Key: "ip", Value: ips})
		}
		if rule.Port != nil && *rule.Port != "" {
			frag = append(frag, phpjson.Member{Key: "port", Value: *rule.Port})
		}
		if rule.Network != nil && *rule.Network != "" {
			frag = append(frag, phpjson.Member{Key: "network", Value: *rule.Network})
		}
		if rule.Protocol != nil && *rule.Protocol != "" {
			frag = append(frag, phpjson.Member{Key: "protocol", Value: phpjson.Array{*rule.Protocol}})
		}
		if rule.InboundTag != nil && *rule.InboundTag != "" {
			frag = append(frag, phpjson.Member{Key: "inboundTag", Value: phpjson.Array{*rule.InboundTag}})
		}
		// 空规则（无任何匹配条件）会命中全部流量，跳过防误配。
		if len(frag) <= 1 {
			continue
		}

		key := "outboundTag"
		if balancerTags[rule.OutboundTag] {
			key = "balancerTag"
		}
		frag = append(frag, phpjson.Member{Key: key, Value: rule.OutboundTag})

		if key == "outboundTag" && tcpOnlyOutboundTags[rule.OutboundTag] &&
			(rule.Network == nil || *rule.Network == "udp") {
			udpFrag := frag.Clone()
			udpFrag.Set("network", "udp")
			udpFrag.Set("outboundTag", "direct") // UDP 回落直连，不进 TCP-only 出站
			rules = append(rules, udpFrag)

			// 原规则限定为 tcp（若原本不限网络），避免 udp 分支重复命中本规则
			if rule.Network == nil {
				frag.Set("network", "tcp")
			}
		}

		rules = append(rules, frag)
	}

	opts := d.Node.RoutingOpts
	routing := phpjson.Object{{Key: "domainStrategy", Value: opts.DomainStrategy}}
	if opts.DomainMatcher != "" {
		routing = append(routing, phpjson.Member{Key: "domainMatcher", Value: opts.DomainMatcher})
	}

	if balancers := renderBalancers(d); len(balancers) > 0 {
		routing = append(routing, phpjson.Member{Key: "balancers", Value: balancers})
	}

	routing = append(routing, phpjson.Member{Key: "rules", Value: rules})
	return routing, nil
}

// stringArray 复刻 cast 'array' + array_filter(is_string) + array_values。
func stringArray(raw json.RawMessage) (phpjson.Array, error) {
	val, err := phpjson.ParseOrdered(raw)
	if err != nil {
		return nil, err
	}
	out := phpjson.Array{}
	for _, v := range phpval.ToArrayValues(val) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out, nil
}

func hasUsableSelectors(b Balancer) bool {
	sel, err := stringArray(b.Selectors)
	return err == nil && len(sel) > 0
}

func renderBalancers(d *Data) phpjson.Array {
	out := phpjson.Array{}
	for _, b := range d.Balancers {
		selectors, err := stringArray(b.Selectors)
		if err != nil || len(selectors) == 0 {
			continue // 无 selector 的 balancer 选不出任何出站，跳过防误配
		}
		frag := phpjson.Object{
			{Key: "tag", Value: b.Tag},
			{Key: "selector", Value: selectors},
			{Key: "strategy", Value: phpjson.Object{{Key: "type", Value: b.Strategy}}},
		}
		if b.FallbackTag != nil && *b.FallbackTag != "" {
			frag = append(frag, phpjson.Member{Key: "fallbackTag", Value: *b.FallbackTag})
		}
		out = append(out, frag)
	}
	return out
}

// renderObservatory 复刻 renderObservatory()：按 balancer 策略决定
// observatory / burstObservatory；有 burst 需求时把 leastPing 的 selector 一并并入。
//
// 这两个段必须位于 config 根（而非 routing 内）——
// xray 对 leastPing / leastLoad 及「random|roundRobin + fallbackTag」策略
// 需要观测器数据，否则启动即报 "not all dependencies are resolved"。
func renderObservatory(d *Data) phpjson.Object {
	observatorySelectors := phpjson.Array{}
	burstSelectors := phpjson.Array{}

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
		merged := uniqueStrings(append(append(phpjson.Array{}, burstSelectors...), observatorySelectors...))
		return phpjson.Object{{Key: "burstObservatory", Value: phpjson.Object{
			{Key: "subjectSelector", Value: merged},
			{Key: "pingConfig", Value: phpjson.Object{
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
		return phpjson.Object{{Key: "observatory", Value: phpjson.Object{
			{Key: "subjectSelector", Value: uniqueStrings(observatorySelectors)},
			{Key: "probeURL", Value: "https://www.google.com/generate_204"},
			{Key: "probeInterval", Value: "1m"},
			{Key: "enableConcurrency", Value: true},
		}}}
	}
	return nil
}

// uniqueStrings 复刻 array_unique（保持首次出现顺序）。
func uniqueStrings(in phpjson.Array) phpjson.Array {
	seen := map[string]bool{}
	out := phpjson.Array{}
	for _, v := range in {
		s := phpval.String(v)
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}
