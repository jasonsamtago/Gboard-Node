package xray

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

// 官方 cedar2025/Xboard-Node #31：vmess+http（xray 路徑一併鎖）。
// 面板選 VMess + HTTP 傳輸後，xray streamSettings 必須真帶 HTTP
//（httpSettings／network=http），不得靜默降 TCP、丟掉 host／path，
// 也不得用 httpupgrade／h2 冒充。本輪不改 production。

const (
	issue31UserID   = 31
	issue31UserUUID = "31313131-3131-4313-8313-313131313131"
	issue31Host     = "cdn.issue31.test"
	issue31Path     = "/vmesshttp"
	issue31Payload  = "ISSUE31-PONG"
)

func TestIssue31_VMessHTTP_MustKeepTransportAndHandshake(t *testing.T) {
	logs := &lockedLogBuf{}
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

	spec := issue31SpecFromPanel(t, nc)
	if spec.Protocol != "vmess" || !strings.EqualFold(spec.Network, "http") {
		t.Fatalf("關掉 HTTP 當修：NodeSpec 沒保留 vmess+http，got protocol=%q network=%q",
			spec.Protocol, spec.Network)
	}

	inbound := buildInbound(spec, issue31Users(), kernel.TLSCert{})
	raw, _ := json.Marshal(inbound)
	if !issue31StreamHasHTTPTransport(inbound, issue31Host, issue31Path) {
		t.Errorf("官方 #31：xray VMess streamSettings 必須真帶 HTTP（network=http＋httpSettings＋host／path），不得只剩 raw TCP／改寫成 h2 冒充／丟掉 host path：\n%s", raw)
	}
	if issue31StreamIsHTTPUpgrade(inbound) {
		t.Errorf("官方 #31：network=http 不得冒充成 httpupgrade：\n%s", raw)
	}
	if issue31StreamIsRawTCP(inbound) {
		t.Errorf("官方 #31：network=http 靜默降成純 TCP：\n%s", raw)
	}
	if issue31StreamRewroteToH2(inbound) {
		t.Errorf("官方 #31：h2 不可冒充成這個 HTTP（applyStreamSettings 把 http 改寫成 h2）：\n%s", raw)
	}

	k, err := startIssue31Xray(t, spec)
	if err != nil {
		t.Fatalf("官方 #31：vmess+http 必須能起 xray 核: %v\nlog=\n%s", err, logs.String())
	}
	defer k.Stop()

	if err := handshakeIssue31VMessHTTP(t, spec.ServerPort, dest, issue31Host, issue31Path); err != nil {
		t.Fatalf("官方 #31：VMess+HTTP 客戶端必須完成 HTTP 握手／連線，got %v\nlog=\n%s\nstream=%s",
			err, logs.String(), raw)
	}
}

func TestIssue31_VMessTCP_Plain_MustStillHandshake(t *testing.T) {
	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, dest := startIssue31Dest(t)
	defer destLn.Close()

	nc := fetchIssue31PanelConfig(t, "tcp", nil)
	spec := issue31SpecFromPanel(t, nc)

	inbound := buildInbound(spec, issue31Users(), kernel.TLSCert{})
	raw, _ := json.Marshal(inbound)
	if !issue31StreamIsRawTCP(inbound) {
		t.Fatalf("無 HTTP 的普通 VMess TCP 不得長出 HTTP transport（改走非 TCP 當修）：\n%s", raw)
	}

	k, err := startIssue31Xray(t, spec)
	if err != nil {
		t.Fatalf("無 HTTP 的普通 VMess TCP 必須能起 xray 核（不得回歸）: %v\nlog=\n%s", err, logs.String())
	}
	defer k.Stop()

	if err := handshakeIssue31VMessTCP(t, spec.ServerPort, dest); err != nil {
		t.Fatalf("無 HTTP 的普通 VMess TCP 必須仍能握手／回聲（不得回歸）: %v", err)
	}
}

func TestIssue31_VMessHTTP_EmptyHost_StillHTTPTransport(t *testing.T) {
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
	if issue31StreamIsRawTCP(inbound) {
		t.Fatalf("空 host 仍須是 HTTP transport，不得靜默降成純 TCP：\n%s", raw)
	}
	if issue31StreamIsHTTPUpgrade(inbound) {
		t.Fatalf("空 host 的 network=http 不得冒充成 httpupgrade：\n%s", raw)
	}
	if !issue31StreamHasHTTPNetwork(inbound) {
		t.Fatalf("空 host 仍須帶 HTTP network，got：\n%s", raw)
	}
	if !strings.Contains(string(raw), issue31Path) {
		t.Fatalf("空 host 不得把 path=%q 一併丟掉：\n%s", issue31Path, raw)
	}
	if strings.Contains(string(raw), issue31Host) {
		t.Fatalf("空 host 不該寫入偽裝 Host %q：\n%s", issue31Host, raw)
	}

	k, err := startIssue31Xray(t, spec)
	if err != nil {
		t.Fatalf("空 host 的 vmess+http 必須仍能起核（不得因缺 host 改降 TCP）: %v", err)
	}
	defer k.Stop()
}

