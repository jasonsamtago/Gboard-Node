package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/cert"
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

// 官方 cedar2025/Xboard-Node #57：面板把用戶流量改成 0G／再改回 1G，
// 必須熱生效踢線／恢復，不准重啟容器才觸發。
//
// 面板 getAvailableUsers 用 `u + d < transfer_enable`：設 0G 或 transfer
// 用盡就把該用戶從 users 抽掉；設回 1G（u+d < 額度）再加回來。
// 用戶仍在線時必須熱更新後拒絕或踢掉既有連線；再改回有額度要不重啟也能再連。
//
// 與 #56（Hy2 限速／限流計數、CloseByUUID 空實作）分開。本票鎖
// 「面板改流量額度後必須熱踢／熱恢復」，不准只修 Hy2 或只修 VMess。
//
// 不准當修：關掉熱更新、改成必須重啟容器、把「起核失敗」當限流成功。
// 本檔只加失敗測試，不改 production。

const (
	quotaHotUserID    = 57
	quotaHotOtherID   = 58
	quotaHotUUID      = "57575757-5757-4757-8757-575757575757"
	quotaHotOtherUUID = "58585858-5858-4858-8858-585858585858"
	quotaHotPayload   = "QUOTA57"
)

func TestApplyUserUpdate_QuotaRestoreAfterKernelStopped(t *testing.T) {
	// 0G 抽光用戶後核常被 Stop（applyChanges 空名單／xray 最後一人 RemoveUsers）。
	// 再改回 1G 必須熱起核並套用 users，不准 ensureRunning 看到空 lastUsers 就 return。
	k := &fakeKernel{running: false}
	s := newQuotaService(k)
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: 1}
	s.lastUsers = nil
	s.lastUserHash = computeUserHash(nil)

	restored := quotaHotUsers()
	s.applyUserUpdate(context.Background(), restored, computeUserHash(restored))

	if k.startCalls == 0 || !k.IsRunning() {
		t.Fatal("額度改回 1G 必須熱起核，不准等手動重啟容器（ensureRunning 在 lastUsers 仍空時直接 return）")
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != quotaHotUUID {
		t.Fatalf("1G 恢復後 lastUsers 必須有人: %#v", s.lastUsers)
	}
	if k.updateCalls == 0 && k.startCalls == 0 {
		t.Fatal("1G 恢復必須走 UpdateUsers 或 Start，不准靜默丟掉面板 users")
	}
}

func TestApplyUserDelta_QuotaZeroThenRestoreAfterLastUserStop(t *testing.T) {
	k := &quotaStopOnEmptyKernel{fakeKernel: fakeKernel{running: true}}
	s := newQuotaService(&k.fakeKernel)
	s.kernel = k
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: 1}
	s.updateUserState(quotaHotUsers())

	s.applyUserDelta(context.Background(), "remove", quotaHotUsers())
	if len(s.lastUsers) != 0 {
		t.Fatalf("0G delta remove 後 lastUsers 必須空: %#v", s.lastUsers)
	}

	s.applyUserDelta(context.Background(), "add", quotaHotUsers())
	if !k.IsRunning() {
		t.Fatal("1G delta add 必須熱恢復，不准核停著等重啟容器")
	}
	if k.addCalls == 0 && k.updateCalls == 0 && k.startCalls == 0 {
		t.Fatal("1G delta add 必須真的加回用戶（官方 #57 設回 1G 也要重啟才恢復）")
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != quotaHotUUID {
		t.Fatalf("1G 恢復後 lastUsers 必須有人: %#v", s.lastUsers)
	}
}

func TestApplyPullResult_QuotaZeroWithConfigThenRestore(t *testing.T) {
	k := &fakeKernel{running: true}
	s := newQuotaService(k)
	spec := &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: 24022}
	s.lastConfig = spec
	s.lastConfigHash = computeConfigHash(spec)
	s.updateUserState(quotaHotUsers())

	changed := *spec
	changed.Host = "quota-0g.example"
	s.applyPullResult(context.Background(), pullResult{
		config:     &changed,
		configHash: computeConfigHash(&changed),
		users:      []model.UserSpec{},
		userHash:   computeUserHash([]model.UserSpec{}),
	})
	if len(s.lastUsers) != 0 {
		t.Fatalf("面板 0G 抽掉用戶後 lastUsers 必須空: %#v", s.lastUsers)
	}

	s.applyPullResult(context.Background(), pullResult{
		users:    quotaHotUsers(),
		userHash: computeUserHash(quotaHotUsers()),
	})
	if !k.IsRunning() {
		t.Fatal("REST 拉回 1G users 必須熱恢復，不准等重啟容器")
	}
	if len(s.lastUsers) != 1 || s.lastUsers[0].UUID != quotaHotUUID {
		t.Fatalf("1G 恢復後 lastUsers 必須有人: %#v", s.lastUsers)
	}
	if k.startCalls == 0 && k.updateCalls == 0 {
		t.Fatal("1G 恢復必須走 Start／UpdateUsers")
	}
}

