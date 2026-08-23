package singbox

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/auth"
)

// Official cedar2025/Xboard-Node #49（1.1.13 進程突然掛掉）：
//
//	users hot-swapped users=211
//	users updated: +0 -1
//	panic: runtime error: index out of range [211] with length 211
//	hysteria2.(*Inbound).NewConnectionEx
//
// 官方 stock inbound 用陣列下標當作用戶 id；熱刪變短後舊 session／舊 password
// 的 leftover index（含 index==舊長度／新長度）OOR 把進程打掛。
//
// 本檔鎖 Gboard-Node **production 路徑**：hy2inbound／tuicinbound + UpdateUsers
// ＋ StableID（與 #54 同一套 inbound，見 newTestHysteria2Inbound）。
// 不准測已被覆寫的 stock protocol/hysteria2／protocol/tuic——那條永遠紅、
// 鎖不到上線行為。
//
// #54 已鎖 StableID／熱更新錯位，現況 hy2inbound 對 leftover int 不 panic。
// 本票單獨鎖「刪用戶變短不得掛」回歸：+0 -N 後 NewConnectionEx／既有連線
// 不得 index out of range，其餘用戶續活，必須走 UpdateUsers。
// 當前 tip（含 #54）預期綠——這是回歸鎖，不是再逼紅。
//
// 審核鎖：
//  1. 熱更新 users 變短（+0 -N）後不得 panic index out of range。
//  2. inbound 續活，其餘用戶仍可服務（StableID／uuid）。
//  3. 不准關掉 UpdateUsers／熱更新、關掉 Hy2／TUIC、改成必須重啟才刪用戶。
//  4. 與 #54 分開：測試名對準 #49 刪用戶不掛。
//  5. Hy2 為主；TUIC 同一套路徑一併鎖。

const (
	// 4 人縮成 3 人（+0 -1），等價官方 212→211。
	// 舊最後下標 3 == 新長度 3，對應官方 [211] with length 211。
	issue49OldLen    = 4
	issue49LastIndex = 3
)

func TestHysteria2DeleteLastUser_StaleIndexEqualNewLengthDoesNotPanic(t *testing.T) {
	in, router := newTestHysteria2Inbound(t, issue49FourUsers())
	if err := in.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪最後一人）: %v", err)
	}
	assertIssue49NoOOR(t, "hy2 tcp leftover index==新長度", func() {
		got := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
		assertIssue49StaleIndexNotRebound(t, got, "hy2 tcp")
	})
	assertIssue49NoOOR(t, "hy2 udp leftover index==新長度", func() {
		got := routeHy2Packet(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
		assertIssue49StaleIndexNotRebound(t, got, "hy2 udp")
	})
	assertIssue49RemainingHy2(t, in, router)
}

func TestHysteria2DeleteLastUser_StaleIndexEqualOldLengthDoesNotPanic(t *testing.T) {
	in, router := newTestHysteria2Inbound(t, issue49FourUsers())
	if err := in.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪最後一人）: %v", err)
	}
	assertIssue49NoOOR(t, "hy2 tcp leftover index==舊長度", func() {
		got := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
		assertIssue49StaleIndexNotRebound(t, got, "hy2 tcp")
	})
	assertIssue49NoOOR(t, "hy2 udp leftover index==舊長度", func() {
		got := routeHy2Packet(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
		assertIssue49StaleIndexNotRebound(t, got, "hy2 udp")
	})
	assertIssue49RemainingHy2(t, in, router)
}

func TestHysteria2DeleteLastUser_ExistingUUIDConnDoesNotPanic(t *testing.T) {
	in, router := newTestHysteria2Inbound(t, issue49FourUsers())
	carolCtx := auth.ContextWithUser(context.Background(), hy2TuicUserCarol)
	aliceCtx := auth.ContextWithUser(context.Background(), hy2TuicUserAlice)

	if got := routeHy2Conn(t, in, router, carolCtx); got != hy2TuicUserCarol {
		t.Fatalf("前置：Carol StableID 必須能連: got %q", got)
	}

	if err := in.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("UpdateUsers +0 -1: %v", err)
	}

	assertIssue49NoOOR(t, "hy2 既有 Carol 連線", func() {
		if got := routeHy2Conn(t, in, router, carolCtx); got != hy2TuicUserCarol {
			t.Fatalf("刪 Alice 後 Carol 必須續活: got %q", got)
		}
	})
	assertIssue49NoOOR(t, "hy2 已刪 Alice 的舊 session", func() {
		got := routeHy2Conn(t, in, router, aliceCtx)
		if got != "" && got != hy2TuicUserAlice {
			t.Fatalf("已刪用戶舊 session 不得掛到別人頭上: got %q", got)
		}
	})
}

