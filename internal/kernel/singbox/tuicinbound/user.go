package tuicinbound

import (
	"github.com/gofrs/uuid/v5"
	"github.com/jasonsamtago/Gboard-Node/internal/kernel/singbox/hotuser"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/exceptions"
)

func tuicUserLists(users []option.TUICUser) (ids []string, uuids [][16]byte, passwords []string, err error) {
	ids = make([]string, 0, len(users))
	uuids = make([][16]byte, 0, len(users))
	passwords = make([]string, 0, len(users))
	for i, user := range users {
		if user.UUID == "" {
			return nil, nil, nil, exceptions.New("missing uuid for user ", i)
		}
		userUUID, parseErr := uuid.FromString(user.UUID)
		if parseErr != nil {
			return nil, nil, nil, exceptions.Cause(parseErr, "invalid uuid for user ", i)
		}
		ids = append(ids, hotuser.StableID(user.Name, user.UUID))
		uuids = append(uuids, userUUID)
		passwords = append(passwords, user.Password)
	}
	return ids, uuids, passwords, nil
}

func (h *Inbound) UpdateUsers(users []option.TUICUser) error {
	from := h.userCount
	ids, userUUIDList, userPasswordList, err := tuicUserLists(users)
	if err != nil {
		return err
	}
	h.server.UpdateUsers(ids, userUUIDList, userPasswordList)
	h.userCount = len(users)
	hotuser.LogUpdate("tuic", from, len(users))
	return nil
}
