package admin

// 管理端多域名（/admin-api/domains…）：CRUD + primary/apply/renew。
//
// 逐条对齐 backend/app/Http/Controllers/Admin/DomainController.php。
// 业务失败一律 code=1（ApiResponse::error($msg, 1)），不是 500。

import (
	"regexp"
	"strings"

	"controlhub-server/internal/httpx"

	"github.com/gin-gonic/gin"
)

// domainHostRe 对齐 store() 里的合法主机名校验（去掉 Go 不支持的 (?= 总长断言，
// 总长 3–253 由 domainHostValid 单独判）：
// 标签 1–63 字符，字母/数字/连字符，不以连字符开头或结尾。
var domainHostRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9\-]{0,61}[a-z0-9])?$`)

// domainHostValid 对齐 PHP 的整条正则（含 3–253 总长断言）。
func domainHostValid(domain string) bool {
	n := len(domain)
	if n < 3 || n > 253 {
		return false
	}
	return domainHostRe.MatchString(domain)
}

// Register 把域名域路由挂到给定分组。
//
// 顺序逐条对齐 backend/routes/web.php。
func (d *DomainsDomain) Register(r gin.IRouter) {
	r.GET("/domains", d.domainIndex)
	r.POST("/domains", d.domainStore)
	r.POST("/domains/:id/primary", d.domainSetPrimary)
	r.POST("/domains/:id/apply", d.domainApply)
	r.POST("/domains/:id/renew", d.domainRenew)
	r.DELETE("/domains/:id", d.domainDestroy)
}

// ---- 响应封装 ----

func (d *DomainsDomain) success(c *gin.Context, data any, msg ...string) {
	m := "ok"
	if len(msg) > 0 && msg[0] != "" {
		m = msg[0]
	}
	writeJSON(c, phpEncode(jo("code", 0, "msg", m, "data", data)))
}

// fail 对齐 ApiResponse::error($msg, $code) —— 本域业务错误固定 code=1。
func (d *DomainsDomain) fail(c *gin.Context, msg string, code int) {
	writeJSON(c, phpEncode(jo("code", code, "msg", msg, "data", nil)))
}

func (d *DomainsDomain) failServer(c *gin.Context, what string, err error) {
	d.logger.Printf("admin %s 失败: %v", what, err)
	c.Set(httpx.OpLogErrorKey, what+": "+err.Error())
	d.fail(c, "服务器错误", 500)
}

// loadDomain 按路由参数取域名行；非数字 / 不存在 / 未启用 → code 1「域名不存在」。
func (d *DomainsDomain) loadDomain(c *gin.Context) (*DomainRow, bool) {
	id, ok := parseUint(c.Param("id"))
	if !ok {
		d.fail(c, "域名不存在", 1)
		return nil, false
	}
	row, err := d.store.DomainByID(ctxOf(c), id)
	if err != nil {
		if NoRows(err) {
			d.fail(c, "域名不存在", 1)
			return nil, false
		}
		d.failServer(c, "查询域名", err)
		return nil, false
	}
	if !row.Enabled {
		d.fail(c, "域名不存在", 1)
		return nil, false
	}
	return row, true
}

// ---- handlers ----

// domainIndex 对齐 GET /admin-api/domains。
func (d *DomainsDomain) domainIndex(c *gin.Context) {
	rows, err := d.store.DomainsIndex(ctxOf(c))
	if err != nil {
		d.failServer(c, "查询域名", err)
		return
	}
	out := make(jsArr, 0, len(rows))
	for i := range rows {
		out = append(out, presentDomain(&rows[i]))
	}
	d.success(c, out)
}

// domainStore 对齐 POST /admin-api/domains。
func (d *DomainsDomain) domainStore(c *gin.Context) {
	input := jsonInput(c)
	raw, _ := inputString(input["domain"])
	domain := strings.ToLower(strings.TrimSpace(raw))

	if domain == "" || len([]rune(domain)) > 190 {
		d.fail(c, "请填写域名（不超过 190 个字符）", 1)
		return
	}
	if !domainHostValid(domain) {
		d.fail(c, "域名格式不正确", 1)
		return
	}
	exists, err := d.store.DomainExists(ctxOf(c), domain)
	if err != nil {
		d.failServer(c, "检查域名唯一性", err)
		return
	}
	if exists {
		d.fail(c, "域名已存在", 1)
		return
	}

	// 表内尚无 enabled 行时，新行直接为主域
	count, err := d.store.DomainEnabledCount(ctxOf(c))
	if err != nil {
		d.failServer(c, "统计域名", err)
		return
	}
	row, err := d.store.CreateDomain(ctxOf(c), domain, count == 0)
	if err != nil {
		d.failServer(c, "创建域名", err)
		return
	}
	d.success(c, presentDomain(row), "创建成功")
}

// domainSetPrimary 对齐 POST /admin-api/domains/{id}/primary。
func (d *DomainsDomain) domainSetPrimary(c *gin.Context) {
	target, ok := d.loadDomain(c)
	if !ok {
		return
	}
	if err := d.store.SetPrimary(ctxOf(c), target.ID); err != nil {
		d.failServer(c, "设置主域", err)
		return
	}
	row, err := d.store.DomainByID(ctxOf(c), target.ID)
	if err != nil {
		d.failServer(c, "读取主域", err)
		return
	}
	d.success(c, presentDomain(row), "已设为主域")
}

// domainApply 对齐 POST /admin-api/domains/{id}/apply（立即返回 task_id）。
func (d *DomainsDomain) domainApply(c *gin.Context) {
	d.submitAction(c, "apply", "已提交应用")
}

// domainRenew 对齐 POST /admin-api/domains/{id}/renew。
func (d *DomainsDomain) domainRenew(c *gin.Context) {
	d.submitAction(c, "renew", "已提交续签")
}

func (d *DomainsDomain) submitAction(c *gin.Context, action, msg string) {
	target, ok := d.loadDomain(c)
	if !ok {
		return
	}
	// apply/renew 先把该行 ssl_status 置 pending（前端据此显示「配置中」）
	if err := d.store.MarkSSLUpdating(ctxOf(c), target.ID); err != nil {
		d.failServer(c, "标记域名配置中", err)
		return
	}
	task, err := d.tasks.Submit(ctxOf(c), action, target.ID, target.Domain)
	if err != nil {
		d.failServer(c, "提交域名任务", err)
		return
	}
	d.success(c, jo("task_id", task.ID), msg)
}

// domainDestroy 对齐 DELETE /admin-api/domains/{id}。
func (d *DomainsDomain) domainDestroy(c *gin.Context) {
	target, ok := d.loadDomain(c)
	if !ok {
		return
	}
	count, err := d.store.DomainEnabledCount(ctxOf(c))
	if err != nil {
		d.failServer(c, "统计域名", err)
		return
	}
	// 唯一 enabled 行（必然是主域）不能删除
	if target.IsPrimary && count == 1 {
		d.fail(c, "主域名不能删除", 1)
		return
	}

	next, err := d.store.DeleteDomain(ctxOf(c), target.ID)
	if err != nil {
		d.failServer(c, "删除域名", err)
		return
	}
	// 删的是主域：剩余 enabled 行中字典序第一行已在 store 内提为主域
	_ = next

	// 删除后按剩余 enabled 域名重建 nginx，清掉该域 80/443 块
	if _, err := d.tasks.Submit(ctxOf(c), "remove", target.ID, target.Domain); err != nil {
		d.logger.Printf("admin 提交域名移除任务失败: %v", err)
	}
	d.success(c, nil, "已删除")
}

// presentDomain 对齐 DomainController::present。
func presentDomain(row *DomainRow) *jsObj {
	return jo(
		"id", row.ID,
		"domain", row.Domain,
		"is_primary", row.IsPrimary,
		"enabled", row.Enabled,
		"ssl_status", row.SSLStatus,
		"cert_expires_at", nullableISO(row.CertExpiresAt),
		"last_error", nullableStr(row.LastError),
		"created_at", nullableISO(row.CreatedAt),
	)
}
