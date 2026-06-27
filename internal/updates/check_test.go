package updates

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func releaseJSON(tag string) []byte {
	return []byte(fmt.Sprintf(`{"tag_name":%q}`, tag))
}

func TestCheck_ReportsNewerRelease(t *testing.T) {
	c := New(Deps{
		Current: "v0.9.1",
		Owner:   "miere",
		Repo:    "murtaugh",
		HTTPGet: func(context.Context, string) ([]byte, error) { return releaseJSON("v0.9.4"), nil },
	})
	res, err := c.Check(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Available || res.Latest != "v0.9.4" {
		t.Fatalf("expected update to v0.9.4, got %+v", res)
	}
}

func TestCheck_EqualVersionIsNotAnUpdate(t *testing.T) {
	c := New(Deps{
		Current: "v0.9.4",
		HTTPGet: func(context.Context, string) ([]byte, error) { return releaseJSON("v0.9.4"), nil },
	})
	res, _ := c.Check(context.Background())
	if res.Available {
		t.Fatalf("equal versions must not report an update: %+v", res)
	}
}

func TestCheck_OlderRemoteIsNotAnUpdate(t *testing.T) {
	c := New(Deps{
		Current: "v1.2.0",
		HTTPGet: func(context.Context, string) ([]byte, error) { return releaseJSON("v1.1.9"), nil },
	})
	res, _ := c.Check(context.Background())
	if res.Available {
		t.Fatalf("older remote must not report an update: %+v", res)
	}
}

func TestCheck_PrefixTolerantComparison(t *testing.T) {
	// Current carries no "v"; remote does. They must still compare.
	c := New(Deps{
		Current: "0.9.1",
		HTTPGet: func(context.Context, string) ([]byte, error) { return releaseJSON("v0.9.2"), nil },
	})
	res, _ := c.Check(context.Background())
	if !res.Available {
		t.Fatalf("expected update despite mixed v-prefix: %+v", res)
	}
}

func TestCheck_DevBuildNeverChecks(t *testing.T) {
	var calls int32
	c := New(Deps{
		Current: "dev",
		HTTPGet: func(context.Context, string) ([]byte, error) {
			atomic.AddInt32(&calls, 1)
			return releaseJSON("v9.9.9"), nil
		},
	})
	res, err := c.Check(context.Background())
	if err != nil {
		t.Fatalf("dev check should not error: %v", err)
	}
	if res.Available {
		t.Fatalf("dev build must never report an update: %+v", res)
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("dev build must not hit the network, got %d calls", got)
	}
}

func TestCheck_FetchErrorIsTolerated(t *testing.T) {
	c := New(Deps{
		Current: "v0.9.1",
		HTTPGet: func(context.Context, string) ([]byte, error) { return nil, errors.New("boom") },
	})
	res, err := c.Check(context.Background())
	if err == nil {
		t.Fatalf("expected the underlying fetch error to surface")
	}
	if res.Available || res.Latest != "" {
		t.Fatalf("a failed fetch must render as no-update: %+v", res)
	}
}

func TestCheck_MalformedJSONIsTolerated(t *testing.T) {
	c := New(Deps{
		Current: "v0.9.1",
		HTTPGet: func(context.Context, string) ([]byte, error) { return []byte("not json"), nil },
	})
	res, err := c.Check(context.Background())
	if err == nil {
		t.Fatalf("expected a parse error")
	}
	if res.Available {
		t.Fatalf("malformed JSON must not report an update: %+v", res)
	}
}

func TestCheck_CachesWithinTTL(t *testing.T) {
	var calls int32
	now := time.Unix(1_700_000_000, 0)
	c := New(Deps{
		Current: "v0.9.1",
		TTL:     time.Hour,
		Now:     func() time.Time { return now },
		HTTPGet: func(context.Context, string) ([]byte, error) {
			atomic.AddInt32(&calls, 1)
			return releaseJSON("v0.9.4"), nil
		},
	})
	for i := 0; i < 3; i++ {
		if _, err := c.Check(context.Background()); err != nil {
			t.Fatalf("check %d errored: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("expected a single fetch within TTL, got %d", got)
	}
	// Advance beyond the TTL: the next check refetches.
	now = now.Add(2 * time.Hour)
	if _, err := c.Check(context.Background()); err != nil {
		t.Fatalf("post-TTL check errored: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("expected a refetch after TTL, got %d calls", got)
	}
}

func TestCheck_ServesStaleCacheOnError(t *testing.T) {
	var calls int32
	now := time.Unix(1_700_000_000, 0)
	c := New(Deps{
		Current: "v0.9.1",
		TTL:     time.Minute,
		Now:     func() time.Time { return now },
		HTTPGet: func(context.Context, string) ([]byte, error) {
			if atomic.AddInt32(&calls, 1) == 1 {
				return releaseJSON("v0.9.4"), nil
			}
			return nil, errors.New("transient")
		},
	})
	if res, _ := c.Check(context.Background()); !res.Available {
		t.Fatalf("first check should report the update")
	}
	now = now.Add(2 * time.Minute) // expire the cache
	res, err := c.Check(context.Background())
	if err == nil {
		t.Fatalf("expected the transient error to surface")
	}
	if !res.Available || res.Latest != "v0.9.4" {
		t.Fatalf("expected the last good cache to be served on error: %+v", res)
	}
}

func TestReleaseURL(t *testing.T) {
	c := New(Deps{Owner: "miere", Repo: "murtaugh"})
	if got, want := c.ReleaseURL("v0.9.4"), "https://github.com/miere/murtaugh/releases/tag/v0.9.4"; got != want {
		t.Fatalf("ReleaseURL(tag) = %q, want %q", got, want)
	}
	if got, want := c.ReleaseURL(""), "https://github.com/miere/murtaugh/releases/latest"; got != want {
		t.Fatalf("ReleaseURL(\"\") = %q, want %q", got, want)
	}
}
