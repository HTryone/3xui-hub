package api

import (
	"database/sql"
	"strconv"

	"github.com/gin-gonic/gin"
)

// 教程两个公开路由（routes/api.php 的闭包）。
//
// 输出形态与 ApiResponse 不同：它们是手写 response()->json()，
//   - 列表：{"code":0,"data":[...]}（**没有 msg**）
//   - 详情：{"code":0,"data":{...}}；不存在或未启用 → HTTP 404 + {"code":404,"msg":"Not found"}
// 字段口径：
//   - 列表：id, title, category（COALESCE(NULLIF(TRIM(category), ''), '默认')）, content, created_at
//   - 详情：id, title, content, created_at（$tutorial->only([...])，无 category）
//   - created_at 走 Eloquent 的 toJSON：2026-07-01T15:42:57.000000Z

// handleTutorials 对齐 GET /api/tutorials。
func (h *Handler) handleTutorials(c *gin.Context) {
	list, err := h.store.EnabledTutorials(c.Request.Context())
	if err != nil {
		h.logger.Printf("tutorials 查询失败: %v", err)
		// 与教程路由的兜底一致：异常也返回 code 非 0 的 JSON。
		fail(c, "服务器错误", 500)
		return
	}

	items := make(jsArr, 0, len(list))
	for _, t := range list {
		items = append(items, jo(
			"id", t.ID,
			"title", t.Title,
			"category", t.Category,
			"content", t.Content,
			"created_at", nullableEloquentTime(t.CreatedAt),
		))
	}
	successNoMsg(c, items)
}

// handleTutorialDetail 对齐 GET /api/tutorials/{tutorial}。
//
// 404 的三种来源（真实 PHP 探针确认）：
//   - {id} 不是数字 → 路由模型绑定直接失败 → NotFoundHttpException → 「接口不存在」
//   - 数字但记录不存在 → 隐式绑定失败（ModelNotFoundException）→ 同样被渲染成「接口不存在」
//   - 记录存在但 enabled=false → 路由闭包里的 json(['code'=>404,'msg'=>'Not found'], 404)
//
// 前两种在 PHP 里无法区分，所以文案必须一致；第三种才是「Not found」。
func (h *Handler) handleTutorialDetail(c *gin.Context) {
	raw := c.Param("id")
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		failStatus(c, notFoundRouteMsg, 404, 404)
		return
	}

	t, err := h.store.TutorialByID(c.Request.Context(), id)
	if err != nil {
		if IsNoRows(err) {
			failStatus(c, notFoundRouteMsg, 404, 404)
			return
		}
		h.logger.Printf("tutorial 查询失败 id=%s: %v", raw, err)
		fail(c, "服务器错误", 500)
		return
	}
	if t == nil {
		failStatus(c, notFoundRouteMsg, 404, 404)
		return
	}
	if !t.Enabled {
		failStatus(c, "Not found", 404, 404)
		return
	}

	successNoMsg(c, jo(
		"id", t.ID,
		"title", t.Title,
		"content", t.Content,
		"created_at", nullableEloquentTime(t.CreatedAt),
	))
}

// nullableEloquentTime 可空时间 → Eloquent toJSON 或 null。
func nullableEloquentTime(v sql.NullTime) any {
	if !v.Valid {
		return nil
	}
	return eloqTime(v.Time)
}
