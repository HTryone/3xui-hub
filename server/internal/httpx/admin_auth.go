package httpx

// 管理端 session 鉴权（P8d）。
//
// 对齐 backend/app/Http/Middleware/AdminAuth.php + Controllers/Admin/AuthController 的登录段：
//
//	session('admin_id') → Admin::find → 注入 $request->user()
//	缺失 / 查不到 → session 清键 + {"code":401,"msg":"未登录","data":null}（HTTP 200）
//
// 会话方案（MIGRATION.md「关键决策点 2」已拍板）：**独立 session**。Go 不读 Laravel 的
// sessions 表（payload 是 PHP 序列化/JSON + cookie 前缀加密，格式与 Go 侧不同），
// 切 Go 后管理员重新登录一次即可。因此这里用 App 自带的内存 SessionStore，
// 并用独立的 cookie 名（ch_admin_session），与 PHP 的 laravel-session 互不干扰 ——
// 灰度期两个后端指向同一浏览器时不会互相踩踏会话。
//
// 已知边界（交付风险，不是实现缺陷）：
//   - 内存 session 随进程重启丢失（需重新登录）、不跨实例共享；多实例部署需换 Redis/DB 后端；
//   - 不做会话滑动续期（Laravel 每次响应刷新过期时间），前端在 lifetime 内活跃即可用。

import (
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	// AdminSessionCookie 是 Go 管理端会话 cookie 名（与 PHP 的 laravel-session 区分）。
	AdminSessionCookie = "ch_admin_session"

	// ctxAdminKey 是中间件注入管理员时用的 gin.Context 键。
	ctxAdminKey = "httpx.admin"
)

// AdminIdentity 是会话中保存的管理员身份（只取展示与鉴权需要的字段）。
//
// 密码哈希故意不放进上下文：中间件只需要「这个 session 属于哪个管理员」，
// 需要校验密码的端点（改密/改用户名/关 2FA）自己按 ID 重新查库。
type AdminIdentity struct {
	ID       uint64
	Username string
}

// SetAdminSession 为管理员签发一个新的会话：写 SessionStore + 下发 cookie。
//
// MaxAge 取 config/session.php 的 SESSION_LIFETIME（分钟，默认 120），与 PHP 的
// Set-Cookie: Max-Age=7200 对齐；HttpOnly + SameSite=Lax 与 PHP 的默认一致。
func (a *App) SetAdminSession(c *gin.Context, adminID uint64) string {
	id := NewSessionID()
	a.session.Set(id, adminID)

	maxAge := 120 * 60
	if a.Cfg != nil && a.Cfg.Session.Lifetime > 0 {
		maxAge = int(a.Cfg.Session.Lifetime / time.Second)
	}
	secure := c.Request.TLS != nil
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     AdminSessionCookie,
		Value:    id,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
	return id
}

