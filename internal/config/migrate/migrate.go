// Package migrate converts an on-disk Murtaugh config directory from an older
// schema to the current one. It is a permanent, reusable framework: a versioned
// `.schema_version` sidecar plus an ordered registry of migrations, each applied
// behind a backup/validate/rollback harness so a botched rewrite can never leave
// a running daemon on a half-converted config. Individual migration steps are
// disposable once the audience has upgraded; the framework stays for the next
// schema change.
package migrate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"gopkg.in/yaml.v3"
)

// schemaVersionFile is the machine-managed sidecar holding the integer schema
// version of a config directory. Absent means version 0 (legacy). It lives
// outside any YAML file so a deployment that has no gateway.yaml at all
// (delegation-only, MCP-only) is still versioned.
const schemaVersionFile = ".schema_version"

// CurrentVersion is the schema version this binary expects on disk.
const CurrentVersion = 1

// Migration is one ordered, idempotent transform of a config directory.
type Migration struct {
	Version     int
	Description string
	// Detect reports whether dir still carries the legacy shape this migration
	// converts. When false the step is a no-op (the dir is already compatible)
	// and Run just advances the version stamp.
	Detect func(dir string) bool
	// Apply performs the transform. It must be safe to run on a dir Detect
	// returned true for.
	Apply func(dir string) error
}

// registry is the ordered list of migrations, lowest version first.
var registry = []Migration{
	{
		Version:     1,
		Description: "split slack.yaml into gateway.yaml + rule files; group agents.yaml defaults; nest agent backends",
		Detect:      detectV1,
		Apply:       applyV1,
	},
}

// Version reads the .schema_version sidecar; a missing or unparsable file is
// version 0 (legacy).
func Version(dir string) int {
	data, err := os.ReadFile(filepath.Join(dir, schemaVersionFile))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return n
}

// Stamp writes the schema version sidecar. Exported so a fresh install
// (bootstrap) can mark a brand-new config dir as current without migrating.
func Stamp(dir string, version int) error {
	return os.WriteFile(filepath.Join(dir, schemaVersionFile), []byte(strconv.Itoa(version)+"\n"), 0o644)
}

// Pending returns the migrations whose version exceeds the dir's current stamp.
func Pending(dir string) []Migration {
	cur := Version(dir)
	var out []Migration
	for _, m := range registry {
		if m.Version > cur {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out
}

// Run applies every pending migration to dir in order. Each step backs up the
// directory's config files, applies the transform, then re-validates by loading
// the result as the daemon would; on any failure it rolls back from the backup
// and returns the error. The version stamp advances only after a clean step, so
// Run is idempotent (a no-op once the dir is current). It returns the versions
// applied. A dir with nothing pending returns (nil, nil).
func Run(dir string) ([]int, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, nil
	}
	pending := Pending(dir)
	if len(pending) == 0 {
		return nil, nil
	}
	var applied []int
	for _, m := range pending {
		// Nothing legacy to convert: just advance the stamp.
		if m.Detect != nil && !m.Detect(dir) {
			if err := Stamp(dir, m.Version); err != nil {
				return applied, fmt.Errorf("migrate v%d: stamp: %w", m.Version, err)
			}
			applied = append(applied, m.Version)
			continue
		}
		backupDir, err := backupConfigDir(dir)
		if err != nil {
			return applied, fmt.Errorf("migrate v%d: back up config: %w", m.Version, err)
		}
		if err := m.Apply(dir); err != nil {
			_ = restoreConfigDir(dir, backupDir)
			return applied, fmt.Errorf("migrate v%d (%s): %w", m.Version, m.Description, err)
		}
		if err := validateDir(dir); err != nil {
			_ = restoreConfigDir(dir, backupDir)
			return applied, fmt.Errorf("migrate v%d (%s): result did not validate, rolled back (original preserved at %s): %w",
				m.Version, m.Description, backupDir, err)
		}
		if err := Stamp(dir, m.Version); err != nil {
			return applied, fmt.Errorf("migrate v%d: stamp: %w", m.Version, err)
		}
		applied = append(applied, m.Version)
	}
	return applied, nil
}

// validateDir checks that the migrated files are STRUCTURALLY sound: each parses
// into the config types without a YAML/type error. It deliberately does not run
// the full business Validate (which requires e.g. resolvable oauth tokens) —
// completeness is the operator's concern, not the migration's; the migration is
// only responsible for producing a well-formed rewrite. A type mismatch (the
// real risk of a botched transform — a value landing in the wrong shape) fails
// here and triggers rollback.
func validateDir(dir string) error {
	if data, err := os.ReadFile(filepath.Join(dir, "gateway.yaml")); err == nil {
		var c config.Config
		if err := yaml.Unmarshal(data, &c); err != nil {
			return fmt.Errorf("gateway.yaml: %w", err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(dir, "agents.yaml")); err == nil {
		var a struct {
			Defaults   config.RuntimeDefaults            `yaml:"defaults"`
			Agents     map[string]config.AgentProfile    `yaml:"agents"`
			MCPServers map[string]config.MCPServerConfig `yaml:"mcp_servers"`
		}
		if err := yaml.Unmarshal(data, &a); err != nil {
			return fmt.Errorf("agents.yaml: %w", err)
		}
	}
	for _, name := range []string{"workflow-rules.yaml", "unfurl-rules.yaml"} {
		if data, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
			var m map[string]any
			if err := yaml.Unmarshal(data, &m); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	return nil
}

// backupConfigDir copies every regular file in dir (non-recursive — config files
// live at the top level) into a fresh sibling backup directory and returns its
// path. Subdirectories (templates/, .agents/) are left untouched since no
// migration rewrites them.
func backupConfigDir(dir string) (string, error) {
	backupDir := filepath.Join(dir, ".migration-backup")
	// A previous aborted run may have left one behind; start clean.
	_ = os.RemoveAll(backupDir)
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(backupDir, e.Name()), data, 0o600); err != nil {
			return "", err
		}
	}
	return backupDir, nil
}

// restoreConfigDir reverts dir to the snapshot in backupDir: it removes every
// top-level regular file the migration may have created or rewritten, then
// copies the backed-up files back. The backup directory is removed on success.
func restoreConfigDir(dir, backupDir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	backups, err := os.ReadDir(backupDir)
	if err != nil {
		return err
	}
	for _, b := range backups {
		data, err := os.ReadFile(filepath.Join(backupDir, b.Name()))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, b.Name()), data, 0o600); err != nil {
			return err
		}
	}
	return os.RemoveAll(backupDir)
}
