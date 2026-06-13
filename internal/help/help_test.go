package help

import (
	"strings"
	"testing"
)

func TestFullNonEmpty(t *testing.T) {
	full := Full()
	if !strings.Contains(full, "# murtaugh — command-line reference") {
		t.Fatalf("Full() missing document title; got %d bytes", len(full))
	}
}

func TestSectionLookup(t *testing.T) {
	cases := []struct {
		name string
		want string // header line the section must start with
	}{
		{"ping", "## murtaugh ping"},
		{"jobs run", "## murtaugh jobs run"},
		{"jobs.run", "## murtaugh jobs run"}, // dotted registry form
		{"slack send-msg", "## murtaugh slack send-msg"},
		{"slack.send-msg", "## murtaugh slack send-msg"},
		{"  Jobs   Define ", "## murtaugh jobs define"}, // whitespace/case tolerant
		{"setup mcp-register", "## murtaugh setup mcp-register"},
	}
	for _, tc := range cases {
		got, ok := Section(tc.name)
		if !ok {
			t.Errorf("Section(%q) not found", tc.name)
			continue
		}
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("Section(%q) = %q…, want prefix %q", tc.name, firstLine(got), tc.want)
		}
	}
}

// TestSectionDoesNotBleed ensures a section stops at the next command header.
func TestSectionDoesNotBleed(t *testing.T) {
	got, ok := Section("ping")
	if !ok {
		t.Fatal("Section(ping) not found")
	}
	if strings.Contains(got, "## murtaugh jobs run") {
		t.Errorf("Section(ping) bled into the next command section:\n%s", got)
	}
}

func TestSectionMissing(t *testing.T) {
	if _, ok := Section("does-not-exist"); ok {
		t.Error("Section(does-not-exist) reported found")
	}
	if _, ok := Section(""); ok {
		t.Error("Section(empty) reported found")
	}
}

func TestRender(t *testing.T) {
	if Render(nil) != Full() {
		t.Error("Render(nil) should return the full document")
	}
	if got := Render([]string{"slack", "send-msg"}); !strings.HasPrefix(got, "## murtaugh slack send-msg") {
		t.Errorf("Render([slack send-msg]) = %q…", firstLine(got))
	}
	if got := Render([]string{"nope"}); !strings.Contains(got, "No help section") {
		t.Error("Render of an unknown command should fall back with a notice")
	}
}

// TestEveryCommandDocumented guards against adding a CLI command without a
// matching help section. Keep this list in sync with the registered tools plus
// the built-in modes (gateway, mcp, version, help).
func TestEveryCommandDocumented(t *testing.T) {
	commands := []string{
		"ping",
		"jobs run", "jobs define",
		"journal query", "journal stats", "journal prune",
		"slack send-msg", "slack fetch-msgs", "slack fetch-reactions", "slack update-msg",
		"slack gateway",
		"mcp",
		"setup bootstrap", "setup slack", "setup agents",
		"setup mcp-register", "setup launchd", "setup update",
		"version", "help",
	}
	for _, c := range commands {
		if _, ok := Section(c); !ok {
			t.Errorf("no help section documents %q (add a `## murtaugh %s` block to cli-help.md)", c, c)
		}
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
