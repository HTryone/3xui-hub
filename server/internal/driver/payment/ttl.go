package payment

// 订单支付有效期（全局，一个值管所有渠道）——**唯一定义处**。
//
// 支付链接本身的有效期由网关决定（常见 5 或 10 分钟）；这里是我们自己那本账：
// 「这笔订单还能不能付」。值存在 site_configs（键 TTLMinutesKey），管理端「支付配置」页可改。
//
// 为什么放本包：handler/api（下单算截止时刻、续付判定、列表下发）与 internal/task
// （兜底作废废单）两处都要用同一套键名、缺省值与取值范围 —— 各写一份就是两份口径，
// 早晚会漂移，所以统一收在这里，两边都引它。

import (
	"strconv"
	"strings"
	"time"
)

const (
	// TTLMinutesKey 是全局支付有效期在 site_configs 里的键（单位：分钟）。
	TTLMinutesKey = "payment_ttl_minutes"
	// ContinuePayEnabledKey 是「允许用户继续支付」总开关的 site_configs 键。
	// 支付网关能力不一（有的链接一次性、复用会失效），后台可随时关掉：
	// 关掉后用户端订单不显示「继续支付」按钮，从根上避免点到死链。
	ContinuePayEnabledKey = "payment_continue_pay_enabled"
	// DefaultTTLMinutes 是缺省值：取常见网关窗口的下限。宁可比网关短
	// （到点让用户重新下单），也绝不能被网关长（发出去的就是死链）。
	DefaultTTLMinutes = 5
	// 允许配置的范围（分钟）：下限 1 分钟，上限 2 小时。
	ttlMinMinutes = 1
	ttlMaxMinutes = 120
)

// ParseTTLMinutes 解析配置值：必须是 1~120 的整数，否则视为未配置（调用方退回缺省）。
// 管理端保存与运行时读取共用它，保证「存得进」与「读得出」同一套判据。
func ParseTTLMinutes(raw string) (int, bool) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < ttlMinMinutes || n > ttlMaxMinutes {
		return 0, false
	}
	return n, true
}

// DefaultTTL 是缺省有效期的 Duration 形式。
func DefaultTTL() time.Duration { return time.Duration(DefaultTTLMinutes) * time.Minute }
