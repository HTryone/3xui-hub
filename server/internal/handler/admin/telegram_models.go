package admin

import (
	"context"
	"log"

	"controlhub-server/internal/httpx"
	"controlhub-server/internal/tgbot"

	"github.com/gin-gonic/gin"
)

// telegramConfigKeys 是 TG 配置页展示/保存的全部 site_configs key。
// token 加密存，其余明文；chat_id 是纯数字字符串。
var telegramConfigKeys = []string{
	tgbot.KeyToken, tgbot.KeyChatID, tgbot.KeyEnabled,
	tgbot.KeyNotifyNodeOff, tgbot.KeyNotifyPayment, tgbot.KeyBackupPush,
}

// TelegramStore 是 TG 配置的读写（复用 sqlStore 的 site_configs 方法）。
type TelegramStore interface {
	SiteConfigMany(ctx context.Context, keys []string) (map[string]string, error)
	SiteConfigSetMany(ctx context.Context, kv []KV) error
}

// TelegramDeps 是 Telegram 域的依赖清单。
type TelegramDeps struct {
	Store  TelegramStore
	AppKey []byte
	Logger loggerLike
}

// TelegramDomain 是 TG 机器人的 HTTP 入口（配置 + 测试发送）。
type TelegramDomain struct {
	store  TelegramStore
	appKey []byte
	logger loggerLike
}

// NewTelegramDomain 组装 Telegram 域。
func NewTelegramDomain(d TelegramDeps) *TelegramDomain {
	if d.Store == nil {
		panic("admin: TelegramStore 不能为空")
	}
	logger := d.Logger
	if logger == nil {
		logger = log.Default()
	}
	return &TelegramDomain{store: d.Store, appKey: d.AppKey, logger: logger}
}

// Register 挂路由（调用方负责 /admin-api 前缀与 admin.auth 中间件）。
func (d *TelegramDomain) Register(r gin.IRouter) {
	r.GET("/telegram", d.telegramShow)
	r.PUT("/telegram", d.telegramSave)
	r.POST("/telegram/test", d.telegramTest)
}

// fail / failServer 复用 Handler 的（TelegramDomain 不重复实现，直接内嵌最小封装）。
func (d *TelegramDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *TelegramDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

func (d *TelegramDomain) failValidation(c *gin.Context, summary string) {
	d.fail(c, summary, 500)
}

func (d *TelegramDomain) success(c *gin.Context, data any) {
	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data", data)))
}
