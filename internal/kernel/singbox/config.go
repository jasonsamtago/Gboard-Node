package singbox

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// M is a shorthand for building JSON-like maps
type M = map[string]interface{}

func buildConfig(kcfg config.KernelConfig, nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	var outbounds []M
	tags := make(map[string]bool)

	// Panel-defined custom outbounds (structured, converted to sing-box native)
	for _, co := range nc.CustomOutbounds {
		outbounds = append(outbounds, outboundConfigToSingbox(co))
		tags[strings.ToLower(co.Tag)] = true
	}

	// Static outbounds from local config file
	for _, co := range kcfg.CustomOutbound {
		tag, _ := co["tag"].(string)
		if tag != "" {
			tags[strings.ToLower(tag)] = true
		}
		outbounds = append(outbounds, M(co))
	}

	// Add default outbounds only if not already defined
	if !tags["direct"] {
		outbounds = append([]M{{"type": "direct", "tag": "direct"}}, outbounds...)
	}
	if !tags["block"] {
		outbounds = append(outbounds, M{"type": "block", "tag": "block"})
	}

	cfg := M{
		"log": M{
			"level":     kcfg.LogLevel,
			"timestamp": true,
		},
		"outbounds": outbounds,
	}

	inbound := buildInbound(nc, users, tc)
	if inbound != nil {
		cfg["inbounds"] = []M{inbound}
	}

	// Merge panel routes and static config routes
	cfg["route"] = buildRoutes(nc.Routes, nc.CustomRouteRules, mergeRouteList(nc.CustomRoutes, kcfg.CustomRoute))

	// Automatically enable rule_set caching (cache_file) when any route source
	// references geoip:/geosite: (panel Routes, CustomRouteRules, CustomRoutes,
	// kernel custom_route) so downloaded .srs files survive restarts.
	if needIP, needSite := kernel.NeedsGeo(nc, kcfg.CustomRoute); needIP || needSite {
		cfg["experimental"] = M{
			"cache_file": M{
				"enabled": true,
				"path":    filepath.Join(kcfg.ConfigDir, "cache.db"),
			},
		}
	}

	mergeCustomSingbox(cfg, kcfg)
	return cfg
}

// outboundConfigToSingbox converts a structured OutboundConfig (from the panel)
// into a sing-box outbound object. sing-box uses a flat layout where all
// protocol-specific fields sit at the top level alongside "type" and "tag".
func outboundConfigToSingbox(oc model.OutboundConfig) M {
	m := M{
		"type": oc.Protocol,
		"tag":  oc.Tag,
	}

	// Transform common protocol keys to sing-box native format
	// WireGuard: secret_key -> private_key; peers: endpoint -> address+port
	if oc.Protocol == "wireguard" {
		if sk, ok := oc.Settings["secret_key"]; ok {
			m["private_key"] = sk
		}
		if peers, ok := oc.Settings["peers"].([]any); ok {
			var wgPeers []M
			for _, p := range peers {
				if peerMap, ok := p.(map[string]any); ok {
					newPeer := M{}
					for k, v := range peerMap {
						if k == "endpoint" {
							if ep, ok := v.(string); ok {
								host, portStr, err := net.SplitHostPort(ep)
								if err == nil {
									newPeer["address"] = host
									port, _ := strconv.Atoi(portStr)
									newPeer["port"] = port
								} else {
									newPeer["address"] = ep
								}
							}
						} else {
							newPeer[k] = v
						}
					}
					wgPeers = append(wgPeers, newPeer)
				}
			}
			m["peers"] = wgPeers
		}

		// Copy any other top-level settings not handled above
		for k, v := range oc.Settings {
			if k != "secret_key" && k != "peers" {
				m[k] = v
			}
		}
	} else {
		flattenServers := socksOrShadowsocks(oc.Protocol)
		for k, v := range oc.Settings {
			// sing-box socks／ss 是扁的 server／server_port，沒有 servers。
			// xray／docs 的 settings.servers 原樣攤平會炸 official #35：
			// parse sing-box options: outbounds[1].servers: json: unknown field "servers"
			if flattenServers && k == "servers" {
				continue
			}
			m[k] = v
		}
		if flattenServers {
			applyXrayServersToSingbox(m, oc.Settings)
		}
	}

	if oc.ProxyTag != "" {
		m["proxy_tag"] = oc.ProxyTag
	}
	return m
}

