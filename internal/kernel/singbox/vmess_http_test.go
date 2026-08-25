package singbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	singJSON "github.com/sagernet/sing/common/json"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
)

// 官方 cedar2025/Xboard-Node #31：vmess+http。
// 面板選 VMess + HTTP 傳輸（network=http，含 host／path）後，sing-box
// inbound 必須真帶 HTTP transport，客戶端能依 HTTP 握手／連線。
//
// 審核 2 鎖定（本輪只加失敗測，不改 production）：
//  1. Happy：protocol=vmess 且 network=http（host／path）→ inbound
//     真帶 HTTP（不得只剩 raw TCP）；host／path 不得丟；VMess+HTTP
//     客戶端能握手／回聲。
//  2. 邊界：VMess + 無 HTTP（普通 TCP）行為與現況一致。
//  3. 失敗：宣稱 HTTP 但 host／path 被丟、靜默降 TCP、或 Start 成功
//     卻連不上 HTTP 客戶端 → 必須紅。
//  4. 再補邊界：空 host 仍須是 HTTP transport，不得降 TCP。
//  5. 再補失敗：httpupgrade／h2 不可冒充成這個 HTTP。
//
// 不准當修：關 HTTP、改成只支援純 TCP、改切 xray、把 HTTP 當不支援忽略、
// 用 httpupgrade／h2 冒充。

const (
	issue31UserID   = 31
	issue31UserUUID = "31313131-3131-4313-8313-313131313131"
	issue31Host     = "cdn.issue31.test"
	issue31Path     = "/vmesshttp"
	issue31Payload  = "ISSUE31-PONG"
)

func TestIssue31_VMessHTTP_MustKeepTransportAndHandshake(t *testing.T) {
	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, dest := startIssue31Dest(t)
	defer destLn.Close()

	nc := fetchIssue31PanelConfig(t, "http", map[string]any{
		"path": issue31Path,
		"host": issue31Host,
	})
	if nc.Protocol != "vmess" || !strings.EqualFold(nc.Network, "http") {
		t.Fatalf("關掉 HTTP 當修：GetConfig 沒保留 protocol=vmess network=http，got protocol=%q network=%q",
			nc.Protocol, nc.Network)
	}
	if fmt.Sprint(nc.NetworkSettings["path"]) != issue31Path || fmt.Sprint(nc.NetworkSettings["host"]) != issue31Host {
		t.Fatalf("關掉 HTTP 當修：GetConfig 沒保留 path=%q host=%q，got %#v",
			issue31Path, issue31Host, nc.NetworkSettings)
	}

	spec := issue31SpecFromPanel(t, nc)
	if spec.Protocol != "vmess" || !strings.EqualFold(spec.Network, "http") {
		t.Fatalf("關掉 HTTP 當修：NodeSpec 沒保留 vmess+http，got protocol=%q network=%q",
			spec.Protocol, spec.Network)
	}

	cfg := buildConfig(config.KernelConfig{Type: "singbox", LogLevel: "warn"}, spec, issue31Users(), kernel.TLSCert{})
	inbound := issue31FirstInbound(cfg)
	raw, _ := json.Marshal(inbound)
	if !issue31InboundHasHTTPTransport(inbound, issue31Host, issue31Path) {
		t.Errorf("官方 #31：sing-box VMess inbound 必須真帶 HTTP transport（type=http＋host＋path），不得只剩 raw TCP／丟掉 host path：\n%s", raw)
	}
	if issue31InboundIsHTTPUpgrade(inbound) {
		t.Errorf("官方 #31：network=http 不得冒充成 httpupgrade：\n%s", raw)
	}
	if issue31InboundIsRawTCP(inbound) {
		t.Errorf("官方 #31：network=http 靜默降成純 TCP：\n%s", raw)
	}

	k, err := startIssue31SingBox(t, spec)
	if err != nil {
		t.Fatalf("官方 #31：vmess+http 必須能起 sing-box 核（不准關 HTTP、不准改切 xray）: %v\nlog=\n%s",
			err, logs.String())
	}
	defer k.Stop()

	if err := handshakeIssue31VMessHTTP(t, spec.ServerPort, dest, issue31Host, issue31Path); err != nil {
		t.Fatalf("官方 #31：VMess+HTTP 客戶端必須完成 HTTP 握手／連線（現況若降 TCP／丟 host path 會紅），got %v\nlog=\n%s\ninbound=%s",
			err, logs.String(), raw)
	}
}

