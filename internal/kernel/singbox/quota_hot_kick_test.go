package singbox

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
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

// 官方 cedar2025/Xboard-Node #57：面板 0G／transfer 用盡把用戶抽掉後，
// sing-box UpdateUsers 必須熱踢既有連線並拒絕新連；改回 1G 不重啟也要能再連。
// 不准只修 Hy2；VLESS 與 VMess 都要覆蓋。與 #56 分開。本檔只加失敗測試。

const (
	quotaSBUserID    = 57
	quotaSBOtherID   = 7
	quotaSBUUID      = "57575757-5757-4757-8757-575757575757"
	quotaSBOtherUUID = "07070707-0707-4707-8707-070707070707"
	quotaSBPayload   = "SQ57"
)

func TestSingBoxUpdateUsers_QuotaZeroKicksLiveAndRestoreReconnects_VLESS(t *testing.T) {
	assertSingBoxQuotaCycle(t, "vless")
}

func TestSingBoxUpdateUsers_Hy2QuotaZeroRejectsThenRestore(t *testing.T) {
	// 與 #56 分開：這裡鎖的是面板抽用戶後 UpdateUsers 熱拒絕／熱恢復，不是 speed_limit 計數。
	k, port, dest, stop := startIssue65Kernel(t, "hysteria2")
	defer stop()
	if err := handshakeIssue65Hy2(t, port, dest); err != nil {
		t.Fatalf("前置：有額度 Hy2 必須能 handshake: %v", err)
	}
	inst := singBoxInstance(k)

	_, _, err := k.UpdateUsers([]model.UserSpec{})
	if err != nil {
		t.Fatalf("UpdateUsers 0G: %v（不准把起核失敗當限流成功）", err)
	}
	if !k.IsRunning() {
		t.Fatal("不准把起核失敗／核停掉當 0G 限流成功")
	}
	if err := handshakeIssue65Hy2(t, port, dest); err == nil {
		t.Fatal("Hy2 0G 後必須拒絕 handshake，卻還能連")
	}

	_, _, err = k.UpdateUsers(issue65Users())
	if err != nil {
		t.Fatalf("UpdateUsers 1G: %v", err)
	}
	if singBoxInstance(k) == nil || !k.IsRunning() {
		t.Fatal("1G 恢復後 sing-box 必須在跑")
	}
	if inst != singBoxInstance(k) {
		t.Log("熱恢復允許重拉 inbound，但不准要求重啟容器")
	}
	if err := handshakeIssue65Hy2(t, port, dest); err != nil {
		t.Fatalf("Hy2 改回 1G 必須不重啟也能再連: %v", err)
	}
}