func TestHandleWSEvent_QuotaZeroThenRestore(t *testing.T) {
	k := &quotaStopOnEmptyKernel{fakeKernel: fakeKernel{running: true}}
	s := newQuotaService(&k.fakeKernel)
	s.kernel = k
	s.lastConfig = &model.NodeSpec{Protocol: "vless", ListenIP: "127.0.0.1", ServerPort: 1}
	s.updateUserState(quotaHotUsers())
	s.nodeLog = nlog.ForNode("vless", 1)

	s.handleWSEvent(context.Background(), controlplane.Event{
		Type:  controlplane.EventSyncUsers,
		Users: []model.UserSpec{},
	})
	if len(s.lastUsers) != 0 {
		t.Fatalf("WS sync.users 空名單（0G）後 lastUsers 必須空: %#v", s.lastUsers)
	}

	s.handleWSEvent(context.Background(), controlplane.Event{
		Type:  controlplane.EventSyncUsers,
		Users: quotaHotUsers(),
	})
	if !k.IsRunning() || len(s.lastUsers) != 1 {
		t.Fatalf("WS 再推 1G users 必須熱恢復 running=%v lastUsers=%#v", k.IsRunning(), s.lastUsers)
	}
}

func TestApplyUserUpdate_QuotaZeroThenRestore_SingBoxVLESS(t *testing.T) {
	assertQuotaHotCycle(t, "singbox", "vless")
}

func TestApplyUserUpdate_QuotaZeroThenRestore_XrayVLESS(t *testing.T) {
	assertQuotaHotCycle(t, "xray", "vless")
}

func TestApplyUserUpdate_QuotaZeroThenRestore_SingBoxVMess(t *testing.T) {
	assertQuotaHotCycle(t, "singbox", "vmess")
}

func TestApplyUserUpdate_QuotaZeroThenRestore_XrayVMess(t *testing.T) {
	assertQuotaHotCycle(t, "xray", "vmess")
}

func TestApplyUserUpdate_QuotaZeroDoesNotAffectSiblingNode(t *testing.T) {
	// 同機多節點／多協議：踢 A 不准連帶把 B 弄掛。不准只修單一協議。
	a, aPort, aDest, _, aStop := startQuotaService(t, "singbox", "vless", quotaHotUUID)
	defer aStop()
	_, bPort, bDest, _, bStop := startQuotaService(t, "singbox", "vmess", quotaHotOtherUUID)
	defer bStop()

	if err := quotaTryConnect(t, "vless", aPort, quotaHotUUID, aDest); err != nil {
		t.Fatalf("前置 A VLESS 必須能連: %v", err)
	}
	if err := quotaTryConnect(t, "vmess", bPort, quotaHotOtherUUID, bDest); err != nil {
		t.Fatalf("前置 B VMess 必須能連: %v", err)
	}

	a.applyUserUpdate(context.Background(), []model.UserSpec{}, computeUserHash([]model.UserSpec{}))
	if err := quotaTryConnect(t, "vless", aPort, quotaHotUUID, aDest); err == nil {
		t.Fatal("A 設 0G 後必須拒絕既有 UUID，卻還能連")
	}
	if err := quotaTryConnect(t, "vmess", bPort, quotaHotOtherUUID, bDest); err != nil {
		t.Fatalf("同機另一協議節點不能被 A 的 0G 熱踢誤殺: %v", err)
	}

	a.applyUserUpdate(context.Background(), []model.UserSpec{{ID: quotaHotUserID, UUID: quotaHotUUID}}, computeUserHash([]model.UserSpec{{ID: quotaHotUserID, UUID: quotaHotUUID}}))
	if err := quotaTryConnect(t, "vless", aPort, quotaHotUUID, aDest); err != nil {
		t.Fatalf("A 改回 1G 必須能再連: %v", err)
	}
	if err := quotaTryConnect(t, "vmess", bPort, quotaHotOtherUUID, bDest); err != nil {
		t.Fatalf("B 在 A 恢復後仍要通: %v", err)
	}
}

