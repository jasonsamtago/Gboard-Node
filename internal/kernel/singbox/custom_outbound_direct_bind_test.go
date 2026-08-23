package singbox

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
)

// 對準官方 cedar2025/Xboard-Node #59（#50 同）：
// sing-box 自訂 outbound 必須收 protocol=direct，
// settings.bind_interface 要進 outbound。
//
// 官方期望形狀：
//
//	[{"tag":"direct-home0","protocol":"direct","settings":{"bind_interface":"home0"}}]
//
// 審核 2 鎖定：
//   - sing-box 自訂 outbound 必須收 protocol=direct
//   - settings.bind_interface 要進 outbound（例如 home0）
//   - 配 route 指到該 outbound 也能起核
//   - 不准只准寫本地 config.yml 的 kernel.custom_outbound 當修
//     （面板 custom_outbounds 路徑必須通）
//
// 現況：OutboundSupportMatrix 的 singbox 白名單沒有 direct，
// ValidateCustomOutboundsForKernel 會拒面板這條路徑。
// 內建已有 tag=direct outbound；自訂是另一個 tag（direct-home0）綁介面。
//
// 失敗測試走面板 JSON → GetConfig → NodeSpecFromPanelValidated
// （控制面同一條 validate），再產出／起核。不要只吃本地 kernel.custom_outbound。
// 這份只鎖行為，不實作修正。

const (
	issue59OfficialTag       = "direct-home0"
	issue59OfficialInterface = "home0"
	issue59OfficialError     = `custom_outbounds[0].protocol "direct" is not supported by kernel "singbox"`
)

func TestCustomOutbound_SupportMatrix_singbox必須允許direct(t *testing.T) {
	support, ok := model.OutboundSupportMatrix()["singbox"]
	if !ok {
		t.Fatal("OutboundSupportMatrix 沒有 singbox：面板自訂 outbound 無從校驗")
	}
	for _, protocol := range support.Protocols {
		if strings.EqualFold(protocol, "direct") {
			return
		}
	}
	t.Fatalf("sing-box 自訂 outbound 必須允許 protocol=direct；現況白名單=%v，面板會拒官方形狀", support.Protocols)
}

func TestCustomOutbound_Validate_官方direct形狀必須過(t *testing.T) {
	err := model.ValidateCustomOutboundsForKernel(issue59OfficialModelOutbounds(), "singbox", nil)
	if err != nil {
		t.Fatalf("面板官方形狀 protocol=direct＋settings.bind_interface=%s 必須過 validate：%v", issue59OfficialInterface, err)
	}
}

func TestCustomOutbound_面板custom_outbounds必須產出bind_interface(t *testing.T) {
	spec := issue59ValidatedPanelSpec(t, issue59OfficialPanelOutbound(), nil)
	if spec.KernelType != "" && !strings.EqualFold(spec.KernelType, "singbox") && !strings.EqualFold(spec.KernelType, "sing-box") {
		t.Fatalf("不准改切 xray 當修：KernelType=%q", spec.KernelType)
	}

	cfg := buildConfig(issue59PanelKernelConfig(), spec, testUsers, kernel.TLSCert{})
	ob := issue59FindOutbound(cfg, issue59OfficialTag)
	if ob == nil {
		t.Fatalf("面板 custom_outbounds 必須產出 tag=%q 的 outbound；關掉面板路徑或只吃本地 config.yml 都是假修。outbounds=%s", issue59OfficialTag, issue59OutboundsJSON(cfg))
	}
	if got, _ := ob["type"].(string); got != "direct" {
		t.Fatalf("面板 custom_outbounds 產出 outbound type=%q，要 type=direct；settings 攤平後 protocol 必須進 type。outbound=%s", got, issue59MapJSON(ob))
	}
	if got, _ := ob["bind_interface"].(string); got != issue59OfficialInterface {
		t.Fatalf("settings.bind_interface 要進 outbound，要 %q，得到 %q。不准把 bind_interface 丟在 settings 裡或只寫進本地 kernel.custom_outbound。outbound=%s", issue59OfficialInterface, got, issue59MapJSON(ob))
	}
	builtin := issue59FindOutbound(cfg, "direct")
	if builtin == nil {
		t.Fatal("內建 tag=direct outbound 必須還在；自訂是另一個 tag direct-home0，不准覆寫內建 direct")
	}
}

func TestCustomOutbound_route指到direct_home0必須起核(t *testing.T) {
	spec := issue59ValidatedPanelSpec(t, issue59OfficialPanelOutbound(), issue59OfficialRoute())
	k := issue59StartKernel(t, spec)
	defer k.Stop()
	assertIssue35RunningSingBox(t, k)
	assertIssue35OutboundLive(t, k, issue59OfficialTag, "direct")
	assertIssue35RoutePointsTo(t, k, issue59OfficialTag)

	cfg := buildConfig(issue59PanelKernelConfig(), spec, testUsers, kernel.TLSCert{})
	ob := issue59FindOutbound(cfg, issue59OfficialTag)
	if ob == nil {
		t.Fatal("起核後產出的 config 沒有 direct-home0：略過自訂 outbound 是假修")
	}
	if got, _ := ob["bind_interface"].(string); got != issue59OfficialInterface {
		t.Fatalf("起核路徑 settings.bind_interface 要進 outbound，要 %q，得到 %q", issue59OfficialInterface, got)
	}
}

