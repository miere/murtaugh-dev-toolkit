// Package updates reports whether a newer Murtaugh release is available on
// GitHub. It backs the App Home control panel's "Update" affordance: the
// running version is compared against the latest published release tag using
// semantic-version ordering.
//
// The check is deliberately conservative and failure-tolerant:
//
//   - A "dev" build (or any version that is not a valid semver) never reports
//     an update — silently overwriting a local checkout would surprise the
//     developer, mirroring the setup.update tool's dev guard.
//   - Results are cached for a TTL so the frequent App Home opens do not
//     exhaust GitHub's unauthenticated rate limit (60 requests/hour).
//   - Any network or parse failure yields "no update" (the panel falls back to
//     showing the version alone) rather than surfacing an error to the user.
package updates

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/mod/semver"
)

// defaultTTL is how long a successful latest-release lookup is reused before
// the next App Home open triggers a fresh fetch.
const defaultTTL = time.Hour

// HTTPGet performs a GET against url and returns the body. It mirrors the
// signature used by the setup.update tool so the composition root can share a
// single implementation.
type HTTPGet func(ctx context.Context, url string) ([]byte, error)

// Deps is the explicit dependency bundle passed to New.
type Deps struct {
	// Current is the running binary's version (e.g. "v0.9.1" or "dev").
	Current string
	// Owner and Repo identify the GitHub repository whose releases are polled.
	Owner, Repo string
	// HTTPGet fetches a URL; injected so tests can stub network access.
	HTTPGet HTTPGet
	// TTL overrides the cache lifetime; <= 0 selects defaultTTL.
	TTL time.Duration
	// Now supplies the current time; nil selects time.Now.
	Now func() time.Time
}

// Result is the outcome of a check.
type Result struct {
	// Current is the running version, echoed back for convenience.
	Current string
	// Latest is the most recent published release tag, or "" when unknown
	// (the lookup failed, or the current build is not a release).
	Latest string
	// Available is true when Latest is a strictly newer release than Current.
	Available bool
}

// Checker compares the running version against the latest GitHub release and
// caches the answer. Safe for concurrent use.
type Checker struct {
	current string
	owner   string
	repo    string
	httpGet HTTPGet
	ttl     time.Duration
	now     func() time.Time

	mu        sync.Mutex
	cached    Result
	fetchedAt time.Time
	hasCache  bool
}

// New constructs a Checker from the supplied dependencies.
func New(deps Deps) *Checker {
	ttl := deps.TTL
	if ttl <= 0 {
		ttl = defaultTTL
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Checker{
		current: strings.TrimSpace(deps.Current),
		owner:   deps.Owner,
		repo:    deps.Repo,
		httpGet: deps.HTTPGet,
		ttl:     ttl,
		now:     now,
	}
}

// Check reports whether a newer release than the running version exists. It
// returns a usable Result in every case; the error is informational (the most
// recent good cache, or a bare "no update" Result, is still returned alongside
// it) so callers can log without branching their render path.
func (c *Checker) Check(ctx context.Context) (Result, error) {
	// A non-release build (notably "dev") can never be "behind" a tagged
	// release in a meaningful way, so short-circuit before any network I/O.
	if canonical(c.current) == "" {
		return Result{Current: c.current}, nil
	}
	if c.httpGet == nil {
		return Result{Current: c.current}, nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hasCache && c.now().Sub(c.fetchedAt) < c.ttl {
		return c.cached, nil
	}

	tag, err := c.fetchLatest(ctx)
	if err != nil {
		// Fall back to the last good answer if we have one; otherwise report
		// no update so the panel still renders the version cleanly.
		if c.hasCache {
			return c.cached, err
		}
		return Result{Current: c.current}, err
	}

	res := Result{
		Current:   c.current,
		Latest:    tag,
		Available: isNewer(c.current, tag),
	}
	c.cached = res
	c.fetchedAt = c.now()
	c.hasCache = true
	return res, nil
}

// ReleaseURL returns the human-facing GitHub release page for a tag, suitable
// for linking from the confirmation modal.
func (c *Checker) ReleaseURL(tag string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return fmt.Sprintf("https://github.com/%s/%s/releases/latest", c.owner, c.repo)
	}
	return fmt.Sprintf("https://github.com/%s/%s/releases/tag/%s", c.owner, c.repo, tag)
}

// fetchLatest GETs the repository's latest-release JSON and returns its
// tag_name.
func (c *Checker) fetchLatest(ctx context.Context) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", c.owner, c.repo)
	body, err := c.httpGet(ctx, url)
	if err != nil {
		return "", fmt.Errorf("fetch latest release: %w", err)
	}
	var doc struct {
		TagName string `json:"tag_name"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("parse release JSON: %w", err)
	}
	tag := strings.TrimSpace(doc.TagName)
	if tag == "" {
		return "", fmt.Errorf("release JSON has no tag_name")
	}
	return tag, nil
}

// canonical normalizes a version tag into a form acceptable to
// golang.org/x/mod/semver, which requires a leading "v". It returns "" when the
// value is not a valid semantic version (e.g. "dev" or an empty string).
func canonical(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return ""
	}
	return v
}

// isNewer reports whether latest is a strictly greater release than current.
// Either side failing to parse yields false (fail-closed: no update offered).
func isNewer(current, latest string) bool {
	cc, cl := canonical(current), canonical(latest)
	if cc == "" || cl == "" {
		return false
	}
	return semver.Compare(cl, cc) > 0
}
