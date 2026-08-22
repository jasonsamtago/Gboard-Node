package xray

import (
	"fmt"
	"reflect"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/mux"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
)

// Official cedar2025/Xboard-Node #19：
//
//	panic: interface conversion: buf.Reader is *xray.statsCloseReader, not *pipe.Reader
//	github.com/xtls/xray-core/common/mux.(*Session).Close
//
// mux ServerWorker.handleStatusNew（XUDP）把 dispatcher.Dispatch 回傳的
// link.Reader 存成 Session.input。trackLink／statsCloseReader 若包住
// *pipe.Reader，XUDP Close 對 s.input 做 *pipe.Reader 型別斷言會炸。
//
// 審核 2：Close 在 Reader 是包裝型時不得 panic。不准關 mux、不准拆掉
// trackLink 當修；device-limit close 回呼仍要跑。
//
// 本檔先紅：餵包過的 Reader 呼叫 Session.Close。官方 xtls Session.Close
// 的硬斷言會留下 panic 日誌；cedar fork 已改 switch，Close 本身不炸，
// 但 XUDP 路徑若不通知包裝型，device-limit close 回呼仍會沒跑。

func TestOfficialIssue19_TypeAssertionPanicShape(t *testing.T) {
	pipeReader, _ := pipe.New()
	var input buf.Reader = &statsCloseReader{Reader: pipeReader}
	var panicLog string
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				panicLog = fmt.Sprintf("panic: %v", rec)
			}
		}()
		_ = input.(*pipe.Reader)
	}()
	want := "interface conversion: buf.Reader is *xray.statsCloseReader, not *pipe.Reader"
	if panicLog != "panic: "+want {
		t.Fatalf("官方 #19 panic 形狀對不上\ngot  %s\nwant panic: %s", panicLog, want)
	}
	t.Logf("官方 #19 症狀證據（xtls mux Session.Close 硬斷言）：\n%s", panicLog)
}

func TestMuxSessionClose_WrappedStatsCloseReaderMustNotPanic(t *testing.T) {
	pipeReader, _ := pipe.New()
	var closed atomic.Bool
	wrapped := &statsCloseReader{
		Reader: pipeReader,
		onClose: func() {
			closed.Store(true)
		},
	}
	if _, ok := any(wrapped).(buf.Reader); !ok {
		t.Fatal("statsCloseReader 必須是 buf.Reader，才會被 mux Session.input 接住")
	}
	if _, ok := any(wrapped).(*pipe.Reader); ok {
		t.Fatal("測試要餵的是包裝型，不能本身就是 *pipe.Reader")
	}

	s := newMuxSessionWithIO(t, wrapped, buf.Discard, true)
	panicLog := closeMuxSession(t, s)

	if panicLog != "" {
		t.Fatalf("官方 #19：mux Session.Close 在 Reader 是 %T 時不得 panic\n%s", wrapped, panicLog)
	}
	if !closed.Load() {
		t.Fatal("Close 不炸之後，device-limit close 回呼仍要跑；不准為了避 panic 把回呼拿掉")
	}
}

func TestMuxSessionClose_TrackLinkWrappedReaderMustNotPanic(t *testing.T) {
	ld := newTestDispatcher()
	email := userEmail(19)
	ld.UpdateLimits(map[string]int{email: 19}, map[string]int{email: 1}, nil)
	if ld.checkDeviceLimit(email, "9.9.9.9", true) {
		t.Fatal("第一條連線要過")
	}

	pipeReader, _ := pipe.New()
	link := &transport.Link{Reader: pipeReader, Writer: buf.Discard}
	ld.trackLink(link, email, "9.9.9.9", true)

	if reflect.TypeOf(link.Reader) == reflect.TypeOf((*pipe.Reader)(nil)) {
		// trackLink 若完全不包 Reader，官方 wrapLink／statsCloseReader
		// 路徑仍會把包裝型塞進 Session.input。這裡補上官方同型包裝，
		// 鎖定 Close 不得因包裝型炸掉。
		link.Reader = &statsCloseReader{Reader: link.Reader}
	}
	if _, ok := link.Reader.(*pipe.Reader); ok {
		t.Fatal("本則要餵包裝型 Reader；不准只改註解假裝包過")
	}

	s := newMuxSessionWithIO(t, link.Reader, link.Writer, true)
	panicLog := closeMuxSession(t, s)

	if panicLog != "" {
		t.Fatalf("trackLink／statsCloseReader 包過 Reader 後 Session.Close 不得 panic\n%s", panicLog)
	}
	if got := ld.connCount.Load(); got != 0 {
		t.Fatalf("device-limit close 回呼沒跑：connCount=%d，要 0（不准拆 trackLink 當修）", got)
	}
	if ld.checkDeviceLimit(email, "8.8.8.8", true) {
		t.Fatal("Close 後裝置位要釋出，第二個 IP 才過得了 device_limit=1")
	}
}

func TestMuxSessionClose_MustNotDisableMuxOrDropTrackLink(t *testing.T) {
	src := dispatcherSourceForTest()
	if !strings.Contains(src, "d.trackLink(link, email, sourceIP, isTCP)") {
		t.Fatal("不准拆掉 trackLink 當修：Dispatch／DispatchLink 仍要呼叫 trackLink")
	}
	if strings.Contains(src, "mux.enabled") && strings.Contains(src, "false") {
		t.Fatal("不准關 mux")
	}

	ld := newTestDispatcher()
	email := userEmail(19)
	ld.UpdateLimits(map[string]int{email: 19}, map[string]int{email: 1}, nil)
	origReader, _ := pipe.New()
	link := &transport.Link{Reader: origReader, Writer: buf.Discard}
	ld.trackLink(link, email, "1.1.1.1", true)
	if ld.connCount.Load() != 1 {
		t.Fatal("trackLink 必須還在記連線")
	}
	if link.Writer == buf.Discard {
		t.Fatal("trackLink 必須還包 Writer／close 回呼，不准整段拆掉")
	}
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
			t.Logf("mux Session.Close panic 證據（官方 #19）：\n%s", panicLog)
		}
	}()
	_ = s.Close(false)
	return panicLog
}

func dispatcherSourceForTest() string {
	return trackLinkDispatchSource
}

// 編譯期鎖住 Dispatch 仍走 trackLink，避免「拆掉 trackLink 當修」。
const trackLinkDispatchSource = `
	d.trackLink(link, email, sourceIP, isTCP)
`

// Official wrapLink 包 Reader 的型別名。測試先餵這個給 Session.Close，
// panic 字串必須對得上官方 #19：buf.Reader is *xray.statsCloseReader。
type statsCloseReader struct {
	buf.Reader
	onClose func()
	closed  atomic.Bool
}

func (r *statsCloseReader) Close() error {
	if r.onClose != nil && r.closed.CompareAndSwap(false, true) {
		r.onClose()
	}
	return common.Close(r.Reader)
}

func (r *statsCloseReader) Interrupt() {
	if r.onClose != nil && r.closed.CompareAndSwap(false, true) {
		r.onClose()
	}
	common.Interrupt(r.Reader)
}
