package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/srs"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/option"
	shadowsocks "github.com/sagernet/sing-shadowsocks2"
	"github.com/sagernet/sing/common/json/badoption"
	singM "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

// 對準官方 cedar2025/Xboard-Node #17：什么时候可以上分流。
// 內文空；官方回覆指 #14（Traffic splitting 已可配）。
//
// 「分流」＝面板自訂路由／geosite／geoip／rule_set 進 sing-box
// route.rules，流量依規則走對應 outbound。已合的 #14 鎖 rule_set
// 形狀進 config／起核；本票鎖**用戶能分流**：核必須真帶
// route.rules（含 rule_set／geosite），不得靜默丟掉；客戶端／探測
// 能證明走對 outbound。
//
// 審核 2 鎖定：
//   - Happy：面板 custom route／geosite（rule_set）→ config 有
//     route.rules／route.rule_set，探測打中對應 outbound
//   - 邊界：無自訂路由的普通節點不回歸
//   - 失敗：rules 被丟、rule_set 不上、只 WARN → 必須紅
//   - 再補邊界：結構化 custom_route_rules 仍須分流
//   - 再補失敗：走錯 outbound 必須紅
//   - 不准重開 #14 實作、不准改切 xray、不准 skip、不准改 production
//
// 現 tip（含 #14）若已完整支援 → 回歸鎖（現況已綠、當回歸鎖；官方指 #14）。
// 測仍會在丟掉 rules／rule_set 不上時失敗。

const (
	issue17UserID    = 17
	issue17UserUUID  = "17171717-1717-4717-8717-171717171717"
	issue17SiteTag   = "geosite-cn"
	issue17DomainA   = "via-a.issue17.test"
	issue17DomainB   = "via-b.issue17.test"
	issue17Unmatched = "plain.issue17.test"
	issue17OutA      = "split-a"
	issue17OutB      = "split-b"
	issue17TokenA    = "ISSUE17-MARK-A"
	issue17TokenB    = "ISSUE17-MARK-B"
	issue17Ping      = "issue17-ping"
)

func TestTrafficSplit_面板rule_set必須進config並走對outbound(t *testing.T) {
	srsPath := issue17WriteLocalSRS(t, issue17DomainA)
	markerA := issue17StartSOCKSMarker(t, issue17TokenA)
	spec := issue17PanelSpec(t, issue17PanelJSON(
		[]map[string]any{issue17SOCKSOutbound(issue17OutA, markerA.port)},
		[]map[string]any{issue17OfficialRouteObject(issue17SiteTag, srsPath, issue17OutA)},
		nil,
	))

	built := issue17BuildConfig(t, spec)
	issue17AssertConfigHasSplit(t, built, issue17SiteTag, issue17OutA, srsPath)

	k := issue17StartKernel(t, spec)
	defer k.Stop()
	issue17AssertRuleSetLive(t, k, issue17SiteTag, issue17OutA)

	got := issue17DialSS(t, spec.ServerPort, issue17DomainA, issue17Ping)
	if got != issue17TokenA {
		t.Fatalf("面板配 geosite／rule_set 分流後，客戶端必須走 outbound %q（探測 token=%q）。rules 被丟／rule_set 不上／只 WARN 會打不到。got %q hits=%v",
			issue17OutA, issue17TokenA, got, markerA.hits())
	}
	if !markerA.saw(issue17DomainA) {
		t.Fatalf("探測打中 token 但 SOCKS 沒看到 dest %q：hits=%v。不得只 WARN 假裝套用", issue17DomainA, markerA.hits())
	}
}

func TestTrafficSplit_無自訂路由普通節點不回歸(t *testing.T) {
	spec := issue17PanelSpec(t, issue17PanelJSON(nil, nil, nil))
	if len(spec.CustomRoutes) != 0 || len(spec.CustomRouteRules) != 0 || len(spec.CustomOutbounds) != 0 {
		t.Fatalf("普通節點不該帶自訂分流：routes=%d rules=%d outbounds=%d",
			len(spec.CustomRoutes), len(spec.CustomRouteRules), len(spec.CustomOutbounds))
	}

	built := issue17BuildConfig(t, spec)
	raw := issue17MustJSON(t, built)
	if bytes.Contains(raw, []byte(issue17SiteTag)) || bytes.Contains(raw, []byte(issue17OutA)) {
		t.Fatalf("無自訂路由不得冒出本票 rule_set／分流 outbound。config=%s", raw)
	}
	route, _ := built["route"].(M)
	if route == nil {
		t.Fatal("普通節點仍須有預設 route（private-IP block＋final）")
	}
	if issue17FindRuleSet(route["rule_set"], issue17SiteTag) != nil {
		t.Fatal("普通節點不得帶 geosite-cn rule_set")
	}
	if got, _ := route["final"].(string); got != "direct" {
		t.Fatalf("普通節點 route.final=%q，要 direct", got)
	}

	k := issue17StartKernel(t, spec)
	defer k.Stop()
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", spec.ServerPort))
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q", k.Name())
	}
}

