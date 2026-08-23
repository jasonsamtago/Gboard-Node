package xray

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/config"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel"
	"github.com/jasonsamtago/Gboard-Node/internal/model"
	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
	"github.com/xtls/xray-core/app/router"
	"google.golang.org/protobuf/proto"
)

// 對準官方 cedar2025/Xboard-Node #20：節點編輯自訂路由用到 geoip／geosite，
// 起核卻只判斷面板 Routes 有沒有 geo，自訂路由不看，docker／非 docker
// 都要手動把 geoip.dat／geosite.dat 放到 /usr/local/bin。
//
// 官方錯誤形狀：
//
//	failed to start kernel error=parse xray config: ...
//	failed to load GeoIP: private > failed to open file: geoip.dat >
//	open /usr/local/bin/geoip.dat: no such file or directory
//
// 審核 2 鎖定：
//   - 僅自訂路由含 geoip:／geosite:（面板 Routes 空或無 geo）也必須自動下載
//   - 起核（或 ensureGeoData）後 geo 檔存在，不得再出現 geoip.dat: no such file
//   - 不准只看面板 Routes
//   - 不准拆掉自訂 geo 當修
//   - 不准改成手動下載當修
//
// 這份只鎖失敗行為，不實作修正。假 HTTP 餵小檔，不准真從 GitHub 下大檔。

const (
	issue20OfficialMissing = "geoip.dat: no such file"
	issue20OfficialLoadIP  = "failed to load GeoIP"
	issue20OfficialOpen    = "failed to open file: geoip.dat"
	issue20GeoIPTag        = "geoip:private"
	issue20GeoSiteTag      = "geosite:cn"
)

func TestCustomRouteGeo_Needs不得只看面板Routes(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)
	hook := installIssue20FakeGeoHTTP(t)

	for _, tc := range []struct {
		name string
		spec *model.NodeSpec
		cfg  config.KernelConfig
	}{
		{
			name: "custom_route_rules",
			spec: issue20SpecWithCustomRouteRules(),
		},
		{
			name: "custom_routes",
			spec: issue20SpecWithCustomRoutes(),
		},
		{
			name: "kernel_custom_route",
			spec: issue20BareSpec(),
			cfg:  config.KernelConfig{CustomRoute: issue20RawCustomRoutes()},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if kernel.NeedsGeoIP(tc.spec.Routes) || kernel.NeedsGeoSite(tc.spec.Routes) {
				t.Fatal("這個案例面板 Routes 不該含 geo，否則測不到官方 #20（只看面板 Routes）")
			}

			dir := t.TempDir()
			cfg := tc.cfg
			cfg.Type = "xray"
			cfg.LogLevel = "warn"
			cfg.GeoDataDir = dir
			x := New(cfg)
			x.ensureGeoData(tc.spec)

			assertIssue20GeoDownloaded(t, dir, hook, "Needs／Ensure 路徑只看面板 Routes：自訂路由含 "+issue20GeoIPTag+"／"+issue20GeoSiteTag+" 卻沒自動下載")
		})
	}
}

func TestCustomRouteGeo_僅CustomRouteRules含geo必須起核(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)
	hook := installIssue20FakeGeoHTTP(t)
	spec := issue20SpecWithCustomRouteRules()
	assertIssue20CompiledKeepsCustomGeo(t, spec, config.KernelConfig{})
	startIssue20Kernel(t, spec, config.KernelConfig{}, hook)
}

func TestCustomRouteGeo_僅CustomRoutes含geo必須起核(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)
	hook := installIssue20FakeGeoHTTP(t)
	spec := issue20SpecWithCustomRoutes()
	assertIssue20CompiledKeepsCustomGeo(t, spec, config.KernelConfig{})
	startIssue20Kernel(t, spec, config.KernelConfig{}, hook)
}

func TestCustomRouteGeo_僅kernel_custom_route含geo必須起核(t *testing.T) {
	nlog.Init(io.Discard, slog.LevelError, false)
	hook := installIssue20FakeGeoHTTP(t)
	spec := issue20BareSpec()
	cfg := config.KernelConfig{CustomRoute: issue20RawCustomRoutes()}
	assertIssue20CompiledKeepsCustomGeo(t, spec, cfg)
	startIssue20Kernel(t, spec, cfg, hook)
}