func TestIssue31_VMessTCP_Plain_MustStillHandshake(t *testing.T) {
	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, dest := startIssue31Dest(t)
	defer destLn.Close()

	nc := fetchIssue31PanelConfig(t, "tcp", nil)
	spec := issue31SpecFromPanel(t, nc)
	if spec.Protocol != "vmess" || !strings.EqualFold(spec.Network, "tcp") {
		t.Fatalf("無 HTTP 回歸組必須是 vmess+tcp，got protocol=%q network=%q", spec.Protocol, spec.Network)
	}

	inbound := buildInbound(spec, issue31Users(), kernel.TLSCert{})
	raw, _ := json.Marshal(inbound)
	if !issue31InboundIsRawTCP(inbound) {
		t.Fatalf("無 HTTP 的普通 VMess TCP 不得長出 HTTP transport（改走非 TCP 當修）：\n%s", raw)
	}

	k, err := startIssue31SingBox(t, spec)
	if err != nil {
		t.Fatalf("無 HTTP 的普通 VMess TCP 必須能起 sing-box 核（不得回歸）: %v\nlog=\n%s", err, logs.String())
	}
	defer k.Stop()

	if err := handshakeIssue31VMessTCP(t, spec.ServerPort, dest); err != nil {
		t.Fatalf("無 HTTP 的普通 VMess TCP 必須仍能握手／回聲（不得回歸）: %v", err)
	}
}

func TestIssue31_VMessHTTP_EmptyHost_StillHTTPTransport(t *testing.T) {
	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	nc := fetchIssue31PanelConfig(t, "http", map[string]any{
		"path": issue31Path,
	})
	spec := issue31SpecFromPanel(t, nc)
	if spec.NetworkSettings != nil {
		if _, ok := spec.NetworkSettings["host"]; ok {
			t.Fatalf("空 host 邊界組不得自己補 host：%#v", spec.NetworkSettings)
		}
	}

	inbound := buildInbound(spec, issue31Users(), kernel.TLSCert{})
	raw, _ := json.Marshal(inbound)
	if issue31InboundIsRawTCP(inbound) {
		t.Fatalf("空 host 仍須是 HTTP transport，不得靜默降成純 TCP：\n%s", raw)
	}
	if issue31InboundIsHTTPUpgrade(inbound) {
		t.Fatalf("空 host 的 network=http 不得冒充成 httpupgrade：\n%s", raw)
	}
	if !issue31InboundHasHTTPType(inbound) {
		t.Fatalf("空 host 仍須帶 HTTP transport type，got：\n%s", raw)
	}
	if !strings.Contains(string(raw), issue31Path) {
		t.Fatalf("空 host 不得把 path=%q 一併丟掉：\n%s", issue31Path, raw)
	}
	if issue31InboundKeepsHost(inbound, issue31Host) {
		t.Fatalf("空 host 不該寫入偽裝 Host %q：\n%s", issue31Host, raw)
	}

	k, err := startIssue31SingBox(t, spec)
	if err != nil {
		t.Fatalf("空 host 的 vmess+http 必須仍能起核（不得因缺 host 改降 TCP）: %v\nlog=\n%s", err, logs.String())
	}
	defer k.Stop()
}

