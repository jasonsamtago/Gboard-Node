// Package ssinbound 覆寫官方 Shadowsocks inbound：面板 plugin／opts 用
// sing-box 官方 V2Ray websocket transport 真帶上，不准 ignoring。
package ssinbound

import (
	"context"
	"net"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/shadowsocks"
	"github.com/sagernet/sing-box/transport/v2ray"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// InboundOptions 在官方 SS inbound 字段上加 plugin／transport。
// 未知 JSON 欄位會被 sing-box DisallowUnknownFields 拒收，必須登記此型別。
type InboundOptions struct {
	option.ShadowsocksInboundOptions
	Transport  *option.V2RayTransportOptions `json:"transport,omitempty"`
	Plugin     string                        `json:"plugin,omitempty"`
	PluginOpts string                        `json:"plugin_opts,omitempty"`
}

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[InboundOptions](registry, C.TypeShadowsocks, NewInbound)
}

// UsersFromOptions 給熱更新抽 users；同時認官方與本覆寫 options。
func UsersFromOptions(opts any) ([]option.ShadowsocksUser, bool) {
	switch o := opts.(type) {
	case *option.ShadowsocksInboundOptions:
		return o.Users, true
	case *InboundOptions:
		return o.Users, true
	default:
		return nil, false
	}
}

var (
	_ adapter.Inbound                     = (*Inbound)(nil)
	_ adapter.UpdatableShadowsocksInbound = (*Inbound)(nil)
	_ adapter.TCPInjectableInbound        = (*Inbound)(nil)
)

// Inbound 在官方 SS 服務外包 V2Ray websocket transport。
// 無 plugin／transport 時直接回官方 inbound，普通 SS 不回歸。
type Inbound struct {
	inner     adapter.Inbound
	tcp       adapter.TCPInjectableInbound
	ctx       context.Context
	logger    logger.ContextLogger
	listener  *listener.Listener
	transport adapter.V2RayServerTransport
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options InboundOptions) (adapter.Inbound, error) {
	transport := options.Transport
	if transport == nil || transport.Type == "" {
		if options.Plugin != "" {
			derived, err := TransportFromPlugin(options.Plugin, options.PluginOpts)
			if err != nil {
				return nil, err
			}
			transport = derived
		}
	}

	inner, err := shadowsocks.NewInbound(ctx, router, logger, tag, options.ShadowsocksInboundOptions)
	if err != nil {
		return nil, err
	}
	if transport == nil || transport.Type == "" {
		return inner, nil
	}

	tcp, ok := inner.(adapter.TCPInjectableInbound)
	if !ok {
		_ = inner.Close()
		return nil, E.New("shadowsocks inbound is not TCP injectable")
	}

	in := &Inbound{
		inner:  inner,
		tcp:    tcp,
		ctx:    ctx,
		logger: logger,
	}
	in.transport, err = v2ray.NewServerTransport(ctx, logger, common.PtrValueOrDefault(transport), nil, (*transportHandler)(in))
	if err != nil {
		_ = inner.Close()
		return nil, E.Cause(err, "create shadowsocks plugin transport: ", transport.Type)
	}
	in.listener = listener.New(listener.Options{
		Context: ctx,
		Logger:  logger,
		Network: []string{N.NetworkTCP},
		Listen:  options.ListenOptions,
	})
	return in, nil
}

func (h *Inbound) Type() string { return h.inner.Type() }

func (h *Inbound) Tag() string { return h.inner.Tag() }

func (h *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	if h.transport == nil {
		return h.inner.Start(stage)
	}
	if !common.Contains(h.transport.Network(), N.NetworkTCP) {
		return E.New("shadowsocks plugin transport has no TCP")
	}
	tcpListener, err := h.listener.ListenTCP()
	if err != nil {
		return err
	}
	go func() {
		sErr := h.transport.Serve(tcpListener)
		if sErr != nil && !E.IsClosed(sErr) {
			h.logger.Error("shadowsocks plugin transport serve error: ", sErr)
		}
	}()
	return nil
}

func (h *Inbound) Close() error {
	return common.Close(h.transport, h.listener, h.inner)
}

func (h *Inbound) UpdateUsersByOptions(users []option.ShadowsocksUser) error {
	if u, ok := h.inner.(adapter.UpdatableShadowsocksInbound); ok {
		return u.UpdateUsersByOptions(users)
	}
	return E.New("inner shadowsocks inbound does not support user update")
}

func (h *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	h.tcp.NewConnectionEx(ctx, conn, metadata, onClose)
}

var _ adapter.V2RayServerTransportHandler = (*transportHandler)(nil)

type transportHandler Inbound

func (h *transportHandler) NewConnectionEx(ctx context.Context, conn net.Conn, source M.Socksaddr, destination M.Socksaddr, onClose N.CloseHandlerFunc) {
	var metadata adapter.InboundContext
	metadata.Source = source
	metadata.Destination = destination
	//nolint:staticcheck
	if h.listener != nil {
		metadata.InboundDetour = h.listener.ListenOptions().Detour
	}
	h.logger.InfoContext(ctx, "inbound connection from ", metadata.Source)
	(*Inbound)(h).NewConnectionEx(ctx, conn, metadata, onClose)
}
