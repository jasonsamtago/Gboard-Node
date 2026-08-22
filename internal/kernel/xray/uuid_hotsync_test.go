package xray

import (
	"bytes"
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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	xrayCore "github.com/xtls/xray-core/core"
)

// Official cedar2025/Xboard-Node #53：面板改同一 email（user@2942）的
// UUID 後，xray 熱同步再 AddUser 被拒 already exists，節點仍記 added=1，
// 新 UUID 連不上、舊 UUID 還能連。必須走 UpdateUsers / UserManager，
// 不准重啟 node。AddUser 失敗絕對不能記 added=1。

const (
	uuidHotUserID    = 2942
	uuidHotOtherID   = 7
	uuidHotOldUUID   = "11111111-1111-1111-1111-111111111111"
	uuidHotNewUUID   = "3e4ce6e5-2d8d-4307-895f-c9a1a348cddd"
	uuidHotOtherUUID = "22222222-2222-2222-2222-222222222222"
	uuidHotSNI       = "uuid.hotsync.test"
	uuidHotPayload   = "PONG"
)

func TestXrayUpdateUsers_UUIDRotationAppliesNewAndDropsOld(t *testing.T) {
	x, port, dest, logs, stop := startXrayVLESS(t, uuidHotUsers(uuidHotOldUUID))
	defer stop()
	inst := xrayInstance(x)

	if err := tryVLESSUser(t, port, uuidHotOldUUID, dest); err != nil {
		t.Fatalf("前置：舊 UUID 應該能連: %v", err)
	}
	if err := tryVLESSUser(t, port, uuidHotOtherUUID, dest); err != nil {
		t.Fatalf("前置：沒改 UUID 的 user@7 應該能連: %v", err)
	}

	before := logs.Len()
	added, removed, err := x.UpdateUsers(uuidHotUsers(uuidHotNewUUID))
	if err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	logText := logs.Since(before)

	if xrayInstance(x) != inst || !x.IsRunning() {
		t.Fatal("換 UUID 不准重啟 xray instance／node")
	}
	if strings.Contains(logText, "performing full restart") || strings.Contains(logText, "fallback to restart") {
		t.Fatalf("必須走 UserManager 熱同步，不能重啟:\n%s", logText)
	}

	if err := tryVLESSUser(t, port, uuidHotNewUUID, dest); err != nil {
		t.Fatalf("面板換新 UUID 後熱同步必須讓新 UUID 能連: %v\nlog=\n%s", err, logText)
	}
	if err := tryVLESSUser(t, port, uuidHotOldUUID, dest); err == nil {
		t.Fatalf("舊 UUID 必須失效，卻還能連\nlog=\n%s", logText)
	}
	if err := tryVLESSUser(t, port, uuidHotOtherUUID, dest); err != nil {
		t.Fatalf("沒改 UUID 的 user@7 不能被踢: %v\nlog=\n%s", err, logText)
	}

	if strings.Contains(logText, "AddUser failed") && strings.Contains(logText, "added=1") && strings.Contains(logText, "removed=0") {
		t.Fatalf("AddUser 失敗不得記 added=1 / removed=0:\n%s", logText)
	}
	if added == 1 && removed == 0 && strings.Contains(logText, "already exists") {
		t.Fatalf("already exists 仍回 added=1 removed=0（官方 #53）:\n%s", logText)
	}
	if added < 1 || removed < 1 {
		t.Fatalf("換 UUID 應先移除再加: added=%d removed=%d log=\n%s", added, removed, logText)
	}

	t.Logf("熱同步證據 email=user@%d new_uuid=ok old_uuid=fail other=ok added=%d removed=%d\n%s", uuidHotUserID, added, removed, logText)
}

