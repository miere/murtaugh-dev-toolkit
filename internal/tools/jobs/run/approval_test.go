package run

import (
	"testing"

	"github.com/miere/murtaugh/internal/config"
)

func TestRequiresApproval(t *testing.T) {
	held, confirmed := false, true
	jobs := map[string]config.JobProfile{
		"held":      {Command: "/bin/echo", Confirmed: &held},      // agent-defined, unconfirmed
		"confirmed": {Command: "/bin/echo", Confirmed: &confirmed}, // agent-defined, confirmed
		"manual":    {Command: "/bin/echo"},                        // hand-written (Confirmed nil)
	}
	tool := New(func(name string) (config.JobProfile, bool) {
		j, ok := jobs[name]
		return j, ok
	})

	cases := []struct {
		name string
		want bool
	}{
		{"held", true},       // an unconfirmed job must be gated
		{"confirmed", false}, // already confirmed
		{"manual", false},    // operator-written, trusted
		{"unknown", false},   // missing job: nothing to gate (Invoke surfaces the error)
	}
	for _, c := range cases {
		if got := tool.RequiresApproval(map[string]any{"name": c.name}); got != c.want {
			t.Errorf("RequiresApproval(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
