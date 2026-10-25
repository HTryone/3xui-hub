// Package phpval 提供 PHP 标量/数组语义与 Go 之间的一层薄转换。
//
// 单独成包的理由：驱动层要逐条复刻 PHP 的 `??` / `?:` / `(int)` / `empty()` 语义，
// 这些判定散落在每个调用点最容易写歪（nodehub 参考项目的字段名事故就是散落的隐式
// 转换导致的）。集中在一处，调用点只写「我在复刻哪条语义」。
package phpval

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"controlhub-server/internal/driver/internal/phpjson"
)

// ToObject 把已解析的 JSON 值当关联数组读（PHP 的 `$x ?? []`）。
// 非对象一律视为空对象（PHP 里对标量取 ['k'] 是 null，再 ?? [] 得到空数组）。
func ToObject(v any) phpjson.Object {
	if o, ok := v.(phpjson.Object); ok {
		return o
	}
	return phpjson.Object{}
}

// ToArrayValues 复刻 `(array) $value`：数组原样；标量包成单元素数组；nil 空数组。
func ToArrayValues(v any) phpjson.Array {
	switch t := v.(type) {
	case phpjson.Array:
		return t
	case []any:
		return phpjson.Array(t)
	case nil:
		return phpjson.Array{}
	default:
		return phpjson.Array{t}
	}
}

// String 复刻 `(string) $value` 对 JSON 值的取值：字符串原样，数字用字面量，
// 布尔 PHP 风格（true→"1"），nil → ""。
func String(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "1"
		}
		return ""
	case json.Number:
		return t.String()
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return FormatFloat(t)
	}
	return ""
}

// Bool 复刻 `(bool) $value`。
func Bool(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != "" && t != "0"
	case json.Number:
		return t.String() != "0"
	case float64:
		return t != 0
	case int, int64:
		return Int(v) != 0
	}
	return true
}

// Int 复刻 `(int) $value`。
func Int(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case json.Number:
		if i, err := t.Int64(); err == nil {
			return i
		}
		if f, err := t.Float64(); err == nil {
			return int64(f)
		}
		return 0
	case string:
		return IntFromString(t)
	case bool:
		if t {
			return 1
		}
		return 0
	case int:
		return int64(t)
	case int64:
		return t
	case float64:
		return int64(t)
	}
	return 0
}

// IntFromString 复刻 PHP 的字符串转 int：取开头的数字前缀，无则 0。
func IntFromString(s string) int64 {
	s = strings.TrimLeft(s, " \t\n\r\v\f")
	if s == "" {
		return 0
	}
	i := 0
	if s[0] == '+' || s[0] == '-' {
		i = 1
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0
	}
	n, err := strconv.ParseInt(s[:i], 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// Empty 复刻 PHP 的 empty()：null / "" / "0" / 0 / 0.0 / false / 空数组 都算空。
func Empty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == "" || t == "0"
	case bool:
		return !t
	case json.Number:
		return t.String() == "0" || t.String() == "0.0"
	case int:
		return t == 0
	case int64:
		return t == 0
	case float64:
		return t == 0
	case phpjson.Array:
		return len(t) == 0
	case []any:
		return len(t) == 0
	case phpjson.Object:
		return len(t) == 0
	}
	return false
}

// FormatFloat 复刻 PHP json_encode 的浮点输出（serialize_precision=-1）：
// 最短往返表示；整数值输出为整数（33.0 → 33）；指数形式小写 e、带符号、尾数补 ".0"。
func FormatFloat(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return "0" // PHP json_encode 对 INF/NAN 返回 false；JSON 列里不会出现
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		e, err := strconv.Atoi(exp)
		if err != nil {
			return s
		}
		sign := "+"
		if e < 0 {
			sign = "-"
			e = -e
		}
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		return mant + "e" + sign + strconv.Itoa(e)
	}
	return s
}

// EqualScalar 比较 JSON 标量与 PHP 默认值（用于跳过 reality 高级字段）。
//
// 注意 PHP 的 `===` 对 int 与 float 不相等，但 `$value === $default` 里
// $default 来自字面量表（int 0 / bool false / "" ），而 $value 来自 json_decode，
// json_decode 的 0 是 int、0.0 是 float —— 这里按「数值相等即视为默认」处理，
// 与 PHP 侧实测行为一致（0 与 0.0 都会命中 maxTimediff=0 的缺省）。
func EqualScalar(a, b any) bool {
	switch bv := b.(type) {
	case int:
		return numericEquals(a, float64(bv))
	case float64:
		return numericEquals(a, bv)
	case bool:
		av, ok := a.(bool)
		return ok && av == bv
	case string:
		return String(a) == bv && isStringish(a)
	}
	return false
}

func isStringish(v any) bool {
	_, ok := v.(string)
	return ok
}

func numericEquals(a any, b float64) bool {
	switch a.(type) {
	case json.Number, int, int64, float64:
		return floatOf(a) == b
	}
	return false
}

func floatOf(v any) float64 {
	switch t := v.(type) {
	case json.Number:
		f, _ := t.Float64()
		return f
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case float64:
		return t
	}
	return 0
}

// Itoa 是 int64 的十进制表示。
func Itoa(n int64) string { return strconv.FormatInt(n, 10) }

// RawURLEncode 复刻 PHP 的 rawurlencode()：保留 A-Za-z0-9-_.~，其余一律 %XX（大写十六进制）。
//
// **不能用 net/url.QueryEscape**：它把空格编码成 '+'，且不保留 '~' —— 订阅链接里
// 一个空格就会变成两种字节，与 PHP 输出对不上（P3 的 golden 对拍就是这条）。
func RawURLEncode(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0x0F])
		}
	}
	return b.String()
}
