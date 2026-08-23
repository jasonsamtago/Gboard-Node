package service

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/xray"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// Official cedar2025/Xboard-Node #16：不能使用 Host 參數值。
//
// 面板在 ws 或 tcp+http 設了 Host 後，用戶訂閱帶 Host：保持預設無法連、
// 用戶刪 Host 也無法連；只能在面板把 Host 清掉才通。設了 Host 時
// 「只能 tcping，不能真正連通」。
//
// 測試先紅：起核 → 客戶端帶 Host 握手 → 寫／讀或 GetUserTraffic 有流量。
// 不准只 Dial TCP。Host 留空仍要通。把 Host 從 inbound 拆掉當修要被抓到。
// 不准改 production。
//
// 面板預設 kernel 是 singbox；xray 同一條 applyStreamSettings 也沒把
// tcp+http Host 寫進 inbound，兩邊都鎖。

const (
	hostConnectUserID  = 16
	hostConnectUUID    = "16161616-1616-1616-1616-161616161616"
	hostConnectHost    = "cdn.example.com"
	hostConnectWSPath  = "/ws"
	hostConnectPayload = 2048
)

func TestHost_WS_WithValueMustReallyConnect(t *testing.T) {
	for _, kernelType := range []string{"singbox", "xray"} {
		t.Run(kernelType, func(t *testing.T) {
			assertHostTransportConnects(t, hostCase{
				kernelType: kernelType,
				name:       "ws+Host",
				network:    "ws",
				settings: map[string]any{
					"path": hostConnectWSPath,
					"headers": map[string]any{
						"Host": hostConnectHost,
					},
				},
				clientHost:        hostConnectHost,
				wantHostInInbound: true,
			})
		})
	}
}

func TestHost_TCPHTTP_WithValueMustReallyConnect(t *testing.T) {
	for _, kernelType := range []string{"singbox", "xray"} {
		t.Run(kernelType, func(t *testing.T) {
			assertHostTransportConnects(t, hostCase{
				kernelType: kernelType,
				name:       "tcp+http+Host",
				network:    "tcp",
				settings: map[string]any{
					"header": map[string]any{
						"type": "http",
						"request": map[string]any{
							"path": []any{"/"},
							"headers": map[string]any{
								"Host": []any{hostConnectHost},
							},
						},
					},
				},
				clientHost:        hostConnectHost,
				wantHostInInbound: true,
			})
		})
	}
}

func TestHost_EmptyStillConnects(t *testing.T) {
	for _, kernelType := range []string{"singbox", "xray"} {
		t.Run(kernelType+"/ws", func(t *testing.T) {
			assertHostTransportConnects(t, hostCase{
				kernelType: kernelType,
				name:       "ws+空Host",
				network:    "ws",
				settings: map[string]any{
					"path": hostConnectWSPath,
				},
			})
		})
		t.Run(kernelType+"/tcp", func(t *testing.T) {
			assertHostTransportConnects(t, hostCase{
				kernelType: kernelType,
				name:       "tcp+空Host",
				network:    "tcp",
			})
		})
	}
}

func TestHost_InboundKeepsHost_StripIsNotAFix(t *testing.T) {
	for _, kernelType := range []string{"singbox", "xray"} {
		t.Run(kernelType+"/ws", func(t *testing.T) {
			assertWrongHostRejected(t, hostCase{
				kernelType: kernelType,
				name:       "ws+錯Host",
				network:    "ws",
				settings: map[string]any{
					"path": hostConnectWSPath,
					"headers": map[string]any{
						"Host": hostConnectHost,
					},
				},
				clientHost:        "evil.example.com",
				wantHostInInbound: true,
				wantConnectFail:   true,
			})
		})
		t.Run(kernelType+"/tcp+http", func(t *testing.T) {
			assertWrongHostRejected(t, hostCase{
				kernelType: kernelType,
				name:       "tcp+http+錯Host",
				network:    "tcp",
				settings: map[string]any{
					"header": map[string]any{
						"type": "http",
						"request": map[string]any{
							"path": []any{"/"},
							"headers": map[string]any{
								"Host": []any{hostConnectHost},
							},
						},
					},
				},
				clientHost:        "evil.example.com",
				wantHostInInbound: true,
				wantConnectFail:   true,
			})
		})
	}
}

type hostCase struct {
	kernelType        string
	name              string
	network           string
	settings          map[string]any
	clientHost        string
	wantHostInInbound bool
	wantConnectFail   bool
}