func TestCustomOutbound_只准本地config_yml是假修(t *testing.T) {
	configSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("讀 config.go: %v", err)
	}
	validateSrc, err := os.ReadFile(filepath.Join("..", "..", "model", "validate.go"))
	if err != nil {
		t.Fatalf("讀 validate.go: %v", err)
	}
	supportSrc, err := os.ReadFile(filepath.Join("..", "..", "model", "custom_outbound_support.go"))
	if err != nil {
		t.Fatalf("讀 custom_outbound_support.go: %v", err)
	}
	cpSrc, err := os.ReadFile(filepath.Join("..", "..", "controlplane", "panel.go"))
	if err != nil {
		t.Fatalf("讀 controlplane/panel.go: %v", err)
	}

	if !bytes.Contains(configSrc, []byte("for _, co := range nc.CustomOutbounds")) {
		t.Fatal("關掉面板 custom_outbounds 當修：buildConfig 不再吃 nc.CustomOutbounds")
	}
	if !bytes.Contains(configSrc, []byte("outboundConfigToSingbox")) {
		t.Fatal("關掉面板 custom_outbounds 當修：不再轉換面板 outbound")
	}
	if !bytes.Contains(validateSrc, []byte("ValidateCustomOutboundsForKernel")) {
		t.Fatal("面板路徑不再 validate custom outbounds：略過白名單是假修")
	}
	if !bytes.Contains(cpSrc, []byte("NodeSpecFromPanelValidated")) {
		t.Fatal("控制面不再走 NodeSpecFromPanelValidated：面板 custom_outbounds 被略過是假修")
	}
	if !bytes.Contains(supportSrc, []byte(`"singbox"`)) {
		t.Fatal("OutboundSupportMatrix 不再宣告 singbox")
	}

	// 官方 workaround 本來就能走本地 kernel.custom_outbound。
	// 那條綠不代表修好；面板 custom_outbounds 必須自己通。
	localOnly := buildConfig(config.KernelConfig{
		Type:     "singbox",
		LogLevel: "warn",
		CustomOutbound: []map[string]any{{
			"tag":            issue59OfficialTag,
			"type":           "direct",
			"bind_interface": issue59OfficialInterface,
		}},
	}, issue59BareSpec(t), testUsers, kernel.TLSCert{})
	if ob := issue59FindOutbound(localOnly, issue59OfficialTag); ob == nil {
		t.Fatal("本地 kernel.custom_outbound 探針壞了，測不到「只准本地 config.yml」這條假修")
	}

	spec, err := model.NodeSpecFromPanelValidated(issue59PanelNode(t, issue59OfficialPanelOutbound(), issue59OfficialRoute()), issue59PanelKernelConfig())
	if err != nil {
		t.Fatalf("不准只准寫本地 config.yml 的 kernel.custom_outbound 當修（面板 custom_outbounds 路徑必須通）：%v", err)
	}
	if len(spec.CustomOutbounds) == 0 {
		t.Fatal("關掉面板 custom_outbounds 當修：NodeSpec 沒帶官方形狀")
	}
	if spec.CustomOutbounds[0].Protocol != "direct" {
		t.Fatalf("面板路徑 protocol=%q，要 direct", spec.CustomOutbounds[0].Protocol)
	}

	cfg := buildConfig(issue59PanelKernelConfig(), spec, testUsers, kernel.TLSCert{})
	ob := issue59FindOutbound(cfg, issue59OfficialTag)
	if ob == nil {
		t.Fatalf("只准本地 config.yml 當修：面板 custom_outbounds 沒進 outbound。outbounds=%s", issue59OutboundsJSON(cfg))
	}
	if got, _ := ob["type"].(string); got != "direct" {
		t.Fatalf("面板路徑 outbound type=%q，要 direct", got)
	}
	if got, _ := ob["bind_interface"].(string); got != issue59OfficialInterface {
		t.Fatalf("面板路徑 bind_interface=%q，要 %q；不准只把介面寫在本地 kernel.custom_outbound", got, issue59OfficialInterface)
	}

	k := issue59StartKernel(t, spec)
	defer k.Stop()
	assertIssue35RunningSingBox(t, k)
	assertIssue35OutboundLive(t, k, issue59OfficialTag, "direct")
	assertIssue35RoutePointsTo(t, k, issue59OfficialTag)
}

func issue59OfficialModelOutbounds() []model.OutboundConfig {
	return []model.OutboundConfig{{
		Tag:      issue59OfficialTag,
		Protocol: "direct",
		Settings: map[string]any{"bind_interface": issue59OfficialInterface},
	}}
}

