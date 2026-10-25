package admin

// admin_auth_routes.go 是管理端认证路由的 handler（login/logout/settings/改密/改用户名/2FA）。
// 逐条对齐 backend/app/Http/Controllers/Admin/AuthController.php。
//
// login 在 AdminAuth 中间件之外（登录前无需 session）；其余在组内。

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// AdminAuthRoutes 持有管理端认证 handler 的依赖。
type AdminAuthRoutes struct {
	app *httpx.App
	db  *sql.DB
	now func() time.Time
}

// NewAdminAuthRoutes 构造管理端认证 handler。
func NewAdminAuthRoutes(app *httpx.App, db *sql.DB) *AdminAuthRoutes {
	return &AdminAuthRoutes{app: app, db: db, now: time.Now}
}

// RegisterLogin 挂 POST /admin-api/login（无需 AdminAuth 中间件）。
func (h *AdminAuthRoutes) RegisterLogin(r gin.IRouter) {
	r.POST("/admin-api/login", h.Login)
}

// RegisterAuthed 挂需要 session 的认证路由（在 admin-api 分组内）。
func (h *AdminAuthRoutes) RegisterAuthed(r gin.IRouter) {
	r.POST("/logout", h.Logout)
	r.GET("/settings", h.Settings)
	r.POST("/change-password", h.ChangePassword)
	r.POST("/update-username", h.UpdateUsername)
	r.POST("/google2fa/generate", h.Google2faGenerate)
	r.POST("/google2fa/enable", h.Google2faEnable)
	r.POST("/google2fa/disable", h.Google2faDisable)
}

// Login 对齐 AuthController::login。
func (h *AdminAuthRoutes) Login(c *gin.Context) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
		OTP      string `json:"otp"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "参数错误", 400)
		return
	}

	var (
		id               uint64
		username         string
		passwordHash     string
		google2faEnabled bool
		google2faSecret  sql.NullString
	)
	err := h.db.QueryRowContext(c.Request.Context(),
		"SELECT id, username, password, google2fa_enabled, google2fa_secret FROM admins WHERE username = ?",
		body.Username).Scan(&id, &username, &passwordHash, &google2faEnabled, &google2faSecret)
	if err == sql.ErrNoRows {
		h.fail(c, "账号或密码错误", 401)
		return
	}
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.Password)) != nil {
		h.fail(c, "账号或密码错误", 401)
		return
	}

	if google2faEnabled && google2faSecret.Valid && google2faSecret.String != "" {
		if body.OTP == "" {
			h.fail(c, "请输入验证码", 401)
			return
		}
		if !verifyTOTP(google2faSecret.String, body.OTP, h.now()) {
			h.fail(c, "验证码错误", 401)
			return
		}
	}

	h.app.SetAdminSession(c, id)
	writeJSON(c, phpEncode(jo("code", 0, "msg", "登录成功", "data",
		jo("admin", jo("id", id, "username", username)))))
}

// Logout 对齐 AuthController::logout。
func (h *AdminAuthRoutes) Logout(c *gin.Context) {
	h.app.ClearAdminSession(c)
	writeJSON(c, phpEncode(jo("code", 0, "msg", "已退出", "data", nil)))
}

// Settings 对齐 AuthController::settings。
func (h *AdminAuthRoutes) Settings(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var google2faEnabled bool
	_ = h.db.QueryRowContext(c.Request.Context(),
		"SELECT google2fa_enabled FROM admins WHERE id = ?", admin.ID).Scan(&google2faEnabled)

	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data",
		jo("id", admin.ID, "username", admin.Username, "google2fa_enabled", google2faEnabled))))
}

// ChangePassword 对齐 AuthController::changePassword。
func (h *AdminAuthRoutes) ChangePassword(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var body struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "old_password 字段不能为空。,new_password 字段不能为空。", 500)
		return
	}
	if len(body.NewPassword) < 6 || len(body.NewPassword) > 64 {
		h.fail(c, "new_password 长度必须介于 6 - 64 个字符之间。", 500)
		return
	}

	var passwordHash string
	err := h.db.QueryRowContext(c.Request.Context(),
		"SELECT password FROM admins WHERE id = ?", admin.ID).Scan(&passwordHash)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.OldPassword)) != nil {
		h.fail(c, "原密码错误", 400)
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		"UPDATE admins SET password = ? WHERE id = ?", string(newHash), admin.ID)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "密码已修改", "data", nil)))
}

// UpdateUsername 对齐 AuthController::updateUsername。
func (h *AdminAuthRoutes) UpdateUsername(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "username 字段不能为空。,password 字段不能为空。", 500)
		return
	}
	if len(body.Username) < 3 || len(body.Username) > 32 {
		h.fail(c, "username 长度必须介于 3 - 32 个字符之间。", 500)
		return
	}

	var passwordHash string
	err := h.db.QueryRowContext(c.Request.Context(),
		"SELECT password FROM admins WHERE id = ?", admin.ID).Scan(&passwordHash)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.Password)) != nil {
		h.fail(c, "密码错误", 400)
		return
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		"UPDATE admins SET username = ? WHERE id = ?", body.Username, admin.ID)
	if err != nil {
		h.fail(c, "用户名已被使用", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "用户名已修改", "data", nil)))
}

// Google2faGenerate 对齐 AuthController::google2faGenerate。
func (h *AdminAuthRoutes) Google2faGenerate(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	secret := generateBase32Secret()
	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data", jo("secret", secret))))
}

// Google2faEnable 对齐 AuthController::google2faEnable。
func (h *AdminAuthRoutes) Google2faEnable(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var body struct {
		Secret string `json:"secret"`
		OTP    string `json:"otp"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "otp 字段不能为空。", 500)
		return
	}

	if body.Secret == "" {
		h.fail(c, "请先生成密钥", 400)
		return
	}

	if !verifyTOTP(body.Secret, body.OTP, h.now()) {
		h.fail(c, "验证码错误", 400)
		return
	}

	_, err := h.db.ExecContext(c.Request.Context(),
		"UPDATE admins SET google2fa_enabled = 1, google2fa_secret = ? WHERE id = ?",
		body.Secret, admin.ID)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "两步验证已启用", "data", nil)))
}

