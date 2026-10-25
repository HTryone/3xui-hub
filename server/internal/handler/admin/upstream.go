package admin

// 管理端代理上游 · Webshare（对齐 Admin\UpstreamWebshareController + Services\Webshare\ProxyProviderService）。
//
// 站长配自己的 Webshare 账号（可多个），给平台用户开独立子用户。
//
// 安全口径：api_token 与子用户 password 都加密存储，接口一律不回明文；
// provider 只回 has_token，账号只回 username + 用量。唯一的例外是建号 / 停用 /
// 显式 credential 三个动作 —— 上游只在创建响应里给一次凭据，不回就等于白开号。

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ---- provider（站长账号）----

// listProviders 对齐 GET /admin-api/upstream/webshare/providers（id 倒序）。
func (h *Handler) listProviders(c *gin.Context) {
	providers, err := h.store.Providers(ctxOf(c))
	if err != nil {
		h.failServer(c, "查询上游账号", err)
		return
	}
	items := make(jsArr, 0, len(providers))
	for i := range providers {
		summary, err := h.providerSummary(ctxOf(c), &providers[i])
		if err != nil {
			h.failServer(c, "统计上游子账号", err)
			return
		}
		items = append(items, summary)
	}
	h.success(c, jo("items", items))
}

// createProvider 对齐 POST /admin-api/upstream/webshare/providers。
func (h *Handler) createProvider(c *gin.Context) {
	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("name", "required", "string", "max:64")
	v.field("api_token", "required", "string", "max:512")
	v.field("enabled", "sometimes", "boolean")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	name, _ := inputString(input["name"])
	token, _ := inputString(input["api_token"])
	enabled := true
	if val, ok := input["enabled"]; ok && val != nil {
		enabled = inputBool(val)
	}

	p, err := h.store.CreateProvider(ctxOf(c), name, token, enabled)
	if err != nil {
		h.failServer(c, "创建上游账号", err)
		return
	}
	summary, err := h.providerSummary(ctxOf(c), p)
	if err != nil {
		h.failServer(c, "统计上游子账号", err)
		return
	}
	h.success(c, summary, "已创建")
}

// updateProvider 对齐 PUT /admin-api/upstream/webshare/providers/{provider}。
func (h *Handler) updateProvider(c *gin.Context) {
	id, ok := h.paramID(c, "provider")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	if _, err := h.store.ProviderByID(ctxOf(c), id); err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询上游账号", err)
		return
	}

	input := jsonInput(c)
	v := newValidator(ctxOf(c), input)
	v.field("name", "sometimes", "string", "max:64")
	v.field("api_token", "sometimes", "string", "max:512")
	v.field("enabled", "sometimes", "boolean")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	patch := ProviderPatch{}
	if val, ok := input["name"]; ok && val != nil {
		s, _ := inputString(val)
		patch.Name = &s
	}
	if val, ok := input["api_token"]; ok && val != nil {
		s, _ := inputString(val)
		patch.APIToken = &s
	}
	if val, ok := input["enabled"]; ok && val != nil {
		b := inputBool(val)
		patch.Enabled = &b
	}

	p, err := h.store.PatchProvider(ctxOf(c), id, patch)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "保存上游账号", err)
		return
	}
	summary, err := h.providerSummary(ctxOf(c), p)
	if err != nil {
		h.failServer(c, "统计上游子账号", err)
		return
	}
	h.success(c, summary, "已更新")
}

// deleteProvider 对齐 DELETE /admin-api/upstream/webshare/providers/{provider}。
//
// 子账号随 provider 级联删除（只删本地记录，不动 Webshare 侧子用户）。
func (h *Handler) deleteProvider(c *gin.Context) {
	id, ok := h.paramID(c, "provider")
	if !ok {
		h.notFoundRoute(c)
		return
	}
	if _, err := h.store.ProviderByID(ctxOf(c), id); err != nil {
		if h.noRowsToNotFound(c, err) {
			return
		}
		h.failServer(c, "查询上游账号", err)
		return
	}
	if err := h.store.DeleteProvider(ctxOf(c), id); err != nil {
		h.failServer(c, "删除上游账号", err)
		return
	}
	h.success(c, nil, "已删除")
}