func socksOrShadowsocks(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "socks", "shadowsocks":
		return true
	default:
		return false
	}
}

// applyXrayServersToSingbox 把 xray／docs 的 settings.servers[0] 轉成
// sing-box 認得的扁欄位。官方評論已有的 server／server_port 等扁欄位當基線，不覆蓋。
func applyXrayServersToSingbox(m M, settings map[string]any) {
	first := firstServerObject(settings["servers"])
	if first == nil {
		return
	}

	if addr := firstNonEmptyString(first, "address", "server"); addr != "" {
		setIfAbsent(m, "server", addr)
	}
	if port, ok := firstServerPort(first, "port", "server_port"); ok {
		setIfAbsent(m, "server_port", port)
	}
	for _, key := range []string{"method", "password", "plugin", "plugin_opts", "network", "version", "username"} {
		if v, exists := first[key]; exists {
			setIfAbsent(m, key, v)
		}
	}
	if users := firstServerUsers(first["users"]); len(users) > 0 {
		u := users[0]
		if name := firstNonEmptyString(u, "user", "username"); name != "" {
			setIfAbsent(m, "username", name)
		}
		if pass := firstNonEmptyString(u, "pass", "password"); pass != "" {
			setIfAbsent(m, "password", pass)
		}
	}
}

func setIfAbsent(m M, key string, val any) {
	if _, exists := m[key]; exists {
		return
	}
	m[key] = val
}

func firstServerObject(raw any) map[string]any {
	switch list := raw.(type) {
	case []any:
		if len(list) == 0 {
			return nil
		}
		return asStringAnyMap(list[0])
	case []map[string]any:
		if len(list) == 0 {
			return nil
		}
		return list[0]
	default:
		return asStringAnyMap(raw)
	}
}

func firstServerUsers(raw any) []map[string]any {
	switch list := raw.(type) {
	case []any:
		out := make([]map[string]any, 0, len(list))
		for _, item := range list {
			if m := asStringAnyMap(item); m != nil {
				out = append(out, m)
			}
		}
		return out
	case []map[string]any:
		return list
	default:
		return nil
	}
}

func asStringAnyMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func firstNonEmptyString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func firstServerPort(m map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		if port, ok := coerceListenPort(m[key]); ok {
			return port, true
		}
	}
	return 0, false
}

func coerceListenPort(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		if n >= 1 && n <= 65535 {
			return n, true
		}
	case int32:
		return coerceListenPort(int(n))
	case int64:
		return coerceListenPort(int(n))
	case float64:
		if n == float64(int(n)) {
			return coerceListenPort(int(n))
		}
	case json.Number:
		if p, err := n.Int64(); err == nil {
			return coerceListenPort(int(p))
		}
	case string:
		if p, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return coerceListenPort(p)
		}
	}
	return 0, false
}

func mergeRouteList(a, b []map[string]any) []map[string]any {
	res := make([]map[string]any, 0, len(a)+len(b))
	res = append(res, a...)
	res = append(res, b...)
	return res
}

func buildRoutes(panelRoutes []model.RouteRule, customRules []model.CustomRouteRule, custom []map[string]any) M {
	var rules []M

	// Structured custom routes now take the highest priority for panel-managed overrides.
	for _, rule := range customRules {
		if rule.Disabled {
			continue
		}
		rules = append(rules, compileCustomRouteRule(rule)...)
	}

	// Raw custom routes remain the escape hatch, but no longer outrank structured rules.
	for _, cr := range custom {
		rules = append(rules, M(cr))
	}

	// Standard blocks for private IPv4 and IPv6 ranges to prevent SSRF.
	rules = append(rules,
		M{
			"outbound": "block",
			"ip_cidr": []string{
				"10.0.0.0/8",
				"100.64.0.0/10",
				"127.0.0.0/8",
				"169.254.0.0/16",
				"172.16.0.0/12",
				"192.0.0.0/24",
				"192.168.0.0/16",
				"198.18.0.0/15",
			},
		},
		M{
			"outbound": "block",
			"ip_cidr": []string{
				"fc00::/7",
				"fe80::/10",
				"::1/128",
			},
		},
	)

	for _, pr := range panelRoutes {
		rules = append(rules, compilePanelRouteRule(pr)...)
	}

	return M{
		"final": "direct",
		"rules": rules,
	}
}

