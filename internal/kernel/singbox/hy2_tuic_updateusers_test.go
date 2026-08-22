package singbox

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	singLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	hy2 "github.com/sagernet/sing-box/protocol/hysteria2"
	"github.com/sagernet/sing-box/protocol/tuic"
	"github.com/sagernet/sing-tun"
	"github.com/sagernet/sing/common/auth"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/json/badoption"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// Official cedar2025/Xboard-Node #54: Hy2 / TUIC UpdateUsers write the array
// index into the connection ctx. After a hot add/remove, that index points at
// someone else (traffic misattribution) or is past the new list (panic).
//
// These tests drive the real replace-module inbound: NewConnectionEx /
// NewPacketConnectionEx after UpdateUsers. The ctx uses the int index the
// current service stores at accept time (Alice = slot 3).

const (
	hy2TuicUserCarol = "11111111-1111-1111-1111-111111111111"
	hy2TuicUserDave  = "22222222-2222-2222-2222-222222222222"
	hy2TuicUserEve   = "33333333-3333-3333-3333-333333333333"
	hy2TuicUserAlice = "44444444-4444-4444-4444-444444444444"
	hy2TuicUserFrank = "55555555-5555-5555-5555-555555555555"
	hy2TuicUserBob   = "66666666-6666-6666-6666-666666666666"

	hy2TuicAliceIndex = 3
	hy2TuicAliceID    = 13
	hy2TuicBobID      = 99
)

func TestHysteria2UpdateUsers_ExistingConnKeepsTrafficOwner(t *testing.T) {
	in, router := newTestHysteria2Inbound(t, hy2TuicFiveUsers())
	ctx := auth.ContextWithUser(context.Background(), hy2TuicAliceIndex)

	if got := routeHy2Conn(t, in, router, ctx); got != hy2TuicUserAlice {
		t.Fatalf("precondition: existing Hy2 conn user = %q, want Alice", got)
	}

	if err := in.UpdateUsers([]option.Hysteria2User{
		{Name: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, Password: hy2TuicUserEve},
		{Name: hy2TuicUserFrank, Password: hy2TuicUserFrank},
		{Name: hy2TuicUserBob, Password: hy2TuicUserBob},
	}); err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}

	got := routeHy2Conn(t, in, router, ctx)
	if got != hy2TuicUserAlice {
		t.Fatalf("Hy2 hot update reassigned existing conn: got %q, want Alice (not Bob)", got)
	}
	assertTrackedToAlice(t, got, "hy2-tcp")

	gotPkt := routeHy2Packet(t, in, router, ctx)
	if gotPkt != hy2TuicUserAlice {
		t.Fatalf("Hy2 packet after hot update: got %q, want Alice", gotPkt)
	}
	assertTrackedToAlice(t, gotPkt, "hy2-udp")
}

func TestHysteria2UpdateUsers_ShortListDoesNotPanic(t *testing.T) {
	in, router := newTestHysteria2Inbound(t, hy2TuicFiveUsers())
	ctx := auth.ContextWithUser(context.Background(), hy2TuicAliceIndex)

	if err := in.UpdateUsers([]option.Hysteria2User{
		{Name: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, Password: hy2TuicUserEve},
		{Name: hy2TuicUserFrank, Password: hy2TuicUserFrank},
	}); err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("Hy2 short user list panicked on existing conn: %v", rec)
		}
	}()
	got := routeHy2Conn(t, in, router, ctx)
	if got != "" && got != hy2TuicUserAlice {
		t.Fatalf("Hy2 short list attributed existing Alice conn to %q", got)
	}
	gotPkt := routeHy2Packet(t, in, router, ctx)
	if gotPkt != "" && gotPkt != hy2TuicUserAlice {
		t.Fatalf("Hy2 short list packet attributed existing Alice conn to %q", gotPkt)
	}
}

func TestTUICUpdateUsers_ExistingConnKeepsTrafficOwner(t *testing.T) {
	in, router := newTestTUICInbound(t, hy2TuicFiveTUICUsers())
	ctx := auth.ContextWithUser(context.Background(), hy2TuicAliceIndex)

	if got := routeTUICConn(t, in, router, ctx); got != hy2TuicUserAlice {
		t.Fatalf("precondition: existing TUIC conn user = %q, want Alice", got)
	}

	if err := in.UpdateUsers([]option.TUICUser{
		{Name: hy2TuicUserDave, UUID: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, UUID: hy2TuicUserEve, Password: hy2TuicUserEve},
		{Name: hy2TuicUserFrank, UUID: hy2TuicUserFrank, Password: hy2TuicUserFrank},
		{Name: hy2TuicUserBob, UUID: hy2TuicUserBob, Password: hy2TuicUserBob},
	}); err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}

	got := routeTUICConn(t, in, router, ctx)
	if got != hy2TuicUserAlice {
		t.Fatalf("TUIC hot update reassigned existing conn: got %q, want Alice (not Bob)", got)
	}
	assertTrackedToAlice(t, got, "tuic-tcp")

	gotPkt := routeTUICPacket(t, in, router, ctx)
	if gotPkt != hy2TuicUserAlice {
		t.Fatalf("TUIC packet after hot update: got %q, want Alice", gotPkt)
	}
	assertTrackedToAlice(t, gotPkt, "tuic-udp")
}

