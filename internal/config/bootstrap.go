package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
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

// DefaultSystemPromptFile is the bundled default system prompt. Bootstrap seeds
// it into the config dir (preserved thereafter; refreshed only under force), and
// native agents that set neither system_prompt nor system_prompt_file fall back
// to it (the on-disk copy first, then the embedded copy).
const DefaultSystemPromptFile = "system-prompt.md"

// BootstrapReport summarises the result of a Bootstrap pass: which on-disk
// files were written for the first time, which were refreshed to the shipped
// version, and which were preserved because they already existed and are
// user-owned. Paths are absolute. Optional assets that are absent from the
// embedded FS are reported in none of the buckets.
type BootstrapReport struct {
	Created   []string
	Updated   []string
	Preserved []string
}

// Bootstrap ensures the config directory containing configPath exists and is
// populated with the built-in defaults the first time the app runs. The
// returned report is discarded; callers that need it should use
// BootstrapWithReport instead.
func Bootstrap(configPath string) error {
	_, err := BootstrapWithReport(configPath, false)
	return err
}

// BootstrapWithReport is the report-returning variant of Bootstrap. On
// success it returns a report describing what was created, refreshed, and
// preserved.
//
// Under the workspace directory (the directory holding configPath, e.g.
// ~/.config/murtaugh) it manages:
//   - slack.yaml (seeded with the default Slack configuration), agents.yaml,
//     and jobs.yaml — created on first run, then PRESERVED: these hold the
//     user's tokens and customisations and are never overwritten.
//   - templates/ — the bundled Block Kit templates (ping/, unfurl/), also
//     PRESERVED once on disk so workflow/unfurl edits survive.
//   - .agents/skills/ — created (empty) as the home for the user's bespoke
//     skills. The bundled murtaugh-* skills are served in-binary and are NOT
//     mirrored here, which keeps them out of reach of the file/terminal tools;
//     an agent's export_skills_to_fs (ReconcileExportedSkills) writes chosen
//     ones into a workdir on demand.
//   - .claude/skills — a symlink to .agents/skills so a filesystem-discovering
//     agent finds whatever lands there (bespoke skills + any exports).
//   - AGENTS.md and BOOTSTRAP.md, when those docs are embedded in assets/.
func BootstrapWithReport(configPath string, force bool) (BootstrapReport, error) {
	report := BootstrapReport{}
	baseDir := filepath.Dir(configPath)
	if err := os.MkdirAll(baseDir, bootstrapDirPerm); err != nil {
		return report, fmt.Errorf("create config dir %q: %w", baseDir, err)
	}

	// Config files and workspace docs are seeded once and then preserved —
	// they carry the user's tokens and edits.
	plan := []struct{ src, dst string }{
		{"slack.yaml", configPath},
		{"agents.yaml", filepath.Join(baseDir, "agents.yaml")},
		{"jobs.yaml", filepath.Join(baseDir, "jobs.yaml")},
		{"journal.yaml", filepath.Join(baseDir, "journal.yaml")},
		{"workflow-rules.yaml", filepath.Join(baseDir, "workflow-rules.yaml")},
		{"unfurl-rules.yaml", filepath.Join(baseDir, "unfurl-rules.yaml")},
		// Seed a template .env (from the non-dotfile asset env.example) so a
		// fresh install has the credentials file to fill in. preserveExisting
		// means a real .env is never clobbered.
		{"env.example", filepath.Join(baseDir, EnvFileName)},
	}
	for _, name := range optionalBootstrapDocs {
		plan = append(plan, struct{ src, dst string }{name, filepath.Join(baseDir, name)})
	}
	for _, entry := range plan {
		outcome, err := copyAssetFile(entry.src, entry.dst, preserveExisting)
		if err != nil {
			return report, err
		}
		report.absorb(outcome, entry.dst)
	}

	// The default system prompt is a Murtaugh-owned default: seeded once and then
	// preserved (operators may edit it), but refreshed to the shipped version
	// under force so a binary upgrade can deliver an improved default on request.
	// User-state files above (slack/agents/jobs/journal/.env) and AGENTS.md — the
	// agent's identity — are always preserved, never force-overwritten.
	promptPolicy := preserveExisting
	if force {
		promptPolicy = refreshFromAssets
	}
	promptPath := filepath.Join(baseDir, DefaultSystemPromptFile)
	outcome, err := copyAssetFile(DefaultSystemPromptFile, promptPath, promptPolicy)
	if err != nil {
		return report, err
	}
	report.absorb(outcome, promptPath)

	// Block Kit templates land at <workspace>/templates/... and are preserved
	// once on disk (they're user-customisable workflow assets).
	outcomes, err := copyAssetTree("templates", filepath.Join(baseDir, "templates"), preserveExisting)
	if err != nil {
		return report, err
	}
	for _, item := range outcomes {
		report.absorb(item.outcome, item.path)
	}

	// The bundled (murtaugh-*) skills are served in-binary and are NOT mirrored
	// to disk — that's what keeps them out of reach of the file/terminal tools.
	// We only ensure <workspace>/.agents/skills exists as the home for the user's
	// own bespoke skills (and the target an agent's export_skills_to_fs writes
	// into); the .claude/skills symlink lets filesystem-discovering agents find
	// whatever lands there.
	if err := os.MkdirAll(filepath.Join(baseDir, ".agents", "skills"), bootstrapDirPerm); err != nil {
		return report, fmt.Errorf("create skills dir: %w", err)
	}
	link, err := linkClaudeSkills(baseDir)
	if err != nil {
		return report, err
	}
	report.absorb(link.outcome, link.path)

	return report, nil
}

