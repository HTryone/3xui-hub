package admin

// 本文件把「值 → JSON 字节」按 PHP json_encode 的默认口径复刻。
//
// 为什么不用 encoding/json（与 handler/api 同因，见其 phpjson.go）：
//   - 中文：PHP 默认 \uXXXX 转义，Go 输出原始 UTF-8；
//   - 斜杠：PHP 转义 "/" 为 "\/"，Go 不转义；
//   - <>&：Go 会转义成 < 等，PHP 不会；
//   - 键序：PHP 关联数组保留插入顺序，Go map 按字典序。
//
// 管理端要能与 PHP 现状逐字节对拍（金样本见 *_test.go），所以统一走这里的编码器。

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// jsObj 是键序可控的 JSON 对象（对应 PHP 的关联数组）。
type jsObj struct {
	keys []string
	vals []any
}

// jo 按参数顺序构造对象：jo("code", 0, "msg", "ok")。
func jo(pairs ...any) *jsObj {
	if len(pairs)%2 != 0 {
		panic("admin: jo() 需要偶数个参数")
	}
	o := &jsObj{keys: make([]string, 0, len(pairs)/2), vals: make([]any, 0, len(pairs)/2)}
	for i := 0; i < len(pairs); i += 2 {
		o.keys = append(o.keys, pairs[i].(string))
		o.vals = append(o.vals, pairs[i+1])
	}
	return o
}

// put 追加或覆盖一个键；覆盖时保持原位置（与 PHP 数组赋值语义一致）。
func (o *jsObj) put(key string, val any) *jsObj {
	for i, k := range o.keys {
		if k == key {
			o.vals[i] = val
			return o
		}
	}
	o.keys = append(o.keys, key)
	o.vals = append(o.vals, val)
	return o
}

// jsArr 是 JSON 数组。
type jsArr []any

// phpFloat 标记「按 PHP 的 (float) 输出」的数值：
// strconv 的 'f' + -1 与 PHP 的 serialize_precision=-1 同为最短往返表示（4.0→4，4.9→4.9）。
type phpFloat float64

// eloqTime 是 Eloquent 模型 toJSON 的时间口径：2026-07-01T15:42:57.000000Z。
type eloqTime time.Time

// isoTime 是 Carbon::toIso8601String() 的口径：2026-11-07T04:29:24+00:00。
type isoTime time.Time

// dateTimeStr 是 Carbon::toDateTimeString() 的口径：2026-10-24 09:01:59。
type dateTimeStr time.Time

// phpEncode 生成与 PHP json_encode（默认选项）字节兼容的输出。
func phpEncode(v any) []byte {
	buf := make([]byte, 0, 256)
	return appendPHP(buf, v)
}

func appendPHP(dst []byte, v any) []byte {
	switch t := v.(type) {
	case nil:
		return append(dst, "null"...)
	case *jsObj:
		dst = append(dst, '{')
		for i, k := range t.keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendPHPString(dst, k)
			dst = append(dst, ':')
			dst = appendPHP(dst, t.vals[i])
		}
		return append(dst, '}')
	case jsObj:
		return appendPHP(dst, &t)
	case jsArr:
		dst = append(dst, '[')
		for i, item := range t {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendPHP(dst, item)
		}
		return append(dst, ']')
	case []any:
		return appendPHP(dst, jsArr(t))
	case []string:
		dst = append(dst, '[')
		for i, s := range t {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendPHPString(dst, s)
		}
		return append(dst, ']')
	case string:
		return appendPHPString(dst, t)
	case bool:
		if t {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case int:
		return strconv.AppendInt(dst, int64(t), 10)
	case int64:
		return strconv.AppendInt(dst, t, 10)
	case uint64:
		return strconv.AppendUint(dst, t, 10)
	case float64:
		return appendPHPFloat(dst, t)
	case json.Number:
		// decodeScheduleConfig 用 UseNumber 解析数字为 json.Number；转回数字避免 default 分支输出 null
		// （applyWarp merge 已有 settings 时，mtu/reserved 等数字字段会踩到）。
		if f, err := t.Float64(); err == nil {
			return appendPHPFloat(dst, f)
		}
		return append(dst, "null"...)
	case phpFloat:
		return appendPHPFloat(dst, float64(t))
	case eloqTime:
		dst = append(dst, '"')
		dst = append(dst, time.Time(t).UTC().Format("2006-01-02T15:04:05.000000Z")...)
		return append(dst, '"')
	case isoTime:
		dst = append(dst, '"')
		dst = append(dst, time.Time(t).UTC().Format("2006-01-02T15:04:05+00:00")...)
		return append(dst, '"')
	case dateTimeStr:
		dst = append(dst, '"')
		dst = append(dst, time.Time(t).UTC().Format("2006-01-02 15:04:05")...)
		return append(dst, '"')
	case time.Time:
		return appendPHP(dst, dateTimeStr(t))
	default:
		return append(dst, "null"...)
	}
}

