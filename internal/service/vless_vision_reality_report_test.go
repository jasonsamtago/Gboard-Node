package service

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/controlplane"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/xray"
	"github.com/jasonsamtago/Gboard-Node/internal/limiter"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/tracker"
	xrayCore "github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/infra/conf/serial"
)

// Official cedar2025/Xboard-Node #21：VLESS + xtls-rprx-vision + reality，
// YouTube 2K 實時約 6w Kbps，中轉面板 0.25GB，管理面板只有 0.01GB（設了
// 2.0x 倍率仍差很多）。懷疑 vision splice／直拷路徑沒進 user stats。
//
// 測試先紅：走 xray 真實 VLESS + flow xtls-rprx-vision + reality，內層
// 必須是 TLS 1.3（才會切 splice／直拷），再經 GetUserTraffic →
// trackAndEnforce → Report。report 下載必須接近目的端實際寫出的字節，
// 不能少一個數量級。沒流量仍是 0。不准灌假流量、不准用倍率湊數。
// VMess／普通 VLESS／TCP 無 vision 不能被改壞。

const (
	visionReportUserID    = 21
	visionReportOtherID   = 7
	visionReportUserUUID  = "21111111-1111-1111-1111-111111111111"
	visionReportOtherUUID = "27777777-7777-7777-7777-777777777777"
	visionReportPayload   = 256 * 1024
	visionReportSNI       = "vision.reality.test"
	visionReportShortID   = "0123456789abcdef"
)

func TestVLESSVisionReality_NoConnectionStaysZero(t *testing.T) {
	svc, rec, logs, dest, stop := startVisionRealityNode(t)
	defer dest.close()
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

	evidence := fmt.Sprintf("沒連線 vision+reality report 證據: traffic=%v online=%v dest_write=%d dest_read=%d log 含 %q\n%s",
		report.Traffic, report.Online, dest.written.Load(), dest.read.Load(), "report pushed: 0 users, 0 online", logText)
	writeVisionEvidence(t, "vision_reality_no_traffic_report.log", evidence)
	t.Log(evidence)
}

func TestVLESSVisionReality_RealDownloadReportsActualBytes(t *testing.T) {
	svc, rec, logs, dest, stop := startVisionRealityNode(t)
	defer dest.close()
	defer stop()

	clientPort := freeTCPPort(t)
	clientStop := startXrayTunnelClient(t, visionClientConfig(clientPort, svc.lastConfig.ServerPort, dest.addr.Port, dest.pub))
	defer clientStop()

	actualDown, actualUp := downloadViaTunnelTLS(t, clientPort, dest.payload)

	svc.trackAndEnforce(context.Background())
	report := rec.waitReport(t, svc)
	logText := logs.String()

	traffic, ok := report.Traffic[visionReportUserID]
	if !ok {
		t.Fatalf("VLESS vision+reality 真實下載後 report.Traffic 沒有 user %d: %#v（官方 #21）", visionReportUserID, report.Traffic)
	}
	if _, ok := report.Traffic[visionReportOtherID]; ok {
		t.Fatalf("流量掛到別人 %#v", report.Traffic)
	}

	reportUp, reportDown := traffic[0], traffic[1]
	if reportDown*10 < actualDown {
		t.Fatalf("vision splice／直拷少報一個數量級: dest 實際寫出 download=%d，report download=%d（官方 #21：0.25GB vs 0.01GB）。不准用倍率湊數。\n%s",
			actualDown, reportDown, visionEvidenceLine(actualUp, actualDown, reportUp, reportDown, report.Online, logText))
	}
	if reportDown == 0 && actualDown > 0 {
		t.Fatalf("dest 實際寫出 download=%d，report download 仍是 0", actualDown)
	}
	if strings.Contains(logText, "report pushed: 0 users, 0 online") {
		t.Fatalf("有真實流量仍打出 report pushed: 0 users, 0 online:\n%s", logText)
	}
	wantLog := fmt.Sprintf("report pushed: %d users, %d online", len(report.Traffic), len(report.Online))
	if !strings.Contains(logText, wantLog) {
		t.Fatalf("report 日誌對不上 users=%d online=%d:\n%s", len(report.Traffic), len(report.Online), logText)
	}

	evidence := visionEvidenceLine(actualUp, actualDown, reportUp, reportDown, report.Online, logText)
	writeVisionEvidence(t, "vision_reality_download_report.log", evidence)
	t.Log(evidence)
}

