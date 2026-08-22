package panel

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePortOrRange 收下單端口或「start-end」區間。
// 區間必須起點 <= 終點，且落在 1–65535；不會只留起點。
func ParsePortOrRange(v any) (start, end int, raw string, err error) {
	switch x := v.(type) {
	case nil:
		return 0, 0, "", nil
	case int:
		return parseSinglePort(x)
	case int64:
		return parseSinglePort(int(x))
	case float64:
		return parseSinglePort(int(x))
	case jsonNumber:
		n, nerr := x.Int64()
		if nerr != nil {
			return 0, 0, "", fmt.Errorf("server_port: %w", nerr)
		}
		return parseSinglePort(int(n))
	case string:
		return parsePortOrRangeString(x)
	default:
		return 0, 0, "", fmt.Errorf("server_port: unsupported type %T", v)
	}
}

type jsonNumber interface {
	Int64() (int64, error)
}

func parseSinglePort(n int) (int, int, string, error) {
	if n == 0 {
		return 0, 0, "", nil
	}
	if n < 1 || n > 65535 {
		return 0, 0, "", fmt.Errorf("server_port %d out of range", n)
	}
	return n, n, strconv.Itoa(n), nil
}

func parsePortOrRangeString(s string) (int, int, string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, "", fmt.Errorf("server_port is empty")
	}
	if start, end, ok := splitPortRange(s); ok {
		if start < 1 || end > 65535 || start > end {
			return 0, 0, "", fmt.Errorf("server_port range %q is invalid", s)
		}
		return start, end, fmt.Sprintf("%d-%d", start, end), nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, 0, "", fmt.Errorf("server_port %q: %w", s, err)
	}
	return parseSinglePort(n)
}

func splitPortRange(s string) (int, int, bool) {
	a, b, ok := strings.Cut(s, "-")
	if !ok || strings.Contains(b, "-") {
		return 0, 0, false
	}
	start, err1 := strconv.Atoi(strings.TrimSpace(a))
	end, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return start, end, true
}

// normalizeServerPort 把面板下發的 server_port（數字或區間字串）拆成
// server_port=起點 與 server_port_range=完整區間。單端口不寫 range。
func normalizeServerPort(input map[string]interface{}) error {
	if input == nil {
		return nil
	}
	v, ok := input["server_port"]
	if !ok || v == nil {
		return nil
	}
	start, end, raw, err := ParsePortOrRange(v)
	if err != nil {
		return err
	}
	if start == 0 {
		return nil
	}
	input["server_port"] = start
	if end > start {
		input["server_port_range"] = raw
	} else {
		delete(input, "server_port_range")
	}
	return nil
}