func compilePanelRouteRule(pr model.RouteRule) []M {
	if len(pr.Match) == 0 {
		return nil
	}

	var domains, cidrs []string
	for _, item := range pr.Match {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		item = strings.TrimPrefix(item, "*.")
		if strings.Contains(item, "/") {
			cidrs = append(cidrs, item)
			continue
		}
		domains = append(domains, item)
	}

	outbound := "block"
	switch pr.Action {
	case "direct":
		outbound = "direct"
	case "dns":
		if pr.ActionValue != "" {
			outbound = pr.ActionValue
		} else {
			outbound = "dns-out"
		}
	case "proxy":
		if pr.ActionValue != "" {
			outbound = pr.ActionValue
		}
	}

	var compiled []M
	if len(domains) > 0 {
		compiled = append(compiled, M{
			"domain_suffix": copyStrings(domains),
			"outbound":      outbound,
		})
	}
	if len(cidrs) > 0 {
		compiled = append(compiled, M{
			"ip_cidr":  copyStrings(cidrs),
			"outbound": outbound,
		})
	}
	return compiled
}

func compileCustomRouteRule(rule model.CustomRouteRule) []M {
	if rule.Disabled {
		return nil
	}

	outbound := singboxOutboundForAction(rule.Action)
	var compiled []M

	if len(rule.Match.Domains) > 0 {
		compiled = append(compiled, M{
			"domain":   copyStrings(rule.Match.Domains),
			"outbound": outbound,
		})
	}
	if len(rule.Match.DomainSuffixes) > 0 {
		compiled = append(compiled, M{
			"domain_suffix": copyStrings(rule.Match.DomainSuffixes),
			"outbound":      outbound,
		})
	}
	if len(rule.Match.IPCIDRs) > 0 {
		compiled = append(compiled, M{
			"ip_cidr":  copyStrings(rule.Match.IPCIDRs),
			"outbound": outbound,
		})
	}
	if len(rule.Match.Ports) > 0 {
		ports, portRanges := splitPorts(rule.Match.Ports)
		entry := M{"outbound": outbound}
		if len(ports) > 0 {
			entry["port"] = ports
		}
		if len(portRanges) > 0 {
			entry["port_range"] = portRanges
		}
		compiled = append(compiled, entry)
	}
	if len(rule.Match.Networks) > 0 {
		compiled = append(compiled, M{
			"network":  copyStrings(rule.Match.Networks),
			"outbound": outbound,
		})
	}
	if len(rule.Match.SourceCIDRs) > 0 {
		compiled = append(compiled, M{
			"source_ip_cidr": copyStrings(rule.Match.SourceCIDRs),
			"outbound":       outbound,
		})
	}
	if len(rule.Match.SourcePorts) > 0 {
		ports, portRanges := splitPorts(rule.Match.SourcePorts)
		entry := M{"outbound": outbound}
		if len(ports) > 0 {
			entry["source_port"] = ports
		}
		if len(portRanges) > 0 {
			entry["source_port_range"] = portRanges
		}
		compiled = append(compiled, entry)
	}
	return compiled
}

func singboxOutboundForAction(action model.RouteAction) string {
	switch action.Type {
	case "direct":
		return "direct"
	case "route":
		if action.Target != "" {
			return action.Target
		}
		return "block"
	default:
		return "block"
	}
}

func splitPorts(values []string) ([]int, []string) {
	var ports []int
	var ranges []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if strings.ContainsAny(value, ":-") {
			ranges = append(ranges, strings.ReplaceAll(value, "-", ":"))
			continue
		}
		if port, err := strconv.Atoi(value); err == nil {
			ports = append(ports, port)
		}
	}
	return ports, ranges
}

func copyStrings(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}

