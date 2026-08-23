package singbox

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
)

// 對準官方 cedar2025/Xboard-Node #35：面板加自訂 outbound／route 後
// sing-box 起不來，官方錯誤形狀是
//
//	parse sing-box options: outbounds[1].servers: json: unknown field "servers"
//
// 審核 2 鎖定：
//   - SOCKS／Shadowsocks 用面板形狀 tag＋protocol＋settings 必須能起核
//   - route 指到該 outbound 也要過
//   - 不得再出現 unknown field "servers"
//   - 不准改切 xray 當修
//   - 不准關掉自訂 outbound／route 當修
//
// 官方 issue 評論的面板形狀（settings 內是扁的 server／server_port）：
//
//	[{"tag":"proxy-name","protocol":"socks","settings":{"server":"127.0.0.1","server_port":1080,"version":"5","username":"","password":""}}]
//	[{"tag":"proxy-name","protocol":"shadowsocks","settings":{"server":"127.0.0.1","server_port":1080,"method":"...","password":"...","plugin":"","plugin_opts":"","network":"udp"}}]
//	[{"action":"route","outbound":"proxy-name"}]  （也可帶 domain／domain_suffix）
//
// 官方 log 的 unknown field "servers" 來自把 xray 風格 settings.servers
// 原樣餵給 sing-box（sing-box socks／ss 是扁的 server／server_port）。
// 本倉庫 docs-custom-outbounds.md 還示範 socks 用 servers 陣列——那正是 #35 的坑。
//
// 失敗測試應走真實起核路徑（面板 JSON → GetConfig → NodeSpec → Start），
// 鎖定「不得再出現 unknown field servers」，且 SOCKS／SS＋route 能 Start。
// 不要只 assert JSON 長得像。這份測試只鎖行為，不實作修正。

const (
	issue35OfficialError = `unknown field "servers"`
	issue35ProxyTag      = "proxy-name"
)

func TestCustomOutbound_官方SOCKS形狀必須起核(t *testing.T) {
	k := startIssue35Kernel(t, issue35PanelJSON(issue35OfficialSOCKS(), nil))
	defer k.Stop()
	assertIssue35RunningSingBox(t, k)
	assertIssue35OutboundLive(t, k, issue35ProxyTag, "socks")
}

func TestCustomOutbound_官方SS形狀必須起核(t *testing.T) {
	k := startIssue35Kernel(t, issue35PanelJSON(issue35OfficialShadowsocks(), nil))
	defer k.Stop()
	assertIssue35RunningSingBox(t, k)
	assertIssue35OutboundLive(t, k, issue35ProxyTag, "shadowsocks")
}

func TestCustomOutbound_官方route指到outbound必須起核(t *testing.T) {
	t.Run("action_route_outbound", func(t *testing.T) {
		k := startIssue35Kernel(t, issue35PanelJSON(issue35OfficialSOCKS(), issue35OfficialRoute()))
		defer k.Stop()
		assertIssue35RunningSingBox(t, k)
		assertIssue35OutboundLive(t, k, issue35ProxyTag, "socks")
		assertIssue35RoutePointsTo(t, k, issue35ProxyTag)
	})
	t.Run("domain_and_domain_suffix", func(t *testing.T) {
		k := startIssue35Kernel(t, issue35PanelJSON(issue35OfficialSOCKS(), issue35OfficialDomainRoute()))
		defer k.Stop()
		assertIssue35RunningSingBox(t, k)
		assertIssue35OutboundLive(t, k, issue35ProxyTag, "socks")
		assertIssue35RoutePointsTo(t, k, issue35ProxyTag)
	})
}

func TestCustomOutbound_xray風格servers不得再出現unknown_field_servers(t *testing.T) {
	// 官方 #35 現場／本倉庫 docs 的坑：settings.servers 原樣進 sing-box。
	t.Run("socks", func(t *testing.T) {
		k := startIssue35Kernel(t, issue35PanelJSON(issue35XrayStyleSOCKS(), nil))
		defer k.Stop()
		assertIssue35RunningSingBox(t, k)
		assertIssue35OutboundLive(t, k, issue35ProxyTag, "socks")
	})
	t.Run("shadowsocks", func(t *testing.T) {
		k := startIssue35Kernel(t, issue35PanelJSON(issue35XrayStyleShadowsocks(), nil))
		defer k.Stop()
		assertIssue35RunningSingBox(t, k)
		assertIssue35OutboundLive(t, k, issue35ProxyTag, "shadowsocks")
	})
}

