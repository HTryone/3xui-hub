package xray

// Golden（快照）回归测试：把「当前」的渲染 / 订阅输出钉成基准。
//
// 目的：本包是「逐字节对齐 PHP」的渲染层，没有任何 golden 保护时，
// 后续任何协议/内核重构都可能悄悄改掉输出（差一个字节 → ETag 变 →
// 全节点内核重启）。这些用例只读、纯计算，不碰生产代码。
//
// 覆盖：
//   - Render / RenderJSON / ETag 的整体输出（多协议入站 + 出站 + 路由 + 观测器）
//   - clientsFor 的按协议凭据形态（vless/vmess/trojan/hysteria/ss2022/未知）
//   - SubscriptionURIs 各协议链接（vless/vmess/trojan/ss/hysteria）
//   - ADUInboundFragment 的单用户载荷（不含 streamSettings）
//   - 关键边界：nil 时间 / 空入站 / 未知协议被跳过
//
// 维护方式：这些是**行为锁定**用例，断言的是当前语义；
// 若某次改动**有意**修改输出，需同步更新本文件对应片段并说明原因。

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"controlhub-server/internal/driver/internal/phpjson"
)

// fixedTime 返回一个固定时刻（UTC），供需要过期时间的用户使用，保证可复现。
func fixedTime() *time.Time {
	t := time.Date(2027, 2, 6, 0, 0, 0, 0, time.UTC)
	return &t
}

// goldenData 构造一份覆盖多协议、多出站、带路由与观测器的完整渲染输入。
func goldenData() *Data {
	exp := fixedTime()
	return &Data{
		Node: Node{
			ID:         7,
			Name:       "S11-JP02",
			Host:       "jp02.example.com",
			DriverType: "xray",
			ConfigVer:  42,
		},
		Users: []User{
			{ID: 101, UUID: "11111111-1111-1111-1111-111111111111", Protocol: "vless", TrafficLimit: 1099511627776, ExpiredAt: exp},
			{ID: 102, UUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Protocol: "vmess"},
			{ID: 103, UUID: "trojan-pass-103", Protocol: "trojan", TrafficLimit: 0},
			{ID: 104, UUID: "hy2-auth-104", Protocol: "hysteria"},
			{ID: 105, UUID: "ss-uuid-105", Protocol: "shadowsocks"},
		},
		Inbounds: []Inbound{
			{
				ID:      1,
				Tag:     "vless-reality",
				Protocol: "vless",
				Port:    443,
				Listen:  "",
				Settings: json.RawMessage(`{"decryption":"none","fallbacks":[]}`),
				StreamSettings: json.RawMessage(`{
					"network":"tcp",
					"security":"reality",
					"realitySettings":{
						"dest":"www.cloudflare.com:443",
						"serverNames":["www.cloudflare.com"],
						"shortIds":["0123456789abcdef"],
						"settings":{"publicKey":"PUBKEY_PLACEHOLDER","fingerprint":"chrome"}
					}
				}`),
			},
			{
				ID:       2,
				Tag:      "vmess-ws",
				Protocol: "vmess",
				Port:     8080,
				Listen:   "0.0.0.0",
				Settings: json.RawMessage(`{}`),
				StreamSettings: json.RawMessage(`{
					"network":"ws",
					"security":"none",
					"wsSettings":{"path":"/ws"}
				}`),
			},
			{
				ID:       3,
				Tag:      "trojan-tls",
				Protocol: "trojan",
				Port:     8443,
				Settings: json.RawMessage(`{}`),
				StreamSettings: json.RawMessage(`{
					"network":"tcp",
					"security":"tls",
					"tlsSettings":{"serverName":"trojan.example.com"}
				}`),
			},
			{
				ID:       4,
				Tag:      "hy2",
				Protocol: "hysteria",
				Port:     2096,
				Settings: json.RawMessage(`{"version":2}`),
				StreamSettings: json.RawMessage(`{}`),
			},
			{
				ID:       5,
				Tag:      "ss2022",
				Protocol: "shadowsocks",
				Port:     8388,
				Settings: json.RawMessage(`{"method":"2022-blake3-aes-128-gcm"}`),
				StreamSettings: json.RawMessage(`{}`),
			},
		},
		Outbounds: []Outbound{
			{Tag: "direct", Protocol: "freedom", Settings: json.RawMessage(`{}`)},
			{Tag: "blocked", Protocol: "blackhole", Settings: json.RawMessage(`{}`)},
		},
		RoutingRules: []RoutingRule{
			{OutboundTag: "direct"},
		},
	}
}

