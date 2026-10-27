package api

import (
	"github.com/gin-gonic/gin"
)

// appVersionConfigKeys 是 GET /api/app-version 返回的 4 个站点配置键。
// 值全部是字符串（site_configs 是 key-value 表）。
var appVersionConfigKeys = []string{
	"app_latest_version_name", "app_update_notes",
	"app_download_url", "app_force_update",
}

// handleAppVersion 返回「App 版本」页配置的 4 项纯数据（版本号/更新说明/下载地址/强制开关）。
// 不做版本比较、不算 action：由 App 端拉取后在本地拿「最新版本名」与自身版本对比，
// 自行决定是否弹更新（未配置→不弹；已最新→不弹；开关开→强制；否则→可选）。
func (h *Handler) handleAppVersion(c *gin.Context) {
	ctx := c.Request.Context()

	values, err := h.store.SiteConfigMany(ctx, appVersionConfigKeys)
	if err != nil {
		h.logger.Printf("app-version 查询站点配置失败: %v", err)
		fail(c, "服务器错误", 500)
		return
	}

	success(c, jo(
		"latest_version_name", values["app_latest_version_name"],
		"download_url", values["app_download_url"],
		"notes", values["app_update_notes"],
		"force_update", values["app_force_update"],
	))
}
