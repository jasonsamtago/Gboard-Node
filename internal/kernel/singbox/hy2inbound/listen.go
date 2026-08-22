package hy2inbound

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// Range 是含端點的 UDP 聽區間。Start==End 代表單端口。
type Range struct {
	Start int
	End   int
}

func (r Range) Valid() bool {
	return r.Start >= 1 && r.End <= 65535 && r.Start <= r.End
}

func (r Range) IsHop() bool {
	return r.Valid() && r.End > r.Start
}

func (r Range) String() string {
	if r.End > r.Start {
		return fmt.Sprintf("%d-%d", r.Start, r.End)
	}
	return fmt.Sprintf("%d", r.Start)
}

type hopKey struct{}

func WithRange(ctx context.Context, r Range) context.Context {
	if !r.Valid() {
		return ctx
	}
	return context.WithValue(ctx, hopKey{}, r)
}

func RangeFromContext(ctx context.Context) (Range, bool) {
	r, ok := ctx.Value(hopKey{}).(Range)
	return r, ok && r.Valid()
}

type packet struct {
	n    int
	addr net.Addr
	buf  []byte
	conn *net.UDPConn
	err  error
}

// muxPacketConn 把 [start, end] 的 UDP socket 合成一個 PacketConn 餵給 Hy2。
// WriteTo 從當初收下該 client 的那個端口回覆，這樣跳躍才會通。
type muxPacketConn struct {
	conns    []*net.UDPConn
	incoming chan packet
	local    net.Addr

	mu     sync.Mutex
	closed bool
	routes sync.Map // client addr string -> *net.UDPConn
}

func ListenUDPRange(host string, start, end int) (net.PacketConn, error) {
	if start < 1 || end > 65535 || start > end {
		return nil, fmt.Errorf("invalid hy2 listen range %d-%d", start, end)
	}
	if host == "" {
		host = "::"
	}

	conns := make([]*net.UDPConn, 0, end-start+1)
	for port := start; port <= end; port++ {
		addr := &net.UDPAddr{Port: port}
		if ip := net.ParseIP(host); ip != nil {
			addr.IP = ip
		} else if host != "::" && host != "0.0.0.0" {
			addr = &net.UDPAddr{IP: net.ParseIP(host), Port: port}
			if addr.IP == nil {
				udpAddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(host, fmt.Sprintf("%d", port)))
				if err != nil {
					closeUDPConns(conns)
					return nil, err
				}
				addr = udpAddr
			}
		}
		c, err := net.ListenUDP("udp", addr)
		if err != nil {
			closeUDPConns(conns)
			return nil, fmt.Errorf("hy2 listen udp %s: %w", addr, err)
		}
		conns = append(conns, c)
	}

	m := &muxPacketConn{
		conns:    conns,
		incoming: make(chan packet, 256),
		local:    conns[0].LocalAddr(),
	}
	for _, c := range conns {
		go m.readLoop(c)
	}
	return m, nil
}

func closeUDPConns(conns []*net.UDPConn) {
	for _, c := range conns {
		_ = c.Close()
	}
}

func (m *muxPacketConn) readLoop(c *net.UDPConn) {
	for {
		buf := make([]byte, 65535)
		n, addr, err := c.ReadFrom(buf)
		pkt := packet{n: n, addr: addr, buf: buf[:n], conn: c, err: err}
		if err != nil {
			m.mu.Lock()
			closed := m.closed
			m.mu.Unlock()
			if closed {
				return
			}
		}
		if addr != nil {
			m.routes.Store(addr.String(), c)
		}
		select {
		case m.incoming <- pkt:
		default:
			// 佇列滿就丟，避免一個慢讀拖死所有 hop 端口。
		}
		if err != nil {
			return
		}
	}
}

func (m *muxPacketConn) ReadFrom(p []byte) (int, net.Addr, error) {
	pkt, ok := <-m.incoming
	if !ok {
		return 0, nil, net.ErrClosed
	}
	if pkt.err != nil {
		return 0, pkt.addr, pkt.err
	}
	n := copy(p, pkt.buf)
	return n, pkt.addr, nil
}

func (m *muxPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if addr == nil {
		return 0, fmt.Errorf("hy2 hop write: nil addr")
	}
	conn := m.conns[0]
	if v, ok := m.routes.Load(addr.String()); ok {
		if c, ok := v.(*net.UDPConn); ok {
			conn = c
		}
	}
	return conn.WriteTo(p, addr)
}

func (m *muxPacketConn) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	m.mu.Unlock()
	closeUDPConns(m.conns)
	return nil
}

func (m *muxPacketConn) LocalAddr() net.Addr { return m.local }

func (m *muxPacketConn) SetDeadline(t time.Time) error {
	var err error
	for _, c := range m.conns {
		if e := c.SetDeadline(t); e != nil && err == nil {
			err = e
		}
	}
	return err
}

func (m *muxPacketConn) SetReadDeadline(t time.Time) error {
	var err error
	for _, c := range m.conns {
		if e := c.SetReadDeadline(t); e != nil && err == nil {
			err = e
		}
	}
	return err
}

func (m *muxPacketConn) SetWriteDeadline(t time.Time) error {
	var err error
	for _, c := range m.conns {
		if e := c.SetWriteDeadline(t); e != nil && err == nil {
			err = e
		}
	}
	return err
}