func assertQuotaHotCycle(t *testing.T, kernelType, protocol string) {
	t.Helper()
	svc, port, dest, logs, stop := startQuotaService(t, kernelType, protocol, quotaHotUUID)
	defer stop()

	if err := quotaTryConnect(t, protocol, port, quotaHotUUID, dest); err != nil {
		t.Fatalf("前置 %s/%s 必須能連: %v", kernelType, protocol, err)
	}

	var live net.Conn
	if protocol != "vmess" {
		var liveErr error
		live, liveErr = quotaOpenSession(t, protocol, port, quotaHotUUID, dest)
		if liveErr != nil {
			t.Fatalf("前置：用戶仍在線時必須先握上既有連線: %v", liveErr)
		}
		defer live.Close()
	}

	before := logs.Len()
	empty := []model.UserSpec{}
	svc.applyUserUpdate(context.Background(), empty, computeUserHash(empty))
	logText := quotaLogSince(logs, before)

	if strings.Contains(logText, "failed to start kernel") && !svc.kernel.IsRunning() {
		t.Fatalf("不准把起核失敗當 0G 限流成功:\n%s", logText)
	}
	if len(svc.lastUsers) != 0 {
		t.Fatalf("0G 後 lastUsers 必須空（面板已抽掉用戶），got %#v\nlog=\n%s", svc.lastUsers, logText)
	}
	if live != nil && quotaSessionAlive(live) {
		t.Fatalf("%s/%s 用戶仍在線時設 0G，既有連線必須被踢／拒絕，卻還活著\nlog=\n%s", kernelType, protocol, logText)
	}
	if err := quotaTryConnect(t, protocol, port, quotaHotUUID, dest); err == nil {
		t.Fatalf("%s/%s 0G 後新連線必須拒絕，卻還能連\nlog=\n%s", kernelType, protocol, logText)
	}

	before = logs.Len()
	svc.applyUserUpdate(context.Background(), quotaHotUsers(), computeUserHash(quotaHotUsers()))
	logText += quotaLogSince(logs, before)

	if !svc.kernel.IsRunning() {
		t.Fatalf("1G 恢復後核必須在跑，不准等重啟容器:\n%s", logText)
	}
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		last = quotaTryConnect(t, protocol, port, quotaHotUUID, dest)
		if last == nil {
			t.Logf("%s/%s 0G 熱踢＋1G 熱恢復證據 lastUsers=%d running=%v\n%s",
				kernelType, protocol, len(svc.lastUsers), svc.kernel.IsRunning(), logText)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s/%s 改回 1G 必須不重啟也能再連: %v\nlog=\n%s", kernelType, protocol, last, logText)
}

type quotaStopOnEmptyKernel struct {
	fakeKernel
}

func (k *quotaStopOnEmptyKernel) UpdateUsers(users []model.UserSpec) (int, int, error) {
	added, removed, err := k.fakeKernel.UpdateUsers(users)
	if err == nil && len(users) == 0 {
		k.Stop()
	}
	return added, removed, err
}

func (k *quotaStopOnEmptyKernel) RemoveUsers(users []model.UserSpec) (int, error) {
	n, err := k.fakeKernel.RemoveUsers(users)
	if err == nil {
		// 模擬 xray RemoveUsers：抽掉最後一人就 Stop。
		k.Stop()
	}
	return n, err
}

func (k *quotaStopOnEmptyKernel) AddUsers(users []model.UserSpec) (int, error) {
	return k.fakeKernel.AddUsers(users)
}

func newQuotaService(k *fakeKernel) *Service {
	shared := limiter.New()
	s := &Service{
		cfg:          &config.Config{Kernel: config.KernelConfig{Type: "singbox"}},
		kernel:       k,
		limiter:      shared,
		speedTracker: limiter.NewSpeedTracker(shared),
		cert:         cert.NewManager(config.CertConfig{}),
	}
	k.SetSpeedLimitFunc(s.speedTracker.GetLimiter)
	k.SetDeviceLimitFunc(s.limiter.GetDeviceLimitByUUID)
	return s
}

func quotaHotUsers() []model.UserSpec {
	return []model.UserSpec{{ID: quotaHotUserID, UUID: quotaHotUUID}}
}

type quotaLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *quotaLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *quotaLogBuf) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Len()
}

