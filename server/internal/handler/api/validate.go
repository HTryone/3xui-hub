package api

import (
	"strings"
)

// 本文件复刻 Laravel Validator 在**本模块用到的四条规则**（required / email / string / size / min /
// in）上的可观测行为：哪些输入过、不过时报什么文案、多条错误如何合并。
//
// 为什么不用手写 if：PHP 侧的失败响应是
//
//	HTTP 200 + {"code":500,"msg":"The email field is required. (and 2 more errors)"}
//
// —— 文案由框架产生、code 恒为 500（bootstrap/app.php 的 render 回调接管了 422 分支）。
// 逐字节对齐这层文案与合并规则，才能保证前端提示与 PHP 版一致（前端直接展示 msg）。
//
// 全局中间件（TrimStrings / ConvertEmptyStringsToNull）在取输入时就已模拟，见 request.go。

// ruleError 是一条校验失败（字段 + 文案）。
type ruleError struct {
	Field string
	Msg   string
}

// rules 是「字段 → 规则列表」的有序切片（顺序与 PHP 里字段声明顺序一致，决定文案顺序）。
type ruleField struct {
	field string
	rules []string
}

// validator 持有待校验的输入（已做 trim / 空串转 null）。
type validator struct {
	data   map[string]any
	fields []ruleField
	errs   []ruleError
}

func newValidator(data map[string]any) *validator {
	return &validator{data: data, errs: []ruleError{}}
}

// field 声明一个字段的规则；规则串与 Laravel 写法一致（如 "required", "email", "size:6", "in:vless,trojan"）。
func (v *validator) field(name string, rules ...string) *validator {
	v.fields = append(v.fields, ruleField{field: name, rules: rules})
	return v
}

// passes 执行校验并返回是否全部通过。
func (v *validator) passes() bool {
	for _, f := range v.fields {
		value, present := v.data[f.field]
		v.applyField(f, value, present)
	}
	return len(v.errs) == 0
}

// applyField 按顺序应用规则，模拟 Validator::isValidatable + shouldStopValidating：
//   - required 是隐式规则：缺失 / null / 空白字符串时也执行；
//   - 其余规则只在「字段存在且值非空」时执行（isValidatable → presentOrRuleIsImplicit）；
//   - 字段一旦有 required 失败，后续规则停止（shouldStopValidating 的 implicitRules 分支），
//     所以空字段只会产生一条 required 错误。
func (v *validator) applyField(f ruleField, value any, present bool) {
	nullValue := present && value == nil
	blankString := false
	if s, ok := value.(string); ok && phpTrimSpaceOnly(s) == "" {
		blankString = true
	}

	for _, rule := range f.rules {
		name, params := splitRule(rule)

		if isImplicitRule(name) {
			if msg, failed := v.checkRule(name, params, f.field, value); failed {
				v.addError(f.field, msg)
			}
			if v.hasFieldError(f.field) {
				return // required 失败 → 后面的规则不再跑
			}
			continue
		}

		// 非隐式规则：字段缺失 / null / 空白字符串时跳过（PHP 认为「没有可校验的值」）。
		if !present || nullValue || blankString {
			continue
		}
		if msg, failed := v.checkRule(name, params, f.field, value); failed {
			v.addError(f.field, msg)
		}
	}
}

func (v *validator) hasFieldError(field string) bool {
	for _, e := range v.errs {
		if e.Field == field {
			return true
		}
	}
	return false
}

// checkRule 判定单条规则；返回 (文案, 是否失败)。
func (v *validator) checkRule(name string, params []string, field string, value any) (string, bool) {
	switch name {
	case "required":
		if !requiredPasses(value) {
			return "The " + field + " field is required.", true
		}
		return "", false

	case "email":
		if !emailPasses(value) {
			return "The " + field + " field must be a valid email address.", true
		}
		return "", false

	case "string":
		if !phpValueIsString(value) {
			return "The " + field + " field must be a string.", true
		}
		return "", false

	case "size":
		want := paramsAt(params, 0)
		if !sizeEquals(value, want) {
			return "The " + field + " field must be " + want + " characters.", true
		}
		return "", false

	case "min":
		want := paramsAt(params, 0)
		if sizeOf(value) < atoiSafe(want) {
			return "The " + field + " field must be at least " + want + " characters.", true
		}
		return "", false

	case "in":
		if !phpInArray(value, params) {
			return "The selected " + field + " is invalid.", true
		}
		return "", false
	}
	return "", false
}

func (v *validator) addError(field, msg string) {
	v.errs = append(v.errs, ruleError{Field: field, Msg: msg})
}

// summary 复刻 ValidationException::summarize：第一条消息 + " (and N more errors)"。
func (v *validator) summary() string {
	if len(v.errs) == 0 {
		return ""
	}
	first := v.errs[0].Msg
	if n := len(v.errs) - 1; n > 0 {
		plural := "error"
		if n > 1 {
			plural = "errors"
		}
		return first + " (and " + itoaInt(n) + " more " + plural + ")"
	}
	return first
}

// ---- 规则实现 ----

// isImplicitRule 与 Laravel 同名单里的本模块子集。
func isImplicitRule(name string) bool { return name == "required" }

// requiredPasses 对齐 validateRequired：null / 空白字符串 / 空数组 视为未填。
func requiredPasses(value any) bool {
	switch t := value.(type) {
	case nil:
		return false
	case string:
		return phpTrimSpaceOnly(t) != ""
	case bool:
		return true
	case map[string]any:
		return len(t) > 0
	case []any:
		return len(t) > 0
	}
	// 数字（含 0）与其它标量都算已填（PHP 里 is_null / '' / 空数组之外都为 true）。
	return true
}

