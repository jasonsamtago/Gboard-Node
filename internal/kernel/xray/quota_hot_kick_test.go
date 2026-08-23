package xray

import (
	"bytes"
	"crypto/tls"
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
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// 官方 cedar2025/Xboard-Node #57：面板 0G／transfer 用盡把用戶抽掉後，
// xray UpdateUsers 必須熱踢既有連線並拒絕新連；改回 1G 不重啟也要能再連。
// CloseUserConnections 現行是 no-op，不准把「起核失敗」當限流成功。
// 與 #56（Hy2 UDP CloseByUUID）分開。本檔只加失敗測試。

const (
	quotaXrayUserID    = 57
	quotaXrayOtherID   = 7
	quotaXrayUUID      = "57575757-5757-4757-8757-575757575757"
	quotaXrayOtherUUID = "07070707-0707-4707-8707-070707070707"
	quotaXrayPayload = "XQ57"
)

func TestXrayUpdateUsers_QuotaZeroKicksLiveAndRestoreReconnects(t *testing.T) {
	x, port, dest, logs, stop := startQuotaXrayVLESS(t, quotaXrayUsers(quotaXrayUUID))
	defer stop()
	inst := xrayInstance(x)

	if err := tryQuotaXrayVLESS(port, quotaXrayUUID, dest); err != nil {
		t.Fatalf("前置：有額度必須能連: %v", err)
	}
	if err := tryQuotaXrayVLESS(port, quotaXrayOtherUUID, dest); err != nil {
		t.Fatalf("前置：沒被抽掉的人必須能連: %v", err)
	}

	live, err := openQuotaXrayVLESS(port, quotaXrayUUID, dest)
	if err != nil {
		t.Fatalf("前置：用戶仍在線時必須先握上既有連線: %v", err)
	}
	defer live.Close()

	before := logs.Len()
	added, removed, err := x.UpdateUsers([]model.UserSpec{{ID: quotaXrayOtherID, UUID: quotaXrayOtherUUID}})
	if err != nil {
		t.Fatalf("UpdateUsers 0G: %v", err)
	}
	logText := logs.Since(before)

	if strings.Contains(logText, "failed to start") && !x.IsRunning() {
		t.Fatalf("不准把起核失敗當 0G 限流成功:\n%s", logText)
	}
	if quotaXraySessionAlive(live) {
		t.Fatalf("用戶仍在線時抽掉額度，既有連線必須被踢，卻還活著 added=%d removed=%d\nlog=\n%s", added, removed, logText)
	}
	if err := tryQuotaXrayVLESS(port, quotaXrayUUID, dest); err == nil {
		t.Fatalf("0G 後該 UUID 新連線必須拒絕，卻還能連\nlog=\n%s", logText)
	}
	if err := tryQuotaXrayVLESS(port, quotaXrayOtherUUID, dest); err != nil {
		t.Fatalf("沒被抽掉的人不能被誤殺: %v\nlog=\n%s", err, logText)
	}

	before = logs.Len()
	_, _, err = x.UpdateUsers(quotaXrayUsers(quotaXrayUUID))
	if err != nil {
		t.Fatalf("UpdateUsers 1G: %v", err)
	}
	logText += logs.Since(before)

	if !x.IsRunning() {
		t.Fatalf("1G 恢復後 xray 必須在跑，不准等重啟容器:\n%s", logText)
	}
	if xrayInstance(x) != inst && strings.Contains(logText, "performing full restart") {
		t.Log("熱恢復允許重拉核，但不准要求重啟容器")
	}
	if err := tryQuotaXrayVLESS(port, quotaXrayUUID, dest); err != nil {
		t.Fatalf("改回 1G 必須不重啟也能再連: %v\nlog=\n%s", err, logText)
	}
	t.Logf("xray 0G 熱踢＋1G 熱恢復證據 removed=%d\n%s", removed, logText)
}

func quotaXrayUsers(rotated string) []model.UserSpec {
	return []model.UserSpec{
		{ID: quotaXrayUserID, UUID: rotated},
		{ID: quotaXrayOtherID, UUID: quotaXrayOtherUUID},
	}
}

func startQuotaXrayVLESS(t *testing.T, users []model.UserSpec) (*Xray, int, *net.TCPAddr, *lockedLogBuf, func()) {
	t.Helper()

	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, destAddr := startQuotaXrayDest(t)
	port := freeTCPPort(t)
	tlsCert := uuidHotTLSCert(t)
	nc := &model.NodeSpec{
		Protocol:    "vless",
		ListenIP:    "127.0.0.1",
		ServerPort:  port,
		Network:     "tcp",
		TLS:         1,
		ServerName:  uuidHotSNI,
		TLSSettings: map[string]any{"server_name": uuidHotSNI},
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

func startQuotaXrayDest(t *testing.T) (net.Listener, *net.TCPAddr) {
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
				_, _ = conn.Write([]byte(quotaXrayPayload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}

func tryQuotaXrayVLESS(nodePort int, userUUID string, dest *net.TCPAddr) error {
	conn, err := openQuotaXrayVLESS(nodePort, userUUID, dest)
	if err != nil {
		return err
	}
	_ = conn.Close()
	return nil
}

func openQuotaXrayVLESS(nodePort int, userUUID string, dest *net.TCPAddr) (net.Conn, error) {
	raw, err := hex.DecodeString(strings.ReplaceAll(userUUID, "-", ""))
	if err != nil || len(raw) != 16 {
		return nil, fmt.Errorf("uuid")
	}

	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 2 * time.Second},
		"tcp",
		fmt.Sprintf("127.0.0.1:%d", nodePort),
		&tls.Config{InsecureSkipVerify: true, ServerName: uuidHotSNI},
	)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
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
		return nil, fmt.Errorf("destination 不是 IPv4")
	}
	hdr.Write(ip4)
	if _, err := conn.Write(hdr.Bytes()); err != nil {
		conn.Close()
		return nil, fmt.Errorf("寫 VLESS 握手: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		conn.Close()
		return nil, fmt.Errorf("讀 VLESS 回應: %w", err)
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
	got := make([]byte, len(quotaXrayPayload))
	if _, err := io.ReadFull(conn, got); err != nil {
		conn.Close()
		return nil, fmt.Errorf("讀 payload: %w", err)
	}
	if string(got) != quotaXrayPayload {
		conn.Close()
		return nil, fmt.Errorf("payload=%q", got)
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func quotaXraySessionAlive(conn net.Conn) bool {
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
