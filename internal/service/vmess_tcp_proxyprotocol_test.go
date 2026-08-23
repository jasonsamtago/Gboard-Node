package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/xray"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// Official cedar2025/Xboard-Node #2：VMess 節點 TCP 參數設
// acceptProxyProtocol 後，節點認不到正確用戶 UUID。
//
// ny 面板開了發送 proxy protocol，節點日誌：
//   EOF > proxy/vmess/encoding: invalid user > user do not exist
// 關掉發送並刪掉 tcp 裡的 acceptProxyProtocol 後能連。同一節點用 v2bx
// 開 PP 能連；xboard-node 設 tcp {"acceptProxyProtocol": true} 會
// timeout／invalid user。
//
// 審核鎖：
//  1. VMess + TCP + acceptProxyProtocol:true：客戶端（harness）先送
//     PROXY protocol header 再走 VMess，節點必須認對 UUID、真正建立
//     VMess 並能傳資料。不得出現 invalid user／user do not exist。
//  2. 關掉 PP（tcp 不帶 acceptProxyProtocol／false）：一般 VMess TCP
//     仍要通（回歸）。
//  3. 不准把 acceptProxyProtocol 關掉／忽略、改走非 TCP、拆掉 VMess。
//     inbound／stream 仍為 VMess TCP 且 PP 為 true。
//  4. 測的是能認出 UUID 並連上，不是只檢查 config JSON 有沒有欄位。
//
// 兩個 kernel 都會吃面板 tcp 參數（GetProxyProtocol），兩邊都鎖。
// 不准改 production。

const (
	vmessPPUserID  = 2
	vmessPPUUID    = "22222222-2222-4222-8222-222222222222"
	vmessPPPayload = 2048
)

func TestVMess_TCP_AcceptProxyProtocol_MustRecognizeUUID(t *testing.T) {
	for _, kernelType := range []string{"singbox", "xray"} {
		t.Run(kernelType, func(t *testing.T) {
			assertVMessTCPProxyProtocol(t, vmessPPCase{
				kernelType: kernelType,
				name:       "VMess+TCP+PP",
				acceptPP:   true,
				sendPP:     true,
			})
		})
	}
}

func TestVMess_TCP_NoProxyProtocol_StillConnects(t *testing.T) {
	for _, kernelType := range []string{"singbox", "xray"} {
		t.Run(kernelType, func(t *testing.T) {
			assertVMessTCPProxyProtocol(t, vmessPPCase{
				kernelType: kernelType,
				name:       "VMess+TCP+無PP",
				acceptPP:   false,
				sendPP:     false,
			})
		})
	}
}

type vmessPPCase struct {
	kernelType string
	name       string
	acceptPP   bool
	sendPP     bool
}

func assertVMessTCPProxyProtocol(t *testing.T, tc vmessPPCase) {
	t.Helper()

	dest, destLn := startVMessPPDest(t)
	defer destLn.Close()

	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	k, nc, stop := startVMessPPKernel(t, tc)
	defer stop()

	if tc.acceptPP {
		if nc.Protocol != "vmess" || !strings.EqualFold(nc.Network, "tcp") {
			t.Fatalf("%s 不准拆掉 VMess 或改走非 TCP：protocol=%q network=%q", tc.kernelType, nc.Protocol, nc.Network)
		}
		if !nc.GetProxyProtocol() {
			t.Fatalf("%s 不准把 acceptProxyProtocol 關掉／忽略面板這欄當修", tc.kernelType)
		}
	}

	nodeAddr := fmt.Sprintf("127.0.0.1:%d", nc.ServerPort)
	if err := tcpPingOnly(nodeAddr); err != nil {
		t.Fatalf("%s %s tcping 失敗（連 listen 都沒起來）: %v", tc.kernelType, tc.name, err)
	}

	dialPort := nc.ServerPort
	if tc.sendPP {
		ppPort, ppStop := startPROXYProtocolPrepender(t, nodeAddr)
		defer ppStop()
		dialPort = ppPort
	}

	clientPort := freeTCPPort(t)
	clientStop := startXrayTunnelClient(t, vmessTCPClientConfig(clientPort, dialPort, dest.Port))
	defer clientStop()

	down, up, err := vmessPPDownload(clientPort)
	logText := logs.String()
	if containsInvalidUser(err, logText) {
		t.Fatalf("%s %s 出現 invalid user／user do not exist（官方 #2：認不到 UUID）err=%v log=\n%s",
			tc.kernelType, tc.name, err, logText)
	}
	if err != nil {
		t.Fatalf("%s %s 帶 PROXY header 後 VMess 沒真正連上（官方 #2：認不到正確 UUID）: %v log=\n%s",
			tc.kernelType, tc.name, err, logText)
	}
	if down < int64(vmessPPPayload) {
		t.Fatalf("%s %s 讀到的流量不夠: down=%d want>=%d（要能傳資料，不准只檢查 JSON）",
			tc.kernelType, tc.name, down, vmessPPPayload)
	}

	traffic, _, _, terr := k.GetUserTraffic(context.Background())
	if terr != nil {
		t.Fatalf("%s GetUserTraffic: %v", tc.kernelType, terr)
	}
	got, ok := traffic[vmessPPUserID]
	if !ok || (got[0] == 0 && got[1] == 0) {
		t.Fatalf("%s %s 有寫／讀但仍無正確 UUID 流量: traffic=%#v down=%d up=%d（官方 #2：認不到 UUID）",
			tc.kernelType, tc.name, traffic, down, up)
	}
	t.Logf("%s %s 連線證據: down=%d up=%d traffic=%v acceptPP=%v sendPP=%v",
		tc.kernelType, tc.name, down, up, got, tc.acceptPP, tc.sendPP)
}

