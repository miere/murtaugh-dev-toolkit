package terminal

import "testing"

func TestRequiresApproval_Allowlist(t *testing.T) {
	tool := NewWithApproval(t.TempDir(), ApprovalPolicy{Mode: ApprovalAllowlist})

	cases := []struct {
		command string
		want    bool // true = requires approval
	}{
		// Read-only: auto-run (no approval).
		{"ls -la", false},
		{"pwd", false},
		{"cat file.txt", false},
		{"grep -r foo .", false},
		{"cat a.txt | wc -l", false},
		{"ls | grep foo | wc -l", false},
		{"git status", false},
		{"git log --oneline -5", false},
		{"git diff HEAD~1", false},
		{"go env GOPATH", false},
		{"go version", false},
		{"find . -name '*.go'", false},
		{"echo $HOME", false},
		{"printenv PATH", false},
		{"FOO=bar ls", false},
		{"/bin/ls", false},
		{"grep 'a>b' file", false}, // > is quoted
		{`echo "a;b"`, false},      // ; is quoted

		// Side-effecting / unknown: require approval (fail closed).
		{"rm -rf x", true},
		{"go build ./...", true},
		{"go test ./...", true},
		{"git push", true},
		{"git commit -m x", true},
		{"git branch -D foo", true},
		{"cargo install ripgrep", true},
		{"npm install", true},
		{"brew install x", true},
		{"echo hi > out.txt", true}, // redirection
		{"ls; rm x", true},          // separator
		{"ls && rm x", true},        // &&
		{"ls || rm x", true},        // ||
		{"cat $(which ls)", true},   // command substitution
		{"echo `whoami`", true},     // backtick
		{`echo "$(rm x)"`, true},    // substitution inside double quotes
		{"find . -delete", true},    // dangerous find flag
		{"find . -exec rm {} ;", true},
		{"sed -i s/a/b/ f", true}, // sed not allowlisted
		{"awk '{print}' f", true}, // awk not allowlisted
		{"ls | xargs rm", true},   // xargs segment not allowlisted
		{"sudo ls", true},         // sudo not allowlisted
		{"kubectl get pods", true},
		{"", true},    // empty → fail closed
		{"   ", true}, // whitespace → fail closed
	}
	for _, tc := range cases {
		got := tool.RequiresApproval(map[string]any{"command": tc.command})
		if got != tc.want {
			t.Errorf("RequiresApproval(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestRequiresApproval_ExtraAllow(t *testing.T) {
	tool := NewWithApproval(t.TempDir(), ApprovalPolicy{
		Mode:  ApprovalAllowlist,
		Allow: []string{"kubectl", "docker ps"},
	})
	cases := []struct {
		command string
		want    bool
	}{
		{"kubectl get pods", false}, // argv0 allowlisted
		{"docker ps", false},        // binary-subcommand pair allowlisted
		{"docker run x", true},      // a different docker subcommand is still gated
		{"helm install x", true},    // not allowlisted
	}
	for _, tc := range cases {
		got := tool.RequiresApproval(map[string]any{"command": tc.command})
		if got != tc.want {
			t.Errorf("RequiresApproval(%q) = %v, want %v", tc.command, got, tc.want)
		}
	}
}

func TestRequiresApproval_Modes(t *testing.T) {
	dir := t.TempDir()

	off := NewWithApproval(dir, ApprovalPolicy{Mode: ApprovalOff})
	if off.RequiresApproval(map[string]any{"command": "rm -rf /"}) {
		t.Error("mode off should never require approval")
	}

	prompt := NewWithApproval(dir, ApprovalPolicy{Mode: ApprovalPrompt})
	if !prompt.RequiresApproval(map[string]any{"command": "ls"}) {
		t.Error("mode prompt should always require approval, even for ls")
	}

	// Bare New defaults to off (the historical, ungated behaviour).
	if New(dir).RequiresApproval(map[string]any{"command": "rm -rf /"}) {
		t.Error("New (no policy) should default to ungated")
	}

	// Empty mode via NewWithApproval defaults to allowlist (gating on).
	deflt := NewWithApproval(dir, ApprovalPolicy{})
	if !deflt.RequiresApproval(map[string]any{"command": "rm -rf /"}) {
		t.Error("empty mode should default to allowlist and gate a destructive command")
	}
}
