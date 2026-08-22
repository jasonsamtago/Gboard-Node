package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
)

func TestGetConfig_Hy2區間字串必須整段留下(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocol":    "hysteria",
			"server_port": "26598-36598",
			"version":     2,
		})
	}))
	t.Cleanup(ts.Close)

	cfg, err := NewClient(config.PanelConfig{URL: ts.URL, Token: "t", NodeID: 1}).GetConfig()
	if err != nil {
		t.Fatalf("區間 server_port 必須能被收下: %v", err)
	}
	if cfg.ServerPort != 26598 {
		t.Errorf("start: got %d, want 26598", cfg.ServerPort)
	}
	if cfg.ServerPortRange != "26598-36598" {
		t.Errorf("range 被丟掉或收成只剩起點: got %q", cfg.ServerPortRange)
	}
}

func TestGetConfig_單端口不得長出區間(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocol":    "hysteria",
			"server_port": 443,
			"version":     2,
		})
	}))
	t.Cleanup(ts.Close)

	cfg, err := NewClient(config.PanelConfig{URL: ts.URL, Token: "t", NodeID: 1}).GetConfig()
	if err != nil {
		t.Fatalf("單端口: %v", err)
	}
	if cfg.ServerPort != 443 {
		t.Errorf("port: got %d, want 443", cfg.ServerPort)
	}
	if cfg.ServerPortRange != "" {
		t.Errorf("單端口不得長出區間: %q", cfg.ServerPortRange)
	}
}
