// Package nodeapi 复刻 Laravel 侧的节点端 API：/api/node-api/* 六端点
// （register/config/push/alive/execute/ack）、node.auth 鉴权，以及公开的节点资产分发
// （node-install.sh / node-agent.sh / node-bin/*）。
//
// 语义逐字对齐 backend/app/Http/Controllers/Api/{NodeApiController,NodeAssetController}.php、
// app/Http/Middleware/NodeAuth.php、app/Services/NodeApiCommandService.php 与
// app/Services/TrafficSyncService.php 的 push 路径。
package nodeapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Object 是保序的 JSON 对象。
//
// 为什么不用 map：PHP 的 json_encode 按数组插入顺序输出字段，而 Go 的 encoding/json
// 对 map 会按键名排序。GET /node-api/config 的输出要与 PHP 逐字节一致（P3/P4 的
// 对拍标准），所以嵌套片段（settings / streamSettings / driver_config）必须保留
// 数据库里的原始顺序。
type Object []Member

// Member 是 Object 的一个键值对。
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

// MarshalJSON 实现 json.Marshaler（供标准库内嵌时使用）。
func (o Object) MarshalJSON() ([]byte, error) { return MarshalValue(o) }

// MarshalJSON 实现 json.Marshaler。
func (a Array) MarshalJSON() ([]byte, error) { return MarshalValue(a) }

// ParseJSON 把 JSON 文本解析成保序结构：对象 → Object，数组 → Array，数字 → json.Number。
// 数字保留原文（不转 float），以免大整数/小数被改写；顺序保留以便逐字节复刻 PHP 输出。
func ParseJSON(raw []byte) (any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	// 末尾不应再有内容（json 列里不会出现多值）。
	if dec.More() {
		return nil, fmt.Errorf("nodeapi: JSON 尾部存在多余内容")
	}
	return v, nil
}

func parseValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	return parseToken(dec, tok)
}

func parseToken(dec *json.Decoder, tok json.Token) (any, error) {
	if delim, ok := tok.(json.Delim); ok {
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
					return nil, fmt.Errorf("nodeapi: JSON 对象键不是字符串")
				}
				val, err := parseValue(dec)
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
				val, err := parseValue(dec)
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
		return nil, fmt.Errorf("nodeapi: 非法的 JSON 分隔符 %q", delim)
	}
	return tok, nil
}

// ParseObject 解析 JSON 对象文本；空/nil 返回 nil。
func ParseObject(raw []byte) (Object, error) {
	v, err := ParseJSON(raw)
	if err != nil || v == nil {
		return nil, err
	}
	o, ok := v.(Object)
	if !ok {
		return nil, fmt.Errorf("nodeapi: 期望 JSON 对象，实际 %T", v)
	}
	return o, nil
}

// MarshalValue 按 PHP json_encode(JSON_UNESCAPED_SLASHES|JSON_UNESCAPED_UNICODE) 的
// 口径序列化：不转义斜杠、非 ASCII 原样输出、不把 < > & 转成 \u00XX。
func MarshalValue(v any) ([]byte, error) {
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
	case Object:
		buf.WriteByte('{')
		for i, m := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendJSONString(buf, m.Key)
			buf.WriteByte(':')
			if err := appendValue(buf, m.Value); err != nil {
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
			if err := appendValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []any: // 未显式转换的切片按数组处理
		return appendValue(buf, Array(t))
	case []string:
		buf.WriteByte('[')
		for i, s := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendJSONString(buf, s)
		}
		buf.WriteByte(']')
	case string:
		appendJSONString(buf, t)
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
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
		// PHP json_encode 默认 serialize_precision=-1：最短往返表示；
		// 整数值输出为整数（33.0 → 33），指数形式为小写 e 且带符号（1.0e+25）。
		buf.WriteString(formatPHPFloat(t))
	case json.RawMessage:
		buf.Write(t)
	default:
		// 兜底：标准库序列化（不用于对拍路径）。
		b, err := json.Marshal(t)
		if err != nil {
			return err
		}
		buf.Write(b)
	}
	return nil
}