func TestCustomRouteGeo_假修要被抓到(t *testing.T) {
	xraySrc, err := os.ReadFile("xray.go")
	if err != nil {
		t.Fatalf("讀 xray.go: %v", err)
	}
	configSrc, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("讀 config.go: %v", err)
	}
	geoSrc, err := os.ReadFile(filepath.Join("..", "geo.go"))
	if err != nil {
		t.Fatalf("讀 geo.go: %v", err)
	}
	geodataSrc, err := os.ReadFile(filepath.Join("..", "geodata", "geodata.go"))
	if err != nil {
		t.Fatalf("讀 geodata.go: %v", err)
	}

	ensureFn := extractGoFunc(t, xraySrc, "func (x *Xray) ensureGeoData")
	if !bytes.Contains(ensureFn, []byte("geodata.Ensure")) {
		t.Fatal("改成手動下載當修：ensureGeoData 不再呼叫 geodata.Ensure")
	}
	if !bytes.Contains(geodataSrc, []byte("geoip.dat")) || !bytes.Contains(geodataSrc, []byte("geosite.dat")) {
		t.Fatal("改成手動下載當修：geodata.Ensure 不再下載 xray geoip.dat／geosite.dat")
	}
	if !bytes.Contains(configSrc, []byte("nc.CustomRouteRules")) || !bytes.Contains(configSrc, []byte("nc.CustomRoutes")) {
		t.Fatal("拆掉自訂路由當修：buildRouting 不再吃 CustomRouteRules／CustomRoutes")
	}
	if !bytes.Contains(configSrc, []byte("kcfg.CustomRoute")) {
		t.Fatal("拆掉自訂路由當修：buildRouting 不再吃 kernel custom_route")
	}

	looksOnlyAtPanelRoutes := bytes.Contains(ensureFn, []byte("NeedsGeoIP(nc.Routes)")) &&
		bytes.Contains(ensureFn, []byte("NeedsGeoSite(nc.Routes)")) &&
		!bytes.Contains(ensureFn, []byte("CustomRoute")) &&
		!bytes.Contains(ensureFn, []byte("NeedsGeoIPRules")) &&
		!bytes.Contains(ensureFn, []byte("NeedsGeoSiteRules"))
	if looksOnlyAtPanelRoutes {
		t.Fatal("不准只看面板 Routes：ensureGeoData 只用 NeedsGeoIP/NeedsGeoSite(nc.Routes)，自訂路由含 geoip／geosite 不會下載")
	}
	if !bytes.Contains(geoSrc, []byte("geoip:")) || !bytes.Contains(geoSrc, []byte("geosite:")) {
		t.Fatal("Needs 路徑不再認 geoip:／geosite: 前綴")
	}

	nlog.Init(io.Discard, slog.LevelError, false)
	hook := installIssue20FakeGeoHTTP(t)
	spec := issue20SpecWithCustomRouteRules()
	spec.CustomRoutes = issue20RawCustomRoutes()
	assertIssue20CompiledKeepsCustomGeo(t, spec, config.KernelConfig{})
	startIssue20Kernel(t, spec, config.KernelConfig{}, hook)
}

func startIssue20Kernel(t *testing.T, spec *model.NodeSpec, extra config.KernelConfig, hook *issue20GeoHook) *Xray {
	t.Helper()
	if kernel.NeedsGeoIP(spec.Routes) || kernel.NeedsGeoSite(spec.Routes) {
		t.Fatal("這個案例面板 Routes 不該含 geo，否則測不到官方 #20（只看面板 Routes）")
	}

	dir := t.TempDir()
	assertIssue20DirEmpty(t, dir)

	port := issue20FreeTCPPort(t)
	spec.Protocol = "shadowsocks"
	spec.ListenIP = "127.0.0.1"
	spec.ServerPort = port
	spec.Cipher = "aes-128-gcm"

	cfg := extra
	cfg.Type = "xray"
	cfg.LogLevel = "warn"
	cfg.GeoDataDir = dir

	x := New(cfg)
	if x.Name() != "xray" {
		t.Fatalf("不准改切別的核當修：Name()=%q，要 xray", x.Name())
	}

	err := x.Start(spec, testUsers, kernel.TLSCert{})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, issue20OfficialMissing) ||
			strings.Contains(msg, issue20OfficialOpen) ||
			strings.Contains(msg, issue20OfficialLoadIP) {
			t.Fatalf("自訂路由含 %s／%s 起核不得再出現官方 #20 錯誤（%s）：%v。不准只看面板 Routes、不准拆自訂 geo、不准改手動下載",
				issue20GeoIPTag, issue20GeoSiteTag, issue20OfficialMissing, err)
		}
		t.Fatalf("僅自訂路由含 geo 也必須能起 xray 核：%v", err)
	}
	t.Cleanup(x.Stop)
	if !x.IsRunning() {
		t.Fatal("Start 沒回錯但 kernel 沒在跑：略過起核是假修")
	}

	assertIssue20GeoDownloaded(t, dir, hook, "起核後 geo 檔必須由自動下載出現，不准預先手動放檔")
	return x
}

