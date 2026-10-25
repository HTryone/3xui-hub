package admin

// 代理订单开通（对齐 Services\ProxyStoreService::completeOrder / doCompleteOrder）。
//
// 口径（与站长确认过的产品规则）：
//   - 流量不归平台管：webshare 建号时把 GB 传给上游 proxy_limit，平台只读回用量展示
//   - webshare 独占：一人一个子账号；续费只改额度/到期，不新开号（省上游子账号额度）
//   - socks 共享：多人共用同一条 socks，不新建任何账号，只挂权益
//   - 库存：新开通占一份（reserve→consume），续费不占；开通失败要把库存还回去
//
// 本文件只承载管理端会用到的「重试开通」路径（ProxyOpsController::retryOrder）。

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// errUpstreamMissing 是上游客户端未配置时的错误（管理端 → 502）。
var errUpstreamMissing = errors.New("上游客户端未配置")

// completeOrder 对齐 ProxyStoreService::completeOrder（幂等 + 互斥）。
//
// 为什么要锁：开号要打上游，最长 21 秒（连接 6s + 总 15s）。网关等不到响应会重推回调，
// 两条回调同时进来各自查「有没有权益」都是「没有」，就会各开一个上游子账号 ——
// 钱只收一次，却白占一个子账号。拿不到锁说明另一条正在处理，直接返回，那条会负责开通。
func (h *Handler) completeOrder(ctx context.Context, order *Order) {
	owner := randomProxyPassword()
	acquired, err := h.store.LockAcquire(ctx, "proxy:provision:"+itoaUint(order.ID), 120, owner)
	if err != nil {
		h.logger.Printf("代理订单开通取锁失败 order=%d: %v", order.ID, err)
		return
	}
	if !acquired {
		h.logger.Printf("代理订单开通已在处理中，跳过重复回调 order_no=%s", order.OrderNo)
		return
	}
	defer func() {
		if err := h.store.LockRelease(ctx, "proxy:provision:"+itoaUint(order.ID), owner); err != nil {
			h.logger.Printf("代理订单开通放锁失败 order=%d: %v", order.ID, err)
		}
	}()

	h.doCompleteOrder(ctx, order)
}

// doCompleteOrder 对齐 doCompleteOrder：失败只记日志、订单保持 paid，由管理员补开。
func (h *Handler) doCompleteOrder(ctx context.Context, order *Order) {
	if order.SKUID == nil {
		h.logger.Printf("代理订单开通失败：规格缺失 order_no=%s", order.OrderNo)
		return
	}
	sku, err := h.store.SKUByID(ctx, *order.SKUID)
	if err != nil || sku == nil {
		h.logger.Printf("代理订单开通失败：规格缺失 order_no=%s", order.OrderNo)
		return
	}
	email, err := h.store.UserEmail(ctx, order.UserID)
	if err != nil && !NoRows(err) {
		h.logger.Printf("代理订单开通失败：查询用户出错 order_no=%s: %v", order.OrderNo, err)
		return
	}
	if email == nil {
		h.logger.Printf("代理订单开通失败：规格或用户缺失 order_no=%s", order.OrderNo)
		return
	}

	// 幂等：同一订单只开通一次。
	exists, err := h.store.EntitlementExistsForOrder(ctx, order.ID)
	if err != nil {
		h.logger.Printf("代理订单开通失败：查询权益出错 order_no=%s: %v", order.OrderNo, err)
		return
	}
	if exists {
		return
	}

	now := h.now().UTC().Truncate(time.Second)
	kind := KindWebshare
	if sku.SourceType != nil && *sku.SourceType == KindSocks {
		kind = KindSocks
	}

	existing, err := h.store.FindRenewableEntitlement(ctx, order.UserID, kind, true, now)
	if err != nil && !NoRows(err) {
		h.logger.Printf("代理订单开通失败：查询可续费权益出错 order_no=%s: %v", order.OrderNo, err)
		return
	}

	days := sku.DurationDays
	if days < 1 {
		days = 1
	}

	var accountID, socksID *uint64
	if kind == KindSocks {
		if sku.SourceID == nil {
			h.logger.Printf("代理订单开通失败：共享 socks 货源不可用 order_no=%s", order.OrderNo)
			return
		}
		sock, sockErr := h.store.SockByID(ctx, *sku.SourceID)
		if sockErr != nil || sock == nil || !sock.IsActive {
			h.logger.Printf("代理订单开通失败：共享 socks 货源不可用 order_no=%s", order.OrderNo)
			return
		}
		socksID = &sock.ID
	} else {
		acc, accErr := h.openOrRenewWebshareAccount(ctx, order, sku, existing, now)
		if accErr != nil {
			// 上游失败：把库存还回去（新开通才占过），订单保持 paid，留日志给管理员补开。
			if !order.ProxyRenewal {
				if relErr := h.store.ReleaseStock(ctx, sku.ID, 1); relErr != nil {
					h.logger.Printf("代理订单开通失败后归还库存出错 order_no=%s: %v", order.OrderNo, relErr)
				}
			}
			h.logger.Printf("代理订单开通失败（用户已付款，需人工补开）order_no=%s user_id=%d sku_id=%d error=%v",
				order.OrderNo, order.UserID, sku.ID, accErr)
			return
		}
		accountID = &acc.ID
	}

	if err := h.upsertEntitlement(ctx, order, sku, kind, accountID, socksID, existing, days, now); err != nil {
		h.logger.Printf("代理订单开通失败：写权益出错 order_no=%s: %v", order.OrderNo, err)
		return
	}

	// 新开通成功：库存占用转已售（续费不占库存）。
	if !order.ProxyRenewal {
		if err := h.store.ConsumeStock(ctx, sku.ID, 1); err != nil {
			h.logger.Printf("代理订单开通后转库存出错 order_no=%s: %v", order.OrderNo, err)
		}
	}
}

