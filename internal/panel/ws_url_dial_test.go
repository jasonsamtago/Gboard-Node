package panel

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Official cedar2025/Xboard-Node #13：WS 已開仍 `dial: unexpected EOF`／
// `bad handshake`。官方 log 連的是 `ws://127.0.0.1:8076`（無 path），
// 反代下發的是 `ws(s)://host/ws` 或自訂 path。
//
// Hypothesis（以碼為準，不是修補）：node 不跟 handshake 下發的
// ws_url（含 /ws、wss 反代、非 8076 port），硬連 :8076 或丟 path。
// 經 #61 parseWSURL 正規化後的 host／path／port 必須原樣 dial 並完成
// websocket upgrade。不准關 WS、不准退 REST、不准把路徑改回 :8076。
// Trojan SNI 證書不在本票。

const issue13LegacyWSPort = "8076"

type issue13UpgradeHit struct {
	host     string
	path     string
	rawQuery string
}

type issue13UpgradeLog struct {
	mu   sync.Mutex
	hits []issue13UpgradeHit
}

func (l *issue13UpgradeLog) note(req *http.Request) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.hits = append(l.hits, issue13UpgradeHit{
		host:     req.Host,
		path:     req.URL.Path,
		rawQuery: req.URL.RawQuery,
	})
}

func (l *issue13UpgradeLog) last() (issue13UpgradeHit, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) == 0 {
		return issue13UpgradeHit{}, false
	}
	return l.hits[len(l.hits)-1], true
}

func (l *issue13UpgradeLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}

// startIssue13PathOnlyWS 只在 exactPath 做 websocket upgrade。
// 其他 path（含 /）立刻關連線，對上官方 unexpected EOF／bad handshake。
func startIssue13PathOnlyWS(t *testing.T, exactPath string, requireMachine bool) (*httptest.Server, *issue13UpgradeLog) {
	t.Helper()
	if exactPath == "" || exactPath == "/" {
		t.Fatal("fixture path 必須是 handshake 下發的反代 path，不准用 /")
	}
	log := &issue13UpgradeLog{}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != exactPath {
			// 丟 path 或打到 / 時，模擬反代沒升級：對端空關 → unexpected EOF。
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.NotFound(w, r)
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				return
			}
			_ = conn.Close()
			return
		}
		if !websocket.IsWebSocketUpgrade(r) {
			http.Error(w, "bad handshake", http.StatusBadRequest)
			return
		}
		q := r.URL.Query()
		if q.Get("token") == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		if requireMachine {
			if q.Get("machine_id") == "" {
				http.Error(w, "missing machine_id", http.StatusUnauthorized)
				return
			}
		} else if q.Get("node_id") == "" {
			http.Error(w, "missing node_id", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("upgrade error: %v", err)
			return
		}
		log.note(r)
		defer conn.Close()
		_ = conn.WriteJSON(wsMessage{Event: "auth.success"})
		for {
			if err := conn.ReadJSON(&wsMessage{}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server, log
}

// startIssue13Legacy8076Decoy 聽 127.0.0.1:8076，連上就關，對上官方
// `dial: unexpected EOF`。硬連 :8076 會打中這裡，不得算 upgrade 成功。
func startIssue13Legacy8076Decoy(t *testing.T) (hits *int32, ok bool) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:"+issue13LegacyWSPort)
	if err != nil {
		t.Logf("無法綁 :%s decoy（可能已被占用），改用 path 斷言抓硬連: %v", issue13LegacyWSPort, err)
		return nil, false
	}
	var n int32
	hits = &n
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(hits, 1)
			// 空關，gorilla 會看到 unexpected EOF／bad handshake。
			_ = c.Close()
		}
	}()
	return hits, true
}

func issue13NormalizedDialTarget(raw string) (host, path, port string) {
	u, err := parseWSURL(raw)
	if err != nil {
		return "", "", ""
	}
	host = u.Hostname()
	path = u.Path
	port = u.Port()
	return host, path, port
}