// appendJSONString 写 PHP 口径的 JSON 字符串：转义 " \ 与控制字符，其余（含 UTF-8、斜杠）原样。
func appendJSONString(buf *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
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
			if c < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hex[c>>4])
				buf.WriteByte(hex[c&0xf])
			} else {
				// 按字节透传，UTF-8 序列保持原样（JSON_UNESCAPED_UNICODE）。
				buf.WriteByte(c)
			}
		}
	}
	buf.WriteByte('"')
}

// phpEncodeDefault 是 PHP json_encode() 默认参数（无 UNESCAPED_*）的输出：
// 斜杠转义成 \/，非 ASCII 转成 \uXXXX（小写十六进制），其余同 appendJSONString。
//
// 仅用于复刻 fingerprint() 里的 sha1(json_encode($node->xrayRoutingSettings()))。
func phpEncodeDefault(v any) (string, error) {
	var buf bytes.Buffer
	if err := appendValuePHPDefault(&buf, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func appendValuePHPDefault(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case Object:
		buf.WriteByte('{')
		for i, m := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendPHPDefaultString(buf, m.Key)
			buf.WriteByte(':')
			if err := appendValuePHPDefault(buf, m.Value); err != nil {
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
			if err := appendValuePHPDefault(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []any:
		return appendValuePHPDefault(buf, Array(t))
	case string:
		appendPHPDefaultString(buf, t)
	default:
		return appendValue(buf, v)
	}
	return nil
}

func appendPHPDefaultString(buf *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
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
			if r < 0x20 {
				buf.WriteString(`\u00`)
				buf.WriteByte(hex[byte(r)>>4])
				buf.WriteByte(hex[byte(r)&0xf])
			} else if r < 0x80 {
				buf.WriteByte(byte(r))
			} else if r <= 0xffff {
				buf.WriteString(`\u`)
				for shift := 12; shift >= 0; shift -= 4 {
					buf.WriteByte(hex[(r>>uint(shift))&0xf])
				}
			} else {
				// 代理对
				r -= 0x10000
				hi := 0xd800 + (r >> 10)
				lo := 0xdc00 + (r & 0x3ff)
				for _, sur := range []rune{hi, lo} {
					buf.WriteString(`\u`)
					for shift := 12; shift >= 0; shift -= 4 {
						buf.WriteByte(hex[(sur>>uint(shift))&0xf])
					}
				}
			}
		}
	}
	buf.WriteByte('"')
}

// mergeRecursive 复刻 PHP array_replace_recursive：两边都是关联数组时逐键递归，
// 否则用 patch 覆盖。用于 driver_config 的键级合并（不得整块回写）。
func mergeRecursive(base, patch Object) Object {
	out := base.Clone()
	if out == nil {
		out = Object{}
	}
	for _, m := range patch {
		cur, exists := out.Get(m.Key)
		if exists {
			curObj, okCur := cur.(Object)
			patchObj, okPatch := m.Value.(Object)
			if okCur && okPatch {
				out.Set(m.Key, mergeRecursive(curObj, patchObj))
				continue
			}
		}
		out.Set(m.Key, cloneValue(m.Value))
	}
	return out
}

// appendValuePHPJSONResponse 复刻 PHP `response()->json()` 的编码口径：
// 与 json_encode 默认参数一致（转义 / 与非 ASCII，< > & 不转义）。
func appendValuePHPJSONResponse(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case Object:
		buf.WriteByte('{')
		for i, m := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			appendPHPDefaultString(buf, m.Key)
			buf.WriteByte(':')
			if err := appendValuePHPJSONResponse(buf, m.Value); err != nil {
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
			if err := appendValuePHPJSONResponse(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case []any:
		return appendValuePHPJSONResponse(buf, Array(t))
	case []string:
		out := make(Array, len(t))
		for i, s := range t {
			out[i] = s
		}
		return appendValuePHPJSONResponse(buf, out)
	case string:
		appendPHPDefaultString(buf, t)
	default:
		return appendValue(buf, v)
	}
	return nil
}
