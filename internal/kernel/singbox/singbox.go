package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/include"
	singLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
	singJSON "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/service"
	"golang.org/x/time/rate"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/geodata"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/hy2inbound"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/tuicinbound"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/vlessinbound"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"

	singInbound "github.com/sagernet/sing-box/adapter/inbound"
)

// drainTimeout is how long stop() waits for in-flight connections to finish
// naturally after closing all listen sockets, before hard-killing them.
const drainTimeout = 5 * time.Second

// SingBox implements kernel.Kernel by embedding sing-box as a Go library.
//
// User operations (AddUsers/RemoveUsers/UpdateUsers) use sing-box's native
// UpdatableInbound interface, which hot-swaps user credentials without
// restarting listeners — zero connection disruption.
type SingBox struct {
	cfg config.KernelConfig

	mu     sync.RWMutex
	box    *box.Box
	ctx    context.Context
	cancel context.CancelFunc

	users      []model.UserSpec
	nodeConfig *model.NodeSpec
	tls        kernel.TLSCert

	// connTracker is our lightweight in-process byte/IP tracker.
	// Created fresh on every Start (full restart).
	// Survives Reload (hot-swap) since live connections persist.
	connTracker *ConnTracker

	// speedLimitFunc resolves a user UUID to a *rate.Limiter.
	// Set once by SetSpeedLimitFunc and forwarded to every new ConnTracker.
	speedLimitFunc func(string) *rate.Limiter

	// deviceLimitFunc resolves a user UUID to (limit, hasLimit) for gate-keeping.
	// Set once by SetDeviceLimitFunc and forwarded to every new ConnTracker.
	deviceLimitFunc func(string) (int, bool)

	// trackerRegistered prevents duplicate AppendTracker calls on the same
	// Router instance during Reload. Reset to false on full restart.
	trackerRegistered bool

	// ppProxy accepts PROXY protocol in front of sing-box (1.6 removed native PP).
	ppProxy *proxyProtocolProxy
}

func New(cfg config.KernelConfig) *SingBox {
	return &SingBox{cfg: cfg}
}

var _ kernel.Kernel = (*SingBox)(nil)

func (s *SingBox) Name() string { return "sing-box" }

func (s *SingBox) Capabilities() kernel.Capabilities {
	return kernel.Capabilities{
		PerUserSpeedLimit:    true,
		DeviceLimit:          true,
		BuiltInTrafficStats:  false,
		AliveIPTracking:      true,
		ForceCloseConnection: false,
		ForceCloseUser:       true,
	}
}

func (s *SingBox) Protocols() []string {
	return []string{
		"vmess", "vless", "trojan", "shadowsocks",
		"hysteria", "hysteria2", "tuic", "naive", "socks", "http", "anytls", "mieru",
	}
}

func (s *SingBox) Start(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.ensureGeoData(nodeConfig)

	closeProxyProtocolProxy(s.ppProxy)
	s.ppProxy = nil

	listenSpec, proxy, err := maybeStartProxyProtocolProxy(nodeConfig)
	if err != nil {
		return err
	}

	cfgMap := buildConfig(s.cfg, listenSpec, users, tls)
	stripHy2HopListenFields(cfgMap)
	stripDeprecatedProxyProtocol(cfgMap)
	data, err := json.Marshal(cfgMap)
	if err != nil {
		closeProxyProtocolProxy(proxy)
		return fmt.Errorf("marshal config: %w", err)
	}

	nlog.Core().Debug("sing-box config generated", "len", len(data))

	ctx, cancel := context.WithCancel(context.Background())
	ctx = include.Context(ctx)
	overrideHy2TUICInbounds(ctx)
	ctx = hy2inbound.WithRange(ctx, hy2HopRange(nodeConfig))

	opts, err := singJSON.UnmarshalExtendedContext[option.Options](ctx, data)
	if err != nil {
		closeProxyProtocolProxy(proxy)
		cancel()
		return fmt.Errorf("parse sing-box options: %w", err)
	}

	// Save old state before creating the new instance.
	oldBox := s.box
	oldCancel := s.cancel
	oldCtx := s.ctx
	oldTracker := s.connTracker

	instance, err := box.New(box.Options{
		Context: ctx,
		Options: opts,
	})
	if err != nil {
		closeProxyProtocolProxy(proxy)
		cancel()
		return fmt.Errorf("create sing-box instance: %w", err)
	}

	// Kernel.Start 合約：已在跑時先放掉舊 listener，同埠／改協議才能再 bind。
	// 必須同步關舊 inbound，不能等 recycleOldBox，否則新核會 address already in use。
	if oldCtx != nil {
		releaseInboundListeners(oldCtx)
	}

	if err := instance.Start(); err != nil {
		closeProxyProtocolProxy(proxy)
		instance.Close()
		cancel()
		return fmt.Errorf("start sing-box: %w", err)
	}

	// New instance started successfully — swap state.
	s.box = instance
	s.ctx = ctx
	s.cancel = cancel
	s.users = users
	s.nodeConfig = nodeConfig
	s.tls = tls

	// Fresh tracker on full restart.
	s.connTracker = NewConnTracker(0)
	s.connTracker.SetUserMap(buildUserMap(users))
	if s.speedLimitFunc != nil {
		s.connTracker.SetSpeedLimitFunc(s.speedLimitFunc)
	}
	if s.deviceLimitFunc != nil {
		s.connTracker.SetDeviceLimitFunc(s.deviceLimitFunc)
	}

	s.trackerRegistered = false
	s.ppProxy = proxy
	s.registerTracker(ctx)

	// Recycle old instance in background — listeners already released above.
	if oldBox != nil {
		go recycleOldBox(oldBox, oldCancel, oldCtx, oldTracker)
	}

	nlog.Core().Debug("sing-box started", "users", len(users))
	return nil
}

