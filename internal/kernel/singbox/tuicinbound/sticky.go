package tuicinbound

import "net"

// stickyPacketConn lets quic.Listener.Close stop Accept without unbinding UDP.
// UpdateUsers 要換掉 tuic.Service（關掉舊 auth 讀 map）時，埠必須留著，
// 不准重啟核／重綁 listen。
type stickyPacketConn struct {
	net.PacketConn
}

func (c stickyPacketConn) Close() error { return nil }
