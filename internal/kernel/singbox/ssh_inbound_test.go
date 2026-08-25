package singbox

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
	"golang.org/x/crypto/ssh"
)

// 官方 cedar2025/Xboard-Node #66：SSH protocol for sing-box。
// 面板選 protocol=ssh（sing-box）並帶 user／認證後，核必須真帶
// inbound type=ssh，配置埠在聽，SSH 客戶端能握手。
//
// 審核 2 鎖定（本輪只加失敗測，不改 production）：
//  1. Happy：protocol=ssh + user／password → inbound type=ssh；
//     埠在聽；SSH 客戶端能握手。不得靜默忽略／當未知丟掉。
//  2. 邊界：既有 VMess／SS／VLESS 行為與現況一致。
//  3. 失敗：忽略 SSH、Start 成功但不聽、只 WARN 不建 inbound → 必須紅。
//  4. 再補邊界：帶 server_key（private key）仍須是 SSH inbound，
//     不得丟掉或改寫成 shadowsocks。
//  5. 再補失敗：缺認證必須明確錯誤，不得 Start 成功當空核。
//
// 不准當修：關 SSH、改切 xray、只印 WARN、把 SSH 當 shadowsocks plugin、
// 順便做票上其他 new features。

const (
	issue66UserID      = 66
	issue66UserUUID    = "66666666-6666-4666-8666-666666666666"
	issue66SSIgnore    = "sing-box shadowsocks inbound does not support plugin, ignoring"
	issue66Payload     = "ISSUE66-PONG"
	issue66SSHBanner   = "SSH-2.0"
	issue66Unsupported = "unsupported protocol"
)

func TestIssue66_SSH_MustBuildInboundListenAndHandshake(t *testing.T) {
	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	destLn, dest := startIssue66Dest(t)
	defer destLn.Close()

	nc := fetchIssue66PanelConfig(t, issue66UserUUID, "")
	if !strings.EqualFold(nc.Protocol, "ssh") {
		t.Fatalf("關掉 SSH 當修：GetConfig 沒保留 protocol=ssh，got %q", nc.Protocol)
	}

	spec := issue66SpecFromPanel(t, nc)
	if !strings.EqualFold(spec.Protocol, "ssh") {
		t.Fatalf("關掉 SSH 當修：NodeSpec 沒保留 protocol=ssh，got %q", spec.Protocol)
	}

	k := New(config.KernelConfig{Type: "singbox", LogLevel: "warn"})
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}
	if !issue66ProtocolsHasSSH(k.Protocols()) {
		t.Errorf("官方 #66：Protocols() 必須含 ssh，否則 validateNodeRuntime 會當不支援丟掉：%v", k.Protocols())
	}

	cfg := buildConfig(config.KernelConfig{Type: "singbox", LogLevel: "warn"}, spec, issue66Users(), kernel.TLSCert{})
	inbound := issue66FirstInbound(cfg)
	raw, _ := json.Marshal(inbound)
	if inbound == nil {
		t.Errorf("官方 #66：buildConfig 必須帶 SSH inbound，不得因未知協議省略 inbounds（現況 buildInbound default 回 nil）")
	}
	if !issue66InboundIsSSH(inbound) {
		t.Errorf("官方 #66：inbound 必須 type=ssh（或官方等價），不得靜默忽略／當未知丟掉：\n%s", raw)
	}
	if issue66InboundIsShadowsocks(inbound) {
		t.Errorf("官方 #66：不得把 SSH 冒充成 shadowsocks：\n%s", raw)
	}
	if inbound != nil && !strings.Contains(string(raw), issue66UserUUID) {
		t.Errorf("官方 #66：user／認證必須進 SSH inbound，不得丟 UUID：\n%s", raw)
	}

	started, err := startIssue66SingBox(t, spec, issue66Users())
	if started != nil {
		defer started.Stop()
	}
	logText := logs.String()
	if strings.Contains(logText, issue66SSIgnore) {
		t.Errorf("官方 #66：不得把 SSH 當 shadowsocks plugin ignoring：\n%s", logText)
	}
	if strings.Contains(strings.ToLower(logText), "ignoring") && strings.Contains(strings.ToLower(logText), "ssh") {
		t.Errorf("官方 #66：不得只印 WARN／ignoring 而不建 inbound：\n%s", logText)
	}
	if err != nil {
		t.Fatalf("官方 #66：protocol=ssh 必須能起 sing-box 核（不准關 SSH、不准改切 xray）: %v\nlog=\n%s",
			err, logText)
	}
	if started == nil || !started.IsRunning() {
		t.Fatal("官方 #66：Start 沒回錯但 kernel 沒在跑")
	}

	if !issue66WaitTCP(spec.ServerPort) {
		t.Fatalf("官方 #66：Start 後配置埠 %d 必須真在聽，不准 process 活著卻不建 inbound running=%v\nlog=\n%s\ninbound=%s",
			spec.ServerPort, started.IsRunning(), logText, raw)
	}

	banner, bannerErr := issue66PeekBanner(spec.ServerPort)
	if bannerErr != nil || !strings.HasPrefix(banner, issue66SSHBanner) {
		t.Fatalf("官方 #66：聽埠後必須是 SSH 握手橫幅 %s，不得是其他協議／空連：banner=%q err=%v\nlog=\n%s",
			issue66SSHBanner, banner, bannerErr, logText)
	}

	if err := handshakeIssue66SSH(t, spec.ServerPort, dest, issue66UserUUID, issue66UserUUID); err != nil {
		t.Fatalf("官方 #66：SSH 客戶端必須完成握手／連線（現況若忽略 SSH／不聽埠會紅），got %v\nlog=\n%s\ninbound=%s",
			err, logText, raw)
	}
}

