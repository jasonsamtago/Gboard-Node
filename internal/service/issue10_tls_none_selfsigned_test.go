package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/controlplane"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// Official cedar2025/Xboard-Node #10 的 service／initial setup 路徑：
// start kernel → create sing-box instance → read certificate: open self-signed。
// 只鎖失敗行為，不修 production。

type issue10StubCP struct {
	spec  *model.NodeSpec
	users []model.UserSpec
}

func (s *issue10StubCP) Initial(ctx context.Context, _ func() map[string]interface{}, _ chan<- controlplane.Event, _ chan<- controlplane.StatusChange) (controlplane.Bootstrap, error) {
	select {
	case <-ctx.Done():
		return controlplane.Bootstrap{}, ctx.Err()
	default:
	}
	return controlplane.Bootstrap{Config: s.spec, Users: s.users, PushInterval: 60, PullInterval: 60}, nil
}
func (s *issue10StubCP) Poll(ctx context.Context) (controlplane.Snapshot, error) {
	select {
	case <-ctx.Done():
		return controlplane.Snapshot{}, ctx.Err()
	default:
		return controlplane.Snapshot{Config: s.spec, Users: s.users}, nil
	}
}
func (s *issue10StubCP) Discover(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.PushClient, error) {
	return nil, nil
}
func (s *issue10StubCP) Report(controlplane.ReportPayload) error { return nil }
func (s *issue10StubCP) ReportDevices(controlplane.PushClient, map[int][]string) {
}
func (s *issue10StubCP) Metrics() controlplane.APIMetrics { return controlplane.APIMetrics{} }
func (s *issue10StubCP) SupportsPolling() bool            { return false }
func (s *issue10StubCP) SupportsDiscovery() bool          { return false }
func (s *issue10StubCP) SupportsReporting() bool          { return false }
func (s *issue10StubCP) SupportsDeviceReports() bool      { return false }

func TestIssue10_ServiceInitialSetup_SingBox_MustStartWithoutSelfSigned(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)

	port := issue10FreeTCPPort(t)
	spec := &model.NodeSpec{
		Protocol:   "vmess",
		ListenIP:   "127.0.0.1",
		ServerPort: port,
		Network:    "ws",
		NetworkSettings: map[string]any{
			"path": "/",
			"headers": map[string]any{
				"Host": "cdn.example.com",
			},
		},
		TLS: 1,
		CertConfig: &config.CertConfig{
			CertMode: "none",
			CertDir:  t.TempDir(),
		},
		CustomRoutes: []map[string]any{{
			"outbound": "direct",
			"ip_cidr":  []string{"127.0.0.0/8"},
		}},
	}
	users := []model.UserSpec{{ID: 50, UUID: "50505050-5050-4505-8505-505050505050"}}

	cfg := &config.Config{
		Kernel: config.KernelConfig{Type: "singbox", LogLevel: "warn"},
		Cert:   config.CertConfig{CertMode: "none", CertDir: t.TempDir()},
	}
	svc := NewWithControlPlane(cfg, &issue10StubCP{spec: spec, users: users})
	if svc.kernel.Name() != "sing-box" {
		t.Fatalf("不准改切 xray 當修：kernel=%q", svc.kernel.Name())
	}

	err := svc.initialSetup(context.Background())
	t.Cleanup(func() {
		if svc.kernel != nil {
			svc.kernel.Stop()
		}
	})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "open self-signed") || strings.Contains(msg, "read certificate") {
			t.Fatalf("官方 #10：initial setup／start kernel 不得再 open self-signed：%v", err)
		}
		t.Fatalf("官方 #10：tls=1＋cert_mode=none 必須能 initial setup／start kernel：%v", err)
	}
	if spec.TLS != 1 {
		t.Fatal("不准關 TLS：NodeSpec.TLS 被改掉")
	}
	if !strings.EqualFold(spec.Network, "ws") {
		t.Fatal("不准拆 ws")
	}
	if !svc.kernel.IsRunning() {
		t.Fatal("initial setup 成功但 kernel 沒在跑：略過起核是假修")
	}
	if err := issue10TCPPing(fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		t.Fatalf("必須真的起核聽端口，不准只建 JSON：%v", err)
	}
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
