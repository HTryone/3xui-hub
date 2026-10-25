package admin

// 逐条对齐 backend/app/Support/NodeRegion.php：从节点名识别两位地区代码。
//
// 顺序有意义（数组顺序即优先级）：「中国香港」必须命中 HK 而不是 CN；
// 「Singapore」必须命中 SG 而不是 IN（IN 是印度的两位码）。
// 两位字母码必须自成一段（前后不紧挨字母），否则 lo·CA·l-verify / Ch·IN·a 这类
// 名字会满屏误判 —— 原实现用 PCRE 环视，Go 的 RE2 不支持环视，这里手写邻接判定。

import "strings"

// regionKeyword 是一条「关键词 → 地区码」。
type regionKeyword struct {
	Keyword string
	Code    string
}

// nodeRegionKeywords 与 PHP 的 KEYWORDS 逐条同序。
var nodeRegionKeywords = []regionKeyword{
	{"香港", "HK"}, {"HK", "HK"}, {"Hong Kong", "HK"},
	{"台湾", "TW"}, {"TW", "TW"}, {"Taiwan", "TW"},
	{"日本", "JP"}, {"JP", "JP"}, {"Japan", "JP"},
	{"韩国", "KR"}, {"KR", "KR"}, {"Korea", "KR"},
	{"新加坡", "SG"}, {"SG", "SG"}, {"Singapore", "SG"},
	{"美国", "US"}, {"US", "US"}, {"USA", "US"}, {"America", "US"},
	{"英国", "UK"}, {"UK", "UK"}, {"Britain", "UK"},
	{"德国", "DE"}, {"DE", "DE"}, {"Germany", "DE"},
	{"法国", "FR"}, {"FR", "FR"}, {"France", "FR"},
	{"加拿大", "CA"}, {"CA", "CA"}, {"Canada", "CA"},
	{"澳大利亚", "AU"}, {"AU", "AU"}, {"Australia", "AU"},
	{"印度", "IN"}, {"IN", "IN"}, {"India", "IN"},
	{"泰国", "TH"}, {"TH", "TH"}, {"Thailand", "TH"},
	{"马来西亚", "MY"}, {"MY", "MY"}, {"Malaysia", "MY"},
	{"越南", "VN"}, {"VN", "VN"}, {"Vietnam", "VN"},
	{"俄罗斯", "RU"}, {"RU", "RU"}, {"Russia", "RU"},
	{"巴西", "BR"}, {"BR", "BR"}, {"Brazil", "BR"},
	{"土耳其", "TR"}, {"TR", "TR"}, {"Turkey", "TR"},
	{"荷兰", "NL"}, {"NL", "NL"}, {"Netherlands", "NL"},
	{"中国", "CN"}, {"CN", "CN"}, {"China", "CN"},
}

// nodeRegionCode 对应 NodeRegion::code()：识别不出返回 nil（不猜）。
func nodeRegionCode(name string) *string {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	for _, kw := range nodeRegionKeywords {
		if regionKeywordMatches(name, kw.Keyword) {
			code := kw.Code
			return &code
		}
	}
	return nil
}

// regionKeywordMatches 对应 NodeRegion::matches()。
func regionKeywordMatches(name, keyword string) bool {
	if len(keyword) == 2 && isASCIILetter(keyword[0]) && isASCIILetter(keyword[1]) {
		return containsStandaloneWord(name, keyword)
	}
	// stripos(strtoupper($name), strtoupper($keyword))：字节级 ASCII 大写后子串匹配。
	return strings.Contains(strings.ToUpper(name), strings.ToUpper(keyword))
}

// containsStandaloneWord 找 keyword（ASCII 不区分大小写）且前后都不是 ASCII 字母。
func containsStandaloneWord(name, keyword string) bool {
	upper := strings.ToUpper(name)
	needle := strings.ToUpper(keyword)
	for start := 0; start+len(needle) <= len(upper); {
		idx := strings.Index(upper[start:], needle)
		if idx < 0 {
			return false
		}
		pos := start + idx
		beforeOK := pos == 0 || !isASCIILetter(upper[pos-1])
		afterIdx := pos + len(needle)
		afterOK := afterIdx >= len(upper) || !isASCIILetter(upper[afterIdx])
		if beforeOK && afterOK {
			return true
		}
		start = pos + 1
	}
	return false
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}