func issue59OfficialPanelOutbound() map[string]any {
	return map[string]any{
		"tag":      issue59OfficialTag,
		"protocol": "direct",
		"settings": map[string]any{
			"bind_interface": issue59OfficialInterface,
		},
	}
}

func issue59OfficialRoute() []map[string]any {
	return []map[string]any{{
		"action":   "route",
		"outbound": issue59OfficialTag,
	}}
}

func issue59PanelKernelConfig() config.KernelConfig {
	return config.KernelConfig{Type: "singbox", LogLevel: "warn"}
}

func issue59BareSpec(t *testing.T) *model.NodeSpec {
	t.Helper()
	return model.NodeSpecFromPanel(&panel.NodeConfig{
		Protocol:   "shadowsocks",
		ServerPort: issue35FreeTCPPort(t),
		Cipher:     "aes-128-gcm",
		KernelType: "singbox",
	})
}

func issue59PanelNode(t *testing.T, outbound map[string]any, routes []map[string]any) *panel.NodeConfig {
	t.Helper()
	payload := map[string]any{
		"protocol":         "shadowsocks",
		"cipher":           "aes-128-gcm",
		"kernel_type":      "singbox",
		"server_port":      issue35FreeTCPPort(t),
		"custom_outbounds": []any{outbound},
	}
	if routes != nil {
		raw := make([]any, 0, len(routes))
		for _, route := range routes {
			raw = append(raw, route)
		}
		payload["custom_routes"] = raw
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)

	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "issue59",
		NodeID: 59,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("面板下發官方 direct＋bind_interface 必須能被 node 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	if len(cfg.CustomOutbounds) == 0 {
		t.Fatal("關掉面板 custom_outbounds 當修：GetConfig 沒帶 custom_outbounds")
	}
	if cfg.CustomOutbounds[0].Protocol != "direct" {
		t.Fatalf("GetConfig protocol=%q，要 direct", cfg.CustomOutbounds[0].Protocol)
	}
	if cfg.CustomOutbounds[0].Settings["bind_interface"] != issue59OfficialInterface {
		t.Fatalf("GetConfig settings.bind_interface=%v，要 %q", cfg.CustomOutbounds[0].Settings["bind_interface"], issue59OfficialInterface)
	}
	if routes != nil && len(cfg.CustomRoutes) == 0 {
		t.Fatal("關掉自訂 route 當修：GetConfig 沒帶 custom_routes")
	}
	return cfg
}

func issue59ValidatedPanelSpec(t *testing.T, outbound map[string]any, routes []map[string]any) *model.NodeSpec {
	t.Helper()
	nc := issue59PanelNode(t, outbound, routes)
	spec, err := model.NodeSpecFromPanelValidated(nc, issue59PanelKernelConfig())
	if err != nil {
		if strings.Contains(err.Error(), issue59OfficialError) || strings.Contains(err.Error(), `protocol "direct" is not supported`) {
			t.Fatalf("面板 custom_outbounds protocol=direct 不得因不支援而拒：%v。不准只准寫本地 config.yml", err)
		}
		t.Fatalf("面板官方 direct＋bind_interface 必須過 NodeSpecFromPanelValidated：%v。不准只准寫本地 config.yml", err)
	}
	if spec == nil {
		t.Fatal("NodeSpecFromPanelValidated 回傳 nil：面板 custom_outbounds 被丟掉了")
	}
	if len(spec.CustomOutbounds) == 0 {
		t.Fatal("關掉面板 custom_outbounds 當修：validate 過了但 NodeSpec 是空的")
	}
	if routes != nil && len(spec.CustomRoutes) == 0 && len(spec.CustomRouteRules) == 0 {
		t.Fatal("關掉自訂 route 當修：面板有 custom_routes，NodeSpec 卻是空的")
	}
	return spec
}

func issue59StartKernel(t *testing.T, spec *model.NodeSpec) *SingBox {
	t.Helper()
	k := New(issue59PanelKernelConfig())
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}
	err := k.Start(spec, testUsers, kernel.TLSCert{})
	if err != nil {
		if strings.Contains(err.Error(), issue59OfficialError) || strings.Contains(err.Error(), `protocol "direct" is not supported`) {
			t.Fatalf("配 route 指到 %s 不得因 protocol 不支援而炸：%v", issue59OfficialTag, err)
		}
		t.Fatalf("面板自訂 direct outbound／route 必須能起 sing-box 核：%v。不准只准寫本地 config.yml、不准改切 xray", err)
	}
	if !k.IsRunning() {
		t.Fatal("Start 沒回錯但 kernel 沒在跑：略過起核或改切別的核都是假修")
	}
	return k
}

func issue59FindOutbound(cfg M, tag string) M {
	outbounds, _ := cfg["outbounds"].([]M)
	for _, ob := range outbounds {
		if got, _ := ob["tag"].(string); got == tag {
			return ob
		}
	}
	return nil
}

func issue59OutboundsJSON(cfg M) string {
	return issue59MapJSON(cfg["outbounds"])
}

func issue59MapJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		return "<marshal error>"
	}
	return string(raw)
}