func (w *quotaLogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func quotaLogSince(w *quotaLogBuf, n int) string {
	s := w.String()
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

func startQuotaService(t *testing.T, kernelType, protocol, userUUID string) (*Service, int, *net.TCPAddr, *quotaLogBuf, func()) {
	t.Helper()

	logs := &quotaLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, destAddr := startQuotaDest(t)
	port := quotaFreePort(t)
	users := []model.UserSpec{{ID: quotaHotUserID, UUID: userUUID}}
	nc := quotaNodeSpec(kernelType, protocol, port)

	var k kernel.Kernel
	switch kernelType {
	case "xray":
		k = xray.New(config.KernelConfig{Type: "xray", LogLevel: "warning"})
	default:
		k = singbox.New(config.KernelConfig{Type: "singbox", LogLevel: "debug"})
	}
	if err := k.Start(nc, users, kernel.TLSCert{}); err != nil {
		destLn.Close()
		t.Fatalf("啟動 %s/%s: %v", kernelType, protocol, err)
	}
	quotaWaitTCP(t, port)

	shared := limiter.New()
	svc := &Service{
		cfg:          &config.Config{Kernel: config.KernelConfig{Type: kernelType}},
		kernel:       k,
		tracker:      tracker.New(),
		limiter:      shared,
		speedTracker: limiter.NewSpeedTracker(shared),
		cert:         cert.NewManager(config.CertConfig{}),
		lastConfig:   nc,
		lastUsers:    users,
		lastUserHash: computeUserHash(users),
		nodeLog:      nlog.ForNode(protocol, port),
	}
	svc.lastConfigHash = computeConfigHash(nc)
	svc.appliedState.Config = nc
	svc.appliedState.Users = users
	k.SetSpeedLimitFunc(svc.speedTracker.GetLimiter)
	k.SetDeviceLimitFunc(svc.limiter.GetDeviceLimitByUUID)

	return svc, port, destAddr, logs, func() {
		k.Stop()
		destLn.Close()
	}
}

func quotaNodeSpec(kernelType, protocol string, port int) *model.NodeSpec {
	nc := &model.NodeSpec{
		Protocol:   protocol,
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Network:    "tcp",
	}
	if kernelType == "xray" {
		nc.CustomRoutes = []map[string]any{{
			"type":        "field",
			"ip":          []string{"127.0.0.0/8"},
			"outboundTag": "direct",
		}}
		return nc
	}
	nc.CustomRoutes = []map[string]any{{
		"ip_cidr":  []string{"127.0.0.0/8"},
		"outbound": "direct",
	}}
	return nc
}

func startQuotaDest(t *testing.T) (net.Listener, *net.TCPAddr) {
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
				_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
				_, _ = conn.Write([]byte(quotaHotPayload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}

func quotaTryConnect(t *testing.T, protocol string, nodePort int, userUUID string, dest *net.TCPAddr) error {
	t.Helper()
	switch protocol {
	case "vmess":
		return quotaTryVMess(t, nodePort, userUUID, dest)
	default:
		return quotaTryVLESS(nodePort, userUUID, dest)
	}
}

func quotaOpenSession(t *testing.T, protocol string, nodePort int, userUUID string, dest *net.TCPAddr) (net.Conn, error) {
	t.Helper()
	if protocol == "vmess" {
		return quotaOpenVMess(t, nodePort, userUUID, dest)
	}
	return quotaOpenVLESS(nodePort, userUUID, dest)
}

func quotaTryVLESS(nodePort int, userUUID string, dest *net.TCPAddr) error {
	conn, err := quotaOpenVLESS(nodePort, userUUID, dest)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func quotaOpenVLESS(nodePort int, userUUID string, dest *net.TCPAddr) (net.Conn, error) {
	raw, err := hex.DecodeString(strings.ReplaceAll(userUUID, "-", ""))
	if err != nil || len(raw) != 16 {
		return nil, fmt.Errorf("uuid")
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", nodePort), 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))

	var hdr bytes.Buffer
	hdr.WriteByte(0)
	hdr.Write(raw)
	hdr.WriteByte(0)
	hdr.WriteByte(1)
	_ = binary.Write(&hdr, binary.BigEndian, uint16(dest.Port))
	hdr.WriteByte(1)
	ip4 := dest.IP.To4()
	if ip4 == nil {
		conn.Close()
		return nil, fmt.Errorf("dest not ipv4")
	}
	hdr.Write(ip4)
	if _, err := conn.Write(hdr.Bytes()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("handshake: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("read vless: %w", err)
	}
	if resp[1] > 0 {
		if _, err := io.ReadFull(conn, make([]byte, int(resp[1]))); err != nil {
			conn.Close()
			return nil, err
		}
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		conn.Close()
		return nil, err
	}
	got := make([]byte, len(quotaHotPayload))
	if _, err := io.ReadFull(conn, got); err != nil {
		conn.Close()
		return nil, fmt.Errorf("payload: %w", err)
	}
	if string(got) != quotaHotPayload {
		conn.Close()
		return nil, fmt.Errorf("payload=%q", got)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func quotaTryVMess(t *testing.T, nodePort int, userUUID string, dest *net.TCPAddr) error {
	t.Helper()
	clientPort := quotaFreePort(t)
	stop := startXrayTunnelClient(t, quotaVMessClientConfig(clientPort, nodePort, dest.Port, userUUID))
	defer stop()
	_, _, err := quotaVMessDownload(clientPort)
	return err
}

func quotaOpenVMess(t *testing.T, nodePort int, userUUID string, dest *net.TCPAddr) (net.Conn, error) {
	t.Helper()
	clientPort := quotaFreePort(t)
	stop := startXrayTunnelClient(t, quotaVMessClientConfig(clientPort, nodePort, dest.Port, userUUID))
	t.Cleanup(stop)

	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", clientPort), 300*time.Millisecond)
		if err != nil {
			last = err
			time.Sleep(40 * time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte("ping")); err != nil {
			_ = conn.Close()
			last = err
			time.Sleep(40 * time.Millisecond)
			continue
		}
		got := make([]byte, len(quotaHotPayload))
		if _, err := io.ReadFull(conn, got); err != nil {
			_ = conn.Close()
			last = err
			time.Sleep(40 * time.Millisecond)
			continue
		}
		if string(got) != quotaHotPayload {
			_ = conn.Close()
			return nil, fmt.Errorf("payload=%q", got)
		}
		_ = conn.SetDeadline(time.Time{})
		return conn, nil
	}
	if last == nil {
		last = fmt.Errorf("timeout")
	}
	return nil, last
}

func quotaVMessClientConfig(clientPort, nodePort, destPort int, userUUID string) map[string]any {
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
			"protocol": "vmess",
			"settings": map[string]any{
				"vnext": []map[string]any{{
					"address": "127.0.0.1",
					"port":    nodePort,
					"users": []map[string]any{{
						"id":       userUUID,
						"alterId":  0,
						"security": "auto",
					}},
				}},
			},
			"streamSettings": map[string]any{
				"network":  "tcp",
				"security": "none",
			},
		}},
	}
}

func quotaVMessDownload(clientPort int) (down, up int64, err error) {
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", clientPort), 300*time.Millisecond)
		if dialErr != nil {
			last = dialErr
			time.Sleep(40 * time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		upN, writeErr := conn.Write([]byte("ping"))
		if writeErr != nil {
			_ = conn.Close()
			last = writeErr
			time.Sleep(40 * time.Millisecond)
			continue
		}
		got := make([]byte, len(quotaHotPayload))
		if _, readErr := io.ReadFull(conn, got); readErr != nil {
			_ = conn.Close()
			last = readErr
			time.Sleep(40 * time.Millisecond)
			continue
		}
		_ = conn.Close()
		if string(got) != quotaHotPayload {
			return 0, 0, fmt.Errorf("payload=%q", got)
		}
		return int64(len(quotaHotPayload)), int64(upN), nil
	}
	if last == nil {
		last = fmt.Errorf("timeout")
	}
	return 0, 0, last
}

func quotaSessionAlive(conn net.Conn) bool {
	if conn == nil {
		return false
	}
	_ = conn.SetDeadline(time.Now().Add(400 * time.Millisecond))
	_, err := conn.Write([]byte("still-online"))
	if err != nil {
		return false
	}
	// 再讀一下：被踢時寫成功也可能隨即 RST。
	buf := make([]byte, 1)
	_ = conn.SetDeadline(time.Now().Add(200 * time.Millisecond))
	_, rerr := conn.Read(buf)
	if rerr != nil {
		if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
			return true
		}
		return false
	}
	return true
}

func quotaFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func quotaWaitTCP(t *testing.T, port int) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		last = err
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("等不到 %s: %v", addr, last)
}