// ReconcileExportedSkills makes <workDir>/.agents/skills hold exactly the bundled
// (murtaugh-*) skills named in list — an agent's export_skills_to_fs — copied
// from the embed, alongside whatever bespoke skills already live there. Listed
// skills are (re)written fresh; any bundled skill previously exported but no
// longer listed is removed; bespoke (non-bundled) directories are never touched.
// The sentinel "all" exports every bundled skill; an empty list removes all
// exported bundled skills (the sealed default). When anything is exported it
// ensures the .claude/skills symlink so filesystem-discovering agents find them.
// It returns the sorted names left on disk. Callers should treat an error as
// non-fatal (the agent still works; only filesystem discovery is affected).
func ReconcileExportedSkills(workDir string, list []string) ([]string, error) {
	skillsDir := filepath.Join(workDir, ".agents", "skills")
	if err := os.MkdirAll(skillsDir, bootstrapDirPerm); err != nil {
		return nil, fmt.Errorf("create skills dir: %w", err)
	}

	bundled := make(map[string]bool)
	for _, n := range assets.SkillNames() {
		bundled[n] = true
	}
	desired := expandExportList(list, assets.SkillNames())
	want := make(map[string]bool, len(desired))
	for _, n := range desired {
		want[n] = true
	}

	// Remove bundled skills previously exported but no longer desired. Only ever
	// touch the murtaugh-* namespace — bespoke skills are left alone.
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil, fmt.Errorf("read skills dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() && bundled[e.Name()] && !want[e.Name()] {
			if err := os.RemoveAll(filepath.Join(skillsDir, e.Name())); err != nil {
				return nil, fmt.Errorf("remove stale exported skill %q: %w", e.Name(), err)
			}
		}
	}

	// (Re)write each desired skill fresh, so a skill that lost files across a
	// binary upgrade doesn't leave orphans behind.
	for _, name := range desired {
		dst := filepath.Join(skillsDir, name)
		if err := os.RemoveAll(dst); err != nil {
			return nil, fmt.Errorf("clear exported skill %q: %w", name, err)
		}
		if _, err := copyAssetTree("skills/"+name, dst, refreshFromAssets); err != nil {
			return nil, fmt.Errorf("export skill %q: %w", name, err)
		}
	}

	if len(desired) > 0 {
		if _, err := linkClaudeSkills(workDir); err != nil {
			return nil, err
		}
	}
	sort.Strings(desired)
	return desired, nil
}