func assertIssue20CompiledKeepsCustomGeo(t *testing.T, spec *model.NodeSpec, extra config.KernelConfig) {
	t.Helper()
	cfg := extra
	cfg.Type = "xray"
	cfg.LogLevel = "warn"
	built := buildConfig(cfg, spec, testUsers, kernel.TLSCert{})
	raw, err := json.MarshalIndent(built, "", "  ")
	if err != nil {
		t.Fatalf("marshal routing: %v", err)
	}
	if !bytes.Contains(raw, []byte(issue20GeoIPTag)) {
		t.Fatalf("拆掉自訂 geo 當修：產出 routing 沒有 %s\n%s", issue20GeoIPTag, raw)
	}
	if !bytes.Contains(raw, []byte(issue20GeoSiteTag)) {
		t.Fatalf("拆掉自訂 geo 當修：產出 routing 沒有 %s\n%s", issue20GeoSiteTag, raw)
	}
}

func assertIssue20GeoDownloaded(t *testing.T, dir string, hook *issue20GeoHook, failPrefix string) {
	t.Helper()
	ipPath := filepath.Join(dir, "geoip.dat")
	sitePath := filepath.Join(dir, "geosite.dat")
	ipData, err := os.ReadFile(ipPath)
	if err != nil {
		t.Fatalf("%s：geoip.dat 不存在（%v）。官方形狀是 %s", failPrefix, err, issue20OfficialMissing)
	}
	siteData, err := os.ReadFile(sitePath)
	if err != nil {
		t.Fatalf("%s：geosite.dat 不存在（%v）", failPrefix, err)
	}
	if !bytes.Equal(ipData, hook.geoIP) {
		t.Fatalf("改成手動下載當修：geoip.dat 不是假 HTTP 餵的小檔（size=%d）。不准預先放檔、不准真從 GitHub 下大檔", len(ipData))
	}
	if !bytes.Equal(siteData, hook.geoSite) {
		t.Fatalf("改成手動下載當修：geosite.dat 不是假 HTTP 餵的小檔（size=%d）", len(siteData))
	}
	if hook.ipHits.Load() == 0 || hook.siteHits.Load() == 0 {
		t.Fatalf("改成手動下載當修：假 HTTP 沒被要 geoip.dat（%d）／geosite.dat（%d）。Needs／Ensure 必須自己下載",
			hook.ipHits.Load(), hook.siteHits.Load())
	}
}

func assertIssue20DirEmpty(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"geoip.dat", "geosite.dat"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Fatalf("測試前 %s 已存在：不准預先手動放檔當修", name)
		}
	}
}

func issue20BareSpec() *model.NodeSpec {
	return &model.NodeSpec{
		Protocol: "shadowsocks",
		ListenIP: "127.0.0.1",
		Cipher:   "aes-128-gcm",
		Routes:   nil,
	}
}

func issue20SpecWithCustomRouteRules() *model.NodeSpec {
	spec := issue20BareSpec()
	spec.CustomRouteRules = []model.CustomRouteRule{
		{
			Name: "issue20-geoip",
			Match: model.RouteMatch{
				IPCIDRs: []string{issue20GeoIPTag},
			},
			Action: model.RouteAction{Type: "direct"},
		},
		{
			Name: "issue20-geosite",
			Match: model.RouteMatch{
				Domains: []string{issue20GeoSiteTag},
			},
			Action: model.RouteAction{Type: "direct"},
		},
	}
	return spec
}

