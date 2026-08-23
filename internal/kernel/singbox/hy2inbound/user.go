package hy2inbound

import (
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/hotuser"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-quic/hysteria2"
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
	if err := h.publishUsers(ids, passwords); err != nil {
		return err
	}
	h.userCount = len(users)
	hotuser.LogUpdate("hysteria2", from, len(users))
	return nil
}

func (h *Inbound) publishUsers(ids, passwords []string) error {
	if h.packetConn == nil {
		// 還沒 Start：沒有 ServeHTTP 讀 userMap，直接寫未上線的 Service。
		h.service.UpdateUsers(ids, passwords)
		return nil
	}
	return h.replaceService(ids, passwords)
}

// replaceService 先關舊 QUIC／ServeHTTP，再把名單寫進新 Service。
// 不准跟進線 handshake 對打同一張 userMap；UDP 埠由 stickyPacketConn 留著。
func (h *Inbound) replaceService(ids, passwords []string) error {
	svc, err := hysteria2.NewService[string](h.serviceOpts)
	if err != nil {
		return err
	}
	svc.UpdateUsers(ids, passwords)
	if old := h.service; old != nil {
		_ = old.Close()
	}
	if err := svc.Start(h.packetConn); err != nil {
		return err
	}
	h.service = svc
	return nil
}
