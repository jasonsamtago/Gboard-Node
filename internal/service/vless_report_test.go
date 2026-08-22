package service

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/controlplane"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/xray"
	"github.com/jasonsamtago/Gboard-Node/internal/limiter"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/tracker"
)

// Official cedar2025/Xboard-Node #68：VLESS 節點能走流量（客戶能下載、
// tcpdump 有包），但每 60s report pushed: 0 users, 0 online，面板 u/d
// 永遠 0。singbox 與 xray 都一樣。官方現場是 vless + tls + ws path=/ws。
//
// 測試先紅：走 kernel 真實 VLESS/TLS/WS 連線（不是手填假計數），再經
// trackAndEnforce → Report。有連線時 report 必須把流量掛到對的 user，
// 且不得是 0 users／0 online；沒連線仍是 0。不准重啟 kernel、不准關
// report。

const (
	vlessReportUserID    = 42
	vlessReportOtherID   = 7
	vlessReportUserUUID  = "11111111-1111-1111-1111-111111111111"
	vlessReportOtherUUID = "22222222-2222-2222-2222-222222222222"
	vlessReportPayload   = 4096
	vlessReportWSPath    = "/ws"
	vlessReportSNI       = "vless.test"
)

func TestVLESSReport_NoConnectionStaysZero(t *testing.T) {
	for _, kernelType := range []string{"singbox", "xray"} {
		t.Run(kernelType, func(t *testing.T) {
			svc, rec, logs, stop := startVLESSReportService(t, kernelType)
			defer stop()

			svc.trackAndEnforce(context.Background())
			report := rec.waitReport(t, svc)
			logText := logs.String()

			if len(report.Traffic) != 0 {
				t.Fatalf("沒連線時 traffic = %#v，要空", report.Traffic)
			}
			if len(report.Online) != 0 {
				t.Fatalf("沒連線時 online = %#v，要空", report.Online)
			}
			if !strings.Contains(logText, "report pushed: 0 users, 0 online") {
				t.Fatalf("沒連線時要留下 report pushed: 0 users, 0 online，log=\n%s", logText)
			}
			t.Logf("沒連線 report 證據: traffic=%v online=%v log 含 %q", report.Traffic, report.Online, "report pushed: 0 users, 0 online")
		})
	}
}

func TestVLESSReport_SingBoxRealConnectionAttributesUser(t *testing.T) {
	assertVLESSReportAttributesUser(t, "singbox")
}

func TestVLESSReport_XrayRealConnectionAttributesUser(t *testing.T) {
	assertVLESSReportAttributesUser(t, "xray")
}

func assertVLESSReportAttributesUser(t *testing.T, kernelType string) {
	t.Helper()

	destLn, destAddr := startVLESSHoldDest(t)
	defer destLn.Close()

	svc, rec, logs, stop := startVLESSReportService(t, kernelType)
	defer stop()

	conn := dialVLESSWS(t, svc.lastConfig.ServerPort, vlessReportUserUUID, destAddr)
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("VLESS 寫入失敗: %v", err)
	}
	got := make([]byte, vlessReportPayload)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("VLESS 讀取失敗（連線沒真正走通）: %v", err)
	}
	if !bytes.Equal(got, bytes.Repeat([]byte("D"), vlessReportPayload)) {
		t.Fatalf("VLESS 下載內容不對，前 8 bytes=%q", got[:min(8, len(got))])
	}

	svc.trackAndEnforce(context.Background())
	report := rec.waitReport(t, svc)

	traffic, ok := report.Traffic[vlessReportUserID]
	if !ok {
		t.Fatalf("%s VLESS/TLS/WS 真實連線後 report.Traffic 沒有 user %d: %#v（官方 #68：0 users）", kernelType, vlessReportUserID, report.Traffic)
	}
	if traffic[0] == 0 && traffic[1] == 0 {
		t.Fatalf("%s report 掛到 user %d 但 u/d 仍是 0: %v", kernelType, vlessReportUserID, traffic)
	}
	if _, ok := report.Traffic[vlessReportOtherID]; ok {
		t.Fatalf("%s 流量掛到別人 %#v", kernelType, report.Traffic)
	}
	if report.Online[vlessReportUserID] == 0 {
		t.Fatalf("%s VLESS 連線仍在，report.Online[%d]=0（官方 #68：0 online） payload=%#v", kernelType, vlessReportUserID, report.Online)
	}

	logText := logs.String()
	if !strings.Contains(logText, "report pushed:") {
		t.Fatalf("%s 沒有 report 日誌:\n%s", kernelType, logText)
	}
	if strings.Contains(logText, "report pushed: 0 users, 0 online") {
		t.Fatalf("%s 有連線仍打出 report pushed: 0 users, 0 online:\n%s", kernelType, logText)
	}
	wantLog := fmt.Sprintf("report pushed: %d users, %d online", len(report.Traffic), len(report.Online))
	if !strings.Contains(logText, wantLog) {
		t.Fatalf("%s report 日誌對不上 users=%d online=%d:\n%s", kernelType, len(report.Traffic), len(report.Online), logText)
	}
	t.Logf("有連線 report 證據 kernel=%s user=%d traffic=%v online=%v log 含 %q", kernelType, vlessReportUserID, traffic, report.Online, wantLog)
}

