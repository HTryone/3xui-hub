package phpjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// 保序 JSON 值：用于逐字节复刻 PHP json_encode 的输出（xray config、指纹、订阅链接）。
//
// 为什么不用 map：PHP 的 json_encode 按数组插入顺序输出字段，Go 的 encoding/json
// 对 map 按键名排序。xray config 的字段顺序对内核无影响，但对「与 PHP 逐字节对拍」
// 是硬约束（节点每轮拉取都靠 ETag 判 304，任何字节差异都会触发无谓重启内核）。

// Object 是保序的 JSON 对象。
type Object []Member

// Member 是一个键值对。
type Member struct {
	Key   string
	Value any
}

// Array 是显式数组类型（用于与空 Object 区分：PHP 空数组编码成 []，空对象 {}）。
type Array []any

// Get 取字段值。
func (o Object) Get(key string) (any, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// Set 写入字段：已存在则原位替换（保留位置，对齐 PHP 数组赋值语义），否则追加到末尾。
func (o *Object) Set(key string, value any) {
	for i := range *o {
		if (*o)[i].Key == key {
			(*o)[i].Value = value
			return
		}
	}
	*o = append(*o, Member{Key: key, Value: value})
}

// Clone 深拷贝 Object。
func (o Object) Clone() Object {
	if o == nil {
		return nil
	}
	out := make(Object, len(o))
	for i, m := range o {
		out[i] = Member{Key: m.Key, Value: cloneValue(m.Value)}
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case Object:
		return t.Clone()
	case Array:
		out := make(Array, len(t))
		for i, e := range t {
			out[i] = cloneValue(e)
		}
		return out
	case []any:
		out := make(Array, len(t))
		for i, e := range t {
			out[i] = cloneValue(e)
		}
		return out
	case json.RawMessage:
		return append(json.RawMessage(nil), t...)
	}
	return v
}

// MarshalJSON 实现 json.Marshaler。
func (o Object) MarshalJSON() ([]byte, error) { return EncodeUnescaped(o) }

// MarshalJSON 实现 json.Marshaler。
func (a Array) MarshalJSON() ([]byte, error) { return EncodeUnescaped(a) }

// EncodeUnescaped 等价于 json_encode($v, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE)。
func EncodeUnescaped(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := appendOrdered(&buf, v, escapeModeNone); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodeUnescapedString 返回字符串形态（拼接指纹、拼 URL 时更方便）。
func EncodeUnescapedString(v any) (string, error) {
	b, err := EncodeUnescaped(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

type escapeMode int

const (
	escapeModeNone  escapeMode = iota // UNESCAPED_SLASHES | UNESCAPED_UNICODE
	escapeModeSlash                   // 只转义斜杠（UNESCAPED_UNICODE）
	escapeModeAll                     // 默认：斜杠 + 非 ASCII 都转义
)

func appendOrdered(buf *bytes.Buffer, v any, mode escapeMode) error {
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
		appendStringMode(buf, t, mode)
	case json.Number:
		buf.WriteString(t.String())
	case int:
		buf.WriteString(strconv.Itoa(t))
	case int32:
		buf.WriteString(strconv.FormatInt(int64(t), 10))
	case int64:
		buf.WriteString(strconv.FormatInt(t, 10))
	case uint64:
		buf.WriteString(strconv.FormatUint(t, 10))
	case float64:
		s, err := orderedFloat(t)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case Object:
		buf.WriteByte('{')
		for i, m := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendStringMode(buf, m.Key, mode)
			buf.WriteByte(':')
			if err := appendOrdered(buf, m.Value, mode); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case Array:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := appendOrdered(buf, e, mode); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []any:
		return appendOrdered(buf, Array(t), mode)
	case []string:
		buf.WriteByte('[')
		for i, s := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendStringMode(buf, s, mode)
		}
		buf.WriteByte(']')
	case []int64:
		buf.WriteByte('[')
		for i, n := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			buf.WriteString(strconv.FormatInt(n, 10))
		}
		buf.WriteByte(']')
	default:
		// 兜底：标准库（不用于对拍路径）。
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}

// appendStringMode 复刻 PHP json_encode 的字符串转义（按 flags 组合）。
func appendStringMode(buf *bytes.Buffer, s string, mode escapeMode) {
	const hex = "0123456789abcdef"
	if !utf8.ValidString(s) {
		return // PHP json_encode 返回 false；调用方会得到空串
	}
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '/':
			if mode == escapeModeNone {
				buf.WriteByte('/')
			} else {
				buf.WriteString(`\/`)
			}
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
			case mode != escapeModeAll:
				buf.WriteRune(r)
			case r <= 0xffff:
				writeUEscape(buf, uint16(r))
			default:
				r -= 0x10000
				writeUEscape(buf, uint16(0xd800+(r>>10)))
				writeUEscape(buf, uint16(0xdc00+(r&0x3ff)))
			}
		}
	}
	buf.WriteByte('"')
}

