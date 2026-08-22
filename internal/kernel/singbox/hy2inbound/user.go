package hy2inbound

import (
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/hotuser"
	"github.com/sagernet/sing-box/option"
)

func hy2UserLists(users []option.Hysteria2User) (ids, passwords []string) {
	ids = make([]string, 0, len(users))
	passwords = make([]string, 0, len(users))
	for _, user := range users {
		ids = append(ids, hotuser.StableID(user.Name, user.Password))
		passwords = append(passwords, user.Password)
	}
	return ids, passwords
}

func (h *Inbound) UpdateUsers(users []option.Hysteria2User) error {
	from := h.userCount
	ids, passwords := hy2UserLists(users)
	h.service.UpdateUsers(ids, passwords)
	h.userCount = len(users)
	hotuser.LogUpdate("hysteria2", from, len(users))
	return nil
}
