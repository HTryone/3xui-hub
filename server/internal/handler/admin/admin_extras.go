package admin

// admin_extras.go 是管理端补充端点（monitor / sync-traffic / async-tasks）。
// 对齐 backend/routes/web.php 的 admin.auth 段中 P0-P9 未覆盖的端点。
// Monitor summary / series 的完整实现见 monitor.go（对齐 MonitorService）。

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/gin-gonic/gin"
)

// AdminExtras 持有补充端点的依赖。
type AdminExtras struct {
	db  *sql.DB
	now func() time.Time
}

// NewAdminExtras 构造补充端点 handler。
func NewAdminExtras(db *sql.DB) *AdminExtras {
	return &AdminExtras{db: db, now: time.Now}
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
}

// SyncTraffic 对齐 POST /admin-api/sync-traffic。
func (h *AdminExtras) SyncTraffic(c *gin.Context) {
	// 查启用的节点
	rows, err := h.db.QueryContext(c.Request.Context(),
		"SELECT id FROM nodes WHERE enabled = 1")
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	nodeIDs := make([]int64, 0, 16)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			nodeIDs = append(nodeIDs, id)
		}
	}

	// 创建异步任务
	taskID, err := h.createTrafficSyncTask(c, nodeIDs)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "同步任务已提交", "data",
		jo("task_id", taskID, "status", "pending", "queued_nodes", len(nodeIDs), "already_running", false))))
}

// AsyncTaskIndex 对齐 GET /admin-api/async-tasks。
func (h *AdminExtras) AsyncTaskIndex(c *gin.Context) {
	rows, err := h.db.QueryContext(c.Request.Context(),
		"SELECT id, type, status, meta, error, created_at, updated_at FROM async_tasks ORDER BY id DESC LIMIT 100")
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
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&id, &typ, &status, &meta, &errMsg, &createdAt, &updatedAt); err != nil {
			continue
		}
		items = append(items, jo(
			"id", id,
			"type", typ,
			"status", status,
			"meta", meta.String,
			"error", errMsg.String,
			"created_at", createdAt.Unix(),
			"updated_at", updatedAt.Unix(),
		))
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "ok", "data", items)))
}

// AsyncTaskRetry 对齐 POST /admin-api/async-tasks/{asyncTask}/retry。
func (h *AdminExtras) AsyncTaskRetry(c *gin.Context) {
	taskID := c.Param("asyncTask")

	// 检查任务存在且状态为 failed
	var status string
	err := h.db.QueryRowContext(c.Request.Context(),
		"SELECT status FROM async_tasks WHERE id = ?", taskID).Scan(&status)
	if err == sql.ErrNoRows {
		h.fail(c, "接口不存在", 404)
		return
	}
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	if status != "failed" {
		h.fail(c, "任务当前状态不允许重试", 400)
		return
	}

	_, err = h.db.ExecContext(c.Request.Context(),
		"UPDATE async_tasks SET status = 'pending', error = NULL WHERE id = ?", taskID)
	if err != nil {
		h.fail(c, "服务器错误", 500)
		return
	}

	writeJSON(c, phpEncode(jo("code", 0, "msg", "任务已重新派发", "data", nil)))
}

// createTrafficSyncTask 创建流量同步异步任务。
func (h *AdminExtras) createTrafficSyncTask(c *gin.Context, nodeIDs []int64) (int64, error) {
	// 对齐 PHP AsyncTaskService::dispatch：meta = ['node_ids' => $nodeIds]
	meta, err := json.Marshal(map[string]any{"node_ids": nodeIDs})
	if err != nil {
		return 0, err
	}
	result, err := h.db.ExecContext(c.Request.Context(),
		`INSERT INTO async_tasks (type, status, meta, created_at, updated_at)
		 VALUES ('traffic_sync', 'pending', ?, ?, ?)`,
		string(meta), h.now().UTC(), h.now().UTC())
	if err != nil {
		return 0, err
	}
	id, _ := result.LastInsertId()
	return id, nil
}

// fail 输出 PHP ApiResponse::error 口径。
func (h *AdminExtras) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}
