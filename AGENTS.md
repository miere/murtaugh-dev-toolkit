# Rules
- always work on a worktree
- worktrees can be placed in the ./ignore/worktrees
- never use "merge" branch commits - always do a rebase-merge so we keep the history linear and clean.
- never commit against the main - upstream won't accept.

# Validated core
- A hard-precondition value (one a downstream tool cannot function without) is
  resolved AND validated exactly once, at the build seam where its inputs first
  co-exist — not re-derived with ad-hoc empty-checks at each use site.
- The agent workspace is the canonical example: it is resolved once in
  `agentbuild.Resolve` (profile workdir → workspace dir) and flows downstream as a
  constructed `*files.Root` (a `ResolvedAgent`), never as raw `profile.WorkDir` + a
  base-dir fallback. Downstream packages must not read `profile.WorkDir` or
  re-apply the fallback; the `internal/archtest` `go/analysis` pass (run in CI via
  `cmd/archcheck`) enforces this.
- When you add a workdir-rooted or native-only tool group, classify it once in
  `toolset.NativeGroups`; the exhaustiveness test keeps both consumers (the
  resolver switch and the ACP strip) in sync.
- A precondition that fails for ONE tool degrades that tool, not the whole agent:
  drop the tool, keep the agent and the rest of its toolset alive, and record a
  structured problem (agent + tool + reason) on the `startup.routing` summary so it
  is visible in logs and the troubleshoot bundle. Reserve a fatal error for states
  where no client can be built at all.