func assertHostTransportConnects(t *testing.T, tc hostCase) {
	t.Helper()

	dest, destLn := startHostConnectDest(t)
	defer destLn.Close()

	k, nc, stop := startHostConnectKernel(t, tc)
	defer stop()

	addr := fmt.Sprintf("127.0.0.1:%d", nc.ServerPort)
	if err := tcpPingOnly(addr); err != nil {
		t.Fatalf("%s %s tcping 失敗（連 listen 都沒起來）: %v", tc.kernelType, tc.name, err)
	}
	t.Logf("%s %s tcping 證據: %s 開著（官方 #16：只能 tcping 不夠）", tc.kernelType, tc.name, addr)

	clientPort := freeTCPPort(t)
	clientStop := startXrayTunnelClient(t, hostConnectClientConfig(tc, clientPort, nc.ServerPort, dest.Port))
	defer clientStop()

	down, up, err := hostConnectDownload(clientPort)
	if err != nil {
		t.Fatalf("%s %s 真實協定連線失敗（官方 #16：設了 Host 只能 tcping）: %v", tc.kernelType, tc.name, err)
	}
	if down < int64(hostConnectPayload) {
		t.Fatalf("%s %s 讀到的流量不夠: down=%d want>=%d（不准只 tcping）", tc.kernelType, tc.name, down, hostConnectPayload)
	}

	traffic, _, _, terr := k.GetUserTraffic(context.Background())
	if terr != nil {
		t.Fatalf("%s GetUserTraffic: %v", tc.kernelType, terr)
	}
	got, ok := traffic[hostConnectUserID]
	if !ok || (got[0] == 0 && got[1] == 0) {
		t.Fatalf("%s %s 有寫／讀但仍無 user 流量: traffic=%#v down=%d up=%d", tc.kernelType, tc.name, traffic, down, up)
	}
	t.Logf("%s %s 連線證據: down=%d up=%d traffic=%v inbound仍含Host=%v", tc.kernelType, tc.name, down, up, got, tc.wantHostInInbound)
}

func assertWrongHostRejected(t *testing.T, tc hostCase) {
	t.Helper()

	dest, destLn := startHostConnectDest(t)
	defer destLn.Close()

	_, nc, stop := startHostConnectKernel(t, tc)
	defer stop()

	clientPort := freeTCPPort(t)
	clientStop := startXrayTunnelClient(t, hostConnectClientConfig(tc, clientPort, nc.ServerPort, dest.Port))
	defer clientStop()

	down, up, err := hostConnectDownload(clientPort)
	if err == nil && down >= int64(hostConnectPayload) {
		t.Fatalf("%s %s 帶錯 Host 仍走通 down=%d up=%d；把 Host 從 inbound 拆掉當修要被抓到", tc.kernelType, tc.name, down, up)
	}
	t.Logf("%s %s 反向證據: 錯 Host 連不上 err=%v down=%d（連線路徑仍要求 Host）", tc.kernelType, tc.name, err, down)
}

func startHostConnectKernel(t *testing.T, tc hostCase) (kernel.Kernel, *model.NodeSpec, func()) {
	t.Helper()

	nlog.Init(io.Discard, slog.LevelError, false)
	port := freeTCPPort(t)
	users := []model.UserSpec{{ID: hostConnectUserID, UUID: hostConnectUUID}}
	nc := &model.NodeSpec{
		Protocol:        "vless",
		ListenIP:        "127.0.0.1",
		ServerPort:      port,
		Network:         tc.network,
		NetworkSettings: tc.settings,
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

func startHostConnectDest(t *testing.T) (*net.TCPAddr, net.Listener) {
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
				_, _ = conn.Write(bytes.Repeat([]byte("D"), hostConnectPayload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln.Addr().(*net.TCPAddr), ln
}

func hostConnectClientConfig(tc hostCase, clientPort, nodePort, destPort int) map[string]any {
	stream := map[string]any{
		"network":  tc.network,
		"security": "none",
	}
	switch tc.network {
	case "ws":
		ws := map[string]any{"path": hostConnectWSPath}
		if tc.clientHost != "" {
			ws["headers"] = map[string]any{"Host": tc.clientHost}
		}
		stream["wsSettings"] = ws
	case "tcp":
		if tc.clientHost != "" {
			stream["tcpSettings"] = map[string]any{
				"header": map[string]any{
					"type": "http",
					"request": map[string]any{
						"version": "1.1",
						"method":  "GET",
						"path":    []any{"/"},
						"headers": map[string]any{
							"Host": []any{tc.clientHost},
						},
					},
				},
			}
		}
	}
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
			"protocol": "vless",
			"settings": map[string]any{
				"vnext": []map[string]any{{
					"address": "127.0.0.1",
					"port":    nodePort,
					"users": []map[string]any{{
						"id":         hostConnectUUID,
						"encryption": "none",
					}},
				}},
			},
			"streamSettings": stream,
		}},
	}
}

func hostConnectDownload(clientPort int) (down, up int64, err error) {
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
		got := make([]byte, hostConnectPayload)
		if _, readErr := io.ReadFull(conn, got); readErr != nil {
			_ = conn.Close()
			lastErr = readErr
			time.Sleep(50 * time.Millisecond)
			continue
		}
		_ = conn.Close()
		return int64(hostConnectPayload), int64(upN), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout")
	}
	return 0, 0, lastErr
}

func tcpPingOnly(addr string) error {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err
	}
	return c.Close()
}
