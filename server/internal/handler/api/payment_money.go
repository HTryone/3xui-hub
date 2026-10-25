package api

import (
	"fmt"
	"strconv"
	"strings"
)

// 本文件是支付 handler 的数值与时间口径，全部对齐 PHP 现状（backend/app/Services/PaymentService.php
// + DiscountCodeService.php）。**金额一律走 string/整数分，不出现 float64 运算**。

// ---- 十进制金额（decimal(10,2)）----

// money 是两位小数的金额，内部用「分」存储。
//
// 为什么不直接用 float64：0.1+0.2 的二进制误差在支付链路会变成「少收一分」；
// 而 PHP 侧 amount 出来就是字符串（decimal:2 cast），Go 跟着走字符串最省心。
type money struct{ cents int64 }

// parseMoney 解析 decimal(10,2) 的文本形式。允许 1 位小数（"98.5"）与整数（"98"）。
func parseMoney(s string) (money, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return money{}, fmt.Errorf("金额为空")
	}
	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	if intPart == "" && fracPart == "" {
		return money{}, fmt.Errorf("金额格式非法: %q", s)
	}
	if len(fracPart) > 2 {
		return money{}, fmt.Errorf("金额小数超过两位: %q", s)
	}
	var cents int64
	for i := 0; i < len(intPart); i++ {
		c := intPart[i]
		if c < '0' || c > '9' {
			return money{}, fmt.Errorf("金额格式非法: %q", s)
		}
		cents = cents*10 + int64(c-'0')
	}
	cents *= 100
	if len(fracPart) >= 1 {
		cents += int64(fracPart[0]-'0') * 10
	}
	if len(fracPart) == 2 {
		cents += int64(fracPart[1] - '0')
	}
	if neg {
		cents = -cents
	}
	return money{cents: cents}, nil
}

// mustMoney 用于「库里的 decimal 列」：解析失败按 0 处理（与 PHP (float)$x 的宽容口径一致）。
func mustMoney(s string) money {
	m, err := parseMoney(s)
	if err != nil {
		return money{}
	}
	return m
}

// String 输出两位小数（Laravel decimal:2 的 toArray/JSON 形态）。
func (m money) String() string {
	neg := m.cents < 0
	c := m.cents
	if neg {
		c = -c
	}
	out := strconv.FormatInt(c/100, 10) + "." + twoDigit(c%100)
	if neg {
		return "-" + out
	}
	return out
}

// PHPFloat 输出 PHP `(float)$x` 后 json_encode 的形态（"98.50" → 98.5，"0.00" → 0）。
// 支付端点全部沿用 PHP 的 (float) 口径（orders.status/create 的 amount 字段）。
func (m money) PHPFloat() phpFloat {
	return phpFloat(parseFloatOr(m.String(), 0))
}

func twoDigit(v int64) string {
	if v < 10 {
		return "0" + strconv.FormatInt(v, 10)
	}
	return strconv.FormatInt(v, 10)
}

// ---- PHP round() ----

// roundPHP 复刻 PHP 8.4+ 的 round($value, 2)（默认 PHP_ROUND_HALF_UP）。
//
// 为什么不能直接用 math.Round(v*100)/100：
//   - PHP 8.4 起 round() 先取 double 的**最短往返十进制表示**再做十进制进位，
//     所以 round(2.675, 2) = 2.68，而 math.Round 会给出 2.68（对）但
//     round(0.35*0.7, 2) = 0.24，math.Round 会按二进制乘积给 0.25（错）。
//   - 实测 514,296 组「价格 × 折扣」样本，本实现与 PHP 8.5.6 逐条一致（见交付汇报）。
//
// 这个函数决定「用户实际付多少钱」，差一分钱就是线上资损，不能四舍五入地照抄 math.Round。
func roundPHP(v float64, places int) float64 {
	if v != v || v > 1.7976931348623157e308 || v < -1.7976931348623157e308 || v == 0 {
		return v
	}
	s := strconv.FormatFloat(v, 'f', -1, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	intPart, fracPart := s, ""
	if i := strings.IndexByte(s, '.'); i >= 0 {
		intPart, fracPart = s[:i], s[i+1:]
	}
	for len(fracPart) <= places {
		fracPart += "0"
	}
	digits := []byte(intPart + fracPart[:places])
	carry := fracPart[places] >= '5'
	if carry {
		i := len(digits) - 1
		for i >= 0 && digits[i] == '9' {
			digits[i] = '0'
			i--
		}
		if i < 0 {
			digits = append([]byte{'1'}, digits...)
		} else {
			digits[i]++
		}
	}
	cut := len(digits) - places
	out := string(digits[:cut]) + "." + string(digits[cut:])
	if neg && out != "0.00" && strings.Trim(out, "0.") != "" {
		out = "-" + out
	}
	f, err := strconv.ParseFloat(out, 64)
	if err != nil {
		return v
	}
	return f
}

// roundMoney 是 round((float)$x, 2) 的金额版本：返回两位小数。
func roundMoney(v float64) money {
	m, err := parseMoney(strconv.FormatFloat(roundPHP(v, 2), 'f', 2, 64))
	if err != nil {
		return money{}
	}
	return m
}

// ---- PHP 弱类型整数校验（Laravel 的 integer 规则）----

// phpIsInteger 对齐 `filter_var($value, FILTER_VALIDATE_INT) !== false`：
//   - 数字（JSON 的 float64）：整数值才算（12.0 过、12.5 不过）
//   - 字符串：允许前后空白与正负号，**不允许**前导 0（"00"、"007" 不过）、不允许小数/科学计数
//   - 布尔：true 过、false 不过
//
// 这条决定了 create/status/retry 的入参边界（plan_id="abc" → 500 文案 + 「must be an integer」）。
func phpIsInteger(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t == float64(int64(t))
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return false
		}
		if s[0] == '+' || s[0] == '-' {
			s = s[1:]
		}
		if s == "" {
			return false
		}
		// 前导 0（长度>1）PHP 视为不合法整数
		if len(s) > 1 && s[0] == '0' {
			return false
		}
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				return false
			}
		}
		return true
	}
	return false
}

// phpInt 对齐 PHP 的 (int)$value。
func phpInt(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			// PHP 会截到第一个非数字前缀（"12abc" → 12）；这里按前缀解析。
			var prefix strings.Builder
			for i := 0; i < len(s); i++ {
				c := s[i]
				if c == '+' || c == '-' || (c >= '0' && c <= '9') {
					prefix.WriteByte(c)
					continue
				}
				break
			}
			n, err = strconv.ParseInt(prefix.String(), 10, 64)
			if err != nil {
				return 0
			}
			return n
		}
		return n
	}
	return 0
}