// ensureGeoData downloads geo databases when any route source references
// geoip/geosite, including CustomRouteRules / CustomRoutes / kernel CustomRoute.
func (s *SingBox) ensureGeoData(nc *model.NodeSpec) {
	needIP, needSite := kernel.NeedsGeo(nc, s.cfg.CustomRoute)
	if !needIP && !needSite {
		return
	}
	if err := geodata.Ensure(s.cfg.GeoDataDir, needIP, needSite, "singbox"); err != nil {
		nlog.Core().Warn("geo database unavailable", "error", err)
	}
}

func releaseInboundListeners(ctx context.Context) {
	if ctx == nil {
		return
	}
	if im := service.FromContext[adapter.InboundManager](ctx); im != nil {
		_ = im.Close()
	}
}

// recycleOldBox shuts down a previous instance in the background after
// Start has already released its listeners.
func recycleOldBox(oldBox *box.Box, oldCancel context.CancelFunc, oldCtx context.Context, oldTracker *ConnTracker) {
	// Step 1: close listen sockets (no-op if Start already released them).
	releaseInboundListeners(oldCtx)

	// Step 2: drain in-flight connections (best-effort).
	if oldTracker != nil {
		deadline := time.Now().Add(drainTimeout)
		for time.Now().Before(deadline) {
			if oldTracker.ActiveCount() == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Step 3: hard-close everything.
	oldBox.Close()
	if oldCancel != nil {
		oldCancel()
	}
	nlog.Core().Debug("sing-box: old instance recycled")
}

// Reload hot-swaps the inbound users and routing rules without restarting the box.
// Routes, outbounds, and the connTracker stay alive so in-flight connections
// continue to be tracked correctly.
func (s *SingBox) Reload(nodeConfig *model.NodeSpec, users []model.UserSpec, tls kernel.TLSCert) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return fmt.Errorf("not running")
	}

	listenSpec := nodeConfig
	switch {
	case nodeConfig.GetProxyProtocol() && s.ppProxy == nil, !nodeConfig.GetProxyProtocol() && s.ppProxy != nil:
		s.mu.Unlock()
		err := s.Start(nodeConfig, users, tls)
		s.mu.Lock()
		return err
	case s.ppProxy != nil:
		listenSpec = innerNodeSpec(nodeConfig, s.ppProxy.internalPort)
	}

	cfgMap := buildConfig(s.cfg, listenSpec, users, tls)
	stripHy2HopListenFields(cfgMap)
	stripDeprecatedProxyProtocol(cfgMap)
	data, err := json.Marshal(cfgMap)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	opts, err := singJSON.UnmarshalExtendedContext[option.Options](s.ctx, data)
	if err != nil {
		return fmt.Errorf("parse options: %w", err)
	}

	im := service.FromContext[adapter.InboundManager](s.ctx)
	if im == nil {
		return fmt.Errorf("inbound manager not available")
	}

	router := service.FromContext[adapter.Router](s.ctx)
	if router == nil {
		return fmt.Errorf("router not available")
	}

	// Update routing rules
	if err := router.UpdateRules(opts.Route.Rules, opts.Route.RuleSet); err != nil {
		nlog.Core().Debug("routing reload failed", "error", err)
	} else {
		nlog.Core().Debug("sing-box routing reloaded")
	}

	nopFactory := singLog.NewNOPFactory()

	// Configuration hash check for inbound reconstruction
	tlsChanged := !bytes.Equal(s.tls.CertPEM, tls.CertPEM) || !bytes.Equal(s.tls.KeyPEM, tls.KeyPEM)
	configChanged := tlsChanged || s.nodeConfig == nil || kernel.ComputeHash(nodeConfig, users) != kernel.ComputeHash(s.nodeConfig, s.users)

	wanted := make(map[string]struct{}, len(opts.Inbounds))
	for _, inb := range opts.Inbounds {
		wanted[inb.Tag] = struct{}{}
	}
	// 協議 tag 會變（vmess-in → vless-in）。只 Remove 新 tag 會留下舊 inbound
	// 占著舊埠，同 node_id 熱更新就 address already in use。
	if configChanged {
		removeStaleInbounds(im, wanted)
	}

	for _, inb := range opts.Inbounds {
		tag := inb.Tag
		if !configChanged {
			if existing, ok := im.Get(tag); ok && existing.Type() == inb.Type {
				var err error
				switch v := existing.(type) {
				case adapter.UpdatableInbound[option.VMessUser]:
					if opts, ok := inb.Options.(*option.VMessInboundOptions); ok {
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.VLESSUser]:
					if opts, ok := inb.Options.(*option.VLESSInboundOptions); ok {
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.TrojanUser]:
					if opts, ok := inb.Options.(*option.TrojanInboundOptions); ok {
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.Hysteria2User]:
					if opts, ok := inb.Options.(*option.Hysteria2InboundOptions); ok {
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableShadowsocksInbound:
					if opts, ok := inb.Options.(*option.ShadowsocksInboundOptions); ok {
						err = v.UpdateUsersByOptions(opts.Users)
					}
				case adapter.UpdatableInbound[option.TUICUser]:
					if opts, ok := inb.Options.(*option.TUICInboundOptions); ok {
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.AnyTLSUser]:
					if opts, ok := inb.Options.(*option.AnyTLSInboundOptions); ok {
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[option.MieruUser]:
					if opts, ok := inb.Options.(*option.MieruInboundOptions); ok {
						err = v.UpdateUsers(opts.Users)
					}
				case adapter.UpdatableInbound[auth.User]:
					switch opts := inb.Options.(type) {
					case *option.NaiveInboundOptions:
						err = v.UpdateUsers(opts.Users)
					case *option.SocksInboundOptions:
						err = v.UpdateUsers(opts.Users)
					case *option.HTTPMixedInboundOptions:
						err = v.UpdateUsers(opts.Users)
					}
				}
				if err == nil {
					continue
				}
				nlog.Core().Warn("incremental update failed, falling back to recreate", "tag", tag, "error", err)
			}
		}

		// Config mismatch or incremental update failure: Reconstruct Inbound.
		// Remove the existing inbound first so im.Create() can bind the same port.
		// im.Create() cannot atomically swap a TCP listener — it tries to start the
		// new socket before the old one is closed, causing "address already in use".
		// The brief listen gap (< 1 ms) is far less disruptive than a full restart.
		_ = im.Remove(tag) // ignore error when tag doesn't exist yet
		logger := nopFactory.NewLogger(fmt.Sprintf("inbound/%s[%s]", inb.Type, tag))
		if err := im.Create(hy2inbound.WithRange(s.ctx, hy2HopRange(nodeConfig)), router, logger, tag, inb.Type, inb.Options); err != nil {
			return fmt.Errorf("recreate inbound %s: %w", tag, err)
		}
	}

	// Trackers remain registered on the Router (which survives ReloadUsers).
	// Only update the user map — do NOT re-register or traffic is double-counted.
	if s.connTracker != nil {
		s.connTracker.SetUserMap(buildUserMap(users))
	}

	nlog.Core().Debug("sing-box reloaded", "users", len(users))
	s.users = users
	s.nodeConfig = nodeConfig
	s.tls = tls
	return nil
}

func removeStaleInbounds(im adapter.InboundManager, wanted map[string]struct{}) {
	if im == nil {
		return
	}
	var stale []string
	for _, existing := range im.Inbounds() {
		if existing == nil {
			continue
		}
		tag := existing.Tag()
		if _, keep := wanted[tag]; !keep {
			stale = append(stale, tag)
		}
	}
	for _, tag := range stale {
		if err := im.Remove(tag); err != nil {
			nlog.Core().Debug("sing-box remove stale inbound", "tag", tag, "error", err)
			continue
		}
		nlog.Core().Debug("sing-box released stale inbound", "tag", tag)
	}
}

// registerTracker wires the ConnTracker to the current Router exactly once.
// The ConnTracker handles both byte counting and optional rate limiting
// in a single wrapper, so no additional trackers are needed.
func (s *SingBox) registerTracker(ctx context.Context) {
	if s.trackerRegistered {
		return
	}
	router := service.FromContext[adapter.Router](ctx)
	if router == nil {
		return
	}
	if s.connTracker != nil {
		router.AppendTracker(s.connTracker)
	}
	s.trackerRegistered = true
}

func (s *SingBox) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop()
}

func (s *SingBox) stop() {
	closeProxyProtocolProxy(s.ppProxy)
	s.ppProxy = nil

	if s.box == nil {
		return
	}

	// Step 1: close all listen sockets so no new connections are accepted.
	// im.Close() sets m.started=false so the subsequent box.Close() call is a no-op
	// for inbounds (avoids double-close), but box.Close() still tears down routers
	// and outbounds.
	if im := service.FromContext[adapter.InboundManager](s.ctx); im != nil {
		_ = im.Close()
	}

	// Step 2: wait for in-flight connections to drain naturally (best-effort).
	if s.connTracker != nil {
		deadline := time.Now().Add(drainTimeout)
		for time.Now().Before(deadline) {
			if s.connTracker.ActiveCount() == 0 {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Step 3: hard-close everything that is still open.
	s.box.Close()
	s.box = nil
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.ctx = nil
}

func (s *SingBox) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.box != nil
}

// connTrackerLocked returns the connTracker under a read-optimised lock.
func (s *SingBox) connTrackerSafe() *ConnTracker {
	s.mu.Lock()
	ct := s.connTracker
	s.mu.Unlock()
	return ct
}

// SetSpeedLimitFunc configures per-user bandwidth throttling.
func (s *SingBox) SetSpeedLimitFunc(fn func(uuid string) *rate.Limiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.speedLimitFunc = fn
	if s.connTracker != nil {
		s.connTracker.SetSpeedLimitFunc(fn)
	}
}

// SetDeviceLimitFunc configures per-user device limit gate-keeping.
// Connections exceeding the limit are rejected at connect time.
func (s *SingBox) SetDeviceLimitFunc(fn func(uuid string) (int, bool)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deviceLimitFunc = fn
	if s.connTracker != nil {
		s.connTracker.SetDeviceLimitFunc(fn)
	}
}

// UpdateGlobalDevices updates the global device state from panel (for multi-node).
func (s *SingBox) UpdateGlobalDevices(users map[int][]string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.connTracker != nil {
		s.connTracker.UpdateGlobalDevices(users)
	}
}

// ClearGlobalDevices clears the global device state (on WS disconnect).
func (s *SingBox) ClearGlobalDevices() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.connTracker != nil {
		s.connTracker.ClearGlobalDevices()
	}
}

// ─── User management (non-disruptive) ───────────────────────────────────────

// AddUsers hot-swaps users into running inbounds. Zero connection disruption.
// Same panel ID with a new UUID (reset subscription / plan change) replaces
// the old identity — ID-only de-dup would skip the inbound write and leave
// sing-box logging "unknown UUID".
func (s *SingBox) AddUsers(users []model.UserSpec) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return 0, fmt.Errorf("not running")
	}
	if len(users) == 0 {
		return 0, nil
	}

	merged := mergeUsersByID(s.users, users)
	toAdd, toRemove := kernel.UserDiff(s.users, merged)
	// Bookkeeping may already have the new UUID while the inbound still
	// holds the old one. Always hot-write the inbound for the requested set.
	if err := s.reloadInboundsLocked(merged); err != nil {
		return 0, err
	}
	s.closeRemovedUsersLocked(toRemove)
	s.users = merged
	added := len(toAdd)
	if added == 0 {
		added = len(users)
	}
	nlog.Core().Info("sing-box: users added via hot-swap",
		"added", added, "removed", len(toRemove), "total", len(merged), "restart", false)
	return added, nil
}

