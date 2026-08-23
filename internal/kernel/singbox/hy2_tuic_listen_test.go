package singbox

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid/v5"
	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/hy2inbound"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/tuicinbound"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
	"github.com/sagernet/sing-box/adapter"
	boxTLS "github.com/sagernet/sing-box/common/tls"
	singLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-quic/hysteria2"
	"github.com/sagernet/sing-quic/tuic"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// 官方 cedar2025/Xboard-Node #65：tuic 協議問題，vless 等都可以正常使用，
// hy2／tuic 啟動後連不上節點，節點端實時未監聽端口。
//
// 現行 production 路徑：Protocols()／watchdog／node_type 認 hysteria2，
// 但 buildInbound 只 switch "hysteria"。Protocol=hysteria2／hy2 時
// Start 回成功、IsRunning=true，inbounds 是空的，配置埠沒在聽——
// 正是官方「process 活著、實時未監聽」。走 hy2inbound／tuicinbound。
//
// 審核 2 鎖定：
//  1. 面板起 hy2／tuic 後，配置埠必須真在聽（ss／ListenUDP／/proc/net/udp），
//     不准只 process／IsRunning 活著。
//  2. 客戶端用正確憑證能連上（至少 handshake／建連成功）。
//  3. vless／vmess 正常時 hy2／tuic 也要通，不准只修一邊。
//
// 不准當修：關掉 hy2／tuic、改成只能用 vless／vmess、把「起核失敗」當成功。
// 與 #33（內核退出 watchdog）、#49（熱刪 panic）分開。
// 必須走 production hy2inbound／tuicinbound（override 後的那套）。
//
// 這份測試只鎖行為，不實作修正。未修 tip 必須紅。

const (
	issue65UserID   = 65
	issue65UserUUID = "65656565-6565-4656-8656-656565656565"
	issue65SNI      = "hy2-tuic.listen.test"
	issue65Payload  = "ISSUE65-PONG"
)

func TestHy2_Start後配置埠必須真在聽(t *testing.T) {
	// Protocols()／watchdog／node_type 都認 hysteria2；buildInbound 若只認
	// hysteria，Start 會成功、process 活著，但沒 inbound、埠沒在聽——官方 #65。
	for _, proto := range []string{"hysteria", "hysteria2", "hy2"} {
		t.Run(proto, func(t *testing.T) {
			k, port, _, stop := startIssue65Kernel(t, proto)
			defer stop()
			assertIssue65UDPListening(t, "hy2/"+proto, port, k)
		})
	}
}

func TestTUIC_Start後配置埠必須真在聽(t *testing.T) {
	k, port, _, stop := startIssue65Kernel(t, "tuic")
	defer stop()
	assertIssue65UDPListening(t, "tuic", port, k)
}

func TestHy2_正確憑證必須能handshake建連(t *testing.T) {
	for _, proto := range []string{"hysteria", "hysteria2", "hy2"} {
		t.Run(proto, func(t *testing.T) {
			k, port, dest, stop := startIssue65Kernel(t, proto)
			defer stop()
			assertIssue65UDPListening(t, "hy2/"+proto, port, k)
			if err := handshakeIssue65Hy2(t, port, dest); err != nil {
				t.Fatalf("官方 #65：Hy2(%s) Start 後正確憑證必須能 handshake／建連（不准只 process 活著）: %v", proto, err)
			}
		})
	}
}

func TestTUIC_正確憑證必須能handshake建連(t *testing.T) {
	k, port, dest, stop := startIssue65Kernel(t, "tuic")
	defer stop()
	assertIssue65UDPListening(t, "tuic", port, k)
	if err := handshakeIssue65TUIC(t, port, dest); err != nil {
		t.Fatalf("官方 #65：TUIC Start 後正確憑證必須能 handshake／建連（不准只 process 活著）: %v", err)
	}
}

