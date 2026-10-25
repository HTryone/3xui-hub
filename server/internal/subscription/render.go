package subscription

import (
	"strings"
)

// 逐条复刻 SubscriptionService.php 的三个渲染器：
//
//	generateClash() / generateSingbox() / generateBase64()（后者在 service.go）
//
// Clash 是**手写字符串拼接**（不是 yaml 库）：字段顺序、引号、缩进、空行都由
// PHP 的拼接顺序决定；这里逐行照抄，连"哪些行可能重复"都保持不变。

// clashInfoProxyKeys 是 generateClash 里输出单个 proxy 的固定字段顺序。
//
// 与 PHP 的 if 序列一一对应，不要按「更合理」的顺序重排：订阅正文是字节级契约。
func (s *Service) generateClash(user *User, plan *Plan, links []string, cfg *siteConfigReader) string {
	proxies := make([]phpMap, 0, len(links)+3)
	proxyNames := make([]string, 0, len(links))
	regionCounters := map[string]int{}

	// 流量信息节点排在真实节点之前；它们不进 proxyNames（不加入代理组）
	proxies = append(proxies, s.createInfoProxies(user, plan, cfg)...)

	for _, link := range links {
		if proxy := s.parseLinkToClashProxy(link, regionCounters, cfg); proxy != nil {
			proxies = append(proxies, proxy)
			proxyNames = append(proxyNames, phpString(field(proxy, "name")))
		}
	}

	if len(proxies) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("proxies:\n")
	for _, proxy := range proxies {
		b.WriteString("  - name: \"" + phpString(field(proxy, "name")) + "\"\n")
		b.WriteString("    type: " + phpString(field(proxy, "type")) + "\n")
		b.WriteString("    server: " + phpString(field(proxy, "server")) + "\n")
		b.WriteString("    port: " + phpString(field(proxy, "port")) + "\n")

		writeClashScalar(&b, proxy, "uuid", "uuid")
		writeClashScalar(&b, proxy, "cipher", "cipher")
		writeClashScalar(&b, proxy, "password", "password")
		writeClashScalar(&b, proxy, "encryption", "encryption")
		writeClashScalar(&b, proxy, "alter_id", "alter_id")
		writeClashScalar(&b, proxy, "flow", "flow")
		writeClashBool(&b, proxy, "udp", "udp")
		writeClashBool(&b, proxy, "sniffing", "sniffing")
		writeClashBool(&b, proxy, "tls", "tls")

		// PHP: `if (isset($proxy['sni'])) $yaml .= "    servername: {$proxy['sni']}\n";`
		writeClashScalar(&b, proxy, "sni", "servername")
		writeClashScalar(&b, proxy, "servername", "servername")
		writeClashScalar(&b, proxy, "client-fingerprint", "client-fingerprint")

		if reality, ok := field(proxy, "reality-opts").(phpMap); ok {
			b.WriteString("    reality-opts:\n")
			// 必须给 public-key / short-id 加引号：066559 这类纯数字前导零的 sid
			// 不加引号会被 YAML 当数值解析，前导零丢失后 mihomo 校验失败。
			if v, has := reality.get("public-key"); has {
				b.WriteString("      public-key: \"" + phpString(v) + "\"\n")
			}
			if v, has := reality.get("short-id"); has {
				b.WriteString("      short-id: \"" + phpString(v) + "\"\n")
			}
		}

		writeClashScalar(&b, proxy, "network", "network")

		if wsOpts, ok := field(proxy, "ws-opts").(phpMap); ok {
			b.WriteString("    ws-opts:\n")
			b.WriteString("      path: \"" + phpString(field(wsOpts, "path")) + "\"\n")
			if headers, okHeaders := field(wsOpts, "headers").(phpMap); okHeaders {
				b.WriteString("      headers:\n")
				for _, kv := range headers {
					b.WriteString("        " + kv.Key + ": \"" + phpString(kv.Value) + "\"\n")
				}
			}
		}

		if xhttpOpts, ok := field(proxy, "xhttp-opts").(phpMap); ok {
			b.WriteString("    xhttp-opts:\n")
			b.WriteString("      path: \"" + phpString(field(xhttpOpts, "path")) + "\"\n")
			b.WriteString("      mode: \"" + phpString(field(xhttpOpts, "mode")) + "\"\n")
			if v, has := xhttpOpts.get("x-padding-bytes"); has {
				b.WriteString("      x-padding-bytes: " + phpString(v) + "\n")
			}
		}

		// 注意：PHP 的 generateClash 没有输出 http-upgrade-opts 的分支
		// （httpupgrade 只影响 network 值），此处保持一致 —— 不是遗漏。
	}

	b.WriteString("\nproxy-groups:\n")
	b.WriteString("  - name: \"Proxy\"\n")
	b.WriteString("    type: select\n")
	b.WriteString("    proxies:\n")
	for _, name := range proxyNames {
		b.WriteString("      - \"" + name + "\"\n")
	}

	b.WriteString("\nrules:\n")
	b.WriteString("  - MATCH,Proxy\n")

	return b.String()
}

