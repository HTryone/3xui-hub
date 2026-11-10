package api

// 「我的订单·继续支付」的核心回归：**优先用下单时缓存的 pay_url，不再拿原单号去打网关**。
//
// 背景：payindex 等网关会把「同单号再次下单」判重，我们据此把单作废成 expired ——
// 表现为「点续付就提示过期」。缓存命中即绕开网关，这条必须钉死。

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

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

func (s *cacheRetryStore) OrderByNoForUser(_ context.Context, _ uint64, _ , _ string) (*Order, error) {
	return s.order, nil
}

func (s *cacheRetryStore) UpdateOrderStatus(_ context.Context, id uint64, status string) error {
	if s.status == nil {
		s.status = map[uint64]string{}
	}
	s.status[id] = status
	return nil
}

// TestRetryUsesCachedPayURL：有缓存链接时必须直接返回缓存值，且**完全不碰支付配置/网关**。
// 订单故意给一个不存在的 payment_config（PaymentConfigID=0、且 store 未实现配置查询）——
// 只要走了 buildPayURL 分支就会 panic / 报错；能返回 200 + 缓存链接即证明是短路返回。
func TestRetryUsesCachedPayURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const cached = "https://pay.example.com/h5?order=20261110000000000001"
	order := &Order{
		ID:      9001,
		OrderNo: "20261110000000000001",
		UserID:  7,
		Status:  "pending",
		PayURL:  sql.NullString{String: cached, Valid: true},
	}
	store := &cacheRetryStore{order: order}
	// svc 绑定同一个 store；缓存路径不会用到 driver/config。
	svc := NewPaymentService(store, PaymentServiceOptions{}, nil)
	h := &PaymentHandlers{svc: svc, store: store}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	body := `{"order_no":"20261110000000000001"}`
	c.Request = httptest.NewRequest("POST", "/api/payment/retry", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxUserKey, &User{ID: 7})

	h.handlePaymentRetry(c)

	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			OrderNo string `json:"order_no"`
			PayURL  string `json:"pay_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, rec.Body.String())
	}
	if out.Code != 0 {
		t.Fatalf("应成功返回缓存链接，实际 code=%d msg=%s", out.Code, out.Msg)
	}
	if out.Data.PayURL != cached {
		t.Fatalf("pay_url = %q, 期望命中缓存 %q", out.Data.PayURL, cached)
	}
	if out.Data.OrderNo != order.OrderNo {
		t.Fatalf("order_no = %q, 期望 %q", out.Data.OrderNo, order.OrderNo)
	}
}
