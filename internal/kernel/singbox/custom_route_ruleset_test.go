package singbox

import (
	"bytes"
	"encoding/json"
	"net"
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
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/srs"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/service"
)

// 對準官方 cedar2025/Xboard-Node #14：sing-box 自訂 Routes／kernel custom_route
// 含 rule_set（含 remote binary，例如 geosite-cn.srs／geoip）時，產生的
// sing-box config 必須帶入 route.rule_set，起核不得因「不支援／丟掉
// rule_set／只認廢棄 geosite: 欄位」而失敗。
//
// 官方「新的」形狀（Geosite 已在 1.8 廢棄、1.12 移除）：
//
//	{
//	  "route": {
//	    "rules": [{"rule_set": "geosite-cn", "outbound": "direct"}],
//	    "rule_set": [{
//	      "tag": "geosite-cn",
//	      "type": "remote",
//	      "format": "binary",
//	      "url": "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs",
//	      "download_detour": "proxy"
//	    }]
//	  }
//	}
//
// 與已合的官方 #20（geo 自動下載）分開：本票鎖 **rule_set 形狀本身**
// 能進 config／起核，不是「缺 geoip.dat／geosite.db 自動下載」。
// 測資刻意不用 geoip:／geosite: 前綴，避免靠 #20 當修。
//
// 審核 2 鎖定：
//   - 面板／NodeInfo 或 kernel custom_route 含 rule_set（remote／local、
//     format binary、url 或 path 指向 .srs）時，BuildConfig／CreateInstance
//     必須保留 route.rule_set（或同等 sing-box 1.x 形狀），rules 能引用該 tag
//   - 不得靜默丟掉、改寫成只剩廢棄 geosite:、panic／unknown field
//   - 必須能起核並套用規則
//   - 不准只靠廢棄 geosite: 當唯一路徑
//   - 不准關自訂 route／改切 xray 當修
//   - 不准只靠本地 custom_config 檔（官方 issue 評論的 workaround）當修
//
// 失敗測試走面板 JSON → GetConfig → NodeSpec → buildConfig／Start。
// 這份只鎖行為，不實作修正。

const (
	issue14GeoSiteTag = "geosite-cn"
	issue14GeoIPTag   = "geoip-cn"
	issue14Domain     = "issue14.example.cn"
)

func TestCustomRouteRuleSet_面板官方wrapper必須進config(t *testing.T) {
	srsPath := issue14WriteLocalSRS(t, "geosite-cn.srs")
	spec := issue14PanelSpec(t, []map[string]any{
		issue14OfficialRouteObject(issue14GeoSiteTag, issue14LocalBinarySet(issue14GeoSiteTag, srsPath)),
	})
	assertIssue14ConfigKeepsRuleSet(t, spec, config.KernelConfig{}, issue14GeoSiteTag, "local", "binary", srsPath, "")
}

func TestCustomRouteRuleSet_面板remote_binary必須進config(t *testing.T) {
	url := "https://raw.githubusercontent.com/SagerNet/sing-geosite/rule-set/geosite-cn.srs"
	spec := issue14PanelSpec(t, []map[string]any{
		issue14OfficialRouteObject(issue14GeoSiteTag, issue14RemoteBinarySet(issue14GeoSiteTag, url)),
	})
	assertIssue14ConfigKeepsRuleSet(t, spec, config.KernelConfig{}, issue14GeoSiteTag, "remote", "binary", "", url)
}

func TestCustomRouteRuleSet_kernel_custom_route必須進config(t *testing.T) {
	srsPath := issue14WriteLocalSRS(t, "geoip-cn.srs")
	spec := issue14BareSpec(t)
	cfg := config.KernelConfig{
		CustomRoute: []map[string]any{
			issue14OfficialRouteObject(issue14GeoIPTag, issue14LocalBinarySet(issue14GeoIPTag, srsPath)),
		},
	}
	assertIssue14ConfigKeepsRuleSet(t, spec, cfg, issue14GeoIPTag, "local", "binary", srsPath, "")
}