func TestVLESSPlainTCP_RealDownloadStillReports(t *testing.T) {
	assertOtherProtocolStillReports(t, "vless-tcp-no-vision", "vless")
}

func TestVMess_RealDownloadStillReports(t *testing.T) {
	assertOtherProtocolStillReports(t, "vmess-tcp", "vmess")
}

func assertOtherProtocolStillReports(t *testing.T, name, protocol string) {
	t.Helper()

	dest, payload := startRawDest(t, visionReportPayload/4)
	defer dest.close()

	port := freeTCPPort(t)
	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	nc := &model.NodeSpec{
		Protocol:     protocol,
		ListenIP:     "127.0.0.1",
		ServerPort:   port,
		Network:      "tcp",
		TLS:          0,
		CustomRoutes: loopbackDirectRoute("xray"),
	}
	users := []model.UserSpec{
		{ID: visionReportUserID, UUID: visionReportUserUUID},
		{ID: visionReportOtherID, UUID: visionReportOtherUUID},
	}

	k := xray.New(config.KernelConfig{Type: "xray", LogLevel: "warning"})
	if err := k.Start(nc, users, kernel.TLSCert{}); err != nil {
		t.Fatalf("啟動 %s: %v", name, err)
	}
	defer k.Stop()
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))

	rec := newReportRecorder()
	sharedLimiter := limiter.New()
	svc := &Service{
		kernel:       k,
		tracker:      tracker.New(),
		sink:         rec,
		source:       controlplane.NewLocalControlPlane(&config.Config{}),
		limiter:      sharedLimiter,
		speedTracker: limiter.NewSpeedTracker(sharedLimiter),
		nodeLog:      nlog.ForNode(protocol, port),
		lastConfig:   nc,
		lastUsers:    users,
	}

	clientPort := freeTCPPort(t)
	clientStop := startXrayTunnelClient(t, rawTunnelClientConfig(protocol, clientPort, port, dest.addr.Port))
	defer clientStop()

	actualDown, actualUp := downloadViaTunnelRaw(t, clientPort, payload)
	svc.trackAndEnforce(context.Background())
	report := rec.waitReport(t, svc)
	logText := logs.String()

	traffic, ok := report.Traffic[visionReportUserID]
	if !ok {
		t.Fatalf("%s 真實下載後 report.Traffic 沒有 user %d: %#v（其他協議被改壞）", name, visionReportUserID, report.Traffic)
	}
	if traffic[1]*10 < actualDown {
		t.Fatalf("%s 被改壞：actual download=%d report download=%d", name, actualDown, traffic[1])
	}
	if traffic[1] == 0 && actualDown > 0 {
		t.Fatalf("%s report download 是 0，actual=%d", name, actualDown)
	}

	evidence := fmt.Sprintf("%s 對照證據 actual_up=%d actual_down=%d report_up=%d report_down=%d online=%v\n%s",
		name, actualUp, actualDown, traffic[0], traffic[1], report.Online, logText)
	writeVisionEvidence(t, name+"_report.log", evidence)
	t.Log(evidence)
}

type countedTLSDest struct {
	ln      net.Listener
	addr    *net.TCPAddr
	payload []byte
	written atomic.Int64
	read    atomic.Int64
	priv    string
	pub     string
}

func (d *countedTLSDest) close() {
	if d != nil && d.ln != nil {
		_ = d.ln.Close()
	}
}

