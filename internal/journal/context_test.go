package journal

import (
	"context"
	"strings"
	"testing"
)

func TestNewCorrIDUniqueAndPrefixed(t *testing.T) {
	a := NewCorrID("gw")
	b := NewCorrID("gw")
	if a == b {
		t.Fatalf("expected distinct corr ids, got %q twice", a)
	}
	for _, id := range []string{a, b} {
		if !strings.HasPrefix(id, "gw_") {
			t.Fatalf("corr id %q missing prefix", id)
		}
		if len(id) != len("gw_")+32 { // 16 bytes hex
			t.Fatalf("corr id %q has unexpected length %d", id, len(id))
		}
	}
}

func TestCorrIDContextRoundTrip(t *testing.T) {
	ctx := WithCorrID(context.Background(), "gw_abc")
	if got := CorrIDFromContext(ctx); got != "gw_abc" {
		t.Fatalf("CorrIDFromContext = %q, want gw_abc", got)
	}
}

func TestCorrIDContextAbsentOrBlank(t *testing.T) {
	if got := CorrIDFromContext(context.Background()); got != "" {
		t.Fatalf("expected empty corr id, got %q", got)
	}
	// WithCorrID with a blank id is a no-op rather than storing "".
	ctx := WithCorrID(context.Background(), "")
	if got := CorrIDFromContext(ctx); got != "" {
		t.Fatalf("expected empty corr id for blank input, got %q", got)
	}
}
