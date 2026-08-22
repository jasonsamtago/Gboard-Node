package hotuser

import (
	"context"

	"github.com/sagernet/sing/common/auth"

	"github.com/jasonsamtago/Gboard-Node/internal/nlog"
)

// StableID returns the hot-update user key. Hy2/TUIC must store uuid/id in
// the connection ctx, never the array index.
func StableID(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

// FromContext reads the stable uuid/id. A leftover int index is ignored so
// a shorter or empty list cannot rebind traffic or panic.
func FromContext(ctx context.Context) string {
	if id, ok := auth.UserFromContext[string](ctx); ok && id != "" {
		return id
	}
	return ""
}

// LogUpdate records a hot-update that must not restart the kernel.
func LogUpdate(protocol string, from, to int) {
	nlog.Core().Info("hot-update users",
		"protocol", protocol,
		"path", "UpdateUsers",
		"from", from,
		"to", to,
		"restart", false,
	)
}