// testProvider 对齐 POST /admin-api/upstream/webshare/providers/{provider}/test。
//
// 成功写 last_status=success；失败写 failed + 原因，并把异常继续抛给调用方。
func (h *Handler) testProvider(c *gin.Context) {
	p, ok := h.loadProvider(c, "provider")
	if !ok {
		return
	}
	if h.upstream == nil {
		h.fail(c, "上游客户端未配置", 502)
		return
	}

	subusers, err := h.upstream.ListSubusers(ctxOf(c), p.APIToken)
	if err != nil {
		msg := err.Error()
		h.markProviderResult(ctxOf(c), p, false, &msg)
		h.fail(c, msg, 502)
		return
	}
	h.markProviderResult(ctxOf(c), p, true, nil)
	h.success(c, jo("subuser_count", len(subusers)), "连接正常")
}

// upstreamPlans 对齐 GET /admin-api/upstream/webshare/providers/{provider}/plans。
//
// 只回 status=active 的套餐：实测账号里有 cancelled 套餐，它们的 subusers_available
// 仍是假数字（建号必然失败），原样展示会误导管理员按这个数去开号。
func (h *Handler) upstreamPlans(c *gin.Context) {
	p, ok := h.loadProvider(c, "provider")
	if !ok {
		return
	}
	if h.upstream == nil {
		h.fail(c, "上游客户端未配置", 502)
		return
	}

	plans, err := h.upstream.ListPlans(ctxOf(c), p.APIToken)
	if err != nil {
		h.fail(c, err.Error(), 502)
		return
	}
	h.markProviderResult(ctxOf(c), p, true, nil)

	items := make(jsArr, 0, len(plans))
	for _, plan := range plans {
		status := "active"
		if v, ok := plan["status"]; ok && v != nil {
			if s, isStr := v.(string); isStr {
				status = s
			}
		}
		if status != "active" {
			continue
		}
		items = append(items, jo(
			"id", plan["id"],
			"status", plan["status"],
			"bandwidth_limit", plan["bandwidth_limit"],
			"proxy_count", plan["proxy_count"],
			"proxy_type", plan["proxy_type"],
			"countries", planCountries(plan["proxy_countries"]),
			"subusers_available", plan["subusers_available"],
			"monthly_price", plan["monthly_price"],
			"yearly_price", plan["yearly_price"],
		))
	}
	h.success(c, jo("items", items))
}

// ---- account（子用户）----

// listProviderAccounts 对齐 GET .../providers/{provider}/accounts（id 倒序）。
func (h *Handler) listProviderAccounts(c *gin.Context) {
	p, ok := h.loadProvider(c, "provider")
	if !ok {
		return
	}
	accounts, err := h.store.ProviderAccounts(ctxOf(c), p.ID)
	if err != nil {
		h.failServer(c, "查询上游子账号", err)
		return
	}
	items := make(jsArr, 0, len(accounts))
	for i := range accounts {
		items = append(items, h.accountSummary(&accounts[i]))
	}
	h.success(c, jo("items", items))
}