func issue20SpecWithCustomRoutes() *model.NodeSpec {
	spec := issue20BareSpec()
	spec.CustomRoutes = issue20RawCustomRoutes()
	return spec
}

func issue20RawCustomRoutes() []map[string]any {
	return []map[string]any{
		{
			"type":        "field",
			"ip":          []string{issue20GeoIPTag},
			"outboundTag": "direct",
		},
		{
			"type":        "field",
			"domain":      []string{issue20GeoSiteTag},
			"outboundTag": "direct",
		},
	}
}

type issue20GeoHook struct {
	geoIP    []byte
	geoSite  []byte
	ipHits   atomic.Int64
	siteHits atomic.Int64
}

func installIssue20FakeGeoHTTP(t *testing.T) *issue20GeoHook {
	t.Helper()
	hook := &issue20GeoHook{
		geoIP:   mustMiniGeoIPDAT(t),
		geoSite: mustMiniGeoSiteDAT(t),
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "geoip"):
			hook.ipHits.Add(1)
			_, _ = w.Write(hook.geoIP)
		case strings.Contains(r.URL.Path, "geosite"):
			hook.siteHits.Add(1)
			_, _ = w.Write(hook.geoSite)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(ts.Close)

	origAsset, hadAsset := os.LookupEnv("XRAY_LOCATION_ASSET")
	t.Cleanup(func() {
		if hadAsset {
			_ = os.Setenv("XRAY_LOCATION_ASSET", origAsset)
		} else {
			_ = os.Unsetenv("XRAY_LOCATION_ASSET")
		}
	})

	orig := http.DefaultTransport
	http.DefaultTransport = &issue20RewriteTransport{base: orig, fake: ts.URL}
	t.Cleanup(func() { http.DefaultTransport = orig })
	return hook
}

type issue20RewriteTransport struct {
	base http.RoundTripper
	fake string
}

func (t *issue20RewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL != nil && issue20IsGeoDownload(req.URL) {
		fake, err := url.Parse(t.fake)
		if err != nil {
			return nil, err
		}
		cloned := req.Clone(req.Context())
		cloned.URL.Scheme = fake.Scheme
		cloned.URL.Host = fake.Host
		cloned.Host = fake.Host
		cloned.RequestURI = ""
		return t.base.RoundTrip(cloned)
	}
	return t.base.RoundTrip(req)
}

func issue20IsGeoDownload(u *url.URL) bool {
	path := u.Path
	return strings.Contains(path, "geoip.dat") ||
		strings.Contains(path, "geosite.dat") ||
		strings.Contains(path, "geoip.db") ||
		strings.Contains(path, "geosite.db")
}

func mustMiniGeoIPDAT(t *testing.T) []byte {
	t.Helper()
	raw, err := proto.Marshal(&router.GeoIPList{
		Entry: []*router.GeoIP{{
			CountryCode: "PRIVATE",
			Cidr: []*router.CIDR{{
				Ip:     []byte{192, 168, 0, 0},
				Prefix: 16,
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal mini geoip.dat: %v", err)
	}
	return raw
}

func mustMiniGeoSiteDAT(t *testing.T) []byte {
	t.Helper()
	raw, err := proto.Marshal(&router.GeoSiteList{
		Entry: []*router.GeoSite{{
			CountryCode: "CN",
			Domain: []*router.Domain{{
				Type:  router.Domain_Domain,
				Value: "example.cn",
			}},
		}},
	})
	if err != nil {
		t.Fatalf("marshal mini geosite.dat: %v", err)
	}
	return raw
}

func extractGoFunc(t *testing.T, src []byte, signature string) []byte {
	t.Helper()
	start := bytes.Index(src, []byte(signature))
	if start < 0 {
		t.Fatalf("找不到 %s", signature)
	}
	rest := src[start:]
	brace := bytes.IndexByte(rest, '{')
	if brace < 0 {
		t.Fatalf("%s 沒有函式本體", signature)
	}
	depth := 0
	for i, b := range rest[brace:] {
		switch b {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:brace+i+1]
			}
		}
	}
	t.Fatalf("%s 括號不配對", signature)
	return nil
}

func issue20FreeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