// Google2faDisable 对齐 AuthController::google2faDisable。
func (h *AdminAuthRoutes) Google2faDisable(c *gin.Context) {
	admin := httpx.AdminFromContext(c)
	if admin == nil {
		h.fail(c, "未登录", 401)
		return
	}

	var body struct {
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		h.fail(c, "password 字段不能为空。", 500)
		return
	}

	var passwordHash string
	err := h.db.QueryRowContext(c.Request.Context(),
		"SELECT password FROM admins WHERE id = ?", admin.ID).Scan(&passwordHash)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(body.Password)) != nil {
		h.fail(c, "密码错误", 400)
		return
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		"UPDATE admins SET google2fa_enabled = 0, google2fa_secret = NULL WHERE id = ?", admin.ID)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "两步验证已关闭", "data", nil)))
}

// fail 输出 PHP ApiResponse::error 口径。
func (h *AdminAuthRoutes) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

// ---- TOTP 工具 ----

// verifyTOTP 验证 TOTP 验证码（RFC 6238，30 秒窗口，±1 容差）。
func verifyTOTP(secret, code string, now time.Time) bool {
	if len(code) != 6 {
		return false
	}
	secret = strings.ToUpper(strings.ReplaceAll(secret, " ", ""))
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil {
		return false
	}

	timestamp := now.Unix()
	for _, offset := range []int64{-1, 0, 1} {
		counter := (timestamp + offset*30) / 30
		if totpCode(key, counter) == code {
			return true
		}
	}
	return false
}

// totpCode 生成 TOTP 验证码（HMAC-SHA1，6 位）。
func totpCode(key []byte, counter int64) string {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf)
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0F
	code := (int(sum[offset])&0x7F)<<24 |
		(int(sum[offset+1])&0xFF)<<16 |
		(int(sum[offset+2])&0xFF)<<8 |
		(int(sum[offset+3]) & 0xFF)
	return fmt.Sprintf("%06d", code%1000000)
}

// generateBase32Secret 生成 20 字节的 base32 密钥。
func generateBase32Secret() string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)
}
