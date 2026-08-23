package panel

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// Official cedar2025/Xboard-Node #63：
//   'users[3660][0]' expected type 'string', got unconvertible type 'map[string]interface {}'
// 大量「用户数据格式错误」warn 把磁碟塞滿。
//
// 根因：weak decoder（mapstructure）把 users 某列的 [0]／uuid 識別欄
// 當成 map／object，卻宣告為 string。一筆壞列讓整批 decode 失敗，
// 錯誤字串列出每一列；sync 每輪再噴一次。
//
// 審核鎖：
//  1. 單筆壞列跳過；其餘正常 string UUID 仍要進 user list（上線）。
//  2. 整批不得因一筆壞列整體 error、清空或中止。
//  3. 同類錯誤日誌有上限：單次呼叫 ≤ 少數次；重複 sync 不得
//     「每 user 一條 × 每輪」線性暴增。
// 不准的假修：關 user sync／熱更新、有壞列就丟整份 users、
// 只把 log level 調靜音但壞列仍讓整批失敗、繞過解碼卻讓合法 users 不上線。

const (
	usersMalformedGoodA = "11111111-1111-1111-1111-111111111111"
	usersMalformedGoodB = "33333333-3333-3333-3333-333333333333"
	usersMalformedGoodC = "55555555-5555-5555-5555-555555555555"
	usersMalformedBadA  = "22222222-2222-2222-2222-222222222222"
	usersMalformedBadB  = "44444444-4444-4444-4444-444444444444"

	// 單次呼叫允許的同類錯誤日誌次數（彙總 1 次，或限速後極少次）。
	usersMalformedLogCapPerCall = 3
	// 多輪 sync 後仍不得接近「壞列數 × 輪數」。
	usersMalformedLogCapRepeat = 8
	usersMalformedBadRows      = 20
	usersMalformedSyncRounds   = 8
)

// usersMalformedMixedJSON：正常物件列＋ tuple 列 [0] 是 map＋ uuid 欄是 map。
// 對照官方 #63 的 unconvertible map[string]interface{}。
const usersMalformedMixedJSON = `{
  "users": [
    {"id": 1, "uuid": "11111111-1111-1111-1111-111111111111", "speed_limit": 0, "device_limit": 0},
    [{"uuid": "22222222-2222-2222-2222-222222222222"}, 50, 1],
    {"id": 3, "uuid": "33333333-3333-3333-3333-333333333333", "speed_limit": 10, "device_limit": 2},
    {"id": 4, "uuid": {"uuid": "44444444-4444-4444-4444-444444444444"}, "speed_limit": 0, "device_limit": 0},
    {"id": 5, "uuid": "55555555-5555-5555-5555-555555555555", "speed_limit": 100, "device_limit": 3}
  ],
  "timestamp": 1,
  "node_id": 7
}`

const usersMalformedAllGoodJSON = `{
  "users": [
    {"id": 1, "uuid": "11111111-1111-1111-1111-111111111111", "speed_limit": 0, "device_limit": 0},
    {"id": 5, "uuid": "55555555-5555-5555-5555-555555555555", "speed_limit": 100, "device_limit": 3}
  ],
  "timestamp": 1,
  "node_id": 7
}`

func TestDecodeUsers_MalformedTupleMapAtZero_KeepsGoodRows(t *testing.T) {
	ev, logs, called := receiveUsersEvent(t, usersMalformedMixedJSON)
	logText := logs.String()

	if !called {
		t.Fatalf("整批掛了：一筆壞列（tuple [0] 是 map／uuid 是 object）讓 sync.users 整包 return，合法 users 不上線:\n%s", logText)
	}
	if ev.Type != "" && ev.Type != WSEventSyncUsers {
		t.Fatalf("event.Type = %q, want %q", ev.Type, WSEventSyncUsers)
	}
	if len(ev.Users) == 0 {
		t.Fatalf("不准的假修／現況：有壞列就空 list 或整批失敗，正常 UUID 沒進 user list:\n%s", logText)
	}

	assertUsersHaveUUID(t, ev.Users, usersMalformedGoodA)
	assertUsersHaveUUID(t, ev.Users, usersMalformedGoodB)
	assertUsersHaveUUID(t, ev.Users, usersMalformedGoodC)
	assertUsersLackUUID(t, ev.Users, usersMalformedBadA)
	assertUsersLackUUID(t, ev.Users, usersMalformedBadB)

	if hits := countUserFormatErrorHits(logText); hits > usersMalformedLogCapPerCall {
		t.Fatalf("同類錯誤日誌 %d 次，單次呼叫上限 %d（官方 #63 會佔磁碟）:\n%s", hits, usersMalformedLogCapPerCall, logText)
	}
	t.Logf("解碼日誌（壞列跳過、好列上線）:\n%s", logText)
}

func TestDecodeUsers_NormalStringUUID_StillApplies(t *testing.T) {
	ev, logs, called := receiveUsersEvent(t, usersMalformedAllGoodJSON)
	logText := logs.String()
	if !called {
		t.Fatalf("回歸失敗：正常 string UUID 的 sync.users 沒送出事件:\n%s", logText)
	}
	if len(ev.Users) != 2 {
		t.Fatalf("正常 payload 應有 2 個 users，got %d:\n%+v\n%s", len(ev.Users), ev.Users, logText)
	}
	assertUsersHaveUUID(t, ev.Users, usersMalformedGoodA)
	assertUsersHaveUUID(t, ev.Users, usersMalformedGoodC)
	if hits := countUserFormatErrorHits(logText); hits != 0 {
		t.Fatalf("正常 UUID 不該噴格式錯誤日誌 (%d):\n%s", hits, logText)
	}
	t.Logf("回歸日誌（全好列）:\n%s", logText)
}

