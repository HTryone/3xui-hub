package admin

// 管理端「支付设置」的回归（只有两项：订单支付有效期 + 允许继续支付）。
//
// 值得钉住的点：
//  1. 路由路径：静态段 /payments/settings 必须排在 /payments/:payment 之前，
//     否则 "settings" 会被当成 id 落到详情路由上（静默 404）。
//  2. 校验口径与运行时同源：保存与读取共用 payment.ParseTTLMinutes。
//  3. 「允许继续支付」关掉时：写 site_configs + 调 ClearOrderPayURLs 清空缓存；
//     开启时不清空。这是「关掉就整套关闭、不留旧链接」的关键契约。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"controlhub-server/internal/driver/payment"

	"github.com/gin-gonic/gin"
)

// stubPaymentStore 满足 PaymentStore（本文件只测 settings，配置 CRUD 用不到）。
type stubPaymentStore struct{}

func (stubPaymentStore) PaymentsAll(context.Context) ([]PaymentRow, error) { return nil, nil }
func (stubPaymentStore) PaymentByID(context.Context, uint64) (*PaymentRow, error) {
	return nil, nil
}
func (stubPaymentStore) CreatePayment(context.Context, PaymentWrite) (*PaymentRow, error) {
	return nil, nil
}
func (stubPaymentStore) UpdatePayment(context.Context, uint64, PaymentWrite) (*PaymentRow, error) {
	return nil, nil
}
func (stubPaymentStore) DeletePayment(context.Context, uint64) error { return nil }

// stubPaymentSettings 是内存版 site_configs（写即可见，与真库一致）。
type stubPaymentSettings struct {
	values  map[string]string
	saved   map[string]string
	cleared int // ClearOrderPayURLs 被调用次数
}

func (s *stubPaymentSettings) SiteConfigMany(_ context.Context, keys []string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		if v, ok := s.values[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

func (s *stubPaymentSettings) SiteConfigSetMany(_ context.Context, kv []KV) error {
	if s.values == nil {
		s.values = map[string]string{}
	}
	if s.saved == nil {
		s.saved = map[string]string{}
	}
	for _, item := range kv {
		if item.Value != nil {
			s.values[item.Key] = *item.Value
			s.saved[item.Key] = *item.Value
		}
	}
	return nil
}

// ClearOrderPayURLs 记录「关闭继续支付时清空缓存」被调用（stub 无库，只记次数）。
func (s *stubPaymentSettings) ClearOrderPayURLs(context.Context) (int64, error) {
	s.cleared++
	return int64(s.cleared), nil
}

func newPaymentSettingsEngine(settings PaymentSettings) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	NewPaymentDomain(PaymentDeps{Store: stubPaymentStore{}, Settings: settings}).Register(engine.Group("/admin-api"))
	return engine
}

type settingsResp struct {
	Code int `json:"code"`
	Data struct {
		PayTTLMinutes      int  `json:"pay_ttl_minutes"`
		ContinuePayEnabled bool `json:"continue_pay_enabled"`
	} `json:"data"`
}

func decodeSettings(t *testing.T, body []byte) settingsResp {
	t.Helper()
	var out settingsResp
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析响应失败: %v body=%s", err, body)
	}
	return out
}

func putSettings(engine *gin.Engine, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/admin-api/payments/settings", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	engine.ServeHTTP(rec, req)
	return rec
}

// 路由必须在（且不能被 :payment 抢走）。
func TestPaymentSettingsRoutesRegistered(t *testing.T) {
	engine := newPaymentSettingsEngine(&stubPaymentSettings{})
	registered := map[string]bool{}
	for _, r := range engine.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	for _, want := range []string{
		"GET /admin-api/payments/settings",
		"PUT /admin-api/payments/settings",
		"GET /admin-api/payments/:payment",
	} {
		if !registered[want] {
			t.Fatalf("路由缺失: %s（已注册: %v）", want, registered)
		}
	}
}

// 未配置 → 回缺省（有效期默认 5 分钟、继续支付默认开）。
func TestPaymentSettingsDefaults(t *testing.T) {
	engine := newPaymentSettingsEngine(&stubPaymentSettings{})
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin-api/payments/settings", nil))

	out := decodeSettings(t, rec.Body.Bytes())
	if out.Code != 0 || out.Data.PayTTLMinutes != payment.DefaultTTLMinutes {
		t.Fatalf("未配置应回缺省 %d 分钟，实际 code=%d minutes=%d",
			payment.DefaultTTLMinutes, out.Code, out.Data.PayTTLMinutes)
	}
	if !out.Data.ContinuePayEnabled {
		t.Fatalf("「允许继续支付」缺省应为开启")
	}
}

// 有效期：合法落库、非法拒绝（与运行时同一套判据）。
func TestPaymentSettingsSaveTTL(t *testing.T) {
	settings := &stubPaymentSettings{}
	engine := newPaymentSettingsEngine(settings)

	out := decodeSettings(t, putSettings(engine, `{"pay_ttl_minutes":10}`).Body.Bytes())
	if out.Code != 0 || out.Data.PayTTLMinutes != 10 {
		t.Fatalf("保存 10 分钟应成功，实际 code=%d minutes=%d", out.Code, out.Data.PayTTLMinutes)
	}
	if settings.saved[payment.TTLMinutesKey] != "10" {
		t.Fatalf("应落库 %q，实际 %q", "10", settings.saved[payment.TTLMinutesKey])
	}

	for _, bad := range []string{`{"pay_ttl_minutes":0}`, `{"pay_ttl_minutes":999}`, `{"pay_ttl_minutes":"abc"}`} {
		settings.saved = nil
		if got := decodeSettings(t, putSettings(engine, bad).Body.Bytes()).Code; got != 400 {
			t.Fatalf("%s 应被拒（code=400），实际 code=%d", bad, got)
		}
		if len(settings.saved) != 0 {
			t.Fatalf("%s 不该写库，实际写入 %v", bad, settings.saved)
		}
	}
}

// 「允许继续支付」：关掉 → 写键 + 清空缓存；开启 → 不清空。
func TestPaymentSettingsContinuePayToggle(t *testing.T) {
	settings := &stubPaymentSettings{}
	engine := newPaymentSettingsEngine(settings)

	// 关掉
	out := decodeSettings(t, putSettings(engine, `{"continue_pay_enabled":false}`).Body.Bytes())
	if out.Code != 0 || out.Data.ContinuePayEnabled {
		t.Fatalf("关掉后应回 false，实际 code=%d enabled=%v", out.Code, out.Data.ContinuePayEnabled)
	}
	if settings.saved[payment.ContinuePayEnabledKey] != "0" {
		t.Fatalf("应写入 %q=0，实际 %v", payment.ContinuePayEnabledKey, settings.saved)
	}
	if settings.cleared == 0 {
		t.Fatalf("关闭继续支付时应调用 ClearOrderPayURLs 清空缓存")
	}
	// 关掉时不该动有效期
	if out.Data.PayTTLMinutes != payment.DefaultTTLMinutes {
		t.Fatalf("只改开关不该动有效期，实际 %d", out.Data.PayTTLMinutes)
	}

	// 重新开启 → 不清空
	settings.cleared = 0
	out = decodeSettings(t, putSettings(engine, `{"continue_pay_enabled":true}`).Body.Bytes())
	if !out.Data.ContinuePayEnabled {
		t.Fatalf("重新开启后应回 true")
	}
	if settings.cleared != 0 {
		t.Fatalf("开启时不该清空缓存，实际清理 %d 次", settings.cleared)
	}
}