func TestTrafficSplit_rules被丟或只WARN必須紅(t *testing.T) {
	configSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("讀 config.go: %v", err)
	}
	startSrc, err := os.ReadFile("singbox.go")
	if err != nil {
		t.Fatalf("讀 singbox.go: %v", err)
	}
	if !bytes.Contains(configSrc, []byte("nc.CustomRouteRules")) || !bytes.Contains(configSrc, []byte("nc.CustomRoutes")) {
		t.Fatal("關掉自訂 route 當修：buildRoutes 不再吃面板 custom route。rules 被丟必須紅")
	}
	if !bytes.Contains(configSrc, []byte("splitCustomRouteRuleSet")) || !bytes.Contains(configSrc, []byte("rule_set")) {
		t.Fatal("rule_set 不上當修：不再把定義抬到 route.rule_set")
	}
	if !bytes.Contains(startSrc, []byte("parse sing-box options")) {
		t.Fatal("起核不再走 sing-box options parse；只 WARN／改切別的核都是假修")
	}
	if bytes.Contains(startSrc, []byte("kernel/xray")) || bytes.Contains(startSrc, []byte("xray.New")) {
		t.Fatal("不准改切 xray 當修")
	}

	// 執行期：只印 WARN、rules 被丟、rule_set 不上 → 探測打不到 outbound。
	srsPath := issue17WriteLocalSRS(t, issue17DomainA)
	markerA := issue17StartSOCKSMarker(t, issue17TokenA)
	spec := issue17PanelSpec(t, issue17PanelJSON(
		[]map[string]any{issue17SOCKSOutbound(issue17OutA, markerA.port)},
		[]map[string]any{issue17OfficialRouteObject(issue17SiteTag, srsPath, issue17OutA)},
		nil,
	))
	k := issue17StartKernel(t, spec)
	defer k.Stop()
	issue17AssertRuleSetLive(t, k, issue17SiteTag, issue17OutA)
	got := issue17DialSS(t, spec.ServerPort, issue17DomainA, issue17Ping)
	if got != issue17TokenA {
		t.Fatalf("只 WARN／丟掉 rules／rule_set 不上必須紅：探測要打中 %q，got %q hits=%v",
			issue17TokenA, got, markerA.hits())
	}
}

func TestTrafficSplit_結構化custom_route_rules仍須分流(t *testing.T) {
	markerA := issue17StartSOCKSMarker(t, issue17TokenA)
	spec := issue17PanelSpec(t, issue17PanelJSON(
		[]map[string]any{issue17SOCKSOutbound(issue17OutA, markerA.port)},
		nil,
		[]map[string]any{issue17StructuredRule(issue17DomainA, issue17OutA)},
	))
	if len(spec.CustomRouteRules) == 0 {
		t.Fatal("關掉結構化 custom_route_rules 當修：面板有規則，NodeSpec 卻是空的")
	}

	built := issue17BuildConfig(t, spec)
	raw := issue17MustJSON(t, built)
	if !issue17RulesPointTo(built, issue17OutA) {
		t.Fatalf("結構化分流必須進 route.rules 並指到 %q。config=%s", issue17OutA, raw)
	}

	k := issue17StartKernel(t, spec)
	defer k.Stop()
	got := issue17DialSS(t, spec.ServerPort, issue17DomainA, issue17Ping)
	if got != issue17TokenA {
		t.Fatalf("結構化 custom_route_rules 必須走 outbound %q，got %q hits=%v。不得回歸",
			issue17OutA, got, markerA.hits())
	}
}

