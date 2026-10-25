package api

import (
	"encoding/json"
	"io"
	"net"
	"strings"

	"github.com/gin-gonic/gin"
)

// jsonInput 读取 JSON 请求体并模拟 Laravel 的全局输入处理：
//
//	TrimStrings            → 字符串去首尾空白（password / current_password / password_confirmation 除外）
//	ConvertEmptyStringsToNull → 去空白后为空的字符串变成 null
//
// 这两步在 PHP 里发生在所有路由之前，所以控制器看到的输入永远是「已整理」的形态：
// 例如 email=" a@b.co " 会通过校验、password="      " 视为未填。
// 不做这一步，Go 版的校验结果就会与 PHP 分叉（尤其是「空格密码」这种输入）。
func jsonInput(c *gin.Context) map[string]any {
	input := map[string]any{}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, 1<<20))
	if err != nil || len(body) == 0 {
		return input
	}
	// 非 JSON 请求体（表单）在用户端接口里不存在：四个认证端点与 protocol 都发 JSON。
	if !strings.Contains(c.GetHeader("Content-Type"), "json") {
		return input
	}
	var raw any
	if err := json.Unmarshal(body, &raw); err != nil {
		return input
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return input
	}
	for k, v := range obj {
		input[k] = normalizeInputValue(k, v)
	}
	return input
}

// normalizeInputValue 递归模拟 TrimStrings + ConvertEmptyStringsToNull。
func normalizeInputValue(key string, v any) any {
	switch t := v.(type) {
	case string:
		if neverTrimKey(key) {
			// password 系字段不 trim，但空串仍转 null（ConvertEmptyStringsToNull 不看 except 列表）。
			if t == "" {
				return nil
			}
			return t
		}
		t = phpTrim(t)
		if t == "" {
			return nil
		}
		return t
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = normalizeInputValue(key, item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, item := range t {
			out[k] = normalizeInputValue(key+"."+k, item)
		}
		return out
	}
	return v
}

// neverTrimKey 对齐 TrimStrings::$except（按 Str::is 通配匹配 —— 这里只有精确键）。
func neverTrimKey(key string) bool {
	last := key
	if i := strings.LastIndexByte(key, '.'); i >= 0 {
		last = key[i+1:]
	}
	switch last {
	case "current_password", "password", "password_confirmation":
		return true
	}
	return false
}

// clientIP 对齐 Laravel Request::ip() 在无 TrustProxies 配置时的行为：取直连地址。
// （测试与本地部署都直连；反向代理场景由运维层在网关补齐真实 IP。）
func clientIP(c *gin.Context) string {
	if ip := c.ClientIP(); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(c.Request.RemoteAddr)
	if err != nil {
		return c.Request.RemoteAddr
	}
	return host
}

// bearerToken 取 Authorization: Bearer {token}。与 ApiAuth 同口径：只认 "Bearer " 前缀。
func bearerToken(c *gin.Context) string {
	header := c.GetHeader("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	return header[len("Bearer "):]
}

// notFoundRouteMsg 是 bootstrap/app.php 对 NotFoundHttpException 的统一改写文案。
const notFoundRouteMsg = "接口不存在"

// respondValidation 把校验失败渲染成 PHP 的形态：HTTP 200 + code 500 + 合并后的文案。
//
// ⚠️ 这是 PHP 现状（bootstrap/app.php 的 render 回调抢在 422 分支之前，取 getStatusCode()
// 拿到 500），不是笔误。前端只读 msg，所以文案必须逐字一致。
func respondValidation(c *gin.Context, v *validator) {
	fail(c, v.summary(), 500)
}
