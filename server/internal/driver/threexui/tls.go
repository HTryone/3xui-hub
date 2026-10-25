package threexui

import "crypto/tls"

// insecureTLSConfig 对应 PHP/Guzzle 的 `verify => false`：
// 接受自签证书与不完整证书链（dev 联调 / 面板用自签证书的现场）。
//
// 单独成文件是为了让「跳过校验」这件事只在驱动层出现一次，
// 便于审计（生产建议 nodes.verify_ssl=1）。
func insecureTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 与 PHP 现状一致，由 nodes.verify_ssl 控制
}
