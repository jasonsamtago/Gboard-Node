package singbox

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/panel"
)

// 對準官方 cedar2025/Xboard-Node #52（面板連線／服務端口都填 26598-36598，
// 部署後只有 26598 能連，端口占用也只有這個）。
//
// 審核 2 鎖定：
//   - Hy2 端口跳躍必須聽完整區間，不能只聽起點
//   - 單端口仍只聽那一個
//   - 不准關掉跳躍當修；把區間當單端口／清掉區間／忽略 hop 都是假修
//
// 面板實際下發（Gboard ServerService::buildNodeConfig + normalizePortOrRange）：
//   hysteria 的 server_port 是區間字串，例如 "26598-36598"，不是只給起點數字。
// 官方區間有一萬個端口；此檔用 26598-26600（3 個端口）等價測「必須整段聽」語意，
// 避免真 bind 一萬個端口把 CI 打爆。另用官方字串 26598-36598 鎖定終點不得消失。
//
// 這份測試只鎖行為，不實作修正。

var hy2HopTLS = kernel.TLSCert{CertPEM: []byte("CERT"), KeyPEM: []byte("KEY")}

func TestHy2PortHop_區間必須聽完整區間不能只聽起點(t *testing.T) {
	// 26598-26600 等價於官方 26598-36598 的「整段聽」語意，只縮短以免真 bind 大區間。
	inbounds := hy2InboundsFromPanelPort(t, "26598-26600")
	coverage := collectListenCoverage(inbounds)

	for _, port := range []int{26598, 26599, 26600} {
		if !coverage.covers(port) {
			t.Errorf("Hy2 端口跳躍必須聽完整區間 26598-26600（等價官方 26598-36598），缺少端口 %d；目前 listen 規格 %s。只聽起點是官方 #52 的假修", port, coverage.dump())
		}
	}
	if coverage.portCount() < 3 && !coverage.hasRange(26598, 26600) {
		t.Errorf("把區間當單端口／清掉區間／忽略 hop 都是假修：目前只綁到 %s", coverage.dump())
	}
}

func TestHy2PortHop_官方大區間終點不得消失(t *testing.T) {
	// 不真 bind 10001 個端口；只檢查產出的 listen 規格仍涵蓋官方終點 36598。
	inbounds := hy2InboundsFromPanelPort(t, "26598-36598")
	coverage := collectListenCoverage(inbounds)

	if !coverage.covers(26598) {
		t.Errorf("官方區間 26598-36598 必須涵蓋起點 26598，目前 %s", coverage.dump())
	}
	if !coverage.covers(36598) {
		t.Errorf("官方區間 26598-36598 的終點消失了（只剩起點是 #52）；目前 %s。不准關 hop、不准只聽 26598", coverage.dump())
	}
	mid := 26598 + (36598-26598)/2
	if !coverage.covers(mid) {
		t.Errorf("官方區間 26598-36598 必須涵蓋中點 %d，目前 %s", mid, coverage.dump())
	}
	if coverage.onlyPort(26598) {
		t.Errorf("把官方區間收成只聽起點 26598 是假修，目前 %s", coverage.dump())
	}
}

func TestHy2PortHop_單端口仍只聽那一個(t *testing.T) {
	inbounds := hy2InboundsFromPanelPort(t, 443)
	coverage := collectListenCoverage(inbounds)

	if !coverage.covers(443) {
		t.Errorf("單端口必須聽 443，目前 %s", coverage.dump())
	}
	if extra := coverage.extraPorts(443); len(extra) > 0 {
		t.Errorf("單端口不得多開，多了 %v；目前 %s", extra, coverage.dump())
	}
	if coverage.hasOpenRange() {
		t.Errorf("單端口不得開跳躍區間，目前 %s", coverage.dump())
	}
}

