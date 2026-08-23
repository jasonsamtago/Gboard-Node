package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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

// Official cedar2025/Xboard-Node #10：sinbox 模式下 tls 证书 bug。
//
// 面板 tls=1 + cert_config.cert_mode=none（nginx 前置終止 TLS），
// vmess+ws。sing-box 起核失敗：
//
//	create sing-box instance: initialize inbound[0]:
//	read certificate: open self-signed: no such file or directory
//
// 切 kernel=xray 可跑——但審核鎖不准靠強制改切 xray 當修。
//
// 審核鎖：
//  1. tls=1 + cert_mode=none（或等價「由前置 nginx 終止 TLS」）時，
//     sing-box 必須能起核；不得 open self-signed: no such file or directory。
//  2. 同配置 xray 基線仍要通（見 xray 包對應測試）。
//  3. 不准：關 TLS、拆 ws、改成必須本機簽證書才起、強制改切 xray 當修。
//  4. 測的是真正 start kernel／build inbound 證書路徑，不是只檢查 JSON 有無欄位。
//
// 這份只鎖失敗行為，不實作修正。

const (
	issue10OfficialOpenSelfSigned = "open self-signed: no such file or directory"
	issue10OfficialReadCert       = "read certificate"
	issue10OfficialCreateInstance = "create sing-box instance"
	issue10UserID                 = 50
	issue10UUID                   = "50505050-5050-4505-8505-505050505050"
)

func TestIssue10_SingBox_TLS1_CertModeNone_MustStartWithoutOpeningSelfSigned(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)

	nc := issue10OfficialPanelNode(t)
	issue10RejectPanelFakeFixes(t, nc, "sing-box")

	spec := issue10ValidatedSpec(t, nc)
	issue10RejectSpecFakeFixes(t, spec, "sing-box")

	tls := issue10ApplyPanelCertNone(t, spec)
	if tls.HasCert() {
		t.Fatal("cert_mode=none 不得本機簽／載入證書才起（不准改成必須本機簽證書當修）")
	}

	cfg := buildConfig(issue10SingBoxKernelConfig(), spec, issue10Users(), tls)
	issue10AssertBuiltConfigDoesNotOpenSelfSigned(t, cfg)

	k := New(issue10SingBoxKernelConfig())
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}

	err := k.Start(spec, issue10Users(), tls)
	if err != nil {
		msg := err.Error()
		if issue10IsOfficialSelfSignedOpen(msg) {
			t.Fatalf("官方 #10：tls=1＋cert_mode=none（nginx 終止 TLS）sing-box 起核不得再 open self-signed：%v", err)
		}
		t.Fatalf("官方 #10：sing-box Start／create instance 必須能起來（tls=1＋cert_mode=none＋vmess+ws）：%v", err)
	}
	t.Cleanup(k.Stop)
	if !k.IsRunning() {
		t.Fatal("Start 沒回錯但 kernel 沒在跑：略過起核或改切別的核都是假修")
	}

	addr := fmt.Sprintf("127.0.0.1:%d", spec.ServerPort)
	if err := issue10TCPPing(addr); err != nil {
		t.Fatalf("官方 #10：sing-box 必須真的聽在 inbound 端口，不得只建 JSON：%v", err)
	}

	issue10AssertInboundCertPathHandlesCertModeNone(t)
}

func TestIssue10_SingBox_CreateInstance_MustNotReadSelfSignedPath(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)

	spec := issue10ValidatedSpec(t, issue10OfficialPanelNode(t))
	tls := issue10ApplyPanelCertNone(t, spec)

	raw := issue10MarshalConfig(t, spec, tls)
	if issue10ConfigReferencesSelfSignedPath(raw) {
		t.Fatalf("建 inbound／config 仍引用不存在的 self-signed 路徑，sing-box create instance 會 open 掛掉：\n%s", raw)
	}

	k := New(issue10SingBoxKernelConfig())
	err := k.Start(spec, issue10Users(), tls)
	if err != nil && issue10IsOfficialSelfSignedOpen(err.Error()) {
		t.Fatalf("create sing-box instance 仍去 open self-signed：%v\nconfig=%s", err, raw)
	}
	if err != nil {
		t.Fatalf("create sing-box instance／Start 必須成功：%v\nconfig=%s", err, raw)
	}
	t.Cleanup(k.Stop)
	if !k.IsRunning() {
		t.Fatal("create instance 後 kernel 沒在跑")
	}

	issue10AssertInboundCertPathHandlesCertModeNone(t)
}