// openOrRenewWebshareAccount 对齐 doCompleteOrder 的 webshare 分支。
func (h *Handler) openOrRenewWebshareAccount(
	ctx context.Context, order *Order, sku *SKU, existing *Entitlement, now time.Time,
) (*Account, error) {
	var account *Account
	if existing != nil && existing.AccountID != nil {
		acc, err := h.store.AccountByID(ctx, *existing.AccountID)
		if err != nil && !NoRows(err) {
			return nil, err
		}
		account = acc
	}

	if account != nil {
		// 续费：把额度重置回规格额度、恢复启用、延长到期。
		renewed, err := h.renewAccountForSKU(ctx, account, sku, now)
		if err != nil {
			return nil, err
		}
		return renewed, nil
	}
	return h.openWebshareAccount(ctx, order, sku, now)
}

// openWebshareAccount 对齐 openWebshareAccount：开一个上游子账号并落库。
func (h *Handler) openWebshareAccount(ctx context.Context, order *Order, sku *SKU, now time.Time) (*Account, error) {
	if h.upstream == nil {
		return nil, errUpstreamMissing
	}

	// 货源：sku.source_id 指定 provider，否则取第一个 enabled 的。
	var provider *Provider
	if sku.SourceID != nil {
		p, err := h.store.ProviderByID(ctx, *sku.SourceID)
		if err != nil && !NoRows(err) {
			return nil, err
		}
		provider = p
	}
	if provider == nil {
		providers, err := h.store.Providers(ctx)
		if err != nil {
			return nil, err
		}
		for i := range providers {
			if providers[i].Enabled {
				provider = &providers[i]
				break
			}
		}
	}
	if provider == nil {
		return nil, fmt.Errorf("没有可用的 Webshare 账号（请先在上游里添加并启用）")
	}

	label := "u" + itoaUint(order.UserID) + "-" + sku.Name
	data, err := h.upstream.CreateSubuser(ctx, provider.APIToken, map[string]any{
		"label":            label,
		"proxy_limit":      float64(sku.TrafficGB),
		"max_thread_count": sku.MaxThreadCount,
	})
	if err != nil {
		return nil, err
	}
	upstreamID := jsonStringOf(data["id"])
	if upstreamID == "" {
		return nil, fmt.Errorf("Webshare 建号响应缺少 id")
	}

	respLabel := label
	if v, ok := data["label"].(string); ok {
		respLabel = v
	}
	username := jsonStringOf(data["username"])
	password := jsonStringOf(data["password"])
	limit := float64(sku.TrafficGB)
	if v, ok := data["proxy_limit"]; ok {
		limit = phpNumberOf(v)
	}
	threads := sku.MaxThreadCount
	if v, ok := data["max_thread_count"]; ok {
		threads = int64(phpNumberOf(v))
	}

	userID := order.UserID
	expiry := now.AddDate(0, 0, daysOf(sku))
	acc, err := h.store.CreateAccount(ctx, AccountCreate{
		ProviderID:     provider.ID,
		UpstreamID:     upstreamID,
		UserID:         &userID,
		Label:          respLabel,
		Username:       username,
		Password:       strPtrOrNil(password),
		ProxyLimit:     floatToDecimal(limit, 2),
		MaxThreadCount: threads,
		ProxyCountries: proxyCountriesJSON(data["proxy_countries"]),
		UsedBytes:      0,
		Status:         "active",
		LastSyncedAt:   &now,
	})
	if err != nil {
		return nil, err
	}

	// 商城字段：sku / 流量上限 / 首单 / 到期。
	skuID := sku.ID
	orderID := order.ID
	accountPatch := AccountPatch{
		SKUID:     &skuID,
		OrderID:   &orderID,
		ExpiresAt: &expiry,
	}
	if err := h.store.PatchAccount(ctx, acc.ID, accountPatch); err != nil {
		return nil, err
	}
	if sku.TrafficGB > 0 {
		bytes := sku.TrafficGB * 1024 * 1024 * 1024
		if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{TrafficLimitBytes: &bytes}); err != nil {
			return nil, err
		}
	}
	return h.store.AccountByID(ctx, acc.ID)
}