func assertSingBoxQuotaCycle(t *testing.T, protocol string) {
	t.Helper()
	s, port, dest, logs, stop := startQuotaSingBox(t, protocol, quotaSBUsers(quotaSBUUID))
	defer stop()
	inst := singBoxInstance(s)

	if err := tryQuotaSBVLESS(port, quotaSBUUID, dest); err != nil {
		t.Fatalf("前置 %s 有額度必須能連: %v", protocol, err)
	}
	if err := tryQuotaSBVLESS(port, quotaSBOtherUUID, dest); err != nil {
		t.Fatalf("前置：沒被抽掉的人必須能連: %v", err)
	}

	live, err := openQuotaSBVLESS(port, quotaSBUUID, dest)
	if err != nil {
		t.Fatalf("前置：用戶仍在線時必須先握上既有連線: %v", err)
	}
	defer live.Close()

	before := logs.Len()
	added, removed, err := s.UpdateUsers([]model.UserSpec{{ID: quotaSBOtherID, UUID: quotaSBOtherUUID}})
	if err != nil {
		t.Fatalf("UpdateUsers 0G: %v", err)
	}
	logText := logs.Since(before)

	if strings.Contains(logText, "failed to start") && !s.IsRunning() {
		t.Fatalf("不准把起核失敗當 0G 限流成功:\n%s", logText)
	}
	if quotaSBSessionAlive(live) {
		t.Fatalf("%s 用戶仍在線時抽掉額度，既有連線必須被踢，卻還活著 added=%d removed=%d\nlog=\n%s",
			protocol, added, removed, logText)
	}
	if err := tryQuotaSBVLESS(port, quotaSBUUID, dest); err == nil {
		t.Fatalf("%s 0G 後該 UUID 新連線必須拒絕，卻還能連\nlog=\n%s", protocol, logText)
	}
	if err := tryQuotaSBVLESS(port, quotaSBOtherUUID, dest); err != nil {
		t.Fatalf("沒被抽掉的人不能被誤殺: %v\nlog=\n%s", err, logText)
	}

	before = logs.Len()
	_, _, err = s.UpdateUsers(quotaSBUsers(quotaSBUUID))
	if err != nil {
		t.Fatalf("UpdateUsers 1G: %v", err)
	}
	logText += logs.Since(before)

	if !s.IsRunning() {
		t.Fatalf("1G 恢復後必須在跑:\n%s", logText)
	}
	if singBoxInstance(s) != inst {
		t.Log("熱恢復允許重拉 inbound")
	}
	if err := tryQuotaSBVLESS(port, quotaSBUUID, dest); err != nil {
		t.Fatalf("%s 改回 1G 必須不重啟也能再連: %v\nlog=\n%s", protocol, err, logText)
	}
	t.Logf("singbox %s 0G 熱踢＋1G 熱恢復證據 removed=%d\n%s", protocol, removed, logText)
}

func quotaSBUsers(rotated string) []model.UserSpec {
	return []model.UserSpec{
		{ID: quotaSBUserID, UUID: rotated},
		{ID: quotaSBOtherID, UUID: quotaSBOtherUUID},
	}
}

func startQuotaSingBox(t *testing.T, protocol string, users []model.UserSpec) (*SingBox, int, *net.TCPAddr, *sbLockedLogBuf, func()) {
	t.Helper()

	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, destAddr := startQuotaSBDest(t)
	port := sbFreeTCPPort(t)
	nc := &model.NodeSpec{
		Protocol:   protocol,
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Network:    "tcp",
		CustomRoutes: []map[string]any{{
			"ip_cidr":  []string{"127.0.0.0/8"},
			"outbound": "direct",
		}},
	}

	s := New(config.KernelConfig{Type: "singbox", LogLevel: "debug"})
	if err := s.Start(nc, users, kernel.TLSCert{}); err != nil {
		destLn.Close()
		t.Fatalf("啟動 singbox %s: %v", protocol, err)
	}
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))

	return s, port, destAddr, logs, func() {
		s.Stop()
		destLn.Close()
	}
}

func startQuotaSBDest(t *testing.T) (net.Listener, *net.TCPAddr) {
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
				_, _ = conn.Write([]byte(quotaSBPayload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}

func tryQuotaSBVLESS(nodePort int, userUUID string, dest *net.TCPAddr) error {
	conn, err := openQuotaSBVLESS(nodePort, userUUID, dest)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func openQuotaSBVLESS(nodePort int, userUUID string, dest *net.TCPAddr) (net.Conn, error) {
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
		return nil, err
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
	got := make([]byte, len(quotaSBPayload))
	if _, err := io.ReadFull(conn, got); err != nil {
		conn.Close()
		return nil, err
	}
	if string(got) != quotaSBPayload {
		conn.Close()
		return nil, fmt.Errorf("payload=%q", got)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func quotaSBSessionAlive(conn net.Conn) bool {
	if conn == nil {
		return false
	}
	_ = conn.SetDeadline(time.Now().Add(400 * time.Millisecond))
	if _, err := conn.Write([]byte("still-online")); err != nil {
		return false
	}
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
