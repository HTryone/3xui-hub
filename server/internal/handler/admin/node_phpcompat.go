package admin

// 本文件是 node_sqlstore.go 需要的「保序 JSON + PHP 标量语义」本地实现。
//
// 为什么不直接引 internal/driver/internal/phpjson|phpval：
// Go 的 internal 规则只允许 `driver/` 子树 import 它们，handler/admin 引用会编译失败
// （use of internal package ... not allowed）。任务边界又禁止动 driver/ 与共享文件，
// 因此在节点域自带一份语义等价实现。
//
// 语义对齐 driver/internal/phpjson（保序 + json.Number 原文 + 两种转义口径）与
// driver/internal/phpval 的 (bool)/(int) 取值；数字保留 json.Number 原文，
// 以便 driver_config 合并回写时与 PHP json_decode(json_encode) 往返一致。

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// ordObj 是保序的 JSON 对象（对应 PHP 关联数组）。
type ordObj []ordMember

// ordMember 是 ordObj 的一个键值对。
type ordMember struct {
	Key   string
	Value any
}

// ordArr 是显式数组（与空 ordObj 区分：PHP 空数组编码成 []，空对象 {}）。
type ordArr []any

// Get 取字段值。
func (o ordObj) Get(key string) (any, bool) {
	for _, m := range o {
		if m.Key == key {
			return m.Value, true
		}
	}
	return nil, false
}

// Set 写入字段：已存在则原位替换（保留位置，对齐 PHP 数组赋值语义），否则追加。
func (o *ordObj) Set(key string, value any) {
	for i := range *o {
		if (*o)[i].Key == key {
			(*o)[i].Value = value
			return
		}
	}
	*o = append(*o, ordMember{Key: key, Value: value})
}

// ordParse 把 JSON 文本解析成保序结构：对象 → ordObj，数组 → ordArr，数字 → json.Number。
// 空文本/空白 → (nil, nil)（对齐 json_decode("") === null）。
func ordParse(raw []byte) (any, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	v, err := ordParseValue(dec)
	if err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("admin: JSON 尾部存在多余内容")
	}
	return v, nil
}

func ordParseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); ok {
		switch delim {
		case '{':
			obj := ordObj{}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("admin: JSON 对象键不是字符串")
				}
				val, err := ordParseValue(dec)
				if err != nil {
					return nil, err
				}
				obj.Set(key, val)
			}
			if _, err := dec.Token(); err != nil { // '}'
				return nil, err
			}
			return obj, nil
		case '[':
			arr := ordArr{}
			for dec.More() {
				val, err := ordParseValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, val)
			}
			if _, err := dec.Token(); err != nil { // ']'
				return nil, err
			}
			return arr, nil
		}
		return nil, errors.New("admin: 非法的 JSON 分隔符")
	}
	return tok, nil
}

type ordEscape int

const (
	ordEscapeNone ordEscape = iota // UNESCAPED_SLASHES | UNESCAPED_UNICODE
	ordEscapeAll                   // PHP json_encode 默认：斜杠 + 非 ASCII 都转义
)

// ordEncode 等价于 json_encode($v)（默认 flags）。
func ordEncode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := ordAppend(&buf, v, ordEscapeAll); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ordEncodeUnescaped 等价于 json_encode($v, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE)。
func ordEncodeUnescaped(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := ordAppend(&buf, v, ordEscapeNone); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ordAppend(buf *bytes.Buffer, v any, mode ordEscape) error {
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
		ordAppendString(buf, t, mode)
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
		s, err := ordFloatText(t)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case ordObj:
		buf.WriteByte('{')
		for i, m := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			ordAppendString(buf, m.Key, mode)
			buf.WriteByte(':')
			if err := ordAppend(buf, m.Value, mode); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	case ordArr:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := ordAppend(buf, e, mode); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []any:
		return ordAppend(buf, ordArr(t), mode)
	case []string:
		buf.WriteByte('[')
		for i, s := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			ordAppendString(buf, s, mode)
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
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}

// ordAppendString 复刻 PHP json_encode 的字符串转义（按 flags 组合）。
func ordAppendString(buf *bytes.Buffer, s string, mode ordEscape) {
	const hex = "0123456789abcdef"
	if !utf8.ValidString(s) {
		return // PHP json_encode 返回 false；调用方得到空串
	}
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '/':
			if mode == ordEscapeNone {
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
			case mode != ordEscapeAll:
				buf.WriteRune(r)
			case r <= 0xffff:
				ordWriteU(buf, uint16(r))
			default:
				r -= 0x10000
				ordWriteU(buf, uint16(0xd800+(r>>10)))
				ordWriteU(buf, uint16(0xdc00+(r&0x3ff)))
			}
		}
	}
	buf.WriteByte('"')
}

func ordWriteU(buf *bytes.Buffer, u uint16) {
	const hex = "0123456789abcdef"
	buf.WriteString(`\u`)
	buf.WriteByte(hex[(u>>12)&0xf])
	buf.WriteByte(hex[(u>>8)&0xf])
	buf.WriteByte(hex[(u>>4)&0xf])
	buf.WriteByte(hex[u&0xf])
}

// ordFloatText 复刻 PHP serialize_precision=-1 的浮点输出。
func ordFloatText(f float64) (string, error) {
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

// ordFloat 复刻 (float) 取值（读 driver_config 整数字段 / 列表元素）。
func ordFloat(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case bool:
		if t {
			return 1
		}
		return 0
	case string:
		f, err := strconv.ParseFloat(strings.TrimLeft(t, " \t\n\r\v\f"), 64)
		if err != nil {
			return 0
		}
		return f
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
		return 0
	case int:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case uint64:
		return float64(t)
	case float64:
		return t
	}
	return 0
}

// ordBool 复刻 (bool) 取值。
func ordBool(v any) bool {
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
	case int:
		return t != 0
	case int64:
		return t != 0
	}
	return true
}
