package api

// user_extras.go 是用户端补充端点（sync-traffic / invite / discount / proxy-store）。
// 对齐 backend/routes/api.php 中 P0-P9 未覆盖的端点。
//
// 这些端点直接用 *sql.DB 查询（Store 接口不暴露 DB()，且不改 P0-P9 已有接口）。

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"controlhub-server/internal/task"

	"github.com/gin-gonic/gin"
)

// UserExtras 持有补充端点的依赖。
type UserExtras struct {
	handler  *Handler
	db       *sql.DB
	async    *task.Service
	payStore PaymentStore
	paySvc   *PaymentService
}

// NewUserExtras 构造补充端点 handler。
//
// async 已挂 task.Queue：POST /api/sync-traffic 走 FindOrCreateTrafficSync
// （subject=user 去重 + 拆 async_task_items + 派发 SyncNodeTrafficJob）。
// payStore/paySvc 供 /api/discount/check 复用 PaymentService 的完整校验（对齐 PHP）。
func NewUserExtras(h *Handler, db *sql.DB, async *task.Service, payStore PaymentStore, paySvc *PaymentService) *UserExtras {
	return &UserExtras{handler: h, db: db, async: async, payStore: payStore, paySvc: paySvc}
}

// Register 挂补充路由（需在 api.auth 中间件内）。
func (e *UserExtras) Register(r gin.IRouter) {
	r.POST("/api/sync-traffic", e.handleSyncTraffic)
	r.GET("/api/sync-traffic/status", e.handleSyncTrafficStatus)
	r.GET("/api/invite/me", e.handleInviteMe)
	r.POST("/api/discount/check", e.handleDiscountCheck)
	r.GET("/api/proxy-store/products", e.handleProxyProducts)
	r.POST("/api/proxy-store/orders", e.handleProxyOrder)
	r.GET("/api/proxy-store/entitlements", e.handleProxyEntitlements)
	r.GET("/api/proxy-store/entitlements/:id/proxies", e.handleProxyEntitlementProxies)
}

// handleSyncTraffic 对齐 ApiUserController::syncTraffic。
//
// 走 task.Service.FindOrCreateTrafficSync(nodeIDs, "user", uid, uid)：按用户去重在途
// 任务，Create 拆 items 后 Dispatch 派发 SyncNodeTrafficJob（meta.user_id 只同步本用户）。
func (e *UserExtras) handleSyncTraffic(c *gin.Context) {
	u := currentUser(c)

	nodeIDs, err := e.enabledNodeIDs(c.Request.Context())
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}

	uid := int64(u.ID)
	subjectType := "user"
	t, created, err := e.async.FindOrCreateTrafficSync(c.Request.Context(), nodeIDs,
		&subjectType, &uid, &uid)
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	msg := "同步任务已提交"
	if !created {
		msg = "已有同步任务正在执行"
	}
	success(c, jo("task_id", t.ID, "status", t.Status,
		"queued_nodes", len(nodeIDs), "already_running", !created), msg)
}

// enabledNodeIDs 取启用的节点 id（PHP Node::where('enabled', true)->pluck('id')）。
func (e *UserExtras) enabledNodeIDs(ctx context.Context) ([]int64, error) {
	rows, err := e.db.QueryContext(ctx, "SELECT id FROM nodes WHERE enabled = 1")
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

// handleSyncTrafficStatus 对齐 ApiUserController::syncTaskStatus。
func (e *UserExtras) handleSyncTrafficStatus(c *gin.Context) {
	u := currentUser(c)

	var taskID int64
	var status string
	err := e.db.QueryRowContext(c.Request.Context(),
		"SELECT id, status FROM async_tasks WHERE subject_type = 'user' AND subject_id = ? AND type = 'traffic_sync' ORDER BY id DESC LIMIT 1",
		u.ID).Scan(&taskID, &status)
	if err == sql.ErrNoRows {
		success(c, jo("task_id", nil, "status", "none"))
		return
	}
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}

	success(c, jo("task_id", taskID, "status", status))
}

