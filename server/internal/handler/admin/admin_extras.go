package admin

// admin_extras.go 是管理端补充端点（monitor / sync-traffic / async-tasks）。
// 对齐 backend/routes/web.php 的 admin.auth 段中 P0-P9 未覆盖的端点。
// Monitor summary / series 的完整实现见 monitor.go（对齐 MonitorService）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"time"

	"controlhub-server/internal/task"

	"github.com/gin-gonic/gin"
)

// AdminExtras 持有补充端点的依赖。
type AdminExtras struct {
	db    *sql.DB
	async *task.Service
	now   func() time.Time
}

// NewAdminExtras 构造补充端点 handler。
//
// async 已挂 task.Queue（cmd/server/routes.go 用 NewQueue 构造后传入）：
// POST /sync-traffic 必须走 FindOrCreateTrafficSync —— 只有它会拆 async_task_items
// 并派发 SyncNodeTrafficJob。手搓 INSERT async_tasks 只会落一条 total=0 的 pending
// 壳，任务永远不会被执行，最后被 async-task-timeout 标失败。
func NewAdminExtras(db *sql.DB, async *task.Service) *AdminExtras {
	return &AdminExtras{db: db, async: async, now: time.Now}
}

// Register 挂补充路由（在 admin-api 分组内）。
func (h *AdminExtras) Register(r gin.IRouter) {
	// 监控
	r.GET("/monitor/summary", h.MonitorSummary)
	r.GET("/monitor/series", h.MonitorSeries)

	// 流量同步
	r.POST("/sync-traffic", h.SyncTraffic)

	// 异步任务
	r.GET("/async-tasks", h.AsyncTaskIndex)
	r.POST("/async-tasks/:asyncTask/retry", h.AsyncTaskRetry)

	// App 版本分布
	r.GET("/app-versions", h.AppVersions)
}

// SyncTraffic 对齐 POST /admin-api/sync-traffic（SyncTrafficController::sync）。
//
// 复用 task.Service.FindOrCreateTrafficSync：同 scope 在途任务去重 → Create 拆
// async_task_items → Dispatch 把 SyncNodeTrafficJob 派进队列。手搓 INSERT 只会
// 落一条 total=0/attempts=0 的 pending 壳，永远不会被执行。
func (h *AdminExtras) SyncTraffic(c *gin.Context) {
	nodeIDs, err := h.enabledNodeIDs(c.Request.Context())
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	t, created, err := h.async.FindOrCreateTrafficSync(c.Request.Context(), nodeIDs, nil, nil, nil)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}
	msg := "同步任务已提交"
	if !created {
		msg = "已有同步任务正在执行"
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", msg, "data",
		jo("task_id", t.ID, "status", t.Status, "queued_nodes", len(nodeIDs), "already_running", !created))))
}

// enabledNodeIDs 取启用的节点 id（PHP Node::where('enabled', true)->pluck('id')）。
func (h *AdminExtras) enabledNodeIDs(ctx context.Context) ([]int64, error) {
	rows, err := h.db.QueryContext(ctx, "SELECT id FROM nodes WHERE enabled = 1")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	nodeIDs := make([]int64, 0, 16)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			nodeIDs = append(nodeIDs, id)
		}
	}
	return nodeIDs, rows.Err()
}

// AsyncTaskIndex 对齐 GET /admin-api/async-tasks。
func (h *AdminExtras) AsyncTaskIndex(c *gin.Context) {
	rows, err := h.db.QueryContext(c.Request.Context(),
		"SELECT id, type, status, meta, error, total, completed, failed, attempts, max_attempts, created_at, updated_at FROM async_tasks ORDER BY id DESC LIMIT 100")
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	items := make(jsArr, 0, 100)
	for rows.Next() {
		var id int64
		var typ, status string
		var meta, errMsg sql.NullString
		var total, completed, failed, attempts, maxAttempts int
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &typ, &status, &meta, &errMsg,
			&total, &completed, &failed, &attempts, &maxAttempts,
			&createdAt, &updatedAt); err != nil {
			continue
		}
		// meta 对齐 PHP 的 array cast：落库是 JSON 字符串，输出解码成数组/对象。
		metaVal := any(nil)
		if meta.Valid && meta.String != "" {
			var decoded any
			if json.Unmarshal([]byte(meta.String), &decoded) == nil {
				metaVal = decoded
			} else {
				metaVal = meta.String
			}
		}
		// created_at/updated_at 对齐 Eloquent datetime cast：ISO 8601（带微秒 + Z），
		// 不能输出 Unix 秒 —— 前端 new Date(秒) 会按毫秒解析成 1970 年。
		items = append(items, jo(
			"id", id,
			"type", typ,
			"status", status,
			"meta", metaVal,
			"error", errMsg.String,
			"total", total,
			"completed", completed,
			"failed", failed,
			"attempts", attempts,
			"max_attempts", maxAttempts,
			"created_at", eloqTime(createdAt),
			"updated_at", eloqTime(updatedAt),
		))
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data", items)))
}

// AsyncTaskRetry 对齐 POST /admin-api/async-tasks/{asyncTask}/retry。
//
// 必须走 task.Service.Retry：它会把失败子项重置为 pending 并按任务类型重新派发
// （入站同步走扫描、其余按批派发）。以前这里只手写 UPDATE ... status='pending'，
// 状态改回 pending 却没人执行 —— 按钮点了等于没点。
func (h *AdminExtras) AsyncTaskRetry(c *gin.Context) {
	taskID, err := strconv.ParseInt(c.Param("asyncTask"), 10, 64)
	if err != nil {
		h.fail(c, "接口不存在", 404)
		return
	}

	ctx := c.Request.Context()
	t, err := h.async.Get(ctx, taskID)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}
	if t == nil {
		h.fail(c, "接口不存在", 404)
		return
	}

	if _, err := h.async.Retry(ctx, taskID); err != nil {
		// 「仅失败任务可以重试 / 任务已达到最大尝试次数 / 没有可重试的失败项」
		h.fail(c, err.Error(), 400)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "任务已重新派发", "data", nil)))
}

// AppVersions 对齐 GET /admin-api/app-versions：返回版本分布（含「未上报」的 null 行）。
// percent 由后端算好（保留一位小数），null 行表示从未上报过版本。
func (h *AdminExtras) AppVersions(c *gin.Context) {
	rows, err := h.db.QueryContext(c.Request.Context(),
		`SELECT app_version_code, app_version_name, COUNT(*) AS cnt
		 FROM users
		 GROUP BY app_version_code, app_version_name
		 ORDER BY app_version_code DESC`)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	type versionRow struct {
		code sql.NullInt64
		name sql.NullString
		cnt  int64
	}
	var list []versionRow
	var total int64
	for rows.Next() {
		var r versionRow
		if err := rows.Scan(&r.code, &r.name, &r.cnt); err != nil {
			continue
		}
		list = append(list, r)
		total += r.cnt
	}
	if err := rows.Err(); err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	items := make(jsArr, 0, len(list))
	for _, r := range list {
		var code any
		var name any
		if r.code.Valid {
			code = r.code.Int64
		}
		if r.name.Valid {
			name = r.name.String
		}
		var percent float64
		if total > 0 {
			percent = math.Round(float64(r.cnt)/float64(total)*1000) / 10
		}
		items = append(items, jo(
			"version_code", code,
			"version_name", name,
			"count", r.cnt,
			"percent", percent,
		))
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data", items)))
}

// fail 输出 PHP ApiResponse::error 口径。
func (h *AdminExtras) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}
