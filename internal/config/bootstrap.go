package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/assets"
)

const (
	bootstrapDirPerm  = 0o755
	bootstrapFilePerm = 0o644
)

// optionalBootstrapDocs are copied from the embedded assets into the config
// directory on first run when present. They are skipped silently when the
// asset is not bundled, satisfying the "skip if they don't exist" convention.
var optionalBootstrapDocs = []string{"AGENTS.md", "BOOTSTRAP.md"}

// BootstrapReport summarises the result of a Bootstrap pass: which on-disk
// files were written for the first time, and which were preserved because
// they already existed. Paths are absolute. Optional assets that are absent
// from the embedded FS are reported in neither bucket.
type BootstrapReport struct {
	Created   []string
	Preserved []string
}

// Bootstrap ensures the config directory containing configPath exists and is
// populated with the built-in defaults the first time the app runs. It is
// idempotent: existing files are never overwritten. The returned report is
// discarded; callers that need it should use BootstrapWithReport instead.
func Bootstrap(configPath string) error {
	_, err := BootstrapWithReport(configPath)
	return err
}

// BootstrapWithReport is the report-returning variant of Bootstrap. It
// performs the same idempotent seeding and, on success, returns a report
// describing what was created and what was preserved.
//
// On a fresh install it creates:
//   - the config directory (e.g. ~/.config/murtaugh)
//   - slack.yaml seeded with the default ping/pong configuration
//   - a skills/ directory holding every skill bundled in assets/skills
//   - AGENTS.md and BOOTSTRAP.md, when those docs are embedded in assets/
func BootstrapWithReport(configPath string) (BootstrapReport, error) {
	report := BootstrapReport{}
	baseDir := filepath.Dir(configPath)
	if err := os.MkdirAll(baseDir, bootstrapDirPerm); err != nil {
		return report, fmt.Errorf("create config dir %q: %w", baseDir, err)
	}

	plan := []struct{ src, dst string }{
		{"slack.yaml", configPath},
		{"agents.yaml", filepath.Join(baseDir, "agents.yaml")},
		{"jobs.yaml", filepath.Join(baseDir, "jobs.yaml")},
	}
	for _, name := range optionalBootstrapDocs {
		plan = append(plan, struct{ src, dst string }{name, filepath.Join(baseDir, name)})
	}
	for _, entry := range plan {
		outcome, err := copyAssetFile(entry.src, entry.dst)
		if err != nil {
			return report, err
		}
		report.absorb(outcome, entry.dst)
	}

	skillOutcomes, err := copySkills(filepath.Join(baseDir, "skills"))
	if err != nil {
		return report, err
	}
	for _, item := range skillOutcomes {
		report.absorb(item.outcome, item.path)
	}
	return report, nil
}

// copyOutcome describes what happened to a single destination path during a
// bootstrap pass.
type copyOutcome int

const (
	copyOutcomeSkippedMissingAsset copyOutcome = iota
	copyOutcomeCreated
	copyOutcomePreserved
)

// skillCopyResult pairs a destination path with its outcome so the caller
// can report skills/* paths individually.
type skillCopyResult struct {
	path    string
	outcome copyOutcome
}

func (r *BootstrapReport) absorb(outcome copyOutcome, dst string) {
	switch outcome {
	case copyOutcomeCreated:
		r.Created = append(r.Created, dst)
	case copyOutcomePreserved:
		r.Preserved = append(r.Preserved, dst)
	}
}

// copySkills mirrors every *.md skill from the embedded assets/skills directory
// into skillsDir, creating it on demand. Existing skills are left untouched.
// The returned slice records the per-file outcome so the caller can report
// skills/* paths individually.
func copySkills(skillsDir string) ([]skillCopyResult, error) {
	entries, err := assets.FS.ReadDir("skills")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read embedded skills: %w", err)
	}
	if err := os.MkdirAll(skillsDir, bootstrapDirPerm); err != nil {
		return nil, fmt.Errorf("create skills dir %q: %w", skillsDir, err)
	}
	var results []skillCopyResult
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		src := path.Join("skills", entry.Name())
		dst := filepath.Join(skillsDir, entry.Name())
		outcome, err := copyAssetFile(src, dst)
		if err != nil {
			return nil, err
		}
		results = append(results, skillCopyResult{path: dst, outcome: outcome})
	}
	return results, nil
}

// copyAssetFile writes the embedded asset src to dst. It never overwrites an
// existing dst, and silently skips when src is not present in the embedded FS
// so that optional assets remain optional. The returned outcome distinguishes
// between a fresh write, a preserved file, and a missing optional asset.
func copyAssetFile(src, dst string) (copyOutcome, error) {
	if _, err := os.Stat(dst); err == nil {
		return copyOutcomePreserved, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return copyOutcomeSkippedMissingAsset, fmt.Errorf("stat %q: %w", dst, err)
	}

	data, err := assets.FS.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return copyOutcomeSkippedMissingAsset, nil
	}
	if err != nil {
		return copyOutcomeSkippedMissingAsset, fmt.Errorf("read embedded asset %q: %w", src, err)
	}

	if err := os.WriteFile(dst, data, bootstrapFilePerm); err != nil {
		return copyOutcomeSkippedMissingAsset, fmt.Errorf("write %q: %w", dst, err)
	}
	return copyOutcomeCreated, nil
}