func mergeCustomSingbox(cfg M, kcfg config.KernelConfig) {
	custom, err := kernel.LoadCustomConfig(kcfg.CustomConfig)
	if err != nil {
		nlog.Core().Error("failed to load custom sing-box config", "error", err)
		return
	}
	if custom == nil {
		return
	}

	// dns — custom replaces
	if v, ok := custom["dns"]; ok {
		cfg["dns"] = v
	}

	// experimental — custom replaces
	if v, ok := custom["experimental"]; ok {
		cfg["experimental"] = v
	}

	// outbounds — append custom entries
	if v, ok := custom["outbounds"]; ok {
		if existing, ok := cfg["outbounds"].([]M); ok {
			cfg["outbounds"] = kernel.MergeAppendList(existing, v)
		}
	}

	// endpoints (sing-box 1.11+ wireguard etc.) — append
	if v, ok := custom["endpoints"]; ok {
		if existing, ok := cfg["endpoints"].([]M); ok {
			cfg["endpoints"] = kernel.MergeAppendList(existing, v)
		} else {
			if items := kernel.MergeAppendList(nil, v); len(items) > 0 {
				cfg["endpoints"] = items
			}
		}
	}

	// route — merge sub-fields
	if customRoute, ok := custom["route"]; ok {
		if customRouteMap, ok := customRoute.(map[string]any); ok {
			mergeCustomSingboxRoute(cfg, customRouteMap)
		}
	}
}

func mergeCustomSingboxRoute(cfg M, customRoute map[string]any) {
	route, ok := cfg["route"].(M)
	if !ok {
		route = M{}
		cfg["route"] = route
	}

	// rules — custom rules prepended (so they match before panel rules)
	if v, ok := customRoute["rules"]; ok {
		if existing, ok := route["rules"].([]M); ok {
			route["rules"] = kernel.MergePrependList(existing, v)
		}
	}

	// rule_set — custom rule_sets appended
	if v, ok := customRoute["rule_set"]; ok {
		if existing, ok := route["rule_set"].([]M); ok {
			route["rule_set"] = kernel.MergeAppendList(existing, v)
		} else {
			route["rule_set"] = kernel.MergeAppendList(nil, v)
		}
	}

	// final, auto_detect_interface, default_interface, etc. — custom overrides
	for k, v := range customRoute {
		if k == "rules" || k == "rule_set" {
			continue
		}
		route[k] = v
	}
}

func buildInbound(nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	base := M{
		"tag":         nc.Protocol + "-in",
		"listen":      "::",
		"listen_port": nc.ServerPort,
	}
	applyHy2ListenRange(base, nc)

	switch nc.Protocol {
	case "shadowsocks":
		return buildShadowsocks(base, nc, users)
	case "vmess":
		return buildVMess(base, nc, users, tc)
	case "vless":
		return buildVLESS(base, nc, users, tc)
	case "trojan":
		return buildTrojan(base, nc, users, tc)
	case "hysteria":
		return buildHysteria(base, nc, users, tc)
	case "tuic":
		return buildTUIC(base, nc, users, tc)
	case "anytls":
		return buildAnyTLS(base, nc, users, tc)
	case "naive":
		return buildNaive(base, nc, users, tc)
	case "socks":
		return buildSocks(base, users)
	case "http":
		return buildHTTP(base, nc, users, tc)
	case "mieru":
		return buildMieru(base, nc, users)
	default:
		return nil
	}
}

func buildMieru(base M, nc *model.NodeSpec, users []model.UserSpec) M {
	base["type"] = "mieru"
	if nc.Transport != "" {
		base["transport"] = nc.Transport
	}
	if nc.TrafficPattern != "" {
		base["traffic_pattern"] = nc.TrafficPattern
	}

	userList := make([]M, 0, len(users))
	for _, u := range users {
		userList = append(userList, M{
			"name":     u.UUID,
			"password": u.UUID,
		})
	}
	base["users"] = userList
	return base
}

type ss2022Config struct {
	method string
	size   int
}

var ss2022Methods = map[string]ss2022Config{
	"2022-blake3-aes-128-gcm":       {"2022-blake3-aes-128-gcm", 16},
	"2022-blake3-aes-256-gcm":       {"2022-blake3-aes-256-gcm", 32},
	"2022-blake3-chacha20-poly1305": {"2022-blake3-chacha20-poly1305", 32},
}

