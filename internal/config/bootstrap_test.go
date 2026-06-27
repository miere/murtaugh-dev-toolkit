package config

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/miere/murtaugh/assets"
)

// assertEmbeddedTreeCopied walks the embedded srcRoot subtree and fails the
// test unless every file was mirrored, byte-for-byte, under dstRoot.
func assertEmbeddedTreeCopied(t *testing.T, srcRoot, dstRoot string) {
	t.Helper()
	err := fs.WalkDir(assets.FS, srcRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		want, err := assets.FS.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, srcRoot+"/")
		got, err := os.ReadFile(filepath.Join(dstRoot, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("read bootstrapped %q: %v", rel, err)
		}
		if string(got) != string(want) {
			t.Fatalf("content mismatch for %q", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk embedded %q: %v", srcRoot, err)
	}
}

func TestBootstrapFreshInstall(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "gateway.yaml")

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	want, err := assets.FS.ReadFile("gateway.yaml")
	if err != nil {
		t.Fatalf("read embedded gateway.yaml: %v", err)
	}
	got, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read bootstrapped gateway.yaml: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("gateway.yaml content mismatch: got %q want %q", got, want)
	}

	wantAgents, err := assets.FS.ReadFile("agents.yaml")
	if err != nil {
		t.Fatalf("read embedded agents.yaml: %v", err)
	}
	gotAgents, err := os.ReadFile(filepath.Join(baseDir, "agents.yaml"))
	if err != nil {
		t.Fatalf("read bootstrapped agents.yaml: %v", err)
	}
	if string(gotAgents) != string(wantAgents) {
		t.Fatalf("agents.yaml content mismatch")
	}

	// Templates are mirrored into the workspace.
	assertEmbeddedTreeCopied(t, "templates", filepath.Join(baseDir, "templates"))

	// The bundled (murtaugh-*) skills are served in-binary and must NOT be
	// written to disk by bootstrap — only an agent's export_skills_to_fs does
	// that. The skills dir exists (home for the user's bespoke skills) but holds
	// no bundled skill.
	skillsDir := filepath.Join(baseDir, ".agents", "skills")
	if info, err := os.Stat(skillsDir); err != nil || !info.IsDir() {
		t.Fatalf("expected %s to exist as a dir: %v", skillsDir, err)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "murtaugh-slack")); !os.IsNotExist(err) {
		t.Fatalf("bundled skill was mirrored to disk by bootstrap (stat err=%v)", err)
	}

	// .claude/skills is a relative symlink to .agents/skills so a
	// filesystem-discovering agent finds whatever lands there (bespoke + exports).
	link := filepath.Join(baseDir, ".claude", "skills")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("expected .claude/skills to be a symlink: %v", err)
	}
	if want := filepath.Join("..", ".agents", "skills"); target != want {
		t.Fatalf("symlink target = %q, want %q", target, want)
	}

	// AGENTS.md is embedded, so it is seeded on a fresh install.
	wantDoc, err := assets.FS.ReadFile("AGENTS.md")
	if err != nil {
		t.Fatalf("read embedded AGENTS.md: %v", err)
	}
	gotDoc, err := os.ReadFile(filepath.Join(baseDir, "AGENTS.md"))
	if err != nil {
		t.Fatalf("AGENTS.md not seeded: %v", err)
	}
	if string(gotDoc) != string(wantDoc) {
		t.Fatalf("seeded AGENTS.md content mismatch")
	}
	// BOOTSTRAP.md is not embedded, so it must be skipped silently.
	if _, err := os.Stat(filepath.Join(baseDir, "BOOTSTRAP.md")); !os.IsNotExist(err) {
		t.Fatalf("expected BOOTSTRAP.md to be skipped, stat err=%v", err)
	}
}

