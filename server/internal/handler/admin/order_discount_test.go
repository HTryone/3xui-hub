package admin

// 优惠码软删除的回归：
//  1. 订单列表里「优惠码」列：没用码 → null；用了码 → 码文本；码已软删 → 「码文本（码已删除）」；
//  2. 后台优惠码筛选的 WHERE 必须带 `c.status <> 'deleted'`（已删的码不进列表/计数）。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPresentOrderDiscountCode(t *testing.T) {
	code := "PROTMV4K4KY"

	// 1) 没用码 → discount_code 为 null
	raw := phpEncode(presentOrder(&OrderRow{OrderNo: "N1", Status: "paid"}))
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("解析失败: %v (%s)", err, raw)
	}
	if v, ok := m["discount_code"]; !ok || v != nil {
		t.Fatalf("没用码时 discount_code 应为 null，得到 ok=%v v=%v (%s)", ok, v, raw)
	}

	// 2) 用了码、未删 → 码文本
	raw = phpEncode(presentOrder(&OrderRow{OrderNo: "N2", Status: "paid", DiscountCode: &code}))
	m = map[string]any{}
	_ = json.Unmarshal(raw, &m)
	if m["discount_code"] != code {
		t.Fatalf("未删码应显示 %q，得到 %v", code, m["discount_code"])
	}

	// 3) 用了码、已软删 → 码文本（码已删除）
	raw = phpEncode(presentOrder(&OrderRow{OrderNo: "N3", Status: "paid", DiscountCode: &code, DiscountCodeDeleted: true}))
	m = map[string]any{}
	_ = json.Unmarshal(raw, &m)
	want := code + "（码已删除）"
	if m["discount_code"] != want {
		t.Fatalf("已删码应显示 %q，得到 %v", want, m["discount_code"])
	}
}

func TestDiscountWhereExcludesDeleted(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)

	where, _ := discountWhere(DiscountFilter{}, now)
	if !strings.Contains(where, "c.status <> 'deleted'") {
		t.Fatalf("筛选 WHERE 未排除软删码: %s", where)
	}

	// 带状态筛选时也要保留软删过滤
	st := "active"
	where2, _ := discountWhere(DiscountFilter{Status: &st}, now)
	if !strings.Contains(where2, "c.status <> 'deleted'") {
		t.Fatalf("带状态筛选时丢了软删过滤: %s", where2)
	}
}
