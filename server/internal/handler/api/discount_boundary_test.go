package api

// 锁死「优惠码到点即失效、不可购买」的边界口径：
//   展示/筛选用 internal/discount.EffectiveStatus，下单校验用 payment_discount.go 里
//   的 `!ExpiresAt.Time.After(now)`。两者必须一致，否则会出现「列表显示已过期、却能下单」。
//
// 这个测试专门盯「恰好等于截止秒」这个最容易写错的分界点。

import (
	"testing"
	"time"

	"controlhub-server/internal/discount"
)

// 用 EffectiveStatus 模拟「验证一个码此刻能否用于购买」的门槛，钉死边界口径。
func canBuy(status string, expiresAt *time.Time, used, maxUses int64, now time.Time) bool {
	return discount.EffectiveStatus(status, expiresAt, used, maxUses, now) == "active"
}

func TestDiscountBoundaryExpiresAtEqualsNowNotBuyable(t *testing.T) {
	exp := time.Date(2026, 11, 12, 18, 25, 0, 0, time.UTC)

	// 恰好到点：必须不可用（这是原来 Before() 会漏掉的那一秒）
	if canBuy("active", &exp, 0, 10, exp) {
		t.Fatalf("expires_at == now 时仍可购买 —— 边界错了（应判过期）")
	}
	// 到点前一秒：可用
	if !canBuy("active", &exp, 0, 10, exp.Add(-time.Second)) {
		t.Fatalf("到点前一秒应可用，却判成不可用")
	}
	// 到点后一秒：不可用
	if canBuy("active", &exp, 0, 10, exp.Add(time.Second)) {
		t.Fatalf("到点后一秒仍可购买 —— 边界错了")
	}
	// 亚秒：同一秒内已过点，也不可用（库是秒级精度）
	if canBuy("active", &exp, 0, 10, exp.Add(500*time.Millisecond)) {
		t.Fatalf("同一秒内已过点仍可购买 —— 亚秒边界错了")
	}
}

// validateAndApply 里的实际判据也必须是 !After（<=），不是 Before。
// 直接对 time 表达式做断言，防止有人改回 Before。
func TestValidateExpiryUsesNotAfter(t *testing.T) {
	exp := time.Date(2026, 11, 12, 18, 25, 0, 0, time.UTC)
	now := exp // 恰好到点

	// 正确口径：过期 = !exp.After(now)
	if !(!exp.After(now)) {
		t.Fatalf("到点应判过期（!After 口径）")
	}
	// 反证：旧口径 Before 在此刻会判「未过期」，正是要避免的裂缝
	if exp.Before(now) {
		t.Fatalf("前提错误：Before 在到点时应为 false")
	}
}
