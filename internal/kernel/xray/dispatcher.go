package xray

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	_ "unsafe"

	xrayDispatcher "github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"

	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// Access xray's internal config creator registry so we can replace the
// default dispatcher factory with ours. This runs AFTER xray's init()
// functions because our package imports xray (dependency order guarantee).
//
//go:linkname typeCreatorRegistry github.com/xtls/xray-core/common.typeCreatorRegistry
var typeCreatorRegistry map[reflect.Type]common.ConfigCreator

var origDispatcherFactory common.ConfigCreator

// globalLimitDispatcher is set when the factory creates a LimitDispatcher.
// The Xray kernel reads it to configure limits and get connections.
var globalLimitDispatcher atomic.Pointer[LimitDispatcher]

func init() {
	configType := reflect.TypeOf((*xrayDispatcher.Config)(nil))
	origDispatcherFactory = typeCreatorRegistry[configType]
	typeCreatorRegistry[configType] = limitDispatcherFactory
}

func limitDispatcherFactory(ctx context.Context, config interface{}) (interface{}, error) {
	orig, err := origDispatcherFactory(ctx, config)
	if err != nil {
		return nil, err
	}
	inner, ok := orig.(routing.Dispatcher)
	if !ok {
		return orig, nil
	}
	ld := &LimitDispatcher{
		inner:      orig,
		innerDisp:  inner,
		limitedIPs: make(map[string]map[string]int),
		conns:      make(map[uint64]*trackedConn),
	}
	globalLimitDispatcher.Store(ld)
	nlog.Core().Debug("xray: limit dispatcher installed")
	return ld, nil
}

// LimitDispatcher wraps xray's DefaultDispatcher to enforce per-user
// admission checks before a request is dispatched into xray-core.
//
// It intentionally does NOT mutate transport.Link.Reader/Writer. Xray's
// mux/XUDP close path requires the original concrete *pipe.Reader to remain
// intact, so the dispatcher is limited to gate-keeping and safe connection
// lifecycle bookkeeping.
type LimitDispatcher struct {
	inner     interface{}        // original DefaultDispatcher (Feature + Dispatcher)
	innerDisp routing.Dispatcher // same object, typed as Dispatcher

	// limitedUsers: users with device limit > 0, protected by mu.
	// Needs deterministic IP ordering for kick decisions.
	mu           sync.RWMutex
	limitedIPs   map[string]map[string]int // email → sourceIP → refcount
	deviceLimits map[string]int            // email → max devices
	emailToUID   map[string]int            // email → panel user ID

	// unlimitedIPs: users without device limit — sync.Map for lock-free access.
	// Each entry is *ipCounter{ips sync.Map}.
	unlimitedIPs sync.Map // email → *ipCounter

	connCount atomic.Int64 // total active connections tracked by dispatcher

	// live: per-link kick targets. Reader is stored, never replaced on the
	// Link (mux/XUDP needs the original *pipe.Reader).
	conns   map[uint64]*trackedConn
	connSeq uint64
}

type trackedConn struct {
	email   string
	writer  *closeTrackingWriter
	reader  buf.Reader
	inbound net.Conn
}

// ipCounter tracks IPs for unlimited users without any lock.
type ipCounter struct {
	ips sync.Map // sourceIP → *atomic.Int64 (refcount)
}

// aliveIPs returns a snapshot of distinct IPs.
func (ic *ipCounter) aliveIPs() map[string]bool {
	result := make(map[string]bool)
	ic.ips.Range(func(key, _ interface{}) bool {
		if rv, ok := ic.ips.Load(key); ok && rv.(*atomic.Int64).Load() > 0 {
			result[key.(string)] = true
		}
		return true
	})
	return result
}

// ─── routing.Dispatcher ──────────────────────────────────────────────────────

func (d *LimitDispatcher) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	email, sourceIP, isTCP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return nil, err
	}

	link, err := d.innerDisp.Dispatch(ctx, dest)
	if err != nil {
		if email != "" && isTCP {
			d.delConn(email, sourceIP)
		}
		return nil, err
	}

	if email != "" {
		d.trackLink(link, email, sourceIP, isTCP, inboundConnFromCtx(ctx))
	}
	return link, nil
}

func (d *LimitDispatcher) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	email, sourceIP, isTCP, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return err
	}

	if email != "" {
		d.trackLink(link, email, sourceIP, isTCP, inboundConnFromCtx(ctx))
	}
	return d.innerDisp.DispatchLink(ctx, dest, link)
}

