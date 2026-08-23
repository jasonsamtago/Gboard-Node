package quicbind

import (
	"net"
	"testing"
	"time"
)

func TestRelay_SessionCloseDoesNotUnbindUDP(t *testing.T) {
	real, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer real.Close()

	relay := NewRelay(real)
	first := relay.Session()
	if err := first.Close(); err != nil {
		t.Fatalf("session close: %v", err)
	}

	second := relay.Session()
	peer, err := net.DialUDP("udp", nil, real.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer peer.Close()

	if _, err := peer.Write([]byte("hy2-relay")); err != nil {
		t.Fatalf("write: %v", err)
	}

	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 32)
	n, _, err := second.ReadFrom(buf)
	if err != nil {
		t.Fatalf("新 Session 必須仍能從同一 UDP 埠收包: %v", err)
	}
	if string(buf[:n]) != "hy2-relay" {
		t.Fatalf("got %q", buf[:n])
	}

	if err := relay.Close(); err != nil {
		t.Fatalf("relay close: %v", err)
	}
	if c, err := net.ListenUDP("udp", real.LocalAddr().(*net.UDPAddr)); err == nil {
		_ = c.Close()
	}
}
