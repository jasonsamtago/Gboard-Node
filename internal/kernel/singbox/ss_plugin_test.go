package singbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
	shadowsocks "github.com/sagernet/sing-shadowsocks2"
	singM "github.com/sagernet/sing/common/metadata"
)

// 官方 cedar2025/Xboard-Node #41：Shadowsocks 不支持插件。
// 面板為 SS 節點設 v2ray-plugin／gost-plugin 後，sing-box 核印
//
//	WARN [core] sing-box shadowsocks inbound does not support plugin, ignoring plugin=v2ray-plugin
//	WARN [core] sing-box shadowsocks inbound does not support plugin, ignoring plugin=gost-plugin
//
// 審核 2 鎖定（本輪只加失敗測，不改 production）：
//  1. Happy：面板 plugin=v2ray-plugin／gost-plugin＋合理 opts 必須進
//     sing-box SS inbound（原欄位或等價字段），不得再出現 ignoring；
//     客戶端依插件握手／連線至少一條可驗證（WS upgrade＋SS AEAD 回聲）。
//  2. 邊界：無 plugin 的普通 SS 行為與現況一致。
//  3. 失敗：未知 plugin 必須明確錯誤（非靜默 ignoring）。
//     v2ray-plugin／gost-plugin 必須支援，不得走忽略。
//
// 不准當修：關插件、改成只支援無插件 SS、改切 xray、擴大 outbound 插件生態。

const (
	issue41IgnoreWarn = "sing-box shadowsocks inbound does not support plugin, ignoring"
	issue41UserID     = 41
	issue41UserUUID   = "41414141-4141-4141-4141-414141414141"
	issue41Host       = "ss-plugin.test"
	issue41V2RayPath  = "/v2ray"
	issue41GostPath   = "/gost"
	issue41V2RayOpts  = "mode=websocket;host=ss-plugin.test;path=/v2ray"
	issue41GostOpts   = "mode=ws;host=ss-plugin.test;path=/gost"
)

func TestIssue41_V2RayPlugin_MustKeepAndHandshake(t *testing.T) {
	runIssue41PluginHappy(t, "v2ray-plugin", issue41V2RayOpts, issue41V2RayPath)
}

func TestIssue41_GostPlugin_MustKeepAndHandshake(t *testing.T) {
	runIssue41PluginHappy(t, "gost-plugin", issue41GostOpts, issue41GostPath)
}

