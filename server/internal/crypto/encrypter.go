// Package crypto 复刻 Laravel Illuminate\Encryption\Encrypter 的 AES-256-CBC 加密，
// 使 Go 后端能直接解密 PHP 侧已加密入库的字段（Node.password/api_key/node_secret）。
//
// Laravel 密文结构（base64 包裹整个 JSON）：
//
//	base64_encode(json_encode({
//	    "iv":    base64(iv),              // 16 字节随机 IV
//	    "value": base64(AES-256-CBC(明文, PKCS#7)),
//	    "mac":   hex(HMAC-SHA256(key, iv_b64 + value_b64)),
//	    "tag":   ""                        // CBC 无 AEAD，恒为空
//	}))
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// ParseAppKey 解析 .env 的 APP_KEY（形如 base64:xxxx=），返回 32 字节 AES 密钥。
func ParseAppKey(env string) ([]byte, error) {
	if !strings.HasPrefix(env, "base64:") {
		return nil, errors.New("APP_KEY 必须以 base64: 开头")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(env, "base64:"))
	if err != nil {
		return nil, fmt.Errorf("APP_KEY 解码失败: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("AES-256-CBC 需要 32 字节密钥，实际 %d 字节", len(key))
	}
	return key, nil
}

// payload 与 Laravel 的 compact('iv','value','mac','tag') 结构对齐。
type payload struct {
	Iv    string `json:"iv"`
	Value string `json:"value"`
	Mac   string `json:"mac"`
	Tag   string `json:"tag"`
}

// macHex 计算 HMAC-SHA256(key, iv_b64 + value_b64) 的小写 hex 串。
func macHex(key []byte, ivB64, valueB64 string) string {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(ivB64 + valueB64))
	return hex.EncodeToString(h.Sum(nil))
}

// EncryptString 等价于 Laravel Crypt::encryptString($value)。
func EncryptString(plaintext string, key []byte) (string, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("生成 IV 失败: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	padded := pkcs7Pad([]byte(plaintext), aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)

	ivB64 := base64.StdEncoding.EncodeToString(iv)
	valueB64 := base64.StdEncoding.EncodeToString(ciphertext)

	p := payload{
		Iv:    ivB64,
		Value: valueB64,
		Mac:   macHex(key, ivB64, valueB64),
		Tag:   "",
	}

	// 与 Laravel JSON_UNESCAPED_SLASHES 对齐：不转义 HTML 字符、不转义斜杠。
	jsonBytes, err := jsonMarshal(p)
	if err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(jsonBytes), nil
}

// DecryptString 等价于 Laravel Crypt::decryptString($payload)。
// 返回明文；MAC 校验失败或格式非法返回错误。
func DecryptString(payloadStr string, key []byte) (string, error) {
	jsonBytes, err := base64.StdEncoding.DecodeString(payloadStr)
	if err != nil {
		return "", fmt.Errorf("payload base64 解码失败: %w", err)
	}

	var p payload
	if err := json.Unmarshal(jsonBytes, &p); err != nil {
		return "", fmt.Errorf("payload JSON 解析失败: %w", err)
	}
	if p.Iv == "" || p.Value == "" || p.Mac == "" {
		return "", errors.New("payload 缺少 iv/value/mac")
	}

	// MAC 校验（hash_equals 防时序侧信道）
	expected := macHex(key, p.Iv, p.Value)
	if !hmac.Equal([]byte(expected), []byte(p.Mac)) {
		return "", errors.New("MAC 校验失败（APP_KEY 不匹配或数据被篡改）")
	}

	iv, err := base64.StdEncoding.DecodeString(p.Iv)
	if err != nil {
		return "", fmt.Errorf("iv 解码失败: %w", err)
	}
	if len(iv) != aes.BlockSize {
		return "", fmt.Errorf("iv 长度错误：期望 %d 字节，实际 %d", aes.BlockSize, len(iv))
	}

	ciphertext, err := base64.StdEncoding.DecodeString(p.Value)
	if err != nil {
		return "", fmt.Errorf("value 解码失败: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	plaintext := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plaintext, ciphertext)

	unpadded, err := pkcs7Unpad(plaintext)
	if err != nil {
		return "", err
	}

	return string(unpadded), nil
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padLen := blockSize - len(data)%blockSize
	padding := make([]byte, padLen)
	for i := range padding {
		padding[i] = byte(padLen)
	}
	return append(data, padding...)
}

func pkcs7Unpad(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("空密文无法去填充")
	}
	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > len(data) || padLen > aes.BlockSize {
		return nil, errors.New("PKCS#7 填充非法")
	}
	for _, b := range data[len(data)-padLen:] {
		if int(b) != padLen {
			return nil, errors.New("PKCS#7 填充校验失败")
		}
	}
	return data[:len(data)-padLen], nil
}

// jsonMarshal 与 Laravel JSON_UNESCAPED_SLASHES 对齐的 JSON 序列化。
func jsonMarshal(v any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	// Encoder 会在末尾追加换行，去掉。
	return []byte(strings.TrimRight(buf.String(), "\n")), nil
}
