package panel

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/sagernet/sing-box/adapter"
	singM "github.com/sagernet/sing/common/metadata"
)

// Official cedar2025/Xboard-Node #29 / #40：
// ws: cannot decode devices payload
//   'users[15029][0]' expected type 'string', got unconvertible type 'map[string]interface {}'
//
// 根因：weak decoder 把 users[id][0] 當成 map 而不是 string。
// 面板格式有字串 IP（{"15029":["1.2.3.4"]}）也有物件（{ip:...}／PHP array_unique 破洞物件）。
// 解失敗後 handleDataEvent 整包 return，UpdateGlobalDevices 吃不到資料，
// device_limit 跨節點／machine 失效（單節點本機還行）。
//
// 不准關 device_limit。單筆解失敗不得拖整包。
// 面板編碼（Gboard #49／#54）已合，本檔只鎖節點端解碼。

const (
	devicesOfficialStringJSON = `{"users":{"15029":["1.2.3.4","5.6.7.8"]},"timestamp":1,"node_id":1}`
	devicesOfficialObjectJSON = `{"users":{"15029":[{"ip":"1.2.3.4"},{"ip":"5.6.7.8"}]},"timestamp":1,"node_id":1}`
	devicesHoleyPHPJSON       = `{"users":{"15029":{"0":"1.2.3.4","2":"5.6.7.8"}},"timestamp":1,"node_id":1}`
	devicesMixedJSON          = `{"users":{"15029":["1.2.3.4"],"18032":[{"ip":"10.0.0.1"}]},"timestamp":1,"node_id":7}`
	devicesOneBadJSON         = `{"users":{"15029":["1.2.3.4"],"18032":[{"nope":true}]},"timestamp":1,"node_id":7}`
)

func TestDecodeDevicesPayload_OfficialStringIPMap(t *testing.T) {
	ev, logs := receiveDevicesEvent(t, devicesOfficialStringJSON)
	assertDecodedUserIPs(t, ev, 15029, "1.2.3.4", "5.6.7.8")
	assertDecodeLogSuccess(t, logs.String(), 15029, "1.2.3.4", "5.6.7.8")
	t.Logf("解碼日誌（字串 IP）:\n%s", logs.String())
}

func TestDecodeDevicesPayload_ObjectElements(t *testing.T) {
	ev, logs := receiveDevicesEvent(t, devicesOfficialObjectJSON)
	if strings.Contains(logs.String(), "cannot decode devices payload") && ev.DeviceUsers == nil {
		t.Fatalf("物件元素被 weak decoder 整包丟掉（官方 #29）:\n%s", logs.String())
	}
	assertDecodedUserIPs(t, ev, 15029, "1.2.3.4", "5.6.7.8")
	assertDecodeLogSuccess(t, logs.String(), 15029, "1.2.3.4", "5.6.7.8")
	t.Logf("解碼日誌（物件 {ip:...}）:\n%s", logs.String())
}

func TestDecodeDevicesPayload_HoleyPHPObject(t *testing.T) {
	ev, logs := receiveDevicesEvent(t, devicesHoleyPHPJSON)
	if strings.Contains(logs.String(), "users[15029][0]") && strings.Contains(logs.String(), "map[string]interface") {
		t.Fatalf("官方症狀：破洞物件被當成 users[id][0] map，整包丟掉:\n%s", logs.String())
	}
	assertDecodedUserIPs(t, ev, 15029, "1.2.3.4", "5.6.7.8")
	assertDecodeLogSuccess(t, logs.String(), 15029, "1.2.3.4", "5.6.7.8")
	t.Logf("解碼日誌（PHP 破洞物件）:\n%s", logs.String())
}

func TestDecodeDevicesPayload_MixedStringAndObjectKeepsBoth(t *testing.T) {
	ev, logs := receiveDevicesEvent(t, devicesMixedJSON)
	if ev.DeviceUsers == nil {
		t.Fatalf("字串＋物件混合時整包丟掉，18032 的物件不該拖死 15029:\n%s", logs.String())
	}
	assertDecodedUserIPs(t, ev, 15029, "1.2.3.4")
	assertDecodedUserIPs(t, ev, 18032, "10.0.0.1")
	assertDecodeLogSuccess(t, logs.String(), 15029, "1.2.3.4")
	assertDecodeLogSuccess(t, logs.String(), 18032, "10.0.0.1")
	t.Logf("解碼日誌（混合）:\n%s", logs.String())
}

func TestDecodeDevicesPayload_SingleBadUserDoesNotDropPackage(t *testing.T) {
	ev, logs := receiveDevicesEvent(t, devicesOneBadJSON)
	if ev.DeviceUsers == nil {
		t.Fatalf("單筆解失敗不得整包丟掉:\n%s", logs.String())
	}
	assertDecodedUserIPs(t, ev, 15029, "1.2.3.4")
	if ips, ok := ev.DeviceUsers[18032]; ok && len(ips) > 0 {
		t.Fatalf("18032 解失敗不該進限制: %v\n%s", ips, logs.String())
	}
	logText := logs.String()
	if !strings.Contains(logText, "18032") || !(strings.Contains(logText, "skip") || strings.Contains(logText, "跳過")) {
		t.Fatalf("解碼日誌要寫出失敗那筆被跳過:\n%s", logText)
	}
	assertDecodeLogSuccess(t, logText, 15029, "1.2.3.4")
	if strings.Contains(logText, "cannot decode devices payload") && len(ev.DeviceUsers) == 0 {
		t.Fatalf("有成功用戶時不准走整包 cannot decode:\n%s", logText)
	}
	t.Logf("解碼日誌（單筆失敗不拖整包）:\n%s", logText)
}