func buildShadowsocks(base M, nc *model.NodeSpec, users []model.UserSpec) M {
	base["type"] = "shadowsocks"
	base["method"] = nc.Cipher

	ss2022, isSS2022 := ss2022Methods[nc.Cipher]
	if isSS2022 {
		base["password"] = nc.ServerKey
	}

	userList := make([]M, len(users))
	var rawBuf []byte
	if isSS2022 {
		rawBuf = make([]byte, ss2022.size)
	}

	for i := range users {
		u := &users[i]
		user := M{
			"name":     u.UUID,
			"password": u.UUID,
		}

		if isSS2022 {
			// Reuse buffer and clear it to maintain SS2022 key integrity
			for j := range rawBuf {
				rawBuf[j] = 0
			}
			copy(rawBuf, u.UUID)
			user["password"] = base64.StdEncoding.EncodeToString(rawBuf)
		}
		userList[i] = user
	}
	base["users"] = userList

	if nc.Plugin != "" {
		nlog.Core().Warn("sing-box shadowsocks inbound does not support plugin, ignoring", "plugin", nc.Plugin)
	}

	return base
}

func buildVMess(base M, nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	base["type"] = "vmess"

	userList := make([]M, 0, len(users))
	for _, u := range users {
		userList = append(userList, M{
			"name":    u.UUID,
			"uuid":    u.UUID,
			"alterId": 0,
		})
	}
	base["users"] = userList

	applyTransport(base, nc)
	applyProxyProtocol(base, nc)
	applyMultiplex(base, nc)

	if nc.TLS == 1 {
		if tls := buildTLSConfig(nc, tc); tls != nil {
			base["tls"] = tls
		}
	}

	return base
}

func buildVLESS(base M, nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	if nc.Decryption != "" && nc.Decryption != "none" {
		nlog.Core().Warn("sing-box does not support VLESS encryption (decryption), use xray kernel for this feature")
	}

	base["type"] = "vless"

	userList := make([]M, 0, len(users))
	for _, u := range users {
		user := M{
			"name": u.UUID,
			"uuid": u.UUID,
		}
		if nc.Flow != "" {
			user["flow"] = nc.Flow
		}
		userList = append(userList, user)
	}
	base["users"] = userList

	applyTransport(base, nc)
	applyProxyProtocol(base, nc)
	applyMultiplex(base, nc)

	if nc.TLS == 1 {
		if tls := buildTLSConfig(nc, tc); tls != nil {
			base["tls"] = tls
		}
	} else if nc.TLS == 2 {
		base["tls"] = buildRealityConfig(nc)
	}

	return base
}

func buildTrojan(base M, nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	base["type"] = "trojan"

	userList := make([]M, len(users))
	for i := range users {
		u := &users[i]
		userList[i] = M{
			"name":     u.UUID,
			"password": u.UUID,
		}
	}
	base["users"] = userList

	applyTransport(base, nc)
	applyProxyProtocol(base, nc)
	applyMultiplex(base, nc)

	if nc.TLS == 1 {
		if tls := buildTLSConfig(nc, tc); tls != nil {
			base["tls"] = tls
		}
	} else if nc.TLS == 2 {
		base["tls"] = buildRealityConfig(nc)
	}

	// If still no TLS/Reality but cert paths exist, enable TLS (panel may omit tls=1).
	if _, ok := base["tls"]; !ok {
		if tls := buildTLSConfig(nc, tc); tls != nil {
			base["tls"] = tls
		}
	}

	if base["tls"] == nil {
		nlog.Core().Warn("trojan inbound has no TLS (no certificate paths and no Reality); sing-box may refuse to start until cert_mode provides cert/key or Reality is configured")
	}

	return base
}

// applyHy2ListenRange 把面板區間寫進 sing-box inbound 的 listen 規格。
// cedar sing-box 的 listen_port 只能是單一 uint16，所以區間用 listen_port_range
// （以及 sing-box 風格的 listen_ports start:end）表達完整 hop；單端口不寫這些欄位。
func applyHy2ListenRange(base M, nc *model.NodeSpec) {
	if nc == nil || nc.Protocol != "hysteria" {
		return
	}
	start, end, ok := parseListenPortRange(nc.ServerPortRange)
	if !ok || end <= start {
		return
	}
	base["listen_port_range"] = fmt.Sprintf("%d-%d", start, end)
	base["listen_ports"] = []string{fmt.Sprintf("%d:%d", start, end)}
}

func parseListenPortRange(raw string) (int, int, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, 0, false
	}
	a, b, ok := strings.Cut(raw, "-")
	if !ok || strings.Contains(b, "-") {
		return 0, 0, false
	}
	start, err1 := strconv.Atoi(strings.TrimSpace(a))
	end, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || start < 1 || end > 65535 || start > end {
		return 0, 0, false
	}
	return start, end, true
}

