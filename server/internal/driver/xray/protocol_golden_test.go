package xray

// Golden 回归测试（续）：按协议的凭据形态与订阅链接。
//
// 这两块是「加协议/换内核」最常被碰的地方，也是散落 switch 最多的地方
// （xray/config.go 的 clientsFor、xray/subscription.go 的 SubscriptionURIs）。
// 先把当前语义钉住，未来抽 protocol 包时以本文件为准绳。

import (
	"encoding/json"
	"strings"
	"testing"

	"controlhub-server/internal/driver/internal/phpjson"
)

// clientKeys 把 clientsFor 的结果编码后，取出「每个用户凭据里用来认人的键名」。
// vless/vmess → id，trojan → password，hysteria → auth，ss2022 → password(PSK)。
func encodeClients(t *testing.T, d *Data, ib Inbound) (string, bool) {
	t.Helper()
	clients, has, err := clientsFor(d, ib)
	if err != nil {
		t.Fatalf("clientsFor(%s) 出错: %v", ib.Protocol, err)
	}
	if !has {
		return "", false
	}
	body, err := phpjson.EncodeUnescaped(clients)
	if err != nil {
		t.Fatalf("编码 clients 出错: %v", err)
	}
	return string(body), true
}

// TestGoldenClientsForPerProtocol 锁定每种协议的凭据键名与形态。
func TestGoldenClientsForPerProtocol(t *testing.T) {
	d := goldenData()

	cases := []struct {
		protocol  string
		wantKey   string // 用来认人的键名
		wantNoKey []string
	}{
		{protocol: "vless", wantKey: `"id":`},
		{protocol: "vmess", wantKey: `"id":`},
		{protocol: "trojan", wantKey: `"password":`},
		{protocol: "hysteria", wantKey: `"auth":`},
		{protocol: "shadowsocks", wantKey: `"password":`}, // ss2022 多用户：password 是 PSK
	}

	for _, tc := range cases {
		t.Run(tc.protocol, func(t *testing.T) {
			ib := Inbound{ID: 1, Tag: tc.protocol, Protocol: tc.protocol, Settings: json.RawMessage(`{"method":"2022-blake3-aes-128-gcm"}`), StreamSettings: json.RawMessage(`{}`)}
			got, has := encodeClients(t, d, ib)
			if !has {
				t.Fatalf("%s 应注入 clients，实际未注入", tc.protocol)
			}
			if !strings.Contains(got, tc.wantKey) {
				t.Errorf("%s 凭据键名应为 %s\n实际: %s", tc.protocol, tc.wantKey, got)
			}
			// 每个用户都要带上 email 与 totalGB/expiryTime 四件套
			for _, must := range []string{`"email":"ch_user_101"`, `"totalGB":`, `"expiryTime":`} {
				if !strings.Contains(got, must) {
					t.Errorf("%s 凭据缺少 %q\n实际: %s", tc.protocol, must, got)
				}
			}
		})
	}
}

// TestGoldenClientsForUnknownProtocolSkipped 未知协议：match 返回 null → 不注入该用户。
func TestGoldenClientsForUnknownProtocolSkipped(t *testing.T) {
	d := goldenData()
	ib := Inbound{ID: 1, Tag: "x", Protocol: "socks", Settings: json.RawMessage(`{}`), StreamSettings: json.RawMessage(`{}`)}
	clients, has, err := clientsFor(d, ib)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	// socks 走 default → continue，clients 为空数组（hasClients 仍为 true，因为 ss 单凭据判断只针对 shadowsocks）
	if has && len(clients) != 0 {
		t.Errorf("未知协议不应注入任何用户凭据，实际: %v", clients)
	}
}

// TestGoldenClientsForNonSS2022Shadowsocks 非 2022 的 ss：单凭据入站，不注入 clients。
func TestGoldenClientsForNonSS2022Shadowsocks(t *testing.T) {
	d := goldenData()
	ib := Inbound{ID: 1, Tag: "ss", Protocol: "shadowsocks", Settings: json.RawMessage(`{"method":"aes-256-gcm"}`), StreamSettings: json.RawMessage(`{}`)}
	_, has, err := clientsFor(d, ib)
	if err != nil {
		t.Fatalf("出错: %v", err)
	}
	if has {
		t.Errorf("非 2022 的 shadowsocks 不应注入 clients（单凭据入站）")
	}
}

// TestGoldenSubscriptionURIs 锁定各协议订阅链接的前缀与关键参数。
func TestGoldenSubscriptionURIs(t *testing.T) {
	d := goldenData()
	node := Node{Name: "S11-JP02", Host: "jp02.example.com"}

	// vless reality 入站
	uris := SubscriptionURIs(node, d.Inbounds[:1], d.Users[0])
	if len(uris) != 1 {
		t.Fatalf("vless 应生成 1 条链接，实际 %d", len(uris))
	}
	if !strings.HasPrefix(uris[0], "vless://") {
		t.Errorf("vless 链接前缀错误: %s", uris[0])
	}
	for _, must := range []string{"@jp02.example.com:443", "security=reality", "type=tcp", "pbk="} {
		if !strings.Contains(uris[0], must) {
			t.Errorf("vless 链接缺少 %q\n实际: %s", must, uris[0])
		}
	}

	// 逐协议前缀断言
	prefixes := map[string]string{
		"vmess":       "vmess://",
		"trojan":      "trojan://",
		"hysteria":    "hysteria2://",
		"shadowsocks": "ss://",
	}
	for i := 1; i < len(d.Inbounds); i++ {
		ib := d.Inbounds[i]
		u := d.Users[0] // 同一用户，各协议通用
		got := SubscriptionURIs(node, []Inbound{ib}, u)
		if len(got) != 1 {
			t.Errorf("%s 应生成 1 条链接，实际 %d", ib.Protocol, len(got))
			continue
		}
		if !strings.HasPrefix(got[0], prefixes[ib.Protocol]) {
			t.Errorf("%s 链接前缀应为 %s，实际: %s", ib.Protocol, prefixes[ib.Protocol], got[0])
		}
	}
}

// TestGoldenSubscriptionURIsSkipUnknown 未知协议不入链接。
func TestGoldenSubscriptionURIsSkipUnknown(t *testing.T) {
	node := Node{Name: "n", Host: "h.example.com"}
	user := User{ID: 1, UUID: "u-1"}
	ib := Inbound{ID: 1, Tag: "x", Protocol: "socks", Port: 1080, Settings: json.RawMessage(`{}`), StreamSettings: json.RawMessage(`{}`)}
	got := SubscriptionURIs(node, []Inbound{ib}, user)
	if len(got) != 0 {
		t.Errorf("未知协议不应生成链接，实际: %v", got)
	}
}
