package service

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
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
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// Official cedar2025/Xboard-Node #55：Hy2／TUIC UDP PacketConn unwrap
// 上下行對調後，GetUserTraffic／report 的 u／d 也反了。
//
// 測試先紅：餵 PacketConn CountFunc（下載大、上傳小），再經
// trackAndEnforce → Report。report 日誌當證據。不准關 UDP 統計。
// TCP Unwrap 反向不得一起被改反。

const (
	packetReportUserID    = 55
	packetReportOtherID   = 7
	packetReportUUID      = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	packetReportOtherUUID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	packetReportDownload  = int64(8192)
	packetReportUpload    = int64(128)
)

func TestHy2PacketConnReport_DownloadLargeUploadSmall(t *testing.T) {
	assertPacketConnReportDir(t, "hysteria2", packetReportUpload, packetReportDownload)
}

func TestTUICPacketConnReport_DownloadLargeUploadSmall(t *testing.T) {
	assertPacketConnReportDir(t, "tuic", packetReportUpload, packetReportDownload)
}

func TestHy2PacketConnReport_UploadLargeDownloadSmall(t *testing.T) {
	assertPacketConnReportDir(t, "hysteria2", packetReportDownload, packetReportUpload)
}

func TestTUICPacketConnReport_UploadLargeDownloadSmall(t *testing.T) {
	assertPacketConnReportDir(t, "tuic", packetReportDownload, packetReportUpload)
}

func TestTCPUnwrapReport_DirectionUnchanged(t *testing.T) {
	svc, rec, logs, ct := startPacketConnReportService(t, "vless")
	wrapped := ct.RoutedConnection(context.Background(), &reportTestConn{}, reportInbound(packetReportUUID, "8.8.8.8"), nil, nil)
	feedUnwrapTCP(t, wrapped, packetReportUpload, packetReportDownload)

	svc.trackAndEnforce(context.Background())
	report := rec.waitReport(t, svc)
	assertReportDirection(t, "tcp", report, logs.String(), packetReportUpload, packetReportDownload)
}

func assertPacketConnReportDir(t *testing.T, protocol string, upload, download int64) {
	t.Helper()

	svc, rec, logs, ct := startPacketConnReportService(t, protocol)
	wrapped := ct.RoutedPacketConnection(context.Background(), &reportTestPacketConn{}, reportInbound(packetReportUUID, "8.8.8.8"), nil, nil)
	feedUnwrapPacket(t, wrapped, upload, download)

	svc.trackAndEnforce(context.Background())
	report := rec.waitReport(t, svc)
	logText := assertReportDirection(t, protocol, report, logs.String(), upload, download)

	dir := t.TempDir()
	name := fmt.Sprintf("%s_packetconn_report.log", protocol)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(logText+"\n"), 0o644); err != nil {
		t.Fatalf("write report log: %v", err)
	}
}

func assertReportDirection(t *testing.T, protocol string, report controlplane.ReportPayload, logText string, upload, download int64) string {
	t.Helper()

	traffic, ok := report.Traffic[packetReportUserID]
	if !ok {
		t.Fatalf("%s report.Traffic 沒有 user %d: %#v", protocol, packetReportUserID, report.Traffic)
	}
	if traffic != [2]int64{upload, download} {
		t.Fatalf("%s report 方向錯 user=%d traffic=%v, want [upload=%d download=%d]（官方 #55）",
			protocol, packetReportUserID, traffic, upload, download)
	}
	if _, ok := report.Traffic[packetReportOtherID]; ok {
		t.Fatalf("%s 流量掛到別人 %#v", protocol, report.Traffic)
	}
	if report.Online[packetReportUserID] == 0 {
		t.Fatalf("%s PacketConn 還在，report.Online[%d]=0 payload=%#v", protocol, packetReportUserID, report.Online)
	}
	if !strings.Contains(logText, "report pushed:") {
		t.Fatalf("%s 沒有 report 日誌:\n%s", protocol, logText)
	}
	if strings.Contains(logText, "report pushed: 0 users, 0 online") {
		t.Fatalf("%s 有流量仍打出 report pushed: 0 users, 0 online:\n%s", protocol, logText)
	}
	wantLog := fmt.Sprintf("report pushed: %d users, %d online", len(report.Traffic), len(report.Online))
	if !strings.Contains(logText, wantLog) {
		t.Fatalf("%s report 日誌對不上 users=%d online=%d:\n%s", protocol, len(report.Traffic), len(report.Online), logText)
	}

	evidence := fmt.Sprintf("%s unwrap report user=%d traffic=%v online=%v log=%q",
		protocol, packetReportUserID, traffic, report.Online, wantLog)
	t.Logf("%s", evidence)
	return evidence + "\n" + logText
}