func hy2InboundsFromPanelPort(t *testing.T, serverPort any) []M {
	t.Helper()
	nc := fetchPanelHy2Config(t, serverPort)
	cfg := buildConfig(config.KernelConfig{LogLevel: "warn"}, testNodeSpec(nc), testUsers, hy2HopTLS)
	raw, ok := cfg["inbounds"]
	if !ok || raw == nil {
		t.Fatalf("產出的 sing-box 設定沒有 inbounds：區間 hop 被關掉或略過了（server_port=%v）", serverPort)
	}
	inbounds := asInboundSlice(raw)
	if len(inbounds) == 0 {
		t.Fatalf("inbounds 是空的：區間 hop 被關掉了（server_port=%v）", serverPort)
	}
	return inbounds
}

func fetchPanelHy2Config(t *testing.T, serverPort any) *panel.NodeConfig {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"protocol":    "hysteria",
			"server_port": serverPort,
			"version":     2,
		})
	}))
	t.Cleanup(ts.Close)

	client := panel.NewClient(config.PanelConfig{
		URL:    ts.URL,
		Token:  "hy2-hop",
		NodeID: 1,
	})
	cfg, err := client.GetConfig()
	if err != nil {
		t.Fatalf("面板下發 Hy2 server_port=%v 必須能被 node 收下，不得把區間當非法值丟掉或收成只剩起點: %v", serverPort, err)
	}
	if cfg == nil {
		t.Fatal("GetConfig 回傳 nil")
	}
	if cfg.Protocol != "hysteria" {
		t.Fatalf("protocol: got %q, want hysteria", cfg.Protocol)
	}
	return cfg
}

func asInboundSlice(raw any) []M {
	switch v := raw.(type) {
	case []M:
		return v
	case []any:
		out := make([]M, 0, len(v))
		for _, item := range v {
			if m, ok := item.(M); ok {
				out = append(out, m)
			} else if m, ok := item.(map[string]any); ok {
				out = append(out, M(m))
			}
		}
		return out
	case M:
		return []M{v}
	case map[string]any:
		return []M{M(v)}
	default:
		return nil
	}
}

type listenCoverage struct {
	ports  map[int]bool
	ranges [][2]int
	raw    string
}

func collectListenCoverage(v any) listenCoverage {
	c := listenCoverage{ports: map[int]bool{}}
	if data, err := json.Marshal(v); err == nil {
		c.raw = string(data)
	}
	walkListenValues(v, func(key string, val any) {
		switch key {
		case "listen_port", "listen_ports", "listen_port_range":
			addListenValue(&c, val)
		case "listen":
			addListenAddress(&c, fmt.Sprint(val))
		}
	})
	return c
}

func walkListenValues(v any, fn func(key string, val any)) {
	switch x := v.(type) {
	case M:
		for k, val := range x {
			fn(k, val)
			walkListenValues(val, fn)
		}
	case map[string]any:
		for k, val := range x {
			fn(k, val)
			walkListenValues(val, fn)
		}
	case []M:
		for _, item := range x {
			walkListenValues(item, fn)
		}
	case []any:
		for _, item := range x {
			walkListenValues(item, fn)
		}
	}
}

func addListenValue(c *listenCoverage, val any) {
	switch x := val.(type) {
	case int:
		c.ports[x] = true
	case int64:
		c.ports[int(x)] = true
	case float64:
		c.ports[int(x)] = true
	case json.Number:
		if n, err := x.Int64(); err == nil {
			c.ports[int(n)] = true
		}
	case string:
		addPortOrRange(c, x)
	case []any:
		for _, item := range x {
			addListenValue(c, item)
		}
	case []string:
		for _, item := range x {
			addPortOrRange(c, item)
		}
	}
}

