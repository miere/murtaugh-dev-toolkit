package native

import (
	"testing"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

func TestMCPServerConfigsIsAuthoritativeAndSorted(t *testing.T) {
	t.Setenv("MY_TOKEN", "secret")
	servers := map[string]config.MCPServerConfig{
		"zeta":  {URL: "https://z.example"},
		"alpha": {Command: "alpha-bin", Args: []string{"--x"}, Env: map[string]string{"TOK": "${MY_TOKEN}"}},
	}
	got := MCPServerConfigs(servers)
	if len(got) != 2 {
		t.Fatalf("expected both global servers attached, got %d", len(got))
	}
	// Sorted by name: alpha before zeta.
	if got[0].Name != "alpha" || got[1].Name != "zeta" {
		t.Fatalf("expected sorted [alpha, zeta], got [%s, %s]", got[0].Name, got[1].Name)
	}
	if got[0].Env["TOK"] != "secret" {
		t.Fatalf("env ${MY_TOKEN} not expanded, got %q", got[0].Env["TOK"])
	}
	if got[1].URL != "https://z.example" {
		t.Fatalf("url server not carried through, got %q", got[1].URL)
	}
}

func TestMCPServerConfigsEmpty(t *testing.T) {
	if got := MCPServerConfigs(nil); got != nil {
		t.Fatalf("expected nil for no servers, got %#v", got)
	}
}
