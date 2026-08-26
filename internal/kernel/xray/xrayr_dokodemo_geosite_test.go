package xray

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
	"github.com/xtls/xray-core/app/router"
	"google.golang.org/protobuf/proto"
)

// 官方 cedar2025/Xboard-Node #39：未能完全迁移xrayr。
// 審核 2 鎖定（本輪只加失敗測，不改 production）：
//  限界是 Xray 核（inbound＋routing）。
//  Happy A：protocol=dokodemo-door（含 Dokodemo-Door／dokodemo）
//    產出 inbound，寫 settings.address/port/network/timeout；
//    Start 後聽埠，連上轉到父節點 address:port。
//  Happy B：domain:["geosite:youtube"] 原樣進 routing.rules 且能起核
//    （geo 走已有 #20）。
//  邊界：vmess／vless 不回歸；Linux／#20 geo 不動。
//  失敗：缺 address／port 明示失敗，不准當 unsupported 丟 inbound。
//
// 不准當修：靜默 buildInbound=nil、拆掉 geosite、改切 sing-box tun、
// 做 WARP／DNS 解鎖、整包重做 XrayR、改 #20 下載路徑。

const (
	issue39UserID       = 39
	issue39UserUUID     = "39393939-3939-4393-8393-393939393939"
	issue39GeoSiteTag   = "geosite:youtube"
	issue39OutboundTag  = "宋仲基"
	issue39Payload      = "ISSUE39-PONG"
	issue39Timeout      = 120
	issue39Network      = "tcp,udp"
	issue39Unsupported  = "unsupported protocol"
	issue39NoInboundMsg = "no inbound configured"
)

func TestIssue39_DokodemoDoor_MustBuildListenAndForward(t *testing.T) {
	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	parentTCP, parentUDP, parentPort := startIssue39Parent(t)
	defer parentTCP.Close()
	defer parentUDP.Close()

	nc := fetchIssue39DokodemoPanel(t, "dokodemo-door", "127.0.0.1", parentPort, issue39Network, issue39Timeout)
	if !issue39IsDokodemoProtocol(nc.Protocol) {
		t.Fatalf("關掉 Dokodemo 當修：GetConfig 沒保留 protocol=dokodemo-door，got %q", nc.Protocol)
	}

	spec := issue39SpecFromPanel(t, nc)
	if !issue39IsDokodemoProtocol(spec.Protocol) {
		t.Fatalf("關掉 Dokodemo 當修：NodeSpec 沒保留 protocol=dokodemo-door，got %q", spec.Protocol)
	}

	k := New(config.KernelConfig{Type: "xray", LogLevel: "warn"})
	if k.Name() != "xray" {
		t.Fatalf("不准改切 sing-box／其他核當修：Name()=%q，要 xray", k.Name())
	}
	if !issue39ProtocolsHasDokodemo(k.Protocols()) {
		t.Errorf("官方 #39：Protocols() 必須含 dokodemo-door（含別名），否則 validateNodeRuntime 會當不支援丟掉：%v", k.Protocols())
	}

	inbound := issue39FirstInbound(buildConfig(config.KernelConfig{Type: "xray", LogLevel: "warn"}, spec, issue39Users(), kernel.TLSCert{}))
	raw := issue39MustJSON(t, inbound)
	if inbound == nil {
		t.Errorf("官方 #39：buildConfig 必須帶 Dokodemo-Door inbound，不得因未知協議省略 inbounds（現況 buildInbound default 回 nil）")
	}
	if !issue39InboundIsDokodemo(inbound) {
		t.Errorf("官方 #39：inbound.protocol 必須是 dokodemo-door，不得靜默忽略／當未知丟掉：\n%s", raw)
	}
	if !issue39InboundHasSettings(inbound, "127.0.0.1", parentPort, issue39Network, issue39Timeout) {
		t.Errorf("官方 #39：inbound.settings 必須帶 address/port/network/timeout（%s %d %s %d）：\n%s",
			"127.0.0.1", parentPort, issue39Network, issue39Timeout, raw)
	}

	started, err := startIssue39Xray(t, spec)
	if started != nil {
		defer started.Stop()
	}
	logText := logs.String()
	if strings.Contains(strings.ToLower(logText), issue39Unsupported) &&
		strings.Contains(strings.ToLower(logText), issue39NoInboundMsg) {
		t.Errorf("官方 #39：不得只印 WARN unsupported／no inbound 而不建 inbound：\n%s", logText)
	}
	if err != nil {
		t.Fatalf("官方 #39：protocol=dokodemo-door 必須能起 xray 核（不准關任意門、不准改切 sing-box）: %v\nlog=\n%s",
			err, logText)
	}
	if started == nil || !started.IsRunning() {
		t.Fatal("官方 #39：Start 沒回錯但 kernel 沒在跑")
	}

	if !issue39WaitTCP(spec.ServerPort) {
		t.Fatalf("官方 #39：Start 後配置埠 %d 必須真在聽，不准 process 活著卻不建 inbound running=%v\nlog=\n%s\ninbound=%s",
			spec.ServerPort, started.IsRunning(), logText, raw)
	}

	if err := issue39DialTCPForward(spec.ServerPort, issue39Payload); err != nil {
		t.Fatalf("官方 #39：連上 Dokodemo 聽埠必須轉到 settings.address:port（TCP）：%v\nlog=\n%s\ninbound=%s",
			err, logText, raw)
	}
	if err := issue39DialUDPForward(spec.ServerPort, issue39Payload); err != nil {
		t.Fatalf("官方 #39：network=tcp,udp 時 UDP 也必須轉到父節點：%v\nlog=\n%s\ninbound=%s",
			err, logText, raw)
	}
}

