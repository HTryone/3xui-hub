package payment

import (
	"sort"
	"strings"
)

// 本文件复刻签名用到的两个 PHP 内置函数 —— 结算串**必须逐字节**一致，
// 差一个字符（编码大小写、空格转 + 还是 %20、'~' 转不转）就是验签失败。
//
// 为什么不用 net/url：url.Values.Encode() 按字典序排（这里要保留调用方的 ksort 结果）、
// 用 %20 而不是 '+'、且不转义 '~'，与 PHP 的 http_build_query 输出不同。

// phpURLEncode 对齐 PHP urlencode()：字母数字与 "-_." 原样，空格转 '+'，其余 %XX（大写）。
func phpURLEncode(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex(c >> 4))
			b.WriteByte(upperHex(c & 0xF))
		}
	}
	return b.String()
}

func upperHex(b byte) byte {
	if b < 10 {
		return '0' + b
	}
	return 'A' + b - 10
}

// signaturePairs 是「参与签名的键值对集合」。
//
// 之所以是切片而不是 map：PHP 的 array_filter + ksort 之后 http_build_query 按**有序数组**
// 输出，键序就是签名串的顺序；map 会丢掉「同键重复」与稳定顺序的语义。
type signaturePairs []string

// sortedNonEmpty 对齐 `array_filter($p, fn($v) => $v !== ” && $v !== null); ksort($p)`：
// 过滤空串与 null（注意 "0" 是保留的），再按 key 的字节序升序。
//
// ksort 默认 SORT_REGULAR 对字符串键就是逐字节比较（大写字母在小写之前），
// 与 Go 的 sort.Strings 一致。
func sortedNonEmpty(params map[string]string) signaturePairs {
	keys := make([]string, 0, len(params))
	for k, v := range params {
		if v == "" {
			continue // PHP 的 $v !== '' && $v !== null 在这里等价于「空串不要」
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make(signaturePairs, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, phpURLEncode(k)+"="+phpURLEncode(params[k]))
	}
	return pairs
}

// String 拼出 `k=v&k=v`（等价 PHP http_build_query 的输出）。
func (p signaturePairs) String() string { return strings.Join(p, "&") }