func issue13AssertUpgradeMatchesHandshake(t *testing.T, raw string, hit issue13UpgradeHit) {
	t.Helper()
	wantHost, wantPath, wantPort := issue13NormalizedDialTarget(raw)
	if wantHost == "" || wantPath == "" {
		t.Fatalf("handshake ws_url 正規化後必須有 host+path，got host=%q path=%q raw=%q", wantHost, wantPath, raw)
	}
	if wantPath == "/" {
		t.Fatalf("本票 handshake 必須帶反代 path，不准正規化成 /: %q", raw)
	}
	gotHost, gotPort := splitHostPort(hit.host)
	if gotHost != wantHost {
		t.Fatalf("dial host 必須等於正規化後的 handshake host：got %q want %q（raw=%q）。不准硬連 :%s", gotHost, wantHost, raw, issue13LegacyWSPort)
	}
	if wantPort != "" && gotPort != wantPort {
		t.Fatalf("dial port 必須等於正規化後的 handshake port：got %q want %q（raw=%q）。不准改回 :%s", gotPort, wantPort, raw, issue13LegacyWSPort)
	}
	if wantPort == "" && gotPort == issue13LegacyWSPort {
		t.Fatalf("handshake 沒給 :%s，不准硬連 :%s（官方 #13）: raw=%q hit.host=%q", issue13LegacyWSPort, issue13LegacyWSPort, raw, hit.host)
	}
	if hit.path != wantPath {
		t.Fatalf("dial path 必須等於正規化後的 handshake path：got %q want %q（raw=%q）。丟 path 會打到 / 或 :%s → unexpected EOF／bad handshake", hit.path, wantPath, raw, issue13LegacyWSPort)
	}
}

func splitHostPort(hostport string) (host, port string) {
	h, p, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, ""
	}
	return h, p
}

