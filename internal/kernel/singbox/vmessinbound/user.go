package vmessinbound

import (
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-vmess"
	"github.com/sagernet/sing/common"
)

func (h *Inbound) UpdateUsers(users []option.VMessUser) error {
	h.storeUsers(users)
	return h.replaceService(users)
}

func (h *Inbound) storeUsers(users []option.VMessUser) {
	if users == nil {
		users = []option.VMessUser{}
	}
	h.users.Store(append([]option.VMessUser(nil), users...))
}

func (h *Inbound) loadUsers() []option.VMessUser {
	users, _ := h.users.Load().([]option.VMessUser)
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

// replaceService publishes a new vmess.Service so UpdateUsers never writes
// the live user map that NewConnection is reading.
func (h *Inbound) replaceService(users []option.VMessUser) error {
	svc := vmess.NewService[int](h.handler, h.serviceOptions...)
	err := svc.UpdateUsers(common.MapIndexed(users, func(index int, _ option.VMessUser) int {
		return index
	}), common.Map(users, func(it option.VMessUser) string {
		return it.UUID
	}), common.Map(users, func(it option.VMessUser) int {
		return it.AlterId
	}))
	if err != nil {
		return err
	}
	if h.started.Load() {
		if err = svc.Start(); err != nil {
			return err
		}
	}
	if old := h.service.Swap(svc); old != nil {
		_ = old.Close()
	}
	return nil
}