// TestBootstrapDoesNotMirrorBundledSkills confirms bootstrap seals the bundled
// skills (never writes murtaugh-* to disk) while preserving config and any
// bespoke skill the user authored.
func TestBootstrapDoesNotMirrorBundledSkills(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "gateway.yaml")

	// Config carries the user's secrets and must never be overwritten.
	const customConfig = "oauth:\n  app_token: keep-me\n"
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(customConfig), 0o644); err != nil {
		t.Fatalf("seed gateway.yaml: %v", err)
	}

	// A skill the user authored (not shipped by Murtaugh) must be left alone.
	customSkill := filepath.Join(baseDir, ".agents", "skills", "my-custom", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(customSkill), 0o755); err != nil {
		t.Fatalf("seed custom skill dir: %v", err)
	}
	const customSkillBody = "user authored skill"
	if err := os.WriteFile(customSkill, []byte(customSkillBody), 0o644); err != nil {
		t.Fatalf("seed custom skill: %v", err)
	}

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	if got, _ := os.ReadFile(configPath); string(got) != customConfig {
		t.Fatalf("gateway.yaml was overwritten: got %q", got)
	}
	// No bundled skill was written to disk.
	if _, err := os.Stat(filepath.Join(baseDir, ".agents", "skills", "murtaugh-slack")); !os.IsNotExist(err) {
		t.Fatalf("bootstrap mirrored a bundled skill to disk (stat err=%v)", err)
	}
	// The user's own skill is untouched.
	if got, _ := os.ReadFile(customSkill); string(got) != customSkillBody {
		t.Fatalf("user-authored skill was overwritten: got %q", got)
	}
}

// TestReconcileExportedSkills covers the export reconcile: listed skills are
// written, "all" expands, an empty list (or removal from the list) cleans up the
// bundled namespace only, and bespoke skills are never touched.
func TestReconcileExportedSkills(t *testing.T) {
	workDir := t.TempDir()
	skillsDir := filepath.Join(workDir, ".agents", "skills")

	// A bespoke skill must survive every reconcile.
	bespoke := filepath.Join(skillsDir, "my-custom", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(bespoke), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bespoke, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Export one bundled skill.
	got, err := ReconcileExportedSkills(workDir, []string{"murtaugh-slack"})
	if err != nil {
		t.Fatalf("export one: %v", err)
	}
	if len(got) != 1 || got[0] != "murtaugh-slack" {
		t.Fatalf("exported = %v, want [murtaugh-slack]", got)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "murtaugh-slack", "SKILL.md")); err != nil {
		t.Fatalf("murtaugh-slack not exported: %v", err)
	}
	// The .claude/skills symlink is created once anything is exported.
	if _, err := os.Lstat(filepath.Join(workDir, ".claude", "skills")); err != nil {
		t.Fatalf(".claude/skills symlink missing after export: %v", err)
	}

	// Re-reconcile with a different single skill: the old one is removed, the new
	// one written, bespoke untouched.
	if _, err := ReconcileExportedSkills(workDir, []string{"murtaugh-jobs"}); err != nil {
		t.Fatalf("export jobs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "murtaugh-slack")); !os.IsNotExist(err) {
		t.Fatalf("murtaugh-slack should have been removed when no longer listed")
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "murtaugh-jobs", "SKILL.md")); err != nil {
		t.Fatalf("murtaugh-jobs not exported: %v", err)
	}

	// "all" exports every bundled skill.
	all, err := ReconcileExportedSkills(workDir, []string{"all"})
	if err != nil {
		t.Fatalf("export all: %v", err)
	}
	if len(all) != len(assets.SkillNames()) {
		t.Fatalf("export all wrote %d skills, want %d", len(all), len(assets.SkillNames()))
	}

	// Empty list seals: every bundled skill removed, bespoke preserved.
	if _, err := ReconcileExportedSkills(workDir, nil); err != nil {
		t.Fatalf("seal: %v", err)
	}
	for _, n := range assets.SkillNames() {
		if _, err := os.Stat(filepath.Join(skillsDir, n)); !os.IsNotExist(err) {
			t.Fatalf("bundled skill %q not removed on empty export", n)
		}
	}
	if got, _ := os.ReadFile(bespoke); string(got) != "mine" {
		t.Fatalf("bespoke skill was touched by reconcile: %q", got)
	}
}

