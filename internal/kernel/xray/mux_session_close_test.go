package xray

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"unsafe"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/mux"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

// Official cedar2025/Xboard-Node #19：mux Session.Close ＋真實 trackLink。
//
// 現況 trackLink 只包 Writer，不包 Reader。測試必須走真實 trackLink，
// 不准自己先 new statsCloseReader 套上去。XUDP Close 只驗不 panic
// （XUDP 本來就不關 output，不准拿它驗 device-limit 回呼）。回呼走
// 非 XUDP：Session.Close 會關 Writer。反向讀 dispatcher.go 原文。

func TestMuxSessionClose_TrackLinkPipeReaderXUDPMustNotPanic(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(19)
	ld.UpdateLimits(map[string]int{email: 19}, map[string]int{email: 1}, nil)

	pipeReader, _ := pipe.New()
	link := &transport.Link{Reader: pipeReader, Writer: buf.Discard}
	ld.trackLink(link, email, "9.9.9.9", true)

	if link.Reader != pipeReader {
		t.Fatalf("真實 trackLink 之後 Reader 必須仍是同一個 *pipe.Reader，got %T", link.Reader)
	}
	if _, ok := link.Reader.(*pipe.Reader); !ok {
		t.Fatalf("真實 trackLink 之後 Reader 必須仍是 *pipe.Reader，got %T；不准測試裡再包一層", link.Reader)
	}

	s := newMuxSessionWithIO(t, link.Reader, link.Writer, true)
	if panicLog := closeMuxSession(t, s); panicLog != "" {
		t.Fatalf("真實 trackLink 後的 *pipe.Reader 餵進 mux XUDP Session.Close 不得 panic\n%s", panicLog)
	}
}

func TestMuxSessionClose_NonXUDPCloseRunsDeviceLimitCallback(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(19)
	ld.UpdateLimits(map[string]int{email: 19}, map[string]int{email: 1}, nil)
	if ld.checkDeviceLimit(email, "9.9.9.9", true) {
		t.Fatal("第一條連線要過")
	}

	pipeReader, _ := pipe.New()
	link := &transport.Link{Reader: pipeReader, Writer: buf.Discard}
	ld.trackLink(link, email, "9.9.9.9", true)
	if _, ok := link.Reader.(*pipe.Reader); !ok {
		t.Fatalf("真實 trackLink 之後 Reader 必須仍是 *pipe.Reader，got %T", link.Reader)
	}

	// 非 XUDP：Session.Close 會 Interrupt input、Close output，
	// closeTrackingWriter.onClose 才會跑。不准用 XUDP Close 驗回呼。
	s := newMuxSessionWithIO(t, link.Reader, link.Writer, false)
	if panicLog := closeMuxSession(t, s); panicLog != "" {
		t.Fatalf("非 XUDP mux Session.Close 不得 panic\n%s", panicLog)
	}
	if got := ld.connCount.Load(); got != 0 {
		t.Fatalf("非 XUDP Close 後 device-limit close 回呼沒跑：connCount=%d，要 0", got)
	}
	if ld.checkDeviceLimit(email, "8.8.8.8", true) {
		t.Fatal("非 XUDP Close 後裝置位要釋出，第二個 IP 才過得了 device_limit=1")
	}
}

func TestMuxSessionClose_DispatcherSourceKeepsTrackLinkAndMux(t *testing.T) {
	src := readDispatcherGo(t)

	if !strings.Contains(src, "func (d *LimitDispatcher) Dispatch(") {
		t.Fatal("dispatcher.go 找不到 Dispatch，讀到的不是原文")
	}
	if !strings.Contains(src, "func (d *LimitDispatcher) DispatchLink(") {
		t.Fatal("dispatcher.go 找不到 DispatchLink，讀到的不是原文")
	}

	dispatchBody := funcBody(t, src, "func (d *LimitDispatcher) Dispatch(")
	dispatchLinkBody := funcBody(t, src, "func (d *LimitDispatcher) DispatchLink(")
	if !strings.Contains(dispatchBody, "d.trackLink(") {
		t.Fatal("不准拆掉 trackLink 當修：Dispatch 必須呼叫 d.trackLink(...)")
	}
	if !strings.Contains(dispatchLinkBody, "d.trackLink(") {
		t.Fatal("不准拆掉 trackLink 當修：DispatchLink 必須呼叫 d.trackLink(...)")
	}

	trackBody := funcBody(t, src, "func (d *LimitDispatcher) trackLink(")
	if regexp.MustCompile(`link\.Reader\s*=`).MatchString(trackBody) {
		t.Fatal("trackLink 不准重置 link.Reader：mux Session.input 必須仍是 *pipe.Reader")
	}

	disabled := []string{
		`"mux": false`,
		`"mux":false`,
		"mux.Enabled = false",
		"DisableMux",
		"disableMux",
		"mux.enabled = false",
	}
	for _, needle := range disabled {
		if strings.Contains(src, needle) {
			t.Fatalf("不准關 mux：dispatcher.go 出現 %q", needle)
		}
	}
}

func readDispatcherGo(t *testing.T) string {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller 找不到測試檔路徑")
	}
	path := filepath.Join(filepath.Dir(testFile), "dispatcher.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("os.ReadFile(%s)：%v", path, err)
	}
	src := string(raw)
	if !strings.Contains(src, "package xray") {
		t.Fatalf("%s 不是 dispatcher.go 原文", path)
	}
	return src
}

func funcBody(t *testing.T, src, signature string) string {
	t.Helper()
	start := strings.Index(src, signature)
	if start < 0 {
		t.Fatalf("dispatcher.go 找不到 %s", signature)
	}
	brace := strings.Index(src[start:], "{")
	if brace < 0 {
		t.Fatalf("%s 找不到函數本體", signature)
	}
	i := start + brace
	depth := 0
	for ; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("%s 函數本體沒有收尾", signature)
	return ""
}

func newMuxSessionWithIO(t *testing.T, r buf.Reader, w buf.Writer, xudp bool) *mux.Session {
	t.Helper()
	sm := mux.NewSessionManager()
	s := sm.Allocate(&mux.ClientStrategy{})
	if s == nil {
		t.Fatal("mux.SessionManager.Allocate 回傳 nil")
	}
	setMuxSessionField(t, s, "input", r)
	setMuxSessionField(t, s, "output", w)
	if xudp {
		s.XUDP = &mux.XUDP{Mux: s, Status: mux.Active}
	}
	return s
}

func setMuxSessionField(t *testing.T, s *mux.Session, name string, val any) {
	t.Helper()
	f := reflect.ValueOf(s).Elem().FieldByName(name)
	if !f.IsValid() {
		t.Fatalf("mux.Session 沒有欄位 %s", name)
	}
	ptr := unsafe.Pointer(f.UnsafeAddr())
	reflect.NewAt(f.Type(), ptr).Elem().Set(reflect.ValueOf(val))
}

func closeMuxSession(t *testing.T, s *mux.Session) (panicLog string) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			panicLog = fmt.Sprintf("panic: %v\n%s", rec, debug.Stack())
			t.Logf("mux Session.Close panic：\n%s", panicLog)
		}
	}()
	_ = s.Close(false)
	return panicLog
}
