package gateway

import (
	"path"
	"strings"
)

// matchChannelAgent resolves a channel to its configured agent from the
// chat.channel_agents map. The map's keys are either exact Slack channel IDs
// (C…/G…, for back-compat) or channel-NAME globs that may contain `*`
// (e.g. "feature-*", "*-prod"). channelName may be empty when the cache has
// not yet learned the channel's name (a brand-new channel); only the exact
// channel-ID rule can match in that case.
//
// Precedence (first hit wins, implemented by scoring rather than map
// iteration order because Go maps are unordered):
//
//  1. exact channel-ID key  — an entry that equals channelID verbatim.
//  2. exact channel-name key — an entry that equals channelName verbatim.
//  3. longest-literal-prefix glob match on the name — among the glob keys
//     that match channelName, the one whose literal prefix (the run of
//     characters before its first `*`) is longest wins, so a more specific
//     pattern like "feature-prod-*" beats a broader "feature-*".
//
// ok is false (and agent is "") when nothing matches; callers fall back to
// the default agent in that case. The function is pure — it does no I/O — so
// it is safe to call on the Slack socket goroutine and reusable wherever a
// channel→agent decision is needed.
func matchChannelAgent(channelID, channelName string, patterns map[string]string) (agent string, ok bool) {
	if len(patterns) == 0 {
		return "", false
	}
	// (1) exact channel-ID key.
	if channelID != "" {
		if a, hit := patterns[channelID]; hit {
			return a, true
		}
	}
	if channelName == "" {
		return "", false
	}
	// (2) exact channel-name key.
	if a, hit := patterns[channelName]; hit {
		return a, true
	}
	// (3) longest-literal-prefix glob match on the name.
	bestAgent := ""
	bestPrefixLen := -1
	bestFound := false
	for key, a := range patterns {
		if !strings.ContainsRune(key, '*') {
			// Non-glob keys were already handled by the exact passes above.
			continue
		}
		if matched, err := path.Match(key, channelName); err != nil || !matched {
			continue
		}
		if n := literalPrefixLen(key); n > bestPrefixLen {
			bestPrefixLen = n
			bestAgent = a
			bestFound = true
		}
	}
	if bestFound {
		return bestAgent, true
	}
	return "", false
}

// literalPrefixLen returns the number of leading characters in pattern before
// its first `*` (or the whole length when there is no `*`). It is the measure
// of how specific a glob is: a longer literal prefix matches a narrower set of
// names, so it wins the precedence-3 tie-break.
func literalPrefixLen(pattern string) int {
	if i := strings.IndexRune(pattern, '*'); i >= 0 {
		return i
	}
	return len(pattern)
}

// validChannelAgentGlob reports whether key is a syntactically valid
// channel_agents key. Exact channel-ID/name keys are always valid; glob keys
// (those containing `*`) must be accepted by path.Match so a malformed pattern
// is rejected at config-load time rather than silently never matching.
func validChannelAgentGlob(key string) bool {
	if !strings.ContainsRune(key, '*') {
		return true
	}
	// path.Match only reports ErrBadPattern for malformed patterns; matching
	// against a fixed probe string is enough to surface that error.
	_, err := path.Match(key, "probe")
	return err == nil
}