func startVisionRealityNode(t *testing.T) (*Service, *reportRecorder, *lockedLogBuf, *countedTLSDest, func()) {
	t.Helper()

	dest, payload := startTLS13Dest(t, visionReportPayload)
	priv, pub := visionRealityKeyPair(t)
	dest.priv, dest.pub = priv, pub
	dest.payload = payload

	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	port := freeTCPPort(t)
	users := []model.UserSpec{
		{ID: visionReportUserID, UUID: visionReportUserUUID},
		{ID: visionReportOtherID, UUID: visionReportOtherUUID},
	}
	nc := &model.NodeSpec{
		Protocol:   "vless",
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Network:    "tcp",
		TLS:        2,
		Flow:       "xtls-rprx-vision",
		ServerName: visionReportSNI,
		TLSSettings: map[string]any{
			"private_key": priv,
			"short_id":    visionReportShortID,
			"server_name": visionReportSNI,
			"dest":        dest.addr.String(),
		},
		CustomRoutes: loopbackDirectRoute("xray"),
	}

	k := xray.New(config.KernelConfig{Type: "xray", LogLevel: "warning"})
	if err := k.Start(nc, users, kernel.TLSCert{}); err != nil {
		dest.close()
		t.Fatalf("啟動 xray VLESS vision+reality: %v", err)
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
		nodeLog:      nlog.ForNode("vless-vision", port),
		lastConfig:   nc,
		lastUsers:    users,
	}

	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))
	return svc, rec, logs, dest, func() { k.Stop() }
}

func startRawDest(t *testing.T, payloadSize int) (*countedTLSDest, []byte) {
	t.Helper()
	payload := bytes.Repeat([]byte("R"), payloadSize)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("raw dest listen: %v", err)
	}
	dest := &countedTLSDest{ln: ln, addr: ln.Addr().(*net.TCPAddr), payload: payload}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				counted := &countedConn{Conn: conn, written: &dest.written, read: &dest.read}
				_, _ = counted.Write(payload)
				_, _ = io.Copy(io.Discard, counted)
			}(c)
		}
	}()
	return dest, payload
}

func startTLS13Dest(t *testing.T, payloadSize int) (*countedTLSDest, []byte) {
	t.Helper()
	payload := bytes.Repeat([]byte("D"), payloadSize)
	cert := visionTLSCert(t, visionReportSNI)
	tlsCert, err := tls.X509KeyPair(cert.CertPEM, cert.KeyPEM)
	if err != nil {
		t.Fatalf("tls pair: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
	})
	if err != nil {
		t.Fatalf("tls dest listen: %v", err)
	}
	dest := &countedTLSDest{ln: ln, addr: ln.Addr().(*net.TCPAddr), payload: payload}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				counted := &countedConn{Conn: conn, written: &dest.written, read: &dest.read}
				_, _ = counted.Write(payload)
				_, _ = io.Copy(io.Discard, counted)
			}(c)
		}
	}()
	return dest, payload
}

type countedConn struct {
	net.Conn
	written *atomic.Int64
	read    *atomic.Int64
}

func (c *countedConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.read.Add(int64(n))
	return n, err
}

func (c *countedConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.written.Add(int64(n))
	return n, err
}

func downloadViaTunnelRaw(t *testing.T, clientPort int, payload []byte) (actualDown, actualUp int64) {
	t.Helper()
	var lastErr error
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", clientPort), 300*time.Millisecond)
		if err != nil {
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		upN, err := conn.Write([]byte("ping"))
		if err != nil {
			_ = conn.Close()
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, got); err != nil {
			_ = conn.Close()
			lastErr = err
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = conn.Close()
		if !bytes.Equal(got, payload) {
			t.Fatalf("raw 下載內容不對，前 8 bytes=%q", got[:min(8, len(got))])
		}
		return int64(len(payload)), int64(upN)
	}
	t.Fatalf("raw tunnel 下載失敗: %v", lastErr)
	return 0, 0
}

func downloadViaTunnelTLS(t *testing.T, clientPort int, payload []byte) (actualDown, actualUp int64) {
	t.Helper()
	var raw net.Conn
	var err error
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		raw, err = net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", clientPort), 300*time.Millisecond)
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if raw == nil {
		t.Fatalf("連不上 tunnel client :%d: %v", clientPort, err)
	}
	defer raw.Close()

	tlsConn := tls.Client(raw, &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         visionReportSNI,
		MinVersion:         tls.VersionTLS13,
		MaxVersion:         tls.VersionTLS13,
	})
	_ = tlsConn.SetDeadline(time.Now().Add(10 * time.Second))
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("tunnel TLS handshake: %v", err)
	}
	if _, err := tlsConn.Write([]byte("ping")); err != nil {
		t.Fatalf("tunnel TLS 寫入: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(tlsConn, got); err != nil {
		t.Fatalf("tunnel TLS 讀取失敗（連線沒真正走通）: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("下載內容不對，前 8 bytes=%q", got[:min(8, len(got))])
	}
	// dest 實際寫出的明文 payload 就是要比對的下載量；TLS record 會多一點，
	// 少一個數量級的判定用 payload 長度即可。
	return int64(len(payload)), int64(len("ping"))
}

func startXrayTunnelClient(t *testing.T, cfg map[string]any) func() {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal client config: %v", err)
	}
	pb, err := serial.LoadJSONConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("parse client config: %v\n%s", err, data)
	}
	inst, err := xrayCore.New(pb)
	if err != nil {
		t.Fatalf("create client xray: %v", err)
	}
	if err := inst.Start(); err != nil {
		inst.Close()
		t.Fatalf("start client xray: %v", err)
	}
	return func() { _ = inst.Close() }
}