// createProviderAccount 对齐 POST .../providers/{provider}/accounts。
//
// 初始密码只在这**一次**响应里回给管理员：上游同样只在创建响应里给一次
// （GET /subuser/{id}/ 不返回凭据）。不回的话管理员拿不到凭据，开号等于白开。
func (h *Handler) createProviderAccount(c *gin.Context) {
	p, ok := h.loadProvider(c, "provider")
	if !ok {
		return
	}
	if h.upstream == nil {
		h.fail(c, "上游客户端未配置", 502)
		return
	}

	input := jsonInput(c)
	v := newValidator(ctxOf(c), input).
		existsChecker(h.existsChecker())
	v.field("label", "required", "string", "max:64")
	v.field("proxy_limit", "sometimes", "numeric", "min:0")
	v.field("max_thread_count", "sometimes", "integer", "min:0")
	v.field("user_id", "sometimes", "nullable", "integer", "exists:users,id")
	v.field("proxy_countries", "sometimes", "nullable", "array")
	if !v.passes() {
		h.failValidation(c, v)
		return
	}

	label, _ := inputString(input["label"])
	proxyLimit := 0.0
	if val, ok := input["proxy_limit"]; ok && val != nil {
		proxyLimit = phpNumberOf(val)
	}
	maxThreads := int64(0)
	if val, ok := input["max_thread_count"]; ok && val != nil {
		maxThreads = inputInt(val)
	}

	payload := map[string]any{
		"label":            label,
		"proxy_limit":      proxyLimit,
		"max_thread_count": maxThreads,
	}
	data, err := h.upstream.CreateSubuser(ctxOf(c), p.APIToken, payload)
	if err != nil {
		h.fail(c, err.Error(), 502)
		return
	}

	upstreamID := jsonStringOf(data["id"])
	if upstreamID == "" {
		h.fail(c, "Webshare 建号响应缺少 id", 502)
		return
	}
	h.markProviderResult(ctxOf(c), p, true, nil)

	respLabel := label
	if v, ok := data["label"]; ok {
		if s, isStr := v.(string); isStr {
			respLabel = s
		}
	}
	respUsername := jsonStringOf(data["username"])
	password := jsonStringOf(data["password"])
	respLimit := proxyLimit
	if v, ok := data["proxy_limit"]; ok {
		respLimit = phpNumberOf(v)
	}
	respThreads := maxThreads
	if v, ok := data["max_thread_count"]; ok {
		respThreads = int64(phpNumberOf(v))
	}

	var userID *uint64
	if val, ok := input["user_id"]; ok && val != nil {
		id := uint64(inputInt(val))
		userID = &id
	}

	countries := proxyCountriesJSON(input["proxy_countries"])
	if countries == nil {
		countries = proxyCountriesJSON(data["proxy_countries"])
	}

	now := h.now().UTC().Truncate(time.Second)
	acc, err := h.store.CreateAccount(ctxOf(c), AccountCreate{
		ProviderID:     p.ID,
		UpstreamID:     upstreamID,
		UserID:         userID,
		Label:          respLabel,
		Username:       respUsername,
		Password:       strPtrOrNil(password),
		ProxyLimit:     floatToDecimal(respLimit, 2),
		MaxThreadCount: respThreads,
		ProxyCountries: countries,
		UsedBytes:      0,
		Status:         "active",
		LastSyncedAt:   &now,
	})
	if err != nil {
		h.failServer(c, "写入上游子账号", err)
		return
	}

	summary := h.accountSummary(acc)
	summary.put("initial_password", password)
	h.success(c, summary, "已创建")
}

// disableUpstreamAccount 对齐 POST /admin-api/upstream/webshare/accounts/{account}/disable。
//
// Webshare 没有启用/禁用开关：停用只能改随机密码把旧凭据废掉，并把 proxy_limit
// 压到 0.01 GB（不是 0 —— 0 表示不限量）。新密码同样只在这**一次**响应里给。
func (h *Handler) disableUpstreamAccount(c *gin.Context) {
	acc, ok := h.loadAccount(c, "account")
	if !ok {
		return
	}
	updated, err := h.disableAccount(ctxOf(c), acc)
	if err != nil {
		h.fail(c, err.Error(), 502)
		return
	}
	summary := h.accountSummary(updated)
	summary.put("new_password", derefString(updated.Password))
	h.success(c, summary, "已停用")
}

