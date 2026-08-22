package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// decodeDevicesPayload parses panel sync.devices data.
//
// Official cedar2025/Xboard-Node #29 / #40: decodeWeakRaw treats
// users[id][0] as map[string]interface{} (object IP / PHP holey array)
// and handleDataEvent discarded the whole payload, so device_limit
// could not see cross-node devices.
//
// String IPs, {ip:...} objects, and holey {"0":"1.2.3.4"} maps are accepted.
// A single user that fails to decode is skipped; other users still apply.
func decodeDevicesPayload(data []byte) (map[int][]string, int, error) {
	var env struct {
		Users  json.RawMessage `json:"users"`
		NodeID int             `json:"node_id"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, 0, err
	}

	usersRaw := bytes.TrimSpace(env.Users)
	if len(usersRaw) == 0 || bytes.Equal(usersRaw, []byte("null")) {
		return nil, 0, fmt.Errorf("devices payload missing users")
	}
	if usersRaw[0] == '[' {
		return nil, 0, fmt.Errorf("devices payload users must be a map")
	}

	var userMap map[string]json.RawMessage
	if err := json.Unmarshal(usersRaw, &userMap); err != nil {
		return nil, 0, err
	}

	users := make(map[int][]string, len(userMap))
	for idStr, rawIPs := range userMap {
		uid, err := strconv.Atoi(idStr)
		if err != nil {
			nlog.Core().Warn("ws: skip devices user", "user", idStr, "error", err)
			continue
		}
		ips, err := decodeUserDeviceIPs(rawIPs)
		if err != nil {
			nlog.Core().Warn("ws: skip devices user", "user", uid, "error", err)
			continue
		}
		users[uid] = ips
		nlog.Core().Info("ws: decoded devices user", "user", uid, "ips", ips)
	}
	return users, env.NodeID, nil
}

func decodeUserDeviceIPs(raw json.RawMessage) ([]string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, fmt.Errorf("empty devices list")
	}

	switch raw[0] {
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return nil, err
		}
		ips, err := collectDeviceIPs(elems)
		if err != nil {
			return nil, err
		}
		return ips, nil
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return nil, err
		}
		if ipRaw, ok := objectIPField(obj); ok {
			ip, err := decodeDeviceIPElement(ipRaw)
			if err != nil {
				return nil, err
			}
			return []string{ip}, nil
		}
		elems := make([]json.RawMessage, 0, len(obj))
		for _, v := range obj {
			elems = append(elems, v)
		}
		return collectDeviceIPs(elems)
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("empty ip")
		}
		return []string{s}, nil
	default:
		return nil, fmt.Errorf("unsupported devices value")
	}
}

func collectDeviceIPs(elems []json.RawMessage) ([]string, error) {
	var ips []string
	var lastErr error
	seen := make(map[string]struct{}, len(elems))
	for _, el := range elems {
		ip, err := decodeDeviceIPElement(el)
		if err != nil {
			lastErr = err
			continue
		}
		if _, ok := seen[ip]; ok {
			continue
		}
		seen[ip] = struct{}{}
		ips = append(ips, ip)
	}
	if len(ips) == 0 {
		if lastErr != nil {
			return nil, lastErr
		}
		return nil, fmt.Errorf("no ips")
	}
	return ips, nil
}

func decodeDeviceIPElement(raw json.RawMessage) (string, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", fmt.Errorf("empty ip")
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return "", fmt.Errorf("empty ip")
		}
		return s, nil
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			return "", err
		}
		ipRaw, ok := objectIPField(obj)
		if !ok {
			return "", fmt.Errorf("object missing ip")
		}
		return decodeDeviceIPElement(ipRaw)
	default:
		return "", fmt.Errorf("unsupported ip element")
	}
}

func objectIPField(obj map[string]json.RawMessage) (json.RawMessage, bool) {
	for _, key := range []string{"ip", "IP", "addr", "address"} {
		if v, ok := obj[key]; ok {
			return v, true
		}
	}
	return nil, false
}
