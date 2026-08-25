// Package sshinbound 註冊 sing-box inbound type=ssh。
// 官方 1.13 只有 SSH outbound；面板 protocol=ssh 必須真聽埠並能握手，
// 所以用官方 type 名／user／password／host_key 欄位補 inbound。
package sshinbound

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/inbound"
	"github.com/sagernet/sing-box/common/listener"
	"github.com/sagernet/sing-box/common/uot"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	"github.com/sagernet/sing/common/json/badoption"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"golang.org/x/crypto/ssh"
)

const TypeSSH = C.TypeSSH

// InboundOptions 對齊官方 SSH 欄位：users（name／password／private_key）＋ host_key。
type InboundOptions struct {
	option.ListenOptions
	Users   []User                     `json:"users,omitempty"`
	HostKey badoption.Listable[string] `json:"host_key,omitempty"`
}

type User struct {
	Name       string `json:"name,omitempty"`
	Username   string `json:"username,omitempty"`
	Password   string `json:"password,omitempty"`
	PrivateKey string `json:"private_key,omitempty"`
}

func (u User) login() string {
	if name := strings.TrimSpace(u.Username); name != "" {
		return name
	}
	return strings.TrimSpace(u.Name)
}

func RegisterInbound(registry *inbound.Registry) {
	inbound.Register[InboundOptions](registry, TypeSSH, NewInbound)
}

// Validate 缺 user／password／private key 必須明確錯誤，不准 Start 成功當空核。
func Validate(protocol string, users []model.UserSpec, serverKey string) error {
	if !strings.EqualFold(strings.TrimSpace(protocol), TypeSSH) {
		return nil
	}
	if hasAuth(users, serverKey) {
		return nil
	}
	return fmt.Errorf("ssh inbound missing auth: user/password or private key required")
}

func hasAuth(users []model.UserSpec, serverKey string) bool {
	if strings.TrimSpace(serverKey) != "" {
		return true
	}
	for _, u := range users {
		if strings.TrimSpace(u.UUID) != "" {
			return true
		}
	}
	return false
}

var _ adapter.TCPInjectableInbound = (*Inbound)(nil)

type Inbound struct {
	inbound.Adapter
	ctx      context.Context
	router   adapter.ConnectionRouterEx
	logger   logger.ContextLogger
	listener *listener.Listener

	mu     sync.RWMutex
	users  []User
	config *ssh.ServerConfig
}

func NewInbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options InboundOptions) (adapter.Inbound, error) {
	if err := optionsAuthError(options); err != nil {
		return nil, err
	}
	cfg, err := newServerConfig(options)
	if err != nil {
		return nil, err
	}
	in := &Inbound{
		Adapter: inbound.NewAdapter(TypeSSH, tag),
		ctx:     ctx,
		router:  uot.NewRouter(router, logger),
		logger:  logger,
		users:   append([]User(nil), options.Users...),
		config:  cfg,
	}
	cfg.PasswordCallback = in.passwordCallback
	cfg.PublicKeyCallback = in.publicKeyCallback
	in.listener = listener.New(listener.Options{
		Context:           ctx,
		Logger:            logger,
		Network:           []string{N.NetworkTCP},
		Listen:            options.ListenOptions,
		ConnectionHandler: in,
	})
	return in, nil
}

func optionsAuthError(options InboundOptions) error {
	if len(options.HostKey) > 0 {
		for _, key := range options.HostKey {
			if strings.TrimSpace(key) != "" {
				return nil
			}
		}
	}
	for _, u := range options.Users {
		if u.login() != "" && (strings.TrimSpace(u.Password) != "" || strings.TrimSpace(u.PrivateKey) != "") {
			return nil
		}
	}
	return fmt.Errorf("ssh inbound missing auth: user/password or private key required")
}

func newServerConfig(options InboundOptions) (*ssh.ServerConfig, error) {
	cfg := &ssh.ServerConfig{}
	if err := addHostKeys(cfg, options.HostKey); err != nil {
		return nil, err
	}
	return cfg, nil
}

func addHostKeys(cfg *ssh.ServerConfig, keys []string) error {
	added := false
	for _, raw := range keys {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		signer, err := ssh.ParsePrivateKey([]byte(raw))
		if err != nil {
			return E.Cause(err, "parse ssh host_key")
		}
		cfg.AddHostKey(signer)
		added = true
	}
	if added {
		return nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return E.Cause(err, "generate ssh host key")
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return E.Cause(err, "ssh host key signer")
	}
	cfg.AddHostKey(signer)
	return nil
}

