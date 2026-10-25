package subscription

import (
	"context"
	"regexp"
	"strings"
)

// 逐条复刻 app/Services/ThirdPartyService.php 里订阅注入用到的两段：
//
//	linksForUser() + renameToNeutral() + neutralName()
//
// 规则（产品铁律）：第三方「不管流量、只管时间」—— 套餐勾选了该订阅就注入，
// 而注入前必须把机场原名换成中性的「地区+序号」（用户端零机场痕迹）。

// thirdPartyRegionLabels 是第三方节点中性改名的地区词表。
//
// **顺序有意义**（命中即停）：与 NodeRegion 的表刻意分开 —— 这里的匹配是纯子串
// （mb_strpos），且输出中文标签而不是两位代码。
var thirdPartyRegionLabels = []struct {
	Label string
	Keys  []string
}{
	{"香港", []string{"香港", "HK", "HONG KONG", "HKG"}},
	{"台湾", []string{"台湾", "TW", "TAIWAN", "TWN"}},
	{"日本", []string{"日本", "JP", "JAPAN", "JPN"}},
	{"韩国", []string{"韩国", "KR", "KOREA", "KOR"}},
	{"新加坡", []string{"新加坡", "SG", "SINGAPORE", "SGP"}},
	{"美国", []string{"美国", "US", "USA", "AMERICA"}},
	{"英国", []string{"英国", "UK", "BRITAIN", "GREAT BRITAIN"}},
	{"德国", []string{"德国", "DE", "GERMANY", "DEU"}},
	{"法国", []string{"法国", "FR", "FRANCE", "FRA"}},
	{"加拿大", []string{"加拿大", "CA", "CANADA", "CAN"}},
	{"澳大利亚", []string{"澳大利亚", "澳洲", "AU", "AUSTRALIA", "AUS"}},
	{"泰国", []string{"泰国", "TH", "THAILAND", "THA"}},
	{"马来西亚", []string{"马来西亚", "MY", "MALAYSIA", "MYS"}},
	{"越南", []string{"越南", "VN", "VIETNAM", "VNM"}},
	{"俄罗斯", []string{"俄罗斯", "RU", "RUSSIA", "RUS"}},
	{"土耳其", []string{"土耳其", "TR", "TURKEY", "TUR"}},
	{"印度", []string{"印度", "IN", "INDIA", "IND"}},
}

// thirdPartyLinks 对应 ThirdPartyService::linksForUser()：
// 总开关 → 套餐勾选 → 已启用订阅 → 中性改名 → 去重（保序）。
func (s *Service) thirdPartyLinks(ctx context.Context, plan *Plan, cfg *siteConfigReader) ([]string, error) {
	// 总开关（SiteConfig：默认 0=关闭，一键从所有订阅摘掉第三方）
	if cfg.get("third_party_enabled", "0") != "1" {
		return nil, nil
	}

	var ids []int64
	if plan != nil {
		ids = plan.ThirdPartySubIDs
	}
	if len(ids) == 0 {
		return nil, nil
	}

	subs, err := s.repo.ThirdPartySubs(ctx, ids)
	if err != nil {
		return nil, err
	}

	regionCounters := map[string]int{}
	links := make([]string, 0, len(subs))
	for _, sub := range subs {
		for _, rawLink := range cachedLinks(sub.CachedLinks) {
			links = append(links, renameToNeutral(rawLink, regionCounters))
		}
	}

	// 去重（同地区序号唯一，正常不会重，兜底）
	out := make([]string, 0, len(links))
	seen := make(map[string]bool, len(links))
	for _, link := range links {
		if !seen[link] {
			seen[link] = true
			out = append(out, link)
		}
	}
	return out, nil
}

// cachedLinks 对应 ThirdPartySub::getCachedLinks()：按行切分、trim、丢空行。
func cachedLinks(cached string) []string {
	if cached == "" {
		return nil
	}
	lines := lineSplitPattern.Split(cached, -1)
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if trimmed := phpTrim(line); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// renameToNeutral 对应 renameToNeutral()：把 #fragment（机场原名）换成中性「地区+序号」。
// 无 #fragment 的链接（极少）补一个。绝不保留机场品牌词。
func renameToNeutral(link string, regionCounters map[string]int) string {
	hashPos := strings.LastIndex(link, "#")
	rawName := ""
	if hashPos >= 0 {
		rawName = urldecode(link[hashPos+1:])
	}
	neutral := neutralName(rawName, regionCounters)

	if hashPos < 0 {
		return link + "#" + urlencode(neutral)
	}
	return link[:hashPos] + "#" + urlencode(neutral)
}

// neutralName 对应 neutralName()：由机场节点名生成「地区+序号」（如 香港1 / 线路3）。
func neutralName(rawName string, counters map[string]int) string {
	upper := asciiUpper(rawName)
	label := "线路" // 兜底：识别不到地区也不带机场名

	for _, region := range thirdPartyRegionLabels {
		matched := false
		for _, key := range region.Keys {
			if strings.Contains(upper, asciiUpper(key)) {
				label = region.Label
				matched = true
				break
			}
		}
		if matched {
			break
		}
	}

	counters[label]++
	return label + itoa(int64(counters[label]))
}

// compilePHPRegex 把 PHP 的 `/pattern/flags` 形态编译成 Go regexp。
//
// 只翻译订阅改名实际会用到的修饰符（i / m / s / u / D）；未知修饰符或语法不被 RE2
// 支持时返回错误，调用方按 PHP「正则错误 → 保留原名」处理。
func compilePHPRegex(phpRegex string) (*regexp.Regexp, error) {
	pattern, flags, ok := splitPHPRegex(phpRegex)
	if !ok {
		return nil, errUnsupported
	}

	var opts strings.Builder
	for _, f := range flags {
		switch f {
		case 'i', 'm', 's':
			opts.WriteRune(f)
		case 'u', 'D', 'U', 'X', 'S', 'A', 'J', 'n', 'r', 'x':
			// u 在 Go 里是默认行为；其余修饰符不影响订阅改名的常见写法，
			// 忽略而非报错（PHP 侧这些也都是合法修饰符）。
		default:
			return nil, errUnsupported
		}
	}

	expr := pattern
	if opts.Len() > 0 {
		expr = "(?" + opts.String() + ")" + expr
	}
	return regexp.Compile(expr)
}

// splitPHPRegex 拆出 `/pattern/flags` 的 pattern 与 modifiers（支持任意非字母数字分隔符）。
func splitPHPRegex(s string) (string, string, bool) {
	if len(s) < 3 {
		return "", "", false
	}
	delim := s[0]
	if isASCIILetter(delim) || (delim >= '0' && delim <= '9') || delim == '\\' {
		return "", "", false
	}
	// 找未被转义的收尾分隔符：从右往左扫，跳过转义的。
	idx := -1
	for i := len(s) - 1; i > 0; i-- {
		if s[i] != delim {
			continue
		}
		backslashes := 0
		for j := i - 1; j > 0 && s[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			idx = i
			break
		}
	}
	if idx <= 0 {
		return "", "", false
	}
	return s[1:idx], s[idx+1:], true
}
