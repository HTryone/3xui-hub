package api

// 代理订单开通（P8d）：对齐 ProxyStoreService::completeOrder。
//
// 支付成功后写 proxy_entitlements、管 stock；webshare 独占账号的上游开通
// 依赖 webshare API（P8d admin 侧），未接通时返回明确错误，不再静默跳过。

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// proxyCompleter 是 ProxyOrderCompleter 的真实实现。
type proxyCompleter struct {
	db     *sql.DB
	logger *log.Logger
	now    func() time.Time
}

// NewProxyCompleter 构造代理订单开通服务。
func NewProxyCompleter(db *sql.DB, logger *log.Logger) ProxyOrderCompleter {
	if logger == nil {
		logger = log.Default()
	}
	return &proxyCompleter{db: db, logger: logger, now: time.Now}
}

// CompleteOrder 对齐 ProxyStoreService::completeOrder。
func (p *proxyCompleter) CompleteOrder(ctx context.Context, order *Order) error {
	if order == nil || !order.IsProxyOrder() {
		return nil
	}

	// 幂等：同一订单只开通一次
	var count int64
	if err := p.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM proxy_entitlements WHERE order_id = ?", order.ID).Scan(&count); err != nil {
		return fmt.Errorf("查询已有权益失败: %w", err)
	}
	if count > 0 {
		return nil
	}

	if !order.ProxySkuID.Valid {
		return fmt.Errorf("代理订单缺少规格（proxy_sku_id 为空）order_no=%s", order.OrderNo)
	}
	skuID := uint64(order.ProxySkuID.Int64)

	// 读 SKU
	var sourceType string
	var sourceID sql.NullInt64
	var durationDays int64
	err := p.db.QueryRowContext(ctx,
		`SELECT source_type, source_id, duration_days FROM proxy_skus WHERE id = ?`, skuID).
		Scan(&sourceType, &sourceID, &durationDays)
	if err == sql.ErrNoRows {
		return fmt.Errorf("规格不存在 sku_id=%d", skuID)
	}
	if err != nil {
		return fmt.Errorf("读规格失败 sku_id=%d: %w", skuID, err)
	}

	now := p.now().UTC()
	var expiresAt time.Time
	if durationDays > 0 {
		expiresAt = now.AddDate(0, 0, int(durationDays))
	}

	// 查已有可续费权益（同 user + 同 kind，active/expired/disabled）
	var existingID sql.NullInt64
	var existingExpires sql.NullTime
	var existingAccountID sql.NullInt64
	p.db.QueryRowContext(ctx,
		`SELECT id, expires_at, account_id FROM proxy_entitlements
		 WHERE user_id = ? AND sku_id = ? AND status IN ('active','expired','disabled')
		 ORDER BY id DESC LIMIT 1`, order.UserID, skuID).
		Scan(&existingID, &existingExpires, &existingAccountID)

	switch sourceType {
	case "socks":
		if !sourceID.Valid {
			return fmt.Errorf("socks 规格缺少货源 sku_id=%d", skuID)
		}
		// 验证 socks 货源可用
		var isActive bool
		err := p.db.QueryRowContext(ctx,
			"SELECT is_active FROM proxy_socks WHERE id = ?", sourceID.Int64).Scan(&isActive)
		if err == sql.ErrNoRows || !isActive {
			return fmt.Errorf("共享 socks 货源不可用 sku_id=%d socks_id=%d", skuID, sourceID.Int64)
		}
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("读 socks 货源失败: %w", err)
		}

		if existingID.Valid {
			// 续费：延长到期
			newExpires := expiresAt
			if existingExpires.Valid && existingExpires.Time.After(now) {
				newExpires = existingExpires.Time.AddDate(0, 0, int(durationDays))
			}
			_, err := p.db.ExecContext(ctx,
				`UPDATE proxy_entitlements
				 SET order_id = ?, sku_id = ?, status = 'active', expires_at = ?
				 WHERE id = ?`,
				order.ID, skuID, newExpires, existingID.Int64)
			return err
		}
		_, err = p.db.ExecContext(ctx,
			`INSERT INTO proxy_entitlements (user_id, order_id, sku_id, kind, account_id, socks_id, expires_at, status, created_at, updated_at)
			 VALUES (?, ?, ?, 'socks', NULL, ?, ?, 'active', ?, ?)`,
			order.UserID, order.ID, skuID, sourceID.Int64, expiresAt, now, now)
		if err != nil {
			p.releaseStock(ctx, skuID, order)
			return fmt.Errorf("创建 socks 权益失败: %w", err)
		}
		p.consumeStock(ctx, skuID, order)
		return nil

	default: // webshare
		if existingID.Valid && existingAccountID.Valid {
			// 续费 webshare：延长到期（上游账号续费依赖 webshare API，此处仅延时）
			newExpires := expiresAt
			if existingExpires.Valid && existingExpires.Time.After(now) {
				newExpires = existingExpires.Time.AddDate(0, 0, int(durationDays))
			}
			_, err := p.db.ExecContext(ctx,
				`UPDATE proxy_entitlements
				 SET order_id = ?, sku_id = ?, status = 'active', expires_at = ?
				 WHERE id = ?`,
				order.ID, skuID, newExpires, existingID.Int64)
			if err != nil {
				return fmt.Errorf("续费 webshare 权益失败: %w", err)
			}
			p.logger.Printf("[proxy] webshare 权益已续期 order_no=%s（上游额度重置需手动或等维护任务）", order.OrderNo)
			p.consumeStock(ctx, skuID, order)
			return nil
		}

		// 新开 webshare 账号需要调上游 API（webshare.io），此处返回明确错误。
		// PHP 的 catch 也是记日志 + 订单保持 paid，由管理员补开。
		p.releaseStock(ctx, skuID, order)
		return fmt.Errorf("webshare 账号开通需上游 API（当前未配置 webshare client），请管理员补开 order_no=%s sku_id=%d", order.OrderNo, skuID)
	}
}

// releaseStock 回退库存（仅新购，续费不占）。
func (p *proxyCompleter) releaseStock(ctx context.Context, skuID uint64, order *Order) {
	// TODO: 订单是否续费需要 proxy_renewal 字段（当前 Order 结构未包含），
	// 先按新购处理 —— releaseStock 是防御性回退，多退不会造成资损。
	_, _ = p.db.ExecContext(ctx,
		`UPDATE proxy_skus SET stock_locked = GREATEST(stock_locked - 1, 0) WHERE id = ?`, skuID)
}

// consumeStock 库存占用转已售。
func (p *proxyCompleter) consumeStock(ctx context.Context, skuID uint64, order *Order) {
	_, _ = p.db.ExecContext(ctx,
		`UPDATE proxy_skus SET stock_locked = GREATEST(stock_locked - 1, 0), stock_sold = stock_sold + 1 WHERE id = ?`, skuID)
}