func TestVLESS正常不准當Hy2TUIC成功假象(t *testing.T) {
	// vless 通了不算 hy2／tuic 修好。hysteria2 別名與 tuic 都要獨立聽、獨立建連。
	vlessK, vlessPort, vlessDest, vlessStop := startIssue65VLESS(t)
	defer vlessStop()
	if err := trySingBoxVLESS(t, vlessPort, issue65UserUUID, vlessDest); err != nil {
		t.Fatalf("對照組 VLESS 必須先通（官方：vless 正常）: %v", err)
	}
	if !vlessK.IsRunning() {
		t.Fatal("對照組 VLESS kernel 必須在跑")
	}

	hy2K, hy2Port, hy2Dest, hy2Stop := startIssue65Kernel(t, "hysteria2")
	defer hy2Stop()
	tuicK, tuicPort, tuicDest, tuicStop := startIssue65Kernel(t, "tuic")
	defer tuicStop()

	if hy2Port == vlessPort || tuicPort == vlessPort {
		t.Fatal("hy2／tuic 不得跟 vless 共用同一埠來假裝聽成功")
	}
	assertIssue65UDPListening(t, "hy2/hysteria2", hy2Port, hy2K)
	assertIssue65UDPListening(t, "tuic", tuicPort, tuicK)
	if err := handshakeIssue65Hy2(t, hy2Port, hy2Dest); err != nil {
		t.Fatalf("vless 通了不算數：Hy2(hysteria2) 仍必須能 handshake／建連: %v", err)
	}
	if err := handshakeIssue65TUIC(t, tuicPort, tuicDest); err != nil {
		t.Fatalf("vless 通了不算數：TUIC 仍必須能 handshake／建連: %v", err)
	}
}

func TestBuildInbound_Hysteria2別名必須產出inbound(t *testing.T) {
	// Start 成功但 inbounds 是空的 → process 活著、埠沒在聽。
	for _, proto := range []string{"hysteria2", "hy2"} {
		inbound := buildInbound(&model.NodeSpec{
			Protocol:   proto,
			ListenIP:   "127.0.0.1",
			ServerPort: 443,
			Version:    2,
		}, issue65Users(), issue65TLSCert(t))
		if inbound == nil {
			t.Errorf("protocol %q 必須產出 inbound（不准 Start 成功卻沒聽埠）", proto)
			continue
		}
		if got, _ := inbound["type"].(string); got != "hysteria2" {
			t.Errorf("protocol %q inbound type=%q，要 hysteria2", proto, got)
		}
		if _, ok := inbound["listen_port"]; !ok {
			t.Errorf("protocol %q inbound 必須有 listen_port", proto)
		}
	}
}

func TestHy2TUIC_仍走production_inbound不准關協議(t *testing.T) {
	startSrc, err := os.ReadFile("singbox.go")
	if err != nil {
		t.Fatalf("讀 singbox.go: %v", err)
	}
	configSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("讀 config.go: %v", err)
	}
	if !bytes.Contains(startSrc, []byte("hy2inbound.RegisterInbound")) || !bytes.Contains(startSrc, []byte("tuicinbound.RegisterInbound")) {
		t.Fatal("關掉 production hy2inbound／tuicinbound 當修：override 不再註冊")
	}
	if !bytes.Contains(startSrc, []byte(`"hysteria2"`)) || !bytes.Contains(startSrc, []byte(`"tuic"`)) {
		t.Fatal("關掉 Hy2／TUIC 當修：Protocols() 不再宣稱支援")
	}
	if !bytes.Contains(configSrc, []byte(`case "hysteria"`)) || !bytes.Contains(configSrc, []byte(`case "tuic"`)) {
		t.Fatal("關掉 Hy2／TUIC 當修：buildInbound 不再產出 hysteria／tuic")
	}
	if !bytes.Contains(configSrc, []byte(`case "hysteria2"`)) && !bytes.Contains(configSrc, []byte(`case "hy2"`)) {
		t.Fatal("protocol hysteria2／hy2 沒進 buildInbound：Start 會成功但沒 inbound、埠沒在聽（官方 #65）")
	}
	if bytes.Contains(startSrc, []byte("kernel/xray")) || bytes.Contains(startSrc, []byte("xray.New")) {
		t.Fatal("不准改切 xray 當修")
	}

	ctx := context.Background()
	raw, err := hy2inbound.NewInbound(ctx, &captureRouter{}, singLog.NewNOPFactory().Logger(), "hy2-lock", option.Hysteria2InboundOptions{
		ListenOptions: option.ListenOptions{ListenPort: 1},
		Users:         []option.Hysteria2User{{Name: issue65UserUUID, Password: issue65UserUUID}},
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: issue65InboundTLS(t),
		},
	})
	if err != nil {
		t.Fatalf("production hy2inbound.NewInbound 必須可用: %v", err)
	}
	if _, ok := raw.(adapter.Inbound); !ok {
		t.Fatal("hy2inbound 必須是 adapter.Inbound（Start 才能起聽）")
	}
	rawTUIC, err := tuicinbound.NewInbound(ctx, &captureRouter{}, singLog.NewNOPFactory().Logger(), "tuic-lock", option.TUICInboundOptions{
		ListenOptions: option.ListenOptions{ListenPort: 1},
		Users:         []option.TUICUser{{Name: issue65UserUUID, UUID: issue65UserUUID, Password: issue65UserUUID}},
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: issue65InboundTLS(t),
		},
	})
	if err != nil {
		t.Fatalf("production tuicinbound.NewInbound 必須可用: %v", err)
	}
	if _, ok := rawTUIC.(adapter.Inbound); !ok {
		t.Fatal("tuicinbound 必須是 adapter.Inbound（Start 才能起聽）")
	}
}