// renewAccountForSKU 对齐 renewWebshareAccount：额度重置、恢复启用、延长到期。
func (h *Handler) renewAccountForSKU(ctx context.Context, acc *Account, sku *SKU, now time.Time) (*Account, error) {
	if h.upstream == nil {
		return nil, errUpstreamMissing
	}
	provider, err := h.store.ProviderByID(ctx, acc.ProviderID)
	if err != nil {
		return nil, err
	}
	password := randomProxyPassword()
	if _, err := h.upstream.UpdateSubuser(ctx, provider.APIToken, acc.UpstreamID, map[string]any{
		"password":         password,
		"proxy_limit":      float64(sku.TrafficGB),
		"max_thread_count": sku.MaxThreadCount,
	}); err != nil {
		return nil, err
	}

	base := now
	if acc.ExpiresAt != nil && acc.ExpiresAt.After(now) {
		base = acc.ExpiresAt.UTC()
	}
	expiry := base.AddDate(0, 0, daysOf(sku))

	skuID := sku.ID
	limit := floatToDecimal(float64(sku.TrafficGB), 2)
	status := "active"
	if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{
		Password:     &password,
		ProxyLimit:   &limit,
		SKUID:        &skuID,
		ExpiresAt:    &expiry,
		Status:       &status,
		LastSyncedAt: &now,
	}); err != nil {
		return nil, err
	}
	if sku.TrafficGB > 0 {
		bytes := sku.TrafficGB * 1024 * 1024 * 1024
		if err := h.store.PatchAccount(ctx, acc.ID, AccountPatch{TrafficLimitBytes: &bytes}); err != nil {
			return nil, err
		}
	}
	return h.store.AccountByID(ctx, acc.ID)
}

// upsertEntitlement 对齐 upsertEntitlement：续费时延长到期，不新增行。
func (h *Handler) upsertEntitlement(
	ctx context.Context, order *Order, sku *SKU, kind string,
	accountID, socksID *uint64, existing *Entitlement, days int64, now time.Time,
) error {
	if existing != nil {
		base := now
		if existing.ExpiresAt != nil && existing.ExpiresAt.After(now) {
			base = existing.ExpiresAt.UTC()
		}
		expiry := base.AddDate(0, 0, int(days))
		orderID := order.ID
		skuID := sku.ID
		status := KindStatusActive
		patch := EntitlementPatch{
			OrderID:   &orderID,
			SKUID:     &skuID,
			Status:    &status,
			ExpiresAt: &expiry,
		}
		if accountID != nil {
			patch.AccountID = accountID
		}
		if socksID != nil {
			patch.SocksID = socksID
		}
		return h.store.UpdateEntitlement(ctx, existing.ID, patch)
	}

	orderID := order.ID
	expiry := now.AddDate(0, 0, int(days))
	_, err := h.store.CreateEntitlement(ctx, EntitlementCreate{
		UserID:    order.UserID,
		OrderID:   &orderID,
		SKUID:     sku.ID,
		Kind:      kind,
		AccountID: accountID,
		SocksID:   socksID,
		ExpiresAt: &expiry,
		Status:    KindStatusActive,
	})
	return err
}

// daysOf 取规格天数（至少 1 天，对齐 max(1, duration_days)）。
func daysOf(sku *SKU) int {
	if sku.DurationDays < 1 {
		return 1
	}
	return int(sku.DurationDays)
}

// itoaUint 是 uint64 → 十进制字符串。
func itoaUint(v uint64) string { return fmt.Sprintf("%d", v) }