// issue10AssertInboundCertPathHandlesCertModeNone 鎖的是真正建 inbound／
// create instance 的證書路徑，不是只看產出 JSON 有沒有 tls 欄位。
// 現況 buildTLSConfig／buildVMess 只看 PEM，沒看 cert_mode=none；
// 官方 #10 就是這條路把 certificate_path 設成 self-signed，
// box.New 才會 read certificate: open self-signed: no such file or directory。
func issue10AssertInboundCertPathHandlesCertModeNone(t *testing.T) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "config.go", nil, 0)
	if err != nil {
		t.Fatalf("parse config.go: %v", err)
	}
	var buildTLSSeesCertMode bool
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "buildTLSConfig" || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			ident, ok := inner.(*ast.Ident)
			if ok && ident.Name == "CertMode" {
				buildTLSSeesCertMode = true
			}
			return true
		})
		return true
	})
	if !buildTLSSeesCertMode {
		t.Fatalf("官方 #10：buildTLSConfig／建 inbound 證書路徑沒處理 cert_mode=none。現況 tls=1 仍可能把 certificate_path 設成 self-signed，create sing-box instance 會 %s。不准關 TLS、不准拆 ws、不准改成必須本機簽證書、不准改切 xray",
			issue10OfficialOpenSelfSigned)
	}
}

func issue10OfficialPanelJSON(port int) map[string]any {
	// 官方面板形狀（Xboard-Node #10）：tls=1、cert_mode=none、vmess+ws。
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
		t.Fatalf("官方面板 tls=1＋cert_mode=none 必須能被 GetConfig 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	return cfg
}

func issue10ValidatedSpec(t *testing.T, nc *panel.NodeConfig) *model.NodeSpec {
	t.Helper()
	spec, err := model.NodeSpecFromPanelValidated(nc, issue10SingBoxKernelConfig())
	if err != nil {
		t.Fatalf("官方 #10 面板形狀必須過 NodeSpecFromPanelValidated：%v", err)
	}
	if spec == nil {
		t.Fatal("NodeSpecFromPanelValidated 回傳 nil")
	}
	spec.ListenIP = "127.0.0.1"
	spec.CustomRoutes = []map[string]any{{
		"outbound": "direct",
		"ip_cidr":  []string{"127.0.0.0/8"},
	}}
	return spec
}

func issue10ApplyPanelCertNone(t *testing.T, spec *model.NodeSpec) kernel.TLSCert {
	t.Helper()
	if spec.CertConfig == nil {
		t.Fatal("關掉／丟掉 cert_config 當修：面板有 cert_mode=none")
	}
	mode := strings.ToLower(strings.TrimSpace(spec.CertConfig.CertMode))
	if mode != "none" {
		t.Fatalf("cert_mode=%q，要 none（nginx 前置終止 TLS）", spec.CertConfig.CertMode)
	}

	dir := t.TempDir()
	cfg := *spec.CertConfig
	cfg.CertDir = dir
	mgr := cert.NewManager(cfg)
	if err := mgr.Start(context.Background()); err != nil {
		t.Fatalf("cert_mode=none 的 cert manager 必須能 Start：%v", err)
	}
	t.Cleanup(mgr.Stop)

	changed, err := mgr.Reconfigure(context.Background(), cfg)
	if err != nil {
		t.Fatalf("套用面板 cert_mode=none 不得錯：%v", err)
	}
	_ = changed

	if _, err := os.Stat(filepath.Join(dir, "self-signed")); err == nil {
		t.Fatal("cert_mode=none 不得寫出 self-signed 檔再叫 kernel 去 open")
	}
	return mgr.TLSCert()
}

func issue10RejectPanelFakeFixes(t *testing.T, nc *panel.NodeConfig, kernelName string) {
	t.Helper()
	var mode string
	if nc.CertConfig != nil {
		mode = nc.CertConfig.CertMode
	}
	issue10RejectLockFields(t, kernelName, nc.Protocol, nc.Network, nc.TLS, mode, nc.CertConfig != nil)
}

func issue10RejectSpecFakeFixes(t *testing.T, spec *model.NodeSpec, kernelName string) {
	t.Helper()
	var mode string
	if spec.CertConfig != nil {
		mode = spec.CertConfig.CertMode
	}
	issue10RejectLockFields(t, kernelName, spec.Protocol, spec.Network, spec.TLS, mode, spec.CertConfig != nil)
}

func issue10RejectLockFields(t *testing.T, kernelName, protocol, network string, tls int, certMode string, hasCertCfg bool) {
	t.Helper()
	if !strings.EqualFold(protocol, "vmess") {
		t.Fatalf("%s 不准拆掉 VMess：protocol=%q", kernelName, protocol)
	}
	if !strings.EqualFold(network, "ws") {
		t.Fatalf("%s 不准拆 ws：network=%q", kernelName, network)
	}
	if tls != 1 {
		t.Fatalf("%s 不准關 TLS：tls=%d，面板是 tls=1（nginx 終止，不是把 tls 改 0）", kernelName, tls)
	}
	if !hasCertCfg || !strings.EqualFold(strings.TrimSpace(certMode), "none") {
		t.Fatalf("%s 不准改成必須本機簽證書：cert_mode=%q，要 none", kernelName, certMode)
	}
}

func issue10AssertBuiltConfigDoesNotOpenSelfSigned(t *testing.T, cfg M) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal sing-box config: %v", err)
	}
	if issue10ConfigReferencesSelfSignedPath(raw) {
		t.Fatalf("建 inbound 仍引用不存在的 self-signed 路徑（官方 #10 會 open 掛掉）：\n%s", raw)
	}
	inbounds, _ := cfg["inbounds"].([]M)
	if len(inbounds) == 0 {
		t.Fatal("不准拆 inbound 當修：config 沒有 inbounds")
	}
	in := inbounds[0]
	if got, _ := in["type"].(string); !strings.EqualFold(got, "vmess") {
		t.Fatalf("不准拆 VMess：inbound type=%q", got)
	}
	transport, _ := in["transport"].(M)
	if transport == nil {
		t.Fatal("不准拆 ws：inbound 沒有 transport")
	}
	if got, _ := transport["type"].(string); !strings.EqualFold(got, "ws") {
		t.Fatalf("不准拆 ws：transport.type=%q", got)
	}
}