func TestIssue39_DokodemoDoor_AliasesMustBuildInbound(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)
	parentPort := 443
	for _, proto := range []string{"dokodemo-door", "Dokodemo-Door", "dokodemo"} {
		t.Run(proto, func(t *testing.T) {
			spec := issue39DokodemoSpec(t, proto, "127.0.0.1", parentPort, issue39Network, issue39Timeout)
			inbound := buildInbound(spec, issue39Users(), kernel.TLSCert{})
			raw := issue39MustJSON(t, inbound)
			if inbound == nil || !issue39InboundIsDokodemo(inbound) {
				t.Fatalf("別名 %q 仍須產出 inbound protocol=dokodemo-door，不得丟掉／當未知忽略：\n%s", proto, raw)
			}
			if !issue39InboundHasSettings(inbound, "127.0.0.1", parentPort, issue39Network, issue39Timeout) {
				t.Fatalf("別名 %q 仍須寫 settings.address/port/network/timeout：\n%s", proto, raw)
			}
		})
	}
}

func TestIssue39_GeositeYoutube_MustStayInRoutingAndStart(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)
	issue39InstallFakeGeoHTTP(t)

	nc := fetchIssue39GeositePanel(t)
	if len(nc.CustomRoutes) == 0 {
		t.Fatal("關掉自訂 route 當修：GetConfig 沒帶 custom_routes")
	}
	spec := issue39SpecFromPanel(t, nc)
	if len(spec.CustomRoutes) == 0 {
		t.Fatal("關掉自訂 route 當修：面板有 geosite:youtube，NodeSpec 卻是空的")
	}

	built := buildConfig(config.KernelConfig{Type: "xray", LogLevel: "warn"}, spec, issue39Users(), kernel.TLSCert{})
	raw := issue39MustJSON(t, built)
	if !issue39RoutingKeepsGeoSite(built, issue39GeoSiteTag, issue39OutboundTag) {
		t.Fatalf("官方 #39：domain:%q 必須原樣留在 routing.rules 並指向 outboundTag %q。拆掉 geosite 當修必須紅。config=%s",
			issue39GeoSiteTag, issue39OutboundTag, raw)
	}

	configSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("讀 config.go: %v", err)
	}
	if !bytes.Contains(configSrc, []byte("nc.CustomRoutes")) {
		t.Fatal("拆掉 geosite 當修：buildRouting 不再吃 CustomRoutes")
	}
	xraySrc, err := os.ReadFile("xray.go")
	if err != nil {
		t.Fatalf("讀 xray.go: %v", err)
	}
	if !bytes.Contains(xraySrc, []byte("geodata.Ensure")) {
		t.Fatal("不准改 #20 下載路徑：ensureGeoData 不再呼叫 geodata.Ensure")
	}

	dir := t.TempDir()
	k := New(config.KernelConfig{Type: "xray", LogLevel: "warn", GeoDataDir: dir})
	if k.Name() != "xray" {
		t.Fatalf("不准改切 sing-box 當修：Name()=%q", k.Name())
	}
	if err := k.Start(spec, issue39Users(), kernel.TLSCert{}); err != nil {
		t.Fatalf("官方 #39：domain:%q 原樣進 routing 後必須能起 xray 核（geo 走 #20）：%v", issue39GeoSiteTag, err)
	}
	t.Cleanup(k.Stop)
	if !k.IsRunning() {
		t.Fatal("Start 沒回錯但 kernel 沒在跑：略過起核是假修")
	}
	if _, err := os.Stat(filepath.Join(dir, "geosite.dat")); err != nil {
		t.Fatalf("geo 必須走已有 #20 自動下載：起核後 geosite.dat 不存在（%v）。不准改下載路徑、不准預先手動放檔", err)
	}
}