func TestTUICUpdateUsers_ShortListDoesNotPanic(t *testing.T) {
	in, router := newTestTUICInbound(t, hy2TuicFiveTUICUsers())
	ctx := auth.ContextWithUser(context.Background(), hy2TuicAliceIndex)

	if err := in.UpdateUsers([]option.TUICUser{
		{Name: hy2TuicUserDave, UUID: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, UUID: hy2TuicUserEve, Password: hy2TuicUserEve},
		{Name: hy2TuicUserFrank, UUID: hy2TuicUserFrank, Password: hy2TuicUserFrank},
	}); err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}

	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("TUIC short user list panicked on existing conn: %v", rec)
		}
	}()
	got := routeTUICConn(t, in, router, ctx)
	if got != "" && got != hy2TuicUserAlice {
		t.Fatalf("TUIC short list attributed existing Alice conn to %q", got)
	}
	gotPkt := routeTUICPacket(t, in, router, ctx)
	if gotPkt != "" && gotPkt != hy2TuicUserAlice {
		t.Fatalf("TUIC short list packet attributed existing Alice conn to %q", gotPkt)
	}
}

func hy2TuicFiveUsers() []option.Hysteria2User {
	return []option.Hysteria2User{
		{Name: hy2TuicUserCarol, Password: hy2TuicUserCarol},
		{Name: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, Password: hy2TuicUserEve},
		{Name: hy2TuicUserAlice, Password: hy2TuicUserAlice},
		{Name: hy2TuicUserFrank, Password: hy2TuicUserFrank},
	}
}

func hy2TuicFiveTUICUsers() []option.TUICUser {
	return []option.TUICUser{
		{Name: hy2TuicUserCarol, UUID: hy2TuicUserCarol, Password: hy2TuicUserCarol},
		{Name: hy2TuicUserDave, UUID: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, UUID: hy2TuicUserEve, Password: hy2TuicUserEve},
		{Name: hy2TuicUserAlice, UUID: hy2TuicUserAlice, Password: hy2TuicUserAlice},
		{Name: hy2TuicUserFrank, UUID: hy2TuicUserFrank, Password: hy2TuicUserFrank},
	}
}

type hy2Inbound interface {
	UpdateUsers([]option.Hysteria2User) error
	NewConnectionEx(context.Context, net.Conn, singM.Socksaddr, singM.Socksaddr, N.CloseHandlerFunc)
	NewPacketConnectionEx(context.Context, N.PacketConn, singM.Socksaddr, singM.Socksaddr, N.CloseHandlerFunc)
}

type tuicInbound interface {
	UpdateUsers([]option.TUICUser) error
	NewConnectionEx(context.Context, net.Conn, singM.Socksaddr, singM.Socksaddr, N.CloseHandlerFunc)
	NewPacketConnectionEx(context.Context, N.PacketConn, singM.Socksaddr, singM.Socksaddr, N.CloseHandlerFunc)
}

func newTestHysteria2Inbound(t *testing.T, users []option.Hysteria2User) (hy2Inbound, *captureRouter) {
	t.Helper()
	router := &captureRouter{}
	raw, err := hy2.NewInbound(context.Background(), router, singLog.NewNOPFactory().Logger(), "hy2-in", option.Hysteria2InboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     hy2TuicListenAddr(),
			ListenPort: 18443,
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
		t.Fatalf("hysteria2 inbound missing UpdateUsers / NewConnectionEx")
	}
	return in, router
}

func newTestTUICInbound(t *testing.T, users []option.TUICUser) (tuicInbound, *captureRouter) {
	t.Helper()
	router := &captureRouter{}
	raw, err := tuic.NewInbound(context.Background(), router, singLog.NewNOPFactory().Logger(), "tuic-in", option.TUICInboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     hy2TuicListenAddr(),
			ListenPort: 18444,
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
		t.Fatalf("tuic inbound missing UpdateUsers / NewConnectionEx")
	}
	return in, router
}

func hy2TuicListenAddr() *badoption.Addr {
	addr := netip.MustParseAddr("127.0.0.1")
	a := badoption.Addr(addr)
	return &a
}

func routeHy2Conn(t *testing.T, in hy2Inbound, router *captureRouter, ctx context.Context) string {
	t.Helper()
	router.lastUser = ""
	in.NewConnectionEx(ctx, &testConn{reads: [][]byte{[]byte("x")}}, hy2TuicSrc(), hy2TuicDst(), nil)
	return router.lastUser
}

func routeHy2Packet(t *testing.T, in hy2Inbound, router *captureRouter, ctx context.Context) string {
	t.Helper()
	router.lastUser = ""
	in.NewPacketConnectionEx(ctx, &testPacketConn{}, hy2TuicSrc(), hy2TuicDst(), nil)
	return router.lastUser
}

func routeTUICConn(t *testing.T, in tuicInbound, router *captureRouter, ctx context.Context) string {
	t.Helper()
	router.lastUser = ""
	in.NewConnectionEx(ctx, &testConn{reads: [][]byte{[]byte("x")}}, hy2TuicSrc(), hy2TuicDst(), nil)
	return router.lastUser
}

