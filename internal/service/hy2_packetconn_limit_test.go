package service

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/controlplane"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox"
	"github.com/jasonsamtago/Gboard-Node/internal/limiter"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/jasonsamtago/Gboard-Node/internal/tracker"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/time/rate"
)

// Official cedar2025/Xboard-Node #56：Hy2 UDP PacketConn 必須吃
// speed_limit 與流量上限。測試餵 PacketConn + limiter，限制日誌當證據。
// 不准關 Hy2／UDP 限制當修。TCP 限制路徑不在這裡改。

const (
	hy2LimitSvcUserID    = 56
	hy2LimitSvcOtherID   = 8
	hy2LimitSvcUUID      = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	hy2LimitSvcOtherUUID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	hy2LimitSvcBurst     = 1024
)

func TestHy2PacketConnService_SpeedLimitThrottlesAndLogs(t *testing.T) {
	svc, logs, ct := startHy2LimitService(t, hy2LimitSvcUUID, 1)
	base := &reportTestPacketConn{}
	wrapped := ct.RoutedPacketConnection(context.Background(), base, reportInbound(hy2LimitSvcUUID, "8.8.8.8"), nil, nil)

	_, writeFns := N.UnwrapCountPacketWriter(wrapped, nil)
	if len(writeFns) == 0 {
		t.Fatal("Hy2 UDP CopyPacket 下載沒套 speed_limit（不准關 Hy2 限制當修）")
	}
	for _, f := range writeFns {
		f(int64(hy2LimitSvcBurst))
	}
	start := time.Now()
	for _, f := range writeFns {
		f(int64(hy2LimitSvcBurst))
	}
	elapsed := time.Since(start)
	if elapsed < 400*time.Millisecond {
		t.Fatalf("speed_limit 沒降速 elapsed=%s", elapsed)
	}

	logText := logs.String()
	if !strings.Contains(logText, "speed limit") && !strings.Contains(logText, "throttl") {
		t.Fatalf("超額降速沒有限制日誌:\n%s", logText)
	}
	writeLimitEvidence(t, "hy2_speed_limit.log", fmt.Sprintf("speed_limit 降速 elapsed=%s\n%s", elapsed, logText))
	t.Logf("service speed_limit 證據 user=%d elapsed=%s limiter=%v", hy2LimitSvcUserID, elapsed, svc.speedTracker.GetLimiter(hy2LimitSvcUUID) != nil)
}

func TestHy2PacketConnService_TrafficQuotaClosesAndLogs(t *testing.T) {
	svc, logs, ct := startHy2LimitService(t, hy2LimitSvcUUID, 1)
	base := &closableReportPacketConn{}
	_ = ct.RoutedPacketConnection(context.Background(), base, reportInbound(hy2LimitSvcUUID, "8.8.8.8"), nil, nil)

	if err := svc.kernel.CloseUserConnections(context.Background(), hy2LimitSvcUUID); err != nil {
		t.Fatalf("CloseUserConnections: %v", err)
	}
	if !base.closed {
		t.Fatal("流量上限超額沒斷 Hy2 PacketConn")
	}
	logText := logs.String()
	if !strings.Contains(logText, "traffic quota") && !strings.Contains(logText, "closing") {
		t.Fatalf("超額斷線沒有限制日誌:\n%s", logText)
	}
	writeLimitEvidence(t, "hy2_traffic_quota.log", fmt.Sprintf("quota close closed=%v\n%s", base.closed, logText))
	t.Logf("service 流量上限證據 user=%d closed=%v", hy2LimitSvcUserID, base.closed)
}

