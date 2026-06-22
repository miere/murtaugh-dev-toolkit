package terminal

import (
	"path/filepath"
	"strings"
)

// This file implements the allowlist-safe classification behind RequiresApproval:
// a command auto-runs only when it provably matches a known read-only shape, and
// anything else asks for approval (fail closed). The allowlist alone is not
// enough — a "safe" command can still mutate via redirection (`grep x > f`),
// chaining (`ls; rm x`), or substitution (`cat $(rm x)`) — so a structural guard
// rejects those shell features before the per-command check.

// alwaysSafe are commands that only read, regardless of their arguments.
var alwaysSafe = map[string]bool{
	"ls": true, "pwd": true, "cat": true, "head": true, "tail": true, "wc": true,
	"stat": true, "file": true, "tree": true, "realpath": true, "basename": true,
	"dirname": true, "du": true, "df": true, "echo": true, "printenv": true,
	"whoami": true, "id": true, "uname": true, "hostname": true, "date": true,
	"uptime": true, "ps": true, "which": true, "type": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	"sort": true, "uniq": true, "cut": true, "tr": true, "column": true,
	"diff": true, "comm": true, "jq": true, "yq": true, "less": true, "more": true,
	"cksum": true, "shasum": true, "md5sum": true, "sha256sum": true,
}

// subcommandSafe are read-only "binary subcommand" pairs. The binary alone is
// not safe (it has mutating subcommands); only these exact pairs are.
var subcommandSafe = map[string]bool{
	"git status": true, "git log": true, "git diff": true, "git show": true,
	"git blame": true, "git describe": true, "git rev-parse": true,
	"git ls-files": true, "git ls-remote": true, "git cat-file": true,
	"git shortlog": true, "git grep": true,
	"go env": true, "go version": true, "go list": true, "go doc": true,
}

// subcommandBins are binaries whose safety depends on the subcommand (the first
// non-flag word after the binary). They never match as a bare argv0.
var subcommandBins = map[string]bool{"git": true, "go": true}

// dangerousFindFlags make `find` mutate or execute, so a find carrying any of
// them is not read-only. `find` with none of them is treated as read-only.
var dangerousFindFlags = map[string]bool{
	"-delete": true, "-exec": true, "-execdir": true, "-ok": true,
	"-okdir": true, "-fprint": true, "-fprintf": true, "-fls": true,
}

// isReadOnly reports whether command is a recognized read-only command (and so
// may auto-run without approval). extraAllow adds caller-configured keys (an
// argv0 like "kubectl", or a "binary subcommand" pair like "docker ps").
func isReadOnly(command string, extraAllow []string) bool {
	command = strings.TrimSpace(command)
	if command == "" {
		return false
	}
	if hasShellControl(command) {
		return false
	}
	extra := allowSet(extraAllow)
	segments := splitPipes(command)
	for _, seg := range segments {
		words := dropEnvAssignments(tokenize(seg))
		if len(words) == 0 {
			return false // empty segment (e.g. from `a || b`) — not safe
		}
		if !segmentSafe(words, extra) {
			return false
		}
	}
	return true
}

// segmentSafe reports whether a single pipeline segment's command is read-only.
func segmentSafe(words []string, extra map[string]bool) bool {
	cmd := filepath.Base(words[0])
	if cmd == "find" {
		return !hasDangerousFind(words)
	}
	sub := firstNonFlag(words[1:])
	twoWord := cmd + " " + sub // meaningful only when sub != ""

	// A binary whose safety depends on its subcommand (git, go) is safe only for
	// a known-read-only pair — unless the user allowlisted the whole binary.
	if subcommandBins[cmd] {
		if extra[cmd] {
			return true
		}
		return sub != "" && (subcommandSafe[twoWord] || extra[twoWord])
	}

	// Otherwise: a built-in read-only argv0, a user-allowed argv0, or a
	// user-allowed "binary subcommand" pair (e.g. "docker ps").
	if alwaysSafe[cmd] || extra[cmd] {
		return true
	}
	return sub != "" && extra[twoWord]
}