func TestIssue31_HttpUpgradeAndH2MustNotImpersonateHTTP(t *testing.T) {
	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, dest := startIssue31Dest(t)
	defer destLn.Close()

	httpInbound := buildInbound(issue31SpecFromPanel(t, fetchIssue31PanelConfig(t, "http", map[string]any{
		"path": issue31Path,
		"host": issue31Host,
	})), issue31Users(), kernel.TLSCert{})
	if issue31StreamIsHTTPUpgrade(httpInbound) {
		raw, _ := json.Marshal(httpInbound)
		t.Errorf("官方 #31：network=http 產出不得是 httpupgrade：\n%s", raw)
	}
	if issue31StreamRewroteToH2(httpInbound) {
		raw, _ := json.Marshal(httpInbound)
		t.Errorf("官方 #31：h2 不可冒充成這個 HTTP（http 被改寫成 h2）：\n%s", raw)
	}

	upgradeSpec := issue31SpecFromPanel(t, fetchIssue31PanelConfig(t, "httpupgrade", map[string]any{
		"path": issue31Path,
		"host": issue31Host,
	}))
	upgradeInbound := buildInbound(upgradeSpec, issue31Users(), kernel.TLSCert{})
	upgradeRaw, _ := json.Marshal(upgradeInbound)
	if !issue31StreamIsHTTPUpgrade(upgradeInbound) {
		t.Fatalf("httpupgrade 節點必須仍是 httpupgrade（不准改成這個 HTTP 當修）：\n%s", upgradeRaw)
	}

	k, err := startIssue31Xray(t, upgradeSpec)
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
}

func fetchIssue31PanelConfig(t *testing.T, network string, settings map[string]any) *panel.NodeConfig {
	t.Helper()
	port := freeTCPPort(t)
	payload := map[string]any{
		"protocol":    "vmess",
		"kernel_type": "xray",
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
		"type":        "field",
		"ip":          []string{"127.0.0.0/8"},
		"outboundTag": "direct",
	}}
	return spec
}

func startIssue31Xray(t *testing.T, spec *model.NodeSpec) (*Xray, error) {
	t.Helper()
	k := New(config.KernelConfig{Type: "xray", LogLevel: "warning"})
	if k.Name() != "xray" {
		t.Fatalf("xray 路徑 Name()=%q，要 xray", k.Name())
	}
	err := k.Start(spec, issue31Users(), kernel.TLSCert{})
	if err != nil {
		return k, err
	}
	if !k.IsRunning() {
		return k, fmt.Errorf("Start 沒回錯但 kernel 沒在跑")
	}
	issue31WaitTCP(t, fmt.Sprintf("127.0.0.1:%d", spec.ServerPort))
	return k, nil
}

func issue31Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue31UserID, UUID: issue31UserUUID}}
}

func issue31Stream(inbound M) M {
	if inbound == nil {
		return nil
	}
	ss, _ := inbound["streamSettings"].(M)
	return ss
}

func issue31StreamHasHTTPNetwork(inbound M) bool {
	ss := issue31Stream(inbound)
	if ss == nil {
		return false
	}
	return strings.EqualFold(fmt.Sprint(ss["network"]), "http")
}

func issue31StreamRewroteToH2(inbound M) bool {
	ss := issue31Stream(inbound)
	if ss == nil {
		return false
	}
	return strings.EqualFold(fmt.Sprint(ss["network"]), "h2")
}

func issue31StreamIsHTTPUpgrade(inbound M) bool {
	ss := issue31Stream(inbound)
	if ss == nil {
		return false
	}
	if strings.EqualFold(fmt.Sprint(ss["network"]), "httpupgrade") {
		return true
	}
	_, ok := ss["httpupgradeSettings"]
	return ok && !issue31StreamHasHTTPNetwork(inbound)
}

func issue31StreamIsRawTCP(inbound M) bool {
	ss := issue31Stream(inbound)
	if ss == nil {
		return true
	}
	network := strings.ToLower(fmt.Sprint(ss["network"]))
	if network == "" || network == "tcp" {
		if tcp, ok := ss["tcpSettings"].(M); ok && tcp != nil {
			raw, _ := json.Marshal(tcp)
			if strings.Contains(strings.ToLower(string(raw)), `"type":"http"`) {
				return false
			}
		}
		return true
	}
	return false
}

func issue31StreamHasHTTPTransport(inbound M, host, path string) bool {
	if !issue31StreamHasHTTPNetwork(inbound) {
		return false
	}
	ss := issue31Stream(inbound)
	httpSettings, _ := ss["httpSettings"].(M)
	if httpSettings == nil {
		return false
	}
	raw, err := json.Marshal(httpSettings)
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
	clientPort := freeTCPPort(t)
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

// 客戶端用 sing-box VMess+HTTP。xray-core 26 已移除 HTTP transport（改走 XHTTP），
// 不能再拿 network=http 當客戶端來躲測。
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

func issue31WaitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等不到 %s: %v", addr, last)
}
