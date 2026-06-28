// Package config is a self-contained fixture mirroring the real
// internal/config.AgentProfile shape (a package named "config" with an
// AgentProfile.WorkDir field), so the analyzer test needs no real-module import.
package config

type AgentProfile struct {
	WorkDir string
	Tools   []string
}