func TestTrafficSplit_走錯outbound必須紅(t *testing.T) {
	markerA := issue17StartSOCKSMarker(t, issue17TokenA)
	markerB := issue17StartSOCKSMarker(t, issue17TokenB)
	spec := issue17PanelSpec(t, issue17PanelJSON(
		[]map[string]any{
			issue17SOCKSOutbound(issue17OutA, markerA.port),
			issue17SOCKSOutbound(issue17OutB, markerB.port),
		},
		nil,
		[]map[string]any{
			issue17StructuredRule(issue17DomainA, issue17OutA),
			issue17StructuredRule(issue17DomainB, issue17OutB),
		},
	))

	k := issue17StartKernel(t, spec)
	defer k.Stop()

	gotA := issue17DialSS(t, spec.ServerPort, issue17DomainA, issue17Ping)
	if gotA != issue17TokenA {
		t.Fatalf("domain A 必須走 %q（token %q）。走錯 outbound／rules 被丟必須紅。got %q A.hits=%v B.hits=%v",
			issue17OutA, issue17TokenA, gotA, markerA.hits(), markerB.hits())
	}
	if markerB.saw(issue17DomainA) {
		t.Fatalf("domain A 打到 outbound B 了：A.hits=%v B.hits=%v", markerA.hits(), markerB.hits())
	}

	gotB := issue17DialSS(t, spec.ServerPort, issue17DomainB, issue17Ping)
	if gotB != issue17TokenB {
		t.Fatalf("domain B 必須走 %q（token %q）。走錯 outbound 必須紅。got %q A.hits=%v B.hits=%v",
			issue17OutB, issue17TokenB, gotB, markerA.hits(), markerB.hits())
	}
	if markerA.saw(issue17DomainB) {
		t.Fatalf("domain B 打到 outbound A 了：A.hits=%v B.hits=%v", markerA.hits(), markerB.hits())
	}

	_, unmatchedErr := issue17DialSSErr(t, spec.ServerPort, issue17Unmatched, issue17Ping)
	if markerA.saw(issue17Unmatched) || markerB.saw(issue17Unmatched) {
		t.Fatalf("未匹配域名不得打到分流 outbound（全部倒進 A／B 是假修）。A.hits=%v B.hits=%v unmatchedErr=%v",
			markerA.hits(), markerB.hits(), unmatchedErr)
	}
}

func issue17OfficialRouteObject(tag, srsPath, outbound string) map[string]any {
	return map[string]any{
		"rules": []any{
			map[string]any{
				"rule_set": tag,
				"outbound": outbound,
			},
		},
		"rule_set": []any{
			map[string]any{
				"tag":    tag,
				"type":   "local",
				"format": "binary",
				"path":   srsPath,
			},
		},
	}
}

func issue17SOCKSOutbound(tag string, port int) map[string]any {
	return map[string]any{
		"tag":      tag,
		"protocol": "socks",
		"settings": map[string]any{
			"server":      "127.0.0.1",
			"server_port": port,
			"version":     "5",
		},
	}
}

func issue17StructuredRule(domain, outbound string) map[string]any {
	return map[string]any{
		"name": "issue17-" + outbound,
		"match": map[string]any{
			"domains": []string{domain},
		},
		"action": map[string]any{
			"type":   "route",
			"target": outbound,
		},
	}
}

func issue17PanelJSON(outbounds, customRoutes []map[string]any, structured []map[string]any) map[string]any {
	payload := map[string]any{
		"protocol":    "shadowsocks",
		"cipher":      "aes-128-gcm",
		"kernel_type": "singbox",
		"listen_ip":   "127.0.0.1",
	}
	if len(outbounds) > 0 {
		raw := make([]any, 0, len(outbounds))
		for _, item := range outbounds {
			raw = append(raw, item)
		}
		payload["custom_outbounds"] = raw
	}
	if len(customRoutes) > 0 {
		raw := make([]any, 0, len(customRoutes))
		for _, item := range customRoutes {
			raw = append(raw, item)
		}
		payload["custom_routes"] = raw
	}
	if len(structured) > 0 {
		raw := make([]any, 0, len(structured))
		for _, item := range structured {
			raw = append(raw, item)
		}
		payload["custom_route_rules"] = raw
	}
	return payload
}

func issue17PanelSpec(t *testing.T, payload map[string]any) *model.NodeSpec {
	t.Helper()
	payload["server_port"] = sbFreeTCPPort(t)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)

	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "issue17",
		NodeID: 17,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("面板下發分流規則必須能被 node 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	if payload["custom_routes"] != nil && len(cfg.CustomRoutes) == 0 {
		t.Fatal("關掉自訂 route 當修：GetConfig 沒帶 custom_routes")
	}
	if payload["custom_route_rules"] != nil && len(cfg.CustomRouteRules) == 0 {
		t.Fatal("關掉結構化分流當修：GetConfig 沒帶 custom_route_rules")
	}

	spec := model.NodeSpecFromPanel(cfg)
	if spec == nil {
		t.Fatal("NodeSpecFromPanel 回傳 nil：分流規則被丟掉了")
	}
	spec.ListenIP = "127.0.0.1"
	if payload["custom_routes"] != nil && len(spec.CustomRoutes) == 0 && len(spec.CustomRouteRules) == 0 {
		t.Fatal("關掉自訂 route 當修：面板有 custom_routes，NodeSpec 卻是空的")
	}
	return spec
}

