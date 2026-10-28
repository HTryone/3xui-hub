package subscription

import (
	"net"
	"strings"
)

// 节点双地址（issue #88）：一台机器同时有 v4/v6 时，节点可存两个对外地址，
// 订阅链接按「入站绑定的地址族」选对应地址。
//
// host_v6 存在 nodes.driver_config（nodes 表没有该列，老库也没有 ALTER），
// 与 node_secret / xray_version 同住一个 JSON 列；它是**纯地址**，不需要解密。

// IsV6Listen 判断入站的 listen 是否绑在 IPv6 地址族上。
//
// 认的形态（xray 的 listen 取值口径）：
//
//	"::" / "::1" / "2001:db8::1"      纯 IPv6 字面量
//	"[::]:443" / "[2001:db8::1]:443"  带方括号（可带端口）的写法
//
// 不认（返回 false）：
//
//	""            未填（DB 默认 0.0.0.0 之外的 NULL/空串）
//	"0.0.0.0"     IPv4 通配
//	"1.2.3.4"     IPv4 字面量
//	"/tmp/x.sock" unix socket 路径
//	"example.com" 域名
//
// 判据用 net.ParseIP：只有「能解析且 To4() == nil」才算 v6，避免把
// "::ffff:1.2.3.4" 这类 IPv4-mapped 地址误判成 v6 入口。
func IsV6Listen(listen string) bool {
	addr := phpTrim(listen)
	if addr == "" {
		return false
	}

	// "[::]:443" / "[2001:db8::1]"：先取方括号内的主机部分。
	if strings.HasPrefix(addr, "[") {
		end := strings.IndexByte(addr, ']')
		if end < 0 {
			return false
		}
		addr = addr[1:end]
	} else if i := strings.LastIndex(addr, ":"); i >= 0 && !strings.Contains(addr, "::") {
		// "2001:db8::1" 这类裸字面量本身含多个冒号，不能按 host:port 切；
		// 只有「非 compressed 形式 + 末段是端口」才切（实际 xray 不会这么写，
		// 但入站表单允许自由输入，这里保守处理）。
		if isAllDigitsASCII(addr[i+1:]) && strings.Count(addr, ":") == 1 {
			addr = addr[:i]
		}
	}
	if addr == "" {
		return false
	}

	// 带 zone 的链路本地地址（fe80::1%eth0）。
	if i := strings.IndexByte(addr, '%'); i >= 0 {
		addr = addr[:i]
	}

	ip := net.ParseIP(addr)
	return ip != nil && ip.To4() == nil
}

// HostV6 取节点 driver_config.host_v6（纯地址，不明文/密文之分）。
//
// driver_config 是整块 JSON；坏 JSON 或键不存在都返回空串（等价于「没配 v6」）。
func (n Node) HostV6() string {
	cfg := decodeJSONAny(n.DriverConfig)
	v, ok := jsonField(cfg, "host_v6").(string)
	if !ok {
		return ""
	}
	return phpTrim(v)
}

// subscriptionHostFor 选出该入站链接该用哪个地址。
//
// 规则（issue #88）：
//   - 入站绑 IPv6 且配了 host_v6 → host_v6
//   - 否则回落节点主地址 host
//   - 两者都空 → 不给地址（调用方跳过该入站，绝不产出坏链接）
//
// 入站 listen 为空/0.0.0.0（老数据）时行为与改动前完全一致。
func subscriptionHostFor(host, hostV6, listen string) (string, bool) {
	if IsV6Listen(listen) && hostV6 != "" {
		return hostV6, true
	}
	if host != "" {
		return host, true
	}
	return "", false
}
