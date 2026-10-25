package subscription

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/gin-gonic/gin"
)

// 逐条复刻 app/Http/Controllers/Api/SubscriptionController.php 的 show()。
//
// 端点契约（GET /api/sub/{token}）：
//   - 用户不存在 → 正文 "[404] 用户不存在"，HTTP 200，X-CH-Code: 404
//   - 账号禁用   → "[403] 账号已禁用"
//   - 格式未启用 → "[400] 不支持的订阅格式: clash"
//   - 业务异常   → "[{code}] {message}"
//   - 成功       → 正文 + Content-Type + Subscription-Userinfo + X-CH-Code: 0
//
// **注意 HTTP 状态恒为 200**：PHP 的 helper 把 code 塞进 X-CH-Code 头，前端/客户端靠它判断。
// 这一点与 api 契约（response.go：失败也 200）一致，不要"改得更 RESTful"。

// Handler 是订阅端点的 HTTP 适配层。
type Handler struct {
	Service *Service
	Repo    Repository
}

// NewHandler 构造 handler。
func NewHandler(repo Repository) *Handler {
	svc := NewService(repo)
	return &Handler{Service: svc, Repo: repo}
}

// Register 把 GET /api/sub/:token 挂到路由器上。
func (h *Handler) Register(r gin.IRouter) {
	r.GET("/api/sub/:token", h.Show)
}

// Show 处理一次订阅请求。
func (h *Handler) Show(c *gin.Context) {
	ctx := c.Request.Context()
	token := c.Param("token")

	user, err := h.Repo.UserByToken(ctx, token)
	if err != nil {
		h.text(c, "服务器错误", 500, "服务器错误")
		return
	}
	if user == nil {
		h.text(c, "[404] 用户不存在", 404, "用户不存在")
		return
	}

	// 加载用户数据（流量由定时任务同步，不在每次请求时同步）
	// 检查封禁状态
	if !user.Enabled {
		h.text(c, "[403] 账号已禁用", 403, "账号已禁用")
		return
	}

	cfg, err := h.Service.loadSiteConfigs(ctx)
	if err != nil {
		h.text(c, "服务器错误", 500, "服务器错误")
		return
	}

	format := resolveFormat(c.Request.URL.Query())
	if !isFormatEnabled(format, cfg) {
		h.text(c, "[400] 不支持的订阅格式: "+string(format), 400, "不支持的订阅格式")
		return
	}

	body, genErr := h.Service.Generate(ctx, user, format)
	if genErr != nil {
		if bizErr, ok := genErr.(*Error); ok {
			h.text(c, "["+strconv.Itoa(bizErr.Code)+"] "+bizErr.Message, bizErr.Code, bizErr.Message)
			return
		}
		h.text(c, "服务器错误", 500, "服务器错误")
		return
	}

	// Subscription-Userinfo 头（upload 恒 0；download = traffic_used；total = traffic_limit）
	expire := int64(0)
	if user.ExpiredAt != nil {
		expire = user.ExpiredAt.Unix()
	}
	userinfo := "upload=0; download=" + itoa(user.TrafficUsed) +
		"; total=" + itoa(user.TrafficLimit) +
		"; expire=" + itoa(expire)

	contentType := "text/plain; charset=UTF-8"
	switch format {
	case FormatClash:
		contentType = "text/yaml; charset=UTF-8"
	case FormatSingbox:
		contentType = "application/json; charset=UTF-8"
	}

	// 订阅标题：管理员后台配置，留空保持原样（不加头、不改正文）
	title := phpTrim(cfg.get("sub_title", ""))
	if title != "" && format == FormatClash {
		// Clash 系客户端读取名称的约定：正文首行注释，值为 base64
		body = "#profile-title: " + b64encode([]byte(title)) + "\n" + body
	}

	c.Header("Content-Type", contentType)
	c.Header("Subscription-Userinfo", userinfo)
	c.Header("X-CH-Code", "0")
	c.Header("X-CH-Msg", "ok")
	if title != "" {
		c.Header("Content-Disposition", "attachment; filename*=UTF-8''"+rawurlencode(title))
	}
	c.String(http.StatusOK, body)
}

// text 对应 PHP 的 text()：正文 + text/plain + X-CH-Code/X-CH-Msg，HTTP 200。
func (h *Handler) text(c *gin.Context, body string, code int, msg string) {
	c.Header("Content-Type", "text/plain; charset=UTF-8")
	c.Header("X-CH-Code", strconv.Itoa(code))
	c.Header("X-CH-Msg", msg)
	c.String(http.StatusOK, body)
}

// resolveFormat 对应 resolveFormat()：`?clash`/`?clash=任意值` 优先，其次 singbox，默认 base64。
//
// PHP 用 `$request->has('clash')`，语义是「该键存在且值不为 null」（值为空串也算存在）；
// 键名大小写敏感 —— `?Clash=1` 在 PHP 里不命中，这里同样不命中。
func resolveFormat(query url.Values) Format {
	if _, ok := query["clash"]; ok {
		return FormatClash
	}
	if _, ok := query["singbox"]; ok {
		return FormatSingbox
	}
	return FormatBase64
}

// isFormatEnabled 对应 isFormatEnabled()：base64 始终启用。
func isFormatEnabled(format Format, cfg *siteConfigReader) bool {
	switch format {
	case FormatClash:
		return cfg.get("sub_clash_enabled", "0") == "1"
	case FormatSingbox:
		return cfg.get("sub_singbox_enabled", "0") == "1"
	default:
		return true
	}
}

// GenerateForToken 是 handler 之外（例如测试/内部调用）复用的一条便捷路径：
// 按 token 取用户并生成指定格式。
func (h *Handler) GenerateForToken(ctx context.Context, token string, format Format) (string, error) {
	user, err := h.Repo.UserByToken(ctx, token)
	if err != nil {
		return "", err
	}
	if user == nil {
		return "", &Error{Message: "用户不存在", Code: 404}
	}
	return h.Service.Generate(ctx, user, format)
}