func issue17BuildConfig(t *testing.T, spec *model.NodeSpec) M {
	t.Helper()
	return buildConfig(config.KernelConfig{
		Type:      "singbox",
		LogLevel:  "warn",
		ConfigDir: t.TempDir(),
	}, spec, issue17Users(), kernel.TLSCert{})
}

func issue17StartKernel(t *testing.T, spec *model.NodeSpec) *SingBox {
	t.Helper()
	k := New(config.KernelConfig{
		Type:      "singbox",
		LogLevel:  "warn",
		ConfigDir: t.TempDir(),
	})
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}
	err := k.Start(spec, issue17Users(), kernel.TLSCert{})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "rule_set") || strings.Contains(msg, "rule-set") ||
			strings.Contains(msg, "unknown field") || strings.Contains(msg, "unsupported") {
			t.Fatalf("面板分流（rule_set／自訂路由）不得因丟掉 rules／不上 rule_set 而失敗：%v。不准關自訂 route、不准改切 xray", err)
		}
		t.Fatalf("面板配分流後必須能起 sing-box 核：%v。不准關自訂、不准改切 xray", err)
	}
	if !k.IsRunning() {
		t.Fatal("Start 沒回錯但 kernel 沒在跑：只 WARN／略過起核都是假修")
	}
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", spec.ServerPort))
	return k
}

func issue17AssertConfigHasSplit(t *testing.T, built M, tag, outbound, srsPath string) {
	t.Helper()
	raw := issue17MustJSON(t, built)
	route, _ := built["route"].(M)
	if route == nil {
		t.Fatalf("產出沒有 route：rules 被丟掉了。config=%s", raw)
	}
	set := issue17FindRuleSet(route["rule_set"], tag)
	if set == nil {
		t.Fatalf("面板 geosite／rule_set 分流必須保留 route.rule_set tag=%q。rule_set 不上必須紅。config=%s", tag, raw)
	}
	if got, _ := set["type"].(string); got != "local" {
		t.Fatalf("route.rule_set[%q].type=%q，要 local。set=%s", tag, got, issue17MustJSON(t, set))
	}
	if got, _ := set["path"].(string); got != srsPath {
		t.Fatalf("route.rule_set[%q].path=%q，要 %q", tag, got, srsPath)
	}
	if !issue17RulesMentionRuleSet(route["rules"], tag) {
		t.Fatalf("route.rules 必須用 rule_set 引用 %q，不得靜默丟掉。config=%s", tag, raw)
	}
	if !issue17RulesPointTo(built, outbound) {
		t.Fatalf("route.rules 必須指到 outbound %q。config=%s", outbound, raw)
	}
}

func issue17AssertRuleSetLive(t *testing.T, k *SingBox, tag, outbound string) {
	t.Helper()
	router := service.FromContext[adapter.Router](k.ctx)
	if router == nil {
		t.Fatal("起核後沒有 Router：沒走到真實 sing-box")
	}
	rs, ok := router.RuleSet(tag)
	if !ok || rs == nil {
		t.Fatalf("rule_set 不上：running sing-box 找不到 tag %q。只 WARN 不算套用", tag)
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
		if strings.Contains(text, tag) && strings.Contains(text, outbound) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("running rules 必須引用 rule_set %q 且走 %q；rules 被丟／只 WARN 必須紅", tag, outbound)
	}
}

func issue17DialSS(t *testing.T, inboundPort int, destHost, payload string) string {
	t.Helper()
	got, err := issue17DialSSErr(t, inboundPort, destHost, payload)
	if err != nil {
		t.Fatalf("客戶端經 inbound 打 %s 必須走對 outbound：%v。rules 被丟／rule_set 不上／只 WARN 會連不上", destHost, err)
	}
	return got
}