// RemoveUsers hot-swaps users out of running inbounds. Zero connection disruption
// for remaining users.
func (s *SingBox) RemoveUsers(users []model.UserSpec) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return 0, fmt.Errorf("not running")
	}

	removeSet := make(map[int]struct{}, len(users))
	for _, u := range users {
		removeSet[u.ID] = struct{}{}
	}
	var kept []model.UserSpec
	removed := 0
	for _, u := range s.users {
		if _, rm := removeSet[u.ID]; rm {
			removed++
		} else {
			kept = append(kept, u)
		}
	}
	if removed == 0 {
		return 0, nil
	}

	if err := s.reloadInboundsLocked(kept); err != nil {
		return 0, err
	}
	s.closeRemovedUsersLocked(users)
	s.users = kept
	nlog.Core().Info("sing-box: users removed via hot-swap",
		"removed", removed, "total", len(kept), "restart", false)
	return removed, nil
}

// UpdateUsers replaces the entire user set atomically via hot-swap.
func (s *SingBox) UpdateUsers(users []model.UserSpec) (added, removed int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.box == nil {
		return 0, 0, fmt.Errorf("not running")
	}

	toAdd, toRemove := kernel.UserDiff(s.users, users)
	added, removed = len(toAdd), len(toRemove)

	if added == 0 && removed == 0 {
		// Only limits may have changed — update tracker map.
		if s.connTracker != nil {
			s.connTracker.SetUserMap(buildUserMap(users))
		}
		s.users = users
		return 0, 0, nil
	}

	if err = s.reloadInboundsLocked(users); err != nil {
		return 0, 0, err
	}
	s.closeRemovedUsersLocked(toRemove)
	s.users = users
	nlog.Core().Info("sing-box: users updated via hot-swap",
		"added", added, "removed", removed, "total", len(users), "restart", false)
	return
}