func TestIssue41_PlainSS_NoPlugin_MustStillHandshake(t *testing.T) {
	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, dest := startSingBoxHotDest(t)
	defer destLn.Close()

	nc := fetchIssue41PanelConfig(t, "", "")
	spec := issue41SpecFromPanel(t, nc)
	if spec.Plugin != "" || spec.PluginOpt != "" {
		t.Fatalf("無 plugin 回歸組不得自己帶 plugin：plugin=%q opts=%q", spec.Plugin, spec.PluginOpt)
	}

	k, err := startIssue41SingBox(t, spec)
	if err != nil {
		t.Fatalf("無 plugin 的普通 SS 必須能起 sing-box 核（不得回歸）: %v\nlog=\n%s", err, logs.String())
	}
	defer k.Stop()

	if strings.Contains(logs.String(), issue41IgnoreWarn) {
		t.Fatalf("無 plugin 不得印 ignoring：\n%s", logs.String())
	}

	raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", spec.ServerPort), 2*time.Second)
	if err != nil {
		t.Fatalf("無 plugin SS 必須聽埠: %v", err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(4 * time.Second))

	if err := handshakeIssue41Shadowsocks(raw, dest); err != nil {
		t.Fatalf("無 plugin 的普通 SS 必須仍能 AEAD 握手／回聲（不得回歸）: %v", err)
	}
}

func TestIssue41_UnknownPlugin_MustErrorNotIgnore(t *testing.T) {
	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	nc := fetchIssue41PanelConfig(t, "obfs-local-unknown", "obfs=http")
	spec := issue41SpecFromPanel(t, nc)
	if spec.Plugin != "obfs-local-unknown" {
		t.Fatalf("面板未知 plugin 必須進 NodeSpec，got %q", spec.Plugin)
	}

	k, err := startIssue41SingBox(t, spec)
	if k != nil {
		defer k.Stop()
	}
	logText := logs.String()

	if strings.Contains(logText, issue41IgnoreWarn) {
		t.Errorf("未知 plugin 必須明確錯誤，不得官方 #41 式靜默 ignoring：\n%s", logText)
	}
	if err == nil {
		t.Error("未知／不支援的 plugin 起核必須回明確錯誤，不得當成功")
	} else if !strings.Contains(err.Error(), "obfs-local-unknown") {
		t.Errorf("錯誤必須點出 plugin 名稱 obfs-local-unknown，got %v", err)
	}
	if k != nil && k.IsRunning() {
		t.Fatal("拒絕未知 plugin 後核不得當成功在跑")
	}
}

func runIssue41PluginHappy(t *testing.T, plugin, opts, path string) {
	t.Helper()

	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, dest := startSingBoxHotDest(t)
	defer destLn.Close()

	nc := fetchIssue41PanelConfig(t, plugin, opts)
	if nc.Plugin != plugin || nc.PluginOpt != opts {
		t.Fatalf("關掉 plugin 當修：GetConfig 沒保留 plugin=%q opts=%q，got plugin=%q opts=%q",
			plugin, opts, nc.Plugin, nc.PluginOpt)
	}

	spec := issue41SpecFromPanel(t, nc)
	if spec.Plugin != plugin || spec.PluginOpt != opts {
		t.Fatalf("關掉 plugin 當修：NodeSpec 沒保留 plugin=%q opts=%q，got plugin=%q opts=%q",
			plugin, opts, spec.Plugin, spec.PluginOpt)
	}

	cfg := buildConfig(config.KernelConfig{Type: "singbox", LogLevel: "warn"}, spec, issue41Users(), kernel.TLSCert{})
	if !issue41ConfigKeepsPlugin(cfg, plugin, path, issue41Host) {
		raw, _ := json.Marshal(cfg)
		t.Errorf("sing-box SS inbound 必須保留 plugin=%s（或等價字段含 path=%s／host），不得丟棄：\n%s",
			plugin, path, raw)
	}

	k, err := startIssue41SingBox(t, spec)
	if err != nil {
		t.Fatalf("官方 #41：%s 必須能起 sing-box 核（不准關插件、不准改切 xray）: %v\nlog=\n%s",
			plugin, err, logs.String())
	}
	defer k.Stop()

	logText := logs.String()
	if strings.Contains(logText, issue41IgnoreWarn) {
		t.Errorf("官方 #41：不得再出現 %q plugin=%s：\n%s", issue41IgnoreWarn, plugin, logText)
	}
	if strings.Contains(logText, "plugin="+plugin) && strings.Contains(logText, "ignoring") {
		t.Errorf("官方 #41：plugin=%s 被忽略：\n%s", plugin, logText)
	}

	if err := handshakeIssue41Plugin(t, spec.ServerPort, path, dest); err != nil {
		t.Fatalf("官方 #41：%s 必須完成插件握手／連線（WS upgrade＋SS AEAD 回聲），got %v\nlog=\n%s",
			plugin, err, logText)
	}
}

func fetchIssue41PanelConfig(t *testing.T, plugin, opts string) *panel.NodeConfig {
	t.Helper()
	port := sbFreeTCPPort(t)
	payload := map[string]any{
		"protocol":    "shadowsocks",
		"cipher":      "aes-128-gcm",
		"kernel_type": "singbox",
		"server_port": port,
		"listen_ip":   "127.0.0.1",
	}
	if plugin != "" {
		payload["plugin"] = plugin
		payload["plugin_opts"] = opts
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)

	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "issue41",
		NodeID: 41,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("面板下發 SS plugin 必須能被 node 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	if cfg.Protocol != "shadowsocks" {
		t.Fatalf("protocol=%q，要 shadowsocks", cfg.Protocol)
	}
	return cfg
}

func issue41SpecFromPanel(t *testing.T, nc *panel.NodeConfig) *model.NodeSpec {
	t.Helper()
	spec := model.NodeSpecFromPanel(nc)
	if spec == nil {
		t.Fatal("NodeSpecFromPanel 回傳 nil")
	}
	spec.CustomRoutes = []map[string]any{{
		"ip_cidr":  []string{"127.0.0.0/8"},
		"outbound": "direct",
	}}
	return spec
}

func startIssue41SingBox(t *testing.T, spec *model.NodeSpec) (*SingBox, error) {
	t.Helper()
	k := New(config.KernelConfig{Type: "singbox", LogLevel: "warn"})
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}
	err := k.Start(spec, issue41Users(), kernel.TLSCert{})
	if err != nil {
		return k, err
	}
	if !k.IsRunning() {
		return k, fmt.Errorf("Start 沒回錯但 kernel 沒在跑")
	}
	sbWaitTCP(t, fmt.Sprintf("127.0.0.1:%d", spec.ServerPort))
	return k, nil
}