// ClearAdminSession 注销会话：删存储 + 让浏览器丢弃 cookie（MaxAge=-1）。
func (a *App) ClearAdminSession(c *gin.Context) {
	if id, err := c.Cookie(AdminSessionCookie); err == nil && id != "" {
		a.session.Del(id)
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     AdminSessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// AdminAuth 对齐 Middleware/AdminAuth.php：cookie → session store → 查 admins → 注入。
//
// 失败语义与 PHP 逐字一致（HTTP status 200，前端只看 code）：
//   - 没有 cookie / 会话不存在 ………「未登录」401
//   - admins 里查不到该 id ………清会话 +「未登录」401（PHP 同样 forget）
//   - 查库出错 ………这里回「服务器错误」500 并记日志；PHP 会把 PDO 异常文案透出，
//     故意不复刻（把 SQL 细节回给前端属于信息泄露），文案差异见交付风险。
func (a *App) AdminAuth(logf func(string, ...any)) gin.HandlerFunc {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return func(c *gin.Context) {
		id, err := c.Cookie(AdminSessionCookie)
		if err != nil || id == "" {
			adminFail(c, "未登录", 401)
			c.Abort()
			return
		}
		adminID, ok := a.session.Get(id)
		if !ok {
			adminFail(c, "未登录", 401)
			c.Abort()
			return
		}

		var (
			username string
			got      uint64
		)
		err = a.DB.QueryRowContext(c.Request.Context(),
			"SELECT id, username FROM admins WHERE id = ?", adminID).Scan(&got, &username)
		if err == sql.ErrNoRows {
			a.session.Del(id)
			adminFail(c, "未登录", 401)
			c.Abort()
			return
		}
		if err != nil {
			logf("admin.auth 查询 admins 失败: %v", err)
			adminFail(c, "服务器错误", 500)
			c.Abort()
			return
		}

		c.Set(ctxAdminKey, &AdminIdentity{ID: got, Username: username})
		c.Next()
	}
}

// AdminFromContext 取中间件注入的管理员（鉴权组内必然非 nil）。
func AdminFromContext(c *gin.Context) *AdminIdentity {
	if v, ok := c.Get(ctxAdminKey); ok {
		if a, ok := v.(*AdminIdentity); ok {
			return a
		}
	}
	return nil
}

// adminFail 输出 PHP json_encode 口径的失败体。
//
// 为什么不用本包的 Error()：httpx.Error 走 gin.H + encoding/json（中文原样输出、
// 转义 <>&），而 PHP 输出 \uXXXX、不转义 <>&。管理端要能跟 PHP 逐字节对拍，
// 所以这里复用最小编码器（只覆盖 status 里用到的类型）。
func adminFail(c *gin.Context, msg string, code int) {
	c.Data(http.StatusOK, "application/json; charset=utf-8",
		adminPHPEncode(map[string]any{"code": code, "msg": msg, "data": nil},
			[]string{"code", "msg", "data"}))
}

// adminPHPEncode 按给定键序输出 PHP json_encode 默认选项的字节。
// 键序固定（PHP 关联数组保序），因此调用方显式传 keys。
func adminPHPEncode(obj map[string]any, keys []string) []byte {
	buf := make([]byte, 0, 96)
	buf = append(buf, '{')
	for i, k := range keys {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = appendAdminPHPString(buf, k)
		buf = append(buf, ':')
		buf = appendAdminPHPValue(buf, obj[k])
	}
	return append(buf, '}')
}

func appendAdminPHPValue(dst []byte, v any) []byte {
	switch t := v.(type) {
	case nil:
		return append(dst, "null"...)
	case string:
		return appendAdminPHPString(dst, t)
	case int:
		return strconv.AppendInt(dst, int64(t), 10)
	case bool:
		if t {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	}
	return append(dst, "null"...)
}

// appendAdminPHPString 复刻 PHP json_encode 的字符串转义：\" \\ \/ 控制字符，
// 非 ASCII 一律 \uXXXX（BMP 外用 UTF-16 代理对）。
func appendAdminPHPString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	for _, r := range s {
		switch r {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '/':
			dst = append(dst, '\\', '/')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		default:
			switch {
			case r < 0x20:
				dst = append(dst, '\\', 'u', '0', '0', adminHex(byte(r)>>4), adminHex(byte(r)&0xF))
			case r < 0x80:
				dst = append(dst, byte(r))
			case r <= 0xFFFF:
				dst = appendAdminU16(dst, uint16(r))
			default:
				v := r - 0x10000
				dst = appendAdminU16(dst, uint16(0xD800+(v>>10)))
				dst = appendAdminU16(dst, uint16(0xDC00+(v&0x3FF)))
			}
		}
	}
	return append(dst, '"')
}

func appendAdminU16(dst []byte, v uint16) []byte {
	return append(dst, '\\', 'u',
		adminHex(byte(v>>12)&0xF), adminHex(byte(v>>8)&0xF),
		adminHex(byte(v>>4)&0xF), adminHex(byte(v)&0xF))
}

func adminHex(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}