func TestIssue39_VMessVLESS_MustNotRegress(t *testing.T) {
	t.Run("vless", func(t *testing.T) {
		x, port, dest, _, stop := startXrayVLESS(t, issue39Users())
		defer stop()
		if x.Name() != "xray" {
			t.Fatalf("不准改切其他核：Name()=%q", x.Name())
		}
		if err := tryVLESSUser(t, port, issue39UserUUID, dest); err != nil {
			t.Fatalf("既有 VLESS 必須仍能握手／回聲（不得回歸）: %v", err)
		}
	})

	t.Run("vmess", func(t *testing.T) {
		nlog.Init(io.Discard, slog.LevelError, false)
		destLn, dest := startIssue31Dest(t)
		defer destLn.Close()

		spec := issue31SpecFromPanel(t, fetchIssue31PanelConfig(t, "tcp", nil))
		if spec.Protocol != "vmess" {
			t.Fatalf("VMess 回歸組必須是 vmess，got %q", spec.Protocol)
		}
		inbound := buildInbound(spec, issue31Users(), kernel.TLSCert{})
		if inbound == nil || fmt.Sprint(inbound["protocol"]) != "vmess" {
			t.Fatalf("既有 VMess 不得被 Dokodemo 測拆掉：inbound=%s", issue39MustJSON(t, inbound))
		}
		k, err := startIssue31Xray(t, spec)
		if err != nil {
			t.Fatalf("既有 VMess TCP 必須能起核（不得回歸）: %v", err)
		}
		defer k.Stop()
		if err := handshakeIssue31VMessTCP(t, spec.ServerPort, dest); err != nil {
			t.Fatalf("既有 VMess 必須仍能握手／回聲（不得回歸）: %v", err)
		}
	})
}

func TestIssue39_DokodemoMissingAddressPort_MustErrorNotDrop(t *testing.T) {
	for _, tc := range []struct {
		name    string
		address string
		port    int
	}{
		{name: "缺 address", address: "", port: 443},
		{name: "缺 port", address: "127.0.0.1", port: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &lockedLogBuf{}
			nlog.Init(logs, slog.LevelDebug, false)

			spec := issue39DokodemoSpec(t, "dokodemo-door", tc.address, tc.port, issue39Network, issue39Timeout)
			inbound := buildInbound(spec, issue39Users(), kernel.TLSCert{})
			k, err := startIssue39Xray(t, spec)
			if k != nil {
				defer k.Stop()
			}
			logText := logs.String()

			if inbound == nil && strings.Contains(strings.ToLower(logText), issue39Unsupported) {
				t.Errorf("官方 #39：缺 address／port 不准當 unsupported 丟 inbound（現況 buildInbound default 回 nil）\nlog=\n%s", logText)
			}
			if err == nil {
				t.Fatalf("官方 #39：缺 address／port 起核必須明示失敗，不得 Start 成功當空核 running=%v inbound=%s",
					k != nil && k.IsRunning(), issue39MustJSON(t, inbound))
			}
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "unsupported") && !strings.Contains(msg, "address") && !strings.Contains(msg, "port") {
				t.Fatalf("不准當 unsupported 丟 inbound：%v", err)
			}
			if !strings.Contains(msg, "address") && !strings.Contains(msg, "port") {
				t.Fatalf("缺 address／port 必須明示（error 要提到 address 或 port）：%v", err)
			}
		})
	}
}

