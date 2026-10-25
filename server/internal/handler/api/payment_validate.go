package api

import (
	"strings"

	"github.com/gin-gonic/gin"
)

// 支付端点用到的 Laravel 校验规则子集：required / integer / string / max / sometimes / nullable。
//
// 为什么不复用 validate.go 的 validator：那个是 P2 建的，只覆盖认证端点用到的六条规则，
// 而 P7 的任务边界要求「不碰 handler/api 已有文件」。这里独立实现同一套**可观测行为**
// （文案、顺序、合并规则），失败时仍然走 respondValidation 的同款响应形态：
// HTTP 200 + {"code":500,"msg":"..."}（bootstrap/app.php 的 render 回调抢在 422 之前，
// 这是 PHP 现状，不是笔误）。
//
// 文案来源：vendor/laravel/framework/.../lang/en/validation.php（实测 PHP 8.5 + Laravel 13.16）：
//
//	required → The :attribute field is required.
//	integer  → The :attribute field must be an integer.
//	string   → The :attribute field must be a string.
//	max      → The :attribute field must not be greater than :max characters.   （字符串按 mb_strlen）
//
// :attribute 是字段名把下划线换成空格（"plan_id" → "plan id"）。

// paymentValidator 是「字段 → 规则」的有序校验器。
type paymentValidator struct {
	data   map[string]any
	fields []paymentField
	errs   []ruleError
}

type paymentField struct {
	name  string
	rules []string
}

func newPaymentValidator(data map[string]any) *paymentValidator {
	return &paymentValidator{data: data, errs: []ruleError{}}
}

func (v *paymentValidator) field(name string, rules ...string) *paymentValidator {
	v.fields = append(v.fields, paymentField{name: name, rules: rules})
	return v
}

// passes 执行校验；顺序与 Laravel 一致（先 required，再其余规则）。
func (v *paymentValidator) passes() bool {
	for _, f := range v.fields {
		value, present := v.data[f.name]
		v.apply(f, value, present)
	}
	return len(v.errs) == 0
}

func (v *paymentValidator) apply(f paymentField, value any, present bool) {
	sometimes := false
	nullable := false
	for _, r := range f.rules {
		switch r {
		case "sometimes":
			sometimes = true
		case "nullable":
			nullable = true
		}
	}
	// sometimes：字段不存在就整条跳过（Laravel 的 sometimes 语义）
	if sometimes && !present {
		return
	}

	attribute := phpStrReplaceUnderscore(f.name)

	for _, rule := range f.rules {
		name, params := splitRule(rule)
		switch name {
		case "sometimes", "nullable":
			continue

		case "required":
			if !requiredPasses(value) {
				v.add(f.name, "The "+attribute+" field is required.")
				return // required 失败后不再跑其余规则（Laravel 的 shouldStopValidating）
			}

		case "integer":
			// nullable + null → 跳过（"sometimes|nullable|integer" 里 null 是合法的「不指定」）
			if nullable && value == nil {
				continue
			}
			if !present || value == nil {
				continue
			}
			if !phpIsInteger(value) {
				v.add(f.name, "The "+attribute+" field must be an integer.")
			}

		case "string":
			if nullable && value == nil {
				continue
			}
			if !present || value == nil {
				continue
			}
			if !phpValueIsString(value) {
				v.add(f.name, "The "+attribute+" field must be a string.")
			}

		case "max":
			if nullable && value == nil {
				continue
			}
			if !present || value == nil {
				continue
			}
			limit := atoiSafe(paramsAt(params, 0))
			if sizeOf(value) > limit {
				v.add(f.name, "The "+attribute+" field must not be greater than "+paramsAt(params, 0)+" characters.")
			}
		}
	}
}

func (v *paymentValidator) add(field, msg string) {
	v.errs = append(v.errs, ruleError{Field: field, Msg: msg})
}

// summary 复刻 ValidationException::summarize：第一条 + " (and N more errors)"。
func (v *paymentValidator) summary() string {
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

// phpStrReplaceUnderscore 是 Laravel 的 attribute 文案化：把下划线换成空格。
func phpStrReplaceUnderscore(s string) string { return strings.ReplaceAll(s, "_", " ") }

// respondPaymentValidation 把校验失败渲染成 PHP 的形态：HTTP 200 + code 500 + 合并后的文案。
//
// ⚠️ 与 P2 的 respondValidation 同款（PHP 现状：bootstrap/app.php 的 render 回调抢在 422
// 分支之前，取 getStatusCode() 拿到 500）。前端只读 msg，所以文案必须逐字一致。
func respondPaymentValidation(c *gin.Context, v *paymentValidator) {
	fail(c, v.summary(), 500)
}