func routeTUICPacket(t *testing.T, in tuicInbound, router *captureRouter, ctx context.Context) string {
	t.Helper()
	router.lastUser = ""
	in.NewPacketConnectionEx(ctx, &testPacketConn{}, hy2TuicSrc(), hy2TuicDst(), nil)
	return router.lastUser
}

func hy2TuicSrc() singM.Socksaddr {
	return singM.Socksaddr{Addr: netip.MustParseAddr("1.2.3.4"), Port: 1234}
}

func hy2TuicDst() singM.Socksaddr {
	return singM.Socksaddr{Addr: netip.MustParseAddr("9.9.9.9"), Port: 443}
}

func assertTrackedToAlice(t *testing.T, inboundUser, label string) {
	t.Helper()
	tracker := NewConnTracker(0)
	tracker.SetUserMap(map[string]int{
		hy2TuicUserAlice: hy2TuicAliceID,
		hy2TuicUserBob:   hy2TuicBobID,
		hy2TuicUserDave:  11,
		hy2TuicUserEve:   12,
		hy2TuicUserFrank: 14,
		hy2TuicUserCarol: 10,
	})
	base := &testConn{reads: [][]byte{[]byte("hello")}}
	wrapped := tracker.RoutedConnection(context.Background(), base, testInboundContext(inboundUser, "1.2.3.4"), nil, nil)
	buf := make([]byte, 16)
	if _, err := wrapped.Read(buf); err != nil {
		t.Fatalf("%s tracker Read: %v", label, err)
	}
	traffic, _, _ := tracker.GetUserTraffic()
	if got := traffic[hy2TuicAliceID]; got[0] != 5 {
		t.Fatalf("%s traffic[Alice]=%v, want upload 5; Bob=%v (inbound user %q)", label, traffic[hy2TuicAliceID], traffic[hy2TuicBobID], inboundUser)
	}
	if got := traffic[hy2TuicBobID]; got != [2]int64{} {
		t.Fatalf("%s traffic booked to Bob %v; inbound user %q", label, got, inboundUser)
	}
}

func hy2TuicTestTLS(t *testing.T) *option.InboundTLSOptions {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("tls key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "hy2-tuic-test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("tls cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return &option.InboundTLSOptions{
		Enabled:     true,
		ServerName:  "localhost",
		Certificate: []string{string(certPEM)},
		Key:         []string{string(keyPEM)},
	}
}

type captureRouter struct {
	lastUser string
}

func (r *captureRouter) Start(adapter.StartStage) error { return nil }
func (r *captureRouter) Close() error                   { return nil }
func (r *captureRouter) RouteConnection(_ context.Context, _ net.Conn, metadata adapter.InboundContext) error {
	r.lastUser = metadata.User
	return nil
}
func (r *captureRouter) RoutePacketConnection(_ context.Context, _ N.PacketConn, metadata adapter.InboundContext) error {
	r.lastUser = metadata.User
	return nil
}
func (r *captureRouter) RouteConnectionEx(_ context.Context, _ net.Conn, metadata adapter.InboundContext, _ N.CloseHandlerFunc) {
	r.lastUser = metadata.User
}
func (r *captureRouter) RoutePacketConnectionEx(_ context.Context, _ N.PacketConn, metadata adapter.InboundContext, _ N.CloseHandlerFunc) {
	r.lastUser = metadata.User
}
func (r *captureRouter) PreMatch(adapter.InboundContext, tun.DirectRouteContext, time.Duration, bool) (tun.DirectRouteDestination, error) {
	return nil, nil
}
func (r *captureRouter) RuleSet(string) (adapter.RuleSet, bool) { return nil, false }
func (r *captureRouter) Rules() []adapter.Rule                  { return nil }
func (r *captureRouter) NeedFindProcess() bool                  { return false }
func (r *captureRouter) NeedFindNeighbor() bool                 { return false }
func (r *captureRouter) NeighborResolver() adapter.NeighborResolver {
	return nil
}
func (r *captureRouter) AppendTracker(adapter.ConnectionTracker) {}
func (r *captureRouter) ResetNetwork()                           {}
func (r *captureRouter) UpdateRules([]option.Rule, []option.RuleSet) error {
	return nil
}

var _ adapter.Router = (*captureRouter)(nil)

type testPacketConn struct{}

func (c *testPacketConn) ReadPacket(*buf.Buffer) (singM.Socksaddr, error) {
	return singM.Socksaddr{}, net.ErrClosed
}
func (c *testPacketConn) WritePacket(*buf.Buffer, singM.Socksaddr) error { return nil }
func (c *testPacketConn) Close() error                                   { return nil }
func (c *testPacketConn) LocalAddr() net.Addr                            { return &net.UDPAddr{} }
func (c *testPacketConn) SetDeadline(time.Time) error                    { return nil }
func (c *testPacketConn) SetReadDeadline(time.Time) error                { return nil }
func (c *testPacketConn) SetWriteDeadline(time.Time) error               { return nil }