func TestCustomRouteRuleSet_面板rule與定義分開列出也必須進config(t *testing.T) {
	srsPath := issue14WriteLocalSRS(t, "geosite-cn.srs")
	spec := issue14PanelSpec(t, []map[string]any{
		{"rule_set": []string{issue14GeoSiteTag}, "outbound": "direct"},
		issue14LocalBinarySet(issue14GeoSiteTag, srsPath),
	})
	assertIssue14ConfigKeepsRuleSet(t, spec, config.KernelConfig{}, issue14GeoSiteTag, "local", "binary", srsPath, "")
}

func TestCustomRouteRuleSet_面板local_binary必須起核並套用(t *testing.T) {
	srsPath := issue14WriteLocalSRS(t, "geosite-cn.srs")
	spec := issue14PanelSpec(t, []map[string]any{
		issue14OfficialRouteObject(issue14GeoSiteTag, issue14LocalBinarySet(issue14GeoSiteTag, srsPath)),
	})
	k := startIssue14Kernel(t, spec, config.KernelConfig{})
	defer k.Stop()
	assertIssue14RuleSetApplied(t, k, issue14GeoSiteTag)
}

func TestCustomRouteRuleSet_面板remote_binary必須起核並套用(t *testing.T) {
	srs := issue14MiniSRS(t)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".srs") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(srs)
	}))
	t.Cleanup(ts.Close)
	url := ts.URL + "/geosite-cn.srs"

	spec := issue14PanelSpec(t, []map[string]any{
		issue14OfficialRouteObject(issue14GeoSiteTag, issue14RemoteBinarySet(issue14GeoSiteTag, url)),
	})
	k := startIssue14Kernel(t, spec, config.KernelConfig{ConfigDir: t.TempDir()})
	defer k.Stop()
	assertIssue14RuleSetApplied(t, k, issue14GeoSiteTag)
}

func TestCustomRouteRuleSet_kernel_custom_route必須起核(t *testing.T) {
	srsPath := issue14WriteLocalSRS(t, "geoip-cn.srs")
	spec := issue14BareSpec(t)
	cfg := config.KernelConfig{
		CustomRoute: []map[string]any{
			issue14OfficialRouteObject(issue14GeoIPTag, issue14LocalBinarySet(issue14GeoIPTag, srsPath)),
		},
	}
	k := startIssue14Kernel(t, spec, cfg)
	defer k.Stop()
	assertIssue14RuleSetApplied(t, k, issue14GeoIPTag)
}

func TestCustomRouteRuleSet_不准改寫成廢棄geosite(t *testing.T) {
	srsPath := issue14WriteLocalSRS(t, "geosite-cn.srs")
	spec := issue14PanelSpec(t, []map[string]any{
		issue14OfficialRouteObject(issue14GeoSiteTag, issue14LocalBinarySet(issue14GeoSiteTag, srsPath)),
	})
	built := issue14BuildConfig(t, spec, config.KernelConfig{})
	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if bytes.Contains(raw, []byte(`"geosite"`)) && !bytes.Contains(raw, []byte(`"rule_set"`)) {
		t.Fatalf("不准只靠已廢棄的 geosite: 欄位當唯一路徑。產出=%s", raw)
	}
	if issue14RouteHasDeprecatedGeoSiteField(built) {
		t.Fatalf("規則被改寫成廢棄 geosite 欄位。產出=%s", raw)
	}
	route, _ := built["route"].(M)
	if issue14FindRuleSet(route["rule_set"], issue14GeoSiteTag) == nil {
		t.Fatalf("不准把 rule_set 定義丟掉或改寫成廢棄 geosite:：route.rule_set 必須保留 tag %q。產出=%s", issue14GeoSiteTag, raw)
	}
	if !issue14RouteHasRuleSetField(built, issue14GeoSiteTag) {
		t.Fatalf("rules 必須用 rule_set 引用 %q，不得改寫成 geosite:。產出=%s", issue14GeoSiteTag, raw)
	}
}

