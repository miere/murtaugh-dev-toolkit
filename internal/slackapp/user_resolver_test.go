package slackapp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/slack-go/slack"
)

type fakeUserDirectory struct {
	users    []slack.User
	calls    int
	getErr   error
}

func (f *fakeUserDirectory) GetUsersContext(context.Context, ...slack.GetUsersOption) ([]slack.User, error) {
	f.calls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.users, nil
}

func TestResolveUserIDsPassesThroughIDsWithoutFetch(t *testing.T) {
	api := &fakeUserDirectory{}
	ids, err := resolveUserIDs(context.Background(), api, []string{"U0ADMIN00", "W0SLACKOR"})
	if err != nil {
		t.Fatalf("resolveUserIDs returned error: %v", err)
	}
	if len(ids) != 2 || ids[0] != "U0ADMIN00" || ids[1] != "W0SLACKOR" {
		t.Fatalf("unexpected resolution: %#v", ids)
	}
	if api.calls != 0 {
		t.Fatalf("expected zero users.list calls when all entries are IDs, got %d", api.calls)
	}
}

func TestResolveUserIDsResolvesHandlesWithSingleFetch(t *testing.T) {
	api := &fakeUserDirectory{users: []slack.User{
		{ID: "UALICE00", Name: "alice"},
		{ID: "UBOB0000", Profile: slack.UserProfile{DisplayNameNormalized: "bob"}},
	}}
	ids, err := resolveUserIDs(context.Background(), api, []string{"@alice", "U0ADMIN00", "bob"})
	if err != nil {
		t.Fatalf("resolveUserIDs returned error: %v", err)
	}
	want := []string{"UALICE00", "U0ADMIN00", "UBOB0000"}
	for i, w := range want {
		if ids[i] != w {
			t.Fatalf("ids[%d] = %q, want %q", i, ids[i], w)
		}
	}
	if api.calls != 1 {
		t.Fatalf("expected exactly one users.list call, got %d", api.calls)
	}
}

func TestResolveUserIDsFailsClosedOnUnresolvableHandle(t *testing.T) {
	api := &fakeUserDirectory{users: []slack.User{{ID: "UALICE00", Name: "alice"}}}
	_, err := resolveUserIDs(context.Background(), api, []string{"@alice", "@ghost"})
	if err == nil {
		t.Fatal("expected error when handle cannot be resolved")
	}
	if !strings.Contains(err.Error(), `"@ghost"`) {
		t.Fatalf("expected error to identify the missing handle, got: %v", err)
	}
}

func TestResolveUserIDsPropagatesFetchError(t *testing.T) {
	boom := errors.New("rate limited")
	api := &fakeUserDirectory{getErr: boom}
	_, err := resolveUserIDs(context.Background(), api, []string{"@alice"})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("expected fetch error to propagate, got: %v", err)
	}
}

func TestResolveUserIDsRejectsBlankEntry(t *testing.T) {
	api := &fakeUserDirectory{}
	_, err := resolveUserIDs(context.Background(), api, []string{"U0ADMIN00", "   "})
	if err == nil {
		t.Fatal("expected blank entry to be rejected")
	}
	if api.calls != 0 {
		t.Fatalf("expected no API call on blank-entry failure, got %d", api.calls)
	}
}

func TestResolveUserIDsEmptyInputReturnsNil(t *testing.T) {
	api := &fakeUserDirectory{}
	ids, err := resolveUserIDs(context.Background(), api, nil)
	if err != nil {
		t.Fatalf("resolveUserIDs returned error: %v", err)
	}
	if ids != nil {
		t.Fatalf("expected nil result for empty input, got %#v", ids)
	}
	if api.calls != 0 {
		t.Fatalf("expected no API call for empty input, got %d", api.calls)
	}
}