func TestXrayUpdateUsers_AddUserAlreadyExistsRemovesThenAdds(t *testing.T) {
	x, port, dest, logs, stop := startXrayVLESS(t, uuidHotUsers(uuidHotOldUUID))
	defer stop()
	inst := xrayInstance(x)

	// 模擬官方症狀：UserManager 仍掛著 user@2942，bookkeeping 卻當他是新人。
	x.mu.Lock()
	x.users = []model.UserSpec{{ID: uuidHotOtherID, UUID: uuidHotOtherUUID}}
	x.mu.Unlock()

	before := logs.Len()
	added, removed, err := x.UpdateUsers(uuidHotUsers(uuidHotNewUUID))
	if err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	logText := logs.Since(before)

	if xrayInstance(x) != inst || !x.IsRunning() {
		t.Fatal("already exists 必須先移除再加，不准重啟")
	}
	if err := tryVLESSUser(t, port, uuidHotNewUUID, dest); err != nil {
		t.Fatalf("AddUser already exists 要先移除再加，新 UUID 必須能連: %v\nlog=\n%s", err, logText)
	}
	if err := tryVLESSUser(t, port, uuidHotOldUUID, dest); err == nil {
		t.Fatalf("舊 UUID 必須失效\nlog=\n%s", logText)
	}
	if err := tryVLESSUser(t, port, uuidHotOtherUUID, dest); err != nil {
		t.Fatalf("沒改 UUID 的人不能被踢: %v", err)
	}
	if added == 1 && removed == 0 && strings.Contains(logText, "AddUser failed") {
		t.Fatalf("AddUser 失敗不得記 added=1:\n%s", logText)
	}
	if strings.Contains(logText, "AddUser failed") && strings.Contains(logText, "already exists") && !strings.Contains(logText, "removed") {
		t.Fatalf("already exists 沒有先移除再加:\n%s", logText)
	}
	t.Logf("already exists 證據 added=%d removed=%d\n%s", added, removed, logText)
}

func TestXrayUpdateUsers_AddUserFailureDoesNotCountAdded(t *testing.T) {
	x, _, _, logs, stop := startXrayVLESS(t, []model.UserSpec{{ID: uuidHotOtherID, UUID: uuidHotOtherUUID}})
	defer stop()

	before := logs.Len()
	added, removed, err := x.UpdateUsers([]model.UserSpec{
		{ID: uuidHotOtherID, UUID: uuidHotOtherUUID},
		{ID: 99, UUID: ""},
	})
	if err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	logText := logs.Since(before)

	if added != 0 {
		t.Fatalf("AddUser／建帳失敗絕對不能記 added=%d（官方 #53 記 added=1） log=\n%s", added, logText)
	}
	if removed != 0 {
		t.Fatalf("失敗路徑不該動到其他人 removed=%d log=\n%s", removed, logText)
	}
	if strings.Contains(logText, "users updated via UserManager added=1") {
		t.Fatalf("失敗仍打 added=1:\n%s", logText)
	}
	t.Logf("失敗不走 added=1 證據 added=%d removed=%d\n%s", added, removed, logText)
}

func TestXrayUpdateUsers_HotSyncLogEvidence(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	x, port, dest, logs, stop := startXrayVLESS(t, uuidHotUsers(uuidHotOldUUID))
	defer stop()
	inst := xrayInstance(x)

	oldOK := tryVLESSUser(t, port, uuidHotOldUUID, dest) == nil
	otherOK := tryVLESSUser(t, port, uuidHotOtherUUID, dest) == nil
	logf("before email=user@%d uuid=%s connect=%v path=UpdateUsers restart=false", uuidHotUserID, uuidHotOldUUID, oldOK)
	logf("before email=user@%d uuid=%s connect=%v", uuidHotOtherID, uuidHotOtherUUID, otherOK)

	before := logs.Len()
	added, removed, err := x.UpdateUsers(uuidHotUsers(uuidHotNewUUID))
	if err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	hotLog := logs.Since(before)
	restarted := xrayInstance(x) != inst || !x.IsRunning()
	logf("update path=UpdateUsers email=user@%d old=%s new=%s added=%d removed=%d restart=%v", uuidHotUserID, uuidHotOldUUID, uuidHotNewUUID, added, removed, restarted)
	for _, line := range strings.Split(strings.TrimSpace(hotLog), "\n") {
		if line != "" {
			logf("xray %s", line)
		}
	}

	newOK := tryVLESSUser(t, port, uuidHotNewUUID, dest) == nil
	oldStill := tryVLESSUser(t, port, uuidHotOldUUID, dest) == nil
	otherStill := tryVLESSUser(t, port, uuidHotOtherUUID, dest) == nil
	logf("after email=user@%d new_uuid connect=%v", uuidHotUserID, newOK)
	logf("after email=user@%d old_uuid connect=%v (must fail)", uuidHotUserID, oldStill)
	logf("after email=user@%d other_uuid connect=%v (must stay)", uuidHotOtherID, otherStill)

	logText := strings.Join(lines, "\n")
	t.Log("\n" + logText)
	if dir := os.Getenv("XRAY_UUID_HOTSYNC_LOG_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("hot-sync log dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "xray_uuid_hotsync.log"), []byte(logText+"\n"), 0o644); err != nil {
			t.Fatalf("hot-sync log: %v", err)
		}
	}

	if restarted {
		t.Fatal("熱同步日誌：不准重啟 node／kernel")
	}
	if !newOK {
		t.Fatalf("熱同步日誌：新 UUID 沒生效\n%s", logText)
	}
	if oldStill {
		t.Fatalf("熱同步日誌：舊 UUID 仍能連\n%s", logText)
	}
	if !otherStill {
		t.Fatalf("熱同步日誌：沒改 UUID 的人被踢\n%s", logText)
	}
	if strings.Contains(hotLog, "AddUser failed") && strings.Contains(hotLog, "added=1") && strings.Contains(hotLog, "removed=0") {
		t.Fatalf("熱同步日誌：失敗走了 added=1\n%s", logText)
	}
}

