package unfurl

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/miere/murtaugh/internal/config"
)

// Rule is a compiled unfurl rule ready for matching.
type Rule struct {
	Name     string
	Config   config.UnfurlRuleConfig
	channels []string
	domain   string
	prefix   string
	pattern  *regexp.Regexp
}

// Match is the result of matching a shared link against a rule.
type Match struct {
	Rule     Rule
	Captures map[string]string
}

// Matcher evaluates shared links against an ordered set of compiled rules.
type Matcher struct {
	rules []Rule
}

// NewMatcher compiles the configured rules. Rules are evaluated in sorted-key
// order so "first match wins" is deterministic, mirroring the workflow engine.
func NewMatcher(rules map[string]config.UnfurlRuleConfig) (*Matcher, error) {
	names := make([]string, 0, len(rules))
	for name := range rules {
		names = append(names, name)
	}
	sort.Strings(names)

	compiled := make([]Rule, 0, len(names))
	for _, name := range names {
		raw := rules[name]
		rule := Rule{
			Name:     name,
			Config:   raw,
			channels: raw.Match.Channels,
			domain:   strings.ToLower(strings.TrimSpace(raw.Match.Domain)),
			prefix:   raw.Match.URLPrefix,
		}
		if pattern := strings.TrimSpace(raw.Match.URLPattern); pattern != "" {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return nil, fmt.Errorf("unfurl rule %q: compile url_pattern: %w", name, err)
			}
			rule.pattern = re
		}
		compiled = append(compiled, rule)
	}
	return &Matcher{rules: compiled}, nil
}

// Len reports the number of compiled rules.
func (m *Matcher) Len() int {
	if m == nil {
		return 0
	}
	return len(m.rules)
}

// Match returns the first rule that applies to the given link in the given
// channel, along with any named regex captures.
func (m *Matcher) Match(linkURL, domain, channel string) (Match, bool) {
	if m == nil {
		return Match{}, false
	}
	host := strings.ToLower(strings.TrimSpace(domain))
	if parsed, err := url.Parse(linkURL); err == nil && parsed.Hostname() != "" {
		host = strings.ToLower(parsed.Hostname())
	}
	for _, rule := range m.rules {
		if !rule.matchesChannel(channel) {
			continue
		}
		if !rule.matchesDomain(host) {
			continue
		}
		if rule.prefix != "" && !strings.HasPrefix(linkURL, rule.prefix) {
			continue
		}
		captures := map[string]string{}
		if rule.pattern != nil {
			groups := rule.pattern.FindStringSubmatch(linkURL)
			if groups == nil {
				continue
			}
			for i, name := range rule.pattern.SubexpNames() {
				if name != "" {
					captures[name] = groups[i]
				}
			}
		}
		return Match{Rule: rule, Captures: captures}, true
	}
	return Match{}, false
}

func (r Rule) matchesChannel(channel string) bool {
	if len(r.channels) == 0 {
		return true
	}
	for _, candidate := range r.channels {
		if strings.TrimSpace(candidate) == channel {
			return true
		}
	}
	return false
}

func (r Rule) matchesDomain(host string) bool {
	if r.domain == "" {
		return true
	}
	return host == r.domain || strings.HasSuffix(host, "."+r.domain)
}
