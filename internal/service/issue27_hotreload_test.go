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
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox"
	"github.com/jasonsamtago/Gboard-Node/internal/limiter"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/tracker"
)

// Official cedar2025/Xboard-Node #27：面板對同一 node_id 推 sync.config
// （直接改協議），必須走熱更新 Reload／必要時重拉核，不准關熱更新、
// 不准改成必須新建節點。起核後要真能連。

const (
	issue27UserID   = 27
	issue27UserUUID = "27272727-2727-2727-2727-272727272727"
	issue27Payload  = "PONG"
)

func TestApplyChanges_SameNodeIDProtocolChangeStartsAndConnects(t *testing.T) {
	svc, port, dest, logs, stop := startIssue27Service(t, "vmess")
	defer stop()

	before := logs.Len()
	svc.metricsMu.Lock()
	svc.lastConfig = issue27ServiceSpec("vless", port)
	svc.metricsMu.Unlock()
	svc.lastConfigHash = computeConfigHash(svc.lastConfig)

	svc.applyChanges(context.Background(), true, false)

	logText := issue27LogSince(logs, before)
	if strings.Contains(logText, "address already in use") {
		t.Fatalf("同 node_id 改協議不得 address already in use:\n%s", logText)
	}
	if !svc.kernel.IsRunning() {
		t.Fatalf("熱更新／重拉核後必須在跑:\n%s", logText)
	}

	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		last = tryIssue27VLESS(port, dest)
		if last == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("同 node_id vmess→vless 後必須真能連: %v\nlog=\n%s", last, logText)
}

type issue27LogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *issue27LogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *issue27LogBuf) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Len()
}

func (w *issue27LogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func issue27LogSince(w *issue27LogBuf, n int) string {
	s := w.String()
	if n >= len(s) {
		return ""
	}
	return s[n:]
}

func startIssue27Service(t *testing.T, protocol string) (*Service, int, *net.TCPAddr, *issue27LogBuf, func()) {
	t.Helper()

	logs := &issue27LogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, destAddr := startIssue27Dest(t)
	port := issue27FreePort(t)
	users := []model.UserSpec{{ID: issue27UserID, UUID: issue27UserUUID}}
	nc := issue27ServiceSpec(protocol, port)

	k := singbox.New(config.KernelConfig{Type: "singbox", LogLevel: "debug"})
	if err := k.Start(nc, users, kernel.TLSCert{}); err != nil {
		destLn.Close()
		t.Fatalf("啟動 %s: %v", protocol, err)
	}
	issue27WaitTCP(t, port)

	shared := limiter.New()
	svc := &Service{
		cfg:          &config.Config{Kernel: config.KernelConfig{Type: "singbox"}},
		kernel:       k,
		tracker:      tracker.New(),
		limiter:      shared,
		speedTracker: limiter.NewSpeedTracker(shared),
		cert:         cert.NewManager(config.CertConfig{}),
		lastConfig:   nc,
		lastUsers:    users,
		nodeLog:      nlog.ForNode(protocol, port),
	}
	svc.lastConfigHash = computeConfigHash(nc)
	svc.appliedState.Config = nc
	svc.appliedState.Users = users

	return svc, port, destAddr, logs, func() {
		k.Stop()
		destLn.Close()
	}
}

func issue27ServiceSpec(protocol string, port int) *model.NodeSpec {
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

func startIssue27Dest(t *testing.T) (net.Listener, *net.TCPAddr) {
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
				_, _ = conn.Write([]byte(issue27Payload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}

func tryIssue27VLESS(nodePort int, dest *net.TCPAddr) error {
	raw, err := hex.DecodeString(strings.ReplaceAll(issue27UserUUID, "-", ""))
	if err != nil || len(raw) != 16 {
		return fmt.Errorf("uuid")
	}
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", nodePort), 2*time.Second)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))

	var hdr bytes.Buffer
	hdr.WriteByte(0)
	hdr.Write(raw)
	hdr.WriteByte(0)
	hdr.WriteByte(1)
	_ = binary.Write(&hdr, binary.BigEndian, uint16(dest.Port))
	hdr.WriteByte(1)
	ip4 := dest.IP.To4()
	if ip4 == nil {
		return fmt.Errorf("dest not ipv4")
	}
	hdr.Write(ip4)
	if _, err := conn.Write(hdr.Bytes()); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(conn, resp); err != nil {
		return fmt.Errorf("read vless: %w", err)
	}
	if resp[1] > 0 {
		if _, err := io.ReadFull(conn, make([]byte, int(resp[1]))); err != nil {
			return err
		}
	}
	if _, err := conn.Write([]byte("ping")); err != nil {
		return err
	}
	got := make([]byte, len(issue27Payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	if string(got) != issue27Payload {
		return fmt.Errorf("payload=%q", got)
	}
	return nil
}

func issue27FreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func issue27WaitTCP(t *testing.T, port int) {
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
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等不到 %s: %v", addr, last)
}
