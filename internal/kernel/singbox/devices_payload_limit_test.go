package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
	"github.com/sagernet/sing-box/adapter"
	singM "github.com/sagernet/sing/common/metadata"
)

// Official cedar2025/Xboard-Node #29 / #40：
// 解完 panel sync.devices 後，跨節點／machine 的 device_limit 必須攔住超額裝置。
// 不准關 device_limit。本檔走真實 WSClient 解碼路徑，再餵 ConnTracker。

const (
	devicesLimitStringJSON = `{"users":{"15029":["1.2.3.4","5.6.7.8"]},"timestamp":1,"node_id":1}`
	devicesLimitObjectJSON = `{"users":{"15029":[{"ip":"1.2.3.4"},{"ip":"5.6.7.8"}]},"timestamp":1,"node_id":1}`
	devicesLimitHoleyJSON  = `{"users":{"15029":{"0":"1.2.3.4","2":"5.6.7.8"}},"timestamp":1,"node_id":1}`
)

func TestDeviceLimitBlocksAfterPanelDevicesSync(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"字串 IP", devicesLimitStringJSON},
		{"物件 {ip:...}", devicesLimitObjectJSON},
		{"PHP 破洞物件", devicesLimitHoleyJSON},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := &devicesLimitLogBuf{}
			nlog.Init(logs, slog.LevelDebug, false)

			users, decodeLog := receivePanelDevices(t, logs, tc.raw)
			if users == nil || len(users[15029]) < 2 {
				t.Fatalf("解不出 users[15029]，跨節點 device_limit 會失效\n%s", decodeLog)
			}
			got := map[string]bool{}
			for _, ip := range users[15029] {
				got[ip] = true
			}
			if !got["1.2.3.4"] || !got["5.6.7.8"] {
				t.Fatalf("users[15029] = %v, want 1.2.3.4 與 5.6.7.8\n%s", users[15029], decodeLog)
			}

			tracker := NewConnTracker(0)
			tracker.SetUserMap(map[string]int{"uuid-15029": 15029})
			tracker.SetDeviceLimitFunc(func(uuid string) (int, bool) {
				if uuid != "uuid-15029" {
					return 0, false
				}
				return 1, true
			})
			tracker.UpdateGlobalDevices(users)

			before := logs.Len()
			same := &devicesLimitConn{}
			if wrapped := tracker.RoutedConnection(context.Background(), same, devicesLimitInbound("uuid-15029", "1.2.3.4"), nil, nil); wrapped == same || same.closed {
				t.Fatalf("他節點已見的 1.2.3.4 必須放行（limit=1）\n%s", logs.String())
			}

			excess := &devicesLimitConn{}
			wrapped := tracker.RoutedConnection(context.Background(), excess, devicesLimitInbound("uuid-15029", "9.9.9.9"), nil, nil)
			gateLogs := logs.Since(before)
			if wrapped != excess || !excess.closed {
				t.Fatalf("跨節點同步後超額 9.9.9.9 必須被 device_limit 攔住（不准關限制）\n解碼:\n%s\n限制:\n%s", decodeLog, gateLogs)
			}
			if !strings.Contains(gateLogs, "device limit") && !strings.Contains(gateLogs, "reject") {
				t.Fatalf("限制日誌要寫出攔住超額裝置:\n%s", gateLogs)
			}
			t.Logf("限制證據 name=%s blocked=9.9.9.9 allowed=1.2.3.4\n解碼:\n%s\n限制:\n%s", tc.name, decodeLog, gateLogs)
		})
	}
}

func receivePanelDevices(t *testing.T, logs *devicesLimitLogBuf, raw string) (map[int][]string, string) {
	t.Helper()

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"event": "auth.success"})
		_ = conn.WriteJSON(map[string]any{
			"event": panel.WSEventSyncDevices,
			"data":  json.RawMessage(raw),
		})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	var mu sync.Mutex
	var got panel.WSEvent
	gotEvent := false
	host := strings.TrimPrefix(server.URL, "http://")
	ws := panel.NewWSClient("ws://"+host, "devices-limit", 1, panel.WSClientConfig{}, func(event panel.WSEvent) {
		mu.Lock()
		got = event
		gotEvent = true
		mu.Unlock()
	}, nil, func() map[string]any { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	t.Cleanup(cancel)
	go ws.Run(ctx)

	deadline := time.After(1500 * time.Millisecond)
	for {
		mu.Lock()
		ok := gotEvent && got.Type == panel.WSEventSyncDevices
		mu.Unlock()
		if ok {
			break
		}
		select {
		case <-deadline:
			t.Logf("WS 沒收到 sync.devices（整包丟掉） log=\n%s", logs.String())
			return nil, logs.String()
		case <-time.After(20 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	return got.DeviceUsers, logs.String()
}

func devicesLimitInbound(uuid, ip string) adapter.InboundContext {
	return adapter.InboundContext{
		User:   uuid,
		Source: singM.Socksaddr{Addr: netip.MustParseAddr(ip)},
	}
}

type devicesLimitConn struct{ closed bool }

func (c *devicesLimitConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *devicesLimitConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *devicesLimitConn) Close() error                     { c.closed = true; return nil }
func (c *devicesLimitConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *devicesLimitConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *devicesLimitConn) SetDeadline(time.Time) error      { return nil }
func (c *devicesLimitConn) SetReadDeadline(time.Time) error  { return nil }
func (c *devicesLimitConn) SetWriteDeadline(time.Time) error { return nil }

type devicesLimitLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *devicesLimitLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *devicesLimitLogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (w *devicesLimitLogBuf) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Len()
}

func (w *devicesLimitLogBuf) Since(n int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.b.String()
	if n >= len(s) {
		return ""
	}
	return s[n:]
}
