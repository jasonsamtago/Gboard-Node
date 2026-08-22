package service

import (
	"bytes"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// Official cedar2025/Xboard-Node #33：singbox Hy2 偶發退出，容器仍 Up
// 但 UDP 不再聽。外部 watchdog 用 ss -ulnp 發現後 docker rm -f。
//
// 審核 2 已鎖定：包裝層必須自己偵測並重拉 kernel；容器 Up 但 UDP
// 消失不算健康。反向：健康 kernel 不能被誤重啟。不准只靠外部
// docker watchdog。watchdog 日誌當證據。
//
// 測試可模擬 kernel 退出／UDP 消失。必須走進程內 WatchKernel，
// 不准 docker rm。Service.Run 必須自己打 tick，不能等使用者更新。

const watchdogHy2UserID = 33

func TestKernelWatchdog_MustBeWiredIntoServiceRun(t *testing.T) {
	src, err := os.ReadFile("service.go")
	if err != nil {
		t.Fatalf("讀 service.go: %v", err)
	}
	if bytes.Contains(src, []byte("docker rm")) || bytes.Contains(src, []byte("docker rm -f")) {
		t.Fatal("不准 docker rm -f 當修，必須是進程內包裝層")
	}
	if !bytes.Contains(src, []byte("WatchKernel()")) {
		t.Fatal("Service.Run 沒有呼叫 WatchKernel：singbox／Hy2 退出後包裝層不會自己重拉（容器 Up 不算健康）")
	}
}

func TestKernelWatchdog_ProcessExitRestartsKernel(t *testing.T) {
	k := &fakeKernel{running: false}
	port := freeUDPPort(t)
	s, logs := newWatchdogService(t, k, "hysteria2", port)

	invokeWatchKernel(t, s)

	if k.startCalls == 0 {
		t.Fatal("singbox／Hy2 進程退出後包裝層沒重拉 kernel（容器可以仍是 Up）")
	}
	if !k.running {
		t.Fatal("重拉後 kernel 仍不是 running")
	}
	logText := logs.String()
	if !strings.Contains(logText, "watchdog") {
		t.Fatalf("進程退出沒有 watchdog 日誌:\n%s", logText)
	}
	if !strings.Contains(logText, "process") && !strings.Contains(logText, "exited") {
		t.Fatalf("沒寫偵測到進程消失:\n%s", logText)
	}
	if !strings.Contains(logText, "restart") {
		t.Fatalf("沒寫重拉 kernel:\n%s", logText)
	}
	if strings.Contains(logText, "docker rm") {
		t.Fatal("不准 docker rm 當修")
	}
	writeWatchdogEvidence(t, "watchdog_process_exit.log", logText)
	t.Logf("進程退出證據 startCalls=%d\n%s", k.startCalls, logText)
}

func TestKernelWatchdog_UDPListenGoneRestartsKernel(t *testing.T) {
	k := &fakeKernel{running: true}
	port := freeUDPPort(t)
	s, logs := newWatchdogService(t, k, "hysteria2", port)

	if udpPortListening(port) {
		t.Fatalf("測試前提失敗：UDP :%d 不該在聽", port)
	}

	invokeWatchKernel(t, s)

	if k.startCalls == 0 {
		t.Fatal("Hy2 UDP listen 消失後包裝層沒重拉 kernel（IsRunning／容器 Up 不算健康）")
	}
	logText := logs.String()
	if !strings.Contains(logText, "watchdog") {
		t.Fatalf("UDP 消失沒有 watchdog 日誌:\n%s", logText)
	}
	if !strings.Contains(logText, "UDP") {
		t.Fatalf("沒寫偵測到 UDP 消失:\n%s", logText)
	}
	if !strings.Contains(logText, "restart") {
		t.Fatalf("沒寫重拉 kernel:\n%s", logText)
	}
	if strings.Contains(logText, "docker rm") {
		t.Fatal("不准 docker rm 當修")
	}
	writeWatchdogEvidence(t, "watchdog_udp_missing.log", logText)
	t.Logf("UDP 消失證據 startCalls=%d\n%s", k.startCalls, logText)
}

func TestKernelWatchdog_HealthyKernelNotRestarted(t *testing.T) {
	k := &fakeKernel{running: true}
	ln, port := listenUDPPort(t)
	defer ln.Close()
	s, logs := newWatchdogService(t, k, "hysteria2", port)

	if !udpPortListening(port) {
		t.Fatalf("測試前提失敗：健康 Hy2 必須先佔 UDP :%d", port)
	}

	invokeWatchKernel(t, s)

	if k.startCalls != 0 {
		t.Fatalf("健康 kernel 被誤重啟 startCalls=%d", k.startCalls)
	}
	logText := logs.String()
	if !strings.Contains(logText, "watchdog") {
		t.Fatalf("健康時沒有 watchdog 日誌:\n%s", logText)
	}
	if !strings.Contains(logText, "healthy") && !strings.Contains(logText, "skip") {
		t.Fatalf("健康時沒寫不重啟:\n%s", logText)
	}
	if strings.Contains(logText, "restart") {
		t.Fatalf("健康 kernel 日誌不該寫 restart:\n%s", logText)
	}
	writeWatchdogEvidence(t, "watchdog_healthy_skip.log", logText)
	t.Logf("健康不重啟證據 startCalls=%d\n%s", k.startCalls, logText)
}

func TestKernelWatchdog_TCPProtocolMissingUDPIsNotUnhealthy(t *testing.T) {
	k := &fakeKernel{running: true}
	port := freeUDPPort(t)
	s, logs := newWatchdogService(t, k, "vless", port)

	invokeWatchKernel(t, s)

	if k.startCalls != 0 {
		t.Fatalf("TCP 協議沒聽 UDP 被當成不健康而重啟 startCalls=%d", k.startCalls)
	}
	logText := logs.String()
	if strings.Contains(logText, "restart") {
		t.Fatalf("VLESS 不該因沒有 UDP 而重拉:\n%s", logText)
	}
	writeWatchdogEvidence(t, "watchdog_tcp_no_udp_skip.log", logText)
	t.Logf("TCP 無 UDP 不重啟證據 startCalls=%d\n%s", k.startCalls, logText)
}

func invokeWatchKernel(t *testing.T, s *Service) {
	t.Helper()
	m := reflect.ValueOf(s).MethodByName("WatchKernel")
	if !m.IsValid() {
		t.Fatal("包裝層沒有 WatchKernel：singbox／Hy2 退出或 UDP 消失後不會自己重拉 kernel（不准只靠 docker rm）")
	}
	m.Call(nil)
}

func newWatchdogService(t *testing.T, k *fakeKernel, protocol string, port int) (*Service, *lockedLogBuf) {
	t.Helper()
	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)
	s := newTestService(k)
	s.lastConfig = &model.NodeSpec{
		Protocol:   protocol,
		ListenIP:   "127.0.0.1",
		ServerPort: port,
	}
	s.lastUsers = []model.UserSpec{{
		ID:   watchdogHy2UserID,
		UUID: "33333333-3333-3333-3333-333333333333",
	}}
	s.nodeLog = nlog.ForNode(protocol, port)
	s.appliedState.Config = s.lastConfig
	s.appliedState.Users = s.lastUsers
	return s, logs
}

func listenUDPPort(t *testing.T) (*net.UDPConn, int) {
	t.Helper()
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen UDP: %v", err)
	}
	return ln, ln.LocalAddr().(*net.UDPAddr).Port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	ln, port := listenUDPPort(t)
	ln.Close()
	return port
}

func udpPortListening(port int) bool {
	want := fmt.Sprintf(":%04X", port)
	for _, path := range []string{"/proc/net/udp", "/proc/net/udp6"} {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || !strings.Contains(fields[1], ":") {
				continue
			}
			local := fields[1]
			if strings.HasSuffix(strings.ToUpper(local), want) {
				return true
			}
		}
	}
	return false
}

func writeWatchdogEvidence(t *testing.T, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(t.TempDir(), name), []byte(text+"\n"), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
}
