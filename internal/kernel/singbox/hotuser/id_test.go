package hotuser

import (
	"context"
	"testing"

	"github.com/sagernet/sing/common/auth"
)

func TestStableIDPrefersNameThenFallback(t *testing.T) {
	if got := StableID("uuid-1", "pw"); got != "uuid-1" {
		t.Fatalf("StableID = %q, want uuid-1", got)
	}
	if got := StableID("", "pw"); got != "pw" {
		t.Fatalf("StableID empty name = %q, want pw", got)
	}
}

func TestFromContextUsesUUIDNotIndex(t *testing.T) {
	ctx := auth.ContextWithUser(context.Background(), "44444444-4444-4444-4444-444444444444")
	if got := FromContext(ctx); got != "44444444-4444-4444-4444-444444444444" {
		t.Fatalf("FromContext uuid = %q", got)
	}
	if got := FromContext(auth.ContextWithUser(context.Background(), 3)); got != "" {
		t.Fatalf("FromContext leftover index = %q, want empty (no rebind)", got)
	}
	if got := FromContext(context.Background()); got != "" {
		t.Fatalf("FromContext empty = %q", got)
	}
}