func issue41Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue41UserID, UUID: issue41UserUUID}}
}

func issue41ConfigKeepsPlugin(cfg M, plugin, path, host string) bool {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return false
	}
	s := string(raw)
	if strings.Contains(s, plugin) && (strings.Contains(s, path) || strings.Contains(s, "plugin_opts")) {
		return true
	}
	hasWS := strings.Contains(s, `"ws"`) || strings.Contains(s, "websocket")
	if hasWS && strings.Contains(s, path) && strings.Contains(s, host) {
		return true
	}
	return false
}

func handshakeIssue41Plugin(t *testing.T, port int, path string, dest *net.TCPAddr) error {
	t.Helper()
	dialer := websocket.Dialer{
		HandshakeTimeout: 2 * time.Second,
		NetDial: func(network, addr string) (net.Conn, error) {
			return net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		},
	}
	ws, resp, err := dialer.Dial(fmt.Sprintf("ws://%s:%d%s", issue41Host, port, path), nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		return fmt.Errorf("插件 WS 握手失敗（官方 #41 現況是 plain SS、ignoring plugin）: %w status=%d", err, status)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(4 * time.Second))
	_ = ws.SetWriteDeadline(time.Now().Add(4 * time.Second))
	return handshakeIssue41Shadowsocks(&issue41WSConn{Conn: ws}, dest)
}

func handshakeIssue41Shadowsocks(raw net.Conn, dest *net.TCPAddr) error {
	method, err := shadowsocks.CreateMethod(context.Background(), "aes-128-gcm", shadowsocks.MethodOptions{
		Password: issue41UserUUID,
	})
	if err != nil {
		return fmt.Errorf("ss method: %w", err)
	}
	ss, err := method.DialConn(raw, singM.SocksaddrFromNet(dest))
	if err != nil {
		return fmt.Errorf("ss dial: %w", err)
	}
	if _, err := ss.Write([]byte("ping")); err != nil {
		return fmt.Errorf("ss write: %w", err)
	}
	got := make([]byte, len(sbHotPayload))
	if _, err := io.ReadFull(ss, got); err != nil {
		return fmt.Errorf("ss read: %w", err)
	}
	if string(got) != sbHotPayload {
		return fmt.Errorf("ss payload=%q want %q", got, sbHotPayload)
	}
	return nil
}

type issue41WSConn struct {
	*websocket.Conn
	reader io.Reader
}

func (c *issue41WSConn) Read(p []byte) (int, error) {
	for {
		if c.reader == nil {
			_, r, err := c.NextReader()
			if err != nil {
				return 0, err
			}
			c.reader = r
		}
		n, err := c.reader.Read(p)
		if err == io.EOF {
			c.reader = nil
			if n == 0 {
				continue
			}
			return n, nil
		}
		return n, err
	}
}

func (c *issue41WSConn) Write(p []byte) (int, error) {
	w, err := c.NextWriter(websocket.BinaryMessage)
	if err != nil {
		return 0, err
	}
	n, err := w.Write(p)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return n, err
}

func (c *issue41WSConn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}
