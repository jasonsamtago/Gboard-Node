package kernel

import (
	"testing"

	"github.com/jasonsamtago/Gboard-Node/internal/model"
)

// Official cedar2025/Xboard-Node #53：面板改同一 user ID 的 UUID 時，
// UserDiff 只把新帳號放進 toAdd，沒把舊帳號放進 toRemove。xray 以
// email（user@<id>）當 UserManager key，再次 AddUser 被拒 already exists，
// 舊 UUID 繼續有效。

func TestUserDiff_UUIDRotationRemovesThenAdds(t *testing.T) {
	oldUser := model.UserSpec{ID: 2942, UUID: "11111111-1111-1111-1111-111111111111"}
	newUser := model.UserSpec{ID: 2942, UUID: "3e4ce6e5-2d8d-4307-895f-c9a1a348cddd"}

	toAdd, toRemove := UserDiff([]model.UserSpec{oldUser}, []model.UserSpec{newUser})

	if len(toAdd) != 1 || toAdd[0] != newUser {
		t.Fatalf("toAdd = %#v, 要先加新 UUID %#v", toAdd, newUser)
	}
	if len(toRemove) != 1 || toRemove[0] != oldUser {
		t.Fatalf("toRemove = %#v, 換 UUID 必須先移除舊帳號 %#v（官方 #53：只加不刪）", toRemove, oldUser)
	}
}

func TestUserDiff_UnchangedUserNotRemoved(t *testing.T) {
	user := model.UserSpec{ID: 7, UUID: "22222222-2222-2222-2222-222222222222"}

	toAdd, toRemove := UserDiff([]model.UserSpec{user}, []model.UserSpec{user})

	if len(toAdd) != 0 || len(toRemove) != 0 {
		t.Fatalf("沒改 UUID 不該進 diff: toAdd=%#v toRemove=%#v", toAdd, toRemove)
	}
}

func TestUserDiff_RotateOneKeepsOthers(t *testing.T) {
	oldRotated := model.UserSpec{ID: 2942, UUID: "aaaaaaa1-1111-1111-1111-111111111111"}
	newRotated := model.UserSpec{ID: 2942, UUID: "bbbbbbb2-2222-2222-2222-222222222222"}
	stable := model.UserSpec{ID: 7, UUID: "22222222-2222-2222-2222-222222222222"}
	gone := model.UserSpec{ID: 3, UUID: "33333333-3333-3333-3333-333333333333"}
	fresh := model.UserSpec{ID: 9, UUID: "99999999-9999-9999-9999-999999999999"}

	toAdd, toRemove := UserDiff(
		[]model.UserSpec{oldRotated, stable, gone},
		[]model.UserSpec{newRotated, stable, fresh},
	)

	if !userSpecsContain(toAdd, newRotated) || !userSpecsContain(toAdd, fresh) {
		t.Fatalf("toAdd = %#v, 要含新 UUID 與新用戶", toAdd)
	}
	if userSpecsContain(toAdd, stable) {
		t.Fatalf("沒改 UUID 的 user@7 不該進 toAdd: %#v", toAdd)
	}
	if !userSpecsContain(toRemove, oldRotated) || !userSpecsContain(toRemove, gone) {
		t.Fatalf("toRemove = %#v, 要含舊 UUID 與被刪用戶", toRemove)
	}
	if userSpecsContain(toRemove, stable) {
		t.Fatalf("沒改 UUID 的 user@7 不該進 toRemove: %#v", toRemove)
	}
}

func userSpecsContain(list []model.UserSpec, want model.UserSpec) bool {
	for _, u := range list {
		if u == want {
			return true
		}
	}
	return false
}
