package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// Official cedar2025/Xboard-Node #63: decodeWeakRaw / encoding/json on
// []User fails the whole batch when one row's [0] / uuid is a map, and
// mapstructure dumps every row into one warn. Sync then repeats that dump.
//
// Rows are decoded one-by-one. A bad row is skipped; string-UUID rows stay.
// Malformed-row warnings are aggregated and de-duplicated (not per-row).

var (
	usersMalformedLogMu  sync.Mutex
	usersMalformedLogKey string
	usersMalformedLogAt  time.Time
)

func decodeUsersPayload(data []byte) (users []User, nodeID int, err error) {
	var env struct {
		Users  json.RawMessage `json:"users"`
		NodeID int             `json:"node_id"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, 0, err
	}
	users, skipped, err := decodeUserRows(env.Users)
	if err != nil {
		return nil, 0, err
	}
	logMalformedUserRows(skipped, len(users))
	return users, env.NodeID, nil
}

func decodeUsersDeltaPayload(data []byte) (users []User, nodeID int, action string, err error) {
	var env struct {
		Action string          `json:"action"`
		Users  json.RawMessage `json:"users"`
		NodeID int             `json:"node_id"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, 0, "", err
	}
	users, skipped, err := decodeUserRows(env.Users)
	if err != nil {
		return nil, 0, "", err
	}
	logMalformedUserRows(skipped, len(users))
	return users, env.NodeID, env.Action, nil
}

func decodeUserRows(raw json.RawMessage) ([]User, int, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, 0, nil
	}
	if raw[0] != '[' {
		return nil, 0, fmt.Errorf("users must be an array")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, 0, err
	}
	out := make([]User, 0, len(rows))
	skipped := 0
	for _, row := range rows {
		u, ok := decodeUserRow(row)
		if !ok {
			skipped++
			continue
		}
		out = append(out, u)
	}
	return out, skipped, nil
}

func decodeUserRow(raw json.RawMessage) (User, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return User{}, false
	}
	if raw[0] != '{' {
		// Tuple / array: official #63 shape users[i][0] is often a map.
		return User{}, false
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return User{}, false
	}
	uuidVal, ok := obj["uuid"]
	if !ok {
		return User{}, false
	}
	uuid, ok := uuidVal.(string)
	if !ok || strings.TrimSpace(uuid) == "" {
		return User{}, false
	}
	var u User
	if err := decodeWeakRaw(obj, &u); err != nil {
		return User{}, false
	}
	if u.ID <= 0 || strings.TrimSpace(u.UUID) == "" {
		return User{}, false
	}
	return u, true
}

func logMalformedUserRows(skipped, kept int) {
	if skipped <= 0 {
		return
	}
	key := fmt.Sprintf("%d/%d", skipped, kept)
	now := time.Now()

	usersMalformedLogMu.Lock()
	defer usersMalformedLogMu.Unlock()
	if usersMalformedLogKey == key && now.Sub(usersMalformedLogAt) < time.Minute {
		return
	}
	usersMalformedLogKey = key
	usersMalformedLogAt = now
	nlog.Core().Warn("ws: dropped malformed user rows", "dropped", skipped, "kept", kept)
}