// deleteUpstreamAccount 对齐 DELETE /admin-api/upstream/webshare/accounts/{account}。
func (h *Handler) deleteUpstreamAccount(c *gin.Context) {
	acc, ok := h.loadAccount(c, "account")
	if !ok {
		return
	}
	if err := h.deleteAccount(ctxOf(c), acc); err != nil {
		h.fail(c, err.Error(), 502)
		return
	}
	h.success(c, nil, "已删除")
}

// pullUpstreamUsage 对齐 POST /admin-api/upstream/webshare/accounts/{account}/usage。
func (h *Handler) pullUpstreamUsage(c *gin.Context) {
	acc, ok := h.loadAccount(c, "account")
	if !ok {
		return
	}
	updated, err := h.pullUsage(ctxOf(c), acc)
	if err != nil {
		h.fail(c, err.Error(), 502)
		return
	}
	h.success(c, h.accountSummary(updated))
}

// pullUsageBatch 对齐 POST .../providers/{provider}/usage（容错：单条失败不影响其它）。
func (h *Handler) pullUsageBatch(c *gin.Context) {
	p, ok := h.loadProvider(c, "provider")
	if !ok {
		return
	}
	accounts, err := h.store.ProviderActiveAccounts(ctxOf(c), p.ID)
	if err != nil {
		h.failServer(c, "查询上游子账号", err)
		return
	}

	updated := 0
	failed := jo()
	for i := range accounts {
		if _, err := h.pullUsage(ctxOf(c), &accounts[i]); err != nil {
			failed.put(strconv.FormatUint(accounts[i].ID, 10), err.Error())
			continue
		}
		updated++
	}
	h.success(c, jo("updated", updated, "failed", failed))
}

// upstreamCredential 对齐 POST /admin-api/upstream/webshare/accounts/{account}/credential。
//
// 读本地保存的子用户凭据（管理员显式点击才调）。从上游同步来的记录没有这份副本 → 404。
// 用 POST 而不是 GET：明文凭据不该进浏览器缓存/历史/Referer。
func (h *Handler) upstreamCredential(c *gin.Context) {
	acc, ok := h.loadAccount(c, "account")
	if !ok {
		return
	}
	password := derefString(acc.Password)
	if password == "" {
		h.fail(c, "本地没有该子用户的密码（从上游同步来的记录不带凭据），需要停用重置后才能拿到新密码", 404)
		return
	}
	h.success(c, jo(
		"id", acc.ID,
		"label", acc.Label,
		"username", acc.Username,
		"password", password,
	))
}

// providerProxies 对齐 GET .../providers/{provider}/proxies?plan_id=。
//
// 只回 IP/端口/国家/城市/ASN/可用状态 —— 上游 results 里带**主账号**代理凭据，
// 逐条白名单化后绝不出接口（用户认证用的是子用户自己的账号密码）。
func (h *Handler) providerProxies(c *gin.Context) {
	p, ok := h.loadProvider(c, "provider")
	if !ok {
		return
	}
	if h.upstream == nil {
		h.fail(c, "上游客户端未配置", 502)
		return
	}

	query := map[string]string{"mode": "direct", "page_size": "100"}
	if raw := queryString(c, "plan_id"); raw != nil {
		query["plan_id"] = *raw
	}
	data, err := h.upstream.ListProxies(ctxOf(c), p.APIToken, query)
	if err != nil {
		h.fail(c, err.Error(), 502)
		return
	}

	results, _ := data["results"].([]any)
	items := make(jsArr, 0, len(results))
	for _, raw := range results {
		item, _ := raw.(map[string]any)
		items = append(items, jo(
			"id", item["id"],
			"address", mapString(item, "proxy_address"),
			"port", item["port"],
			"country", mapString(item, "country_code"),
			"city", mapString(item, "city_name"),
			"asn", mapString(item, "asn_name"),
			"valid", inputBool(item["valid"]),
			"last_verification", item["last_verification"],
		))
	}
	total := int64(len(items))
	if v, ok := data["count"]; ok {
		total = int64(phpNumberOf(v))
	}
	h.success(c, jo("items", items, "total", total))
}