func buildHysteria(base M, nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	if nc.Version == 2 {
		base["type"] = "hysteria2"

		userList := make([]M, 0, len(users))
		for _, u := range users {
			userList = append(userList, M{
				"name":     u.UUID,
				"password": u.UUID,
			})
		}
		base["users"] = userList

		if nc.Obfs != "" {
			base["obfs"] = M{
				"type":     nc.Obfs,
				"password": nc.ObfsPassword,
			}
		}
	} else {
		base["type"] = "hysteria"

		userList := make([]M, 0, len(users))
		for _, u := range users {
			userList = append(userList, M{
				"name":     u.UUID,
				"auth_str": u.UUID,
			})
		}
		base["users"] = userList
		base["up_mbps"] = nc.UpMbps
		base["down_mbps"] = nc.DownMbps

		if nc.Obfs != "" {
			base["obfs"] = nc.Obfs
		}
	}

	tls := buildTLSConfig(nc, tc)
	if tls == nil {
		nlog.Core().Warn("hysteria requires TLS certificate files on disk; configure cert_mode (self, file, http, dns, or content). Sing-box will not start this inbound without tls.")
		return base
	}
	// Hysteria/Hysteria2 uses QUIC and requires ALPN; default to h3 if not set.
	if _, ok := tls["alpn"]; !ok {
		tls["alpn"] = []string{"h3"}
	}
	base["tls"] = tls
	return base
}

func buildTUIC(base M, nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	base["type"] = "tuic"

	userList := make([]M, 0, len(users))
	for _, u := range users {
		userList = append(userList, M{
			"name":     u.UUID,
			"uuid":     u.UUID,
			"password": u.UUID,
		})
	}
	base["users"] = userList

	if nc.CongestionControl != "" {
		base["congestion_control"] = nc.CongestionControl
	}

	tls := buildTLSConfig(nc, tc)
	if tls == nil {
		nlog.Core().Warn("tuic requires TLS certificate files on disk; configure cert_mode (self, file, http, dns, or content). Sing-box will not start this inbound without tls.")
		return base
	}
	// TUIC requires ALPN for QUIC negotiation; default to h3 if not set by panel.
	if _, ok := tls["alpn"]; !ok {
		tls["alpn"] = []string{"h3"}
	}
	base["tls"] = tls
	return base
}

func buildAnyTLS(base M, nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	base["type"] = "anytls"

	userList := make([]M, 0, len(users))
	for _, u := range users {
		userList = append(userList, M{
			"name":     u.UUID,
			"password": u.UUID,
		})
	}
	base["users"] = userList

	if nc.PaddingScheme != "" {
		base["padding_scheme"] = nc.PaddingScheme
	}

	if tls := buildTLSConfig(nc, tc); tls != nil {
		base["tls"] = tls
	} else {
		nlog.Core().Warn("anytls requires TLS certificate files on disk; configure cert_mode (self, file, http, dns, or content). Sing-box will not start this inbound without tls.")
	}
	return base
}

func buildNaive(base M, nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	base["type"] = "naive"

	userList := make([]M, 0, len(users))
	for _, u := range users {
		userList = append(userList, M{
			"username": strconv.Itoa(u.ID),
			"password": u.UUID,
		})
	}
	base["users"] = userList

	if nc.TLS == 1 {
		if tls := buildTLSConfig(nc, tc); tls != nil {
			base["tls"] = tls
		}
	}
	return base
}

func buildSocks(base M, users []model.UserSpec) M {
	base["type"] = "socks"

	userList := make([]M, 0, len(users))
	for _, u := range users {
		userList = append(userList, M{
			"username": u.UUID,
			"password": u.UUID,
		})
	}
	base["users"] = userList
	return base
}

func buildHTTP(base M, nc *model.NodeSpec, users []model.UserSpec, tc kernel.TLSCert) M {
	base["type"] = "http"

	userList := make([]M, 0, len(users))
	for _, u := range users {
		userList = append(userList, M{
			"username": u.UUID,
			"password": u.UUID,
		})
	}
	base["users"] = userList

	applyProxyProtocol(base, nc)
	if nc.TLS == 1 {
		if tls := buildTLSConfig(nc, tc); tls != nil {
			base["tls"] = tls
		}
	}
	return base
}

