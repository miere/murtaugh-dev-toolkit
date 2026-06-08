package slackapp

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
)

// userDirectoryAPI is the minimal Slack surface needed to look up users by
// handle or display name. *slack.Client satisfies it.
type userDirectoryAPI interface {
	GetUsersContext(context.Context, ...slack.GetUsersOption) ([]slack.User, error)
}

// resolveUserIDs maps a list of Slack user references (IDs and/or handles) to
// Slack user IDs. Entries that already look like IDs are passed through
// untouched; otherwise the function performs at most one users.list lookup
// and matches handles against name, username, and display name (case
// insensitive). A leading "@" is stripped from each entry.
//
// The result preserves input order. An entry that cannot be resolved aborts
// the call with an error (fail-closed). Blank entries are rejected as input
// errors by config validation, so they are treated as fatal here too.
func resolveUserIDs(ctx context.Context, api userDirectoryAPI, refs []string) ([]string, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	resolved := make([]string, len(refs))
	handles := make([]string, len(refs))
	needFetch := false
	for i, raw := range refs {
		handle := strings.TrimPrefix(strings.TrimSpace(raw), "@")
		if handle == "" {
			return nil, fmt.Errorf("user reference at index %d is blank", i)
		}
		handles[i] = handle
		if looksLikeUserID(handle) {
			resolved[i] = handle
			continue
		}
		needFetch = true
	}
	if !needFetch {
		return resolved, nil
	}
	users, err := api.GetUsersContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Slack users: %w", err)
	}
	for i, handle := range handles {
		if resolved[i] != "" {
			continue
		}
		for _, user := range users {
			if user.Deleted || user.IsBot {
				continue
			}
			if slackUserMatchesHandle(user, handle) {
				resolved[i] = user.ID
				break
			}
		}
		if resolved[i] == "" {
			return nil, fmt.Errorf("Slack user %q was not found", refs[i])
		}
	}
	return resolved, nil
}