// syncAccounts 对齐 POST .../providers/{provider}/accounts/sync。
//
// 上游只读接口不返回 username/password（凭据只在创建那一刻给一次），
// 所以同步回来的记录没有凭据；已有记录的 status 不覆盖（避免把 disabled 拉回 active）。
func (h *Handler) syncAccounts(c *gin.Context) {
	p, ok := h.loadProvider(c, "provider")
	if !ok {
		return
	}
	if h.upstream == nil {
		h.fail(c, "上游客户端未配置", 502)
		return
	}

	subusers, err := h.upstream.ListSubusers(ctxOf(c), p.APIToken)
	if err != nil {
		h.fail(c, err.Error(), 502)
		return
	}

	created, updated := 0, 0
	for _, item := range subusers {
		upstreamID := jsonStringOf(item["id"])
		if upstreamID == "" {
			continue
		}
		now := h.now().UTC().Truncate(time.Second)
		usedBytes := int64(0)
		if stats, ok := item["aggregate_stats"].(map[string]any); ok {
			usedBytes = int64(phpNumberOf(stats["bandwidth_total"]))
		}
		label := jsonStringOf(item["label"])
		limit := floatToDecimal(phpNumberOf(item["proxy_limit"]), 2)
		threads := int64(phpNumberOf(item["max_thread_count"]))
		countries := proxyCountriesJSON(item["proxy_countries"])

		patch := AccountPatch{
			ProxyLimit:     &limit,
			MaxThreadCount: &threads,
			UsedBytes:      &usedBytes,
			LastSyncedAt:   &now,
		}
		if countries != nil {
			patch.ProxyCountries = countries
		}

		wasCreated, err := h.store.UpsertAccountByUpstream(ctxOf(c), p.ID, upstreamID, patch, label, "active")
		if err != nil {
			h.failServer(c, "同步上游子账号", err)
			return
		}
		if wasCreated {
			created++
		} else {
			updated++
		}
	}

	h.markProviderResult(ctxOf(c), p, true, nil)
	h.success(c, jo("created", created, "updated", updated, "total", len(subusers)), "已同步")
}

// ---- 内部：ProxyProviderService 等价实现 ----

