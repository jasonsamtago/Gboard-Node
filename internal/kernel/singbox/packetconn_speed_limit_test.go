package singbox

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/buf"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/time/rate"
)

// Official cedar2025/Xboard-Node #56：Hy2 走 UDP PacketConn 時限制不住流量。
//
// sing CopyPacket 用 UnwrapCountPacketWriter，會先 UnwrapPacketWriter
//（WriterReplaceable=true）把 trackedPacketConn 剝掉，speed_limit 的
// CountFunc 根本沒掛上。CloseByUUID 是空實作，面板因流量上限抽掉用戶
// 也斷不了現有 Hy2 UDP。
//
// 測試先紅：餵 PacketConn + limiter（不是關 Hy2 限制、不是只改註解）。
// 正向：speed_limit 要降速；流量上限要斷。反向：沒設限的人不能被誤殺；
// TCP Read/Write 限制路徑不能一起被改掉。

const (
	hy2LimitUserID    = 56
	hy2LimitOtherID   = 8
	hy2LimitUUID      = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	hy2LimitOtherUUID = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	hy2LimitBurst     = 1024
)

func TestHysteria2PacketConnCopyPacket_SpeedLimitThrottlesDownload(t *testing.T) {
	assertHy2CopyPacketSpeedLimit(t, "download")
}

func TestHysteria2PacketConnCopyPacket_SpeedLimitThrottlesUpload(t *testing.T) {
	assertHy2CopyPacketSpeedLimit(t, "upload")
}

func TestHysteria2PacketConn_UnlimitedUserNotThrottledOrClosed(t *testing.T) {
	logs := startLimitLogCapture()
	ct, router := newHy2LimitTracker(t, nil)
	base := &closablePacketConn{}
	in := newHy2InboundOnRouter(t, router, hy2TuicFiveUsers())
	ctx := auth.ContextWithUser(context.Background(), hy2TuicUserAlice)
	in.NewPacketConnectionEx(ctx, base, hy2TuicSrc(), hy2TuicDst(), nil)
	if router.wrapped == nil {
		t.Fatal("Hy2 inbound 沒把 PacketConn 交給 ConnTracker")
	}

	_, writeFns := N.UnwrapCountPacketWriter(router.wrapped, nil)
	_, readFns := N.UnwrapCountPacketReader(router.wrapped, nil)
	start := time.Now()
	for _, f := range writeFns {
		f(int64(hy2LimitBurst * 8))
	}
	for _, f := range readFns {
		f(int64(hy2LimitBurst * 8))
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Fatalf("沒設限的人被降速了 elapsed=%s（反向誤殺）", elapsed)
	}
	if n := ct.CloseByUUID(hy2LimitUUID); n != 0 {
		t.Fatalf("CloseByUUID 別人卻動到 Alice n=%d", n)
	}
	if base.closed {
		t.Fatal("沒設限的 Alice PacketConn 被關掉了（反向誤殺）")
	}

	logText := logs.String()
	if !strings.Contains(logText, "unlimited user allowed") && !strings.Contains(logText, "udp unlimited") {
		t.Fatalf("沒設限放行要有限制日誌:\n%s", logText)
	}
	t.Logf("反向證據 user=%s closed=%v log=%q", hy2TuicUserAlice, base.closed, trimLimitLog(logText))
}

