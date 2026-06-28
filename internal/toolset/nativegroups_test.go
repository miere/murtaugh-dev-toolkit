package toolset

import (
	"os"
	"slices"
	"testing"
)

// TestNativeGroups_ResolveHandlesEachAsNative asserts the single source of truth
// (NativeGroups) stays in sync with the Resolve switch: every classified native
// group must actually synthesize at least one tool (i.e. it is recognised as
// native, not silently treated as a registry namespace). Adding a group to
// NativeGroups without wiring it into Resolve fails here.
func TestNativeGroups_ResolveHandlesEachAsNative(t *testing.T) {
	dir := t.TempDir()
	deps := Deps{Root: mustRoot(t, dir), ManagedSkillsFS: os.DirFS(dir)}
	for _, g := range NativeGroups {
		got, probs, err := Resolve([]string{g.Name}, nil, deps)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", g.Name, err)
		}
		if len(probs) != 0 {
			t.Fatalf("Resolve(%q): unexpected problems %v", g.Name, probs)
		}
		if len(got) == 0 {
			t.Fatalf("native group %q synthesized no tools — it is classified in NativeGroups but not handled by Resolve", g.Name)
		}
	}
}

// TestNativeGroups_Membership pins the classification so a drift is a test change,
// not a silent behaviour change. `manage` is deliberately not a native group (it
// synthesizes nothing); the workdir-rooted and ACP-strip sets are asserted here so
// the agentbuild ACP strip-list (IsNativeOnlyForACP) and the seam's prune
// (IsWorkdirRooted) share one authoritative classification.
func TestNativeGroups_Membership(t *testing.T) {
	var groupNames []string
	for _, g := range NativeGroups {
		groupNames = append(groupNames, g.Name)
	}
	slices.Sort(groupNames)
	wantNames := []string{GroupAttach, GroupFiles, GroupSkills, GroupTerminal}
	slices.Sort(wantNames)
	if !slices.Equal(groupNames, wantNames) {
		t.Fatalf("NativeGroups membership drifted: got %v, want %v", groupNames, wantNames)
	}

	if IsNativeGroup(GroupManage) {
		t.Fatal("manage must not be a native group (it synthesizes nothing)")
	}

	wantRooted := map[string]bool{GroupFiles: true, GroupTerminal: true, GroupAttach: true, GroupSkills: false}
	for name, want := range wantRooted {
		if IsWorkdirRooted(name) != want {
			t.Errorf("IsWorkdirRooted(%q) = %v, want %v", name, IsWorkdirRooted(name), want)
		}
	}

	wantNativeOnly := map[string]bool{GroupFiles: true, GroupTerminal: true, GroupSkills: true, GroupAttach: false}
	for name, want := range wantNativeOnly {
		if IsNativeOnlyForACP(name) != want {
			t.Errorf("IsNativeOnlyForACP(%q) = %v, want %v", name, IsNativeOnlyForACP(name), want)
		}
	}
}