func (s *SingBox) closeRemovedUsersLocked(removed []model.UserSpec) {
	if s.connTracker == nil {
		return
	}
	for _, u := range removed {
		if u.UUID == "" {
			continue
		}
		s.connTracker.CloseByUUID(u.UUID)
	}
}

func mergeUsersByID(base, overlay []model.UserSpec) []model.UserSpec {
	if len(overlay) == 0 {
		out := make([]model.UserSpec, len(base))
		copy(out, base)
		return out
	}
	index := make(map[int]int, len(base)+len(overlay))
	out := make([]model.UserSpec, 0, len(base)+len(overlay))
	for _, u := range base {
		index[u.ID] = len(out)
		out = append(out, u)
	}
	for _, u := range overlay {
		if i, ok := index[u.ID]; ok {
			out[i] = u
			continue
		}
		index[u.ID] = len(out)
		out = append(out, u)
	}
	return out
}

// reloadInboundsLocked hot-swaps inbound users using UpdatableInbound.
// Must be called with s.mu held.
func (s *SingBox) reloadInboundsLocked(users []model.UserSpec) error {
	listenSpec := s.nodeConfig
	if s.ppProxy != nil {
		listenSpec = innerNodeSpec(s.nodeConfig, s.ppProxy.internalPort)
	}
	cfgMap := buildConfig(s.cfg, listenSpec, users, s.tls)
	stripHy2HopListenFields(cfgMap)
	stripDeprecatedProxyProtocol(cfgMap)
	data, err := json.Marshal(cfgMap)
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	opts, err := singJSON.UnmarshalExtendedContext[option.Options](s.ctx, data)
	if err != nil {
		return fmt.Errorf("parse options: %w", err)
	}

	im := service.FromContext[adapter.InboundManager](s.ctx)
	if im == nil {
		return fmt.Errorf("inbound manager not available")
	}

	router := service.FromContext[adapter.Router](s.ctx)
	if router == nil {
		return fmt.Errorf("router not available")
	}

	nopFactory := singLog.NewNOPFactory()

	for _, inb := range opts.Inbounds {
		tag := inb.Tag
		if existing, ok := im.Get(tag); ok && existing.Type() == inb.Type {
			var err error
			switch v := existing.(type) {
			case adapter.UpdatableInbound[option.VMessUser]:
				if opts, ok := inb.Options.(*option.VMessInboundOptions); ok {
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.VLESSUser]:
				if opts, ok := inb.Options.(*option.VLESSInboundOptions); ok {
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.TrojanUser]:
				if opts, ok := inb.Options.(*option.TrojanInboundOptions); ok {
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.Hysteria2User]:
				if opts, ok := inb.Options.(*option.Hysteria2InboundOptions); ok {
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableShadowsocksInbound:
				if opts, ok := inb.Options.(*option.ShadowsocksInboundOptions); ok {
					err = v.UpdateUsersByOptions(opts.Users)
				}
			case adapter.UpdatableInbound[option.TUICUser]:
				if opts, ok := inb.Options.(*option.TUICInboundOptions); ok {
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.AnyTLSUser]:
				if opts, ok := inb.Options.(*option.AnyTLSInboundOptions); ok {
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[option.MieruUser]:
				if opts, ok := inb.Options.(*option.MieruInboundOptions); ok {
					err = v.UpdateUsers(opts.Users)
				}
			case adapter.UpdatableInbound[auth.User]:
				switch opts := inb.Options.(type) {
				case *option.NaiveInboundOptions:
					err = v.UpdateUsers(opts.Users)
				case *option.SocksInboundOptions:
					err = v.UpdateUsers(opts.Users)
				case *option.HTTPMixedInboundOptions:
					err = v.UpdateUsers(opts.Users)
				}
			}
			if err == nil {
				continue
			}
			nlog.Core().Warn("incremental update failed, recreating inbound", "tag", tag, "error", err)
		}

		_ = im.Remove(tag)
		logger := nopFactory.NewLogger(fmt.Sprintf("inbound/%s[%s]", inb.Type, tag))
		if err := im.Create(hy2inbound.WithRange(s.ctx, hy2HopRange(s.nodeConfig)), router, logger, tag, inb.Type, inb.Options); err != nil {
			return fmt.Errorf("recreate inbound %s: %w", tag, err)
		}
	}

	if s.connTracker != nil {
		s.connTracker.SetUserMap(buildUserMap(users))
	}

	nlog.Core().Info("sing-box users hot-swapped", "users", len(users), "restart", false)
	return nil
}

// ─── Observability ──────────────────────────────────────────────────────────
func (s *SingBox) GetUserTraffic(_ context.Context) (traffic map[int][2]int64, aliveIPs map[int]map[string]bool, connCount int, err error) {
	ct := s.connTrackerSafe()
	if ct == nil {
		return nil, nil, 0, nil
	}
	traffic, aliveIPs, connCount = ct.GetUserTraffic()
	return traffic, aliveIPs, connCount, nil
}

// CloseConnection force-closes a specific connection by its ID.
func (s *SingBox) CloseConnection(_ context.Context, connID string) error {
	ct := s.connTrackerSafe()
	if ct == nil {
		return fmt.Errorf("not running")
	}
	if !ct.CloseByID(connID) {
		nlog.Core().Debug("CloseConnection: connection not found (already closed?)", "id", connID)
	}
	return nil
}

// CloseUserConnections terminates all connections for a user UUID.
func (s *SingBox) CloseUserConnections(_ context.Context, uuid string) error {
	ct := s.connTrackerSafe()
	if ct == nil {
		return nil
	}
	ct.CloseByUUID(uuid)
	return nil
}

// buildUserMap creates a UUID→userID mapping used by ConnTracker to attribute
// connections to the correct user. All sing-box protocols use the user's UUID
// as the inbound name/username, so this covers every protocol.
func buildUserMap(users []model.UserSpec) map[string]int {
	m := make(map[string]int, len(users))
	for _, u := range users {
		m[u.UUID] = u.ID
	}
	return m
}

// overrideHy2TUICInbounds swaps cedar2025 Hy2/TUIC/VLESS constructors for the
// Gboard-Node copies. VLESS 覆寫是為了 ws Host 必須在 listener 核對。
// Must run after include.Context.
func overrideHy2TUICInbounds(ctx context.Context) {
	reg, ok := service.FromContext[adapter.InboundRegistry](ctx).(*singInbound.Registry)
	if !ok {
		nlog.Core().Warn("hy2/tuic inbound override skipped: registry type mismatch")
		return
	}
	hy2inbound.RegisterInbound(reg)
	tuicinbound.RegisterInbound(reg)
	vlessinbound.RegisterInbound(reg)
}

func hy2HopRange(nc *model.NodeSpec) hy2inbound.Range {
	if nc == nil {
		return hy2inbound.Range{}
	}
	start, end, ok := parseListenPortRange(nc.ServerPortRange)
	if !ok {
		return hy2inbound.Range{}
	}
	return hy2inbound.Range{Start: start, End: end}
}

func stripHy2HopListenFields(cfg M) {
	raw, ok := cfg["inbounds"]
	if !ok {
		return
	}
	inbounds, ok := raw.([]M)
	if !ok {
		return
	}
	for _, inbound := range inbounds {
		delete(inbound, "listen_port_range")
		delete(inbound, "listen_ports")
	}
}