func TestHy2PacketConnService_UnlimitedUserAllowed(t *testing.T) {
	svc, logs, ct := startHy2LimitService(t, hy2LimitSvcUUID, 1)
	base := &closableReportPacketConn{}
	wrapped := ct.RoutedPacketConnection(context.Background(), base, reportInbound(hy2LimitSvcOtherUUID, "9.9.9.9"), nil, nil)

	if svc.speedTracker.GetLimiter(hy2LimitSvcOtherUUID) != nil {
		t.Fatal("沒設限的人卻拿到 limiter（反向誤殺）")
	}
	_, writeFns := N.UnwrapCountPacketWriter(wrapped, nil)
	start := time.Now()
	for _, f := range writeFns {
		f(int64(hy2LimitSvcBurst * 8))
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("沒設限的人被降速 elapsed=%s", elapsed)
	}
	if err := svc.kernel.CloseUserConnections(context.Background(), hy2LimitSvcUUID); err != nil {
		t.Fatalf("CloseUserConnections: %v", err)
	}
	if base.closed {
		t.Fatal("斷設限用戶時把沒設限的人一起殺掉")
	}
	logText := logs.String()
	if !strings.Contains(logText, "unlimited") && !strings.Contains(logText, "allowed") {
		t.Fatalf("沒設限放行沒有限制日誌:\n%s", logText)
	}
	writeLimitEvidence(t, "hy2_unlimited_allowed.log", fmt.Sprintf("unlimited allowed closed=%v\n%s", base.closed, logText))
	t.Logf("service 反向證據 other=%d closed=%v", hy2LimitSvcOtherID, base.closed)
}

func startHy2LimitService(t *testing.T, limitedUUID string, speedMbps int) (*Service, *lockedLogBuf, *singbox.ConnTracker) {
	t.Helper()

	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	ct := singbox.NewConnTracker(0)
	ct.SetUserMap(map[string]int{
		hy2LimitSvcUUID:      hy2LimitSvcUserID,
		hy2LimitSvcOtherUUID: hy2LimitSvcOtherID,
	})

	k := &limitPacketConnKernel{ct: ct}
	k.running = true

	sharedLimiter := limiter.New()
	sharedLimiter.UpdateUsers([]model.UserSpec{
		{ID: hy2LimitSvcUserID, UUID: hy2LimitSvcUUID, SpeedLimit: speedMbps},
		{ID: hy2LimitSvcOtherID, UUID: hy2LimitSvcOtherUUID, SpeedLimit: 0},
	})
	st := limiter.NewSpeedTracker(sharedLimiter)
	st.UpdateBuckets()
	// 測試餵小 burst limiter，才能在短時間內證明降速；沒設限的人仍走 SpeedTracker（nil）。
	ct.SetSpeedLimitFunc(func(uuid string) *rate.Limiter {
		if uuid == hy2LimitSvcUUID {
			return rate.NewLimiter(rate.Limit(hy2LimitSvcBurst), hy2LimitSvcBurst)
		}
		return st.GetLimiter(uuid)
	})

	svc := &Service{
		kernel:       k,
		tracker:      tracker.New(),
		sink:         newReportRecorder(),
		source:       controlplane.NewLocalControlPlane(&config.Config{}),
		limiter:      sharedLimiter,
		speedTracker: st,
		nodeLog:      nlog.ForNode("hysteria2", 443),
		lastConfig:   &model.NodeSpec{Protocol: "hysteria2", ListenIP: "127.0.0.1", ServerPort: 443},
		lastUsers: []model.UserSpec{
			{ID: hy2LimitSvcUserID, UUID: hy2LimitSvcUUID, SpeedLimit: speedMbps},
			{ID: hy2LimitSvcOtherID, UUID: hy2LimitSvcOtherUUID},
		},
	}
	_ = limitedUUID
	return svc, logs, ct
}

func writeLimitEvidence(t *testing.T, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(t.TempDir(), name), []byte(text+"\n"), 0o644); err != nil {
		t.Fatalf("write evidence: %v", err)
	}
}

type limitPacketConnKernel struct {
	fakeKernel
	ct *singbox.ConnTracker
}

func (k *limitPacketConnKernel) GetUserTraffic(ctx context.Context) (map[int][2]int64, map[int]map[string]bool, int, error) {
	_ = ctx
	traffic, alive, connCount := k.ct.GetUserTraffic()
	return traffic, alive, connCount, nil
}

func (k *limitPacketConnKernel) CloseUserConnections(_ context.Context, uuid string) error {
	k.ct.CloseByUUID(uuid)
	return nil
}

type closableReportPacketConn struct {
	reportTestPacketConn
	closed bool
}

func (c *closableReportPacketConn) Close() error {
	c.closed = true
	return nil
}