func TestDecodeUsers_MalformedRows_DoNotSpamLogsOnRepeatSync(t *testing.T) {
	raw := usersMalformedManyBadJSON(t)
	var all strings.Builder
	var last WSEvent
	var lastCalled bool

	for round := 0; round < usersMalformedSyncRounds; round++ {
		ev, logs, called := receiveUsersEvent(t, raw)
		last, lastCalled = ev, called
		all.WriteString(logs.String())
		hits := countUserFormatErrorHits(logs.String())
		if hits > usersMalformedLogCapPerCall {
			t.Fatalf("第 %d 輪同類錯誤日誌 %d 次，單次上限 %d:\n%s", round+1, hits, usersMalformedLogCapPerCall, logs.String())
		}
	}

	logText := all.String()
	total := countUserFormatErrorHits(logText)
	linear := usersMalformedBadRows * usersMalformedSyncRounds
	if total > usersMalformedLogCapRepeat {
		t.Fatalf("重複 sync %d 輪同類錯誤 %d 次，上限 %d；現況會狂刷或把每列錯誤都倒進 warn（官方 #63）:\n%s",
			usersMalformedSyncRounds, total, usersMalformedLogCapRepeat, trimLog(logText))
	}
	if total >= linear {
		t.Fatalf("日誌命中 %d，已達「每 user 一條×每輪」=%d", total, linear)
	}

	if !lastCalled || len(last.Users) == 0 {
		t.Fatalf("不准只把 log 調靜音：重複 sync 後合法 users 仍要在 list 裡 called=%v users=%d\n%s",
			lastCalled, len(last.Users), trimLog(logText))
	}
	assertUsersHaveUUID(t, last.Users, usersMalformedGoodA)
	assertUsersHaveUUID(t, last.Users, usersMalformedGoodC)
	t.Logf("重複 sync 日誌命中=%d（壞列=%d 輪=%d）:\n%s", total, usersMalformedBadRows, usersMalformedSyncRounds, trimLog(logText))
}

func TestGetUsers_MalformedTupleMapAtZero_KeepsGoodRows(t *testing.T) {
	ts, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/server/UniProxy/user" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(usersMalformedMixedJSON))
	})
	defer ts.Close()

	users, err := client.GetUsers()
	if err != nil {
		t.Fatalf("GetUsers 不得因一筆壞列整體失敗: %v", err)
	}
	if len(users) == 0 {
		t.Fatal("不准的假修：REST users 有壞列就空 list")
	}
	assertUsersHaveUUID(t, users, usersMalformedGoodA)
	assertUsersHaveUUID(t, users, usersMalformedGoodB)
	assertUsersHaveUUID(t, users, usersMalformedGoodC)
	assertUsersLackUUID(t, users, usersMalformedBadA)
	assertUsersLackUUID(t, users, usersMalformedBadB)
}

func receiveUsersEvent(t *testing.T, raw string) (WSEvent, *devicesLogBuf, bool) {
	t.Helper()
	logs := &devicesLogBuf{}
	nlog.Init(logs, slog.LevelDebug, false)

	var ev WSEvent
	called := false
	w := NewWSClient("ws://users-malformed.test", "tok", 1, WSClientConfig{}, func(event WSEvent) {
		called = true
		ev = event
	}, nil, nil)
	w.handleDataEvent(wsMessage{Event: WSEventSyncUsers, Data: []byte(raw)})
	if !called {
		t.Logf("handleDataEvent 沒有送出 sync.users（整包丟掉） log=\n%s", logs.String())
	}
	return ev, logs, called
}

func usersMalformedManyBadJSON(t *testing.T) string {
	t.Helper()
	users := make([]any, 0, usersMalformedBadRows+2)
	users = append(users, map[string]any{
		"id": 1, "uuid": usersMalformedGoodA, "speed_limit": 0, "device_limit": 0,
	})
	for i := 0; i < usersMalformedBadRows; i++ {
		users = append(users, []any{
			map[string]any{"uuid": usersMalformedBadA},
			0,
			0,
		})
	}
	users = append(users, map[string]any{
		"id": 5, "uuid": usersMalformedGoodC, "speed_limit": 100, "device_limit": 3,
	})
	raw, err := json.Marshal(map[string]any{
		"users":     users,
		"timestamp": 1,
		"node_id":   7,
	})
	if err != nil {
		t.Fatalf("marshal many-bad users: %v", err)
	}
	return string(raw)
}

func assertUsersHaveUUID(t *testing.T, users []User, uuid string) {
	t.Helper()
	for _, u := range users {
		if u.UUID == uuid {
			return
		}
	}
	t.Fatalf("正常 UUID %s 必須出現在結果（其餘列仍要上線／進 user list），got %+v", uuid, users)
}

func assertUsersLackUUID(t *testing.T, users []User, uuid string) {
	t.Helper()
	for _, u := range users {
		if u.UUID == uuid {
			t.Fatalf("壞列 UUID %s 應被跳過，卻出現在結果: %+v", uuid, users)
		}
	}
}

func countUserFormatErrorHits(logs string) int {
	hits := 0
	hits += strings.Count(logs, "expected type")
	hits += strings.Count(logs, "expected a map or struct")
	hits += strings.Count(logs, "unconvertible type")
	hits += strings.Count(logs, "cannot decode users payload")
	hits += strings.Count(logs, "用户数据格式")
	hits += strings.Count(logs, "使用者資料格式")
	lower := strings.ToLower(logs)
	hits += strings.Count(lower, "skip user")
	hits += strings.Count(logs, "跳過 user")
	hits += strings.Count(logs, "跳過使用者")
	return hits
}

func trimLog(s string) string {
	const max = 2500
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n... (truncated)"
}