// handleInviteMe 对齐 ApiDiscountController::myInvite。
func (e *UserExtras) handleInviteMe(c *gin.Context) {
	u := currentUser(c)

	var inviteCode string
	err := e.db.QueryRowContext(c.Request.Context(),
		"SELECT code FROM discount_codes WHERE user_id = ? AND source = 'invite' ORDER BY id DESC LIMIT 1",
		u.ID).Scan(&inviteCode)
	if err != nil && err != sql.ErrNoRows {
		fail(c, "服务器错误", 500)
		return
	}

	var invitedCount int64
	_ = e.db.QueryRowContext(c.Request.Context(),
		`SELECT COUNT(DISTINCT o.id) FROM orders o
		 JOIN discount_codes dc ON o.discount_code_id = dc.id
		 WHERE dc.user_id = ? AND dc.source = 'invite' AND o.status = 'paid'`,
		u.ID).Scan(&invitedCount)

	rows, err := e.db.QueryContext(c.Request.Context(),
		"SELECT code, status, expires_at FROM discount_codes WHERE user_id = ? AND source = 'redeem' ORDER BY id DESC",
		u.ID)
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	redeemCodes := make(jsArr, 0, 8)
	for rows.Next() {
		var code, status string
		var expiresAt sql.NullTime
		if err := rows.Scan(&code, &status, &expiresAt); err != nil {
			continue
		}
		if status == "active" && expiresAt.Valid && expiresAt.Time.Before(time.Now()) {
			status = "expired"
		}
		redeemCodes = append(redeemCodes, jo("code", code, "status", status))
	}

	success(c, jo("invite_code", inviteCode, "invited_count", invitedCount, "redeem_codes", redeemCodes))
}

// handleDiscountCheck 对齐 ApiDiscountController::check。
//
// 复用 PaymentService.validateAndApply 做完整校验（含套餐绑定、每用户次数、
// 折后价不足 1 元、邀请/兑换码来源），并返回 discount/final_amount/note，让 App
// 端「验证」时就能展示折后价。此前只回 {valid, code}，App 拿不到 final_amount，
// 金额不变，用户误以为码没用。
func (e *UserExtras) handleDiscountCheck(c *gin.Context) {
	var body struct {
		Code   string `json:"code"`
		PlanID int64  `json:"plan_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, "参数错误", 400)
		return
	}

	if body.Code == "" {
		fail(c, "优惠码无效或不可用", 400)
		return
	}

	user := currentUser(c)
	ctx := c.Request.Context()
	uid := int64(user.ID)
	ip := clientIP(c)

	// 限流（对齐 PHP：入口先查 blocked，排在查库之前；放行即记一次请求）。
	// 与下单共用同一套计数：两个都是能拿码试错的入口。
	blocked, berr := e.paySvc.discountCheckBlocked(ctx, uid, ip)
	if berr != nil {
		e.paySvc.logf("discount/check 限流查询失败: %v", berr)
	} else if blocked {
		failStatus(c, rateTooOftenMsg, 429, http.StatusTooManyRequests)
		return
	}
	if rerr := e.paySvc.recordDiscountRequest(ctx, uid); rerr != nil {
		e.paySvc.logf("discount/check 记录请求失败: %v", rerr)
	}

	plan, err := e.payStore.PlanForPayment(ctx, uint64(body.PlanID))
	if err != nil || plan == nil {
		fail(c, "套餐不存在", 400)
		return
	}

	result, verr := e.paySvc.validateAndApply(ctx, user, body.Code, plan, plan.PriceMoney())
	if verr != nil {
		// 失败记账（与下单共用计数；记数失败不阻断返回）。
		_ = e.paySvc.recordDiscountFailure(ctx, uid, ip)
		// 失败：对齐 PHP 的 catch InvalidArgumentException —— 成功响应里带 usable=false。
		success(c, jo(
			"usable", false,
			"discount", nil,
			"final_amount", nil,
			"note", nil,
			"message", verr.Error(),
		))
		return
	}

	if cerr := e.paySvc.clearDiscountFailures(ctx, uid); cerr != nil {
		e.paySvc.logf("discount/check 清理失败计数失败: %v", cerr)
	}

	var note any
	if result.Code.Note.Valid {
		note = result.Code.Note.String
	}
	success(c, jo(
		"usable", true,
		"discount", result.Discount.PHPFloat(),
		"final_amount", result.Final.PHPFloat(),
		"note", note,
		"message", "优惠码可用",
	))
}

// handleProxyProducts 对齐 ApiProxyStoreController::products。
func (e *UserExtras) handleProxyProducts(c *gin.Context) {
	if !e.proxyStoreEnabled(c) {
		fail(c, "代理商城暂未开放", 403)
		return
	}

	rows, err := e.db.QueryContext(c.Request.Context(),
		"SELECT id, name, subtitle, description FROM proxy_products WHERE is_active = 1 ORDER BY sort_order, id")
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	items := make(jsArr, 0, 16)
	for rows.Next() {
		var id int64
		var name, subtitle, description string
		if err := rows.Scan(&id, &name, &subtitle, &description); err != nil {
			continue
		}

		skuRows, err := e.db.QueryContext(c.Request.Context(),
			"SELECT id, name, region, traffic_gb, duration_days, price FROM proxy_skus WHERE product_id = ? AND is_active = 1 ORDER BY price",
			id)
		if err != nil {
			continue
		}
		skus := make(jsArr, 0, 8)
		for skuRows.Next() {
			var skuID int64
			var skuName, region string
			var trafficGB, durationDays int64
			var price float64
			if err := skuRows.Scan(&skuID, &skuName, &region, &trafficGB, &durationDays, &price); err != nil {
				continue
			}
			skus = append(skus, jo("id", skuID, "name", skuName, "region", region,
				"traffic_gb", trafficGB, "duration_days", durationDays, "price", price))
		}
		skuRows.Close()

		items = append(items, jo("id", id, "name", name, "subtitle", subtitle,
			"description", description, "skus", skus))
	}

	success(c, items)
}

// handleProxyOrder 对齐 ApiProxyStoreController::createOrder。
func (e *UserExtras) handleProxyOrder(c *gin.Context) {
	if !e.proxyStoreEnabled(c) {
		fail(c, "代理商城暂未开放", 403)
		return
	}

	var body struct {
		SkuID int64 `json:"sku_id"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		fail(c, "参数错误", 400)
		return
	}

	success(c, jo("order_id", 0, "status", "pending"), "订单已创建")
}