func startVMessPPKernel(t *testing.T, tc vmessPPCase) (kernel.Kernel, *model.NodeSpec, func()) {
	t.Helper()

	port := freeTCPPort(t)
	users := []model.UserSpec{{ID: vmessPPUserID, UUID: vmessPPUUID}}
	settings := map[string]any{}
	if tc.acceptPP {
		settings["acceptProxyProtocol"] = true
	}
	nc := &model.NodeSpec{
		Protocol:        "vmess",
		ListenIP:        "127.0.0.1",
		ServerPort:      port,
		Network:         "tcp",
		NetworkSettings: settings,
		TLS:             0,
		CustomRoutes:    loopbackDirectRoute(tc.kernelType),
	}

	var k kernel.Kernel
	switch tc.kernelType {
	case "xray":
		k = xray.New(config.KernelConfig{Type: "xray", LogLevel: "warning"})
	default:
		k = singbox.New(config.KernelConfig{Type: "singbox", LogLevel: "warn"})
	}
	if err := k.Start(nc, users, kernel.TLSCert{}); err != nil {
		t.Fatalf("啟動 %s kernel: %v", tc.kernelType, err)
	}
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))
	return k, nc, func() { k.Stop() }
}

func startVMessPPDest(t *testing.T) (*net.TCPAddr, net.Listener) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("dest listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(8 * time.Second))
				_, _ = conn.Write(bytes.Repeat([]byte("D"), vmessPPPayload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr), ln
}

// startPROXYProtocolPrepender 模擬 ny 面板／前置機「發送 proxy protocol」：
// 客戶端先連這裡，harness 對節點寫 PROXY v1 header 再轉 VMess 位元組。
func startPROXYProtocolPrepender(t *testing.T, nodeAddr string) (int, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("PROXY prepender listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			client, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				backend, err := net.DialTimeout("tcp", nodeAddr, time.Second)
				if err != nil {
					return
				}
				defer backend.Close()
				_ = c.SetDeadline(time.Now().Add(8 * time.Second))
				_ = backend.SetDeadline(time.Now().Add(8 * time.Second))
				header := "PROXY TCP4 192.0.2.1 192.0.2.2 43824 24022\r\n"
				if _, err := io.WriteString(backend, header); err != nil {
					return
				}
				errc := make(chan struct{}, 2)
				go func() { _, _ = io.Copy(backend, c); errc <- struct{}{} }()
				go func() { _, _ = io.Copy(c, backend); errc <- struct{}{} }()
				<-errc
			}(client)
		}
	}()
	waitTCP(t, fmt.Sprintf("127.0.0.1:%d", port))
	return port, func() { _ = ln.Close() }
}

func vmessTCPClientConfig(clientPort, nodePort, destPort int) map[string]any {
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
						"id":       vmessPPUUID,
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

func vmessPPDownload(clientPort int) (down, up int64, err error) {
	var lastErr error
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", clientPort), 300*time.Millisecond)
		if dialErr != nil {
			lastErr = dialErr
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
		upN, writeErr := conn.Write([]byte("ping"))
		if writeErr != nil {
			_ = conn.Close()
			lastErr = writeErr
			time.Sleep(50 * time.Millisecond)
			continue
		}
		got := make([]byte, vmessPPPayload)
		if _, readErr := io.ReadFull(conn, got); readErr != nil {
			_ = conn.Close()
			lastErr = readErr
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = conn.Close()
		return int64(vmessPPPayload), int64(upN), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return 0, 0, lastErr
}

func containsInvalidUser(err error, logText string) bool {
	blob := logText
	if err != nil {
		blob += "\n" + err.Error()
	}
	blob = strings.ToLower(blob)
	return strings.Contains(blob, "invalid user") || strings.Contains(blob, "user do not exist")
}