func TestIssue31_HttpUpgradeAndH2MustNotImpersonateHTTP(t *testing.T) {
	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, dest := startIssue31Dest(t)
	defer destLn.Close()

	httpInbound := buildInbound(issue31SpecFromPanel(t, fetchIssue31PanelConfig(t, "http", map[string]any{
		"path": issue31Path,
		"host": issue31Host,
	})), issue31Users(), kernel.TLSCert{})
	if issue31InboundIsHTTPUpgrade(httpInbound) {
		raw, _ := json.Marshal(httpInbound)
		t.Errorf("官方 #31：network=http 產出不得是 httpupgrade：\n%s", raw)
	}

	upgradeSpec := issue31SpecFromPanel(t, fetchIssue31PanelConfig(t, "httpupgrade", map[string]any{
		"path": issue31Path,
		"host": issue31Host,
	}))
	upgradeInbound := buildInbound(upgradeSpec, issue31Users(), kernel.TLSCert{})
	upgradeRaw, _ := json.Marshal(upgradeInbound)
	if !issue31InboundIsHTTPUpgrade(upgradeInbound) {
		t.Fatalf("httpupgrade 節點必須仍是 httpupgrade（不准改成這個 HTTP 當修）：\n%s", upgradeRaw)
	}
	if issue31InboundHasHTTPTransport(upgradeInbound, issue31Host, issue31Path) && !issue31InboundIsHTTPUpgrade(upgradeInbound) {
		t.Errorf("httpupgrade 不得被改寫成這個 HTTP：\n%s", upgradeRaw)
	}

	k, err := startIssue31SingBox(t, upgradeSpec)
	if err != nil {
		t.Fatalf("httpupgrade 節點必須能起核（本測只鎖不可冒充 HTTP）: %v\nlog=\n%s", err, logs.String())
	}
	defer k.Stop()

	if err := handshakeIssue31VMessHTTPFastFail(t, upgradeSpec.ServerPort, dest, issue31Host, issue31Path); err == nil {
		t.Fatal("官方 #31：VMess+HTTP 客戶端打 httpupgrade inbound 必須連不上（httpupgrade 不可冒充成這個 HTTP）")
	}

	h2Spec := issue31SpecFromPanel(t, fetchIssue31PanelConfig(t, "h2", map[string]any{
		"path": issue31Path,
		"host": issue31Host,
	}))
	if !strings.EqualFold(h2Spec.Network, "h2") {
		t.Fatalf("h2 節點 Network 必須仍是 h2，got %q（不准改成 http 冒充）", h2Spec.Network)
	}
	h2Inbound := buildInbound(h2Spec, issue31Users(), kernel.TLSCert{})
	h2Raw, _ := json.Marshal(h2Inbound)
	// h2 可以是 HTTP/2（sing-box type=http），但面板選 h2 不得被當成已實作 network=http。
	// 鎖：h2 節點 Network 仍是 h2；HTTP/1.1（tcp+http header）客戶端打 h2 不得被本票當成功。
	if h2Spec.Network == "http" {
		t.Errorf("h2 不可冒充成這個 HTTP：NodeSpec.Network 被改成 http\ninbound=%s", h2Raw)
	}
}

func fetchIssue31PanelConfig(t *testing.T, network string, settings map[string]any) *panel.NodeConfig {
	t.Helper()
	port := sbFreeTCPPort(t)
	payload := map[string]any{
		"protocol":    "vmess",
		"kernel_type": "singbox",
		"server_port": port,
		"listen_ip":   "127.0.0.1",
		"network":     network,
		"tls":         0,
	}
	if settings != nil {
		payload["networkSettings"] = settings
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)

	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "issue31",
		NodeID: 31,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("面板下發 VMess HTTP 必須能被 node 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	return cfg
}

func issue31SpecFromPanel(t *testing.T, nc *panel.NodeConfig) *model.NodeSpec {
	t.Helper()
	spec := model.NodeSpecFromPanel(nc)
	if spec == nil {
		t.Fatal("NodeSpecFromPanel 回傳 nil")
	}
	spec.CustomRoutes = []map[string]any{{
		"ip_cidr":  []string{"127.0.0.0/8"},
		"outbound": "direct",
	}}
	return spec
}

func startIssue31SingBox(t *testing.T, spec *model.NodeSpec) (*SingBox, error) {
	t.Helper()
	k := New(config.KernelConfig{Type: "singbox", LogLevel: "warn"})
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}
	err := k.Start(spec, issue31Users(), kernel.TLSCert{})
	if err != nil {
		return k, err
	}
	if !k.IsRunning() {
		return k, fmt.Errorf("Start 沒回錯但 kernel 沒在跑")
	}
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", spec.ServerPort))
	return k, nil
}

