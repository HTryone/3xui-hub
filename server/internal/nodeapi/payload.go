package nodeapi

import (
	"fmt"
)

// 请求体校验链：逐行对齐 NodeApiController 的 $request->validate([...])。
//
// 规则顺序与 PHP 完全一致（决定多条错误时的首条文案）；参数上限也照抄
// （push 2000 条、ack 50 条、output 40000 字符）。

const (
	// MaxStatsPerPush 是 push 单次接受的流量用户数上限（与 NodeWsServer::MAX_STATS_PER_REPORT 一致）。
	MaxStatsPerPush = 2000

	// MaxAckResults 是 ack 单次接受的回执条数上限。
	MaxAckResults = 50

	// ackOutputMaxLen 是单条回执输出的字符上限（reality_scan 的结果 JSON 也落在这里）。
	ackOutputMaxLen = 40000
)

// validateRegister 对齐 register() 的校验：
//
//	'xray_version'  => nullable|string|max:64
//	'agent_version' => nullable|string|max:64
//	'kernel_arch'   => nullable|string|max:32
//	'hostname'      => nullable|string|max:128
func validateRegister(data any) error {
	v := newValidator(data)
	v.field("xray_version", Nullable(), String(), Max(64))
	v.field("agent_version", Nullable(), String(), Max(64))
	v.field("kernel_arch", Nullable(), String(), Max(32))
	v.field("hostname", Nullable(), String(), Max(128))
	return collect(v)
}

// validatePush 对齐 push() 的校验：
//
//	'stats'         => required|array|max:2000
//	'stats.*.up'    => nullable|integer|min:0
//	'stats.*.down'  => nullable|integer|min:0
func validatePush(data any) error {
	v := newValidator(data)
	v.field("stats", Required(), ArrayRule(), Max(MaxStatsPerPush))
	v.field("stats.*.up", Nullable(), Integer(), Min(0))
	v.field("stats.*.down", Nullable(), Integer(), Min(0))
	return collect(v)
}

// validateAlive 对齐 alive() 的校验（字段顺序即报错优先级）。
func validateAlive(data any) error {
	v := newValidator(data)
	v.field("xray_version", Nullable(), String(), Max(64))
	v.field("config_version", Nullable(), Integer(), Min(0))
	v.field("cpu", Nullable(), Numeric(), Min(0), Max(100))
	v.field("mem", Nullable(), Numeric(), Min(0), Max(100))
	v.field("uptime", Nullable(), Integer(), Min(0))
	v.field("conns", Nullable(), Integer(), Min(0))
	v.field("active_users", Nullable(), Integer(), Min(0))
	v.field("up_speed", Nullable(), Numeric(), Min(0))
	v.field("down_speed", Nullable(), Numeric(), Min(0))
	v.field("net_total_up", Nullable(), Integer(), Min(0))
	v.field("net_total_down", Nullable(), Integer(), Min(0))
	return collect(v)
}

// validateAck 对齐 ack() 的校验：
//
//	'results'         => required|array|max:50
//	'results.*.id'    => required|integer
//	'results.*.success' => required|boolean
//	'results.*.output'  => nullable|string|max:40000
func validateAck(data any) error {
	v := newValidator(data)
	v.field("results", Required(), ArrayRule(), Max(MaxAckResults))
	v.field("results.*.id", Required(), Integer())
	v.field("results.*.success", Required(), Boolean())
	v.field("results.*.output", Nullable(), String(), Max(ackOutputMaxLen))
	return collect(v)
}

func collect(v *validator) error {
	msgs := v.run()
	if len(msgs) == 0 {
		return nil
	}
	return &ValidationError{Messages: msgs}
}

// PushStats 是 push 的入参（用户 email → 原始累计字节）。
type PushStats struct {
	Email string
	Up    int64
	Down  int64
}

// ParsePushStats 复刻 controller 里对 validated stats 的归一化：
// 只接受「键是字符串、值是数组」的条目，up/down 缺省 0。
func ParsePushStats(ordered any) []PushStats {
	data, _ := mapToPlain(ordered).(map[string]any)
	raw, _ := data["stats"].(map[string]any)
	if raw == nil {
		return nil
	}
	out := make([]PushStats, 0, len(raw))
	for email, pair := range raw {
		p, ok := pair.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, PushStats{
			Email: email,
			Up:    intOf(p["up"]),
			Down:  intOf(p["down"]),
		})
	}
	return out
}

// AckResult 是一条指令回执（对齐 ack 的 results.*）。
//
// OutputSet 对应 PHP 的 isset($row['output'])：显式传 null 与不传都算「无输出」
// （数据库写 null），显式传空串则写 ""。两者在库里的结果不同，必须区分。
type AckResult struct {
	ID        int64
	Success   bool
	Output    string
	OutputSet bool
}

// ParseAckResults 复刻 controller 对 validated results 的读取。
func ParseAckResults(ordered any) []AckResult {
	data, _ := mapToPlain(ordered).(map[string]any)
	raw, _ := data["results"].([]any)
	out := make([]AckResult, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		res := AckResult{
			ID:      intOf(m["id"]),
			Success: boolOf(m["success"]),
		}
		if rawOut, exists := m["output"]; exists && rawOut != nil {
			if s, ok := rawOut.(string); ok {
				res.Output = s
				res.OutputSet = true
			}
		}
		out = append(out, res)
	}
	return out
}

// parseBody 读取并解析请求体；空体视作空对象（PHP 对空 body 得到空数组）。
//
// 返回值保留 JSON 文档顺序（Object），因为 Laravel 的校验报错顺序依赖数据顺序。
func parseBody(raw []byte) (any, error) {
	if len(raw) == 0 {
		return Object{}, nil
	}
	parsed, err := ParseJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if parsed == nil {
		return Object{}, nil
	}
	if _, ok := parsed.(Object); !ok {
		// PHP 的 $request->validate 对 JSON 数组请求体同样按「无字段」处理。
		return Object{}, nil
	}
	return parsed, nil
}