func applyTransport(base M, nc *model.NodeSpec) {
	if nc.Network == "" {
		return
	}
	if nc.Network == "tcp" {
		applyTCPHTTPTransport(base, nc)
		return
	}

	transport := M{"type": nc.Network}

	if nc.NetworkSettings != nil {
		switch nc.Network {
		case "ws":
			if v, ok := nc.NetworkSettings["path"]; ok {
				transport["path"] = v
			}
			if v, ok := nc.NetworkSettings["headers"]; ok {
				transport["headers"] = v
			}
			if v, ok := nc.NetworkSettings["host"]; ok {
				if transport["headers"] == nil {
					transport["headers"] = M{"Host": v}
				}
			}
			if v, ok := nc.NetworkSettings["max_early_data"]; ok {
				transport["max_early_data"] = v
			}
			if v, ok := nc.NetworkSettings["early_data_header_name"]; ok {
				transport["early_data_header_name"] = v
			}
		case "grpc":
			if v, ok := nc.NetworkSettings["serviceName"]; ok {
				transport["service_name"] = v
			} else if v, ok := nc.NetworkSettings["service_name"]; ok {
				transport["service_name"] = v
			}
		case "httpupgrade":
			if v, ok := nc.NetworkSettings["path"]; ok {
				transport["path"] = v
			}
			if v, ok := nc.NetworkSettings["host"]; ok {
				transport["host"] = v
			}
		case "h2", "http":
			transport["type"] = "http"
			if v, ok := nc.NetworkSettings["path"]; ok {
				transport["path"] = v
			}
			if v, ok := nc.NetworkSettings["host"]; ok {
				transport["host"] = v
			}
		}
	}

	base["transport"] = transport
}

// applyTCPHTTPTransport writes the panel tcp+http disguise (header.type=http +
// request.headers.Host) as sing-box V2Ray HTTP transport. Pure tcp must not
// grow a transport object. Host stays on the inbound.
func applyTCPHTTPTransport(base M, nc *model.NodeSpec) {
	if nc == nil || nc.NetworkSettings == nil {
		return
	}
	header := asSettingsMap(nc.NetworkSettings["header"])
	if header == nil {
		return
	}
	typ, _ := header["type"].(string)
	if !strings.EqualFold(typ, "http") {
		return
	}

	transport := M{"type": "http"}
	if request := asSettingsMap(header["request"]); request != nil {
		if path := firstSettingsString(request["path"]); path != "" {
			transport["path"] = path
		}
		if reqHeaders := asSettingsMap(request["headers"]); reqHeaders != nil {
			if hosts := settingsStringList(reqHeaders["Host"]); len(hosts) > 0 {
				transport["host"] = hosts
			}
		}
	}
	if transport["host"] == nil {
		if hdrs := asSettingsMap(header["headers"]); hdrs != nil {
			if hosts := settingsStringList(hdrs["Host"]); len(hosts) > 0 {
				transport["host"] = hosts
			}
		}
	}
	if transport["path"] == nil && transport["host"] == nil {
		return
	}
	if transport["path"] == nil {
		transport["path"] = "/"
	}
	base["transport"] = transport
}

func asSettingsMap(v any) M {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return nil
}

func firstSettingsString(v any) string {
	list := settingsStringList(v)
	if len(list) == 0 {
		return ""
	}
	return list[0]
}

func settingsStringList(v any) []string {
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) == "" {
			return nil
		}
		return []string{x}
	case []string:
		out := make([]string, 0, len(x))
		for _, s := range x {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			s := strings.TrimSpace(fmt.Sprint(item))
			if s == "" || s == "<nil>" {
				continue
			}
			out = append(out, s)
		}
		return out
	default:
		return nil
	}
}

// buildTLSConfig returns sing-box inbound TLS options when certificate material
// is available. Returns nil if no TLS material is provided.
func buildTLSConfig(nc *model.NodeSpec, tc kernel.TLSCert) M {
	if !tc.HasCert() {
		return nil
	}

	t := M{"enabled": true}

	serverName := nc.ServerName
	if serverName == "" && nc.Host != "" {
		serverName = nc.Host
	}
	if serverName != "" {
		t["server_name"] = serverName
	}

	if nc.TLSSettings != nil {
		if sn, ok := nc.TLSSettings["server_name"]; ok && sn != "" {
			t["server_name"] = sn
		}
		if alpn, ok := nc.TLSSettings["alpn"]; ok {
			t["alpn"] = alpn
		}
		// ECH server-side: only key/key_path needed for inbound
		if ech := extractECHInbound(nc.TLSSettings); ech != nil {
			t["ech"] = ech
		}
	}

	t["certificate"] = []string{string(tc.CertPEM)}
	t["key"] = []string{string(tc.KeyPEM)}

	return t
}