func TestCustomRouteRuleSet_假修要被抓到(t *testing.T) {
	configSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("讀 config.go: %v", err)
	}
	startSrc, err := os.ReadFile("singbox.go")
	if err != nil {
		t.Fatalf("讀 singbox.go: %v", err)
	}

	if !bytes.Contains(configSrc, []byte("nc.CustomRouteRules")) || !bytes.Contains(configSrc, []byte("nc.CustomRoutes")) {
		t.Fatal("關掉自訂 route 當修：buildRoutes 不再吃面板 custom route")
	}
	if !bytes.Contains(configSrc, []byte("kcfg.CustomRoute")) {
		t.Fatal("關掉 kernel custom_route 當修：buildRoutes 不再吃 kernel custom_route")
	}
	if !bytes.Contains(startSrc, []byte("parse sing-box options")) {
		t.Fatal("起核不再走 sing-box options parse；改切別的核或略過 parse 都是假修")
	}
	if bytes.Contains(startSrc, []byte("kernel/xray")) || bytes.Contains(startSrc, []byte("xray.New")) {
		t.Fatal("不准改切 xray 當修")
	}

	// 只靠 custom_config 檔（官方 issue 評論 workaround）當修：面板／kernel
	// custom_route 路徑仍必須自己帶出 route.rule_set。
	srsPath := issue14WriteLocalSRS(t, "geosite-cn.srs")
	spec := issue14PanelSpec(t, []map[string]any{
		issue14OfficialRouteObject(issue14GeoSiteTag, issue14LocalBinarySet(issue14GeoSiteTag, srsPath)),
	})
	k := startIssue14Kernel(t, spec, config.KernelConfig{})
	defer k.Stop()
	assertIssue14RuleSetApplied(t, k, issue14GeoSiteTag)
}

func issue14OfficialRouteObject(tag string, ruleSet map[string]any) map[string]any {
	return map[string]any{
		"rules": []any{
			map[string]any{
				"rule_set": tag,
				"outbound": "direct",
			},
		},
		"rule_set": []any{ruleSet},
	}
}

func issue14LocalBinarySet(tag, path string) map[string]any {
	return map[string]any{
		"tag":    tag,
		"type":   "local",
		"format": "binary",
		"path":   path,
	}
}

func issue14RemoteBinarySet(tag, url string) map[string]any {
	return map[string]any{
		"tag":             tag,
		"type":            "remote",
		"format":          "binary",
		"url":             url,
		"download_detour": "direct",
	}
}

func issue14BareSpec(t *testing.T) *model.NodeSpec {
	t.Helper()
	return &model.NodeSpec{
		Protocol:   "shadowsocks",
		ListenIP:   "127.0.0.1",
		ServerPort: issue14FreeTCPPort(t),
		Cipher:     "aes-128-gcm",
		KernelType: "singbox",
		Routes:     nil,
	}
}

func issue14PanelSpec(t *testing.T, routes []map[string]any) *model.NodeSpec {
	t.Helper()
	payload := map[string]any{
		"protocol":    "shadowsocks",
		"cipher":      "aes-128-gcm",
		"kernel_type": "singbox",
		"server_port": issue14FreeTCPPort(t),
	}
	raw := make([]any, 0, len(routes))
	for _, route := range routes {
		raw = append(raw, route)
	}
	payload["custom_routes"] = raw

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)

	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "issue14",
		NodeID: 14,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("面板下發含 rule_set 的 custom route 必須能被 node 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	if len(cfg.CustomRoutes) == 0 {
		t.Fatal("關掉自訂 route 當修：GetConfig 沒帶 custom_routes")
	}

	spec := model.NodeSpecFromPanel(cfg)
	if spec == nil {
		t.Fatal("NodeSpecFromPanel 回傳 nil：自訂 route 被丟掉了")
	}
	if len(spec.CustomRoutes) == 0 && len(spec.CustomRouteRules) == 0 {
		t.Fatal("關掉自訂 route 當修：面板有 custom_routes，NodeSpec 卻是空的")
	}
	if kernel.NeedsGeoIP(spec.Routes) || kernel.NeedsGeoSite(spec.Routes) {
		t.Fatal("本票不得靠 #20 geo 前綴：面板 Routes 不該含 geoip:／geosite:")
	}
	return spec
}

