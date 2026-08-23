package xray

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

// Official cedar2025/Xboard-Node #27：同一 node_id 改協議／埠後，
// Reload 或 Start（熱更新／重啟核）必須能起核並真能連；舊 listener
// 必須放掉，不得長期 address already in use。不准靠新建 node_id。

const (
	issue27UserID    = 27
	issue27UserUUID  = "27272727-2727-2727-2727-272727272727"
	issue27OtherID   = 7
	issue27OtherUUID = "07070707-0707-0707-0707-070707070707"
)

func TestXrayReload_SameNodeIDPortChangeReleasesOldListener(t *testing.T) {
	x, oldPort, dest, logs, stop := startXrayIssue27VLESS(t)
	defer stop()

	// 先占一條連線：官方現場舊埠被舊 instance 拖住。
	// closeOld 若先 drain 再關 listen，舊埠會長期 address already in use。
	held, err := holdXrayVLESS(t, oldPort, dest)
	if err != nil {
		t.Fatalf("前置：占住舊連線: %v", err)
	}
	defer held.Close()

	newPort := freeTCPPort(t)
	before := logs.Len()
	next := issue27VLESSSpec(newPort)
	if err := x.Reload(next, issue27Users(), uuidHotTLSCert(t)); err != nil {
		t.Fatalf("同 node_id 改埠 Reload 必須成功: %v\nlog=\n%s", err, logs.Since(before))
	}
	logText := logs.Since(before)
	if strings.Contains(logText, "address already in use") {
		t.Fatalf("改埠不得 address already in use:\n%s", logText)
	}

	if err := issue27MustBind("127.0.0.1", oldPort); err != nil {
		t.Fatalf("舊埠 %d 必須在熱更新後放掉，不得長期被占: %v\nlog=\n%s", oldPort, err, logText)
	}
	waitTLS(t, fmt.Sprintf("127.0.0.1:%d", newPort), uuidHotSNI)
	if err := tryVLESSUser(t, newPort, issue27UserUUID, dest); err != nil {
		t.Fatalf("新埠必須真能連: %v\nlog=\n%s", err, logText)
	}
}

func TestXrayStart_SameNodeIDSamePortMustRebind(t *testing.T) {
	// kernel.Start 合約：已在跑的核再 Start 必須先停舊 instance。
	// 同一 node_id、同一埠改協議（或重拉核）不得 EADDRINUSE。
	x, port, dest, logs, stop := startXrayIssue27VLESS(t)
	defer stop()

	before := logs.Len()
	vmess := &model.NodeSpec{
		Protocol:     "vmess",
		ListenIP:     "127.0.0.1",
		ServerPort:   port,
		Network:      "tcp",
		CustomRoutes: issue27XrayLoopback(),
	}
	if err := x.Start(vmess, issue27Users(), kernel.TLSCert{}); err != nil {
		t.Fatalf("同 node_id 同埠 Start(vmess) 必須先放掉舊 listener: %v\nlog=\n%s", err, logs.Since(before))
	}
	logText := logs.Since(before)
	if strings.Contains(logText, "address already in use") {
		t.Fatalf("同埠改協議不得 address already in use:\n%s", logText)
	}
	if !x.IsRunning() {
		t.Fatal("改協議後 kernel 必須在跑")
	}

	// 舊 vless inbound 必須消失：再握手應該失敗。
	if err := tryVLESSUser(t, port, issue27UserUUID, dest); err == nil {
		t.Fatalf("已改成 vmess，舊 vless listener 還能連（舊核沒放掉）\nlog=\n%s", logText)
	}
}

func startXrayIssue27VLESS(t *testing.T) (*Xray, int, *net.TCPAddr, *lockedLogBuf, func()) {
	t.Helper()

	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, destAddr := startUUIDHotDest(t)
	port := freeTCPPort(t)
	tlsCert := uuidHotTLSCert(t)
	x := New(config.KernelConfig{Type: "xray", LogLevel: "warning"})
	if err := x.Start(issue27VLESSSpec(port), issue27Users(), tlsCert); err != nil {
		destLn.Close()
		t.Fatalf("啟動 xray: %v", err)
	}
	waitTLS(t, fmt.Sprintf("127.0.0.1:%d", port), uuidHotSNI)

	return x, port, destAddr, logs, func() {
		x.Stop()
		destLn.Close()
	}
}

func issue27VLESSSpec(port int) *model.NodeSpec {
	return &model.NodeSpec{
		Protocol:     "vless",
		ListenIP:     "127.0.0.1",
		ServerPort:   port,
		Network:      "tcp",
		TLS:          1,
		ServerName:   uuidHotSNI,
		TLSSettings:  map[string]any{"server_name": uuidHotSNI},
		CustomRoutes: issue27XrayLoopback(),
	}
}

func issue27XrayLoopback() []map[string]any {
	return []map[string]any{{
		"type":        "field",
		"ip":          []string{"127.0.0.0/8"},
		"outboundTag": "direct",
	}}
}

func issue27Users() []model.UserSpec {
	return []model.UserSpec{
		{ID: issue27UserID, UUID: issue27UserUUID},
		{ID: issue27OtherID, UUID: issue27OtherUUID},
	}
}

func holdXrayVLESS(t *testing.T, nodePort int, dest *net.TCPAddr) (net.Conn, error) {
	t.Helper()
	// 只建立 TCP/TLS，不走完 VLESS 握手也足以讓舊 inbound 占著埠。
	// 若 kernel 把連線算進 drain，舊 listener 更不能拖數分鐘。
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", nodePort), 2*time.Second)
	if err != nil {
		return nil, err
	}
	_ = dest
	return conn, nil
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