type reportRecorder struct {
	mu    sync.Mutex
	last  controlplane.ReportPayload
	calls int
	ch    chan controlplane.ReportPayload
}

func newReportRecorder() *reportRecorder {
	return &reportRecorder{ch: make(chan controlplane.ReportPayload, 4)}
}

func (r *reportRecorder) Report(payload controlplane.ReportPayload) error {
	r.mu.Lock()
	r.last = payload
	r.calls++
	r.mu.Unlock()
	r.ch <- payload
	return nil
}
func (r *reportRecorder) ReportDevices(controlplane.PushClient, map[int][]string) {}
func (r *reportRecorder) SupportsReporting() bool                                 { return true }
func (r *reportRecorder) SupportsDeviceReports() bool                             { return false }

func (r *reportRecorder) waitReport(t *testing.T, svc *Service) controlplane.ReportPayload {
	t.Helper()
	svc.pushReportAsync()
	var payload controlplane.ReportPayload
	select {
	case payload = <-r.ch:
	case <-time.After(3 * time.Second):
		t.Fatal("等不到 Report")
		return controlplane.ReportPayload{}
	}
	// ReportPushed 在 Report 回傳之後才寫 log；等 push goroutine 結束再讀 buffer。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if !svc.pushActive.Load() {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return payload
}

type lockedLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *lockedLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *lockedLogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func startVLESSReportService(t *testing.T, kernelType string) (*Service, *reportRecorder, *lockedLogBuf, func()) {
	t.Helper()

	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	port := freeTCPPort(t)
	users := []model.UserSpec{
		{ID: vlessReportUserID, UUID: vlessReportUserUUID},
		{ID: vlessReportOtherID, UUID: vlessReportOtherUUID},
	}
	tlsCert := vlessReportTLSCert(t)
	nc := &model.NodeSpec{
		Protocol:   "vless",
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Network:    "ws",
		NetworkSettings: map[string]any{
			"path": vlessReportWSPath,
			"headers": map[string]any{
				"Host": vlessReportSNI,
			},
		},
		TLS:          1,
		ServerName:   vlessReportSNI,
		TLSSettings:  map[string]any{"server_name": vlessReportSNI},
		CustomRoutes: loopbackDirectRoute(kernelType),
	}

	var k kernel.Kernel
	switch kernelType {
	case "xray":
		k = xray.New(config.KernelConfig{Type: "xray", LogLevel: "warning"})
	default:
		k = singbox.New(config.KernelConfig{Type: "singbox", LogLevel: "warn"})
	}
	if err := k.Start(nc, users, tlsCert); err != nil {
		t.Fatalf("啟動 %s kernel: %v", kernelType, err)
	}

	rec := newReportRecorder()
	sharedLimiter := limiter.New()
	svc := &Service{
		kernel:       k,
		tracker:      tracker.New(),
		sink:         rec,
		source:       controlplane.NewLocalControlPlane(&config.Config{}),
		limiter:      sharedLimiter,
		speedTracker: limiter.NewSpeedTracker(sharedLimiter),
		nodeLog:      nlog.ForNode("vless", port),
		lastConfig:   nc,
		lastUsers:    users,
	}

	waitTLS(t, fmt.Sprintf("127.0.0.1:%d", port))

	return svc, rec, logs, func() { k.Stop() }
}