func waitIssue13WSConnected(t *testing.T, ws *WSClient, errCh <-chan error) {
	t.Helper()
	deadline := time.After(1500 * time.Millisecond)
	for !ws.IsConnected() {
		select {
		case err := <-errCh:
			if err != nil {
				t.Fatalf("handshake ws_url（經 #61 正規化）必須原樣 dial 並 upgrade，got %v（官方 #13 unexpected EOF／bad handshake／硬連 :%s）", err, issue13LegacyWSPort)
			}
		case <-deadline:
			t.Fatalf("等待 WS upgrade 逾時：沒跟 handshake host／path／port，或硬連 :%s（官方 #13）。不准關 WS／退 REST", issue13LegacyWSPort)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestWSClient_HandshakeURLHostPathPortMustDialAndUpgrade(t *testing.T) {
	decoyHits, decoyOK := startIssue13Legacy8076Decoy(t)
	t.Cleanup(func() {
		if decoyOK && decoyHits != nil && *decoyHits > 0 {
			t.Errorf("硬連 :%s decoy %d 次（官方 #13 unexpected EOF）。必須跟 handshake path／port", issue13LegacyWSPort, *decoyHits)
		}
	})

	for _, path := range []string{"/ws", "/custom"} {
		t.Run("path"+path, func(t *testing.T) {
			server, rec := startIssue13PathOnlyWS(t, path, false)
			host := mustHost(server.URL)
			raws := []struct {
				name string
				raw  string
			}{
				{name: "ws_scheme", raw: "ws://" + host + path},
				{name: "http_scheme", raw: "http://" + host + path},
			}
			for _, tc := range raws {
				t.Run(tc.name, func(t *testing.T) {
					before := rec.count()
					ws := NewWSClient(tc.raw, "test-token", 18, WSClientConfig{
						HandshakeTimeout: time.Second,
						BackoffInitial:   time.Second,
						BackoffMax:       time.Second,
					}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					errCh := make(chan error, 1)
					go func() { errCh <- ws.connect(ctx) }()

					waitIssue13WSConnected(t, ws, errCh)
					if rec.count() <= before {
						t.Fatal("沒打到 handshake 下發的 path 做 upgrade（官方 #13）。不准改回 :8076")
					}
					hit, ok := rec.last()
					if !ok {
						t.Fatal("upgrade 沒留下 request")
					}
					issue13AssertUpgradeMatchesHandshake(t, tc.raw, hit)
					if !strings.Contains(hit.rawQuery, "token=test-token") || !strings.Contains(hit.rawQuery, "node_id=18") {
						t.Fatalf("WS handshake query 不對: %q", hit.rawQuery)
					}
				})
			}
		})
	}
}

func TestWSClient_HandshakeURLNon8076PortMustDialThatPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	if addr.Port == 8076 {
		_ = ln.Close()
		t.Fatal("測試 port 碰巧是 8076，換一個")
	}

	rec := &issue13UpgradeLog{}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ws" {
			hj, ok := w.(http.Hijacker)
			if ok {
				if c, _, err := hj.Hijack(); err == nil {
					_ = c.Close()
				}
			}
			return
		}
		if r.URL.Query().Get("token") == "" || r.URL.Query().Get("node_id") == "" {
			http.Error(w, "missing auth", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		rec.note(r)
		defer conn.Close()
		_ = conn.WriteJSON(wsMessage{Event: "auth.success"})
		for {
			if err := conn.ReadJSON(&wsMessage{}); err != nil {
				return
			}
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	raw := "ws://" + addr.String() + "/ws"
	ws := NewWSClient(raw, "test-token", 18, WSClientConfig{
		HandshakeTimeout: time.Second,
	}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- ws.connect(ctx) }()
	waitIssue13WSConnected(t, ws, errCh)

	hit, ok := rec.last()
	if !ok {
		t.Fatalf("非 8076 port 的 handshake URL 必須完成 upgrade，got 0 hits。不准硬連 :8076（官方 #13）")
	}
	issue13AssertUpgradeMatchesHandshake(t, raw, hit)
}

func TestWSClient_MachineMuxHandshakeURLMustKeepPath(t *testing.T) {
	server, rec := startIssue13PathOnlyWS(t, "/ws", true)
	raw := "http://" + mustHost(server.URL) + "/ws"
	ws := NewWSClient(raw, "machine-token", 0, WSClientConfig{
		HandshakeTimeout: time.Second,
		MachineID:        9,
	}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- ws.connect(ctx) }()
	waitIssue13WSConnected(t, ws, errCh)

	hit, ok := rec.last()
	if !ok {
		t.Fatal("machine mux 必須跟 handshake path 完成 upgrade（官方 #13）。不准 REST only、不准 :8076")
	}
	issue13AssertUpgradeMatchesHandshake(t, raw, hit)
	if !strings.Contains(hit.rawQuery, "machine_id=9") {
		t.Fatalf("machine mux 必須帶 machine_id，got %q", hit.rawQuery)
	}
}

func TestParseWSURL_MustNotRewriteHandshakeTo8076(t *testing.T) {
	cases := []struct {
		raw      string
		wantHost string
		wantPath string
		wantPort string
	}{
		{raw: "ws://panel.example.com/ws", wantHost: "panel.example.com", wantPath: "/ws"},
		{raw: "wss://panel.example.com/custom", wantHost: "panel.example.com", wantPath: "/custom"},
		{raw: "ws://panel.example.com:9443/ws", wantHost: "panel.example.com", wantPath: "/ws", wantPort: "9443"},
		{raw: "http://panel.example.com/ws", wantHost: "panel.example.com", wantPath: "/ws"},
		{raw: "https://ws.example.com:8443/custom", wantHost: "ws.example.com", wantPath: "/custom", wantPort: "8443"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			u, err := parseWSURL(tc.raw)
			if err != nil {
				t.Fatalf("parseWSURL(%q): %v", tc.raw, err)
			}
			if u.Hostname() != tc.wantHost {
				t.Fatalf("host: got %q want %q", u.Hostname(), tc.wantHost)
			}
			if u.Path != tc.wantPath {
				t.Fatalf("path: got %q want %q（丟 path 會對上官方 #13 unexpected EOF／bad handshake）", u.Path, tc.wantPath)
			}
			if u.Port() != tc.wantPort {
				t.Fatalf("port: got %q want %q。不准改回 :8076", u.Port(), tc.wantPort)
			}
			if !strings.Contains(tc.raw, "8076") && strings.Contains(u.Host, "8076") {
				t.Fatalf("handshake 沒給 :8076，parseWSURL 不准寫入 :8076：got %q", u.Host)
			}
		})
	}
}

func TestWSClient_WrongPathMustSurfaceOfficialEOF(t *testing.T) {
	// 對照組：若實作丟 path 打到 /，fixture 會空關，錯誤必須能對上官方現象。
	server, rec := startIssue13PathOnlyWS(t, "/ws", false)
	ws := NewWSClient("ws://"+mustHost(server.URL)+"/", "test-token", 18, WSClientConfig{
		HandshakeTimeout: time.Second,
	}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := ws.connect(ctx)
	if rec.count() != 0 {
		t.Fatal("打到 / 不得算 upgrade 成功")
	}
	if err == nil {
		t.Fatal("丟 path 打到 / 必須失敗")
	}
	s := err.Error()
	if !strings.Contains(s, "unexpected EOF") && !strings.Contains(s, "bad handshake") && !strings.Contains(s, "EOF") {
		t.Fatalf("對照組錯誤要能對上官方 #13（unexpected EOF／bad handshake），got %v", err)
	}
}