func issue31Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue31UserID, UUID: issue31UserUUID}}
}

func issue31FirstInbound(cfg M) M {
	inbounds, _ := cfg["inbounds"].([]M)
	if len(inbounds) == 0 {
		return nil
	}
	return inbounds[0]
}

func issue31InboundHasHTTPType(inbound M) bool {
	if inbound == nil {
		return false
	}
	transport, _ := inbound["transport"].(M)
	if transport == nil {
		return false
	}
	return strings.EqualFold(fmt.Sprint(transport["type"]), "http")
}

func issue31InboundIsHTTPUpgrade(inbound M) bool {
	if inbound == nil {
		return false
	}
	transport, _ := inbound["transport"].(M)
	if transport == nil {
		return false
	}
	return strings.EqualFold(fmt.Sprint(transport["type"]), "httpupgrade")
}

func issue31InboundIsRawTCP(inbound M) bool {
	if inbound == nil {
		return true
	}
	transport, ok := inbound["transport"]
	if !ok || transport == nil {
		return true
	}
	return false
}

func issue31InboundKeepsHost(inbound M, host string) bool {
	if inbound == nil || host == "" {
		return false
	}
	raw, _ := json.Marshal(inbound)
	return strings.Contains(string(raw), host)
}

func issue31InboundHasHTTPTransport(inbound M, host, path string) bool {
	if !issue31InboundHasHTTPType(inbound) {
		return false
	}
	raw, err := json.Marshal(inbound)
	if err != nil {
		return false
	}
	s := string(raw)
	if !strings.Contains(s, path) {
		return false
	}
	if host != "" && !strings.Contains(s, host) {
		return false
	}
	if issue31InboundIsHTTPUpgrade(inbound) {
		return false
	}
	return true
}

func startIssue31Dest(t *testing.T) (net.Listener, *net.TCPAddr) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("destination listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = conn.Write([]byte(issue31Payload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}

func handshakeIssue31VMessHTTP(t *testing.T, nodePort int, dest *net.TCPAddr, host, path string) error {
	t.Helper()
	return issue31DownloadViaSingBox(t, dest, issue31HTTPClientConfig(nodePort, host, path), 6*time.Second)
}

func handshakeIssue31VMessHTTPFastFail(t *testing.T, nodePort int, dest *net.TCPAddr, host, path string) error {
	t.Helper()
	return issue31DownloadViaSingBox(t, dest, issue31HTTPClientConfig(nodePort, host, path), 1500*time.Millisecond)
}

func handshakeIssue31VMessTCP(t *testing.T, nodePort int, dest *net.TCPAddr) error {
	t.Helper()
	return issue31DownloadViaSingBox(t, dest, issue31TCPClientConfig(nodePort), 6*time.Second)
}

func issue31DownloadViaSingBox(t *testing.T, dest *net.TCPAddr, cfg M, wait time.Duration) error {
	t.Helper()
	clientPort := sbFreeTCPPort(t)
	inbounds, _ := cfg["inbounds"].([]M)
	if len(inbounds) > 0 {
		inbounds[0]["listen_port"] = clientPort
	}
	stop, err := startIssue31SingBoxClient(cfg)
	if err != nil {
		return err
	}
	defer stop()
	if err := issue31WaitListen(fmt.Sprintf("127.0.0.1:%d", clientPort)); err != nil {
		return err
	}

	var last error
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		conn, err := issue31SOCKS5Dial(fmt.Sprintf("127.0.0.1:%d", clientPort), dest)
		if err != nil {
			last = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
		if _, err := conn.Write([]byte("ping")); err != nil {
			_ = conn.Close()
			last = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		got := make([]byte, len(issue31Payload))
		if _, err := io.ReadFull(conn, got); err != nil {
			_ = conn.Close()
			last = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = conn.Close()
		if string(got) != issue31Payload {
			return fmt.Errorf("payload=%q want %q", got, issue31Payload)
		}
		return nil
	}
	if last == nil {
		last = fmt.Errorf("timeout")
	}
	return last
}

func startIssue31SingBoxClient(cfg M) (func(), error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshal client config: %w", err)
	}
	ctx, cancel := context.WithCancel(include.Context(context.Background()))
	opts, err := singJSON.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("parse client sing-box: %w\n%s", err, data)
	}
	inst, err := box.New(box.Options{Context: ctx, Options: opts})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("create client sing-box: %w", err)
	}
	if err := inst.Start(); err != nil {
		inst.Close()
		cancel()
		return nil, fmt.Errorf("start client sing-box: %w", err)
	}
	return func() {
		_ = inst.Close()
		cancel()
	}, nil
}