func TestHysteria2PacketConn_TrafficQuotaClosesUDP(t *testing.T) {
	logs := startLimitLogCapture()
	ct, router := newHy2LimitTracker(t, rate.NewLimiter(rate.Limit(hy2LimitBurst), hy2LimitBurst))
	base := &closablePacketConn{}
	in := newHy2InboundOnRouter(t, router, hy2TuicFiveUsers())
	ctx := auth.ContextWithUser(context.Background(), hy2TuicUserAlice)
	in.NewPacketConnectionEx(ctx, base, hy2TuicSrc(), hy2TuicDst(), nil)
	if router.wrapped == nil {
		t.Fatal("Hy2 inbound 沒把 PacketConn 交給 ConnTracker")
	}

	// 面板因流量上限把用戶抽掉 → CloseByUUID 必須斷現有 Hy2 UDP。
	closed := ct.CloseByUUID(hy2TuicUserAlice)
	if closed == 0 || !base.closed {
		t.Fatalf("流量上限超額沒斷 Hy2 UDP CloseByUUID=%d closed=%v（官方 #56）", closed, base.closed)
	}

	logText := logs.String()
	if !strings.Contains(logText, "traffic quota") && !strings.Contains(logText, "closing") {
		t.Fatalf("超額斷線要有限制日誌:\n%s", logText)
	}
	t.Logf("流量上限證據 user=%s closed=%v n=%d log=%q", hy2TuicUserAlice, base.closed, closed, trimLimitLog(logText))
}

func TestHysteria2PacketConn_QuotaCloseDoesNotKillUnlimited(t *testing.T) {
	ct := NewConnTracker(0)
	ct.SetUserMap(map[string]int{
		hy2TuicUserAlice:  hy2TuicAliceID,
		hy2LimitUUID:      hy2LimitUserID,
		hy2LimitOtherUUID: hy2LimitOtherID,
	})
	limited := &closablePacketConn{}
	unlimited := &closablePacketConn{}
	ct.SetSpeedLimitFunc(func(uuid string) *rate.Limiter {
		if uuid == hy2LimitUUID {
			return rate.NewLimiter(rate.Limit(hy2LimitBurst), hy2LimitBurst)
		}
		return nil
	})
	_ = ct.RoutedPacketConnection(context.Background(), limited, testInboundContext(hy2LimitUUID, "1.2.3.4"), nil, nil)
	_ = ct.RoutedPacketConnection(context.Background(), unlimited, testInboundContext(hy2LimitOtherUUID, "5.6.7.8"), nil, nil)

	if n := ct.CloseByUUID(hy2LimitUUID); n == 0 || !limited.closed {
		t.Fatalf("設限用戶超額沒斷 PacketConn n=%d closed=%v", n, limited.closed)
	}
	if unlimited.closed {
		t.Fatal("沒設限的人被 CloseByUUID 誤殺")
	}
	t.Logf("反向證據 limited_closed=%v unlimited_closed=%v", limited.closed, unlimited.closed)
}

func TestTCPRateLimitPathUnchanged(t *testing.T) {
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{"uuid-1": 1})
	tracker.SetSpeedLimitFunc(func(uuid string) *rate.Limiter {
		if uuid != "uuid-1" {
			return nil
		}
		return rate.NewLimiter(rate.Limit(1), 1)
	})

	readCtx, cancelRead := context.WithCancel(context.Background())
	readBase := &testConn{reads: [][]byte{[]byte("a"), []byte("a")}}
	readTracked := tracker.RoutedConnection(readCtx, readBase, testInboundContext("uuid-1", "1.1.1.1"), nil, nil)
	buf := make([]byte, 8)
	if n, err := readTracked.Read(buf); err != nil || n != 1 {
		t.Fatalf("TCP 限制路徑被改了 first Read() = (%d, %v)", n, err)
	}
	cancelRead()
	if n, err := readTracked.Read(buf); !errorsIsCanceled(err) || n != 1 {
		t.Fatalf("TCP Read 限制被改掉了 second Read() = (%d, %v)", n, err)
	}

	writeCtx, cancelWrite := context.WithCancel(context.Background())
	writeBase := &testConn{}
	writeTracked := tracker.RoutedConnection(writeCtx, writeBase, testInboundContext("uuid-1", "1.1.1.2"), nil, nil)
	if n, err := writeTracked.Write([]byte("a")); err != nil || n != 1 {
		t.Fatalf("TCP 限制路徑被改了 first Write() = (%d, %v)", n, err)
	}
	cancelWrite()
	if n, err := writeTracked.Write([]byte("a")); !errorsIsCanceled(err) || n != 0 {
		t.Fatalf("TCP Write 限制被改掉了 second Write() = (%d, %v)", n, err)
	}
	t.Logf("TCP 限制路徑仍在 path=Read/Write limiter=1B/s")
}

