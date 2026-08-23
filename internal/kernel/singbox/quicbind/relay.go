// Package quicbind lets Hy2／TUIC 熱換 quic.Service 時保住 UDP 埠。
// quic.Listener.Close() 會關底下 PacketConn；若直接 no-op Close，舊讀迴圈
// 會繼續搶包，新 Service 握手會 timeout。
package quicbind

import (
	"net"
	"os"
	"sync"
	"time"
)

type packet struct {
	n    int
	addr net.Addr
	buf  []byte
	err  error
}

// Relay 獨占 real UDP 的 ReadFrom，把包轉給當前 Session。
type Relay struct {
	real net.PacketConn

	mu     sync.Mutex
	sess   *Session
	closed bool
}

// Session 是交給 quic.Listener 的 PacketConn。Close 只脫管子，不拆埠。
type Session struct {
	relay    *Relay
	incoming chan packet

	mu           sync.Mutex
	closed       bool
	readDeadline time.Time
}

func NewRelay(real net.PacketConn) *Relay {
	r := &Relay{real: real}
	go r.loop()
	return r
}

func (r *Relay) Session() net.PacketConn {
	s := &Session{
		relay:    r,
		incoming: make(chan packet, 256),
	}
	r.mu.Lock()
	old := r.sess
	r.sess = s
	r.mu.Unlock()
	if old != nil {
		old.kill()
	}
	return s
}

func (r *Relay) Close() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	r.closed = true
	old := r.sess
	r.sess = nil
	r.mu.Unlock()
	if old != nil {
		old.kill()
	}
	return r.real.Close()
}

func (r *Relay) loop() {
	for {
		buf := make([]byte, 65535)
		n, addr, err := r.real.ReadFrom(buf)
		pkt := packet{n: n, addr: addr, err: err}
		if err == nil && n > 0 {
			pkt.buf = append([]byte(nil), buf[:n]...)
		}
		r.mu.Lock()
		sess := r.sess
		closed := r.closed
		r.mu.Unlock()
		if sess != nil {
			sess.push(pkt)
		}
		if err != nil || closed {
			return
		}
	}
}

func (s *Session) push(pkt packet) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.incoming <- pkt:
	default:
	}
}

func (s *Session) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.incoming)
}

func (s *Session) ReadFrom(p []byte) (int, net.Addr, error) {
	s.mu.Lock()
	deadline := s.readDeadline
	s.mu.Unlock()

	var (
		pkt packet
		ok  bool
	)
	if deadline.IsZero() {
		pkt, ok = <-s.incoming
	} else {
		d := time.Until(deadline)
		if d <= 0 {
			return 0, nil, os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(d)
		select {
		case pkt, ok = <-s.incoming:
			timer.Stop()
		case <-timer.C:
			return 0, nil, os.ErrDeadlineExceeded
		}
	}
	if !ok {
		return 0, nil, net.ErrClosed
	}
	if pkt.err != nil {
		return 0, pkt.addr, pkt.err
	}
	return copy(p, pkt.buf), pkt.addr, nil
}

func (s *Session) WriteTo(p []byte, addr net.Addr) (int, error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	return s.relay.real.WriteTo(p, addr)
}

func (s *Session) Close() error {
	s.relay.mu.Lock()
	if s.relay.sess == s {
		s.relay.sess = nil
	}
	s.relay.mu.Unlock()
	s.kill()
	return nil
}

func (s *Session) LocalAddr() net.Addr { return s.relay.real.LocalAddr() }

func (s *Session) SetDeadline(t time.Time) error {
	return s.SetReadDeadline(t)
}

func (s *Session) SetReadDeadline(t time.Time) error {
	s.mu.Lock()
	s.readDeadline = t
	s.mu.Unlock()
	return nil
}

func (s *Session) SetWriteDeadline(t time.Time) error {
	return s.relay.real.SetWriteDeadline(t)
}
