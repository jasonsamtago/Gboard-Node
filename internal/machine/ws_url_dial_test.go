package machine

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
// Hypothesis（以碼為準）：machine mux tryStartWS 不跟 handshake
// 下發的 ws_url（/ws、/custom、非 8076 port），硬連 :8076 或丟 path。
// 經 #61 正規化後的 host／path／port 必須原樣 dial 並完成 upgrade。
// 不准關 WS、不准 REST only（o.ws == nil）、不准把路徑改回 :8076。

type issue13MachineHit struct {
	host string
	path string
	port string
}

type issue13MachineLog struct {
	mu   sync.Mutex
	hits []issue13MachineHit
}

func (l *issue13MachineLog) note(req *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	host, port := issue13SplitHostPort(req.Host)
	l.hits = append(l.hits, issue13MachineHit{host: host, path: req.URL.Path, port: port})
}

func (l *issue13MachineLog) last() (issue13MachineHit, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) == 0 {
		return issue13MachineHit{}, false
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

func startIssue13MachineServers(t *testing.T, exactPath string, issue func(wsBase string) string) (panelURL string, rec *issue13MachineLog) {
	t.Helper()
	rec = &issue13MachineLog{}
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
	panelMux.HandleFunc("/api/v2/server/machine/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nodes":       []any{},
			"base_config": map[string]any{"push_interval": 60, "pull_interval": 60},
		})
	})
	panelMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
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

func issue13MachineWSURL(wsBase, path, scheme string) string {
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

func issue13MachineCases() []struct {
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

func issue13AssertMachineHit(t *testing.T, issued string, hit issue13MachineHit) {
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
	if u.Hostname() != hit.host {
		t.Fatalf("machine mux dial host 必須等於 handshake：got %q want %q（issued=%q）。不准硬連 :8076", hit.host, u.Hostname(), issued)
	}
	if u.Path != hit.path {
		t.Fatalf("machine mux dial path 必須等於 handshake：got %q want %q。丟 path → unexpected EOF／bad handshake", hit.path, u.Path)
	}
	if u.Port() != "" && u.Port() != hit.port {
		t.Fatalf("machine mux dial port 必須等於 handshake：got %q want %q。不准改回 :8076", hit.port, u.Port())
	}
	if u.Port() == "" && hit.port == "8076" {
		t.Fatalf("handshake 沒給 :8076，不准硬連 :8076（issued=%q hit=%+v）", issued, hit)
	}
}

func waitIssue13MachineWS(t *testing.T, o *Orchestrator) {
	t.Helper()
	deadline := time.After(1500 * time.Millisecond)
	for o.ws == nil || !o.ws.IsConnected() {
		select {
		case <-deadline:
			t.Fatal("machine mux 沒跟 handshake host／path／port 完成 upgrade（官方 #13 unexpected EOF／bad handshake／硬連 :8076）。不准 REST only")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestOrchestrator_TryStartWS_HandshakeURLMustDialHostPathPort(t *testing.T) {
	for _, tc := range issue13MachineCases() {
		t.Run(tc.name, func(t *testing.T) {
			var issued string
			panelURL, rec := startIssue13MachineServers(t, tc.path, func(wsBase string) string {
				issued = issue13MachineWSURL(wsBase, tc.path, tc.how)
				return issued
			})
			o := New(&config.Config{
				Panel:   config.PanelConfig{URL: panelURL},
				Machine: &config.MachineConfig{MachineID: 9, Token: "machine-token"},
				WS:      config.WSConfig{HandshakeTimeout: 2, BackoffInitial: 1, BackoffMax: 2},
			})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			o.tryStartWS(ctx)
			if o.ws == nil {
				t.Fatal("handshake 已開 WS，不准走 REST only（o.ws == nil）")
			}
			waitIssue13MachineWS(t, o)
			hit, ok := rec.last()
			if !ok {
				t.Fatal("machine mux 沒在 handshake path 完成 upgrade（官方 #13）。不准改回 :8076")
			}
			issue13AssertMachineHit(t, issued, hit)
		})
	}
}