func issue17DialSSErr(t *testing.T, inboundPort int, destHost, payload string) (string, error) {
	t.Helper()
	raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", inboundPort), 2*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial inbound: %w", err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(4 * time.Second))

	method, err := shadowsocks.CreateMethod(context.Background(), "aes-128-gcm", shadowsocks.MethodOptions{
		Password: issue17UserUUID,
	})
	if err != nil {
		return "", fmt.Errorf("ss method: %w", err)
	}
	dest := singM.ParseSocksaddrHostPort(destHost, 443)
	ss, err := method.DialConn(raw, dest)
	if err != nil {
		return "", fmt.Errorf("ss dial %s: %w", destHost, err)
	}
	if _, err := ss.Write([]byte(payload)); err != nil {
		return "", fmt.Errorf("ss write: %w", err)
	}
	buf := make([]byte, 64)
	n, err := ss.Read(buf)
	if err != nil {
		return "", fmt.Errorf("ss read: %w", err)
	}
	return string(buf[:n]), nil
}

type issue17Marker struct {
	mu    sync.Mutex
	seen  []string
	token string
	port  int
}

func (m *issue17Marker) record(host string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seen = append(m.seen, host)
}

func (m *issue17Marker) hits() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.seen))
	copy(out, m.seen)
	return out
}

func (m *issue17Marker) saw(host string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.seen {
		if item == host {
			return true
		}
	}
	return false
}

func issue17StartSOCKSMarker(t *testing.T, token string) *issue17Marker {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen socks marker: %v", err)
	}
	marker := &issue17Marker{
		token: token,
		port:  ln.Addr().(*net.TCPAddr).Port,
	}
	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go issue17HandleSOCKS(conn, marker)
		}
	}()
	return marker
}

func issue17HandleSOCKS(conn net.Conn, marker *issue17Marker) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	hdr := make([]byte, 2)
	if _, err := io.ReadFull(conn, hdr); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return
	}
	if _, err := conn.Write([]byte{5, 0}); err != nil {
		return
	}

	req := make([]byte, 4)
	if _, err := io.ReadFull(conn, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case 1:
		addr := make([]byte, 4)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	case 3:
		l := make([]byte, 1)
		if _, err := io.ReadFull(conn, l); err != nil {
			return
		}
		d := make([]byte, int(l[0]))
		if _, err := io.ReadFull(conn, d); err != nil {
			return
		}
		host = string(d)
	case 4:
		addr := make([]byte, 16)
		if _, err := io.ReadFull(conn, addr); err != nil {
			return
		}
		host = net.IP(addr).String()
	default:
		return
	}
	portb := make([]byte, 2)
	if _, err := io.ReadFull(conn, portb); err != nil {
		return
	}
	marker.record(host)
	if _, err := conn.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}
	buf := make([]byte, 256)
	_, _ = conn.Read(buf)
	_, _ = conn.Write([]byte(marker.token))
}

func issue17WriteLocalSRS(t *testing.T, domain string) string {
	t.Helper()
	plain := option.PlainRuleSet{
		Rules: []option.HeadlessRule{{
			Type: C.RuleTypeDefault,
			DefaultOptions: option.DefaultHeadlessRule{
				Domain: badoption.Listable[string]{domain},
			},
		}},
	}
	var buf bytes.Buffer
	if err := srs.Write(&buf, plain, C.RuleSetVersionCurrent); err != nil {
		t.Fatalf("編譯測試用 .srs：%v", err)
	}
	path := filepath.Join(t.TempDir(), "geosite-cn.srs")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("寫 %s: %v", path, err)
	}
	return path
}

func issue17FindRuleSet(raw any, tag string) M {
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

func issue17RulesMentionRuleSet(v any, tag string) bool {
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
			if issue17RulesMentionRuleSet(item, tag) {
				return true
			}
		}
	case []M:
		for _, item := range x {
			if issue17RulesMentionRuleSet(item, tag) {
				return true
			}
		}
	case map[string]any:
		if issue17RulesMentionRuleSet(x["rule_set"], tag) {
			return true
		}
		if nested, ok := x["rules"]; ok && issue17RulesMentionRuleSet(nested, tag) {
			return true
		}
	}
	return false
}

func issue17RulesPointTo(cfg M, outbound string) bool {
	route, _ := cfg["route"].(M)
	if route == nil {
		return false
	}
	return issue17ValueMentionsOutbound(route["rules"], outbound)
}

func issue17ValueMentionsOutbound(v any, outbound string) bool {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if issue17ValueMentionsOutbound(item, outbound) {
				return true
			}
		}
	case []M:
		for _, item := range x {
			if issue17ValueMentionsOutbound(item, outbound) {
				return true
			}
		}
	case map[string]any:
		if s, _ := x["outbound"].(string); s == outbound {
			return true
		}
	}
	return false
}

func issue17MustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func issue17Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue17UserID, UUID: issue17UserUUID}}
}