// expandExportList resolves an export_skills_to_fs list to concrete bundled
// skill names: the sentinel "all" expands to every known name; otherwise the
// listed names, de-duplicated and order-preserving. Blanks are dropped. (Names
// are validated at config load, so unknowns never reach here.)
func expandExportList(list, known []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, raw := range list {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if s == exportSkillsAll {
			return append([]string(nil), known...)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// copyOutcome describes what happened to a single destination path during a
// bootstrap pass.
type copyOutcome int

const (
	copyOutcomeSkippedMissingAsset copyOutcome = iota
	copyOutcomeCreated
	copyOutcomePreserved
	// copyOutcomeUpdated means an existing destination was overwritten with the
	// shipped asset because its content differed (only possible under
	// refreshFromAssets).
	copyOutcomeUpdated
)

// copyPolicy decides what copyAssetFile does when the destination already
// exists.
type copyPolicy int

const (
	// preserveExisting never overwrites an existing destination — the policy
	// for user-owned files (config, templates, workspace docs).
	preserveExisting copyPolicy = iota
	// refreshFromAssets overwrites an existing destination when the shipped
	// content differs — the policy for the bundled skills, which Murtaugh owns
	// and keeps current across binary upgrades.
	refreshFromAssets
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
	case copyOutcomeUpdated:
		r.Updated = append(r.Updated, dst)
	case copyOutcomePreserved:
		r.Preserved = append(r.Preserved, dst)
	}
}

// copyAssetTree mirrors the embedded directory srcRoot into dstRoot,
// preserving the subtree structure. dstRoot is created even when the tree is
// empty so a dependent symlink (see linkClaudeSkills) never dangles. The
// supplied policy governs how existing destination files are handled
// (preserve vs refresh); see copyAssetFile. The walk only ever writes files
// that exist in the embedded tree, so destination files Murtaugh does not ship
// (e.g. a user's own skill) are never touched. A missing embedded srcRoot is
// tolerated (returns no results), keeping the asset optional.
func copyAssetTree(srcRoot, dstRoot string, policy copyPolicy) ([]skillCopyResult, error) {
	if _, err := fs.Stat(assets.FS, srcRoot); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err := os.MkdirAll(dstRoot, bootstrapDirPerm); err != nil {
		return nil, fmt.Errorf("create dir %q: %w", dstRoot, err)
	}
	var results []skillCopyResult
	walkErr := fs.WalkDir(assets.FS, srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel := strings.TrimPrefix(p, srcRoot+"/")
		dst := filepath.Join(dstRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), bootstrapDirPerm); err != nil {
			return fmt.Errorf("create dir %q: %w", filepath.Dir(dst), err)
		}
		outcome, err := copyAssetFile(p, dst, policy)
		if err != nil {
			return err
		}
		results = append(results, skillCopyResult{path: dst, outcome: outcome})
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("copy embedded tree %q: %w", srcRoot, walkErr)
	}
	return results, nil
}

// linkClaudeSkills creates <baseDir>/.claude/skills as a relative symlink to
// the sibling .agents/skills directory, so a filesystem-discovering agent finds
// whatever lands there (the user's bespoke skills plus any an agent exported via
// export_skills_to_fs) without a duplicate copy. It is
// non-destructive and idempotent: when anything already exists at the link
// path (a prior symlink, a real directory, a file) it is preserved untouched.
func linkClaudeSkills(baseDir string) (skillCopyResult, error) {
	link := filepath.Join(baseDir, ".claude", "skills")
	if _, err := os.Lstat(link); err == nil {
		return skillCopyResult{path: link, outcome: copyOutcomePreserved}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return skillCopyResult{}, fmt.Errorf("stat %q: %w", link, err)
	}
	if err := os.MkdirAll(filepath.Dir(link), bootstrapDirPerm); err != nil {
		return skillCopyResult{}, fmt.Errorf("create dir %q: %w", filepath.Dir(link), err)
	}
	// Relative target resolved from the link's own directory (.claude/):
	// ../.agents/skills → <baseDir>/.agents/skills.
	target := filepath.Join("..", ".agents", "skills")
	if err := os.Symlink(target, link); err != nil {
		return skillCopyResult{}, fmt.Errorf("symlink %q -> %q: %w", link, target, err)
	}
	return skillCopyResult{path: link, outcome: copyOutcomeCreated}, nil
}

// copyAssetFile writes the embedded asset src to dst. It silently skips when
// src is not present in the embedded FS so that optional assets remain
// optional. When dst already exists, the policy decides: preserveExisting
// leaves it untouched; refreshFromAssets overwrites it only when the shipped
// content differs (so an unchanged skill is a no-op and does not churn the
// file's mtime). The returned outcome distinguishes a fresh write (Created),
// an in-place refresh (Updated), a left-alone file (Preserved), and a missing
// optional asset.
func copyAssetFile(src, dst string, policy copyPolicy) (copyOutcome, error) {
	existing, statErr := os.ReadFile(dst)
	switch {
	case statErr == nil:
		// dst exists.
	case errors.Is(statErr, fs.ErrNotExist):
		existing = nil
	default:
		return copyOutcomeSkippedMissingAsset, fmt.Errorf("read %q: %w", dst, statErr)
	}

	if statErr == nil && policy == preserveExisting {
		return copyOutcomePreserved, nil
	}

	data, err := assets.FS.ReadFile(src)
	if errors.Is(err, fs.ErrNotExist) {
		return copyOutcomeSkippedMissingAsset, nil
	}
	if err != nil {
		return copyOutcomeSkippedMissingAsset, fmt.Errorf("read embedded asset %q: %w", src, err)
	}

	if statErr == nil {
		// Existing file under refreshFromAssets: rewrite only if it drifted.
		if bytes.Equal(existing, data) {
			return copyOutcomePreserved, nil
		}
		if err := os.WriteFile(dst, data, bootstrapFilePerm); err != nil {
			return copyOutcomeSkippedMissingAsset, fmt.Errorf("write %q: %w", dst, err)
		}
		return copyOutcomeUpdated, nil
	}

	if err := os.WriteFile(dst, data, bootstrapFilePerm); err != nil {
		return copyOutcomeSkippedMissingAsset, fmt.Errorf("write %q: %w", dst, err)
	}
	return copyOutcomeCreated, nil
}