func TestIssue66_ExistingVMessSSVLESS_MustStillHandshake(t *testing.T) {
	t.Run("vless", func(t *testing.T) {
		k, port, dest, _, stop := startSingBoxVLESS(t, issue66Users())
		defer stop()
		if err := trySingBoxVLESS(t, port, issue66UserUUID, dest); err != nil {
			t.Fatalf("既有 VLESS 必須仍能握手／回聲（不得回歸）: %v", err)
		}
		if k.Name() != "sing-box" {
			t.Fatalf("不准改切 xray 當修：Name()=%q", k.Name())
		}
	})

	t.Run("vmess", func(t *testing.T) {
		logs := &sbLockedLogBuf{}
		nlog.Init(logs, slog.LevelDebug, false)
		destLn, dest := startIssue31Dest(t)
		defer destLn.Close()

		spec := issue31SpecFromPanel(t, fetchIssue31PanelConfig(t, "tcp", nil))
		if spec.Protocol != "vmess" {
			t.Fatalf("VMess 回歸組必須是 vmess，got %q", spec.Protocol)
		}
		k, err := startIssue31SingBox(t, spec)
		if err != nil {
			t.Fatalf("既有 VMess TCP 必須能起核（不得回歸）: %v\nlog=\n%s", err, logs.String())
		}
		defer k.Stop()
		if err := handshakeIssue31VMessTCP(t, spec.ServerPort, dest); err != nil {
			t.Fatalf("既有 VMess 必須仍能握手／回聲（不得回歸）: %v", err)
		}
	})

	t.Run("shadowsocks", func(t *testing.T) {
		logs := &sbLockedLogBuf{}
		nlog.Init(logs, slog.LevelDebug, false)
		destLn, dest := startSingBoxHotDest(t)
		defer destLn.Close()

		spec := issue41SpecFromPanel(t, fetchIssue41PanelConfig(t, "", ""))
		if spec.Protocol != "shadowsocks" {
			t.Fatalf("SS 回歸組必須是 shadowsocks，got %q", spec.Protocol)
		}
		k, err := startIssue41SingBox(t, spec)
		if err != nil {
			t.Fatalf("既有 SS 必須能起核（不得回歸）: %v\nlog=\n%s", err, logs.String())
		}
		defer k.Stop()

		raw, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", spec.ServerPort), 2*time.Second)
		if err != nil {
			t.Fatalf("既有 SS 必須聽埠: %v", err)
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(4 * time.Second))
		if err := handshakeIssue41Shadowsocks(raw, dest); err != nil {
			t.Fatalf("既有 SS 必須仍能 AEAD 握手／回聲（不得回歸）: %v", err)
		}
	})
}

