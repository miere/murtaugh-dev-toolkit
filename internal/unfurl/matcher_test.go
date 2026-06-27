package unfurl

import (
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func templateAction(path string) config.UnfurlActionConfig {
	return config.UnfurlActionConfig{Template: path}
}

func TestMatcherDomainSuffixAndCaptures(t *testing.T) {
	m, err := NewMatcher(map[string]config.UnfurlRuleConfig{
		"github-pr": {
			Match:  config.UnfurlMatchConfig{Domain: "github.com", URLPattern: `/pull/(?P<number>\d+)`},
			Unfurl: templateAction("templates/unfurl/github-pr.json"),
		},
	})
	if err != nil {
		t.Fatalf("NewMatcher returned error: %v", err)
	}
	match, ok := m.Match("https://gist.github.com/acme/widgets/pull/42", "gist.github.com", "C1")
	if !ok {
		t.Fatal("expected subdomain suffix match")
	}
	if match.Rule.Name != "github-pr" {
		t.Fatalf("unexpected rule: %q", match.Rule.Name)
	}
	if match.Captures["number"] != "42" {
		t.Fatalf("unexpected captures: %#v", match.Captures)
	}
}

func TestMatcherURLPrefix(t *testing.T) {
	m, _ := NewMatcher(map[string]config.UnfurlRuleConfig{
		"docs": {Match: config.UnfurlMatchConfig{URLPrefix: "https://docs.example.com/"}, Unfurl: templateAction("t.json")},
	})
	if _, ok := m.Match("https://docs.example.com/page", "docs.example.com", "C1"); !ok {
		t.Fatal("expected prefix match")
	}
	if _, ok := m.Match("https://other.example.com/page", "other.example.com", "C1"); ok {
		t.Fatal("expected no match for different prefix")
	}
}

func TestMatcherChannelAllowlist(t *testing.T) {
	m, _ := NewMatcher(map[string]config.UnfurlRuleConfig{
		"eng": {Match: config.UnfurlMatchConfig{Channels: []string{"C0ENG"}, Domain: "github.com"}, Unfurl: templateAction("t.json")},
	})
	if _, ok := m.Match("https://github.com/x", "github.com", "C0OTHER"); ok {
		t.Fatal("expected no match outside channel allowlist")
	}
	if _, ok := m.Match("https://github.com/x", "github.com", "C0ENG"); !ok {
		t.Fatal("expected match inside channel allowlist")
	}
}

func TestMatcherFirstMatchWinsBySortedKey(t *testing.T) {
	m, _ := NewMatcher(map[string]config.UnfurlRuleConfig{
		"bbb": {Match: config.UnfurlMatchConfig{Domain: "github.com"}, Unfurl: templateAction("b.json")},
		"aaa": {Match: config.UnfurlMatchConfig{Domain: "github.com"}, Unfurl: templateAction("a.json")},
	})
	match, ok := m.Match("https://github.com/x", "github.com", "C1")
	if !ok || match.Rule.Name != "aaa" {
		t.Fatalf("expected aaa to win, got %q ok=%v", match.Rule.Name, ok)
	}
}

func TestMatcherNoMatchWhenPatternFails(t *testing.T) {
	m, _ := NewMatcher(map[string]config.UnfurlRuleConfig{
		"pr": {Match: config.UnfurlMatchConfig{Domain: "github.com", URLPattern: `/pull/(?P<n>\d+)`}, Unfurl: templateAction("t.json")},
	})
	if _, ok := m.Match("https://github.com/acme/widgets/issues/9", "github.com", "C1"); ok {
		t.Fatal("expected no match for non-PR url")
	}
}

func TestNewMatcherRejectsBadPattern(t *testing.T) {
	_, err := NewMatcher(map[string]config.UnfurlRuleConfig{
		"bad": {Match: config.UnfurlMatchConfig{Domain: "github.com", URLPattern: "([a-z"}, Unfurl: templateAction("t.json")},
	})
	if err == nil {
		t.Fatal("expected compile error for bad pattern")
	}
}
