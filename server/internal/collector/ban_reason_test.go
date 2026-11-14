package collector

// 封禁原因分类单测：锁死「到期 ≠ 超量」的归因口径。
//
// 背景 bug：以前 banReasonAt 把「到期 / 当月超量 / 总量超量」都返回同一个 "disable"，
// notifyTrafficExhausted 不分原因一律发「流量用尽」，导致到期用户被误发「流量已用尽」
// （整点到期时还会和「套餐已到期」通知重复发两封）。
//
// 这里的用例把「到期优先」「只超量才算 quota」两条铁律钉死，任何回退都会红。

import (
	"database/sql"
	"testing"
	"time"
)

func atTime(sec int64) sql.NullTime {
	return sql.NullTime{Time: time.Unix(sec, 0).UTC(), Valid: true}
}

func TestBanReasonAt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	past := atTime(now.Unix() - 3600)  // 已过期
	future := atTime(now.Unix() + 3600) // 未过期

	cases := []struct {
		name string
		u    userRow
		want string
	}{
		{
			name: "未到期、未超量 → 正常",
			u:    userRow{PlanID: sql.NullInt64{Int64: 1, Valid: true}, ExpiredAt: future, MonthlyLimit: 100, MonthlyTrafficUsed: 10, TrafficLimit: 100, TrafficUsed: 10},
			want: "",
		},
		{
			name: "只到期（用量没满）→ expired（关键：不能算 quota）",
			u:    userRow{PlanID: sql.NullInt64{Int64: 1, Valid: true}, ExpiredAt: past, MonthlyLimit: 100, MonthlyTrafficUsed: 10, TrafficLimit: 100, TrafficUsed: 10},
			want: banReasonExpired,
		},
		{
			name: "只当月超量 → quota",
			u:    userRow{PlanID: sql.NullInt64{Int64: 1, Valid: true}, ExpiredAt: future, MonthlyLimit: 100, MonthlyTrafficUsed: 100, TrafficLimit: 1000, TrafficUsed: 10},
			want: banReasonQuota,
		},
		{
			name: "只总量超量 → quota",
			u:    userRow{PlanID: sql.NullInt64{Int64: 1, Valid: true}, ExpiredAt: future, MonthlyLimit: 1000, MonthlyTrafficUsed: 10, TrafficLimit: 100, TrafficUsed: 100},
			want: banReasonQuota,
		},
		{
			name: "既到期又超量 → 到期优先（expired，只发「已到期」不发「流量用尽」）",
			u:    userRow{PlanID: sql.NullInt64{Int64: 1, Valid: true}, ExpiredAt: past, MonthlyLimit: 100, MonthlyTrafficUsed: 100, TrafficLimit: 100, TrafficUsed: 100},
			want: banReasonExpired,
		},
		{
			name: "无套餐（plan_id NULL）→ 正常（无套餐不走封禁）",
			u:    userRow{PlanID: sql.NullInt64{Valid: false}, ExpiredAt: future, TrafficLimit: 0, TrafficUsed: 999999},
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := banReasonAt(c.u, now); got != c.want {
				t.Fatalf("banReasonAt=%q，期望 %q", got, c.want)
			}
		})
	}
}

// TestBanReasonOnlyQuotaTriggersExhausted 用一句断言锁住 notifyTrafficExhausted 的闸门口径：
// 只有 banReasonAt == banReasonQuota 才该发「流量用尽」，到期不满足。
func TestBanReasonOnlyQuotaTriggersExhausted(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	expired := userRow{PlanID: sql.NullInt64{Int64: 1, Valid: true}, ExpiredAt: atTime(now.Unix() - 3600), MonthlyLimit: 100, MonthlyTrafficUsed: 0, TrafficLimit: 100, TrafficUsed: 0}
	if banReasonAt(expired, now) == banReasonQuota {
		t.Fatal("到期用户不应被判为 quota —— 否则会误发「流量用尽」")
	}
	quota := userRow{PlanID: sql.NullInt64{Int64: 1, Valid: true}, ExpiredAt: atTime(now.Unix() + 3600), MonthlyLimit: 100, MonthlyTrafficUsed: 100, TrafficLimit: 1000, TrafficUsed: 0}
	if banReasonAt(quota, now) != banReasonQuota {
		t.Fatal("当月超量用户应被判为 quota")
	}
}