// providerSummary 对齐 ProxyProviderService::providerSummary。
func (h *Handler) providerSummary(ctx context.Context, p *Provider) (*jsObj, error) {
	count, err := h.store.ProviderAccountCount(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	return jo(
		"id", p.ID,
		"name", p.Name,
		"driver", p.Driver,
		"enabled", p.Enabled,
		"last_status", p.LastStatus,
		"last_error", nullableStr(p.LastError),
		"last_checked_at", nullableISO(p.LastCheckedAt),
		// 只回布尔：token 明文/密文都不出接口
		"has_token", p.APIToken != "",
		"account_count", count,
	), nil
}

// accountSummary 对齐 ProxyProviderService::accountSummary。
func (h *Handler) accountSummary(a *Account) *jsObj {
	return jo(
		"id", a.ID,
		"provider_id", a.ProviderID,
		"upstream_id", a.UpstreamID,
		"user_id", nullableUint(a.UserID),
		"label", a.Label,
		"username", a.Username,
		// password 走 $hidden，这里也不展开
		"proxy_limit", phpFloatOf(a.ProxyLimit),
		"max_thread_count", a.MaxThreadCount,
		"proxy_countries", jsonArrayOrEmpty(a.ProxyCountries),
		"used_bytes", a.UsedBytes,
		"status", a.Status,
		// 代理商城的号带这两项：卖家用来核对「谁买的、什么时候到期」
		"sku_id", nullableUint(a.SKUID),
		"expires_at", nullableISO(a.ExpiresAt),
		"last_synced_at", nullableISO(a.LastSyncedAt),
	)
}

// markProviderResult 对齐 markProviderResult：写 last_status / last_error / last_checked_at。
func (h *Handler) markProviderResult(ctx context.Context, p *Provider, ok bool, errMsg *string) {
	status := "failed"
	if ok {
		status = "success"
	}
	now := h.now().UTC().Truncate(time.Second)
	patch := ProviderPatch{LastStatus: &status, CheckedAt: &now}
	if ok {
		patch.LastError = nil
	} else if errMsg != nil {
		trimmed := truncateRunes(*errMsg, 250)
		patch.LastError = &trimmed
	}
	if _, err := h.store.PatchProvider(ctx, p.ID, patch); err != nil {
		h.logger.Printf("admin 记录上游检查结果失败 provider=%d: %v", p.ID, err)
	}
}

// disableAccount 对齐 ProxyProviderService::disableAccount。
func (h *Handler) disableAccount(ctx context.Context, acc *Account) (*Account, error) {
	if h.upstream == nil {
		return nil, errors.New("上游客户端未配置")
	}
	provider, err := h.store.ProviderByID(ctx, acc.ProviderID)
	if err != nil {
		return nil, err
	}
	newPassword := randomProxyPassword()
	if _, err := h.upstream.UpdateSubuser(ctx, provider.APIToken, acc.UpstreamID, map[string]any{
		"password":    newPassword,
		"proxy_limit": parseFloatSafe(DisabledProxyLimit),
	}); err != nil {
		return nil, err
	}

	now := h.now().UTC().Truncate(time.Second)
	status := "disabled"
	limit := DisabledProxyLimit
	if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{
		Password:     &newPassword,
		ProxyLimit:   &limit,
		Status:       &status,
		LastSyncedAt: &now,
	}); err != nil {
		return nil, err
	}
	return h.store.AccountByID(ctx, acc.ID)
}

// deleteAccount 对齐 ProxyProviderService::deleteAccount（上游 404 也把本地清掉）。
func (h *Handler) deleteAccount(ctx context.Context, acc *Account) error {
	if h.upstream == nil {
		return errors.New("上游客户端未配置")
	}
	provider, err := h.store.ProviderByID(ctx, acc.ProviderID)
	if err != nil {
		return err
	}
	if err := h.upstream.DeleteSubuser(ctx, provider.APIToken, acc.UpstreamID); err != nil {
		var we *WebshareError
		if !errors.As(err, &we) || we.Status != 404 {
			return err
		}
	}
	return h.store.DeleteAccount(ctx, acc.ID)
}

// pullUsage 对齐 ProxyProviderService::pullUsage：落 aggregate_stats.bandwidth_total。
func (h *Handler) pullUsage(ctx context.Context, acc *Account) (*Account, error) {
	if h.upstream == nil {
		return nil, errors.New("上游客户端未配置")
	}
	provider, err := h.store.ProviderByID(ctx, acc.ProviderID)
	if err != nil {
		return nil, err
	}
	data, err := h.upstream.GetSubuser(ctx, provider.APIToken, acc.UpstreamID)
	if err != nil {
		return nil, err
	}
	used := int64(0)
	if stats, ok := data["aggregate_stats"].(map[string]any); ok {
		used = int64(phpNumberOf(stats["bandwidth_total"]))
	}
	now := h.now().UTC().Truncate(time.Second)
	if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{
		UsedBytes:    &used,
		LastSyncedAt: &now,
	}); err != nil {
		return nil, err
	}
	return h.store.AccountByID(ctx, acc.ID)
}

