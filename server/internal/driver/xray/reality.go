package xray

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// 逐条对齐 backend/app/Drivers/Xray/RealityKeyService.php。
//
// 生成格式与 `xray x25519` 输出完全兼容（base64url 无填充）：X25519 keypair，
// 私钥按 xray 惯例 clamp 后存储。兼容性已与 xray v26.3.27 对拍验证：
// PHP 生成的私钥经 `xray x25519 -i` 推出的公钥与 PHP 侧计算完全一致 ——
// 因此 Go 侧只要遵循同一套 clamp + base64url 口径，两边私钥可互换。

// 默认 Reality dest / SNI。
//
// dest 不能用 TLS 记录 >8192 字节的站（P1 真机踩坑记录）。
const (
	DefaultDest = "www.cloudflare.com:443"
	DefaultSNI  = "www.cloudflare.com"

	// Flow 是 VLESS+Reality 的传输流控（v26 校验要求该值，旧写法 xtls-rpra-vision 校验不过）。
	Flow = "xtls-rprx-vision"
)

// GenerateKeypair 对应 RealityKeyService::generateKeypair()。
//
// 返回 [privateKey, publicKey]，均为 base64url 无填充。
// 私钥按 X25519 标量钳位（幂等；公钥配对不变），与 xray x25519 的落盘形态一致。
func GenerateKeypair() ([2]string, error) {
	var sk [32]byte
	if _, err := rand.Read(sk[:]); err != nil {
		return [2]string{}, err
	}
	return keypairFromSecret(sk)
}

// GenerateKeypairFrom 用给定的 32 字节种子生成密钥对（测试可复现）。
func GenerateKeypairFrom(seed [32]byte) ([2]string, error) {
	return keypairFromSecret(seed)
}

func keypairFromSecret(raw [32]byte) ([2]string, error) {
	sk := clampScalar(raw)
	pk, err := curve25519.X25519(sk[:], curve25519.Basepoint)
	if err != nil {
		return [2]string{}, err
	}
	var pkArr [32]byte
	copy(pkArr[:], pk)
	return [2]string{Base64URL(sk[:]), Base64URL(pkArr[:])}, nil
}

// clampScalar 复刻 PHP 侧的手工钳位：
//
//	$sk[0]  = $sk[0]  & 248
//	$sk[31] = ($sk[31] & 127) | 64
//
// curve25519.X25519 内部会再 clamp 一次（幂等），这里手工做是为了让**存储形态**
// 与 xray/PHP 完全一致（私钥文件内容逐字节相同）。
func clampScalar(sk [32]byte) [32]byte {
	out := sk
	out[0] &= 248
	out[31] = (out[31] & 127) | 64
	return out
}

// ShortID 对应 shortId()：随机 hex 串（默认 4 字节 → 8 个 hex 字符），xray reality shortIds 格式。
func ShortID(bytes int) (string, error) {
	if bytes <= 0 {
		bytes = 4
	}
	buf := make([]byte, bytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// PublicKeyFromPrivateKey 对应 publicKeyFromPrivateKey()：
// 从既有私钥推导公钥（base64url 无填充；解析失败返回空串）。
//
// 用于对接只存 privateKey、不存 publicKey 的面板数据
// （如 3x-ui v3.10 的 reality 入站 realitySettings）。
func PublicKeyFromPrivateKey(privateKey string) string {
	raw, ok := decodeBase64URL(strings.TrimSpace(privateKey))
	if !ok || len(raw) != 32 {
		return ""
	}
	pk, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return ""
	}
	return Base64URL(pk)
}

// Base64URL 对应 base64url()：base64 的 URL 安全变体、去掉尾部填充。
func Base64URL(bin []byte) string {
	return base64.RawURLEncoding.EncodeToString(bin)
}

// decodeBase64URL 复刻 PHP 的
//
//	base64_decode(strtr($s, '-_', '+/') . padding, true)
//
// 即「URL 安全字符转回标准字符 + 补齐 padding + 严格解码」，失败返回 false。
func decodeBase64URL(s string) ([]byte, bool) {
	standard := strings.NewReplacer("-", "+", "_", "/").Replace(s)
	if pad := (4 - len(standard)%4) % 4; pad > 0 {
		standard += strings.Repeat("=", pad)
	}
	raw, err := base64.StdEncoding.DecodeString(standard)
	if err != nil {
		return nil, false
	}
	return raw, true
}

// WireGuardKeypair 对应 WarpService::generateWireguardKeypair()。
//
// 与 Reality 的区别：标准 base64（带填充、+/ 字符集），私钥同样 clamp。
func WireGuardKeypair() ([2]string, error) {
	var sk [32]byte
	if _, err := rand.Read(sk[:]); err != nil {
		return [2]string{}, err
	}
	clamped := clampScalar(sk)
	pk, err := curve25519.X25519(clamped[:], curve25519.Basepoint)
	if err != nil {
		return [2]string{}, err
	}
	return [2]string{
		base64.StdEncoding.EncodeToString(clamped[:]),
		base64.StdEncoding.EncodeToString(pk),
	}, nil
}
