package singbox

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	officialHy2 "github.com/sagernet/sing-box/protocol/hysteria2"
	officialTuic "github.com/sagernet/sing-box/protocol/tuic"
	"github.com/sagernet/sing/common/auth"
)

// Official cedar2025/Xboard-Node #49（1.1.13 進程突然掛掉）：
//
//	users hot-swapped users=211
//	users updated: +0 -1
//	panic: runtime error: index out of range [211] with length 211
//	hysteria2.(*Inbound).NewConnectionEx  ← inbound.go:159
//
// 官方 inbound 把陣列下標當作用戶 id（Service[int] + userNameList[userID]）。
// 熱刪最後一人後，舊 session／舊 password 仍帶 index==新長度（=舊最後下標），
// 或 index==舊長度，直接 OOR 把進程打掛。
//
// #54 已在 hy2inbound／tuicinbound 鎖 StableID／熱更新錯位；那份測試走的是
// 已覆寫 inbound，現況已綠。本檔單獨鎖「刪用戶變短不得掛」回歸，故意走
// **未走 StableID** 的官方 protocol/hysteria2、protocol/tuic inbound
// （與 #54 第一版失敗測試同一條 index 路徑），讓當前 `dev` 先紅。
//
// 審核鎖：
//  1. 熱更新 users 變短（+0 -N，至少刪一人）後，既有／新連線不得 panic
//     `index out of range`（含官方形狀 index==舊長度／新長度）。
//  2. inbound 必須續活：UpdateUsers 後仍可服務其餘用戶。
//  3. 不准假修：關掉 UpdateUsers／熱更新、關掉 Hy2／TUIC、改成必須重啟才刪用戶。
//  4. 與 #54 分開：測試名／斷言對準 #49 OOR panic，不重做流量錯位鎖。
//  5. Hy2 為主；TUIC 同一套 user index 路徑一併鎖。

const (
	// 4 人縮成 3 人（+0 -1），等價官方 212→211。
	// 舊最後下標 3 == 新長度 3，對應官方 [211] with length 211。
	issue49OldLen    = 4
	issue49LastIndex = 3
)

func TestHysteria2DeleteLastUser_OfficialIndexEqualNewLengthDoesNotPanic(t *testing.T) {
	// 官方形狀：刪掉最後一人後 leftover index == 新長度（=舊最後下標）。
	in, router := newOfficialHy2Inbound(t, issue49FourUsers())
	if err := in.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪最後一人）: %v", err)
	}
	assertIssue49NoOOR(t, "hy2 tcp index==新長度", func() {
		_ = routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
	})
	assertIssue49NoOOR(t, "hy2 udp index==新長度", func() {
		_ = routeHy2Packet(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
	})
	assertIssue49RemainingHy2(t, in, router)
}

func TestHysteria2DeleteLastUser_OfficialIndexEqualOldLengthDoesNotPanic(t *testing.T) {
	// 審核 1：含官方形狀 index==舊長度（userNameList[len]）。
	in, router := newOfficialHy2Inbound(t, issue49FourUsers())
	if err := in.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪最後一人）: %v", err)
	}
	assertIssue49NoOOR(t, "hy2 tcp index==舊長度", func() {
		_ = routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
	})
	assertIssue49NoOOR(t, "hy2 udp index==舊長度", func() {
		_ = routeHy2Packet(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
	})
	assertIssue49RemainingHy2(t, in, router)
}

func TestHysteria2DeleteMiddleUser_StaleLastIndexDoesNotPanic(t *testing.T) {
	in, router := newOfficialHy2Inbound(t, issue49FourUsers())
	if err := in.UpdateUsers(issue49Hy2WithoutMiddle()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪中間一人）: %v", err)
	}
	assertIssue49NoOOR(t, "hy2 刪中間後舊最後下標", func() {
		_ = routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
	})
	assertIssue49NoOOR(t, "hy2 刪中間後 index==舊長度", func() {
		_ = routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
	})
	got := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), 0))
	if got != hy2TuicUserCarol {
		t.Fatalf("刪中間後其餘用戶必須仍可服務：slot0=%q, want Carol", got)
	}
}

