package xray

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/jasonsamtago/Gboard-Node/internal/model"
)

// maybeStartHTTPTransport puts a V2Ray HTTP/1.1 unwrap in front of xray.
//
// xray-core 26 已移除舊 HTTP transport（parse 直接失敗，官方改走 XHTTP
// stream-one H2 & H3）。XHTTP 線上格式（x_padding／session）與面板
// network=http 客戶端（sing-box／V2Ray type=http，無 TLS 時是 HTTP/1.1 PUT）
// 不相容，不能拿來冒充這個握手。
//
// applyStreamSettings 仍輸出 network=http＋httpSettings（測鎖的 inbound JSON）。
// Start 時這層解包 HTTP 請求後，把 raw VMess 交給核內 TCP inbound。
func maybeStartHTTPTransport(nc *model.NodeSpec) (*model.NodeSpec, *hostProxy, error) {
	if nc == nil || !strings.EqualFold(nc.Network, "http") {
		return nc, nil, nil
	}

	internalLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, err
	}
	internalPort := internalLn.Addr().(*net.TCPAddr).Port
	_ = internalLn.Close()

	publicLn, err := net.Listen("tcp", publicListenHostPort(nc))
	if err != nil {
		return nil, nil, err
	}
	proxy := &hostProxy{ln: publicLn}
	host := httpTransportHost(nc)
	path := httpTransportPath(nc)
	go proxy.serveHTTPTransport(net.JoinHostPort("127.0.0.1", strconv.Itoa(internalPort)), host, path)

	cp := *nc
	cp.ListenIP = "127.0.0.1"
	cp.ServerPort = internalPort
	cp.Network = "tcp"
	cp.NetworkSettings = nil
	cp.TLS = 0
	return &cp, proxy, nil
}

func (p *hostProxy) serveHTTPTransport(dialAddr, expectedHost, expectedPath string) {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go handleHTTPTransportConn(conn, dialAddr, expectedHost, expectedPath)
	}
}

func handleHTTPTransportConn(conn net.Conn, dialAddr, expectedHost, expectedPath string) {
	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)
	if err != nil {
		_ = conn.Close()
		return
	}

	if expectedPath != "" && !strings.HasPrefix(req.URL.Path, expectedPath) {
		_, _ = conn.Write([]byte("HTTP/1.1 404 Not Found\r\nConnection: close\r\n\r\n"))
		_ = conn.Close()
		return
	}
	if expectedHost != "" && !httpTransportHostMatches(req.Host, expectedHost) {
		_, _ = conn.Write([]byte("HTTP/1.1 400 Bad Request\r\nConnection: close\r\n\r\n"))
		_ = conn.Close()
		return
	}

	if _, err := conn.Write([]byte("HTTP/1.1 200 OK\r\nCache-Control: no-store\r\n\r\n")); err != nil {
		_ = conn.Close()
		return
	}

	upstream, err := net.Dial("tcp", dialAddr)
	if err != nil {
		_ = conn.Close()
		return
	}
	defer conn.Close()
	defer upstream.Close()

	go func() {
		_, _ = io.Copy(upstream, io.MultiReader(req.Body, br))
		_ = closeWrite(upstream)
	}()
	_, _ = io.Copy(conn, upstream)
}

func httpTransportPath(nc *model.NodeSpec) string {
	if nc == nil || nc.NetworkSettings == nil {
		return ""
	}
	v, ok := nc.NetworkSettings["path"]
	if !ok || v == nil {
		return ""
	}
	path := strings.TrimSpace(fmt.Sprint(v))
	if path == "" || path == "<nil>" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return path
}

func httpTransportHost(nc *model.NodeSpec) string {
	if nc == nil || nc.NetworkSettings == nil {
		return ""
	}
	return firstHostValue(nc.NetworkSettings["host"])
}

func httpTransportHostMatches(got, expected string) bool {
	return normalizeHTTPHost(got) == normalizeHTTPHost(expected)
}

func normalizeHTTPHost(h string) string {
	h = strings.TrimSpace(strings.ToLower(h))
	if h == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return h
}