func TestIssue66_SSH_PrivateKey_StillSSHInbound(t *testing.T) {
	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	hostKey := issue66ServerKeyPEM(t)
	nc := fetchIssue66PanelConfig(t, issue66UserUUID, hostKey)
	if strings.TrimSpace(nc.ServerKey) == "" {
		t.Fatalf("關掉 private key 當修：GetConfig 沒保留 server_key")
	}
	spec := issue66SpecFromPanel(t, nc)
	if strings.TrimSpace(spec.ServerKey) == "" {
		t.Fatalf("關掉 private key 當修：NodeSpec 沒保留 ServerKey")
	}

	inbound := buildInbound(spec, issue66Users(), kernel.TLSCert{})
	raw, _ := json.Marshal(inbound)
	if inbound == nil || !issue66InboundIsSSH(inbound) {
		t.Fatalf("帶 private key 的 SSH 仍須產出 inbound type=ssh，不得丟掉／當未知忽略：\n%s", raw)
	}
	if issue66InboundIsShadowsocks(inbound) {
		t.Fatalf("帶 private key 不得把 SSH 改寫成 shadowsocks：\n%s", raw)
	}
	if !issue66InboundKeepsKey(inbound, hostKey) {
		t.Fatalf("server_key／private key 必須進 SSH inbound（host_key／private_key 或官方等價），不得丟：\n%s", raw)
	}

	k, err := startIssue66SingBox(t, spec, issue66Users())
	if k != nil {
		defer k.Stop()
	}
	if strings.Contains(logs.String(), issue66SSIgnore) {
		t.Errorf("帶 private key 不得走 shadowsocks plugin ignoring：\n%s", logs.String())
	}
	if err != nil {
		t.Fatalf("帶 private key 的 SSH 必須仍能起核: %v\nlog=\n%s", err, logs.String())
	}
}

func TestIssue66_SSH_MissingAuth_MustErrorNotSilent(t *testing.T) {
	logs := &sbLockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	nc := fetchIssue66PanelConfig(t, "", "")
	spec := issue66SpecFromPanel(t, nc)
	if strings.TrimSpace(spec.ServerKey) != "" {
		t.Fatalf("缺認證組不得自己補 ServerKey：%q", spec.ServerKey)
	}

	k, err := startIssue66SingBox(t, spec, nil)
	if k != nil {
		defer k.Stop()
	}
	logText := logs.String()

	if strings.Contains(logText, issue66SSIgnore) {
		t.Errorf("缺認證不得走 shadowsocks plugin ignoring：\n%s", logText)
	}
	if err == nil {
		t.Error("官方 #66：缺 user／password／private key 起核必須回明確錯誤，不得 Start 成功當空核")
	} else {
		msg := strings.ToLower(err.Error())
		if !strings.Contains(msg, "ssh") && !strings.Contains(msg, "auth") &&
			!strings.Contains(msg, "user") && !strings.Contains(msg, "password") &&
			!strings.Contains(msg, "key") {
			t.Errorf("缺認證錯誤必須點出 ssh／認證，got %v", err)
		}
		if strings.Contains(msg, issue66Unsupported) && !strings.Contains(msg, "auth") {
			t.Errorf("缺認證必須是認證錯誤，不得只回 unsupported 把 SSH 當不支援丟掉：%v", err)
		}
	}
	if k != nil && k.IsRunning() {
		t.Fatal("拒絕缺認證後核不得當成功在跑（Start 成功但不聽／空 inbound 不算修）")
	}
}

