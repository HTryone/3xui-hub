package api

// 「我的订单·继续支付」的两条核心回归：
//  1. 未到点 + 有缓存链接 → 直接用缓存，**完全不碰网关**（缓存命中即绕开判重）；
//  2. 已到点 → 一律作废并回 410，**绝不把网关那边已失效的链接发给用户**。
//
// 背景：payindex 等网关会把「同单号再次下单」判重，我们据此把单作废成 expired ——
// 表现为「点续付就提示过期」。缓存 + 我们自己的支付有效期共同解决这个问题。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"controlhub-server/internal/driver/payment"

	"github.com/gin-gonic/gin"
)

// cacheRetryStore 只实现续付用到的两个方法：取单 + 记录状态变更。
// 其余方法由嵌入的 nil PaymentStore 兜底 —— 一旦续付路径意外去查支付配置就会 panic，
// 正好证明「缓存命中时没有打网关」。
type cacheRetryStore struct {
	PaymentStore
	order  *Order
	status map[uint64]string
}

func (s *cacheRetryStore) OrderByNoForUser(_ context.Context, _ uint64, _, _ string) (*Order, error) {
	return s.order, nil
}

func (s *cacheRetryStore) UpdateOrderStatus(_ context.Context, id uint64, status string) error {
	if s.status == nil {
		s.status = map[uint64]string{}
	}
	s.status[id] = status
	return nil
}

// retryResp 是 /api/payment/retry 的响应体。
type retryResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		OrderNo string `json:"order_no"`
		PayURL  string `json:"pay_url"`
	} `json:"data"`
}

// postRetry 用给定订单跑一次续付，返回响应。
func postRetry(t *testing.T, order *Order) (retryResp, *cacheRetryStore) {
	t.Helper()
	return postRetryWithSettings(t, order, nil)
}

