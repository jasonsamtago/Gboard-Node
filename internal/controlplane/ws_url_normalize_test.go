package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jasonsamtago/Gboard-Node/internal/config"
)

// Official cedar2025/Xboard-Node #61：單節點 handshake 下發畸形
// ws_url 時，Initial／Discover 必須正規成可 dial 的 ws／wss 並真連上。
// 不准關 WS、不准退回 REST（boot.Push == nil）當修。
// https→wss 的 gorilla／TLS dial 契約鎖在 panel.WSClient 測試。

type panelWSRecorder struct {
	mu       sync.Mutex
	upgraded int
	rawQuery string
}

func (r *panelWSRecorder) note(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upgraded++
	r.rawQuery = req.URL.RawQuery
}

func (r *panelWSRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.upgraded
}

func startSingleNodeHandshakeWSServer(t *testing.T, malform func(panelURL string) string) (*httptest.Server, *panelWSRecorder) {
	t.Helper()
	rec := &panelWSRecorder{}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	var panelURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/server/handshake", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"websocket": map[string]any{
				"enabled": true,
				"ws_url":  malform(panelURL),
			},
			"settings": map[string]any{"push_interval": 60, "pull_interval": 60},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.NotFound(w, r)
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
	server := httptest.NewServer(mux)
	panelURL = server.URL
	return server, rec
}

func singleNodeMalformedWSURL(panelURL string) []struct {
	name    string
	malform func(string) string
} {
	hostOf := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			panic(err)
		}
		return u.Host
	}
	return []struct {
		name    string
		malform func(string) string
	}{
		{name: "http_scheme", malform: func(u string) string { return "http://" + hostOf(u) }},
		{name: "http_trailing_slash", malform: func(u string) string { return "http://" + hostOf(u) + "/" }},
		{name: "missing_scheme_host_path", malform: func(u string) string { return hostOf(u) + "/ws" }},
		{name: "protocol_relative", malform: func(u string) string { return "//" + hostOf(u) + "/ws" }},
	}
}

func waitPushConnected(t *testing.T, push PushClient) {
	t.Helper()
	deadline := time.After(1500 * time.Millisecond)
	for !push.IsConnected() {
		select {
		case <-deadline:
			t.Fatal("單節點畸形 ws_url 沒被正規成可 dial 的 ws／wss 並連上（官方 #61）。不准改 REST")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestPanelControlPlane_InitialMalformedWSURLMustDial(t *testing.T) {
	// 用同一個 server URL 形狀產案例名；每個 subtest 自建 server。
	for _, tc := range singleNodeMalformedWSURL("http://127.0.0.1:0") {
		t.Run("initial/"+tc.name, func(t *testing.T) {
			server, rec := startSingleNodeHandshakeWSServer(t, tc.malform)
			defer server.Close()

			cp := NewPanelControlPlane(
				config.PanelConfig{URL: server.URL, Token: "node-token", NodeID: 18},
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
			waitPushConnected(t, boot.Push)
			if rec.count() == 0 {
				t.Fatal("單節點 Initial 連線路徑沒使用正規化後的 ws_url（官方 #61）")
			}
		})
	}
}

func TestPanelControlPlane_DiscoverMalformedWSURLMustDial(t *testing.T) {
	for _, tc := range singleNodeMalformedWSURL("http://127.0.0.1:0") {
		t.Run("discover/"+tc.name, func(t *testing.T) {
			server, rec := startSingleNodeHandshakeWSServer(t, tc.malform)
			defer server.Close()

			cp := NewPanelControlPlane(
				config.PanelConfig{URL: server.URL, Token: "node-token", NodeID: 18},
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
			waitPushConnected(t, push)
			if rec.count() == 0 {
				t.Fatal("單節點 Discover 連線路徑沒使用正規化後的 ws_url（官方 #61）")
			}
		})
	}
}