func writeUEscape(buf *bytes.Buffer, u uint16) {
	const hex = "0123456789abcdef"
	buf.WriteString(`\u`)
	buf.WriteByte(hex[(u>>12)&0xf])
	buf.WriteByte(hex[(u>>8)&0xf])
	buf.WriteByte(hex[(u>>4)&0xf])
	buf.WriteByte(hex[u&0xf])
}

// orderedFloat 复刻 PHP serialize_precision=-1 的浮点输出。
func orderedFloat(f float64) (string, error) {
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

// EncodeDefault 等价于 json_encode($v)（默认 flags：斜杠与非 ASCII 都转义）。
func EncodeDefault(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := appendOrdered(&buf, v, escapeModeAll); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// EncodeDefaultString 返回字符串形态。
func EncodeDefaultString(v any) (string, error) {
	b, err := EncodeDefault(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseOrdered 把 JSON 文本解析成保序结构：对象 → Object，数组 → Array，数字 → json.Number。
//
// 数字保留原文（不转 float），以免大整数（流量字节）与小数被改写；
// 顺序保留以便逐字节复刻 PHP 输出。空文本/空白 → (nil, nil)（对齐 json_decode("") === null）。
func ParseOrdered(raw []byte) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	v, err := parseOrderedValue(dec)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("phpjson: JSON 尾部存在多余内容")
	}
	return v, nil
}

func parseOrderedValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return tok, nil
	}
	switch delim {
	case '{':
		o := Object{}
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, errors.New("phpjson: JSON 对象键不是字符串")
			}
			val, err := parseOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			o = append(o, Member{Key: key, Value: val})
		}
		if _, err := dec.Token(); err != nil { // 吃掉 '}'
			return nil, err
		}
		return o, nil
	case '[':
		a := Array{}
		for dec.More() {
			val, err := parseOrderedValue(dec)
			if err != nil {
				return nil, err
			}
			a = append(a, val)
		}
		if _, err := dec.Token(); err != nil { // 吃掉 ']'
			return nil, err
		}
		return a, nil
	}
	return nil, errors.New("phpjson: 非法的 JSON 分隔符")
}

// UnmarshalJSON 让保序结构可以直接作为结构体字段解码（测试夹具、配置反序列化用）。
func (o *Object) UnmarshalJSON(data []byte) error {
	v, err := ParseOrdered(data)
	if err != nil {
		return err
	}
	if v == nil {
		*o = nil
		return nil
	}
	obj, ok := v.(Object)
	if !ok {
		return errors.New("phpjson: 期望 JSON 对象")
	}
	*o = obj
	return nil
}

// UnmarshalJSON 同上（数组）。
func (a *Array) UnmarshalJSON(data []byte) error {
	v, err := ParseOrdered(data)
	if err != nil {
		return err
	}
	if v == nil {
		*a = nil
		return nil
	}
	arr, ok := v.(Array)
	if !ok {
		return errors.New("phpjson: 期望 JSON 数组")
	}
	*a = arr
	return nil
}
