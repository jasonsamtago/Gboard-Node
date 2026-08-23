package panel

import (
	"fmt"
	"net/url"
	"strings"
)

// parseWSURL turns a handshake-issued ws_url into a gorilla-dialable URL.
//
// Official cedar2025/Xboard-Node #61: gorilla Dialer only accepts ws/wss.
// Panels commonly return http(s)://, a missing scheme, protocol-relative
// //host, and trailing slashes. Those must be rewritten here — not by
// disabling WebSocket or falling back to REST.
func parseWSURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty ws url")
	}

	// url.Parse("host:port/path") fails with
	// "first path segment in URL cannot contain colon".
	// Protocol-relative URLs parse with an empty scheme.
	switch {
	case strings.HasPrefix(raw, "//"):
		raw = "ws:" + raw
	case !strings.Contains(raw, "://"):
		raw = "ws://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "ws":
		u.Scheme = "ws"
	case "https", "wss":
		u.Scheme = "wss"
	default:
		return nil, fmt.Errorf("unsupported ws url scheme %q", u.Scheme)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("ws url missing host")
	}
	return u, nil
}
