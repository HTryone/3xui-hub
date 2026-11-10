package payment

// 支付有效期解析的契约：**存（管理端保存）与读（下单/兜底作废）共用这一套判据**，
// 所以边界必须钉死 —— 否则会出现「面板存得进 0/999，运行时按另一套解释」。

import (
	"testing"
	"time"
)

func TestParseTTLMinutes(t *testing.T) {
	cases := []struct {
		raw   string
		want  int
		valid bool
	}{
		{"5", 5, true},
		{"  10  ", 10, true}, // 首尾空白容忍（面板输入常带空格）
		{"1", 1, true},       // 下限
		{"120", 120, true},   // 上限
		{"0", 0, false},      // 小于下限
		{"121", 0, false},    // 超过上限
		{"-3", 0, false},
		{"", 0, false},      // 未配置
		{"abc", 0, false},   // 非数字
		{"5.5", 0, false},   // 必须整数分钟
		{"5m", 0, false},    // 不接受单位后缀
	}
	for _, tc := range cases {
		got, ok := ParseTTLMinutes(tc.raw)
		if ok != tc.valid || (ok && got != tc.want) {
			t.Fatalf("ParseTTLMinutes(%q) = (%d, %v)，期望 (%d, %v)", tc.raw, got, ok, tc.want, tc.valid)
		}
	}
}

// TestDefaultTTL：缺省值取常见网关窗口的下限（宁可比网关短，也不能比网关长）。
func TestDefaultTTL(t *testing.T) {
	if DefaultTTLMinutes != 5 {
		t.Fatalf("缺省有效期应为 5 分钟，实际 %d", DefaultTTLMinutes)
	}
	if DefaultTTL() != 5*time.Minute {
		t.Fatalf("DefaultTTL() = %v，期望 5m", DefaultTTL())
	}
	if TTLMinutesKey != "payment_ttl_minutes" {
		t.Fatalf("site_configs 键名变动会影响线上已存配置，实际 %q", TTLMinutesKey)
	}
}
