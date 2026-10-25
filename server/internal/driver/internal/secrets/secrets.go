// Package secrets 是驱动层共用的 Laravel 密文字段解密助手。
//
// 只做一件事：把「PHP 侧 Crypt::encryptString 落库的字符串」解成明文，并保留 PHP
// 两套不同的失败口径（驱动层必须逐字段对齐，否则要么静默用错凭据、要么整条链路报错）：
//
//   - 严格：Node 的 password / api_key accessor —— 解密失败抛 DecryptException，
//     上层据此判定节点不可用（backend/app/Models/Node.php:password/apiKey）。
//   - 宽松：XrayInbound::realityPrivateKey()、XrayOutbound::secretKey()、
//     NodeWarpAccount::*（access_token/private_key）—— catch 后**返回原始密文**，
//     兼容历史上手工写库的明文值。
//
// 以 `internal/driver/internal/` 放置，是为了只允许 driver 下的包引用，
// 不把「读 PHP 密文」这件事散布到其它模块（crypto 包本身保持纯粹的算法实现）。
package secrets

import (
	"errors"

	"controlhub-server/internal/crypto"
)

// ErrNoKey 表示 APP_KEY 未初始化（长度不是 32 字节）。
var ErrNoKey = errors.New("APP_KEY 未初始化（需要 32 字节 AES 密钥）")

// Decrypt 严格解密：空串原样返回空串；失败返回错误。
func Decrypt(cipherText string, key []byte) (string, error) {
	if cipherText == "" {
		return "", nil
	}
	if len(key) != 32 {
		return "", ErrNoKey
	}
	return crypto.DecryptString(cipherText, key)
}

// DecryptOrRaw 宽松解密：失败时返回原始字符串。
// 对齐 PHP 侧 `try { Crypt::decryptString(...) } catch (\Throwable) { return $raw; }`。
func DecryptOrRaw(cipherText string, key []byte) string {
	plain, err := Decrypt(cipherText, key)
	if err != nil {
		return cipherText
	}
	return plain
}

// DecryptPtrOrNil 是 DecryptOrRaw 的指针版：nil / 空串 → nil（对齐 PHP 的
// 「非字符串或空串 → null」的分支）。
func DecryptPtrOrNil(cipherText *string, key []byte) *string {
	if cipherText == nil || *cipherText == "" {
		return nil
	}
	plain := DecryptOrRaw(*cipherText, key)
	return &plain
}

// DecryptStrictPtr 是 Decrypt 的指针版：nil / 空串 → nil。
// Node::password / Node::api_key 的 accessor 在新值写入侧把空串存成 null，
// 读回时空值即「未配置」，不是错误。
func DecryptStrictPtr(cipherText *string, key []byte) (*string, error) {
	if cipherText == nil || *cipherText == "" {
		return nil, nil
	}
	plain, err := Decrypt(*cipherText, key)
	if err != nil {
		return nil, err
	}
	return &plain, nil
}