func TestTUICDeleteLastUser_OfficialIndexEqualNewLengthDoesNotPanic(t *testing.T) {
	in, router := newOfficialTUICInbound(t, issue49FourTUICUsers())
	if err := in.UpdateUsers(issue49TUICWithoutLast()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪最後一人）: %v", err)
	}
	assertIssue49NoOOR(t, "tuic tcp index==新長度", func() {
		_ = routeTUICConn(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
	})
	assertIssue49NoOOR(t, "tuic udp index==新長度", func() {
		_ = routeTUICPacket(t, in, router, auth.ContextWithUser(context.Background(), issue49LastIndex))
	})
	assertIssue49RemainingTUIC(t, in, router)
}

func TestTUICDeleteLastUser_OfficialIndexEqualOldLengthDoesNotPanic(t *testing.T) {
	in, router := newOfficialTUICInbound(t, issue49FourTUICUsers())
	if err := in.UpdateUsers(issue49TUICWithoutLast()); err != nil {
		t.Fatalf("UpdateUsers +0 -1（刪最後一人）: %v", err)
	}
	assertIssue49NoOOR(t, "tuic tcp index==舊長度", func() {
		_ = routeTUICConn(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
	})
	assertIssue49NoOOR(t, "tuic udp index==舊長度", func() {
		_ = routeTUICPacket(t, in, router, auth.ContextWithUser(context.Background(), issue49OldLen))
	})
	assertIssue49RemainingTUIC(t, in, router)
}

func TestHy2TUICDeleteUser_InboundStaysAliveAfterPlusZeroMinusOne(t *testing.T) {
	// 審核 2／3：UpdateUsers 後 inbound 續活，仍可再熱更新、服務其餘用戶。
	// 不准重啟 inbound、不准關掉 Hy2／TUIC／UpdateUsers。
	hy2In, hy2R := newOfficialHy2Inbound(t, issue49FourUsers())
	tuicIn, tuicR := newOfficialTUICInbound(t, issue49FourTUICUsers())

	if err := hy2In.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("hy2 UpdateUsers: %v", err)
	}
	if err := tuicIn.UpdateUsers(issue49TUICWithoutLast()); err != nil {
		t.Fatalf("tuic UpdateUsers: %v", err)
	}

	assertIssue49NoOOR(t, "hy2 續活舊下標", func() {
		_ = routeHy2Conn(t, hy2In, hy2R, auth.ContextWithUser(context.Background(), issue49LastIndex))
	})
	assertIssue49NoOOR(t, "tuic 續活舊下標", func() {
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

func TestHy2DeleteUser_OfficialOORLogEvidence(t *testing.T) {
	var lines []string
	logf := func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	in, router := newOfficialHy2Inbound(t, issue49FourUsers())
	logf("before hy2 users=%d path=UpdateUsers restart=false", issue49OldLen)

	if err := in.UpdateUsers(issue49Hy2WithoutLast()); err != nil {
		t.Fatalf("UpdateUsers: %v", err)
	}
	logf("update hy2 users updated: +0 -1 from=%d to=%d removed=%s restart=false", issue49OldLen, issue49OldLen-1, hy2TuicUserAlice)

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

	remain := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), 0))
	logf("after hy2 remaining slot0 user=%s inbound_alive=true", remain)

	logText := strings.Join(lines, "\n")
	t.Log("\n" + logText)
	if panicked != "" {
		t.Fatalf("官方 #49：users updated +0 -1 後 NewConnectionEx 不得 panic index out of range（含 [3] with length 3／[211] with length 211）: %s\n%s", panicked, logText)
	}
	if remain != hy2TuicUserCarol {
		t.Fatalf("其餘用戶必須仍可服務：slot0=%q\n%s", remain, logText)
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
			t.Fatalf("官方 #49 %s 不得 panic index out of range（官方 hysteria2 NewConnectionEx inbound.go:159）: %v", label, rec)
		}
	}()
	fn()
}

func assertIssue49RemainingHy2(t *testing.T, in hy2Inbound, router *captureRouter) {
	t.Helper()
	got := routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), 0))
	if got != hy2TuicUserCarol {
		t.Fatalf("Hy2 刪用戶後其餘用戶必須仍可服務：slot0=%q, want Carol（不准關掉 Hy2／UpdateUsers）", got)
	}
	got = routeHy2Conn(t, in, router, auth.ContextWithUser(context.Background(), 2))
	if got != hy2TuicUserEve {
		t.Fatalf("Hy2 刪用戶後其餘用戶必須仍可服務：slot2=%q, want Eve", got)
	}
}

func assertIssue49RemainingTUIC(t *testing.T, in tuicInbound, router *captureRouter) {
	t.Helper()
	got := routeTUICConn(t, in, router, auth.ContextWithUser(context.Background(), 0))
	if got != hy2TuicUserCarol {
		t.Fatalf("TUIC 刪用戶後其餘用戶必須仍可服務：slot0=%q, want Carol（不准關掉 TUIC／UpdateUsers）", got)
	}
	got = routeTUICConn(t, in, router, auth.ContextWithUser(context.Background(), 2))
	if got != hy2TuicUserEve {
		t.Fatalf("TUIC 刪用戶後其餘用戶必須仍可服務：slot2=%q, want Eve", got)
	}
}

func newOfficialHy2Inbound(t *testing.T, users []option.Hysteria2User) (hy2Inbound, *captureRouter) {
	t.Helper()
	if len(users) != issue49OldLen {
		t.Fatalf("官方 #49 repro 先載入 %d 人，got %d", issue49OldLen, len(users))
	}
	router := &captureRouter{}
	raw, err := officialHy2.NewInbound(context.Background(), router, log.NewNOPFactory().Logger(), "hy2-issue49", option.Hysteria2InboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     hy2TuicListenAddr(),
			ListenPort: 18543,
		},
		Users: users,
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: hy2TuicTestTLS(t),
		},
	})
	if err != nil {
		t.Fatalf("official hysteria2.NewInbound: %v", err)
	}
	in, ok := raw.(hy2Inbound)
	if !ok {
		t.Fatalf("official hysteria2 inbound missing UpdateUsers／NewConnectionEx（不准關掉 Hy2）")
	}
	return in, router
}

func newOfficialTUICInbound(t *testing.T, users []option.TUICUser) (tuicInbound, *captureRouter) {
	t.Helper()
	if len(users) != issue49OldLen {
		t.Fatalf("官方 #49 repro 先載入 %d 人，got %d", issue49OldLen, len(users))
	}
	router := &captureRouter{}
	raw, err := officialTuic.NewInbound(context.Background(), router, log.NewNOPFactory().Logger(), "tuic-issue49", option.TUICInboundOptions{
		ListenOptions: option.ListenOptions{
			Listen:     hy2TuicListenAddr(),
			ListenPort: 18544,
		},
		Users: users,
		InboundTLSOptionsContainer: option.InboundTLSOptionsContainer{
			TLS: hy2TuicTestTLS(t),
		},
	})
	if err != nil {
		t.Fatalf("official tuic.NewInbound: %v", err)
	}
	in, ok := raw.(tuicInbound)
	if !ok {
		t.Fatalf("official tuic inbound missing UpdateUsers／NewConnectionEx（不准關掉 TUIC）")
	}
	return in, router
}