func startIssue65Kernel(t *testing.T, protocol string) (*SingBox, int, *net.TCPAddr, func()) {
	t.Helper()
	nlog.Init(io.Discard, slog.LevelError, false)

	destLn, destAddr := startIssue65Dest(t)
	port := issue65FreeUDPPort(t)
	spec := issue65PanelSpec(t, protocol, port)
	tlsCert := issue65TLSCert(t)

	k := New(config.KernelConfig{Type: "singbox", LogLevel: "warn"})
	if k.Name() != "sing-box" {
		destLn.Close()
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}
	if err := k.Start(spec, issue65Users(), tlsCert); err != nil {
		destLn.Close()
		t.Fatalf("官方 #65：面板起 %s 必須能起核並聽埠，不准把起核失敗當成功: %v", protocol, err)
	}
	if !k.IsRunning() {
		destLn.Close()
		k.Stop()
		t.Fatalf("官方 #65：%s Start 沒回錯但 kernel 沒在跑（不准只假裝起核）", protocol)
	}
	return k, port, destAddr, func() {
		k.Stop()
		destLn.Close()
	}
}

func startIssue65VLESS(t *testing.T) (*SingBox, int, *net.TCPAddr, func()) {
	t.Helper()
	nlog.Init(io.Discard, slog.LevelError, false)
	destLn, destAddr := startSingBoxHotDest(t)
	port := sbFreeTCPPort(t)
	spec := &model.NodeSpec{
		Protocol:   "vless",
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Network:    "tcp",
		CustomRoutes: []map[string]any{{
			"ip_cidr":  []string{"127.0.0.0/8"},
			"outbound": "direct",
		}},
	}
	k := New(config.KernelConfig{Type: "singbox", LogLevel: "warn"})
	if err := k.Start(spec, issue65Users(), kernel.TLSCert{}); err != nil {
		destLn.Close()
		t.Fatalf("對照組 VLESS Start: %v", err)
	}
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))
	return k, port, destAddr, func() {
		k.Stop()
		destLn.Close()
	}
}

func issue65PanelSpec(t *testing.T, protocol string, port int) *model.NodeSpec {
	t.Helper()
	payload := map[string]any{
		"protocol":    protocol,
		"server_port": port,
		"listen_ip":   "127.0.0.1",
		"kernel_type": "singbox",
	}
	switch protocol {
	case "hysteria", "hysteria2", "hy2":
		payload["version"] = 2
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)

	cfg, err := panel.NewClient(config.PanelConfig{URL: ts.URL, Token: "issue65", NodeID: 65}).GetConfig()
	if err != nil {
		t.Fatalf("面板下發 %s 必須能被 node 收下: %v", protocol, err)
	}
	spec := model.NodeSpecFromPanel(cfg)
	if spec == nil {
		t.Fatal("NodeSpecFromPanel 回傳 nil：hy2／tuic 被關掉了")
	}
	spec.ListenIP = "127.0.0.1"
	spec.ServerPort = port
	spec.CustomRoutes = []map[string]any{{
		"ip_cidr":  []string{"127.0.0.0/8"},
		"outbound": "direct",
	}}
	return spec
}

func issue65Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue65UserID, UUID: issue65UserUUID}}
}

