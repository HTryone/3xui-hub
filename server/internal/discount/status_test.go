package discount

import (
	"testing"
	"time"
)

func TestEffectiveStatus(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	past := now.Add(-time.Hour)
	future := now.Add(time.Hour)

	cases := []struct {
		name      string
		status    string
		expiresAt *time.Time
		usedCount int64
		maxUses   int64
		want      string
	}{
		// 终态原样返回，哪怕时间/次数怎么变都不复活
		{"refreshed 保持", StatusRefreshed, &past, 0, 10, StatusRefreshed},
		{"used_up 保持", StatusUsedUp, &future, 0, 10, StatusUsedUp},
		{"人工 expired 保持", StatusExpired, &future, 0, 10, StatusExpired},

		// active + 截止时间：核心修复点
		{"active 已过截止 → 过期", StatusActive, &past, 0, 10, StatusExpired},
		{"active 恰好到点 → 过期（到点即失效）", StatusActive, &now, 0, 10, StatusExpired},
		{"active 未到截止 → 生效", StatusActive, &future, 0, 10, StatusActive},
		{"active 无截止时间 → 生效", StatusActive, nil, 0, 10, StatusActive},

		// active + 次数（数据兜底，正常库已是 used_up）
		{"active 次数已满 → 用完", StatusActive, nil, 5, 5, StatusUsedUp},
		{"active 次数未满 → 生效", StatusActive, nil, 4, 5, StatusActive},
		// max_uses=0 视为不限，不判用完
		{"max_uses=0 不判用完", StatusActive, nil, 100, 0, StatusActive},

		// 优先级：过期 > 用完（已过期的码就算次数没满也是过期）
		{"过期优先于用完", StatusActive, &past, 5, 5, StatusExpired},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := EffectiveStatus(c.status, c.expiresAt, c.usedCount, c.maxUses, now)
			if got != c.want {
				t.Fatalf("EffectiveStatus(%q, exp=%v, used=%d/%d) = %q, want %q",
					c.status, c.expiresAt, c.usedCount, c.maxUses, got, c.want)
			}
		})
	}
}

// 亚秒精度：库是秒级 DATETIME，now 带纳秒时不能误判成「还没过期」。
func TestEffectiveStatusSubSecond(t *testing.T) {
	exp := time.Date(2026, 11, 12, 18, 25, 0, 0, time.UTC)
	now := exp.Add(500 * time.Millisecond) // 同一秒内，但物理上已过点
	if got := EffectiveStatus(StatusActive, &exp, 0, 10, now); got != StatusExpired {
		t.Fatalf("同一秒内已过点应判过期，得到 %q", got)
	}
}
