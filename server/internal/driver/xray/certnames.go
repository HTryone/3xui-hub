package xray

// REALITY serverNames 的「dest 真实证书」判定。
//
// 历史口径是 SameSite（注册域猜测）：同域放行、跨域拦截。它既会误拦同一张证书里
// 的跨注册域 SAN（2026-10-28：www.samsung.com 的证书带 api.semiconductor.samsung.cn），
// 又会放过同注册域但证书里根本没有的名字。这里改成实际对 dest 做一次 TLS 握手，
// 拿叶子证书的 DNS SAN 当依据；取不到证书时才退回 SameSite（行为不劣于改动前）。

import (
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	// certProbeTimeout 是单次探测（连接 + 握手）的总时限。
	certProbeTimeout = 5 * time.Second
	// certCacheTTL 是探测结果（含失败）的缓存时长，避免每次保存都干等 5 秒。
	certCacheTTL = 10 * time.Minute
)

// 判定依据（ServerNameSuspects 的第二个返回值）。
const (
	CertEvidenceCert     = "cert"      // 依据 dest 出示的叶子证书
	CertEvidenceSameSite = "same-site" // 证书不可得，退回 SameSite 猜测
)

type certCacheEntry struct {
	names   []string
	ok      bool
	expires time.Time
}

var (
	certCacheMu sync.Mutex
	certCache   = map[string]certCacheEntry{}

	// certProbe / certNow 是测试缝：单测不碰网络、不依赖真实时钟。
	certProbe = fetchCertDNSNames
	certNow   = time.Now
)

// CertDNSNames 返回 dest 出示的叶子证书 DNS SAN；第二个返回值 false 表示取不到证书
// （连不上、握手失败、没有可用 DNS SAN）。
//
// 返回的列表保留通配符（形如 *.example.com），只用于匹配，**不能**当可选 serverName
// 使用。结果按 host+port 缓存 10 分钟，失败也缓存。
func CertDNSNames(dest string) ([]string, bool) {
	host, port := destHostPort(dest)
	if host == "" || isIPLiteral(host) {
		// IP 字面量的证书里没有 DNS SAN，直接算「取不到」→ 调用方退回 SameSite。
		return nil, false
	}
	addr := net.JoinHostPort(host, port)

	now := certNow()
	certCacheMu.Lock()
	if entry, hit := certCache[addr]; hit && now.Before(entry.expires) {
		certCacheMu.Unlock()
		return append([]string(nil), entry.names...), entry.ok
	}
	certCacheMu.Unlock()

	// 探测（最长 5 秒）放在锁外，避免一个慢 dest 卡住其它保存请求。
	names, ok := certProbe(host, port)

	certCacheMu.Lock()
	certCache[addr] = certCacheEntry{names: names, ok: ok, expires: now.Add(certCacheTTL)}
	certCacheMu.Unlock()
	return append([]string(nil), names...), ok
}

// CertCoversName 判定 name 是否被证书的 DNS SAN 覆盖。
//
// 通配符按 RFC 6125 严格口径：*.example.com 只覆盖恰好一层（a.example.com 覆盖，
// a.b.example.com 与裸 example.com 不覆盖）。带 * 的 serverName 本身一律不算覆盖。
func CertCoversName(names []string, name string) bool {
	n := normalizeCertName(name)
	if n == "" || strings.Contains(n, "*") {
		return false
	}
	for _, certName := range names {
		if certName == n {
			return true
		}
		if !strings.HasPrefix(certName, "*.") {
			continue
		}
		suffix := certName[1:] // ".example.com"
		if !strings.HasSuffix(n, suffix) {
			continue
		}
		label := strings.TrimSuffix(n, suffix)
		if label != "" && !strings.Contains(label, ".") {
			return true
		}
	}
	return false
}

