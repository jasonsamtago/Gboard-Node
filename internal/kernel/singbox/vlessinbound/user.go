package vlessinbound

import (
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-vmess/vless"
	"github.com/sagernet/sing/common"
)

func (h *Inbound) UpdateUsers(users []option.VLESSUser) error {
	h.storeUsers(users)
	return h.replaceService(users)
}

func (h *Inbound) storeUsers(users []option.VLESSUser) {
	if users == nil {
		users = []option.VLESSUser{}
	}
	h.users.Store(append([]option.VLESSUser(nil), users...))
}

func (h *Inbound) loadUsers() []option.VLESSUser {
	users, _ := h.users.Load().([]option.VLESSUser)
	return users
}

func (h *Inbound) userName(index int) (string, bool) {
	users := h.loadUsers()
	if index < 0 || index >= len(users) {
		return "", false
	}
	name := users[index].Name
	if name == "" {
		return "", true
	}
	return name, true
}

// replaceService publishes a new vless.Service so UpdateUsers never writes
// the live user map that NewConnection is reading.
func (h *Inbound) replaceService(users []option.VLESSUser) error {
	svc := vless.NewService[int](h.logger, h.handler)
	svc.UpdateUsers(common.MapIndexed(users, func(index int, _ option.VLESSUser) int {
		return index
	}), common.Map(users, func(it option.VLESSUser) string {
		return it.UUID
	}), common.Map(users, func(it option.VLESSUser) string {
		return it.Flow
	}))
	h.service.Store(svc)
	return nil
}
