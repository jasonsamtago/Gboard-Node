package machine

import (
	"context"
	"encoding/json"
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

// Official cedar2025/Xboard-Node #61：machine mux handshake 下發畸形
// ws_url 時，tryStartWS 必須正規成可 dial 的 ws／wss 並真連上。
// 不准關 WS、不准「REST only」當修（o.ws == nil 或永遠斷線）。
// https→wss 的 gorilla／TLS dial 契約鎖在 panel.WSClient 測試。

type machineWSRecorder struct {
	mu       sync.Mutex
	upgraded int
	rawQuery string
}

func (r *machineWSRecorder) note(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upgraded++
	r.rawQuery = req.URL.RawQuery
}

func (r *machineWSRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.upgraded
}

func startMachineHandshakeWSServer(t *testing.T, malform func(panelURL string) string) (*httptest.Server, *machineWSRecorder) {
	t.Helper()
	rec := &machineWSRecorder{}
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
	mux.HandleFunc("/api/v2/server/machine/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodes":       []any{},
			"base_config": map[string]any{"push_interval": 60, "pull_interval": 60},
		})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("token") == "" || r.URL.Query().Get("machine_id") == "" {
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

func machineMalformedWSURL() []struct {
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

func waitMachineWSConnected(t *testing.T, o *Orchestrator) {
	t.Helper()
	deadline := time.After(1500 * time.Millisecond)
	for o.ws == nil || !o.ws.IsConnected() {
		select {
		case <-deadline:
			t.Fatal("machine mux 畸形 ws_url 沒被正規成可 dial 的 ws／wss 並連上（官方 #61）。不准 REST only")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestOrchestrator_TryStartWS_MalformedHandshakeURLMustDial(t *testing.T) {
	for _, tc := range machineMalformedWSURL() {
		t.Run(tc.name, func(t *testing.T) {
			server, rec := startMachineHandshakeWSServer(t, tc.malform)
			defer server.Close()

			o := New(&config.Config{
				Panel:   config.PanelConfig{URL: server.URL},
				Machine: &config.MachineConfig{MachineID: 9, Token: "machine-token"},
				WS:      config.WSConfig{HandshakeTimeout: 2, BackoffInitial: 1, BackoffMax: 2},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			o.tryStartWS(ctx)
			if o.ws == nil {
				t.Fatal("handshake 已開 WS，不准走 REST only（o.ws == nil）")
			}
			waitMachineWSConnected(t, o)
			if rec.count() == 0 {
				t.Fatal("machine mux 連線路徑沒使用正規化後的 ws_url（官方 #61）")
			}
			if !strings.Contains(rec.rawQuery, "machine_id=9") {
				t.Fatalf("machine mux 必須帶 machine_id，got %q", rec.rawQuery)
			}
		})
	}
}
