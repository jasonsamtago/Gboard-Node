package singbox

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/hy2inbound"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/tuicinbound"
	"github.com/sagernet/sing-box/adapter"
	singLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/buf"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// Official cedar2025/Xboard-Node #55：Hy2／TUIC 走 UDP PacketConn 時，
// sing CopyPacket 用 UnwrapPacketReader／Writer 的 CountFunc 計費。
// 官方現場是這兩條對調，下載進 upload、上傳進 download。
//
// 測試先紅：餵 PacketConn unwrap 計數（不是關統計、不是手填 GetUserTraffic）。
// 正向：下載進 download、上傳進 upload。反向：TCP UnwrapReader／Writer
// 方向不能被一起改反；ReadPacket／WritePacket 仍要計數。

const (
	packetDirUserID   = 55
	packetDirUUID     = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	packetDirDownload = int64(8192)
	packetDirUpload   = int64(128)
)

func TestHysteria2PacketConnUnwrap_DownloadLargeUploadSmall(t *testing.T) {
	assertInboundPacketUnwrapDir(t, "hysteria2", packetDirUpload, packetDirDownload)
}

func TestTUICPacketConnUnwrap_DownloadLargeUploadSmall(t *testing.T) {
	assertInboundPacketUnwrapDir(t, "tuic", packetDirUpload, packetDirDownload)
}

func TestHysteria2PacketConnUnwrap_UploadLargeDownloadSmall(t *testing.T) {
	assertInboundPacketUnwrapDir(t, "hysteria2", packetDirDownload, packetDirUpload)
}

func TestTUICPacketConnUnwrap_UploadLargeDownloadSmall(t *testing.T) {
	assertInboundPacketUnwrapDir(t, "tuic", packetDirDownload, packetDirUpload)
}

func TestTCPUnwrap_DirectionUnchanged(t *testing.T) {
	ct := NewConnTracker(0)
	ct.SetUserMap(map[string]int{packetDirUUID: packetDirUserID})
	wrapped := ct.RoutedConnection(context.Background(), &testConn{}, testInboundContext(packetDirUUID, "1.2.3.4"), nil, nil)
	feedUnwrapTCPCounts(t, wrapped, packetDirUpload, packetDirDownload)

	traffic, _, _ := ct.GetUserTraffic()
	if got := traffic[packetDirUserID]; got != [2]int64{packetDirUpload, packetDirDownload} {
		t.Fatalf("TCP UnwrapReader/Writer 方向被改反了 traffic=%v, want upload=%d download=%d",
			got, packetDirUpload, packetDirDownload)
	}
	t.Logf("TCP unwrap 反向證據 user=%d traffic=%v path=UnwrapReader/Writer", packetDirUserID, traffic[packetDirUserID])
}

func TestPacketConnReadWrite_StillCounts(t *testing.T) {
	ct := NewConnTracker(0)
	ct.SetUserMap(map[string]int{packetDirUUID: packetDirUserID})
	base := &feedPacketConn{reads: [][]byte{bytesOf('U', int(packetDirUpload))}}
	wrapped := ct.RoutedPacketConnection(context.Background(), base, testInboundContext(packetDirUUID, "1.2.3.4"), nil, nil)

	rb := buf.NewSize(int(packetDirUpload) + 16)
	if _, err := wrapped.ReadPacket(rb); err != nil {
		t.Fatalf("ReadPacket: %v", err)
	}
	rb.Release()

	wb := buf.NewSize(int(packetDirDownload))
	if _, err := wb.Write(bytesOf('D', int(packetDirDownload))); err != nil {
		t.Fatalf("Write buffer: %v", err)
	}
	if err := wrapped.WritePacket(wb, hy2TuicDst()); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}

	traffic, _, _ := ct.GetUserTraffic()
	if got := traffic[packetDirUserID]; got != [2]int64{packetDirUpload, packetDirDownload} {
		t.Fatalf("UDP ReadPacket/WritePacket 統計被關掉或方向錯 traffic=%v, want [%d %d]",
			got, packetDirUpload, packetDirDownload)
	}
	t.Logf("UDP ReadPacket/WritePacket 仍計數 user=%d traffic=%v", packetDirUserID, traffic[packetDirUserID])
}

