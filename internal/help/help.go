// Package help surfaces the canonical CLI/MCP command reference that ships in
// assets/cli-help.md. The document is the single source of truth for what
// every murtaugh command does and which flags it takes; `murtaugh help` prints
// it, and CLI usage errors point at it. Keeping the prose in one embedded file
// (rather than scattered across flag definitions) means a tool change is
// documented in exactly one place.
package help

import (
	"strings"

	"github.com/miere/murtaugh/assets"
)

// fileName is the embedded reference document.
const fileName = "cli-help.md"

// commandPrefix marks a per-command section header in cli-help.md. Everything
// from such a header up to the next header (any `## ` or `# ` line) is that
// command's help block.
const commandPrefix = "## murtaugh "

// Full returns the entire CLI reference document.
func Full() string {
	b, err := assets.FS.ReadFile(fileName)
	if err != nil {
		// The file is embedded at build time; a read error means the binary
		// was assembled wrong. Degrade to an empty string rather than panic.
		return ""
	}
	return string(b)
}

// Section returns the help block for a single command, keyed by its CLI
// invocation ("jobs run", "slack send-msg", "ping"). The dotted registry form
// ("jobs.run", "slack.send-msg") is accepted too. The boolean reports whether
// a matching section was found.
func Section(command string) (string, bool) {
	key := normalizeKey(command)
	if key == "" {
		return "", false
	}

	lines := strings.Split(Full(), "\n")
	start := -1
	for i, ln := range lines {
		if !strings.HasPrefix(ln, commandPrefix) {
			continue
		}
		if normalizeKey(strings.TrimPrefix(ln, commandPrefix)) == key {
			start = i
			break
		}
	}
	if start < 0 {
		return "", false
	}

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if ln := lines[i]; strings.HasPrefix(ln, "## ") || strings.HasPrefix(ln, "# ") {
			end = i
			break
		}
	}
	return strings.TrimRight(strings.Join(lines[start:end], "\n"), "\n") + "\n", true
}

// Render resolves the help text for the given command tokens. With no tokens
// it returns the full document. When the tokens name a known command its
// section is returned; otherwise the full document is returned behind a short
// notice so callers still get something useful.
func Render(tokens []string) string {
	key := normalizeKey(strings.Join(tokens, " "))
	if key == "" {
		return Full()
	}
	if sec, ok := Section(key); ok {
		return sec
	}
	return "No help section for \"" + key + "\". Showing the full reference.\n\n" + Full()
}

// normalizeKey lower-cases the input, treats the dotted registry form as the
// spaced CLI form, and collapses runs of whitespace so "jobs.run",
// "jobs run", and "  Jobs   Run " all resolve to the same key.
func normalizeKey(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, ".", " ")
	return strings.Join(strings.Fields(s), " ")
}