// identifyAndCheck extracts user identity from the session context, enforces
// device limits, and returns the user's email, source IP, and TCP flag.
// Returns a non-nil error only when the connection should be rejected.
func (d *LimitDispatcher) identifyAndCheck(ctx context.Context, dest net.Destination) (email, sourceIP string, isTCP bool, err error) {
	si := session.InboundFromContext(ctx)
	if si == nil || si.User == nil || len(si.User.Email) == 0 {
		return "", "", false, nil
	}
	email = si.User.Email
	sourceIP = si.Source.Address.IP().String()
	isTCP = dest.Network == net.Network_TCP

	if d.checkDeviceLimit(email, sourceIP, isTCP) {
		nlog.Core().Debug("xray: device limit exceeded", "email", email, "ip", sourceIP)
		return "", "", false, errors.New("device limit exceeded for " + email)
	}
	return email, sourceIP, isTCP, nil
}

func inboundConnFromCtx(ctx context.Context) net.Conn {
	si := session.InboundFromContext(ctx)
	if si == nil {
		return nil
	}
	return si.Conn
}

// trackLink records connection lifecycle so device-limit state is released
// when the link closes. Reader is left intact (mux/XUDP needs *pipe.Reader).
//
// Writer is wrapped with closeTrackingWriter. If the outer writer is already
// *dispatcher.SizeStatWriter, wrap inside it: xray vision splice
// (CopyRawConnIfExist) only increments the outermost SizeStatWriter, so
// hiding that type drops user stats by an order of magnitude.
func (d *LimitDispatcher) trackLink(link *transport.Link, email, sourceIP string, isTCP bool, inbound net.Conn) {
	d.connCount.Add(1)

	tracker := &closeTrackingWriter{}
	d.mu.Lock()
	d.connSeq++
	id := d.connSeq
	if d.conns == nil {
		d.conns = make(map[uint64]*trackedConn)
	}
	d.conns[id] = &trackedConn{
		email:   email,
		writer:  tracker,
		reader:  link.Reader,
		inbound: inbound,
	}
	d.mu.Unlock()

	tracker.onClose = func() {
		if isTCP {
			d.delConn(email, sourceIP)
		}
		d.connCount.Add(-1)
		d.mu.Lock()
		delete(d.conns, id)
		d.mu.Unlock()
	}

	if sw, ok := link.Writer.(*xrayDispatcher.SizeStatWriter); ok {
		tracker.Writer = sw.Writer
		sw.Writer = tracker
		return
	}
	tracker.Writer = link.Writer
	link.Writer = tracker
}

// CloseByEmail force-closes live links for a panel user email (user@<id>).
// Interrupts the tracked writer and original reader, and closes the inbound
// net.Conn when xray exposed it — without replacing link.Reader.
func (d *LimitDispatcher) CloseByEmail(email string) {
	if email == "" || d == nil {
		return
	}
	d.mu.Lock()
	var targets []*trackedConn
	for id, c := range d.conns {
		if c != nil && c.email == email {
			targets = append(targets, c)
			delete(d.conns, id)
		}
	}
	d.mu.Unlock()
	for _, c := range targets {
		if c.writer != nil {
			c.writer.Interrupt()
		}
		if c.reader != nil {
			common.Interrupt(c.reader)
		}
		if c.inbound != nil {
			_ = c.inbound.Close()
		}
	}
}

// ─── features.Feature (delegated) ───────────────────────────────────────────

func (d *LimitDispatcher) Type() interface{} { return routing.DispatcherType() }

func (d *LimitDispatcher) Start() error {
	if s, ok := d.inner.(interface{ Start() error }); ok {
		return s.Start()
	}
	return nil
}

