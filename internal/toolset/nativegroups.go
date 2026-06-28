package toolset

// NativeGroup classifies one synthesized native tool group: whether it needs a
// per-agent workspace root (a *files.Root) to build, and whether it is
// native-only — i.e. stripped from an ACP agent's bridge surface, which has its
// own files/terminal/skills. This is the SINGLE source of truth both consumers
// read from: the Resolve switch below and the ACP aggregator's strip-list (in
// internal/agentbuild). Adding a native group here, and only here, keeps the two
// in sync; the exhaustiveness test asserts neither drifts.
type NativeGroup struct {
	Name          string
	WorkdirRooted bool // needs a *files.Root (Deps.Root) to construct
	NativeOnlyACP bool // stripped from the ACP aggregator's surface
}

// NativeGroups is the authoritative classification of the synthesized native
// tool groups. `attach` is deliberately NOT native-only: it delivers a workspace
// file to the user and is wanted on the ACP path too (confined to the agent's
// workdir Root, so it cannot exfiltrate host files). `manage` is intentionally
// absent — it synthesizes no tool (it is a skills-visibility token that falls
// through to a registry no-match), so it is not a native group.
var NativeGroups = []NativeGroup{
	{Name: GroupFiles, WorkdirRooted: true, NativeOnlyACP: true},
	{Name: GroupTerminal, WorkdirRooted: true, NativeOnlyACP: true},
	{Name: GroupSkills, WorkdirRooted: false, NativeOnlyACP: true},
	{Name: GroupAttach, WorkdirRooted: true, NativeOnlyACP: false},
}

func nativeGroup(name string) (NativeGroup, bool) {
	for _, g := range NativeGroups {
		if g.Name == name {
			return g, true
		}
	}
	return NativeGroup{}, false
}

// IsNativeGroup reports whether name is a synthesized native tool group.
func IsNativeGroup(name string) bool {
	_, ok := nativeGroup(name)
	return ok
}

// IsWorkdirRooted reports whether the named native group needs a *files.Root to
// build (files/terminal/attach). Used by the build seam to decide which groups
// to drop when an agent has no resolvable workspace.
func IsWorkdirRooted(name string) bool {
	g, ok := nativeGroup(name)
	return ok && g.WorkdirRooted
}

// IsNativeOnlyForACP reports whether the named native group must be stripped from
// an ACP agent's bridge surface (files/terminal/skills). `attach` returns false:
// it is served to ACP agents too.
func IsNativeOnlyForACP(name string) bool {
	g, ok := nativeGroup(name)
	return ok && g.NativeOnlyACP
}
