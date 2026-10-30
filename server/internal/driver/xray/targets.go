package xray

import "strings"

// 逐条对齐 backend/app/Drivers/Xray/RealityTargetCatalog.php。
//
// Reality 目标候选清单（面板侧维护，agent 只当「哑探测器」）。
// 候选目录放在面板侧的好处：调整候选域名不需要重新给节点发 agent 二进制。
// 清单口径参考 3x-ui 的内置候选 —— 都是同时满足 TLS 1.3 + h2 + 证书链受信任的大厂站点。

// MaxTargets 是单次扫描候选上限（与 agent 侧 realityScanMaxTargets 一致，双保险）。
const MaxTargets = 20

// defaultTargets 是内置常用候选（留空即用它），顺序与 PHP 一致。
var defaultTargets = []string{
	"www.cloudflare.com:443",
	"www.samsung.com:443",
	"www.nvidia.com:443",
	"www.apple.com:443",
	"aws.amazon.com:443",
	"www.microsoft.com:443",
	"www.icloud.com:443",
	"www.tesla.com:443",
	"dl.google.com:443",
	"www.yahoo.com:443",
	"www.bing.com:443",
	"www.amazon.com:443",
	// issue #96：补到 MaxTargets=20。不含 www.intel.com —— 它的证书是 *.intel.com，
	// SAN 里没有 www.intel.com 这个具体名，手填该 SNI 会握手失败（issue 作者同口径）。
	// 这 8 条实测 TLS1.3 + ALPN h2 + 链校验 ok，且 SAN 含自身具体名。
	"www.sony.com:443",
	"www.bmw.com:443",
	"www.oracle.com:443",
	"www.cisco.com:443",
	"www.ibm.com:443",
	"www.autodesk.com:443",
	"www.paypal.com:443",
	"www.dell.com:443",
}

// DefaultTargets 返回内置候选的副本（调用方可以随意改动）。
func DefaultTargets() []string {
	out := make([]string, len(defaultTargets))
	copy(out, defaultTargets)
	return out
}

// SanitizeTargets 对应 sanitize()：去空白、去重、截上限。
//
// 空入参返回空切片（由调用方决定是否回落内置候选）——
// 面板的 reality-scan 接口就是这么用的：管理员没填 → 用 DefaultTargets()。
func SanitizeTargets(targets []string) []string {
	out := make([]string, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, raw := range targets {
		target := strings.TrimSpace(raw)
		if target == "" || seen[target] {
			continue
		}
		seen[target] = true
		out = append(out, target)
		if len(out) >= MaxTargets {
			break
		}
	}
	return out
}