// sizeOf 对齐 getSize 在本模块的路径：数组取元素数，其余取 mb_strlen((string)$value)。
func sizeOf(value any) int {
	switch t := value.(type) {
	case nil:
		return 0
	case string:
		return phpMbStrlen(t)
	case []any:
		return len(t)
	case map[string]any:
		return len(t)
	}
	s, ok := phpStringValue(value)
	if !ok {
		return 0
	}
	return phpMbStrlen(s)
}

// sizeEquals 对齐 validateSize：BigNumber 比字符串参数（数值相等即可，"6" == "06"）。
func sizeEquals(value any, want string) bool {
	return sizeOf(value) == atoiSafe(want)
}

// emailPasses 对齐 validateEmail + RFCValidation（egulias/email-validator）在本项目的可观测结果。
//
// RFCValidation 的具体口径（对拍自真实 PHP，见 validate_test.go 的语料）：
//   - 非字符串（对象转字符串除外）→ 失败；含 \r \n → 失败；
//   - 必须「有且仅有一个 @」（不含引号包裹的 local part 时）；
//   - local part 允许 UTF-8（用户@b.co 通过）、允许引号包裹任意字符（"a b"@c.co 通过）；
//   - domain 允许 IP 字面量 [1.2.3.4]、允许纯数字标签（a@1.2.3.4、a@b.123 通过）；
//   - domain 不允许下划线、不允许首尾 '-'、不允许空标签（连续点）、不允许结尾点；
//   - 整体首尾空格不通过（全局 TrimStrings 已去掉首尾空格，所以这里只处理内部）。
//
// 这里的实现覆盖上述边界；与 RFC 的极端差异（注释/域字面量 IPv6 全格式）按「失败」处理，
// 与常见客户端行为一致，且不影响契约字段。
func emailPasses(value any) bool {
	s, ok := value.(string)
	if !ok {
		return false
	}
	if strings.ContainsAny(s, "\r\n") {
		return false
	}
	if s == "" {
		return false
	}

	at := strings.LastIndexByte(s, '@')
	if at <= 0 || at == len(s)-1 {
		return false
	}
	// 未加引号时，local part 内不允许再来一个 @。
	quoted := strings.HasPrefix(s, `"`)
	if !quoted && strings.Count(s, "@") != 1 {
		return false
	}
	local, domain := s[:at], s[at+1:]

	if !localPasses(local, quoted) {
		return false
	}
	return domainPasses(domain)
}

func localPasses(local string, quoted bool) bool {
	if local == "" {
		return false
	}
	if quoted {
		// "..."：必须成对引号，内部允许空格等任意可打印字符（不含控制字符）。
		if len(local) < 2 || local[len(local)-1] != '"' {
			return false
		}
		for _, r := range local[1 : len(local)-1] {
			if r < 0x20 || r == 0x7F {
				return false
			}
		}
		return true
	}
	// 未加引号：允许 dot-atom 的 atext 集合（RFC 5322 §3.2.3）+ 非 ASCII 可见字符。
	if strings.HasPrefix(local, ".") || strings.HasSuffix(local, ".") || strings.Contains(local, "..") {
		return false
	}
	const atext = "!#$%&'*+-/=?^_`{|}~."
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune(atext, r):
		case r > 0x7F && r != 0x7F:
			// 用户@b.co 在 RFCValidation 下通过（UTF-8 local part）。
		default:
			return false
		}
	}
	return true
}

func domainPasses(domain string) bool {
	if domain == "" {
		return false
	}
	// IP 字面量：[1.2.3.4] / [IPv6:...]
	if strings.HasPrefix(domain, "[") && strings.HasSuffix(domain, "]") {
		inner := domain[1 : len(domain)-1]
		if strings.HasPrefix(inner, "IPv6:") {
			return strings.Contains(inner, ":")
		}
		return ipv4Literal(inner)
	}
	if strings.HasSuffix(domain, ".") {
		return false
	}
	for _, label := range strings.Split(domain, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		if strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
			default:
				return false // 下划线等一律不合法
			}
		}
	}
	return true
}

func ipv4Literal(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return false
	}
	for _, p := range parts {
		if p == "" || len(p) > 3 {
			return false
		}
		n := 0
		for i := 0; i < len(p); i++ {
			if p[i] < '0' || p[i] > '9' {
				return false
			}
			n = n*10 + int(p[i]-'0')
		}
		if n > 255 {
			return false
		}
	}
	return true
}

// ---- 小工具 ----

func splitRule(rule string) (string, []string) {
	if i := strings.IndexByte(rule, ':'); i >= 0 {
		params := strings.Split(rule[i+1:], ",")
		for i := range params {
			params[i] = strings.TrimSpace(params[i])
		}
		return rule[:i], params
	}
	return rule, nil
}

func paramsAt(params []string, i int) string {
	if i < len(params) {
		return params[i]
	}
	return ""
}

func atoiSafe(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return -1
		}
		n = n*10 + int(s[i]-'0')
	}
	if len(s) == 0 {
		return -1
	}
	return n
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// phpTrimSpaceOnly 对齐 PHP trim() 在「仅空白」判定时的行为：
// Validator 用 trim($value) === ”（默认字符集 " \t\n\r\0\x0B"）。
func phpTrimSpaceOnly(s string) string { return strings.Trim(s, " \t\n\r\x00\x0B") }