// randomProxyPassword 对齐 ProxyProviderService::randomPassword：
// base64 里换掉 +/ 为 Aa 再裁掉 '='，只用 URL 安全子集。
func randomProxyPassword() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("admin: 生成代理密码随机数失败: " + err.Error())
	}
	s := base64.StdEncoding.EncodeToString(b)
	s = strings.NewReplacer("+", "A", "/", "a").Replace(s)
	return strings.TrimRight(s, "=")
}

// ---- 小工具 ----

// loadProvider 解析并加载 {provider}（不存在 → 404 路由文案）。
func (h *Handler) loadProvider(c *gin.Context, param string) (*Provider, bool) {
	id, ok := h.paramID(c, param)
	if !ok {
		h.notFoundRoute(c)
		return nil, false
	}
	p, err := h.store.ProviderByID(ctxOf(c), id)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return nil, false
		}
		h.failServer(c, "查询上游账号", err)
		return nil, false
	}
	return p, true
}

// loadAccount 解析并加载 {account}。
func (h *Handler) loadAccount(c *gin.Context, param string) (*Account, bool) {
	id, ok := h.paramID(c, param)
	if !ok {
		h.notFoundRoute(c)
		return nil, false
	}
	a, err := h.store.AccountByID(ctxOf(c), id)
	if err != nil {
		if h.noRowsToNotFound(c, err) {
			return nil, false
		}
		h.failServer(c, "查询上游子账号", err)
		return nil, false
	}
	return a, true
}

// jsonStringOf 对齐 PHP 的 (string) 转换（数字 id → 十进制字符串）。
func jsonStringOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case float64:
		// upstream id 可能是数字；PHP 的 (string) 会给出 "115781" 而不是 "1.15781e+05"。
		return strconv.FormatFloat(t, 'f', -1, 64)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case jsonNumber:
		return string(t)
	}
	return ""
}

// mapString 取 map 里的字符串（不存在 → 空串，对齐 PHP 的 ?? ”）。
func mapString(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	return jsonStringOf(m[key])
}

// planCountries 输出套餐的国家池（缺失 → 空数组，PHP 里是 []）。
func planCountries(v any) any {
	return proxyCountriesValue(v)
}

// proxyCountriesValue 把上游的 JSON 国家池转成输出口径。
func proxyCountriesValue(v any) any {
	obj, ok := v.(map[string]any)
	if !ok {
		return jsArr{}
	}
	out := jo()
	for _, k := range sortedKeys(obj) {
		out.put(k, obj[k])
	}
	return out
}

// proxyCountriesJSON 把国家池转成入库 JSON 文本（nil = 不写）。
func proxyCountriesJSON(v any) *string {
	obj, ok := v.(map[string]any)
	if !ok || len(obj) == 0 {
		// 空数组也要落库（"[]"），与 PHP 的 cast 行为一致；null 输入则保持 nil。
		if list, isList := v.([]any); isList && len(list) == 0 {
			s := "[]"
			return &s
		}
		return nil
	}
	s := string(phpEncode(proxyCountriesValue(obj)))
	return &s
}

// jsonArrayOrEmpty 输出账号的 proxy_countries（null → []，与 PHP array cast 一致）。
func jsonArrayOrEmpty(raw *string) any {
	if raw == nil || *raw == "" {
		return jsArr{}
	}
	dec := newJSONDecoder(*raw)
	var v any
	if err := dec.Decode(&v); err != nil {
		return jsArr{}
	}
	if list, ok := v.([]any); ok {
		return jsArr(list)
	}
	if obj, ok := v.(map[string]any); ok {
		out := jo()
		for _, k := range sortedKeys(obj) {
			out.put(k, obj[k])
		}
		return out
	}
	return jsArr{}
}

// floatToDecimal 把 float 规范成 decimal 字符串（保留 scale 位）。
func floatToDecimal(v float64, scale int) string {
	return strconv.FormatFloat(v, 'f', scale, 64)
}

// strPtrOrNil 空串 → nil（PHP 里空密码存 null）。
func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// derefString 解引用字符串指针（nil → 空串）。
func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
