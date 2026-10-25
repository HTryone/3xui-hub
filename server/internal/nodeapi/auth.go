package nodeapi

import (
	"context"
	"crypto/subtle"
	"fmt"

	"controlhub-server/internal/crypto"
)

// 节点鉴权：逐行对齐 backend/app/Http/Middleware/NodeAuth.php。
//
//	X-Node-Secret: <32B 明文密钥>
//	→ 遍历 driver_type='xray' 节点，解密 driver_config.node_secret
//	→ 常数时间比较（hash_equals）
//	失败：http 401 + {"code":401,"msg":"missing node secret"|"invalid node secret"}
//
// 注意：比对的是**明文**（PHP hash_equals($candidate, $secret) 两边都是明文），
// 不是摘要 —— nodehub 参考项目在这里先做 sha256 摘要比较，语义等价但会在
// 「节点密钥含非法 UTF-8/长度差异」时产生不同的失败路径，这里按 PHP 原样复刻。

// ErrMissingSecret / ErrInvalidSecret 对应 NodeAuth 的两种失败。
var (
	ErrMissingSecret = fmt.Errorf("missing node secret")
	ErrInvalidSecret = fmt.Errorf("invalid node secret")
)

// Authenticate 校验密钥并返回节点。secret 为空 → ErrMissingSecret。
func (r *Repo) Authenticate(ctx context.Context, secret string, key []byte) (Node, error) {
	if secret == "" {
		return Node{}, ErrMissingSecret
	}

	nodes, err := r.XrayNodes(ctx)
	if err != nil {
		return Node{}, err
	}
	if node, ok := matchNodeSecret(nodes, secret, key); ok {
		return node, nil
	}
	return Node{}, ErrInvalidSecret
}

// matchNodeSecret 是鉴权的纯函数部分（不碰数据库）：
// 遍历候选节点，解密 driver_config.node_secret 后做常数时间比较。
//
// 抽出成纯函数是为了在无数据库的测试里覆盖「真实 PHP 密文 → Go 解密 → 比对」
// 这条完整链路（node_secret 解密鉴权测试）。
func matchNodeSecret(nodes []Node, secret string, key []byte) (Node, bool) {
	if secret == "" {
		return Node{}, false
	}
	for _, n := range nodes {
		candidate := decryptNodeSecret(n.DriverConfig, key)
		if candidate == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(secret)) == 1 {
			return n, true
		}
	}
	return Node{}, false
}

// decryptNodeSecret 复刻 Node::nodeSecret()：
// driver_config.node_secret 是 Crypt::encryptString 的密文，解密失败/缺失返回空串。
func decryptNodeSecret(driverConfig []byte, key []byte) string {
	if len(driverConfig) == 0 {
		return ""
	}
	cfg, err := ParseObject(driverConfig)
	if err != nil {
		return ""
	}
	raw, ok := cfg.Get("node_secret")
	if !ok {
		return ""
	}
	ciphertext := stringOf(raw)
	if ciphertext == "" {
		return ""
	}
	plain, err := crypto.DecryptString(ciphertext, key)
	if err != nil {
		return ""
	}
	return plain
}