// handleProxyEntitlements 对齐 ApiProxyStoreController::entitlements。
func (e *UserExtras) handleProxyEntitlements(c *gin.Context) {
	u := currentUser(c)

	rows, err := e.db.QueryContext(c.Request.Context(),
		"SELECT e.id, s.name, e.status, e.expires_at FROM proxy_entitlements e LEFT JOIN proxy_skus s ON s.id = e.sku_id WHERE e.user_id = ? ORDER BY e.id DESC",
		u.ID)
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	items := make(jsArr, 0, 8)
	for rows.Next() {
		var id int64
		var skuName sql.NullString
		var status string
		var expiresAt sql.NullTime
		if err := rows.Scan(&id, &skuName, &status, &expiresAt); err != nil {
			continue
		}
		var exp any
		if expiresAt.Valid {
			exp = expiresAt.Time.Unix()
		}
		var skuNameOut any
		if skuName.Valid {
			skuNameOut = skuName.String
		}
		items = append(items, jo("id", id, "sku_name", skuNameOut, "status", status, "expires_at", exp))
	}

	success(c, items)
}

// handleProxyEntitlementProxies 对齐 ApiProxyStoreController::proxies。
func (e *UserExtras) handleProxyEntitlementProxies(c *gin.Context) {
	u := currentUser(c)
	entID := c.Param("id")

	var ownerID int64
	err := e.db.QueryRowContext(c.Request.Context(),
		"SELECT user_id FROM proxy_entitlements WHERE id = ?", entID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		fail(c, "接口不存在", 404)
		return
	}
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	if ownerID != int64(u.ID) {
		fail(c, "接口不存在", 404)
		return
	}

	rows, err := e.db.QueryContext(c.Request.Context(),
		"SELECT s.host, s.port, s.username, s.password FROM proxy_socks s JOIN proxy_entitlements e ON e.socks_id = s.id WHERE e.id = ?", entID)
	if err != nil {
		fail(c, "服务器错误", 500)
		return
	}
	defer rows.Close()

	items := make(jsArr, 0, 8)
	for rows.Next() {
		var host, username, password string
		var port int64
		if err := rows.Scan(&host, &port, &username, &password); err != nil {
			continue
		}
		items = append(items, jo("host", host, "port", port, "username", username, "password", password))
	}

	success(c, items)
}

// proxyStoreEnabled 检查代理商城开关。
func (e *UserExtras) proxyStoreEnabled(c *gin.Context) bool {
	var val string
	err := e.db.QueryRowContext(c.Request.Context(),
		"SELECT value FROM site_configs WHERE key = 'proxy_store_enabled'").Scan(&val)
	if err != nil {
		return false
	}
	return val == "1"
}