// hasShellControl reports whether command uses shell features that could escape
// the allowlist's intent: redirection (`>`), command separators (`;`, `&`,
// newline), subshells (`(`, `)`), or command substitution (backtick, `$(`). It
// is quote- and backslash-aware, so a metacharacter inside a quoted string or
// escaped is not counted. The pipe `|` is allowed and handled by splitPipes.
// Inside double quotes only substitution stays active (backtick, `$(`); the
// other operators are literal there. Inside single quotes nothing is active.
func hasShellControl(command string) bool {
	var inSingle, inDouble bool
	rs := []rune(command)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && !inSingle {
			i++ // escape: skip the next rune
			continue
		}
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			switch {
			case c == '"':
				inDouble = false
			case c == '`':
				return true
			case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
				return true
			}
		case c == '\'':
			inSingle = true
		case c == '"':
			inDouble = true
		case c == ';' || c == '&' || c == '`' || c == '>' || c == '(' || c == ')' || c == '\n':
			return true
		case c == '$' && i+1 < len(rs) && rs[i+1] == '(':
			return true
		}
	}
	return false
}

// splitPipes splits command on top-level pipes, quote- and backslash-aware.
func splitPipes(command string) []string {
	var segs []string
	var cur []rune
	var inSingle, inDouble bool
	rs := []rune(command)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && !inSingle {
			cur = append(cur, c)
			if i+1 < len(rs) {
				cur = append(cur, rs[i+1])
				i++
			}
			continue
		}
		switch {
		case inSingle:
			cur = append(cur, c)
			if c == '\'' {
				inSingle = false
			}
		case inDouble:
			cur = append(cur, c)
			if c == '"' {
				inDouble = false
			}
		case c == '\'':
			inSingle = true
			cur = append(cur, c)
		case c == '"':
			inDouble = true
			cur = append(cur, c)
		case c == '|':
			segs = append(segs, string(cur))
			cur = nil
		default:
			cur = append(cur, c)
		}
	}
	return append(segs, string(cur))
}

// tokenize splits a segment into words, quote- and backslash-aware, with quotes
// removed from the resulting tokens.
func tokenize(seg string) []string {
	var words []string
	var cur []rune
	var inSingle, inDouble, has bool
	flush := func() {
		if has {
			words = append(words, string(cur))
			cur = nil
			has = false
		}
	}
	rs := []rune(seg)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c == '\\' && !inSingle {
			if i+1 < len(rs) {
				cur = append(cur, rs[i+1])
				has = true
				i++
			}
			continue
		}
		switch {
		case inSingle:
			if c == '\'' {
				inSingle = false
			} else {
				cur = append(cur, c)
				has = true
			}
		case inDouble:
			if c == '"' {
				inDouble = false
			} else {
				cur = append(cur, c)
				has = true
			}
		case c == '\'':
			inSingle = true
			has = true
		case c == '"':
			inDouble = true
			has = true
		case c == ' ' || c == '\t':
			flush()
		default:
			cur = append(cur, c)
			has = true
		}
	}
	flush()
	return words
}

// dropEnvAssignments strips leading VAR=value tokens (an inline environment
// prefix) so "FOO=bar ls" classifies on "ls".
func dropEnvAssignments(words []string) []string {
	i := 0
	for i < len(words) && isEnvAssignment(words[i]) {
		i++
	}
	return words[i:]
}

func isEnvAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for j, r := range w[:eq] {
		switch {
		case r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'):
		case j > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

// firstNonFlag returns the first word that is not an option (does not start with
// "-"), or "" if there is none.
func firstNonFlag(words []string) string {
	for _, w := range words {
		if !strings.HasPrefix(w, "-") {
			return w
		}
	}
	return ""
}

func hasDangerousFind(words []string) bool {
	for _, w := range words {
		if dangerousFindFlags[w] {
			return true
		}
	}
	return false
}

func allowSet(extra []string) map[string]bool {
	if len(extra) == 0 {
		return nil
	}
	m := make(map[string]bool, len(extra))
	for _, e := range extra {
		if e = strings.TrimSpace(e); e != "" {
			m[e] = true
		}
	}
	return m
}
