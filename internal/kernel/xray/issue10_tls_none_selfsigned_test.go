package xray

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/cert"
	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
)

// Official cedar2025/Xboard-Node #10 xray 基線：
// 同配置 tls=1 + cert_mode=none + vmess+ws，xray 必須仍能起核。
// 官方現場切 kernel=xray 可跑；這份鎖定回歸綠，不准為了修 sing-box 把 xray 弄壞。

const (
	issue10OfficialOpenSelfSigned = "open self-signed: no such file or directory"
	issue10UserID                 = 50
	issue10UUID                   = "50505050-5050-4505-8505-505050505050"
)

func TestIssue10_Xray_TLS1_CertModeNone_MustStillStart(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)

	nc := issue10OfficialPanelNode(t)
	if nc.TLS != 1 || nc.Network != "ws" || nc.Protocol != "vmess" {
		t.Fatalf("官方形狀被改掉：protocol=%q network=%q tls=%d", nc.Protocol, nc.Network, nc.TLS)
	}
	if nc.CertConfig == nil || nc.CertConfig.CertMode != "none" {
		t.Fatalf("cert_config 必須是 cert_mode=none，got %#v", nc.CertConfig)
	}

	spec := issue10ValidatedSpec(t, nc)
	tls := issue10ApplyPanelCertNone(t, spec)
	if tls.HasCert() {
		t.Fatal("cert_mode=none 不得本機簽／載入證書才起")
	}

	cfg := buildConfig(issue10XrayKernelConfig(), spec, issue10Users(), tls)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal xray config: %v", err)
	}
	if issue10ConfigReferencesSelfSignedPath(raw) {
		t.Fatalf("xray 建 inbound 不得引用不存在的 self-signed 路徑：\n%s", raw)
	}

	k := New(issue10XrayKernelConfig())
	if k.Name() != "xray" {
		t.Fatalf("xray 基線不准改切別的核：Name()=%q", k.Name())
	}
	err = k.Start(spec, issue10Users(), tls)
	if err != nil {
		if strings.Contains(err.Error(), issue10OfficialOpenSelfSigned) {
			t.Fatalf("xray 同配置回歸不得也去 open self-signed：%v", err)
		}
		t.Fatalf("官方 #10 同配置 xray 基線必須能起核：%v", err)
	}
	t.Cleanup(k.Stop)
	if !k.IsRunning() {
		t.Fatal("xray Start 沒回錯但 kernel 沒在跑")
	}
	if err := issue10TCPPing(fmt.Sprintf("127.0.0.1:%d", spec.ServerPort)); err != nil {
		t.Fatalf("xray 必須真的聽在 inbound 端口：%v", err)
	}
}

func issue10OfficialPanelJSON(port int) map[string]any {
	return map[string]any{
		"protocol":    "vmess",
		"listen_ip":   "0.0.0.0",
		"server_port": port,
		"network":     "ws",
		"networkSettings": map[string]any{
			"path": "/",
			"headers": map[string]any{
				"Host": "cdn.example.com",
			},
		},
		"tls": 1,
		"cert_config": map[string]any{
			"email":     nil,
			"domain":    nil,
			"cert_mode": "none",
			"http_port": nil,
		},
		"base_config": map[string]any{
			"push_interval": 60,
			"pull_interval": 60,
		},
	}
}

func issue10OfficialPanelNode(t *testing.T) *panel.NodeConfig {
	t.Helper()
	port := issue10FreeTCPPort(t)
	payload := issue10OfficialPanelJSON(port)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)
	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "issue10",
		NodeID: issue10UserID,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("官方面板必須能被 GetConfig 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	return cfg
}

func issue10ValidatedSpec(t *testing.T, nc *panel.NodeConfig) *model.NodeSpec {
	t.Helper()
	spec, err := model.NodeSpecFromPanelValidated(nc, issue10XrayKernelConfig())
	if err != nil {
		t.Fatalf("官方形狀必須過 NodeSpecFromPanelValidated：%v", err)
	}
	spec.ListenIP = "127.0.0.1"
	spec.CustomRoutes = []map[string]any{{
		"type":        "field",
		"ip":          []string{"127.0.0.0/8"},
		"outboundTag": "direct",
	}}
	return spec
}

func issue10ApplyPanelCertNone(t *testing.T, spec *model.NodeSpec) kernel.TLSCert {
	t.Helper()
	dir := t.TempDir()
	cfg := *spec.CertConfig
	cfg.CertDir = dir
	mgr := cert.NewManager(cfg)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("cert_mode=none cert manager Start：%v", err)
	}
	t.Cleanup(mgr.Stop)
	if _, err := os.Stat(filepath.Join(dir, "self-signed")); err == nil {
		t.Fatal("cert_mode=none 不得寫出 self-signed 檔")
	}
	return mgr.TLSCert()
}

func issue10ConfigReferencesSelfSignedPath(raw []byte) bool {
	lower := bytes.ToLower(raw)
	for _, key := range [][]byte{
		[]byte(`"certificatefile"`),
		[]byte(`"keyfile"`),
		[]byte(`"certificate_path"`),
		[]byte(`"key_path"`),
	} {
		idx := bytes.Index(lower, key)
		if idx < 0 {
			continue
		}
		window := lower[idx:]
		if len(window) > 80 {
			window = window[:80]
		}
		if bytes.Contains(window, []byte("self-signed")) {
			return true
		}
	}
	return false
}

func issue10Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue10UserID, UUID: issue10UUID}}
}

func issue10XrayKernelConfig() config.KernelConfig {
	return config.KernelConfig{Type: "xray", LogLevel: "warning"}
}

func issue10FreeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func issue10TCPPing(addr string) error {
	deadline := time.Now().Add(3 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	return last
}
