package panel

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Official cedar2025/Xboard-Node #61：machine／node 連面板 WS 失敗，
// log 為 `dial: malformed ws or wss URL`。
//
// Hypothesis（以碼為準，不是修補）：handshake 的 ws_url 原樣進
// NewWSClient／Dialer。gorilla 只接受 ws／wss；面板常下發 http(s)、
// 缺 scheme、//host、尾斜線。不准關 WS、不准改 REST 當修。
//
// 本檔只寫會紅的測：畸形 URL 必須正規成可 dial 的 ws／wss，且
// 連線路徑真的用正規化結果連上（https→wss 以 TLS dial 為證）。

type wsUpgradeRecorder struct {
	mu       sync.Mutex
	upgraded int
	host     string
	path     string
	rawQuery string
}

func (r *wsUpgradeRecorder) note(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upgraded++
	r.host = req.Host
	r.path = req.URL.Path
	r.rawQuery = req.URL.RawQuery
}

func (r *wsUpgradeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.upgraded
}

func startRecordedWSServer(t *testing.T, tlsMode bool, requireMachine bool) (*httptest.Server, *wsUpgradeRecorder) {
	t.Helper()
	rec := &wsUpgradeRecorder{}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		rec.note(r)
		defer conn.Close()
		_ = conn.WriteJSON(wsMessage{Event: "auth.success"})
		for {
			var msg wsMessage
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
		}
	})
	if tlsMode {
		return httptest.NewTLSServer(handler), rec
	}
	return httptest.NewServer(handler), rec
}

func malformedWSURLCases(serverURL string) []struct {
	name string
	raw  string
} {
	u, err := url.Parse(serverURL)
	if err != nil {
		panic(err)
	}
	host := u.Host
	return []struct {
		name string
		raw  string
	}{
		{name: "http_scheme", raw: "http://" + host},
		{name: "http_trailing_slash", raw: "http://" + host + "/"},
		{name: "http_path_trailing_slash", raw: "http://" + host + "/ws/"},
		{name: "missing_scheme_host_path", raw: host + "/ws"},
		{name: "protocol_relative", raw: "//" + host + "/ws"},
		{name: "protocol_relative_trailing_slash", raw: "//" + host + "/"},
	}
}

func waitWSConnected(t *testing.T, ws *WSClient, errCh <-chan error) {
	t.Helper()
	deadline := time.After(1500 * time.Millisecond)
	for !ws.IsConnected() {
		select {
		case err := <-errCh:
			t.Fatalf("畸形 ws_url 正規化後必須成功 dial 並連上，got %v", err)
		case <-deadline:
			t.Fatal("等待 WS 連線逾時：畸形 ws_url 沒被正規成可 dial 的 ws／wss（官方 #61）")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestWSClient_MalformedHTTPLikeURLMustNormalizeAndDial(t *testing.T) {
	server, rec := startRecordedWSServer(t, false, false)
	defer server.Close()

	for _, tc := range malformedWSURLCases(server.URL) {
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

			waitWSConnected(t, ws, errCh)
			if rec.count() <= before {
				t.Fatal("連線路徑沒打到正規化後的 ws 端點（官方 #61）")
			}
			if !strings.Contains(rec.rawQuery, "token=test-token") || !strings.Contains(rec.rawQuery, "node_id=18") {
				t.Fatalf("WS handshake query 不對: %q", rec.rawQuery)
			}
		})
	}
}

func TestWSClient_HTTPSURLMustBecomeWSSAndDialTLS(t *testing.T) {
	server, _ := startRecordedWSServer(t, true, false)
	defer server.Close()

	// handshake 常見形：https://host/ 尾斜線。必須變成 wss:// 再 TLS dial。
	raw := strings.TrimRight(server.URL, "/") + "/"
	if !strings.HasPrefix(raw, "https://") {
		t.Fatalf("fixture URL 必須是 https，got %q", raw)
	}

	ws := NewWSClient(raw, "test-token", 18, WSClientConfig{
		HandshakeTimeout: time.Second,
	}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := ws.connect(ctx)
	if ws.IsConnected() {
		return
	}
	if err == nil {
		t.Fatal("https ws_url 沒連上且沒有錯誤")
	}
	if isMalformedWSURLError(err) {
		t.Fatalf("https:// 必須正規成 wss://，gorilla 才不會報 malformed（官方 #61）: %v", err)
	}
	if !isWSSDialAttempt(err) {
		t.Fatalf("https:// 必須走 wss TLS dial（憑證錯誤也算有走 wss），got %v", err)
	}
}

func TestWSClient_MachineMuxMalformedURLMustNormalizeAndDial(t *testing.T) {
	server, rec := startRecordedWSServer(t, false, true)
	defer server.Close()

	raw := "http://" + mustHost(server.URL) + "/"
	ws := NewWSClient(raw, "machine-token", 0, WSClientConfig{
		HandshakeTimeout: time.Second,
		MachineID:        9,
	}, func(WSEvent) {}, nil, func() map[string]interface{} { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- ws.connect(ctx) }()

	waitWSConnected(t, ws, errCh)
	if rec.count() == 0 {
		t.Fatal("machine mux 連線路徑沒打到正規化後的 ws 端點（官方 #61）")
	}
	if !strings.Contains(rec.rawQuery, "machine_id=9") {
		t.Fatalf("machine mux 必須帶 machine_id，got %q", rec.rawQuery)
	}
}

func isMalformedWSURLError(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "malformed ws or wss URL")
}

func isWSSDialAttempt(err error) bool {
	if err == nil {
		return true
	}
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "x509") ||
		strings.Contains(s, "certificate") ||
		strings.Contains(s, "tls:")
}

func mustHost(serverURL string) string {
	u, err := url.Parse(serverURL)
	if err != nil {
		panic(err)
	}
	return u.Host
}
