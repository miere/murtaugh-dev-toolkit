// Package downstream is a (non-sanctioned) consumer used to exercise the guard:
// reading config.AgentProfile.WorkDir here must be flagged, while reading an
// unrelated WorkDir field must not (proving type precision).
package downstream

import "config"

func use(p config.AgentProfile) string {
	return p.WorkDir // want `downstream access to config.AgentProfile.WorkDir is forbidden`
}

func viaPointer(p *config.AgentProfile) string {
	return p.WorkDir // want `downstream access to config.AgentProfile.WorkDir is forbidden`
}

// other has its own WorkDir; it is NOT config.AgentProfile, so it must not be
// flagged — the analyzer matches by resolved field, not by name.
type other struct{ WorkDir string }

func clean(o other) string {
	return o.WorkDir
}