func visionClientConfig(clientPort, nodePort, destPort int, pub string) map[string]any {
	return map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []map[string]any{{
			"listen":   "127.0.0.1",
			"port":     clientPort,
			"protocol": "dokodemo-door",
			"settings": map[string]any{
				"address": "127.0.0.1",
				"port":    destPort,
				"network": "tcp",
			},
		}},
		"outbounds": []map[string]any{{
			"protocol": "vless",
			"settings": map[string]any{
				"vnext": []map[string]any{{
					"address": "127.0.0.1",
					"port":    nodePort,
					"users": []map[string]any{{
						"id":         visionReportUserUUID,
						"encryption": "none",
						"flow":       "xtls-rprx-vision",
					}},
				}},
			},
			"streamSettings": map[string]any{
				"network":  "tcp",
				"security": "reality",
				"realitySettings": map[string]any{
					"serverName":  visionReportSNI,
					"fingerprint": "chrome",
					"publicKey":   pub,
					"shortId":     visionReportShortID,
				},
			},
		}},
	}
}

func rawTunnelClientConfig(protocol string, clientPort, nodePort, destPort int) map[string]any {
	user := map[string]any{"id": visionReportUserUUID}
	if protocol == "vless" {
		user["encryption"] = "none"
	} else {
		user["alterId"] = 0
		user["security"] = "auto"
	}
	return map[string]any{
		"log": map[string]any{"loglevel": "warning"},
		"inbounds": []map[string]any{{
			"listen":   "127.0.0.1",
			"port":     clientPort,
			"protocol": "dokodemo-door",
			"settings": map[string]any{
				"address": "127.0.0.1",
				"port":    destPort,
				"network": "tcp",
			},
		}},
		"outbounds": []map[string]any{{
			"protocol": protocol,
			"settings": map[string]any{
				"vnext": []map[string]any{{
					"address": "127.0.0.1",
					"port":    nodePort,
					"users":   []map[string]any{user},
				}},
			},
			"streamSettings": map[string]any{
				"network":  "tcp",
				"security": "none",
			},
		}},
	}
}

func visionRealityKeyPair(t *testing.T) (priv, pub string) {
	t.Helper()
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("x25519: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(key.Bytes()),
		base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
}

func visionTLSCert(t *testing.T, sni string) kernel.TLSCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("tls key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: sni},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{sni, "localhost"},
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

func waitTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等不到 %s TCP", addr)
}

func visionEvidenceLine(actualUp, actualDown, reportUp, reportDown int64, online map[int]int, logText string) string {
	return fmt.Sprintf("vision+reality report 證據 actual_up=%d actual_down=%d report_up=%d report_down=%d online=%v\n%s",
		actualUp, actualDown, reportUp, reportDown, online, logText)
}

func writeVisionEvidence(t *testing.T, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(t.TempDir(), name), []byte(text+"\n"), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
}
