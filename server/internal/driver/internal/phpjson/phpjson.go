// Package phpjson 是驱动层用到的两套 PHP JSON 口径。
//
//   - EncodeRequest：PHP Guzzle `'json' => $arr` 的实际编码（json_encode 默认参数：
//     斜杠转义成 `\/`、非 ASCII 转 \uXXXX、对象按插入顺序）。3x-ui 面板只按语义解析，
//     但要与 PHP 侧「同一个请求体」保持可对拍，因此这里复刻默认参数而非 Go 的宽松编码。
//   - Decode：解析响应为 map/slice，数字保留原文（json.Number），避免大整数（流量字节）
//     与 expiryTime 被 float64 改写。
package phpjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// EncodeRequest 等价于 PHP json_encode($v)（默认 flags）。
//
// 支持 map[string]any / []any / string / bool / nil / 各整型 / float64 / json.Number。
// map 的键按字典序输出 —— PHP 保持插入顺序，但 3x-ui 的请求体（client/inboundIds）
// 对称解析，顺序不影响语义；这里不做保序结构，避免驱动层背一份有序 JSON 实现。
func EncodeRequest(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := appendValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func appendValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		appendString(buf, t)
	case int:
		buf.WriteString(itoa(int64(t)))
	case int32:
		buf.WriteString(itoa(int64(t)))
	case int64:
		buf.WriteString(itoa(t))
	case uint64:
		buf.WriteString(strconv.FormatUint(t, 10))
	case float64:
		s, err := phpFloat(t)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case json.Number:
		buf.WriteString(t.String())
	case []int64:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(itoa(e))
		}
		buf.WriteByte(']')
	case []string:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendString(buf, e)
		}
		buf.WriteByte(']')
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := appendValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendString(buf, k)
			buf.WriteByte(':')
			if err := appendValue(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case map[string]string:
		conv := make(map[string]any, len(t))
		for k, v := range t {
			conv[k] = v
		}
		return appendValue(buf, conv)
	default:
		// 兜底：标准库（不用于契约路径）。
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}

// appendString 复刻 PHP json_encode 的字符串转义（默认 flags，即不含 UNESCAPED_*）：
// `/` → `\/`，非 ASCII → \uXXXX（BMP 外输出代理对），控制字符用短转义。
func appendString(buf *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	if !utf8.ValidString(s) {
		// PHP json_encode 对非法 UTF-8 返回 false（调用方会看到空 body）。
		buf.WriteString(`""`)
		return
	}
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '/':
			buf.WriteString(`\/`)
		case '\b':
			buf.WriteString(`\b`)
		case '\f':
			buf.WriteString(`\f`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			switch {
			case r < 0x20:
				buf.WriteString(`\u00`)
				buf.WriteByte(hex[byte(r)>>4])
				buf.WriteByte(hex[byte(r)&0xf])
			case r < 0x80:
				buf.WriteByte(byte(r))
			case r <= 0xffff:
				writeU(buf, uint16(r))
			default:
				r -= 0x10000
				writeU(buf, uint16(0xd800+(r>>10)))
				writeU(buf, uint16(0xdc00+(r&0x3ff)))
			}
		}
	}
	buf.WriteByte('"')
}

func writeU(buf *bytes.Buffer, u uint16) {
	const hex = "0123456789abcdef"
	buf.WriteString(`\u`)
	buf.WriteByte(hex[(u>>12)&0xf])
	buf.WriteByte(hex[(u>>8)&0xf])
	buf.WriteByte(hex[(u>>4)&0xf])
	buf.WriteByte(hex[u&0xf])
}

// phpFloat 复刻 PHP serialize_precision=-1 的浮点输出：
// 最短往返表示；整数值不带 ".0"（33.0 → 33）；指数形式为 E±n。
//
// 3x-ui 请求体里浮点极少（client 字段都是整数），这里只保证形态不劣化。
func phpFloat(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errors.New("PHP json_encode 对 NAN/INF 返回 false")
	}
	s := strconv.FormatFloat(f, 'g', -1, 64)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		mant, exp := s[:i], s[i+1:]
		e, err := strconv.Atoi(exp)
		if err != nil {
			return "", err
		}
		sign := "+"
		if e < 0 {
			sign = "-"
			e = -e
		}
		if !strings.Contains(mant, ".") {
			mant += ".0"
		}
		return mant + "E" + sign + strconv.Itoa(e), nil
	}
	return s, nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Decode 解析 JSON 文本为通用结构：对象 → map[string]any，数组 → []any，
// 数字 → json.Number（保原文）。失败返回错误（等价于 PHP json_decode 返回 null）。
func Decode(body []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// DecodeObject 解析为 JSON 对象；不是对象时返回错误（对齐 PHP 的 is_array 判定）。
func DecodeObject(body []byte) (map[string]any, error) {
	v, err := Decode(body)
	if err != nil {
		return nil, err
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return obj, nil
}