func assertHy2CopyPacketSpeedLimit(t *testing.T, direction string) {
	t.Helper()
	logs := startLimitLogCapture()
	lim := rate.NewLimiter(rate.Limit(hy2LimitBurst), hy2LimitBurst)
	_, router := newHy2LimitTracker(t, lim)
	in := newHy2InboundOnRouter(t, router, hy2TuicFiveUsers())
	ctx := auth.ContextWithUser(context.Background(), hy2TuicUserAlice)
	in.NewPacketConnectionEx(ctx, &closablePacketConn{}, hy2TuicSrc(), hy2TuicDst(), nil)
	if router.wrapped == nil {
		t.Fatal("Hy2 inbound 沒把 PacketConn 交給 ConnTracker")
	}

	var fns []N.CountFunc
	if direction == "download" {
		_, fns = N.UnwrapCountPacketWriter(router.wrapped, nil)
	} else {
		_, fns = N.UnwrapCountPacketReader(router.wrapped, nil)
	}
	if len(fns) == 0 {
		t.Fatalf("Hy2 UDP CopyPacket %s 沒套 speed_limit（UnwrapCount 把 limiter CountFunc 剝掉了；不准關 Hy2 限制當修）", direction)
	}

	// 先吃光 burst，再餵同樣大小，必須降速。
	for _, f := range fns {
		f(int64(hy2LimitBurst))
	}
	start := time.Now()
	for _, f := range fns {
		f(int64(hy2LimitBurst))
	}
	elapsed := time.Since(start)
	if elapsed < 400*time.Millisecond {
		t.Fatalf("Hy2 UDP %s speed_limit 沒降速 elapsed=%s want>=400ms（官方 #56）", direction, elapsed)
	}

	logText := logs.String()
	if !strings.Contains(logText, "speed limit") && !strings.Contains(logText, "throttl") {
		t.Fatalf("超額降速要有限制日誌:\n%s", logText)
	}
	t.Logf("speed_limit 證據 dir=%s elapsed=%s log=%q", direction, elapsed, trimLimitLog(logText))
}

func newHy2LimitTracker(t *testing.T, lim *rate.Limiter) (*ConnTracker, *trafficRouter) {
	t.Helper()
	ct := NewConnTracker(0)
	ct.SetUserMap(map[string]int{
		hy2TuicUserAlice:  hy2TuicAliceID,
		hy2LimitUUID:      hy2LimitUserID,
		hy2LimitOtherUUID: hy2LimitOtherID,
	})
	ct.SetSpeedLimitFunc(func(uuid string) *rate.Limiter {
		if uuid == hy2TuicUserAlice || uuid == hy2LimitUUID {
			return lim
		}
		return nil
	})
	return ct, &trafficRouter{tracker: ct}
}

func startLimitLogCapture() *limitLogBuf {
	logs := &limitLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)
	return logs
}

func trimLimitLog(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 240 {
		return s[:240]
	}
	return s
}

func errorsIsCanceled(err error) bool {
	return err != nil && (err == context.Canceled || strings.Contains(err.Error(), "canceled"))
}

type closablePacketConn struct {
	closed bool
}

func (c *closablePacketConn) ReadPacket(*buf.Buffer) (singM.Socksaddr, error) {
	return singM.Socksaddr{}, io.EOF
}
func (c *closablePacketConn) WritePacket(*buf.Buffer, singM.Socksaddr) error { return nil }
func (c *closablePacketConn) Close() error {
	c.closed = true
	return nil
}
func (c *closablePacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *closablePacketConn) SetDeadline(time.Time) error      { return nil }
func (c *closablePacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *closablePacketConn) SetWriteDeadline(time.Time) error { return nil }

type limitLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *limitLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *limitLogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}