func fetchIssue39DokodemoPanel(t *testing.T, protocol, address string, port int, network string, timeout int) *panel.NodeConfig {
	t.Helper()
	listen := freeTCPPort(t)
	payload := map[string]any{
		"protocol":    protocol,
		"kernel_type": "xray",
		"listen_ip":   "127.0.0.1",
		"server_port": listen,
		"networkSettings": map[string]any{
			"address": address,
			"port":    port,
			"network": network,
			"timeout": timeout,
		},
	}
	return issue39GetConfig(t, payload)
}

func fetchIssue39GeositePanel(t *testing.T) *panel.NodeConfig {
	t.Helper()
	payload := map[string]any{
		"protocol":    "shadowsocks",
		"cipher":      "aes-128-gcm",
		"kernel_type": "xray",
		"listen_ip":   "127.0.0.1",
		"server_port": freeTCPPort(t),
		"custom_outbounds": []any{
			map[string]any{
				"tag":      issue39OutboundTag,
				"protocol": "freedom",
			},
		},
		"custom_routes": []any{
			map[string]any{
				"type":        "field",
				"outboundTag": issue39OutboundTag,
				"domain":      []string{issue39GeoSiteTag},
			},
		},
	}
	return issue39GetConfig(t, payload)
}

func issue39GetConfig(t *testing.T, payload map[string]any) *panel.NodeConfig {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)

	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "issue39",
		NodeID: issue39UserID,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("面板下發官方 #39 形狀必須能被 node 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	return cfg
}

func issue39SpecFromPanel(t *testing.T, nc *panel.NodeConfig) *model.NodeSpec {
	t.Helper()
	spec := model.NodeSpecFromPanel(nc)
	if spec == nil {
		t.Fatal("NodeSpecFromPanel 回傳 nil：官方 #39 欄位被丟掉了")
	}
	spec.ListenIP = "127.0.0.1"
	return spec
}

func issue39DokodemoSpec(t *testing.T, protocol, address string, port int, network string, timeout int) *model.NodeSpec {
	t.Helper()
	settings := map[string]any{
		"network": network,
		"timeout": timeout,
	}
	if address != "" {
		settings["address"] = address
	}
	if port > 0 {
		settings["port"] = port
	}
	return &model.NodeSpec{
		Protocol:        protocol,
		ListenIP:        "127.0.0.1",
		ServerPort:      freeTCPPort(t),
		KernelType:      "xray",
		NetworkSettings: settings,
	}
}

func startIssue39Xray(t *testing.T, spec *model.NodeSpec) (*Xray, error) {
	t.Helper()
	k := New(config.KernelConfig{Type: "xray", LogLevel: "warn"})
	if k.Name() != "xray" {
		t.Fatalf("不准改切其他核當修：Name()=%q，要 xray", k.Name())
	}
	err := k.Start(spec, issue39Users(), kernel.TLSCert{})
	if err != nil {
		return k, err
	}
	if !k.IsRunning() {
		return k, fmt.Errorf("Start 沒回錯但 kernel 沒在跑")
	}
	return k, nil
}

func issue39Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue39UserID, UUID: issue39UserUUID}}
}

func issue39FirstInbound(cfg M) M {
	raw, _ := cfg["inbounds"].([]M)
	if len(raw) == 0 {
		return nil
	}
	return raw[0]
}

func issue39IsDokodemoProtocol(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "dokodemo-door", "dokodemo":
		return true
	default:
		return false
	}
}

func issue39ProtocolsHasDokodemo(list []string) bool {
	for _, item := range list {
		if issue39IsDokodemoProtocol(item) {
			return true
		}
	}
	return false
}

func issue39InboundIsDokodemo(inbound M) bool {
	if inbound == nil {
		return false
	}
	return issue39IsDokodemoProtocol(fmt.Sprint(inbound["protocol"]))
}