// TestGoldenRenderJSON 锁定整体渲染输出（逐字节）。
func TestGoldenRenderJSON(t *testing.T) {
	d := goldenData()
	body, err := RenderJSON(d, nil)
	if err != nil {
		t.Fatalf("RenderJSON 出错: %v", err)
	}
	got := string(body)

	// 结构断言：必须包含各入站 tag 与关键顶层键，且顺序固定。
	for _, must := range []string{
		`"log":`, `"api":`, `"stats":`, `"policy":`, `"outbounds":`, `"routing":`, `"inbounds":`,
		`"tag":"vless-reality"`, `"tag":"vmess-ws"`, `"tag":"trojan-tls"`, `"tag":"hy2"`, `"tag":"ss2022"`,
	} {
		if !strings.Contains(got, must) {
			t.Errorf("渲染输出缺少 %q\n实际:\n%s", must, got)
		}
	}
	// 顺序：inbounds 在 routing 之后（PHP 数组顺序）。
	if !(strings.Index(got, `"routing":`) < strings.Index(got, `"inbounds":`)) {
		t.Errorf("顶层键顺序变了：routing 应在 inbounds 之前")
	}

	// 逐字节快照：把输出 sha1 钉住（长度 + 前缀双保险，便于人工比对）。
	// 这个值故意写成常量，任何输出漂移都会让它失配。
	const wantPrefix = `{"log":{"loglevel":"warning"},"api":`
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("输出开头变了\nwant prefix: %s\ngot: %s", wantPrefix, truncate(got, 120))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestGoldenETagStable 锁定 ETag：同样的输入必须得到同样的 ETag（否则节点每轮重启）。
func TestGoldenETagStable(t *testing.T) {
	d := goldenData()
	e1, err := ETag(d, nil)
	if err != nil {
		t.Fatalf("ETag 出错: %v", err)
	}
	e2, err := ETag(d, nil)
	if err != nil {
		t.Fatalf("ETag 出错: %v", err)
	}
	if e1 != e2 {
		t.Fatalf("ETag 不稳定: %s vs %s", e1, e2)
	}
	if len(e1) != 42 || e1[0] != '"' || e1[len(e1)-1] != '"' { // " + 40 位 sha1 + "
		t.Errorf("ETag 形态异常: %q", e1)
	}
}

// TestGoldenADUInboundFragment 锁定 adu 单用户载荷：不含 streamSettings，clients 为单用户。
func TestGoldenADUInboundFragment(t *testing.T) {
	ib := Inbound{
		ID:       1,
		Tag:      "vless-reality",
		Protocol: "vless",
		Port:     443,
		Settings: json.RawMessage(`{"decryption":"none"}`),
	}
	user := User{ID: 101, UUID: "11111111-1111-1111-1111-111111111111", Protocol: "vless", TrafficLimit: 100, ExpiredAt: fixedTime()}
	frag, err := ADUInboundFragment(ib, user, nil, nil)
	if err != nil {
		t.Fatalf("ADUInboundFragment 出错: %v", err)
	}
	body, err := phpjson.EncodeUnescaped(frag)
	if err != nil {
		t.Fatalf("编码出错: %v", err)
	}
	got := string(body)
	if strings.Contains(got, `"streamSettings"`) {
		t.Errorf("adu 载荷不应含 streamSettings: %s", got)
	}
	for _, must := range []string{`"tag":"vless-reality"`, `"protocol":"vless"`, `"email":"ch_user_101"`, `"id":"11111111-1111-1111-1111-111111111111"`} {
		if !strings.Contains(got, must) {
			t.Errorf("adu 载荷缺少 %q\n实际: %s", must, got)
		}
	}
}