func issue14BuildConfig(t *testing.T, spec *model.NodeSpec, extra config.KernelConfig) M {
	t.Helper()
	cfg := extra
	cfg.Type = "singbox"
	if cfg.LogLevel == "" {
		cfg.LogLevel = "warn"
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = t.TempDir()
	}
	return buildConfig(cfg, spec, testUsers, kernel.TLSCert{})
}

func assertIssue14ConfigKeepsRuleSet(t *testing.T, spec *model.NodeSpec, extra config.KernelConfig, tag, typ, format, path, url string) {
	t.Helper()
	built := issue14BuildConfig(t, spec, extra)
	raw, err := json.MarshalIndent(built, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	route, _ := built["route"].(M)
	if route == nil {
		t.Fatalf("產出沒有 route：rule_set 被丟掉了。config=%s", raw)
	}
	if issue14RouteHasDeprecatedGeoSiteField(built) {
		t.Fatalf("不准改寫成只剩廢棄 geosite: 欄位。config=%s", raw)
	}

	set := issue14FindRuleSet(route["rule_set"], tag)
	if set == nil {
		t.Fatalf("自訂 route 含 rule_set 時，產出必須保留 route.rule_set（tag=%q type=%q format=%q）。現行若把定義塞進 rules 或靜默丟掉，就是官方 #14。config=%s", tag, typ, format, raw)
	}
	if got, _ := set["type"].(string); got != typ {
		t.Fatalf("route.rule_set[%q].type=%q，要 %q。不准改成只認廢棄 geosite:。set=%s", tag, got, typ, issue14MapJSON(set))
	}
	if got, _ := set["format"].(string); got != format {
		t.Fatalf("route.rule_set[%q].format=%q，要 %q（binary .srs）。set=%s", tag, got, format, issue14MapJSON(set))
	}
	if path != "" {
		if got, _ := set["path"].(string); got != path {
			t.Fatalf("route.rule_set[%q].path=%q，要 %q。set=%s", tag, got, path, issue14MapJSON(set))
		}
	}
	if url != "" {
		if got, _ := set["url"].(string); got != url {
			t.Fatalf("route.rule_set[%q].url=%q，要 %q。set=%s", tag, got, url, issue14MapJSON(set))
		}
	}
	if !issue14RouteHasRuleSetField(built, tag) {
		t.Fatalf("rules 必須能引用 rule_set tag %q。config=%s", tag, raw)
	}
}

func startIssue14Kernel(t *testing.T, spec *model.NodeSpec, extra config.KernelConfig) *SingBox {
	t.Helper()
	cfg := extra
	cfg.Type = "singbox"
	if cfg.LogLevel == "" {
		cfg.LogLevel = "warn"
	}
	if cfg.ConfigDir == "" {
		cfg.ConfigDir = t.TempDir()
	}

	k := New(cfg)
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}
	err := k.Start(spec, testUsers, kernel.TLSCert{})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "unknown field") ||
			strings.Contains(msg, "rule-set") ||
			strings.Contains(msg, "rule_set") ||
			strings.Contains(msg, "geosite") ||
			strings.Contains(msg, "not found") ||
			strings.Contains(msg, "unsupported") {
			t.Fatalf("自訂 route 含 rule_set 時 sing-box 不得因不支援／丟掉 rule_set／只認廢棄 geosite: 而失敗：%v。不准關自訂 route、不准改切 xray", err)
		}
		t.Fatalf("自訂 route 含 rule_set 必須能起 sing-box 核：%v。不准關自訂 route、不准改切 xray", err)
	}
	if !k.IsRunning() {
		t.Fatal("Start 沒回錯但 kernel 沒在跑：略過起核或改切別的核都是假修")
	}
	return k
}