// postRetryWithSettings 允许注入 site_configs（测试「允许继续支付」开关关闭时的行为）。
func postRetryWithSettings(t *testing.T, order *Order, siteCfg map[string]string) (retryResp, *cacheRetryStore) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	store := &cacheRetryStore{order: order}
	opts := PaymentServiceOptions{}
	if siteCfg != nil {
		opts.SiteConfigs = fakeSiteConfigReader(siteCfg)
	}
	svc := NewPaymentService(store, opts, nil)
	// 时钟固定：now 字段是本包私有，测试里直接注入（与生产同一条读取路径）。
	h := &PaymentHandlers{svc: svc, store: store, cfg: &Handler{now: func() time.Time { return fixedNow }}}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/api/payment/retry",
		strings.NewReader(`{"order_no":"`+order.OrderNo+`"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxUserKey, &User{ID: order.UserID})

	h.handlePaymentRetry(c)

	var out retryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
	}
	return out, store
}

// TestRetryBlockedWhenContinuePayDisabled：「允许继续支付」关掉后，续付接口直接拒绝（403），
// 不下发任何链接 —— 与前端隐藏按钮形成双保险。
func TestRetryBlockedWhenContinuePayDisabled(t *testing.T) {
	order := &Order{
		ID: 9010, OrderNo: "20261110000000009010", UserID: 7, Status: "pending",
		PayURL:       sql.NullString{String: "https://pay.example.com/ok", Valid: true},
		CreatedAt:    fixedNow,
		PayExpiresAt: PayExpiresAt(fixedNow, 5*time.Minute),
	}
	out, store := postRetryWithSettings(t, order, map[string]string{
		payment.ContinuePayEnabledKey: "0",
	})
	if out.Code != 403 || out.Data.PayURL != "" {
		t.Fatalf("关闭继续支付后应 403 且不给链接，实际 code=%d url=%q", out.Code, out.Data.PayURL)
	}
	if len(store.status) != 0 {
		t.Fatalf("被拒时不该改订单状态，实际 %v", store.status)
	}
}

// TestRetryUsesCachedPayURL：未到点 + 有缓存 → 直接返回缓存值，且不碰支付配置/网关。
// 订单故意给一个不存在的 payment_config（PaymentConfigID=0、store 未实现配置查询）——
// 只要走了 buildPayURL 分支就会 panic / 报错；能返回 200 + 缓存链接即证明是短路返回。
func TestRetryUsesCachedPayURL(t *testing.T) {
	const cached = "https://pay.example.com/h5?order=20261110000000000001"
	order := &Order{
		ID:      9001,
		OrderNo: "20261110000000000001",
		UserID:  7,
		Status:  "pending",
		PayURL:  sql.NullString{String: cached, Valid: true},
		// 支付窗口未到点（下单时刻 + 有效期）
		CreatedAt:    fixedNow,
		PayExpiresAt: PayExpiresAt(fixedNow, 5*time.Minute),
	}

	out, store := postRetry(t, order)
	if out.Code != 0 {
		t.Fatalf("应成功返回缓存链接，实际 code=%d msg=%s", out.Code, out.Msg)
	}
	if out.Data.PayURL != cached {
		t.Fatalf("pay_url = %q, 期望命中缓存 %q", out.Data.PayURL, cached)
	}
	if out.Data.OrderNo != order.OrderNo {
		t.Fatalf("order_no = %q, 期望 %q", out.Data.OrderNo, order.OrderNo)
	}
	if len(store.status) != 0 {
		t.Fatalf("未到点不该改状态，实际 %v", store.status)
	}
}

// TestRetryExpiredReturns410：支付窗口已过 → 作废该单并回 410，不给死链。
func TestRetryExpiredReturns410(t *testing.T) {
	order := &Order{
		ID:      9002,
		OrderNo: "20261110000000000002",
		UserID:  7,
		Status:  "pending",
		// 网关那边这条链接早已失效，缓存里却还留着 —— 必须拦住，不能发给用户
		PayURL:       sql.NullString{String: "https://pay.example.com/h5?dead=1", Valid: true},
		CreatedAt:    fixedNow.Add(-30 * time.Minute),
		PayExpiresAt: PayExpiresAt(fixedNow.Add(-30*time.Minute), 5*time.Minute),
	}

	out, store := postRetry(t, order)
	if out.Code != 410 {
		t.Fatalf("已到点应回 410，实际 code=%d msg=%s", out.Code, out.Msg)
	}
	if out.Data.PayURL != "" {
		t.Fatalf("已到点绝不能下发支付链接，实际 %q", out.Data.PayURL)
	}
	if store.status[order.ID] != "expired" {
		t.Fatalf("应把该单落库为 expired，实际 %v", store.status)
	}
}

// TestRetryLegacyOrderWithoutExpiry：老单（无 pay_expires_at）按缺省 TTL 兜底判定 ——
// 刚落单的能续付，久远的拦下。这条钉住「没有新列也不能把老单判错」。
func TestRetryLegacyOrderWithoutExpiry(t *testing.T) {
	const cached = "https://pay.example.com/h5?legacy=1"

	fresh := &Order{
		ID: 9003, OrderNo: "20261110000000000003", UserID: 7, Status: "pending",
		PayURL:    sql.NullString{String: cached, Valid: true},
		CreatedAt: fixedNow.Add(-time.Minute), // 缺省 5 分钟内
	}
	if out, _ := postRetry(t, fresh); out.Code != 0 || out.Data.PayURL != cached {
		t.Fatalf("老单未到点应回缓存链接，实际 code=%d url=%q", out.Code, out.Data.PayURL)
	}

	stale := &Order{
		ID: 9004, OrderNo: "20261110000000000004", UserID: 7, Status: "pending",
		PayURL:    sql.NullString{String: cached, Valid: true},
		CreatedAt: fixedNow.Add(-time.Hour), // 远超缺省 5 分钟
	}
	if out, _ := postRetry(t, stale); out.Code != 410 {
		t.Fatalf("老单超期应回 410，实际 code=%d", out.Code)
	}
}