func fetchIssue66PanelConfig(t *testing.T, password, serverKey string) *panel.NodeConfig {
	t.Helper()
	port := sbFreeTCPPort(t)
	payload := map[string]any{
		"protocol":    "ssh",
		"kernel_type": "singbox",
		"server_port": port,
		"listen_ip":   "127.0.0.1",
	}
	if password != "" {
		payload["password"] = password
	}
	if serverKey != "" {
		payload["server_key"] = serverKey
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)

	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "issue66",
		NodeID: 66,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("面板下發 SSH 必須能被 node 收下：%v", err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	if cfg.Protocol != "ssh" {
		t.Fatalf("protocol=%q，要 ssh", cfg.Protocol)
	}
	return cfg
}

func issue66SpecFromPanel(t *testing.T, nc *panel.NodeConfig) *model.NodeSpec {
	t.Helper()
	spec := model.NodeSpecFromPanel(nc)
	if spec == nil {
		t.Fatal("NodeSpecFromPanel 回傳 nil：SSH 被關掉了")
	}
	spec.CustomRoutes = []map[string]any{{
		"ip_cidr":  []string{"127.0.0.0/8"},
		"outbound": "direct",
	}}
	return spec
}

func startIssue66SingBox(t *testing.T, spec *model.NodeSpec, users []model.UserSpec) (*SingBox, error) {
	t.Helper()
	k := New(config.KernelConfig{Type: "singbox", LogLevel: "warn"})
	if k.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：Name()=%q，要 sing-box", k.Name())
	}
	err := k.Start(spec, users, kernel.TLSCert{})
	if err != nil {
		return k, err
	}
	if !k.IsRunning() {
		return k, fmt.Errorf("Start 沒回錯但 kernel 沒在跑")
	}
	return k, nil
}

func issue66Users() []model.UserSpec {
	return []model.UserSpec{{ID: issue66UserID, UUID: issue66UserUUID}}
}

func issue66ProtocolsHasSSH(protocols []string) bool {
	for _, p := range protocols {
		if strings.EqualFold(p, "ssh") {
			return true
		}
	}
	return false
}

func issue66FirstInbound(cfg M) M {
	inbounds, _ := cfg["inbounds"].([]M)
	if len(inbounds) == 0 {
		return nil
	}
	return inbounds[0]
}

func issue66InboundIsSSH(inbound M) bool {
	if inbound == nil {
		return false
	}
	return strings.EqualFold(fmt.Sprint(inbound["type"]), "ssh")
}

func issue66InboundIsShadowsocks(inbound M) bool {
	if inbound == nil {
		return false
	}
	return strings.EqualFold(fmt.Sprint(inbound["type"]), "shadowsocks")
}

func issue66InboundKeepsKey(inbound M, hostKey string) bool {
	if inbound == nil || strings.TrimSpace(hostKey) == "" {
		return false
	}
	raw, err := json.Marshal(inbound)
	if err != nil {
		return false
	}
	s := string(raw)
	if strings.Contains(s, hostKey) {
		return true
	}
	// 官方等價：host_key／private_key／server_key 任一欄有金鑰本體或 OpenSSH 標記。
	hasField := strings.Contains(s, "host_key") || strings.Contains(s, "private_key") || strings.Contains(s, "server_key")
	hasPEM := strings.Contains(s, "BEGIN") && (strings.Contains(s, "PRIVATE KEY") || strings.Contains(s, "OPENSSH"))
	return hasField && hasPEM
}

func issue66ServerKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ssh host key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(key, "issue66")
	if err != nil {
		t.Fatalf("marshal ssh host key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

func issue66WaitTCP(port int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 150*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return false
}

func issue66PeekBanner(port int) (string, error) {
	c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if n == 0 && err != nil {
		return "", err
	}
	return strings.TrimSpace(string(buf[:n])), nil
}

func handshakeIssue66SSH(t *testing.T, nodePort int, dest *net.TCPAddr, user, password string) error {
	t.Helper()
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}
	client, err := ssh.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", nodePort), cfg)
	if err != nil {
		return fmt.Errorf("SSH 握手失敗（現況多半是沒 inbound／沒聽埠／不是 SSH）: %w", err)
	}
	defer client.Close()

	conn, err := client.Dial("tcp", dest.String())
	if err != nil {
		return fmt.Errorf("SSH 握手成功但無法建隧道到目的地: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(4 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		return fmt.Errorf("ssh tunnel write: %w", err)
	}
	got := make([]byte, len(issue66Payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		return fmt.Errorf("ssh tunnel read: %w", err)
	}
	if string(got) != issue66Payload {
		return fmt.Errorf("ssh payload=%q want %q", got, issue66Payload)
	}
	return nil
}

func startIssue66Dest(t *testing.T) (net.Listener, *net.TCPAddr) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("destination listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = conn.Write([]byte(issue66Payload))
				_, _ = io.Copy(io.Discard, conn)
			}(c)
		}
	}()
	return ln, ln.Addr().(*net.TCPAddr)
}
