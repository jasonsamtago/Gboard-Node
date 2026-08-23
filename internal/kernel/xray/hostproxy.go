package xray

import (
	"io"
	"net"
	"strconv"

	"github.com/jasonsamtago/Gboard-Node/internal/kernel/hostfilter"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
)

// hostProxy sits in front of xray tcp+http so request Host is enforced.
// xray 的 HTTP disguise 只核對 path，不核對 Host；設了 Host 時必須擋錯 Host。
type hostProxy struct {
	ln net.Listener
}

func maybeStartTCPHTTPHostProxy(nc *model.NodeSpec) (*model.NodeSpec, *hostProxy, error) {
	host := extractTCPHTTPHost(nc)
	if host == "" {
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
	go proxy.serve(net.JoinHostPort("127.0.0.1", strconv.Itoa(internalPort)), host)

	cp := *nc
	cp.ListenIP = "127.0.0.1"
	cp.ServerPort = internalPort
	return &cp, proxy, nil
}

func (p *hostProxy) serve(dialAddr, expectedHost string) {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go handleHostProxyConn(conn, dialAddr, expectedHost)
	}
}

func handleHostProxyConn(conn net.Conn, dialAddr, expectedHost string) {
	checked, ok := hostfilter.Accept(conn, expectedHost)
	if !ok {
		_ = conn.Close()
		return
	}
	upstream, err := net.Dial("tcp", dialAddr)
	if err != nil {
		_ = checked.Close()
		return
	}
	defer checked.Close()
	defer upstream.Close()
	go func() {
		_, _ = io.Copy(upstream, checked)
		_ = closeWrite(upstream)
	}()
	_, _ = io.Copy(checked, upstream)
}

func closeWrite(conn net.Conn) error {
	type closer interface {
		CloseWrite() error
	}
	if c, ok := conn.(closer); ok {
		return c.CloseWrite()
	}
	return conn.Close()
}

func closeHostProxy(p *hostProxy) {
	if p == nil || p.ln == nil {
		return
	}
	_ = p.ln.Close()
}

func publicListenHostPort(nc *model.NodeSpec) string {
	ip := nc.ListenIP
	if ip == "" {
		ip = "::"
	}
	return net.JoinHostPort(ip, strconv.Itoa(nc.ServerPort))
}

func extractTCPHTTPHost(nc *model.NodeSpec) string {
	if nc == nil || nc.Network != "tcp" || nc.NetworkSettings == nil {
		return ""
	}
	header, _ := nc.NetworkSettings["header"].(map[string]any)
	if header == nil {
		return ""
	}
	typ, _ := header["type"].(string)
	if !stringsEqualFoldHTTP(typ) {
		return ""
	}
	if request, _ := header["request"].(map[string]any); request != nil {
		if headers, _ := request["headers"].(map[string]any); headers != nil {
			if host := firstHostValue(headers["Host"]); host != "" {
				return host
			}
		}
	}
	return ""
}

func stringsEqualFoldHTTP(typ string) bool {
	return typ == "http" || typ == "HTTP"
}

func firstHostValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []any:
		if len(x) > 0 {
			if s, ok := x[0].(string); ok {
				return s
			}
		}
	case []string:
		if len(x) > 0 {
			return x[0]
		}
	}
	return ""
}