// writeClashScalar 输出 `    {label}: {value}`（键存在即输出，空串也输出）。
func writeClashScalar(b *strings.Builder, proxy phpMap, key, label string) {
	if v, has := proxy.get(key); has {
		b.WriteString("    " + label + ": " + phpString(v) + "\n")
	}
}

// writeClashBool 输出 `    {label}: true|false`（PHP 真值语义：空串/"0" 为 false）。
func writeClashBool(b *strings.Builder, proxy phpMap, key, label string) {
	if v, has := proxy.get(key); has {
		b.WriteString("    " + label + ": " + phpBoolText(v) + "\n")
	}
}

func phpBoolText(v any) string {
	if phpBool(v) {
		return "true"
	}
	return "false"
}

// field 取 phpMap 的键值（缺失返回 nil，与 PHP 的 $arr[$k] ?? null 一致）。
func field(m phpMap, key string) any {
	if m == nil {
		return nil
	}
	v, _ := m.get(key)
	return v
}

// generateSingbox 对应 generateSingbox()：
// direct 固定第一，其次（可选提示条目）→ 信息出站 → 真实出站。
func (s *Service) generateSingbox(user *User, plan *Plan, links []string, cfg *siteConfigReader) string {
	regionCounters := map[string]int{}

	infoOutbounds := s.createInfoOutbounds(user, plan, cfg)

	realOutbounds := make([]phpMap, 0, len(links))
	for _, link := range links {
		if outbound := s.parseLinkToSingboxOutbound(link, regionCounters, cfg); outbound != nil {
			realOutbounds = append(realOutbounds, outbound)
		}
	}

	// 有链接但全部被跳过（无真实节点出站）时注入提示条目，排 direct 之后第一位；
	// $links 本身为空保持现状（返回空串），不加提示。
	var hintOutbounds []phpMap
	if len(links) > 0 && len(realOutbounds) == 0 {
		hintOutbounds = append(hintOutbounds, obj(
			"type", "shadowsocks",
			"tag", "不支持Sing-box请用Clash或Base64",
			"server", "127.0.0.1",
			"server_port", jsonNumber("1"),
			"method", "aes-256-gcm",
			"password", "info",
		))
	}

	outbounds := make([]any, 0, len(hintOutbounds)+len(infoOutbounds)+len(realOutbounds))
	for _, o := range hintOutbounds {
		outbounds = append(outbounds, o)
	}
	for _, o := range infoOutbounds {
		outbounds = append(outbounds, o)
	}
	for _, o := range realOutbounds {
		outbounds = append(outbounds, o)
	}

	if len(outbounds) == 0 {
		return ""
	}

	allOutbounds := make([]any, 0, len(outbounds)+1)
	allOutbounds = append(allOutbounds, obj("type", "direct", "tag", "direct"))
	allOutbounds = append(allOutbounds, outbounds...)

	config := obj("outbounds", allOutbounds)
	return jsonPretty(config)
}
