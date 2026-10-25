package payment

import (
	"errors"
	"fmt"
	"strings"
)

// 金额一律以「十进制字符串」在进程内流转（contracts/data-contract.md §3）：
// MySQL decimal(10,2) 读出来是什么就传什么，任何一步都**不转 float64** ——
// 0.1+0.2 之类的二进制误差在支付链路里会变成「少收/多收一分」。
//
// 需要比较或运算时转成「分」（int64），这是精确的整数表示。

// ErrMoney 是金额串无法解析时的错误。
var ErrMoney = errors.New("payment: 金额格式非法")

// ParseCents 把 "98.5" / "98.50" / "-3" / "0.00" 转成分（int64）。
//
// 只接受最多两位小数（decimal(10,2) 的定义）；三位及以上直接报错，不四舍五入 ——
// 静默舍入会把网关返回的 "98.505" 悄悄变成 "98.51"，对账时无法解释。
func ParseCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrMoney
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return 0, ErrMoney
	}

	intPart := s
	fracPart := ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return 0, ErrMoney
	}
	if len(fracPart) > 2 {
		return 0, fmt.Errorf("%w: %q 小数超过两位", ErrMoney, s)
	}

	var cents int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("%w: %q", ErrMoney, s)
		}
		cents = cents*10 + int64(c-'0')
		if cents > 1<<62 {
			return 0, fmt.Errorf("%w: %q 超出范围", ErrMoney, s)
		}
	}
	cents *= 100
	if fracPart != "" {
		cents += int64(fracPart[0]-'0') * 10
		if len(fracPart) == 2 {
			cents += int64(fracPart[1] - '0')
		}
	}
	if neg {
		cents = -cents
	}
	return cents, nil
}

// FormatCents 输出两位小数的金额串（等价 PHP number_format($x, 2, '.', ”)）。
func FormatCents(cents int64) string {
	neg := cents < 0
	if neg {
		cents = -cents
	}
	s := fmt.Sprintf("%d.%02d", cents/100, cents%100)
	if neg {
		return "-" + s
	}
	return s
}

// NormalizeMoney 对齐 AbstractPaymentDriver::money()：把任意金额串规范成两位小数。
// 解析不了时原样返回（由调用方决定报错口径，别在这里静默变成 0）。
func NormalizeMoney(s string) string {
	cents, err := ParseCents(s)
	if err != nil {
		return s
	}
	return FormatCents(cents)
}