func TestCustomOutbound_route指到servers形狀outbound必須起核(t *testing.T) {
	k := startIssue35Kernel(t, issue35PanelJSON(issue35XrayStyleSOCKS(), issue35OfficialRoute()))
	defer k.Stop()
	assertIssue35RunningSingBox(t, k)
	assertIssue35OutboundLive(t, k, issue35ProxyTag, "socks")
	assertIssue35RoutePointsTo(t, k, issue35ProxyTag)
}

func TestCustomOutbound_關掉自訂或切xray是假修(t *testing.T) {
	configSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("讀 config.go: %v", err)
	}
	startSrc, err := os.ReadFile("singbox.go")
	if err != nil {
		t.Fatalf("讀 singbox.go: %v", err)
	}

	if !bytes.Contains(configSrc, []byte("for _, co := range nc.CustomOutbounds")) {
		t.Fatal("關掉自訂 outbound 當修：buildConfig 不再吃面板 CustomOutbounds")
	}
	if !bytes.Contains(configSrc, []byte("outboundConfigToSingbox")) {
		t.Fatal("關掉自訂 outbound 當修：不再轉換面板 outbound")
	}
	if !bytes.Contains(configSrc, []byte("nc.CustomRouteRules")) || !bytes.Contains(configSrc, []byte("nc.CustomRoutes")) {
		t.Fatal("關掉自訂 route 當修：buildRoutes 不再吃面板 custom route")
	}
	if !bytes.Contains(startSrc, []byte("parse sing-box options")) {
		t.Fatal("起核不再走 sing-box options parse；改切別的核或略過 parse 都是假修")
	}
	if bytes.Contains(startSrc, []byte("kernel/xray")) || bytes.Contains(startSrc, []byte("xray.New")) {
		t.Fatal("不准改切 xray 當修")
	}

	// 真起核：自訂 outbound／route 必須還在 running sing-box 裡。
	// 用會觸發官方 unknown field "servers" 的形狀，關掉自訂當修會被抓到。
	k := startIssue35Kernel(t, issue35PanelJSON(issue35XrayStyleSOCKS(), issue35OfficialDomainRoute()))
	defer k.Stop()
	assertIssue35RunningSingBox(t, k)
	assertIssue35OutboundLive(t, k, issue35ProxyTag, "socks")
	assertIssue35RoutePointsTo(t, k, issue35ProxyTag)
}

func issue35OfficialSOCKS() map[string]any {
	return map[string]any{
		"tag":      issue35ProxyTag,
		"protocol": "socks",
		"settings": map[string]any{
			"server":      "127.0.0.1",
			"server_port": 1080,
			"version":     "5",
			"username":    "",
			"password":    "",
		},
	}
}

func issue35OfficialShadowsocks() map[string]any {
	// 官方評論 method／password 是空字串佔位；空 method 會變成 unknown method，
	// 不是 #35。這裡填真實值，欄位名仍對準官方形狀。
	return map[string]any{
		"tag":      issue35ProxyTag,
		"protocol": "shadowsocks",
		"settings": map[string]any{
			"server":      "127.0.0.1",
			"server_port": 1080,
			"method":      "aes-128-gcm",
			"password":    "issue35-ss-pass",
			"plugin":      "",
			"plugin_opts": "",
			"network":     "udp",
		},
	}
}

func issue35OfficialRoute() []map[string]any {
	return []map[string]any{{
		"action":   "route",
		"outbound": issue35ProxyTag,
	}}
}

func issue35OfficialDomainRoute() []map[string]any {
	return []map[string]any{{
		"domain":        []string{"www.netflix.com"},
		"domain_suffix": []string{".netflix.com"},
		"action":        "route",
		"outbound":      issue35ProxyTag,
	}}
}

func issue35XrayStyleSOCKS() map[string]any {
	// docs-custom-outbounds.md 與 xray socks settings 的 servers 陣列。
	return map[string]any{
		"tag":      issue35ProxyTag,
		"protocol": "socks",
		"settings": map[string]any{
			"servers": []any{
				map[string]any{"address": "1.2.3.4", "port": 1080},
			},
		},
	}
}

func issue35XrayStyleShadowsocks() map[string]any {
	return map[string]any{
		"tag":      issue35ProxyTag,
		"protocol": "shadowsocks",
		"settings": map[string]any{
			"servers": []any{
				map[string]any{
					"address":  "1.2.3.4",
					"port":     1080,
					"method":   "aes-128-gcm",
					"password": "issue35-ss-pass",
				},
			},
		},
	}
}