// issue31HTTPClientConfig 用 sing-box VMess + transport.type=http（host／path）。
// 這是面板選 VMess+HTTP 後客戶端依 HTTP 握手的契約，不得改成純 TCP。
// 不用 xray-core network=http：該版本已把 HTTP transport 移除並改走 XHTTP。
func issue31HTTPClientConfig(nodePort int, host, path string) M {
	transport := M{"type": "http", "path": path}
	if host != "" {
		transport["host"] = []string{host}
	}
	return M{
		"log": M{"level": "warn"},
		"inbounds": []M{{
			"type":        "mixed",
			"tag":         "client-in",
			"listen":      "127.0.0.1",
			"listen_port": 0,
		}},
		"outbounds": []M{{
			"type":        "vmess",
			"tag":         "vmess-http",
			"server":      "127.0.0.1",
			"server_port": nodePort,
			"uuid":        issue31UserUUID,
			"security":    "auto",
			"transport":   transport,
		}},
		"route": M{"final": "vmess-http"},
	}
}

func issue31TCPClientConfig(nodePort int) M {
	return M{
		"log": M{"level": "warn"},
		"inbounds": []M{{
			"type":        "mixed",
			"tag":         "client-in",
			"listen":      "127.0.0.1",
			"listen_port": 0,
		}},
		"outbounds": []M{{
			"type":        "vmess",
			"tag":         "vmess-tcp",
			"server":      "127.0.0.1",
			"server_port": nodePort,
			"uuid":        issue31UserUUID,
			"security":    "auto",
		}},
		"route": M{"final": "vmess-tcp"},
	}
}

func issue31SOCKS5Dial(proxyAddr string, dest *net.TCPAddr) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		conn.Close()
		return nil, err
	}
	hello := make([]byte, 2)
	if _, err := io.ReadFull(conn, hello); err != nil {
		conn.Close()
		return nil, err
	}
	if hello[0] != 5 || hello[1] != 0 {
		conn.Close()
		return nil, fmt.Errorf("socks5 hello %v", hello)
	}
	ip := dest.IP.To4()
	if ip == nil {
		conn.Close()
		return nil, fmt.Errorf("dest 不是 IPv4: %v", dest.IP)
	}
	req := []byte{5, 1, 0, 1, ip[0], ip[1], ip[2], ip[3], 0, 0}
	binary.BigEndian.PutUint16(req[8:], uint16(dest.Port))
	if _, err := conn.Write(req); err != nil {
		conn.Close()
		return nil, err
	}
	reply := make([]byte, 10)
	if _, err := io.ReadFull(conn, reply); err != nil {
		conn.Close()
		return nil, err
	}
	if reply[1] != 0 {
		conn.Close()
		return nil, fmt.Errorf("socks5 connect status=%d", reply[1])
	}
	return conn, nil
}

func issue31WaitListen(addr string) error {
	deadline := time.Now().Add(3 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("等不到 client listen %s: %v", addr, last)
}
