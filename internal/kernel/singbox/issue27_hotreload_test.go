package singbox

import (
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// Official cedar2025/Xboard-Node #27：面板對同一 node_id 直接改協議
// （vmess→vless）／埠後，熱更新必須能起核並真能連。複製新 node_id 才通
// 不准當修。協議／埠變更時舊 listener 必須放掉，不得長期
// address already in use。

const (
	issue27UserID   = 27
	issue27UserUUID = "27272727-2727-2727-2727-272727272727"
)

func TestSingBoxReload_SameNodeIDProtocolChangeRebindsAndConnects(t *testing.T) {
	s, port, dest, logs, stop := startSingBoxIssue27(t, "vmess", 0)
	defer stop()

	before := logs.Len()
	vless := issue27NodeSpec("vless", port)
	if err := s.Reload(vless, issue27Users(), kernel.TLSCert{}); err != nil {
		t.Fatalf("同 node_id vmess→vless Reload 必須成功（官方 #27，不准新建 node_id）: %v\nlog=\n%s", err, logs.Since(before))
	}
	logText := logs.Since(before)
	if !s.IsRunning() {
		t.Fatal("協議變更後 kernel 必須仍在跑")
	}
	if strings.Contains(logText, "address already in use") {
		t.Fatalf("協議變更不得卡住舊 listener:\n%s", logText)
	}

	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))
	if err := trySingBoxVLESS(t, port, issue27UserUUID, dest); err != nil {
		t.Fatalf("同 node_id 改成 vless 後必須真能連: %v\nlog=\n%s", err, logText)
	}
}

func TestSingBoxReload_SameNodeIDPortChangeReleasesOldListener(t *testing.T) {
	s, oldPort, dest, logs, stop := startSingBoxIssue27(t, "vless", 0)
	defer stop()

	if err := trySingBoxVLESS(t, oldPort, issue27UserUUID, dest); err != nil {
		t.Fatalf("前置：舊埠 vless 應該能連: %v", err)
	}

	newPort := sbFreeTCPPort(t)
	before := logs.Len()
	next := issue27NodeSpec("vless", newPort)
	if err := s.Reload(next, issue27Users(), kernel.TLSCert{}); err != nil {
		t.Fatalf("同 node_id 改埠 Reload 必須成功: %v\nlog=\n%s", err, logs.Since(before))
	}
	logText := logs.Since(before)
	if strings.Contains(logText, "address already in use") {
		t.Fatalf("改埠不得 address already in use:\n%s", logText)
	}

	if err := issue27MustBind("127.0.0.1", oldPort); err != nil {
		t.Fatalf("舊埠 %d 必須放掉，卻仍被占（官方 #27 address already in use）: %v\nlog=\n%s", oldPort, err, logText)
	}
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", newPort))
	if err := trySingBoxVLESS(t, newPort, issue27UserUUID, dest); err != nil {
		t.Fatalf("新埠 %d 必須真能連: %v\nlog=\n%s", newPort, err, logText)
	}
}

func TestSingBoxReload_SameNodeIDProtocolAndPortChangeDropsOldInbound(t *testing.T) {
	// 協議 tag 從 vmess-in 換成 vless-in 時，若只 Remove 新 tag，
	// 舊 inbound 會繼續占舊埠；官方現場就是再 bind 8443 報
	// address already in use。
	s, oldPort, dest, logs, stop := startSingBoxIssue27(t, "vmess", 0)
	defer stop()

	newPort := sbFreeTCPPort(t)
	before := logs.Len()
	next := issue27NodeSpec("vless", newPort)
	if err := s.Reload(next, issue27Users(), kernel.TLSCert{}); err != nil {
		t.Fatalf("同 node_id vmess:舊埠→vless:新埠 Reload 必須成功: %v\nlog=\n%s", err, logs.Since(before))
	}
	logText := logs.Since(before)

	if err := issue27MustBind("127.0.0.1", oldPort); err != nil {
		t.Fatalf("協議＋埠變更後舊埠 %d 必須放掉: %v\nlog=\n%s", oldPort, err, logText)
	}
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", newPort))
	if err := trySingBoxVLESS(t, newPort, issue27UserUUID, dest); err != nil {
		t.Fatalf("新協議新埠必須真能連: %v\nlog=\n%s", err, logText)
	}
}

func startSingBoxIssue27(t *testing.T, protocol string, port int) (*SingBox, int, *net.TCPAddr, *sbLockedLogBuf, func()) {
	t.Helper()

	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, destAddr := startSingBoxHotDest(t)
	if port == 0 {
		port = sbFreeTCPPort(t)
	}
	nc := issue27NodeSpec(protocol, port)
	s := New(config.KernelConfig{Type: "singbox", LogLevel: "debug"})
	if err := s.Start(nc, issue27Users(), kernel.TLSCert{}); err != nil {
		destLn.Close()
		t.Fatalf("啟動 singbox %s: %v", protocol, err)
	}
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))

	return s, port, destAddr, logs, func() {
		s.Stop()
		destLn.Close()
	}
}

func issue27NodeSpec(protocol string, port int) *model.NodeSpec {
	return &model.NodeSpec{
		Protocol:   protocol,
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Network:    "tcp",
		CustomRoutes: []map[string]any{{
			"ip_cidr":  []string{"127.0.0.0/8"},
			"outbound": "direct",
		}},
	}
}

func issue27Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue27UserID, UUID: issue27UserUUID}}
}

func issue27MustBind(host string, port int) error {
	deadline := time.Now().Add(2 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, port))
		if err == nil {
			_ = ln.Close()
			return nil
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	return last
}