func issue35PanelJSON(outbound map[string]any, routes []map[string]any) map[string]any {
	payload := map[string]any{
		"protocol":         "shadowsocks",
		"cipher":           "aes-128-gcm",
		"kernel_type":      "singbox",
		"custom_outbounds": []any{outbound},
	}
	if routes != nil {
		raw := make([]any, 0, len(routes))
		for _, route := range routes {
			raw = append(raw, route)
		}
		payload["custom_routes"] = raw
	}
	return payload
}

func startIssue35Kernel(t *testing.T, panelPayload map[string]any) *SingBox {
	t.Helper()
	nc := fetchIssue35PanelConfig(t, panelPayload)
	spec := model.NodeSpecFromPanel(nc)
	if spec == nil {
		t.Fatal("NodeSpecFromPanel 回傳 nil：自訂 outbound／route 被丟掉了")
	}
	if len(spec.CustomOutbounds) == 0 {
		t.Fatal("關掉自訂 outbound 當修：面板有 custom_outbounds，NodeSpec 卻是空的")
	}
	if panelPayload["custom_routes"] != nil && len(spec.CustomRoutes) == 0 && len(spec.CustomRouteRules) == 0 {
		t.Fatal("關掉自訂 route 當修：面板有 custom_routes，NodeSpec 卻是空的")
	}

	k := New(config.KernelConfig{Type: "singbox", LogLevel: "warn"})
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}
	err := k.Start(spec, testUsers, kernel.TLSCert{})
	if err != nil {
		if strings.Contains(err.Error(), issue35OfficialError) {
			t.Fatalf("面板加自訂 SOCKS／SS outbound 後 sing-box 不得再出現官方 #35 錯誤 %q：%v。不准關自訂 outbound／route、不准改切 xray", issue35OfficialError, err)
		}
		t.Fatalf("面板自訂 outbound／route 必須能起 sing-box 核：%v。不准關自訂、不准改切 xray", err)
	}
	if !k.IsRunning() {
		t.Fatal("Start 沒回錯但 kernel 沒在跑：略過起核或改切別的核都是假修")
	}
	return k
}

func fetchIssue35PanelConfig(t *testing.T, payload map[string]any) *panel.NodeConfig {
	t.Helper()
	port := issue35FreeTCPPort(t)
	payload["server_port"] = port

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)

	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "issue35",
		NodeID: 50,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("面板下發自訂 outbound／route 必須能被 node 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	if len(cfg.CustomOutbounds) == 0 {
		t.Fatal("關掉自訂 outbound 當修：GetConfig 沒帶 custom_outbounds")
	}
	return cfg
}

func assertIssue35RunningSingBox(t *testing.T, k *SingBox) {
	t.Helper()
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q", k.Name())
	}
	if !k.IsRunning() {
		t.Fatal("sing-box 核沒在跑")
	}
}

func assertIssue35OutboundLive(t *testing.T, k *SingBox, tag, wantType string) {
	t.Helper()
	om := service.FromContext[adapter.OutboundManager](k.ctx)
	if om == nil {
		t.Fatal("起核後沒有 OutboundManager：沒走到真實 sing-box")
	}
	ob, ok := om.Outbound(tag)
	if !ok || ob == nil {
		t.Fatalf("關掉自訂 outbound 當修：running sing-box 找不到 tag %q", tag)
	}
	if got := ob.Type(); got != wantType {
		t.Fatalf("outbound %q type=%q，要 %q；改切別的核或丟掉設定都是假修", tag, got, wantType)
	}
	if ob.Tag() != tag {
		t.Fatalf("outbound tag=%q，要 %q", ob.Tag(), tag)
	}
}

func assertIssue35RoutePointsTo(t *testing.T, k *SingBox, tag string) {
	t.Helper()
	router := service.FromContext[adapter.Router](k.ctx)
	if router == nil {
		t.Fatal("起核後沒有 Router：沒走到真實 sing-box")
	}
	for _, rule := range router.Rules() {
		if rule == nil || rule.Action() == nil {
			continue
		}
		if strings.Contains(rule.Action().String(), tag) || strings.Contains(rule.String(), tag) {
			return
		}
	}
	t.Fatalf("關掉自訂 route 當修：running sing-box 沒有指到 outbound %q 的 route", tag)
}

func issue35FreeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