func loopbackDirectRoute(kernelType string) []map[string]any {
	if kernelType == "xray" {
		return []map[string]any{{
			"type":        "field",
			"ip":          []string{"127.0.0.0/8"},
			"outboundTag": "direct",
		}}
	}
	return []map[string]any{{
		"outbound": "direct",
		"ip_cidr":  []string{"127.0.0.0/8"},
	}}
}

func startVLESSHoldDest(t *testing.T) (net.Listener, *net.TCPAddr) {
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
				_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
				_, _ = conn.Write(bytes.Repeat([]byte("D"), vlessReportPayload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}

func dialVLESSWS(t *testing.T, nodePort int, userUUID string, dest *net.TCPAddr) net.Conn {
	t.Helper()
	uid, err := parseVLESSUUID(userUUID)
	if err != nil {
		t.Fatalf("uuid: %v", err)
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         vlessReportSNI,
		},
	}
	url := fmt.Sprintf("wss://127.0.0.1:%d%s", nodePort, vlessReportWSPath)
	var ws *websocket.Conn
	deadline := time.Now().Add(5 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ws, _, lastErr = dialer.Dial(url, http.Header{"Host": []string{vlessReportSNI}})
		if lastErr == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ws == nil {
		t.Fatalf("連不上 VLESS/TLS/WS :%d: %v", nodePort, lastErr)
	}

	stream := &wsStream{Conn: ws}
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))

	var hdr bytes.Buffer
	hdr.WriteByte(0)
	hdr.Write(uid[:])
	hdr.WriteByte(0)
	hdr.WriteByte(1)
	_ = binary.Write(&hdr, binary.BigEndian, uint16(dest.Port))
	hdr.WriteByte(1)
	ip4 := dest.IP.To4()
	if ip4 == nil {
		stream.Close()
		t.Fatalf("destination 不是 IPv4: %v", dest.IP)
	}
	hdr.Write(ip4)
	if _, err := stream.Write(hdr.Bytes()); err != nil {
		stream.Close()
		t.Fatalf("寫 VLESS 握手: %v", err)
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(stream, resp); err != nil {
		stream.Close()
		t.Fatalf("讀 VLESS 回應: %v", err)
	}
	if resp[1] > 0 {
		if _, err := io.ReadFull(stream, make([]byte, int(resp[1]))); err != nil {
			stream.Close()
			t.Fatalf("讀 VLESS addon: %v", err)
		}
	}
	return stream
}

type wsStream struct {
	*websocket.Conn
	rbuf []byte
}

func (c *wsStream) Read(p []byte) (int, error) {
	if len(c.rbuf) == 0 {
		_, msg, err := c.Conn.ReadMessage()
		if err != nil {
			return 0, err
		}
		c.rbuf = msg
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *wsStream) Write(p []byte) (int, error) {
	if err := c.Conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *wsStream) SetDeadline(t time.Time) error {
	_ = c.Conn.SetReadDeadline(t)
	_ = c.Conn.SetWriteDeadline(t)
	return nil
}

func parseVLESSUUID(s string) ([16]byte, error) {
	var out [16]byte
	raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(raw) != 16 {
		return out, fmt.Errorf("invalid uuid %q", s)
	}
	copy(out[:], raw)
	return out, nil
}

func vlessReportTLSCert(t *testing.T) kernel.TLSCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("tls key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: vlessReportSNI},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{vlessReportSNI, "localhost"},
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

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func waitTLS(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := tls.DialWithDialer(
			&net.Dialer{Timeout: 200 * time.Millisecond},
			"tcp",
			addr,
			&tls.Config{InsecureSkipVerify: true, ServerName: vlessReportSNI},
		)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等不到 %s TLS", addr)
}