func addListenAddress(c *listenCoverage, addr string) {
	addr = strings.TrimSpace(addr)
	if addr == "" || addr == "::" || addr == "0.0.0.0" || addr == "[::]" {
		return
	}
	portPart := addr
	if i := strings.LastIndex(addr, "]:"); i >= 0 {
		portPart = addr[i+2:]
	} else if strings.HasPrefix(addr, ":") && !strings.HasPrefix(addr, "::") {
		portPart = strings.TrimPrefix(addr, ":")
	} else if i := strings.LastIndex(addr, ":"); i >= 0 && !strings.Contains(addr, "]") {
		host, port := addr[:i], addr[i+1:]
		if host == "" || netHostLooksLikeIPOrName(host) {
			portPart = port
		}
	} else {
		return
	}
	if portPart == addr && !looksLikePortOrRange(portPart) {
		return
	}
	addPortOrRange(c, portPart)
}

func netHostLooksLikeIPOrName(host string) bool {
	return host != "" && !looksLikePortOrRange(host)
}

func looksLikePortOrRange(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, sep := range []string{"-", ":"} {
		if a, b, ok := strings.Cut(s, sep); ok {
			return isPortNumber(a) && isPortNumber(b)
		}
	}
	return isPortNumber(s)
}

func isPortNumber(s string) bool {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	return err == nil && n >= 1 && n <= 65535
}

func addPortOrRange(c *listenCoverage, s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	for _, sep := range []string{"-", ":"} {
		if a, b, ok := strings.Cut(s, sep); ok && isPortNumber(a) && isPortNumber(b) {
			start, _ := strconv.Atoi(a)
			end, _ := strconv.Atoi(b)
			if start > end {
				start, end = end, start
			}
			c.ranges = append(c.ranges, [2]int{start, end})
			return
		}
	}
	if isPortNumber(s) {
		n, _ := strconv.Atoi(s)
		c.ports[n] = true
	}
}

func (c listenCoverage) covers(port int) bool {
	if c.ports[port] {
		return true
	}
	for _, r := range c.ranges {
		if port >= r[0] && port <= r[1] {
			return true
		}
	}
	return false
}

func (c listenCoverage) hasRange(start, end int) bool {
	for _, r := range c.ranges {
		if r[0] <= start && r[1] >= end {
			return true
		}
	}
	return false
}

func (c listenCoverage) hasOpenRange() bool {
	for _, r := range c.ranges {
		if r[0] != r[1] {
			return true
		}
	}
	return false
}

func (c listenCoverage) onlyPort(port int) bool {
	if c.hasOpenRange() {
		return false
	}
	if !c.ports[port] {
		return false
	}
	return len(c.ports) == 1
}

func (c listenCoverage) extraPorts(keep int) []int {
	var extra []int
	for p := range c.ports {
		if p != keep {
			extra = append(extra, p)
		}
	}
	for _, r := range c.ranges {
		for p := r[0]; p <= r[1] && p-r[0] < 8; p++ {
			if p != keep {
				extra = append(extra, p)
			}
		}
		if r[1] != keep && r[1]-r[0] >= 8 {
			extra = append(extra, r[1])
		}
	}
	sort.Ints(extra)
	return uniqueInts(extra)
}

func (c listenCoverage) portCount() int {
	n := len(c.ports)
	for _, r := range c.ranges {
		if r[1] >= r[0] {
			n += r[1] - r[0] + 1
		}
	}
	return n
}

func (c listenCoverage) dump() string {
	var b strings.Builder
	b.WriteString("ports=")
	b.WriteString(fmt.Sprint(sortedPorts(c.ports)))
	b.WriteString(" ranges=")
	b.WriteString(fmt.Sprint(c.ranges))
	if c.raw != "" {
		b.WriteString(" inbound=")
		b.WriteString(c.raw)
	}
	return b.String()
}

func sortedPorts(ports map[int]bool) []int {
	out := make([]int, 0, len(ports))
	for p := range ports {
		out = append(out, p)
	}
	sort.Ints(out)
	return out
}

func uniqueInts(in []int) []int {
	if len(in) < 2 {
		return in
	}
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