func issue10MarshalConfig(t *testing.T, spec *model.NodeSpec, tls kernel.TLSCert) []byte {
	t.Helper()
	cfg := buildConfig(issue10SingBoxKernelConfig(), spec, issue10Users(), tls)
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func issue10ConfigReferencesSelfSignedPath(raw []byte) bool {
	lower := bytes.ToLower(raw)
	if !bytes.Contains(lower, []byte("self-signed")) {
		return false
	}
	// 路徑欄位才算「會去 open」；註解／log 不算。
	for _, key := range []string{
		`"certificate_path"`,
		`"key_path"`,
		`"certificate_file"`,
		`"key_file"`,
	} {
		idx := bytes.Index(lower, []byte(key))
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
	return bytes.Contains(lower, []byte(`"certificate_path": "self-signed"`)) ||
		bytes.Contains(lower, []byte(`"certificate_path":"self-signed"`)) ||
		bytes.Contains(lower, []byte(`"key_path": "self-signed"`)) ||
		bytes.Contains(lower, []byte(`"key_path":"self-signed"`))
}

func issue10IsOfficialSelfSignedOpen(msg string) bool {
	return strings.Contains(msg, issue10OfficialOpenSelfSigned) ||
		(strings.Contains(msg, issue10OfficialReadCert) && strings.Contains(msg, "self-signed")) ||
		(strings.Contains(msg, issue10OfficialCreateInstance) && strings.Contains(msg, "self-signed"))
}

func issue10Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue10UserID, UUID: issue10UUID}}
}

func issue10SingBoxKernelConfig() config.KernelConfig {
	return config.KernelConfig{Type: "singbox", LogLevel: "warn"}
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
