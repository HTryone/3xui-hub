package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 响应封装。契约见 contracts/api-contract.md §1：
//
//	成功：{"code":0,"msg":"ok","data":...}
//	失败：{"code":非0,"msg":"...","data":null}    HTTP status 默认 200
//
// 与 httpx.Success/Error 的差别：本包用 PHP 口径的编码器（中文转义、"/" 转义、键序保持），
// 这样与 PHP 的响应能逐字节对拍，也让前端 devtools 里看到的编码与 PHP 版一致。

// writeJSON 输出 HTTP 200 + 给定 JSON 字节（content-type 与 Laravel JsonResponse 一致）。
func writeJSON(c *gin.Context, body []byte) {
	c.Data(http.StatusOK, "application/json; charset=utf-8", body)
}

// writeRaw 输出非 JSON 响应（图形验证码 PNG）。
func writeRaw(c *gin.Context, contentType string, body []byte, headers map[string]string) {
	for k, v := range headers {
		c.Header(k, v)
	}
	c.Data(http.StatusOK, contentType, body)
}

// success 成功响应，msg 默认 "ok"。
func success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

// successNoMsg 用于 PHP 里直接 response()->json(['code'=>0,'data'=>...]) 的端点
// （没有 msg 字段 —— tutorials 两个路由就是这样）。
func successNoMsg(c *gin.Context, data any) {
	writeJSON(c, phpEncode(jo("code", 0, "data", data)))
}

// fail 失败响应：HTTP 200 + {code, msg, data:null}。
func fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

// failStatus 少数显式指定 HTTP status 的失败响应（429 限流、email-verify 的 400/500）。
func failStatus(c *gin.Context, msg string, code, status int) {
	c.Data(status, "application/json; charset=utf-8",
		phpEncode(jo("code", code, "msg", msg, "data", nil)))
}
