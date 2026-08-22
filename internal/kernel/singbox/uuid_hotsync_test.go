package singbox

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
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
	box "github.com/sagernet/sing-box"
)

// Official cedar2025/Xboard-Node #45／#67／#47：重置訂閱、換套餐、新用戶
// 熱同步後 singbox 仍打 unknown UUID，要重啟全部 node 才拿得到。
// 必須走 UpdateUsers／AddUsers／Reload users，不准重啟 kernel。
// #49／#54（Hy2／TUIC 下標）與 #53（xray already exists）不要重做。

const (
	sbHotResetID    = 45
	sbHotStableID   = 7
	sbHotPlanID     = 67
	sbHotNewID      = 88
	sbHotOldUUID    = "11111111-1111-1111-1111-111111111111"
	sbHotNewUUID    = "35b2af08-9507-4ebd-99fc-e4172286c88e"
	sbHotStableUUID = "22222222-2222-2222-2222-222222222222"
	sbHotPlanUUID   = "67676767-6767-6767-6767-676767676767"
	sbHotFreshUUID  = "88888888-8888-8888-8888-888888888888"
	sbHotPayload    = "PONG"
)

func TestSingBoxUpdateUsers_ResetPlanNewUserHotSync(t *testing.T) {
	s, port, dest, logs, stop := startSingBoxVLESS(t, sbHotInitialUsers())
	defer stop()
	inst := singBoxInstance(s)

	if err := trySingBoxVLESS(t, port, sbHotOldUUID, dest); err != nil {
		t.Fatalf("前置：重置前舊 UUID 應該能連: %v", err)
	}
	if err := trySingBoxVLESS(t, port, sbHotStableUUID, dest); err != nil {
		t.Fatalf("前置：沒改的人應該能連: %v", err)
	}

	before := logs.Len()
	added, removed, err := s.UpdateUsers(sbHotAfterUsers())
	if err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	logText := logs.Since(before)

	if singBoxInstance(s) != inst || !s.IsRunning() {
		t.Fatal("重置訂閱／換套餐／新用戶不准重啟 singbox instance／node")
	}
	if sbHotRestarted(logText) {
		t.Fatalf("必須走 UpdateUsers 熱同步，不能重啟:\n%s", logText)
	}

	if err := trySingBoxVLESS(t, port, sbHotNewUUID, dest); err != nil {
		t.Fatalf("重置訂閱後新 UUID 必須能連（官方 #45 unknown UUID）: %v\nlog=\n%s", err, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotOldUUID, dest); err == nil {
		t.Fatalf("舊 UUID 必須失效，卻還能連\nlog=\n%s", logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotPlanUUID, dest); err != nil {
		t.Fatalf("換套餐進這台節點的人熱同步後必須能連: %v\nlog=\n%s", err, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotFreshUUID, dest); err != nil {
		t.Fatalf("新用戶熱同步後必須能連: %v\nlog=\n%s", err, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotStableUUID, dest); err != nil {
		t.Fatalf("沒改的人不能被踢: %v\nlog=\n%s", err, logText)
	}
	if added < 3 || removed < 1 {
		t.Fatalf("重置＋換套餐＋新用戶應有 added>=3 removed>=1: added=%d removed=%d log=\n%s", added, removed, logText)
	}
	t.Logf("UpdateUsers 證據 added=%d removed=%d\n%s", added, removed, logText)
}

func TestSingBoxAddUsers_UUIDRotationAppliesWithoutRestart(t *testing.T) {
	// 官方／面板只推 sync.user.delta add（同 ID 新 UUID）時，
	// AddUsers 若用 ID 去重會直接略過，inbound 仍是舊 UUID → unknown UUID。
	s, port, dest, logs, stop := startSingBoxVLESS(t, sbHotInitialUsers())
	defer stop()
	inst := singBoxInstance(s)

	before := logs.Len()
	added, err := s.AddUsers([]model.UserSpec{{ID: sbHotResetID, UUID: sbHotNewUUID}})
	if err != nil {
		t.Fatalf("AddUsers: %v", err)
	}
	logText := logs.Since(before)

	if singBoxInstance(s) != inst || !s.IsRunning() {
		t.Fatal("AddUsers 換 UUID 不准重啟")
	}
	if added < 1 {
		t.Fatalf("同 ID 換 UUID 不得略過: added=%d log=\n%s", added, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotNewUUID, dest); err != nil {
		t.Fatalf("AddUsers 換 UUID 後新 UUID 必須能連: %v\nlog=\n%s", err, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotOldUUID, dest); err == nil {
		t.Fatalf("AddUsers 換 UUID 後舊 UUID 必須失效\nlog=\n%s", logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotStableUUID, dest); err != nil {
		t.Fatalf("沒改的人不能被踢: %v", err)
	}
}

func TestSingBoxRemoveThenAdd_PanelUUIDRotation(t *testing.T) {
	// Gboard 面板 #29／#40：UUID 變了先 sync.user.delta remove 舊 UUID，再 add 新的。
	s, port, dest, logs, stop := startSingBoxVLESS(t, sbHotInitialUsers())
	defer stop()
	inst := singBoxInstance(s)

	before := logs.Len()
	removed, err := s.RemoveUsers([]model.UserSpec{{ID: sbHotResetID, UUID: sbHotOldUUID}})
	if err != nil {
		t.Fatalf("RemoveUsers: %v", err)
	}
	added, err := s.AddUsers([]model.UserSpec{{ID: sbHotResetID, UUID: sbHotNewUUID}})
	if err != nil {
		t.Fatalf("AddUsers after remove: %v", err)
	}
	logText := logs.Since(before)

	if singBoxInstance(s) != inst || !s.IsRunning() {
		t.Fatal("面板 remove→add 不准重啟")
	}
	if removed < 1 || added < 1 {
		t.Fatalf("remove→add 必須真的刪再加: removed=%d added=%d log=\n%s", removed, added, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotNewUUID, dest); err != nil {
		t.Fatalf("remove→add 後新 UUID 必須能連: %v\nlog=\n%s", err, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotOldUUID, dest); err == nil {
		t.Fatalf("remove→add 後舊 UUID 必須失效\nlog=\n%s", logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotStableUUID, dest); err != nil {
		t.Fatalf("沒改的人不能被踢: %v", err)
	}
}

func TestSingBoxAddUsers_BookkeepingAheadStillAppliesNewUUID(t *testing.T) {
	// lastUsers／s.users 已寫新 UUID，inbound 還掛舊的：AddUsers 不得因 ID 重複就略過。
	s, port, dest, logs, stop := startSingBoxVLESS(t, sbHotInitialUsers())
	defer stop()
	inst := singBoxInstance(s)

	s.mu.Lock()
	s.users = []model.UserSpec{
		{ID: sbHotResetID, UUID: sbHotNewUUID},
		{ID: sbHotStableID, UUID: sbHotStableUUID},
	}
	s.mu.Unlock()

	before := logs.Len()
	added, err := s.AddUsers([]model.UserSpec{{ID: sbHotResetID, UUID: sbHotNewUUID}})
	if err != nil {
		t.Fatalf("AddUsers: %v", err)
	}
	logText := logs.Since(before)

	if singBoxInstance(s) != inst || !s.IsRunning() {
		t.Fatal("帳本超前時換 UUID 不准重啟")
	}
	if added < 1 && trySingBoxVLESS(t, port, sbHotNewUUID, dest) != nil {
		t.Fatalf("帳本已是新 UUID 時仍要把 inbound 熱換上去: added=%d log=\n%s", added, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotNewUUID, dest); err != nil {
		t.Fatalf("帳本超前：新 UUID 必須能連: %v\nlog=\n%s", err, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotOldUUID, dest); err == nil {
		t.Fatalf("帳本超前：舊 UUID 必須失效\nlog=\n%s", logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotStableUUID, dest); err != nil {
		t.Fatalf("沒改的人不能被踢: %v", err)
	}
}

func TestSingBoxAddUsers_NewUserAndPlanChange(t *testing.T) {
	s, port, dest, logs, stop := startSingBoxVLESS(t, sbHotInitialUsers())
	defer stop()
	inst := singBoxInstance(s)

	before := logs.Len()
	added, err := s.AddUsers([]model.UserSpec{
		{ID: sbHotPlanID, UUID: sbHotPlanUUID},
		{ID: sbHotNewID, UUID: sbHotFreshUUID},
	})
	if err != nil {
		t.Fatalf("AddUsers: %v", err)
	}
	logText := logs.Since(before)

	if singBoxInstance(s) != inst || !s.IsRunning() {
		t.Fatal("換套餐／新用戶不准重啟")
	}
	if added < 2 {
		t.Fatalf("換套餐＋新用戶必須加上: added=%d log=\n%s", added, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotPlanUUID, dest); err != nil {
		t.Fatalf("換套餐進節點必須能連: %v\nlog=\n%s", err, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotFreshUUID, dest); err != nil {
		t.Fatalf("新用戶必須能連: %v\nlog=\n%s", err, logText)
	}
	if err := trySingBoxVLESS(t, port, sbHotOldUUID, dest); err != nil {
		t.Fatalf("沒改的重置用戶不能被踢: %v", err)
	}
	if err := trySingBoxVLESS(t, port, sbHotStableUUID, dest); err != nil {
		t.Fatalf("沒改的人不能被踢: %v", err)
	}
}

func TestSingBoxUpdateUsers_UnchangedUserNotKicked(t *testing.T) {
	s, port, dest, _, stop := startSingBoxVLESS(t, sbHotInitialUsers())
	defer stop()
	inst := singBoxInstance(s)

	added, removed, err := s.UpdateUsers(sbHotInitialUsers())
	if err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	if singBoxInstance(s) != inst || !s.IsRunning() {
		t.Fatal("沒改的人重跑 UpdateUsers 不准重啟")
	}
	if added != 0 || removed != 0 {
		t.Fatalf("沒改不該有 diff: added=%d removed=%d", added, removed)
	}
	if err := trySingBoxVLESS(t, port, sbHotOldUUID, dest); err != nil {
		t.Fatalf("沒改的人不能被踢: %v", err)
	}
	if err := trySingBoxVLESS(t, port, sbHotStableUUID, dest); err != nil {
		t.Fatalf("沒改的人不能被踢: %v", err)
	}
}

func TestSingBoxHotSyncLogEvidence(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	s, port, dest, logs, stop := startSingBoxVLESS(t, sbHotInitialUsers())
	defer stop()
	inst := singBoxInstance(s)

	oldOK := trySingBoxVLESS(t, port, sbHotOldUUID, dest) == nil
	stableOK := trySingBoxVLESS(t, port, sbHotStableUUID, dest) == nil
	logf("before email=user@%d uuid=%s connect=%v path=UpdateUsers restart=false", sbHotResetID, sbHotOldUUID, oldOK)
	logf("before email=user@%d uuid=%s connect=%v", sbHotStableID, sbHotStableUUID, stableOK)

	before := logs.Len()
	added, removed, err := s.UpdateUsers(sbHotAfterUsers())
	if err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	hotLog := logs.Since(before)
	restarted := singBoxInstance(s) != inst || !s.IsRunning() || sbHotRestarted(hotLog)
	logf("update path=UpdateUsers reset=%d plan=%d new=%d added=%d removed=%d restart=%v", sbHotResetID, sbHotPlanID, sbHotNewID, added, removed, restarted)
	for _, line := range strings.Split(strings.TrimSpace(hotLog), "\n") {
		if line != "" {
			logf("singbox %s", line)
		}
	}

	newOK := trySingBoxVLESS(t, port, sbHotNewUUID, dest) == nil
	oldStill := trySingBoxVLESS(t, port, sbHotOldUUID, dest) == nil
	planOK := trySingBoxVLESS(t, port, sbHotPlanUUID, dest) == nil
	freshOK := trySingBoxVLESS(t, port, sbHotFreshUUID, dest) == nil
	stableStill := trySingBoxVLESS(t, port, sbHotStableUUID, dest) == nil
	logf("after email=user@%d new_uuid connect=%v", sbHotResetID, newOK)
	logf("after email=user@%d old_uuid connect=%v (must fail)", sbHotResetID, oldStill)
	logf("after email=user@%d plan_uuid connect=%v", sbHotPlanID, planOK)
	logf("after email=user@%d new_user connect=%v", sbHotNewID, freshOK)
	logf("after email=user@%d other_uuid connect=%v (must stay)", sbHotStableID, stableStill)

	logText := strings.Join(lines, "\n")
	t.Log("\n" + logText)
	if dir := os.Getenv("SINGBOX_HOTSYNC_LOG_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("hot-sync log dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "singbox_uuid_hotsync.log"), []byte(logText+"\n"), 0o644); err != nil {
			t.Fatalf("hot-sync log: %v", err)
		}
	}

	if restarted {
		t.Fatal("熱同步日誌：不准重啟 node／kernel")
	}
	if !newOK {
		t.Fatalf("熱同步日誌：重置訂閱新 UUID 沒生效\n%s", logText)
	}
	if oldStill {
		t.Fatalf("熱同步日誌：舊 UUID 仍能連\n%s", logText)
	}
	if !planOK {
		t.Fatalf("熱同步日誌：換套餐沒生效\n%s", logText)
	}
	if !freshOK {
		t.Fatalf("熱同步日誌：新用戶沒生效\n%s", logText)
	}
	if !stableStill {
		t.Fatalf("熱同步日誌：沒改的人被踢\n%s", logText)
	}
}

func sbHotInitialUsers() []model.UserSpec {
	return []model.UserSpec{
		{ID: sbHotResetID, UUID: sbHotOldUUID},
		{ID: sbHotStableID, UUID: sbHotStableUUID},
	}
}

func sbHotAfterUsers() []model.UserSpec {
	return []model.UserSpec{
		{ID: sbHotResetID, UUID: sbHotNewUUID},
		{ID: sbHotStableID, UUID: sbHotStableUUID},
		{ID: sbHotPlanID, UUID: sbHotPlanUUID},
		{ID: sbHotNewID, UUID: sbHotFreshUUID},
	}
}

func sbHotRestarted(logText string) bool {
	return strings.Contains(logText, "performing full restart") ||
		strings.Contains(logText, "fallback to restart") ||
		strings.Contains(logText, "restarting kernel") ||
		strings.Contains(logText, "old instance recycled")
}

type sbLockedLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *sbLockedLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *sbLockedLogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (w *sbLockedLogBuf) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Len()
}

func (w *sbLockedLogBuf) Since(n int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.b.String()
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

func startSingBoxVLESS(t *testing.T, users []model.UserSpec) (*SingBox, int, *net.TCPAddr, *sbLockedLogBuf, func()) {
	t.Helper()

	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, destAddr := startSingBoxHotDest(t)
	port := sbFreeTCPPort(t)
	nc := &model.NodeSpec{
		Protocol:   "vless",
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Network:    "tcp",
		// 預設會把 127.0.0.0/8 丟進 block；測試目的地在本機，必須先走 direct。
		CustomRoutes: []map[string]any{{
			"ip_cidr":  []string{"127.0.0.0/8"},
			"outbound": "direct",
		}},
	}

	s := New(config.KernelConfig{Type: "singbox", LogLevel: "debug"})
	if err := s.Start(nc, users, kernel.TLSCert{}); err != nil {
		destLn.Close()
		t.Fatalf("啟動 singbox: %v", err)
	}
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))

	return s, port, destAddr, logs, func() {
		s.Stop()
		destLn.Close()
	}
}

func startSingBoxHotDest(t *testing.T) (net.Listener, *net.TCPAddr) {
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
				_, _ = conn.Write([]byte(sbHotPayload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}

func trySingBoxVLESS(t *testing.T, nodePort int, userUUID string, dest *net.TCPAddr) error {
	t.Helper()
	uid, err := parseSingBoxUUID(userUUID)
	if err != nil {
		return err
	}

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", nodePort), 2*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
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
		return fmt.Errorf("讀 VLESS 回應（多半是 unknown UUID）: %w", err)
	}
	if resp[1] > 0 {
		if _, err := io.ReadFull(conn, make([]byte, int(resp[1]))); err != nil {
			return fmt.Errorf("讀 VLESS addon: %w", err)
		}
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		return fmt.Errorf("寫 payload: %w", err)
	}
	got := make([]byte, len(sbHotPayload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("讀 payload: %w", err)
	}
	if string(got) != sbHotPayload {
		return fmt.Errorf("payload=%q", got)
	}
	return nil
}

func parseSingBoxUUID(s string) ([16]byte, error) {
	var out [16]byte
	raw, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
	if err != nil || len(raw) != 16 {
		return out, fmt.Errorf("invalid uuid %q", s)
	}
	copy(out[:], raw)
	return out, nil
}

func sbFreeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func sbWaitTCP(t *testing.T, addr string) {
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

func singBoxInstance(s *SingBox) *box.Box {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.box
}
