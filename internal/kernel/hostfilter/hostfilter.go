// Package hostfilter peeks the first HTTP request Host and rejects mismatches.
// Used so ws／tcp+http 設了 Host 時，連線路徑必須真的要求 Host，不准只寫進 config。
package hostfilter

import (
	"bytes"
	"net"
	"strings"
	"time"
)

const (
	maxHeaderBytes = 8192
	peekTimeout    = 10 * time.Second
)

// WrapListener rejects accepted connections whose first HTTP Host does not match.
func WrapListener(l net.Listener, expected string) net.Listener {
	expected = normalizeHost(expected)
	if l == nil || expected == "" {
		return l
	}
	return &hostListener{Listener: l, expected: expected}
}

// Accept peeks Host on conn. On match it returns a conn that replays the peek.
func Accept(conn net.Conn, expected string) (net.Conn, bool) {
	expected = normalizeHost(expected)
	if conn == nil || expected == "" {
		return conn, expected == ""
	}
	replay, host, ok := peekHTTPRequest(conn)
	if !ok || !hostMatches(host, expected) {
		return conn, false
	}
	return replay, true
}

type hostListener struct {
	net.Listener
	expected string
}

func (l *hostListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		checked, ok := Accept(conn, l.expected)
		if !ok {
			_ = conn.Close()
			continue
		}
		return checked, nil
	}
}

func peekHTTPRequest(conn net.Conn) (net.Conn, string, bool) {
	_ = conn.SetReadDeadline(time.Now().Add(peekTimeout))
	defer conn.SetReadDeadline(time.Time{})

	var buf bytes.Buffer
	tmp := make([]byte, 1024)
	for buf.Len() < maxHeaderBytes {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if bytes.Contains(buf.Bytes(), []byte("\r\n\r\n")) {
			break
		}
		if err != nil {
			return conn, "", false
		}
	}
	data := buf.Bytes()
	if !bytes.Contains(data, []byte("\r\n\r\n")) {
		return conn, "", false
	}
	host := parseHTTPHost(data)
	return &prefixConn{Conn: conn, prefix: data}, host, host != ""
}

func parseHTTPHost(headerBlock []byte) string {
	for _, line := range bytes.Split(headerBlock, []byte("\r\n")) {
		if len(line) < 5 {
			continue
		}
		if !strings.EqualFold(string(line[:5]), "Host:") {
			continue
		}
		return strings.TrimSpace(string(line[5:]))
	}
	return ""
}

func hostMatches(got, expected string) bool {
	return normalizeHost(got) == normalizeHost(expected)
}

func normalizeHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}

type prefixConn struct {
	net.Conn
	prefix []byte
	off    int
}

func (c *prefixConn) Read(p []byte) (int, error) {
	if c.off < len(c.prefix) {
		n := copy(p, c.prefix[c.off:])
		c.off += n
		return n, nil
	}
	return c.Conn.Read(p)
}