// appendPHPFloat 输出 PHP json_encode 的浮点表示（业务里只有金额与倍率，2 位小数）。
func appendPHPFloat(dst []byte, f float64) []byte {
	if f != f || f > 1.7976931348623157e308 || f < -1.7976931348623157e308 {
		return append(dst, "null"...)
	}
	return strconv.AppendFloat(dst, f, 'f', -1, 64)
}

// appendPHPString 复刻 PHP json_encode 默认选项的字符串转义：
// 转义 " \ / 与全部控制字符；所有非 ASCII 字符输出 \uXXXX（BMP 之外用 UTF-16 代理对）。
func appendPHPString(dst []byte, s string) []byte {
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			var esc byte
			switch c {
			case '"':
				esc = '"'
			case '\\':
				esc = '\\'
			case '/':
				esc = '/'
			case '\n':
				esc = 'n'
			case '\r':
				esc = 'r'
			case '\t':
				esc = 't'
			case '\b':
				esc = 'b'
			case '\f':
				esc = 'f'
			default:
				if c < 0x20 {
					dst = append(dst, s[start:i]...)
					dst = append(dst, '\\', 'u', '0', '0', hexDigit(c>>4), hexDigit(c&0xF))
					i++
					start = i
					continue
				}
				i++
				continue
			}
			dst = append(dst, s[start:i]...)
			dst = append(dst, '\\', esc)
			i++
			start = i
			continue
		}

		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			r = utf8.RuneError
		}
		dst = append(dst, s[start:i]...)
		dst = appendUnicodeEscape(dst, r)
		i += size
		start = i
	}
	dst = append(dst, s[start:]...)
	return append(dst, '"')
}

func appendUnicodeEscape(dst []byte, r rune) []byte {
	if r > 0xFFFF {
		r -= 0x10000
		dst = appendU16Escape(dst, uint16(0xD800+(r>>10)))
		return appendU16Escape(dst, uint16(0xDC00+(r&0x3FF)))
	}
	return appendU16Escape(dst, uint16(r))
}

func appendU16Escape(dst []byte, v uint16) []byte {
	return append(dst, '\\', 'u',
		hexDigit(byte(v>>12)&0xF), hexDigit(byte(v>>8)&0xF),
		hexDigit(byte(v>>4)&0xF), hexDigit(byte(v)&0xF))
}

func hexDigit(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'a' + b - 10
}

// ---- 可空值的输出助手（PHP 里 null 原样输出，非 null 走对应口径）----

// nullableStr 对齐 PHP 里 `$row->col` 为 null 时输出 null、否则输出字符串。
func nullableStr(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

// nullableDateTime 对齐 `$m->col?->toDateTimeString()`。
func nullableDateTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return dateTimeStr(*t)
}

// nullableISO 对齐 `$m->col?->toIso8601String()`。
func nullableISO(t *time.Time) any {
	if t == nil {
		return nil
	}
	return isoTime(*t)
}

// nullableEloq 对齐 Eloquent 模型 toJSON 的可空时间字段。
func nullableEloq(t *time.Time) any {
	if t == nil {
		return nil
	}
	return eloqTime(*t)
}

// newJSONDecoder 构造保留数字原文的解码器（上游响应里的 id 可能是大整数）。
func newJSONDecoder(raw string) *json.Decoder {
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	return dec
}

// sortedKeys 取 map 的键并按字典序排序（Go map 无序，输出必须稳定）。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