func issue39InboundHasSettings(inbound M, address string, port int, network string, timeout int) bool {
	if inbound == nil {
		return false
	}
	settings, _ := inbound["settings"].(M)
	if settings == nil {
		if nested, ok := inbound["settings"].(map[string]any); ok {
			settings = M(nested)
		}
	}
	if settings == nil {
		return false
	}
	if fmt.Sprint(settings["address"]) != address {
		return false
	}
	if issue39AnyInt(settings["port"]) != port {
		return false
	}
	if fmt.Sprint(settings["network"]) != network {
		return false
	}
	if issue39AnyInt(settings["timeout"]) != timeout {
		return false
	}
	return true
}

func issue39AnyInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		var parsed int
		_, _ = fmt.Sscanf(fmt.Sprint(v), "%d", &parsed)
		return parsed
	}
}

func issue39RoutingKeepsGeoSite(cfg M, site, outbound string) bool {
	routing, _ := cfg["routing"].(M)
	if routing == nil {
		return false
	}
	rules, _ := routing["rules"].([]M)
	for _, rule := range rules {
		if fmt.Sprint(rule["outboundTag"]) != outbound {
			continue
		}
		switch domains := rule["domain"].(type) {
		case []string:
			for _, item := range domains {
				if item == site {
					return true
				}
			}
		case []any:
			for _, item := range domains {
				if fmt.Sprint(item) == site {
					return true
				}
			}
		}
	}
	return false
}

func startIssue39Parent(t *testing.T) (net.Listener, *net.UDPConn, int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("parent tcp listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				buf := make([]byte, 256)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				if string(buf[:n]) == issue39Payload {
					_, _ = conn.Write([]byte(issue39Payload))
				}
			}(c)
		}
	}()

	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	if err != nil {
		ln.Close()
		t.Fatalf("parent udp listen: %v", err)
	}
	go func() {
		buf := make([]byte, 256)
		for {
			n, addr, err := udp.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if string(buf[:n]) == issue39Payload {
				_, _ = udp.WriteToUDP([]byte(issue39Payload), addr)
			}
		}
	}()
	return ln, udp, port
}

func issue39WaitTCP(port int) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 150*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(40 * time.Millisecond)
	}
	return false
}

func issue39DialTCPForward(port int, payload string) error {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		return err
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		return err
	}
	if string(got) != payload {
		return fmt.Errorf("payload=%q want %q", got, payload)
	}
	return nil
}

func issue39DialUDPForward(port int, payload string) error {
	c, err := net.DialTimeout("udp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(payload)); err != nil {
		return err
	}
	got := make([]byte, len(payload))
	n, err := c.Read(got)
	if err != nil {
		return err
	}
	if string(got[:n]) != payload {
		return fmt.Errorf("udp payload=%q want %q", got[:n], payload)
	}
	return nil
}

func issue39MustJSON(t *testing.T, v any) string {
	t.Helper()
	if v == nil {
		return "null"
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(raw)
}

func issue39InstallFakeGeoHTTP(t *testing.T) {
	t.Helper()
	hook := &issue20GeoHook{
		geoIP:   mustMiniGeoIPDAT(t),
		geoSite: mustIssue39GeoSiteDAT(t),
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "geoip"):
			hook.ipHits.Add(1)
			_, _ = w.Write(hook.geoIP)
		case strings.Contains(r.URL.Path, "geosite"):
			hook.siteHits.Add(1)
			_, _ = w.Write(hook.geoSite)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	origAsset, hadAsset := os.LookupEnv("XRAY_LOCATION_ASSET")
	t.Cleanup(func() {
		if hadAsset {
			_ = os.Setenv("XRAY_LOCATION_ASSET", origAsset)
		} else {
			_ = os.Unsetenv("XRAY_LOCATION_ASSET")
		}
	})

	orig := http.DefaultTransport
	http.DefaultTransport = &issue20RewriteTransport{base: orig, fake: ts.URL}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

func mustIssue39GeoSiteDAT(t *testing.T) []byte {
	t.Helper()
	raw, err := proto.Marshal(&router.GeoSiteList{
		Entry: []*router.GeoSite{{
			CountryCode: "YOUTUBE",
			Domain: []*router.Domain{{
				Type:  router.Domain_Domain,
				Value: "youtube.com",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal issue39 geosite.dat: %v", err)
	}
	return raw
}