func startPacketConnReportService(t *testing.T, protocol string) (*Service, *reportRecorder, *lockedLogBuf, *singbox.ConnTracker) {
	t.Helper()

	logs := &lockedLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	ct := singbox.NewConnTracker(0)
	ct.SetUserMap(map[string]int{
		packetReportUUID:      packetReportUserID,
		packetReportOtherUUID: packetReportOtherID,
	})

	k := &packetConnKernel{ct: ct}
	k.running = true

	rec := newReportRecorder()
	sharedLimiter := limiter.New()
	svc := &Service{
		kernel:       k,
		tracker:      tracker.New(),
		sink:         rec,
		source:       controlplane.NewLocalControlPlane(&config.Config{}),
		limiter:      sharedLimiter,
		speedTracker: limiter.NewSpeedTracker(sharedLimiter),
		nodeLog:      nlog.ForNode(protocol, 443),
		lastConfig:   &model.NodeSpec{Protocol: protocol, ListenIP: "127.0.0.1", ServerPort: 443},
		lastUsers: []model.UserSpec{
			{ID: packetReportUserID, UUID: packetReportUUID},
			{ID: packetReportOtherID, UUID: packetReportOtherUUID},
		},
	}
	return svc, rec, logs, ct
}

func feedUnwrapPacket(t *testing.T, conn N.PacketConn, upload, download int64) {
	t.Helper()
	reader, ok := conn.(N.PacketReadCounter)
	if !ok {
		t.Fatalf("PacketConn 沒有 UnwrapPacketReader: %T", conn)
	}
	writer, ok := conn.(N.PacketWriteCounter)
	if !ok {
		t.Fatalf("PacketConn 沒有 UnwrapPacketWriter: %T", conn)
	}
	_, readFns := reader.UnwrapPacketReader()
	_, writeFns := writer.UnwrapPacketWriter()
	if len(readFns) == 0 || len(writeFns) == 0 {
		t.Fatal("UDP PacketConn 計數被關掉了（不准關統計當修）")
	}
	for _, f := range readFns {
		f(upload)
	}
	for _, f := range writeFns {
		f(download)
	}
}

func feedUnwrapTCP(t *testing.T, conn net.Conn, upload, download int64) {
	t.Helper()
	reader, ok := conn.(N.ReadCounter)
	if !ok {
		t.Fatalf("TCP conn 沒有 UnwrapReader: %T", conn)
	}
	writer, ok := conn.(N.WriteCounter)
	if !ok {
		t.Fatalf("TCP conn 沒有 UnwrapWriter: %T", conn)
	}
	_, readFns := reader.UnwrapReader()
	_, writeFns := writer.UnwrapWriter()
	if len(readFns) == 0 || len(writeFns) == 0 {
		t.Fatal("TCP Unwrap 計數被關掉了")
	}
	for _, f := range readFns {
		f(upload)
	}
	for _, f := range writeFns {
		f(download)
	}
}

func reportInbound(uuid, ip string) adapter.InboundContext {
	return adapter.InboundContext{
		User:   uuid,
		Source: singM.Socksaddr{Addr: netip.MustParseAddr(ip)},
	}
}

type packetConnKernel struct {
	fakeKernel
	ct *singbox.ConnTracker
}

func (k *packetConnKernel) GetUserTraffic(ctx context.Context) (map[int][2]int64, map[int]map[string]bool, int, error) {
	_ = ctx
	traffic, alive, connCount := k.ct.GetUserTraffic()
	return traffic, alive, connCount, nil
}

type reportTestConn struct{}

func (c *reportTestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *reportTestConn) Write(b []byte) (int, error)      { return len(b), nil }
func (c *reportTestConn) Close() error                     { return nil }
func (c *reportTestConn) LocalAddr() net.Addr              { return &net.TCPAddr{} }
func (c *reportTestConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c *reportTestConn) SetDeadline(time.Time) error      { return nil }
func (c *reportTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c *reportTestConn) SetWriteDeadline(time.Time) error { return nil }

type reportTestPacketConn struct{}

func (c *reportTestPacketConn) ReadPacket(*buf.Buffer) (singM.Socksaddr, error) {
	return singM.Socksaddr{}, io.EOF
}
func (c *reportTestPacketConn) WritePacket(*buf.Buffer, singM.Socksaddr) error { return nil }
func (c *reportTestPacketConn) Close() error                                   { return nil }
func (c *reportTestPacketConn) LocalAddr() net.Addr                            { return &net.UDPAddr{} }
func (c *reportTestPacketConn) SetDeadline(time.Time) error                    { return nil }
func (c *reportTestPacketConn) SetReadDeadline(time.Time) error                { return nil }
func (c *reportTestPacketConn) SetWriteDeadline(time.Time) error               { return nil }