func TestBootstrapSeedsSystemPromptWithForceRefresh(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "gateway.yaml")
	promptPath := filepath.Join(baseDir, DefaultSystemPromptFile)

	want, err := assets.FS.ReadFile(DefaultSystemPromptFile)
	if err != nil {
		t.Fatalf("read embedded default prompt: %v", err)
	}

	// Fresh install seeds the bundled default.
	if _, err := BootstrapWithReport(configPath, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if got, _ := os.ReadFile(promptPath); string(got) != string(want) {
		t.Fatalf("system prompt not seeded to the shipped default")
	}

	// An operator edit survives a normal re-run (preserveExisting).
	if err := os.WriteFile(promptPath, []byte("EDITED BY OPERATOR"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := BootstrapWithReport(configPath, false); err != nil {
		t.Fatalf("bootstrap re-run: %v", err)
	}
	if got, _ := os.ReadFile(promptPath); string(got) != "EDITED BY OPERATOR" {
		t.Fatalf("non-force bootstrap overwrote the operator's edit: %q", got)
	}

	// --force refreshes it back to the shipped default.
	if _, err := BootstrapWithReport(configPath, true); err != nil {
		t.Fatalf("bootstrap --force: %v", err)
	}
	if got, _ := os.ReadFile(promptPath); string(got) != string(want) {
		t.Fatalf("force did not refresh the prompt to the shipped default")
	}
}

func TestBootstrapForceNeverOverwritesAgentsDoc(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "gateway.yaml")
	agentsDocPath := filepath.Join(baseDir, "AGENTS.md")

	if _, err := BootstrapWithReport(configPath, false); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	// Simulate an onboarded identity the agent wrote into AGENTS.md.
	const named = "## Persona\nI am Nova, terse and dry."
	if err := os.WriteFile(agentsDocPath, []byte(named), 0o644); err != nil {
		t.Fatal(err)
	}
	// --force refreshes the system prompt but must NOT touch the agent's identity.
	if _, err := BootstrapWithReport(configPath, true); err != nil {
		t.Fatalf("bootstrap --force: %v", err)
	}
	if got, _ := os.ReadFile(agentsDocPath); string(got) != named {
		t.Fatalf("--force clobbered the agent's AGENTS.md identity: %q", got)
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func TestBootstrapCopiesJobsYAML(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "gateway.yaml")

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	wantJobs, err := assets.FS.ReadFile("jobs.yaml")
	if err != nil {
		t.Fatalf("read embedded jobs.yaml: %v", err)
	}
	gotJobs, err := os.ReadFile(filepath.Join(baseDir, "jobs.yaml"))
	if err != nil {
		t.Fatalf("read bootstrapped jobs.yaml: %v", err)
	}
	if string(gotJobs) != string(wantJobs) {
		t.Fatalf("jobs.yaml content mismatch: got %q want %q", gotJobs, wantJobs)
	}
}

func TestBootstrapDoesNotOverwriteExistingJobsYAML(t *testing.T) {
	baseDir := filepath.Join(t.TempDir(), "murtaugh")
	configPath := filepath.Join(baseDir, "gateway.yaml")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}

	const customJobs = "jobs:\n  my-job:\n    command: /bin/true\n"
	jobsPath := filepath.Join(baseDir, "jobs.yaml")
	if err := os.WriteFile(jobsPath, []byte(customJobs), 0o644); err != nil {
		t.Fatalf("seed jobs.yaml: %v", err)
	}

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}

	if got, _ := os.ReadFile(jobsPath); string(got) != customJobs {
		t.Fatalf("jobs.yaml was overwritten: got %q", got)
	}
}

func TestBootstrapIsIdempotent(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "murtaugh", "gateway.yaml")

	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("first Bootstrap returned error: %v", err)
	}
	if err := Bootstrap(configPath); err != nil {
		t.Fatalf("second Bootstrap returned error: %v", err)
	}
}