func (h *Inbound) passwordCallback(conn ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
	user := strings.TrimSpace(conn.User())
	pass := string(password)
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, u := range h.users {
		if u.login() == user && u.Password != "" && u.Password == pass {
			return &ssh.Permissions{Extensions: map[string]string{"user": user}}, nil
		}
	}
	return nil, fmt.Errorf("ssh auth rejected")
}

func (h *Inbound) publicKeyCallback(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	user := strings.TrimSpace(conn.User())
	want := key.Marshal()
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, u := range h.users {
		if u.login() != user || strings.TrimSpace(u.PrivateKey) == "" {
			continue
		}
		signer, err := ssh.ParsePrivateKey([]byte(u.PrivateKey))
		if err != nil {
			continue
		}
		if string(signer.PublicKey().Marshal()) == string(want) {
			return &ssh.Permissions{Extensions: map[string]string{"user": user}}, nil
		}
	}
	return nil, fmt.Errorf("ssh public key auth rejected")
}

func (h *Inbound) Start(stage adapter.StartStage) error {
	if stage != adapter.StartStateStart {
		return nil
	}
	return h.listener.Start()
}

func (h *Inbound) Close() error {
	return h.listener.Close()
}

func (h *Inbound) NewConnectionEx(ctx context.Context, conn net.Conn, metadata adapter.InboundContext, onClose N.CloseHandlerFunc) {
	sshConn, chans, reqs, err := ssh.NewServerConn(conn, h.config)
	if err != nil {
		N.CloseOnHandshakeFailure(conn, onClose, err)
		if E.IsClosedOrCanceled(err) {
			h.logger.DebugContext(ctx, "ssh handshake closed: ", err)
			return
		}
		h.logger.ErrorContext(ctx, E.Cause(err, "ssh handshake from ", metadata.Source))
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() {
		defer sshConn.Close()
		if onClose != nil {
			defer onClose(nil)
		}
		user := sshConn.User()
		for newCh := range chans {
			go h.handleChannel(ctx, newCh, metadata, user)
		}
	}()
}

func (h *Inbound) handleChannel(ctx context.Context, newCh ssh.NewChannel, inboundMeta adapter.InboundContext, user string) {
	if newCh.ChannelType() != "direct-tcpip" {
		_ = newCh.Reject(ssh.UnknownChannelType, "unsupported")
		return
	}
	host, port, err := parseDirectTCPIP(newCh.ExtraData())
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	ch, reqs, err := newCh.Accept()
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)

	metadata := inboundMeta
	metadata.Inbound = h.Tag()
	metadata.InboundType = h.Type()
	metadata.User = user
	metadata.Network = N.NetworkTCP
	metadata.Destination = M.ParseSocksaddrHostPort(host, port)
	h.logger.InfoContext(ctx, "[", user, "] inbound connection to ", metadata.Destination)
	h.router.RouteConnectionEx(ctx, wrapSSHChannel(ch, inboundMeta.Source), metadata, nil)
}

type directTCPIP struct {
	DestAddr   string
	DestPort   uint32
	OriginAddr string
	OriginPort uint32
}

func parseDirectTCPIP(extra []byte) (string, uint16, error) {
	var payload directTCPIP
	if err := ssh.Unmarshal(extra, &payload); err != nil {
		return "", 0, err
	}
	if payload.DestPort == 0 || payload.DestPort > 65535 {
		return "", 0, fmt.Errorf("invalid dest port %d", payload.DestPort)
	}
	if strings.TrimSpace(payload.DestAddr) == "" {
		return "", 0, fmt.Errorf("empty dest addr")
	}
	return payload.DestAddr, uint16(payload.DestPort), nil
}

type sshNetConn struct {
	ssh.Channel
	local  dummyAddr
	remote net.Addr
}

func wrapSSHChannel(ch ssh.Channel, remote M.Socksaddr) net.Conn {
	return &sshNetConn{Channel: ch, local: "ssh", remote: remote}
}

func (c *sshNetConn) LocalAddr() net.Addr              { return c.local }
func (c *sshNetConn) RemoteAddr() net.Addr             { return c.remote }
func (c *sshNetConn) SetDeadline(time.Time) error      { return nil }
func (c *sshNetConn) SetReadDeadline(time.Time) error  { return nil }
func (c *sshNetConn) SetWriteDeadline(time.Time) error { return nil }

type dummyAddr string

func (d dummyAddr) Network() string { return "tcp" }
func (d dummyAddr) String() string  { return string(d) }
