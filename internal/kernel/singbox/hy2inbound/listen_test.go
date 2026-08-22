package hy2inbound

import (
	"net"
	"testing"
)

func TestListenUDPRange_小區間每個端口都被占用(t *testing.T) {
	const start, end = 26598, 26600
	pc, err := ListenUDPRange("127.0.0.1", start, end)
	if err != nil {
		t.Skipf("無法綁定測試區間 %d-%d: %v", start, end, err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	for port := start; port <= end; port++ {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
		if err == nil {
			_ = c.Close()
			t.Errorf("Hy2 hop 必須占用完整區間，端口 %d 仍可被別人 ListenUDP", port)
		}
	}
}

func TestListenUDPRange_單端口不得多開(t *testing.T) {
	const port = 26601
	pc, err := ListenUDPRange("127.0.0.1", port, port)
	if err != nil {
		t.Skipf("無法綁定測試端口 %d: %v", port, err)
	}
	t.Cleanup(func() { _ = pc.Close() })

	if c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port}); err == nil {
		_ = c.Close()
		t.Fatalf("單端口 %d 必須被占用", port)
	}
	if c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port + 1}); err != nil {
		t.Fatalf("單端口不得多開到 %d: %v", port+1, err)
	} else {
		_ = c.Close()
	}
}
