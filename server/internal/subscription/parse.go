package subscription

import (
	"strings"
)

// 逐条复刻 SubscriptionService.php 的解析段：
//
//	parseLinkToClashProxy() / parseLinkToSingboxOutbound() /
//	applyClashTlsAndNetwork() / parseSsCredentials() / parseHysteriaPassword()
//
// 两个解析器共用「协议 + userinfo + host + port / query / #name」的解析结果，
// 只在落地结构上分叉（Clash proxy 字典 vs Sing-box outbound 字典）。

// parsedLink 是链接的公共解析结果。
type parsedLink struct {
	Protocol string
	UserPart string
	Server   string
	Port     int64
	Params   map[string]string
	Name     string
}

// parseLink 对应两个解析器开头的正则 + 参数 + 名称解析。
// 解析失败（协议不支持 / 形态不匹配）返回 (nil, false)，调用方跳过该节点。
func parseLink(link string) (*parsedLink, bool) {
	m := linkPattern.FindStringSubmatch(link)
	if m == nil {
		return nil, false
	}

	parsed := &parsedLink{
		Protocol: m[1],
		UserPart: m[2],
		Server:   m[3],
		Port:     parseIntPrefix(m[4]),
		Params:   map[string]string{},
	}

	// 解析参数：'?' 之后到 '#' 之前
	if q := strings.IndexByte(link, '?'); q >= 0 {
		queryString := link[q+1:]
		if h := strings.IndexByte(queryString, '#'); h >= 0 {
			queryString = queryString[:h]
		}
		parsed.Params = parseStr(queryString)
	}

	// 解析名称：#fragment（url 解码）
	if h := strings.LastIndex(link, "#"); h >= 0 {
		parsed.Name = urldecode(link[h+1:])
	}
	return parsed, true
}

// parsedSsCredentials 对应 parseSsCredentials()：base64(method:password) → [method, password]。
func parseSsCredentials(userInfo string) (string, string, bool) {
	decoded, ok := decodeBase64URL(userInfo)
	if !ok {
		return "", "", false
	}
	colon := strings.IndexByte(decoded, ':')
	if colon < 0 {
		return "", "", false
	}
	method, password := decoded[:colon], decoded[colon+1:]
	if method == "" || password == "" {
		return "", "", false
	}
	return method, password, true
}

// parseHysteriaPassword 对应 parseHysteriaPassword()：base64(密码[:/aead]) → 明文密码。
// aead 后缀（/gch、/aead）是拥塞控制/混淆方式，客户端自动处理，不进密码字段。
func parseHysteriaPassword(userPart string) (string, bool) {
	decoded, ok := decodeBase64URL(userPart)
	if !ok || decoded == "" {
		return "", false
	}
	decoded = hysteriaAeadPattern.ReplaceAllString(decoded, "")
	if decoded == "" {
		return "", false
	}
	return decoded, true
}

// applyClashTlsAndNetwork 对应 applyClashTlsAndNetwork()：mihomo 的 TLS / 传输段（vmess 分支复用）。
func applyClashTlsAndNetwork(proxy *phpMap, params map[string]string, server string) {
	// TLS / Reality
	if security, ok := params["security"]; ok {
		switch security {
		case "tls":
			proxy.set("tls", true)
			if sni, has := params["sni"]; has {
				proxy.set("sni", cleanSni(sni))
			}
		case "reality":
			proxy.set("tls", true)
			proxy.set("servername", cleanSni(mapOr(params, "sni", server)))
			if fp, has := params["fp"]; has {
				proxy.set("client-fingerprint", fp)
			}
			if pbk, has := params["pbk"]; has {
				reality := obj("public-key", pbk)
				if sid, hasSid := params["sid"]; hasSid {
					reality.set("short-id", sid)
				}
				proxy.set("reality-opts", reality)
			}
		}
	}

	// 传输协议
	proxy.set("network", mapOr(params, "type", "tcp"))
	if typ, has := params["type"]; has {
		switch typ {
		case "ws":
			wsOpts := obj("path", mapOr(params, "path", "/"))
			if host, hasHost := params["host"]; hasHost {
				wsOpts.set("headers", obj("Host", host))
			}
			proxy.set("ws-opts", wsOpts)
		case "xhttp":
			xhttpOpts := obj(
				"path", mapOr(params, "path", "/"),
				"mode", mapOr(params, "mode", "auto"),
			)
			if extra, hasExtra := params["extra"]; hasExtra {
				if decoded, isMap := decodeJSONAny(extra).(phpMap); isMap {
					if padding, found := decoded.get("xPaddingBytes"); found {
						xhttpOpts.set("x-padding-bytes", padding)
					}
				}
			}
			proxy.set("xhttp-opts", xhttpOpts)
		case "httpupgrade":
			// mihomo：network=httpupgrade + http-upgrade-opts（键名与 3x-ui clash 输出一致）
			huOpts := phpMap{}
			if p := params["path"]; p != "" {
				huOpts.set("path", p)
			}
			if h := params["host"]; h != "" {
				huOpts.set("headers", obj("Host", h))
			}
			if len(huOpts) > 0 {
				proxy.set("http-upgrade-opts", huOpts)
			}
		}
	}
}

