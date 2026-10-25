package nodeapi

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

// ── PHP 值语义辅助 ────────────────────────────────────────────
//
// 这一层集中处理「PHP 标量/数组语义」与 Go 类型之间的落差。每个函数名都说清
// 它对应 PHP 的哪条语义，避免调用点各写一套（nodehub 参考项目的字段名事故，
// 根因就是散落的隐式转换）。

// toObject 把已解析的 JSON 值当关联数组读（PHP 的 `$x ?? []`）。
// 非对象一律视为空对象（PHP 里对标量取 ['k'] 是 null，再 ?? [] 得到空数组）。
func toObject(v any) Object {
	if o, ok := v.(Object); ok {
		return o
	}
	return Object{}
}

// toArrayValues 复刻 `(array) $value`：数组原样；标量包成单元素数组；nil 空数组。
func toArrayValues(v any) Array {
	switch t := v.(type) {
	case Array:
		return t
	case []any:
		return Array(t)
	case nil:
		return Array{}
	default:
		return Array{t}
	}
}

// stringOf 复刻 `(string) $value` 对 JSON 值的取值：字符串原样，数字用字面量，
// 布尔 php 风格（true→"1"），nil → ""。
func stringOf(v any) string {
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
		return formatPHPFloat(t)
	}
	return ""
}

// intOf 复刻 `(int) $value`。
func intOf(v any) int64 {
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
		return phpStrToInt(t)
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

// phpStrToInt 复刻 PHP 的字符串转 int：取开头的数字前缀，无则 0。
func phpStrToInt(s string) int64 {
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

// floatOf 复刻 `(float) $value`。
func floatOf(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case json.Number:
		f, _ := t.Float64()
		return f
	case string:
		f, _ := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f
	case bool:
		if t {
			return 1
		}
		return 0
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case float64:
		return t
	}
	return 0
}

// boolOf 复刻 `(bool) $value`。
func boolOf(v any) bool {
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
		return intOf(t) != 0
	}
	return true
}

// phpEmpty 复刻 PHP 的 empty()：null / "" / "0" / 0 / 0.0 / false / 空数组 都算空。
func phpEmpty(v any) bool {
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
	case Array:
		return len(t) == 0
	case []any:
		return len(t) == 0
	case Object:
		return len(t) == 0
	}
	return false
}

// formatPHPFloat 复刻 PHP json_encode 的浮点输出（serialize_precision=-1）：
// 最短往返表示；整数值输出为整数（33.0 → 33）；指数形式小写 e、带符号、尾数补 ".0"
// （1.0e+25 / 1.5e-7）。PHP 实测口径见 phpvalue_test.go 的对拍用例。
func formatPHPFloat(f float64) string {
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

// runeTruncate 复刻 PHP mb_substr($s, 0, $limit)（按字符截断，不是字节）。
func runeTruncate(s string, limit int) string {
	if limit < 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}

// ── 时间格式 ─────────────────────────────────────────────────

// iso8601 复刻 Carbon::toIso8601String()（app 时区 UTC）：2026-10-24T07:44:27+00:00。
func iso8601(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05-07:00")
}

// iso8601Zulu 复刻 Carbon::toIso8601ZuluString()：2026-11-07T00:00:00Z。
func iso8601Zulu(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// dbDateTime 是 MySQL datetime 列的写入格式（Laravel 用 toDateTimeString）。
func dbDateTime(t time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05")
}

// itoa 是 int64 的十进制表示（拼接指纹/计数器用）。
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// mapToPlain 把保序结构转成普通 map/slice（校验器只按路径取值，不关心顺序）。
func mapToPlain(v any) any {
	switch t := v.(type) {
	case Object:
		m := make(map[string]any, len(t))
		for _, mem := range t {
			m[mem.Key] = mapToPlain(mem.Value)
		}
		return m
	case Array:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = mapToPlain(e)
		}
		return out
	case json.Number:
		return t
	}
	return v
}
