package admin

// 优惠码「有效状态」在管理端的回归：
//  1. 列表 presentDiscount 输出的 status 必须是现算值（过了截止时间即过期），不能是库里死值；
//  2. 状态筛选走的是「有效状态」CASE，且占位符与参数顺序必须严格对齐（改错就整页筛错或 500）。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPresentDiscountEffectiveStatus(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	cases := []struct {
		name   string
		status string
		exp    *time.Time
		want   string
	}{
		{"过了截止时间仍 active → 显示已过期", "active", &past, "expired"},
		{"未到截止时间 → 生效中", "active", &future, "active"},
		{"人工作废保持 expired", "expired", &future, "expired"},
		{"已用完保持 used_up", "used_up", nil, "used_up"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &DiscountRow{
				ID: 1, Code: "PROTMV4K4KY", Source: "admin",
				Discount: "0.70", MaxUses: 10, UsedCount: 9,
				ExpiresAt: c.exp, Status: c.status,
			}
			raw := phpEncode(presentDiscount(r, now))
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("解析 presentDiscount 输出失败: %v (%s)", err, raw)
			}
			if m["status"] != c.want {
				t.Fatalf("status = %v, want %q (raw=%s)", m["status"], c.want, raw)
			}
		})
	}
}

func TestDiscountWhereStatusUsesEffectiveSQL(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	st := "expired"
	src := "admin"

	where, args := discountWhere(DiscountFilter{Status: &st, Source: &src}, now)

	// 必须用「有效状态」CASE，不能再退回库里那个会漂移的 c.status 死值。
	if !strings.Contains(where, "CASE") || !strings.Contains(where, "c.status <> 'active'") {
		t.Fatalf("状态筛选未走有效状态 CASE: %s", where)
	}
	if strings.Contains(where, "c.status = ?") {
		t.Fatalf("状态筛选仍在读库里的死值 c.status: %s", where)
	}
	// 占位符个数必须等于参数个数，且顺序为 [now, 状态值, 来源值]。
	if got := strings.Count(where, "?"); got != len(args) {
		t.Fatalf("占位符 %d 个但参数 %d 个: %s %v", got, len(args), where, args)
	}
	if len(args) != 3 {
		t.Fatalf("参数应为 3 个，得到 %d: %v", len(args), args)
	}
	tm, ok := args[0].(time.Time)
	if !ok || !tm.Equal(now.UTC()) {
		t.Fatalf("第 1 个参数应是 now(UTC)，得到 %v", args[0])
	}
	if args[1] != "expired" || args[2] != "admin" {
		t.Fatalf("参数顺序错位: %v", args)
	}
}