// mapOr 对应 PHP 的 `$arr[$key] ?? $default`。
func mapOr(m map[string]string, key, def string) string {
	if v, ok := m[key]; ok {
		return v
	}
	return def
}

// parseLinkToClashProxy 对应 parseLinkToClashProxy()：解析失败/不支持返回 nil（跳过该节点）。
func (s *Service) parseLinkToClashProxy(link string, regionCounters map[string]int, cfg *siteConfigReader) phpMap {
	parsed, ok := parseLink(link)
	if !ok {
		return nil
	}

	name := parsed.Name
	if phpEmptyString(name) {
		name = fallbackName(parsed)
	}
	// 应用名称处理（用户 ID 清理 / 正则重命名 / 旗帜）
	name = s.processNodeName(name, regionCounters, cfg)
	if phpTrim(name) == "" {
		name = fallbackName(parsed)
	}

	switch parsed.Protocol {
	case "ss":
		method, password, ok := parseSsCredentials(parsed.UserPart)
		if !ok {
			return nil
		}
		return obj(
			"name", name,
			"type", "ss",
			"server", parsed.Server,
			"port", jsonNumber(itoa(parsed.Port)),
			"cipher", method,
			"password", password,
			"udp", true,
		)

	case "vmess":
		decoded, ok := decodeBase64URL(parsed.UserPart)
		if !ok {
			return nil
		}
		auth, isMap := decodeJSONAny(decoded).(phpMap)
		if !isMap {
			return nil
		}
		vmessUUID := jsonFieldString(auth, "id")
		if vmessUUID == "" {
			return nil
		}

		proxy := obj(
			"name", name,
			"type", "vmess",
			"server", parsed.Server,
			"port", jsonNumber(itoa(parsed.Port)),
			"udp", true,
			"password", vmessUUID,
			"client-fingerprint", mapOr(parsed.Params, "fp", "chrome"),
		)
		// mihomo：aid≠0 时透传 alter_id + 对应加密；aid=0 一律 none
		if phpInt(jsonField(auth, "alterId")) > 0 {
			proxy.set("alter_id", jsonNumber(itoa(phpInt(jsonField(auth, "alterId")))))
			proxy.set("encryption", mapOr(parsed.Params, "enc", "auto"))
		} else {
			proxy.set("encryption", "none")
		}
		applyClashTlsAndNetwork(&proxy, parsed.Params, parsed.Server)
		return proxy

	case "hysteria2", "hysteria":
		password, ok := parseHysteriaPassword(parsed.UserPart)
		if !ok {
			return nil
		}
		proxy := obj(
			"name", name,
			"type", parsed.Protocol,
			"server", parsed.Server,
			"port", jsonNumber(itoa(parsed.Port)),
			"password", password,
			"udp", true,
		)
		if _, hasSni := parsed.Params["sni"]; hasSni || mapOr(parsed.Params, "insecure", "0") != "0" {
			proxy.set("tls", true)
			proxy.set("servername", cleanSni(mapOr(parsed.Params, "sni", parsed.Server)))
		}
		if fp, has := parsed.Params["fp"]; has {
			proxy.set("client-fingerprint", fp)
		}
		// mihomo 的 hysteria 依赖 sniffing 探测流量类型，显式开启
		proxy.set("sniffing", true)
		return proxy
	}

	proxy := obj(
		"name", name,
		"type", parsed.Protocol,
		"server", parsed.Server,
		"port", jsonNumber(itoa(parsed.Port)),
		"udp", true,
		"client-fingerprint", mapOr(parsed.Params, "fp", "chrome"),
	)

	// trojan 用 password，vless 用 uuid
	if parsed.Protocol == "trojan" {
		proxy.set("password", parsed.UserPart)
	} else {
		proxy.set("uuid", parsed.UserPart)

		// flow 只透传不发明：链接没有 flow 就绝不输出
		if flow, has := parsed.Params["flow"]; has {
			proxy.set("flow", flow)
		}
		if enc, has := parsed.Params["encryption"]; has {
			proxy.set("encryption", enc)
		} else {
			// VLESS 协议默认使用 none 加密
			proxy.set("encryption", "none")
		}
	}

	// TLS / Reality（与 vmess 分支同一套字段写法，但 vless/trojan 的 sni 走 'sni' 键）
	if security, has := parsed.Params["security"]; has {
		switch security {
		case "tls":
			proxy.set("tls", true)
			if sni, hasSni := parsed.Params["sni"]; hasSni {
				proxy.set("sni", cleanSni(sni))
			}
		case "reality":
			proxy.set("tls", true)
			proxy.set("servername", cleanSni(mapOr(parsed.Params, "sni", parsed.Server)))
			if fp, hasFp := parsed.Params["fp"]; hasFp {
				proxy.set("client-fingerprint", fp)
			}
			if pbk, hasPbk := parsed.Params["pbk"]; hasPbk {
				reality := obj("public-key", pbk)
				if sid, hasSid := parsed.Params["sid"]; hasSid {
					reality.set("short-id", sid)
				}
				proxy.set("reality-opts", reality)
			}
		}
	}

	// 传输协议
	proxy.set("network", mapOr(parsed.Params, "type", "tcp"))
	if typ, has := parsed.Params["type"]; has {
		switch typ {
		case "ws":
			wsOpts := obj("path", mapOr(parsed.Params, "path", "/"))
			if host, hasHost := parsed.Params["host"]; hasHost {
				wsOpts.set("headers", obj("Host", host))
			}
			proxy.set("ws-opts", wsOpts)
		case "xhttp":
			// clash 的 xhttp-opts 保留（mihomo 1.19.31 实测支持）
			xhttpOpts := obj(
				"path", mapOr(parsed.Params, "path", "/"),
				"mode", mapOr(parsed.Params, "mode", "auto"),
			)
			if extra, hasExtra := parsed.Params["extra"]; hasExtra {
				if decoded, isMap := decodeJSONAny(extra).(phpMap); isMap {
					if padding, found := decoded.get("xPaddingBytes"); found {
						xhttpOpts.set("x-padding-bytes", padding)
					}
				}
			}
			proxy.set("xhttp-opts", xhttpOpts)
		case "httpupgrade":
			huOpts := phpMap{}
			if p := parsed.Params["path"]; p != "" {
				huOpts.set("path", p)
			}
			if h := parsed.Params["host"]; h != "" {
				huOpts.set("headers", obj("Host", h))
			}
			if len(huOpts) > 0 {
				proxy.set("http-upgrade-opts", huOpts)
			}
		}
	}

	return proxy
}

