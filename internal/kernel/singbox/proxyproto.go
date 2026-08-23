package singbox

import (
	"io"
	"net"
	"strconv"
	"strings"

	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/pires/go-proxyproto"
)

// proxyProtocolProxy sits in front of sing-box when the panel sets
// acceptProxyProtocol. sing-box 1.6 removed native PROXY protocol (setting
// proxy_protocol makes ListenTCP fail), so the wrapper reads v1/v2 headers
// and forwards the remaining bytes to an internal inbound.
type proxyProtocolProxy struct {
	ln           net.Listener
	internalPort int
}

func maybeStartProxyProtocolProxy(nc *model.NodeSpec) (*model.NodeSpec, *proxyProtocolProxy, error) {
	if !shouldWrapProxyProtocol(nc) {
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
	ppLn := &proxyproto.Listener{
		Listener: publicLn,
		Policy: func(net.Addr) (proxyproto.Policy, error) {
			return proxyproto.REQUIRE, nil
		},
	}
	proxy := &proxyProtocolProxy{ln: ppLn, internalPort: internalPort}
	go proxy.serve(net.JoinHostPort("127.0.0.1", strconv.Itoa(internalPort)))
	return innerNodeSpec(nc, internalPort), proxy, nil
}

func shouldWrapProxyProtocol(nc *model.NodeSpec) bool {
	if nc == nil || !nc.GetProxyProtocol() {
		return false
	}
	switch strings.ToLower(nc.Protocol) {
	case "hysteria", "hysteria2", "tuic", "mieru":
		return false
	}
	return true
}

func innerNodeSpec(nc *model.NodeSpec, internalPort int) *model.NodeSpec {
	cp := *nc
	cp.ListenIP = "127.0.0.1"
	cp.ServerPort = internalPort
	cp.AcceptProxyProtocol = false
	if nc.NetworkSettings != nil {
		ns := make(map[string]any, len(nc.NetworkSettings))
		for k, v := range nc.NetworkSettings {
			if k == "acceptProxyProtocol" {
				continue
			}
			ns[k] = v
		}
		cp.NetworkSettings = ns
	}
	return &cp
}

func (p *proxyProtocolProxy) serve(dialAddr string) {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		go spliceProxyProtocolConn(conn, dialAddr)
	}
}

func spliceProxyProtocolConn(conn net.Conn, dialAddr string) {
	defer conn.Close()
	upstream, err := net.Dial("tcp", dialAddr)
	if err != nil {
		return
	}
	defer upstream.Close()
	errc := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, conn)
		errc <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, upstream)
		errc <- struct{}{}
	}()
	<-errc
}

func closeProxyProtocolProxy(p *proxyProtocolProxy) {
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

func stripDeprecatedProxyProtocol(cfg M) {
	raw, ok := cfg["inbounds"]
	if !ok {
		return
	}
	inbounds, ok := raw.([]M)
	if !ok {
		return
	}
	for _, inbound := range inbounds {
		delete(inbound, "proxy_protocol")
		delete(inbound, "proxy_protocol_accept_no_header")
	}
}