func TestHysteria2DeleteMiddleUser_StaleLastIndexDoesNotPanic(t *testing.T) {
	in, router := newTestHysteria2Inbound(t, issue49FourUsers())
	if err := in.UpdateUsers(issue49Hy2WithoutMiddle()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪中間一人）: %v", err)
	}
	assertIssue49NoOOR(t, "hy2 刪中間後舊最後下標", func() {
		got := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
		assertIssue49StaleIndexNotRebound(t, got, "hy2 刪中間")
	})
	assertIssue49NoOOR(t, "hy2 刪中間後 index==舊長度", func() {
		_ = routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
	})
	if got := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), hy2TuicUserCarol)); got != hy2TuicUserCarol {
		t.Fatalf("刪中間後 Carol 必須續活: got %q", got)
	}
	if got := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), hy2TuicUserAlice)); got != hy2TuicUserAlice {
		t.Fatalf("刪中間後 Alice 必須續活: got %q", got)
	}
}

func TestTUICDeleteLastUser_StaleIndexEqualNewLengthDoesNotPanic(t *testing.T) {
	in, router := newTestTUICInbound(t, issue49FourTUICUsers())
	if err := in.UpdateUsers(issue49TUICWithoutLast()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪最後一人）: %v", err)
	}
	assertIssue49NoOOR(t, "tuic tcp leftover index==新長度", func() {
		got := routeTUICConn(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
		assertIssue49StaleIndexNotRebound(t, got, "tuic tcp")
	})
	assertIssue49NoOOR(t, "tuic udp leftover index==新長度", func() {
		got := routeTUICPacket(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
		assertIssue49StaleIndexNotRebound(t, got, "tuic udp")
	})
	assertIssue49RemainingTUIC(t, in, router)
}

func TestTUICDeleteLastUser_StaleIndexEqualOldLengthDoesNotPanic(t *testing.T) {
	in, router := newTestTUICInbound(t, issue49FourTUICUsers())
	if err := in.UpdateUsers(issue49TUICWithoutLast()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪最後一人）: %v", err)
	}
	assertIssue49NoOOR(t, "tuic tcp leftover index==舊長度", func() {
		got := routeTUICConn(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
		assertIssue49StaleIndexNotRebound(t, got, "tuic tcp")
	})
	assertIssue49NoOOR(t, "tuic udp leftover index==舊長度", func() {
		got := routeTUICPacket(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
		assertIssue49StaleIndexNotRebound(t, got, "tuic udp")
	})
	assertIssue49RemainingTUIC(t, in, router)
}

func TestHy2TUICDeleteUser_InboundStaysAliveAfterPlusZeroMinusOne(t *testing.T) {
	// 必須走 UpdateUsers 熱刪，不准重啟 inbound、不准關掉 Hy2／TUIC。
	hy2In, hy2R := newTestHysteria2Inbound(t, issue49FourUsers())
	tuicIn, tuicR := newTestTUICInbound(t, issue49FourTUICUsers())

	if err := hy2In.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("hy2 UpdateUsers: %v", err)
	}
	if err := tuicIn.UpdateUsers(issue49TUICWithoutLast()); err != nil {
		t.Fatalf("tuic UpdateUsers: %v", err)
	}

	assertIssue49NoOOR(t, "hy2 續活 leftover index", func() {
		_ = routeHy2Conn(t, hy2In, hy2R, auth.ContextWithUser(context.Background(), issue49LastIndex))
	})
	assertIssue49NoOOR(t, "tuic 續活 leftover index", func() {
		_ = routeTUICConn(t, tuicIn, tuicR, auth.ContextWithUser(context.Background(), issue49LastIndex))
	})

	if err := hy2In.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("hy2 第二次 UpdateUsers 必須仍可用（不准改成必須重啟才刪用戶）: %v", err)
	}
	if err := tuicIn.UpdateUsers(issue49TUICWithoutLast()); err != nil {
		t.Fatalf("tuic 第二次 UpdateUsers 必須仍可用: %v", err)
	}
	assertIssue49RemainingHy2(t, hy2In, hy2R)
	assertIssue49RemainingTUIC(t, tuicIn, tuicR)
}

func TestHy2DeleteUser_UpdateUsersNoPanicLogEvidence(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	in, router := newTestHysteria2Inbound(t, issue49FourUsers())
	logf("before hy2 users=%d path=UpdateUsers inbound=hy2inbound restart=false", issue49OldLen)

	if err := in.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	logf("update hy2 users updated: +0 -1 from=%d to=%d removed=%s path=UpdateUsers restart=false", issue49OldLen, issue49OldLen-1, hy2TuicUserAlice)

	panicked := ""
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				panicked = fmt.Sprint(rec)
			}
		}()
		_ = routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
	}()
	logf("after hy2 NewConnectionEx stale_index=%d panic=%q", issue49LastIndex, panicked)

	remain := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), hy2TuicUserCarol))
	logf("after hy2 remaining carol_uuid user=%s inbound_alive=true path=UpdateUsers", remain)

	logText := strings.Join(lines, "\n")
	t.Log("\n" + logText)
	if panicked != "" {
		t.Fatalf("官方 #49：hy2inbound UpdateUsers +0 -1 後 NewConnectionEx 不得 panic index out of range: %s\n%s", panicked, logText)
	}
	if remain != hy2TuicUserCarol {
		t.Fatalf("其餘用戶必須仍可服務（StableID）：carol=%q\n%s", remain, logText)
	}
}

