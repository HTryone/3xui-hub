// Package ws 是节点 WebSocket 长连接通道：替换 backend/app/WebSocket/{NodeWsServer,NodeRegistry}.php
// （workerman），协议与常量逐字保留。
//
// 定位：面板侧「主通道」。节点 agent 主动出连、X-Node-Secret 鉴权后保持长连接；
// 面板顺着连接主动推指令（commands）/ 配置变更通知（config.changed），
// 节点顺路上报流量 + 监控指标（report）与指令回执（ack）。
//
// 可靠性设计（与 PHP 同构）：
//   - 指令推送前先原子认领（pending→acknowledged），与 HTTP 长轮询共用一套认领逻辑
//     （nodeapi.Repo.ClaimPending，behavior-contract.md §6），双通道并发也不会重复下发；
//   - 推送到 agent 的流量/指标与 HTTP /push /alive 走完全相同的落账实现
//     （nodeapi.Repo.SyncNodeFromPush / MergeDriverConfig 口径），两条通道永远一致；
//   - 僵死连接：75s 未收到任何消息（含 pong）即断开，agent 侧会自动重连；
//   - 所有回调错误兜底 + 单条消息出错绝不影响服务进程。
//
// 消息协议（应用层 JSON：{"event": ..., "data": {...}}）：
//
//	上行 hello / report / ack / pong
//	下行 hello.ok / commands / config.changed / ping
package ws

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// ── 协议常量（NodeWsServer.php 的 private const 逐字保留：名字与取值都不得改动） ──
//
// 命名刻意保留 PHP 的下划线形态：这是与 agent 对齐的协议面，
// 「逐字保留」优先于 Go 命名习惯（go vet 不报，人眼 grep 可对拍）。
const (
	// PING_INTERVAL 心跳：每 30s 向所有连接发 ping。
	PING_INTERVAL = 30
	// STALE_SECONDS 僵死判定：75s 无任何消息（> 2×心跳周期）即断开。
	STALE_SECONDS = 75
	// POLL_INTERVAL 轮询节拍（秒）：pending 指令 + config 版本变更检查。
	POLL_INTERVAL = 2
	// AUTH_TIMEOUT 未鉴权连接存活上限（秒）。
	AUTH_TIMEOUT = 10
	// MAX_COMMANDS_PER_PUSH 单次推送的指令条数上限（与 HTTP execute 一致）。
	MAX_COMMANDS_PER_PUSH = 5
	// MAX_STATS_PER_REPORT 单次上报的流量用户数上限（与 HTTP /push 的 max 校验一致）。
	MAX_STATS_PER_REPORT = 2000
	// OVERFLOW_LOG_INTERVAL 超限告警的节流间隔（秒）：agent 每 5s 推一轮，不节流会把日志刷爆。
	OVERFLOW_LOG_INTERVAL = 60
	// LOG_ROTATE_BYTES node-ws.log 轮转阈值（字节）：Workerman 只追加写、无内建轮转，超限即改名归档。
	LOG_ROTATE_BYTES = 20 * 1024 * 1024
)

// 上行事件名（agent → 面板）。
const (
	EventHello  = "hello"
	EventReport = "report"
	EventAck    = "ack"
	EventPong   = "pong"
)

// 下行事件名（面板 → agent）。
const (
	EventHelloOK       = "hello.ok"
	EventCommands      = "commands"
	EventConfigChanged = "config.changed"
	EventPing          = "ping"
	EventError         = "error"
)

// message 是应用层信封 {"event":..., "data":...}。
//
// PHP：$msg = ['event' => $event]; if ($data !== null) $msg['data'] = $data;
// —— data 为 null 时**整个键不出现**（ping 就是 {"event":"ping"}）。
type message struct {
	Event string
	Data  any // nil → 不编码 data 键
}

// MarshalJSON 按 PHP 的键序输出 {"event","data"}，data 为 null 时省略该键。
func (m message) MarshalJSON() ([]byte, error) {
	ev, err := encodePHP(m.Event)
	if err != nil {
		return nil, err
	}
	if m.Data == nil {
		return append(append([]byte(`{"event":`), ev...), '}'), nil
	}
	data, err := encodePHP(m.Data)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 16+len(ev)+len(data))
	out = append(out, `{"event":`...)
	out = append(out, ev...)
	out = append(out, `,"data":`...)
	out = append(out, data...)
	out = append(out, '}')
	return out, nil
}

// inbound 是上行消息的解码形态。data 不是对象时按空对象处理（PHP：is_array 才取）。
type inbound struct {
	Event string
	Data  map[string]any
}

// decodeInbound 解析上行消息。非法 JSON → (inbound{}, false)（PHP：!is_array($msg) → return）。
func decodeInbound(raw []byte) (inbound, bool) {
	var env struct {
		Event string          `json:"event"`
		Data  json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return inbound{}, false
	}
	out := inbound{Event: env.Event}
	if len(env.Data) > 0 && !bytes.Equal(bytes.TrimSpace(env.Data), []byte("null")) {
		var obj map[string]any
		if err := json.Unmarshal(env.Data, &obj); err == nil && obj != nil {
			out.Data = obj
		}
	}
	if out.Data == nil {
		out.Data = map[string]any{}
	}
	return out, true
}

// encodePHP 复刻 json_encode($v, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES)：
// 中文/非 ASCII 原样输出、斜杠不转义、<>& 不做 < 形态的 HTML 转义。
//
// 与 HTTP 侧（PHP 默认参数：转义斜杠/中文）是两套口径，别混用。
func encodePHP(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("ws: 编码下行消息失败: %w", err)
	}
	// Encoder.Encode 会补一个 \n，信封拼接不需要它。
	out := bytes.TrimRight(buf.Bytes(), "\n")
	return out, nil
}

// eventBytes 产出一条完整下行消息的字节（不含换行）。
func eventBytes(event string, data any) ([]byte, error) {
	return encodePHP(message{Event: event, Data: data})
}

// closeError 是断开前发的 error 消息形态（PHP closeWith 的 payload）。
type closeError struct {
	Message string `json:"message"`
}

// errorPayload 复刻 closeWith() 的 {"event":"error","data":{"message": reason}}。
func errorPayload(reason string) []byte {
	b, err := eventBytes(EventError, closeError{Message: reason})
	if err != nil {
		// 信封结构固定、reason 是我们自己拼的字面量，理论不可达；
		// 真出错也必须给出一个合法 JSON 的断开理由，不能发半截。
		return []byte(`{"event":"error","data":{"message":"close failed"}}`)
	}
	return b
}

// mbSubstr 复刻 mb_substr($s, 0, 128)（出站 tag 截断用）。
func mbSubstr(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit])
}