func (d *LimitDispatcher) Close() error {
	if c, ok := d.inner.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ─── Limit management (called by Xray kernel) ──────────────────────────────

func (d *LimitDispatcher) UpdateLimits(emailToUID map[string]int, deviceLimits, _ map[string]int) {
	d.mu.Lock()
	d.emailToUID = emailToUID
	d.deviceLimits = deviceLimits
	d.mu.Unlock()

}

func (d *LimitDispatcher) ResetConns() {
	d.mu.Lock()
	d.limitedIPs = make(map[string]map[string]int)
	d.conns = make(map[uint64]*trackedConn)
	d.mu.Unlock()

	// Clear unlimited IPs
	d.unlimitedIPs.Range(func(key, _ interface{}) bool {
		d.unlimitedIPs.Delete(key)
		return true
	})

	d.connCount.Store(0)
}

// GetConnectionState returns dispatcher-tracked alive IPs and connection count.
// Traffic bytes are intentionally left to xray's built-in stats pipeline.
func (d *LimitDispatcher) GetConnectionState() (aliveIPs map[int]map[string]bool, connCount int) {
	d.mu.RLock()
	emailToUID := d.emailToUID
	limitedIPs := d.limitedIPs
	d.mu.RUnlock()

	aliveIPs = make(map[int]map[string]bool)

	// Collect IPs from limited users (under RLock snapshot).
	for email, ipsMap := range limitedIPs {
		uid := emailToUID[email]
		if uid == 0 {
			continue
		}
		ipSet := make(map[string]bool, len(ipsMap))
		for ip := range ipsMap {
			ipSet[ip] = true
		}
		if len(ipSet) > 0 {
			aliveIPs[uid] = ipSet
		}
	}

	// Collect IPs from unlimited users (lock-free).
	d.unlimitedIPs.Range(func(key, value interface{}) bool {
		email := key.(string)
		uid := emailToUID[email]
		if uid == 0 {
			return true
		}
		ic := value.(*ipCounter)
		if ips := ic.aliveIPs(); len(ips) > 0 {
			// Merge with limited IPs if any
			if existing, ok := aliveIPs[uid]; ok {
				for ip := range ips {
					existing[ip] = true
				}
			} else {
				aliveIPs[uid] = ips
			}
		}
		return true
	})

	connCount = int(d.connCount.Load())
	return
}

// ─── Internal helpers ───────────────────────────────────────────────────────

// checkDeviceLimit enforces per-user device limits.
// Fast path: unlimited users use lock-free sync.Map.
// Slow path: limited users use RWMutex with deterministic IP ordering.
func (d *LimitDispatcher) checkDeviceLimit(email, sourceIP string, isTCP bool) bool {
	d.mu.RLock()
	limit, hasLimit := d.deviceLimits[email]
	d.mu.RUnlock()

	// Fast path: no device limit — use lock-free sync.Map.
	if !hasLimit || limit <= 0 {
		if isTCP {
			v, _ := d.unlimitedIPs.LoadOrStore(email, &ipCounter{})
			ic := v.(*ipCounter)

			// Increment IP refcount atomically.
			rv, _ := ic.ips.LoadOrStore(sourceIP, &atomic.Int64{})
			rv.(*atomic.Int64).Add(1)
		}
		return false
	}

	// Slow path: user has device limit — need deterministic ordering.
	d.mu.RLock()
	ips := d.limitedIPs[email]
	if ips != nil && ips[sourceIP] > 0 {
		d.mu.RUnlock()
		if isTCP {
			d.mu.Lock()
			d.limitedIPs[email][sourceIP]++
			d.mu.Unlock()
		}
		return false
	}

	if ips != nil && len(ips) < limit {
		d.mu.RUnlock()
		if isTCP {
			d.mu.Lock()
			if d.limitedIPs[email] == nil {
				d.limitedIPs[email] = make(map[string]int)
			}
			d.limitedIPs[email][sourceIP]++
			d.mu.Unlock()
		}
		return false
	}
	d.mu.RUnlock()

	// Over limit — need write lock for deterministic check.
	d.mu.Lock()
	defer d.mu.Unlock()

	// Re-check under write lock.
	ips = d.limitedIPs[email]
	if ips == nil {
		ips = make(map[string]int)
		d.limitedIPs[email] = ips
	}

	if ips[sourceIP] > 0 {
		if isTCP {
			ips[sourceIP]++
		}
		return false
	}

	if len(ips) < limit {
		if isTCP {
			ips[sourceIP]++
		}
		return false
	}

	// Over limit — deterministic: allow lowest IPs lexicographically.
	ipList := make([]string, 0, len(ips)+1)
	for ip := range ips {
		ipList = append(ipList, ip)
	}
	ipList = append(ipList, sourceIP)
	sort.Strings(ipList)

	for i := 0; i < limit && i < len(ipList); i++ {
		if ipList[i] == sourceIP {
			if isTCP {
				ips[sourceIP]++
			}
			return false
		}
	}
	return true
}

// delConn decrements the IP refcount when a connection closes.
func (d *LimitDispatcher) delConn(email, sourceIP string) {
	// Check if this is an unlimited user first (lock-free).
	if v, ok := d.unlimitedIPs.Load(email); ok {
		ic := v.(*ipCounter)
		if rv, ok := ic.ips.Load(sourceIP); ok {
			counter := rv.(*atomic.Int64)
			if counter.Add(-1) <= 0 {
				ic.ips.Delete(sourceIP)
			}
		}
		return
	}

	// Limited user — use write lock.
	d.mu.Lock()
	defer d.mu.Unlock()
	if ips, ok := d.limitedIPs[email]; ok {
		ips[sourceIP]--
		if ips[sourceIP] <= 0 {
			delete(ips, sourceIP)
		}
		if len(ips) == 0 {
			delete(d.limitedIPs, email)
		}
	}
}

type closeTrackingWriter struct {
	buf.Writer
	onClose func()
	closed  atomic.Bool
}

func (w *closeTrackingWriter) Close() error {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	return common.Close(w.Writer)
}

func (w *closeTrackingWriter) Interrupt() {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	common.Interrupt(w.Writer)
}