// ServerNameSuspects 返回 names 中不被 dest 证书覆盖的项。
//
// 证书取不到时退回 SameSite(name, HostOf(dest))（改动前的行为）；dest 解析不出主机名
// 时返回空（与调用点原有的提前放行一致）。evidence 说明本次用的是哪套依据。
func ServerNameSuspects(dest string, names []string) (suspects []string, evidence string) {
	host := HostOf(dest)
	if host == "" {
		return nil, CertEvidenceSameSite
	}

	certNames, ok := CertDNSNames(dest)
	if ok {
		suspects = make([]string, 0, len(names))
		for _, name := range names {
			if !CertCoversName(certNames, name) {
				suspects = append(suspects, name)
			}
		}
		return suspects, CertEvidenceCert
	}

	suspects = make([]string, 0, len(names))
	for _, name := range names {
		if !SameSite(name, host) {
			suspects = append(suspects, name)
		}
	}
	return suspects, CertEvidenceSameSite
}

// destHostPort 把 dest 拆成 host + port（缺省 443）；解析不出返回空 host。
func destHostPort(dest string) (string, string) {
	dest = strings.TrimSpace(dest)
	if dest == "" {
		return "", ""
	}

	if strings.HasPrefix(dest, "[") { // [v6] 或 [v6]:port
		end := strings.IndexByte(dest, ']')
		if end < 0 {
			return "", ""
		}
		host := strings.ToLower(dest[1:end])
		if rest := dest[end+1:]; strings.HasPrefix(rest, ":") && isAllDigitsASCII(rest[1:]) {
			return host, rest[1:]
		}
		return host, "443"
	}

	host, port := dest, "443"
	if i := strings.LastIndex(dest, ":"); i >= 0 && isAllDigitsASCII(dest[i+1:]) {
		host, port = dest[:i], dest[i+1:]
	}
	host = strings.ToLower(strings.Trim(host, " \t\n\r\x00\x0B."))
	return host, port
}

// fetchCertDNSNames 对 host:port 做一次 TLS 握手，取叶子证书的 DNS SAN。
//
// 不校验信任链：dest 是管理员自选的伪装站点，我们只关心它「出示了哪些名字」，
// 关心信任与否反而会让自签站点直接失去依据。
func fetchCertDNSNames(host, port string) ([]string, bool) {
	dialer := &net.Dialer{Timeout: certProbeTimeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, port), &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: true, // 只取证书内容，不做信任判定（见上）
		MinVersion:         tls.VersionTLS12,
	})
	if err != nil {
		return nil, false
	}
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return nil, false
	}
	return sanitizeCertNames(certs[0].DNSNames)
}

// sanitizeCertNames 过滤空串 / IP / 非法主机名，统一小写并去重。
// 通配符只保留最左标签恰好为 * 的形式（*.example.com）；带 * 的其它写法丢弃。
func sanitizeCertNames(raw []string) ([]string, bool) {
	out := make([]string, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		name := normalizeCertName(item)
		if !validCertName(name) || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	// 一条 DNS SAN 都没有的证书（纯 IP 证书）不算可用依据 → 让调用方退回 SameSite。
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// normalizeCertName 小写并去掉首尾空白与结尾的点。
func normalizeCertName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.TrimSuffix(strings.Trim(name, " \t\n\r\x00\x0B"), ".")
}

// validCertName 判定一个已小写化的名字能否作为证书 SAN 使用：
// 至少两个标签、每段 1–63 字符且只能是 [a-z0-9-]、不以 - 开头/结尾；
// 通配符只允许最左标签恰为 *。
func validCertName(name string) bool {
	if name == "" || len(name) > 253 || isIPLiteral(name) {
		return false
	}
	labels := strings.Split(name, ".")
	if len(labels) < 2 {
		return false
	}
	for i, label := range labels {
		if i == 0 && label == "*" {
			continue
		}
		if label == "" || len(label) > 63 || strings.Contains(label, "*") {
			return false
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for j := 0; j < len(label); j++ {
			c := label[j]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}