func uuidHotUsers(rotatedUUID string) []model.UserSpec {
	return []model.UserSpec{
		{ID: uuidHotUserID, UUID: rotatedUUID},
		{ID: uuidHotOtherID, UUID: uuidHotOtherUUID},
	}
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

func (w *lockedLogBuf) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Len()
}

func (w *lockedLogBuf) Since(n int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.b.String()
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

func startXrayVLESS(t *testing.T, users []model.UserSpec) (*Xray, int, *net.TCPAddr, *lockedLogBuf, func()) {
	t.Helper()

	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, destAddr := startUUIDHotDest(t)
	port := freeTCPPort(t)
	tlsCert := uuidHotTLSCert(t)
	nc := &model.NodeSpec{
		Protocol:     "vless",
		ListenIP:     "127.0.0.1",
		ServerPort:   port,
		Network:      "tcp",
		TLS:          1,
		ServerName:   uuidHotSNI,
		TLSSettings:  map[string]any{"server_name": uuidHotSNI},
		CustomRoutes: []map[string]any{{
			"type":        "field",
			"ip":          []string{"127.0.0.0/8"},
			"outboundTag": "direct",
		}},
	}

	x := New(config.KernelConfig{Type: "xray", LogLevel: "warning"})
	if err := x.Start(nc, users, tlsCert); err != nil {
		destLn.Close()
		t.Fatalf("啟動 xray: %v", err)
	}
	waitTLS(t, fmt.Sprintf("127.0.0.1:%d", port), uuidHotSNI)

	return x, port, destAddr, logs, func() {
		x.Stop()
		destLn.Close()
	}
}

func startUUIDHotDest(t *testing.T) (net.Listener, *net.TCPAddr) {
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
				_, _ = conn.Write([]byte(uuidHotPayload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}

func tryVLESSUser(t *testing.T, nodePort int, userUUID string, dest *net.TCPAddr) error {
	t.Helper()
	uid, err := parseVLESSUUID(userUUID)
	if err != nil {
		return err
	}

	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp",
		fmt.Sprintf("127.0.0.1:%d", nodePort),
		&tls.Config{InsecureSkipVerify: true, ServerName: uuidHotSNI},
	)
	if err != nil {
		return fmt.Errorf("tls: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	var hdr bytes.Buffer
	hdr.WriteByte(0)
	hdr.Write(uid[:])
	hdr.WriteByte(0)
	hdr.WriteByte(1)
	_ = binary.Write(&hdr, binary.BigEndian, uint16(dest.Port))
	hdr.WriteByte(1)
	ip4 := dest.IP.To4()
	if ip4 == nil {
		return fmt.Errorf("destination 不是 IPv4: %v", dest.IP)
	}
	hdr.Write(ip4)
	if _, err := conn.Write(hdr.Bytes()); err != nil {
		return fmt.Errorf("寫 VLESS 握手: %w", err)
	}

	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("讀 VLESS 回應（多半是 invalid request user id）: %w", err)
	}
	if resp[1] > 0 {
		if _, err := io.ReadFull(conn, make([]byte, int(resp[1]))); err != nil {
			return fmt.Errorf("讀 VLESS addon: %w", err)
		}
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		return fmt.Errorf("寫 payload: %w", err)
	}
	got := make([]byte, len(uuidHotPayload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("讀 payload: %w", err)
	}
	if string(got) != uuidHotPayload {
		return fmt.Errorf("payload=%q", got)
	}
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

func uuidHotTLSCert(t *testing.T) kernel.TLSCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("tls key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: uuidHotSNI},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{uuidHotSNI, "localhost"},
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

func waitTLS(t *testing.T, addr, sni string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c, err := tls.DialWithDialer(
			&net.Dialer{Timeout: 200 * time.Millisecond},
			"tcp",
			addr,
			&tls.Config{InsecureSkipVerify: true, ServerName: sni},
		)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等不到 %s TLS", addr)
}

func xrayInstance(x *Xray) *xrayCore.Instance {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.instance
}