func TestDecodeDevicesPayload_DeviceLimitBlocksAfterCrossNodeSync(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"字串 IP", devicesOfficialStringJSON},
		{"物件 {ip:...}", devicesOfficialObjectJSON},
		{"PHP 破洞物件", devicesHoleyPHPJSON},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev, logs := receiveDevicesEvent(t, tc.raw)
			if ev.DeviceUsers == nil {
				t.Fatalf("解不出 devices，跨節點 device_limit 會失效:\n%s", logs.String())
			}
			assertDecodedUserIPs(t, ev, 15029, "1.2.3.4", "5.6.7.8")

			tracker := singbox.NewConnTracker(0)
			tracker.SetUserMap(map[string]int{"uuid-15029": 15029})
			tracker.SetDeviceLimitFunc(func(uuid string) (int, bool) {
				if uuid != "uuid-15029" {
					return 0, false
				}
				return 1, true
			})
			tracker.UpdateGlobalDevices(ev.DeviceUsers)

			before := logs.Len()
			same := &devicesTestConn{}
			if wrapped := tracker.RoutedConnection(context.Background(), same, devicesInbound("uuid-15029", "1.2.3.4"), nil, nil); wrapped == same || same.closed {
				t.Fatalf("他節點已見的 1.2.3.4 必須放行（limit=1）\n%s", logs.String())
			}

			excess := &devicesTestConn{}
			wrapped := tracker.RoutedConnection(context.Background(), excess, devicesInbound("uuid-15029", "9.9.9.9"), nil, nil)
			gateLogs := logs.Since(before)
			if wrapped != excess || !excess.closed {
				t.Fatalf("跨節點同步後超額 9.9.9.9 必須被 device_limit 攔住（不准關限制）\n解碼日誌:\n%s\n限制日誌:\n%s", logs.String(), gateLogs)
			}
			if !strings.Contains(gateLogs, "device limit") && !strings.Contains(gateLogs, "reject") {
				t.Fatalf("限制日誌要寫出攔住超額裝置:\n%s", gateLogs)
			}
			t.Logf("限制證據 name=%s blocked=9.9.9.9 allowed=1.2.3.4\n解碼:\n%s\n限制:\n%s", tc.name, logs.String(), gateLogs)
		})
	}
}

func receiveDevicesEvent(t *testing.T, raw string) (WSEvent, *devicesLogBuf) {
	t.Helper()
	logs := &devicesLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	var ev WSEvent
	called := false
	w := NewWSClient("ws://devices.test", "tok", 1, WSClientConfig{}, func(event WSEvent) {
		called = true
		ev = event
	}, nil, nil)
	w.handleDataEvent(wsMessage{Event: WSEventSyncDevices, Data: []byte(raw)})
	if !called {
		t.Logf("handleDataEvent 沒有送出事件（整包丟掉） log=\n%s", logs.String())
	}
	return ev, logs
}

func assertDecodedUserIPs(t *testing.T, ev WSEvent, userID int, want ...string) {
	t.Helper()
	if ev.Type != "" && ev.Type != WSEventSyncDevices {
		t.Fatalf("event.Type = %q, want %q", ev.Type, WSEventSyncDevices)
	}
	if ev.DeviceUsers == nil {
		t.Fatalf("DeviceUsers 是 nil，users[%d] 解不出來", userID)
	}
	got := ev.DeviceUsers[userID]
	if len(got) != len(want) {
		t.Fatalf("users[%d] = %v, want %v", userID, got, want)
	}
	seen := map[string]bool{}
	for _, ip := range got {
		seen[ip] = true
	}
	for _, ip := range want {
		if !seen[ip] {
			t.Fatalf("users[%d] 缺少 %s: %v", userID, ip, got)
		}
	}
}

func assertDecodeLogSuccess(t *testing.T, logs string, userID int, ips ...string) {
	t.Helper()
	if !strings.Contains(logs, strconv.Itoa(userID)) {
		t.Fatalf("解碼日誌要寫出成功的 user=%d:\n%s", userID, logs)
	}
	for _, ip := range ips {
		if !strings.Contains(logs, ip) {
			t.Fatalf("解碼日誌要寫出成功的 IP %s:\n%s", ip, logs)
		}
	}
}

func devicesInbound(uuid, ip string) adapter.InboundContext {
	return adapter.InboundContext{
		User:   uuid,
		Source: singM.Socksaddr{Addr: netip.MustParseAddr(ip)},
	}
}

type devicesTestConn struct{ closed bool }

func (c *devicesTestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *devicesTestConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *devicesTestConn) Close() error                     { c.closed = true; return nil }
func (c *devicesTestConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *devicesTestConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *devicesTestConn) SetDeadline(time.Time) error      { return nil }
func (c *devicesTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *devicesTestConn) SetWriteDeadline(time.Time) error { return nil }

type devicesLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *devicesLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *devicesLogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func (w *devicesLogBuf) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Len()
}

func (w *devicesLogBuf) Since(n int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.b.String()
	if n >= len(s) {
		return ""
	}
	return s[n:]
}