// fallbackName 对应 PHP 的 `"{$protocol}-{$server}:{$port}"`。
func fallbackName(parsed *parsedLink) string {
	return parsed.Protocol + "-" + parsed.Server + ":" + itoa(parsed.Port)
}

// parseLinkToSingboxOutbound 对应 parseLinkToSingboxOutbound()。
func (s *Service) parseLinkToSingboxOutbound(link string, regionCounters map[string]int, cfg *siteConfigReader) phpMap {
	parsed, ok := parseLink(link)
	if !ok {
		return nil
	}

	name := parsed.Name
	if phpEmptyString(name) {
		name = fallbackName(parsed)
	}
	// 应用名称处理（用户 ID 清理 / 正则重命名 / 旗帜）
	name = s.processNodeName(name, regionCounters, cfg)
	if phpTrim(name) == "" {
		name = fallbackName(parsed)
	}

	outboundType := parsed.Protocol
	if parsed.Protocol == "ss" {
		outboundType = "shadowsocks"
	}
	outbound := obj(
		"type", outboundType,
		"tag", name,
		"server", parsed.Server,
		"server_port", jsonNumber(itoa(parsed.Port)),
	)

	switch parsed.Protocol {
	case "vless":
		outbound.set("uuid", parsed.UserPart)
		// flow 只透传不发明：链接没有 flow 就绝不输出
		if flow, has := parsed.Params["flow"]; has {
			outbound.set("flow", flow)
		}
		// 注意：sing-box 的 vless 不输出 encryption 字段

	case "trojan":
		outbound.set("password", parsed.UserPart)

	case "vmess":
		decoded, ok := decodeBase64URL(parsed.UserPart)
		if !ok {
			return nil
		}
		auth, isMap := decodeJSONAny(decoded).(phpMap)
		if !isMap {
			return nil
		}
		vmessUUID := jsonFieldString(auth, "id")
		if vmessUUID == "" {
			return nil
		}
		outbound.set("uuid", vmessUUID)
		// sing-box 只认这几种 vmess 加密
		enc := mapOr(parsed.Params, "enc", "auto")
		switch enc {
		case "none", "aes-128-gcm", "chacha20-poly1305", "xchacha20-poly1305":
			outbound.set("cipher", enc)
		default:
			outbound.set("cipher", "auto")
		}

	case "ss":
		method, password, ok := parseSsCredentials(parsed.UserPart)
		if !ok {
			return nil
		}
		outbound.set("method", method)
		outbound.set("password", password)

	case "hysteria2", "hysteria":
		password, ok := parseHysteriaPassword(parsed.UserPart)
		if !ok {
			return nil
		}
		outbound.set("type", parsed.Protocol)
		outbound.set("password", password)
		// sing-box hysteria2 出站要求 TLS 段：有 sni 用 sni，否则默认带 server_name
		tls := obj("enabled", true)
		if sni, has := parsed.Params["sni"]; has {
			tls.set("server_name", cleanSni(sni))
		}
		if mapOr(parsed.Params, "insecure", "0") == "1" {
			tls.set("insecure", true)
		}
		if fp, has := parsed.Params["fp"]; has {
			tls.set("utls", obj("enabled", true, "fingerprint", fp))
		}
		outbound.set("tls", tls)
	}

	// TLS / Reality
	if security, has := parsed.Params["security"]; has {
		switch security {
		case "tls":
			outbound.set("tls", obj(
				"enabled", true,
				"server_name", cleanSni(mapOr(parsed.Params, "sni", parsed.Server)),
			))
		case "reality":
			tlsConfig := obj(
				"enabled", true,
				"server_name", cleanSni(mapOr(parsed.Params, "sni", parsed.Server)),
				"reality", obj(
					"enabled", true,
					"public_key", mapOr(parsed.Params, "pbk", ""),
					"short_id", mapOr(parsed.Params, "sid", ""),
				),
			)
			if fp, hasFp := parsed.Params["fp"]; hasFp {
				tlsConfig.set("utls", obj("enabled", true, "fingerprint", fp))
			}
			outbound.set("tls", tlsConfig)
		}
	}

	// 传输协议
	network := mapOr(parsed.Params, "type", "tcp")
	if network != "" && network != "tcp" && network != "none" {
		// sing-box 1.14.1 不支持 xhttp 等传输：跳过该节点
		switch network {
		case "ws", "grpc", "http", "httpupgrade":
		default:
			return nil
		}
		transport := obj("type", network)
		if path, has := parsed.Params["path"]; has {
			transport.set("path", path)
		}
		if host, has := parsed.Params["host"]; has {
			transport.set("headers", obj("Host", host))
		}
		outbound.set("transport", transport)
	}

	return outbound
}
