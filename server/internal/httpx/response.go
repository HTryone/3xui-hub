// Package httpx 提供与 Laravel ApiResponse trait 完全对齐的响应封装。
//
// 契约（Traits/ApiResponse.php，contracts/api-contract.md §1）：
//   - 成功：{"code":0,"msg":"ok","data":...}
//   - 失败：{"code":非0,"msg":"...","data":null}，HTTP status 默认 200（前端靠 code 判断，不看 http status）
//   - 分页：{"code":0,"msg":"ok","data":[当前页数组],"pagination":{current_page,last_page,total,per_page}}
package httpx

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// PerPage 是分页固定条数。契约明确：固定 50 条/页，不接受 per_page 参数。
const PerPage = 50

// Success 成功响应。data 可为 nil / 对象 / 数组。msg 缺省为 "ok"。
func Success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	c.JSON(http.StatusOK, gin.H{"code": 0, "msg": m, "data": data})
}

// Error 失败响应。code 为业务错误码（非 0），httpStatus 默认 200（与 Laravel 一致）。
// 只有少数端点（429 限流、节点端 401）需要显式传 httpStatus。
func Error(c *gin.Context, msg string, code int, httpStatus ...int) {
	status := http.StatusOK
	if len(httpStatus) > 0 {
		status = httpStatus[0]
	}
	c.JSON(status, gin.H{"code": code, "msg": msg, "data": nil})
}

// Pagination 分页元信息，字段名与 Laravel paginate() 一致（snake_case，逐字对齐）。
type Pagination struct {
	CurrentPage int   `json:"current_page"`
	LastPage    int   `json:"last_page"`
	Total       int64 `json:"total"`
	PerPage     int   `json:"per_page"`
}

// NewPagination 由请求页码与总数计算分页元信息，并把越界页码夹回最后一页。
//
// 对齐 Laravel successPage()：?page=999 时返回最后一页的数据而不是空页，
// 因此 handler 必须用返回值的 Offset()/Limit() 取值，而不是直接用请求里的 page。
func NewPagination(page int, total int64) Pagination {
	lastPage := int((total + PerPage - 1) / PerPage)
	if lastPage < 1 {
		lastPage = 1
	}
	if page < 1 {
		page = 1
	}
	if page > lastPage {
		page = lastPage
	}
	return Pagination{
		CurrentPage: page,
		LastPage:    lastPage,
		Total:       total,
		PerPage:     PerPage,
	}
}

// Offset 是 LIMIT 子句的偏移量（已按夹取后的页码计算）。
func (p Pagination) Offset() int { return (p.CurrentPage - 1) * p.PerPage }

// Limit 是 LIMIT 子句的条数。
func (p Pagination) Limit() int { return p.PerPage }

// Page 分页成功响应。data 为当前页数组，分页元信息放顶层 pagination
// （data 仍是数组，老消费方解 data 不受影响）。
func Page(c *gin.Context, data any, p Pagination) {
	c.JSON(http.StatusOK, gin.H{
		"code":       0,
		"msg":        "ok",
		"data":       data,
		"pagination": p,
	})
}

// QueryPage 读取 ?page= 参数，缺省/非法值回落到第 1 页（Laravel paginate 的行为）。
func QueryPage(c *gin.Context) int {
	page := 1
	if raw := c.Query("page"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			page = n
		}
	}
	return page
}