func buildRealityConfig(nc *model.NodeSpec) M {
	tls := M{"enabled": true}
	if nc.TLSSettings == nil {
		return tls
	}

	reality := M{"enabled": true}
	var settings struct {
		PrivateKey string `mapstructure:"private_key"`
		ShortID    any    `mapstructure:"short_id"`
		Dest       string `mapstructure:"dest"`
		ServerName string `mapstructure:"server_name"`
		ServerPort int    `mapstructure:"server_port"`
	}

	decoderConfig := &mapstructure.DecoderConfig{
		Metadata:         nil,
		Result:           &settings,
		WeaklyTypedInput: true,
	}
	decoder, _ := mapstructure.NewDecoder(decoderConfig)
	_ = decoder.Decode(nc.TLSSettings)

	if settings.PrivateKey != "" {
		reality["private_key"] = settings.PrivateKey
	}

	switch v := settings.ShortID.(type) {
	case string:
		reality["short_id"] = []string{v}
	case []any:
		ids := make([]string, 0, len(v))
		for _, item := range v {
			ids = append(ids, fmt.Sprintf("%v", item))
		}
		reality["short_id"] = ids
	case []string:
		reality["short_id"] = v
	}

	dest := settings.Dest
	if dest == "" {
		dest = settings.ServerName
	}

	if dest != "" {
		handshake := M{"server": dest, "server_port": 443}
		if parts := strings.SplitN(dest, ":", 2); len(parts) == 2 {
			handshake["server"] = parts[0]
			if p, err := strconv.Atoi(parts[1]); err == nil {
				handshake["server_port"] = p
			}
		} else if settings.ServerPort > 0 {
			handshake["server_port"] = settings.ServerPort
		}
		reality["handshake"] = handshake
	}

	if settings.ServerName != "" {
		tls["server_name"] = settings.ServerName
	}

	tls["reality"] = reality
	return tls
}

func applyMultiplex(base M, nc *model.NodeSpec) {
	if nc.Multiplex == nil || !nc.Multiplex.Enabled {
		return
	}

	mux := M{
		"enabled": true,
	}
	if nc.Multiplex.Padding {
		mux["padding"] = true
	}

	if nc.Multiplex.Brutal != nil && nc.Multiplex.Brutal.Enabled {
		brutal := M{
			"enabled": true,
		}
		if nc.Multiplex.Brutal.UpMbps > 0 {
			brutal["up_mbps"] = nc.Multiplex.Brutal.UpMbps
		}
		if nc.Multiplex.Brutal.DownMbps > 0 {
			brutal["down_mbps"] = nc.Multiplex.Brutal.DownMbps
		}
		mux["brutal"] = brutal
	}

	base["multiplex"] = mux
}

func applyProxyProtocol(base M, nc *model.NodeSpec) {
	// if !nc.GetProxyProtocol() {
	// 	return
	// }
	// base["proxy_protocol"] = true
}

// extractECHInbound extracts ECH config for sing-box server (inbound).
// sing-box InboundECHOptions needs: enabled, key (PEM array), key_path.
func extractECHInbound(tlsSettings map[string]interface{}) M {
	echRaw, ok := tlsSettings["ech"]
	if !ok {
		return nil
	}
	echMap, ok := echRaw.(map[string]interface{})
	if !ok {
		return nil
	}
	enabled, _ := echMap["enabled"].(bool)
	if !enabled {
		return nil
	}
	ech := M{"enabled": true}
	hasSource := false
	if key, _ := echMap["key"].(string); key != "" {
		ech["key"] = []string{key} // sing-box expects PEM as-is
		hasSource = true
	}
	if keyPath, _ := echMap["key_path"].(string); keyPath != "" {
		ech["key_path"] = keyPath
		hasSource = true
	}
	if !hasSource {
		nlog.Core().Warn("ECH enabled but no key or key_path provided")
	}
	return ech // let sing-box report a clear error if key is missing
}
