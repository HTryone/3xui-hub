package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// 图形验证码要跨请求（GET /api/captcha 出图 → POST /api/register 校验），
// PHP 侧放在 session 里。Go 侧不读 Laravel 的 sessions 表（payload 是 PHP 序列化 +
// 加密的，且与 admin session 混用），改为签发一个**自包含、带 HMAC 的**验证码 cookie：
//
//	cookie value = base64url( JSON{"code": "...", "exp": unix} ) + "." + base64url(HMAC-SHA256)
//
// 优点：无状态、可多实例、不需要与 Laravel 共享 session 表；校验一次即失效（handler 同时清 cookie）。
// 键用 APP_KEY 派生（不是直接复用加密器，避免和 Crypt 的密文格式混淆）。
//
// ⚠️ 并存期的已知差异：PHP 版把验证码放 session，前端拿到 captcha 图后由同一个浏览器会话提交；
// Go 版用 cookie。两者对前端都不可见（axios 带 withCredentials，同源 cookie 自动往返），
// 但只要请求跨了 PHP/Go 两个后端（灰度分流），验证码就会对不上 —— 灰度期间 captcha/register
// 必须整组路由到同一后端（运维层约束，已记入交付风险）。

const (
	captchaCookieName = "ch_captcha"
	captchaTTLSeconds = 300 // 5 分钟，与验证码邮件的有效期同量级
)

// captchaSecret 由 APP_KEY 派生出验证码 cookie 的签名键。
func captchaSecret(appKey []byte) []byte {
	m := hmac.New(sha256.New, appKey)
	m.Write([]byte("controlhub/captcha/v1"))
	return m.Sum(nil)
}

// signCaptcha 生成 cookie 值。code 已按 PHP 的 captcha 字符集大写，比较时再 tolower。
func signCaptcha(secret []byte, code string, exp int64) string {
	payload, _ := json.Marshal(struct {
		Code string `json:"code"`
		Exp  int64  `json:"exp"`
	}{Code: code, Exp: exp})
	enc := payload
	mac := hmac.New(sha256.New, secret)
	mac.Write(enc)
	return base64.RawURLEncoding.EncodeToString(enc) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// verifyCaptcha 校验 cookie 值，返回验证码明文与是否有效。
func verifyCaptcha(secret []byte, value string, now int64) (string, bool) {
	parts := strings.SplitN(value, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	enc, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(enc)
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return "", false
	}
	var payload struct {
		Code string `json:"code"`
		Exp  int64  `json:"exp"`
	}
	if err := json.Unmarshal(enc, &payload); err != nil {
		return "", false
	}
	if payload.Exp < now || payload.Code == "" {
		return "", false
	}
	return payload.Code, true
}

// clearCaptchaCookie 让浏览器丢弃验证码 cookie（路径与 MaxAge=0 删除）。
func clearCaptchaCookie(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     captchaCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// randomBytes 生成 n 字节密码学随机数；失败直接 panic（熵源坏了继续跑比停更危险）。
func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("api: 随机数生成失败: " + err.Error())
	}
	return b
}
