package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jasonsamtago/Gboard-Node/internal/config"
)

// Official cedar2025/Xboard-Node #13：WS 已開仍 `dial: unexpected EOF`／
// `bad handshake`。官方 log 硬連 `ws://host:8076`（無 path）。
//
// Hypothesis（以碼為準）：單節點 Initial／Discover 不跟 handshake
// 下發的 ws_url（/ws、/custom、非 8076 port），硬連 :8076 或丟 path。
// 經 #61 正規化後的 host／path／port 必須原樣 dial 並完成 upgrade。
// 不准關 WS、不准退 REST（Push == nil）、不准把路徑改回 :8076。

type issue13PanelHit struct {
	host string
	path string
	port string
}

type issue13PanelLog struct {
	mu   sync.Mutex
	hits []issue13PanelHit
}

func (l *issue13PanelLog) note(req *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	host, port := issue13SplitHostPort(req.Host)
	l.hits = append(l.hits, issue13PanelHit{host: host, path: req.URL.Path, port: port})
}

func (l *issue13PanelLog) last() (issue13PanelHit, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) == 0 {
		return issue13PanelHit{}, false
	}
	return l.hits[len(l.hits)-1], true
}

func issue13SplitHostPort(hostport string) (string, string) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, ""
	}
	return h, p
}

// startIssue13SingleNodeServers：handshake 在 panel HTTP；WS 只在
// exactPath upgrade。/ 與其他 path 空關 → unexpected EOF。
// handshake 下發的 ws_url 指向 WS 端（可與 panel 不同 port）。
func startIssue13SingleNodeServers(t *testing.T, exactPath string, issue func(wsBase string) string) (panelURL string, rec *issue13PanelLog) {
	t.Helper()
	rec = &issue13PanelLog{}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	wsMux := http.NewServeMux()
	wsMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != exactPath {
			if hj, ok := w.(http.Hijacker); ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
				}
			}
			return
		}
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "bad handshake", http.StatusBadRequest)
			return
		}
		if r.URL.Query().Get("token") == "" || r.URL.Query().Get("node_id") == "" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		rec.note(r)
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"event": "auth.success"})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	wsServer := httptest.NewServer(wsMux)
	t.Cleanup(wsServer.Close)

	issued := issue(wsServer.URL)
	panelMux := http.NewServeMux()
	panelMux.HandleFunc("/api/v2/server/handshake", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"websocket": map[string]any{
				"enabled": true,
				"ws_url":  issued,
			},
			"settings": map[string]any{"push_interval": 60, "pull_interval": 60},
		})
	})
	panelMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// panel HTTP 不是 WS；打錯這裡要能對上官方 EOF／bad handshake。
		if websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "bad handshake", http.StatusBadRequest)
			return
		}
		http.NotFound(w, r)
	})
	panelServer := httptest.NewServer(panelMux)
	t.Cleanup(panelServer.Close)
	return panelServer.URL, rec
}

func issue13WSURLFromBase(wsBase, path, scheme string) string {
	u, err := url.Parse(wsBase)
	if err != nil {
		panic(err)
	}
	switch scheme {
	case "http":
		return "http://" + u.Host + path
	case "ws":
		return "ws://" + u.Host + path
	case "bare":
		return u.Host + path
	case "proto":
		return "//" + u.Host + path
	default:
		return "ws://" + u.Host + path
	}
}

func issue13SingleNodeCases() []struct {
	name string
	path string
	how  string
} {
	return []struct {
		name string
		path string
		how  string
	}{
		{name: "ws_path_ws", path: "/ws", how: "ws"},
		{name: "http_path_ws", path: "/ws", how: "http"},
		{name: "ws_custom_path", path: "/custom", how: "ws"},
		{name: "http_custom_path", path: "/custom", how: "http"},
		{name: "missing_scheme_host_path", path: "/ws", how: "bare"},
		{name: "protocol_relative_ws", path: "/ws", how: "proto"},
	}
}

