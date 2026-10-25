package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"controlhub-server/internal/config"
	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// /api/ping 是与 PHP 后端逐字节对齐的探活契约：
// 成功响应直接对比 JSON 原文，字段顺序/命名/大小写都不能漂。
func TestPingContract(t *testing.T) {
	// 只测路由，不连库：NewApp 的 db 传 nil。
	app := httpx.NewApp(nil, &config.Config{AppEnv: "testing"})
	r, _ := newRouter(app, nil)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ping", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HTTP status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if body["code"] != float64(0) || body["msg"] != "ok" {
		t.Errorf("外层字段不对: %v", body)
	}
	data, ok := body["data"].(map[string]any)
	if !ok {
		t.Fatalf("data 应为对象，实际 %T", body["data"])
	}
	if data["pong"] != true {
		t.Errorf("data.pong = %v, want true", data["pong"])
	}
	if len(data) != 1 {
		t.Errorf("data 应只含 pong，实际 %v", data)
	}
}

// 生产环境要自动切 gin release 模式，别把调试输出打到线上。
func TestRouterUsesReleaseModeInProduction(t *testing.T) {
	app := httpx.NewApp(nil, &config.Config{AppEnv: "production"})
	_, _ = newRouter(app, nil)
	if gin.Mode() != gin.ReleaseMode {
		t.Fatalf("gin.Mode() = %q, want release", gin.Mode())
	}
}