func assertInboundPacketUnwrapDir(t *testing.T, protocol string, upload, download int64) {
	t.Helper()

	ct := NewConnTracker(0)
	ct.SetUserMap(map[string]int{
		hy2TuicUserAlice: hy2TuicAliceID,
		packetDirUUID:    packetDirUserID,
	})
	router := &trafficRouter{tracker: ct}
	ctx := auth.ContextWithUser(context.Background(), hy2TuicUserAlice)

	switch protocol {
	case "hysteria2":
		in := newHy2InboundOnRouter(t, router, hy2TuicFiveUsers())
		in.NewPacketConnectionEx(ctx, &feedPacketConn{}, hy2TuicSrc(), hy2TuicDst(), nil)
	case "tuic":
		in := newTUICInboundOnRouter(t, router, hy2TuicFiveTUICUsers())
		in.NewPacketConnectionEx(ctx, &feedPacketConn{}, hy2TuicSrc(), hy2TuicDst(), nil)
	default:
		t.Fatalf("unknown protocol %q", protocol)
	}

	if router.lastUser != hy2TuicUserAlice {
		t.Fatalf("%s inbound PacketConn user=%q, want Alice", protocol, router.lastUser)
	}
	if router.wrapped == nil {
		t.Fatalf("%s inbound 沒把 PacketConn 交給 ConnTracker", protocol)
	}

	feedUnwrapPacketCounts(t, router.wrapped, upload, download)
	traffic, _, _ := ct.GetUserTraffic()
	got := traffic[hy2TuicAliceID]
	want := [2]int64{upload, download}
	if got != want {
		t.Fatalf("%s UDP PacketConn unwrap GetUserTraffic[Alice]=%v, want [upload=%d download=%d]（官方 #55：上下行對調）",
			protocol, got, upload, download)
	}
	if other := traffic[packetDirUserID]; other != [2]int64{} {
		t.Fatalf("%s 流量掛到別人 user=%d traffic=%v", protocol, packetDirUserID, other)
	}
	t.Logf("%s unwrap 證據 user=%d traffic=%v path=UnwrapPacketReader/Writer", protocol, hy2TuicAliceID, got)
}

func feedUnwrapPacketCounts(t *testing.T, conn N.PacketConn, upload, download int64) {
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

func feedUnwrapTCPCounts(t *testing.T, conn net.Conn, upload, download int64) {
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

type trafficRouter struct {
	captureRouter
	tracker *ConnTracker
	wrapped N.PacketConn
}

func (r *trafficRouter) RoutePacketConnectionEx(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext, _ N.CloseHandlerFunc) {
	r.lastUser = metadata.User
	r.wrapped = r.tracker.RoutedPacketConnection(ctx, conn, metadata, nil, nil)
}

func (r *trafficRouter) RoutePacketConnection(ctx context.Context, conn N.PacketConn, metadata adapter.InboundContext) error {
	r.RoutePacketConnectionEx(ctx, conn, metadata, nil)
	return nil
}

func newHy2InboundOnRouter(t *testing.T, router adapter.Router, users []option.Hysteria2User) hy2Inbound {
	t.Helper()
	raw, err := hy2inbound.NewInbound(context.Background(), router, singLog.NewNOPFactory().Logger(), "hy2-dir", option.Hysteria2InboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     hy2TuicListenAddr(),
			ListenPort: 18553,
		},
		Users: users,
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: hy2TuicTestTLS(t),
		},
	})
	if err != nil {
		t.Fatalf("hysteria2.NewInbound: %v", err)
	}
	in, ok := raw.(hy2Inbound)
	if !ok {
		t.Fatalf("hysteria2 inbound missing NewPacketConnectionEx")
	}
	return in
}

func newTUICInboundOnRouter(t *testing.T, router adapter.Router, users []option.TUICUser) tuicInbound {
	t.Helper()
	raw, err := tuicinbound.NewInbound(context.Background(), router, singLog.NewNOPFactory().Logger(), "tuic-dir", option.TUICInboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     hy2TuicListenAddr(),
			ListenPort: 18554,
		},
		Users: users,
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: hy2TuicTestTLS(t),
		},
	})
	if err != nil {
		t.Fatalf("tuic.NewInbound: %v", err)
	}
	in, ok := raw.(tuicInbound)
	if !ok {
		t.Fatalf("tuic inbound missing NewPacketConnectionEx")
	}
	return in
}

type feedPacketConn struct {
	reads  [][]byte
	writes [][]byte
}

func (c *feedPacketConn) ReadPacket(buffer *buf.Buffer) (singM.Socksaddr, error) {
	if len(c.reads) == 0 {
		return singM.Socksaddr{}, io.EOF
	}
	chunk := c.reads[0]
	c.reads = c.reads[1:]
	if _, err := buffer.Write(chunk); err != nil {
		return singM.Socksaddr{}, err
	}
	return hy2TuicDst(), nil
}

func (c *feedPacketConn) WritePacket(buffer *buf.Buffer, _ singM.Socksaddr) error {
	c.writes = append(c.writes, append([]byte(nil), buffer.Bytes()...))
	return nil
}

func (c *feedPacketConn) Close() error                     { return nil }
func (c *feedPacketConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *feedPacketConn) SetDeadline(time.Time) error      { return nil }
func (c *feedPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *feedPacketConn) SetWriteDeadline(time.Time) error { return nil }

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}