func issue13AssertPanelHit(t *testing.T, issued string, hit issue13PanelHit) {
	t.Helper()
	raw := issued
	if !strings.Contains(raw, "://") {
		if strings.HasPrefix(raw, "//") {
			raw = "ws:" + raw
		} else {
			raw = "ws://" + raw
		}
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("issued ws_url 不可解析: %q %v", issued, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		// #61 正規化後 scheme 變 ws／wss，host／path／port 必須留下。
	}
	if u.Hostname() != hit.host {
		t.Fatalf("單節點 dial host 必須等於 handshake：got %q want %q（issued=%q）。不准硬連 :8076", hit.host, u.Hostname(), issued)
	}
	if u.Path != hit.path {
		t.Fatalf("單節點 dial path 必須等於 handshake：got %q want %q（issued=%q）。丟 path → unexpected EOF／bad handshake", hit.path, u.Path, issued)
	}
	if u.Port() != "" && u.Port() != hit.port {
		t.Fatalf("單節點 dial port 必須等於 handshake：got %q want %q。不准改回 :8076", hit.port, u.Port())
	}
	if u.Port() == "" && hit.port == "8076" {
		t.Fatalf("handshake 沒給 :8076，不准硬連 :8076（issued=%q hit=%+v）", issued, hit)
	}
}

func waitIssue13Push(t *testing.T, push PushClient) {
	t.Helper()
	deadline := time.After(1500 * time.Millisecond)
	for !push.IsConnected() {
		select {
		case <-deadline:
			t.Fatal("單節點沒跟 handshake host／path／port 完成 upgrade（官方 #13 unexpected EOF／bad handshake／硬連 :8076）。不准改 REST")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestPanelControlPlane_InitialHandshakeURLMustDialHostPathPort(t *testing.T) {
	for _, tc := range issue13SingleNodeCases() {
		t.Run("initial/"+tc.name, func(t *testing.T) {
			var issued string
			panelURL, rec := startIssue13SingleNodeServers(t, tc.path, func(wsBase string) string {
				issued = issue13WSURLFromBase(wsBase, tc.path, tc.how)
				return issued
			})
			cp := NewPanelControlPlane(
				config.PanelConfig{URL: panelURL, Token: "node-token", NodeID: 18},
				config.WSConfig{HandshakeTimeout: 2, BackoffInitial: 1, BackoffMax: 2},
				config.KernelConfig{},
			)
			events := make(chan Event, 4)
			statuses := make(chan StatusChange, 4)
			boot, err := cp.Initial(context.Background(), func() map[string]interface{} { return nil }, events, statuses)
			if err != nil {
				t.Fatalf("handshake Initial: %v", err)
			}
			if boot.Push == nil {
				t.Fatal("handshake 已開 WS，不准退回 REST（Push == nil）")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			go boot.Push.Run(ctx)
			waitIssue13Push(t, boot.Push)
			hit, ok := rec.last()
			if !ok {
				t.Fatal("單節點 Initial 沒在 handshake path 完成 upgrade（官方 #13）。不准改回 :8076")
			}
			issue13AssertPanelHit(t, issued, hit)
		})
	}
}

func TestPanelControlPlane_DiscoverHandshakeURLMustDialHostPathPort(t *testing.T) {
	for _, tc := range issue13SingleNodeCases() {
		t.Run("discover/"+tc.name, func(t *testing.T) {
			var issued string
			panelURL, rec := startIssue13SingleNodeServers(t, tc.path, func(wsBase string) string {
				issued = issue13WSURLFromBase(wsBase, tc.path, tc.how)
				return issued
			})
			cp := NewPanelControlPlane(
				config.PanelConfig{URL: panelURL, Token: "node-token", NodeID: 18},
				config.WSConfig{HandshakeTimeout: 2, BackoffInitial: 1, BackoffMax: 2},
				config.KernelConfig{},
			)
			events := make(chan Event, 4)
			statuses := make(chan StatusChange, 4)
			push, err := cp.Discover(context.Background(), func() map[string]interface{} { return nil }, events, statuses)
			if err != nil {
				t.Fatalf("handshake Discover: %v", err)
			}
			if push == nil {
				t.Fatal("handshake 已開 WS，不准 Discover 回 nil 改走 REST")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			go push.Run(ctx)
			waitIssue13Push(t, push)
			hit, ok := rec.last()
			if !ok {
				t.Fatal("單節點 Discover 沒在 handshake path 完成 upgrade（官方 #13）。不准改回 :8076")
			}
			issue13AssertPanelHit(t, issued, hit)
		})
	}
}