func assertIssue14RuleSetApplied(t *testing.T, k *SingBox, tag string) {
	t.Helper()
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q", k.Name())
	}
	if !k.IsRunning() {
		t.Fatal("sing-box 核沒在跑")
	}
	router := service.FromContext[adapter.Router](k.ctx)
	if router == nil {
		t.Fatal("起核後沒有 Router：沒走到真實 sing-box")
	}
	rs, ok := router.RuleSet(tag)
	if !ok || rs == nil {
		t.Fatalf("關掉／丟掉 rule_set 當修：running sing-box 找不到 rule_set tag %q", tag)
	}
	found := false
	for _, rule := range router.Rules() {
		if rule == nil {
			continue
		}
		text := rule.String()
		if rule.Action() != nil {
			text += " " + rule.Action().String()
		}
		if strings.Contains(text, tag) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("rules 必須引用 rule_set tag %q；改寫成只剩廢棄 geosite: 或丟掉引用都是假修", tag)
	}
}

func issue14FindRuleSet(raw any, tag string) M {
	switch list := raw.(type) {
	case []M:
		for _, item := range list {
			if s, _ := item["tag"].(string); s == tag {
				return item
			}
		}
	case []any:
		for _, item := range list {
			if m, ok := item.(map[string]any); ok {
				if s, _ := m["tag"].(string); s == tag {
					return m
				}
			}
		}
	}
	return nil
}

func issue14RouteHasRuleSetField(cfg M, tag string) bool {
	route, _ := cfg["route"].(M)
	if route == nil {
		return false
	}
	return issue14ValueMentionsRuleSetTag(route["rules"], tag)
}

func issue14ValueMentionsRuleSetTag(v any, tag string) bool {
	switch x := v.(type) {
	case string:
		return x == tag
	case []string:
		for _, s := range x {
			if s == tag {
				return true
			}
		}
	case []any:
		for _, item := range x {
			if issue14ValueMentionsRuleSetTag(item, tag) {
				return true
			}
		}
	case []M:
		for _, item := range x {
			if issue14ValueMentionsRuleSetTag(item, tag) {
				return true
			}
		}
	case map[string]any:
		if issue14ValueMentionsRuleSetTag(x["rule_set"], tag) {
			return true
		}
		if nested, ok := x["rules"]; ok && issue14ValueMentionsRuleSetTag(nested, tag) {
			return true
		}
	}
	return false
}

func issue14RouteHasDeprecatedGeoSiteField(cfg M) bool {
	route, _ := cfg["route"].(M)
	if route == nil {
		return false
	}
	if _, ok := route["geosite"]; ok {
		return true
	}
	return issue14RulesHaveDeprecatedGeoSite(route["rules"])
}

func issue14RulesHaveDeprecatedGeoSite(v any) bool {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if issue14RulesHaveDeprecatedGeoSite(item) {
				return true
			}
		}
	case []M:
		for _, item := range x {
			if _, ok := item["geosite"]; ok {
				return true
			}
		}
	case map[string]any:
		if _, ok := x["geosite"]; ok {
			return true
		}
	}
	return false
}

func issue14MiniSRS(t *testing.T) []byte {
	t.Helper()
	plain := option.PlainRuleSet{
		Rules: []option.HeadlessRule{{
			Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultHeadlessRule{
				Domain: badoption.Listable[string]{issue14Domain},
			},
		}},
	}
	var buf bytes.Buffer
	if err := srs.Write(&buf, plain, C.RuleSetVersionCurrent); err != nil {
		t.Fatalf("編譯測試用 .srs：%v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("編譯出的 .srs 是空的")
	}
	return buf.Bytes()
}

func issue14WriteLocalSRS(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, issue14MiniSRS(t), 0o644); err != nil {
		t.Fatalf("寫 %s: %v", path, err)
	}
	return path
}

func issue14MapJSON(m M) string {
	raw, err := json.Marshal(m)
	if err != nil {
		return "<marshal error>"
	}
	return string(raw)
}

func issue14FreeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}