func issue49FourUsers() []option.Hysteria2User {
	return []option.Hysteria2User{
		{Name: hy2TuicUserCarol, Password: hy2TuicUserCarol},
		{Name: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, Password: hy2TuicUserEve},
		{Name: hy2TuicUserAlice, Password: hy2TuicUserAlice},
	}
}

func issue49Hy2WithoutLast() []option.Hysteria2User {
	return []option.Hysteria2User{
		{Name: hy2TuicUserCarol, Password: hy2TuicUserCarol},
		{Name: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, Password: hy2TuicUserEve},
	}
}

func issue49Hy2WithoutMiddle() []option.Hysteria2User {
	return []option.Hysteria2User{
		{Name: hy2TuicUserCarol, Password: hy2TuicUserCarol},
		{Name: hy2TuicUserEve, Password: hy2TuicUserEve},
		{Name: hy2TuicUserAlice, Password: hy2TuicUserAlice},
	}
}

func issue49FourTUICUsers() []option.TUICUser {
	return []option.TUICUser{
		{Name: hy2TuicUserCarol, UUID: hy2TuicUserCarol, Password: hy2TuicUserCarol},
		{Name: hy2TuicUserDave, UUID: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, UUID: hy2TuicUserEve, Password: hy2TuicUserEve},
		{Name: hy2TuicUserAlice, UUID: hy2TuicUserAlice, Password: hy2TuicUserAlice},
	}
}

func issue49TUICWithoutLast() []option.TUICUser {
	return []option.TUICUser{
		{Name: hy2TuicUserCarol, UUID: hy2TuicUserCarol, Password: hy2TuicUserCarol},
		{Name: hy2TuicUserDave, UUID: hy2TuicUserDave, Password: hy2TuicUserDave},
		{Name: hy2TuicUserEve, UUID: hy2TuicUserEve, Password: hy2TuicUserEve},
	}
}

func assertIssue49NoOOR(t *testing.T, label string, fn func()) {
	t.Helper()
	defer func() {
		if rec := recover(); rec != nil {
			t.Fatalf("官方 #49 %s：hy2inbound／tuicinbound 刪用戶變短不得 panic index out of range: %v", label, rec)
		}
	}()
	fn()
}

func assertIssue49StaleIndexNotRebound(t *testing.T, got, label string) {
	t.Helper()
	if got != "" && got != hy2TuicUserAlice {
		t.Fatalf("%s leftover int index 不得掛到別人頭上: got %q", label, got)
	}
}

func assertIssue49RemainingHy2(t *testing.T, in hy2Inbound, router *captureRouter) {
	t.Helper()
	if got := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), hy2TuicUserCarol)); got != hy2TuicUserCarol {
		t.Fatalf("Hy2 刪用戶後其餘用戶必須續活（StableID）：carol=%q（不准關掉 Hy2／UpdateUsers）", got)
	}
	if got := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), hy2TuicUserEve)); got != hy2TuicUserEve {
		t.Fatalf("Hy2 刪用戶後其餘用戶必須續活（StableID）：eve=%q", got)
	}
}

func assertIssue49RemainingTUIC(t *testing.T, in tuicInbound, router *captureRouter) {
	t.Helper()
	if got := routeTUICConn(t, in, router, auth.ContextWithUser(context.Background(), hy2TuicUserCarol)); got != hy2TuicUserCarol {
		t.Fatalf("TUIC 刪用戶後其餘用戶必須續活（StableID）：carol=%q（不准關掉 TUIC／UpdateUsers）", got)
	}
	if got := routeTUICConn(t, in, router, auth.ContextWithUser(context.Background(), hy2TuicUserEve)); got != hy2TuicUserEve {
		t.Fatalf("TUIC 刪用戶後其餘用戶必須續活（StableID）：eve=%q", got)
	}
}