func assertIssue65UDPListening(t *testing.T, label string, port int, k *SingBox) {
	t.Helper()
	if k != nil && !k.IsRunning() {
		t.Fatalf("官方 #65：%s kernel 沒在跑；不准把起核失敗當聽成功", label)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if issue65UDPPortTaken(port) && issue65ProcUDPHasPort(port) {
			t.Logf("%s UDP :%d 在聽（ss／ListenUDP 占用）", label, port)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("官方 #65：%s Start 後配置埠 %d 必須真在聽（ss／dial／ListenUDP），不准只 process 活著 running=%v proc=%v taken=%v",
		label, port, k != nil && k.IsRunning(), issue65ProcUDPHasPort(port), issue65UDPPortTaken(port))
}

func issue65UDPPortTaken(port int) bool {
	for _, host := range []string{"127.0.0.1", "::1"} {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(host), Port: port})
		if err != nil {
			return true
		}
		_ = c.Close()
	}
	return false
}

func issue65ProcUDPHasPort(port int) bool {
	want := fmt.Sprintf(":%04X", port)
	for _, path := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || !strings.Contains(fields[1], ":") {
				continue
			}
			if strings.HasSuffix(strings.ToUpper(fields[1]), want) {
				return true
			}
		}
	}
	return false
}

func handshakeIssue65Hy2(t *testing.T, port int, dest *net.TCPAddr) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tlsCfg, err := issue65ClientTLS(ctx)
	if err != nil {
		return err
	}
	client, err := hysteria2.NewClient(hysteria2.ClientOptions{
		Context:       ctx,
		Dialer:        N.SystemDialer,
		Logger:        singLog.NewNOPFactory().Logger(),
		ServerAddress: singM.ParseSocksaddrHostPort("127.0.0.1", uint16(port)),
		Password:      issue65UserUUID,
		TLSConfig:     tlsCfg,
	})
	if err != nil {
		return fmt.Errorf("hy2 client: %w", err)
	}
	defer client.CloseWithError(io.EOF)

	conn, err := client.DialConn(ctx, singM.SocksaddrFromNet(dest))
	if err != nil {
		return fmt.Errorf("hy2 DialConn handshake: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		return fmt.Errorf("hy2 write: %w", err)
	}
	got := make([]byte, len(issue65Payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("hy2 read: %w", err)
	}
	if string(got) != issue65Payload {
		return fmt.Errorf("hy2 payload=%q", got)
	}
	return nil
}

func handshakeIssue65TUIC(t *testing.T, port int, dest *net.TCPAddr) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tlsCfg, err := issue65ClientTLS(ctx)
	if err != nil {
		return err
	}
	uid, err := uuid.FromString(issue65UserUUID)
	if err != nil {
		return err
	}
	client, err := tuic.NewClient(tuic.ClientOptions{
		Context:       ctx,
		Dialer:        N.SystemDialer,
		ServerAddress: singM.ParseSocksaddrHostPort("127.0.0.1", uint16(port)),
		TLSConfig:     tlsCfg,
		UUID:          uid,
		Password:      issue65UserUUID,
	})
	if err != nil {
		return fmt.Errorf("tuic client: %w", err)
	}
	defer client.CloseWithError(io.EOF)

	conn, err := client.DialConn(ctx, singM.SocksaddrFromNet(dest))
	if err != nil {
		return fmt.Errorf("tuic DialConn handshake: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		return fmt.Errorf("tuic write: %w", err)
	}
	got := make([]byte, len(issue65Payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("tuic read: %w", err)
	}
	if string(got) != issue65Payload {
		return fmt.Errorf("tuic payload=%q", got)
	}
	return nil
}

func issue65ClientTLS(ctx context.Context) (boxTLS.Config, error) {
	return boxTLS.NewClient(ctx, singLog.NewNOPFactory().Logger(), "127.0.0.1", option.OutboundTLSOptions{
		Enabled:    true,
		Insecure:   true,
		ServerName: issue65SNI,
		ALPN:       []string{"h3"},
	})
}

func startIssue65Dest(t *testing.T) (net.Listener, *net.TCPAddr) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("dest listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
				_, _ = conn.Write([]byte(issue65Payload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}

func issue65FreeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("free udp: %v", err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	_ = c.Close()
	return port
}

func issue65TLSCert(t *testing.T) kernel.TLSCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("tls key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(65),
		Subject:               pkix.Name{CommonName: issue65SNI},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{issue65SNI, "localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("tls cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return kernel.TLSCert{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	}
}

func issue65InboundTLS(t *testing.T) *option.InboundTLSOptions {
	t.Helper()
	cert := issue65TLSCert(t)
	return &option.InboundTLSOptions{
		Enabled:     true,
		ServerName:  issue65SNI,
		Certificate: []string{string(cert.CertPEM)},
		Key:         []string{string(cert.KeyPEM)},
	}
}
