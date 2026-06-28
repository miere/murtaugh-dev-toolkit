// Package native is the in-process, LLM-backed agent.Client (the native backend). It
// owns the conversation array per session and runs the tool-calling turn loop
// itself — no external agent process, no ACP. It satisfies the same agent.Client
// interface ProcessClient does, so SessionManager, the Slack ChatHandler,
// streaming, the journal, and agentdelegate consume it unchanged.
package native

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/miere/murtaugh/assets"
	"github.com/miere/murtaugh/internal/agent"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/llm"
	"github.com/miere/murtaugh/internal/mcpclient"
	"github.com/miere/murtaugh/internal/tools"
	"github.com/miere/murtaugh/internal/tools/files"
	"github.com/miere/murtaugh/internal/tools/skills"
	"github.com/miere/murtaugh/internal/tools/terminal"
	"github.com/miere/murtaugh/internal/toolset"
)

// defaultCacheRetention is the prompt-cache TTL native agents request when the
// profile does not set one. "5m" (ephemeral) matches the providers' short cache
// window and has no behavioural effect — it only lets the static system+tools
// prefix be reused across turns.
const defaultCacheRetention = "5m"

// resolveCacheRetention maps an agent profile's cache_retention to the value the
// provider layer applies: empty ⇒ the default; "off"/"none" ⇒ "" (disabled);
// otherwise the configured value (e.g. "5m"/"1h"). Config validation has already
// restricted the input to the accepted set.
func resolveCacheRetention(configured string) string {
	switch strings.ToLower(strings.TrimSpace(configured)) {
	case "":
		return defaultCacheRetention
	case "off", "none":
		return ""
	default:
		return strings.TrimSpace(configured)
	}
}

// Client is the native LLM-backed agent.Client. It holds the static
// configuration resolved from an AgentProfile; the MCP servers are opened and
// the toolset resolved lazily in Initialize (matching ACP, whose Initialize
// starts the agent process), so constructing a Client does no I/O.
type Client struct {
	provider         llm.Provider
	model            string
	systemPrompt     string
	agentsDoc        string
	skillsIndex      string
	maxTurns         int
	workDir          string
	root             *files.Root
	managedSkillsFS  fs.FS
	bespokeSkillsDir string
	registry         *tools.Registry
	toolAllow        []string
	serverCfgs       []mcpclient.ServerConfig
	contextLimit     int
	compaction       CompactionMode
	cacheRetention   string
	terminalApproval terminal.ApprovalPolicy
	approver         Approver
	logger           *slog.Logger
	now              func() time.Time

	mu          sync.Mutex
	mcp         *mcpclient.Manager
	loop        *Loop
	initialized bool
	seq         int
	sessions    map[string]*nativeSession
	cancels     map[string]*inflight
}

// nativeSession is the per-conversation state: the owned message array.
type nativeSession struct {
	conv *Conversation
}

// inflight wraps a prompt's cancel func so cleanup can remove its own entry by
// pointer identity (func values are not comparable in Go).
type inflight struct {
	cancel context.CancelFunc
}

// BuildDeps carries the shared context Build needs to turn a profile into a
// live Client: the in-process tool registry (for the `tools:` allowlist), the
// MCP server definitions to resolve references against, the workspace/config dir
// (persona + system-prompt + skills location), and the agent's already-resolved
// workspace root and effective allowlist.
type BuildDeps struct {
	Registry   *tools.Registry
	MCPServers map[string]config.MCPServerConfig
	// WorkspaceDir is the workspace/config root for persona (SOUL.md), the
	// system-prompt file, and the bespoke-skills dir. It is NOT the agent workdir
	// (that is Root) — the two were disentangled by the validated-core refactor.
	WorkspaceDir string
	// Root is the agent's resolved workspace root (the files/terminal/attach
	// root and the turn-context cwd). nil means the agent has no workspace; the
	// workdir-rooted groups were already dropped from Tools upstream.
	Root *files.Root
	// Tools is the effective (already-pruned) tool allowlist. Build uses this
	// rather than profile.Tools so a group dropped at the seam stays dropped.
	Tools  []string
	Logger *slog.Logger
	// Approver gates side-effecting tool calls behind human approval. nil
	// disables gating — set only on the interactive chat path (the gateway),
	// never for headless/delegated agents that have no human to ask.
	Approver Approver
}

// Build constructs a native Client from a kind:native AgentProfile. It resolves
// the provider and credentials and maps the agent's MCP references, but does no
// network/process I/O — that happens in Initialize. The api key is read from the
// environment variable the profile names (api_key_env), which the dotenv loader
// has already populated.
func Build(profile config.AgentProfile, deps BuildDeps) (*Client, error) {
	n := profile.Native
	if n == nil {
		return nil, fmt.Errorf("native: profile has no native block")
	}
	family, err := llm.ParseFamily(n.Provider)
	if err != nil {
		return nil, fmt.Errorf("native: %w", err)
	}
	keyEnv := strings.TrimSpace(n.APIKeyEnv)
	apiKey := strings.TrimSpace(os.Getenv(keyEnv))
	if apiKey == "" {
		return nil, fmt.Errorf("native: api_key_env %q is empty (set it in ~/.config/murtaugh/.env)", keyEnv)
	}
	provider, err := llm.New(family, n.Model, n.BaseURL, apiKey)
	if err != nil {
		return nil, fmt.Errorf("native: build provider: %w", err)
	}
	contextLimit := n.ContextLimit
	if contextLimit <= 0 {
		contextLimit = llm.DefaultContextLimit(family)
	}
	systemPrompt, err := resolveSystemPrompt(profile, deps.WorkspaceDir)
	if err != nil {
		return nil, err
	}
	// The shared persona (SOUL.md, set up once for Murtaugh) is prepended to the
	// static system prompt so it stays in the cacheable prefix. It is the same
	// persona an ACP agent gets injected, keeping the two backends' voice aligned.
	systemPrompt = PrependPersona(ReadSoul(deps.WorkspaceDir), systemPrompt)
	// The agent workdir is resolved upstream (the seam) into deps.Root; an absent
	// root means no workspace (the workdir-rooted tools were already pruned).
	var workDir string
	if deps.Root != nil {
		workDir = deps.Root.Dir()
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	// Global mcp_servers are authoritative: every agent attaches all of them, so
	// the per-agent profile.MCPServers list is no longer a selector (spec 015 §4).
	serverCfgs := MCPServerConfigs(deps.MCPServers)

	// Advertise the available skills in the (static, cacheable) system prompt —
	// but only when the agent can actually load them, i.e. the skills tool is in
	// its allowlist. Read once here; skills change rarely and a restart re-reads,
	// which keeps the index stable across turns so the system prompt stays
	// cacheable.
	// Managed (murtaugh-*) skills are served from the embedded FS, never disk;
	// the on-disk dir holds only the user's bespoke skills, layered in.
	managedSkillsFS := assets.Skills()
	bespokeSkillsDir := filepath.Join(deps.WorkspaceDir, ".agents", "skills")
	var skillsIndex string
	if containsString(deps.Tools, toolset.GroupSkills) {
		skillsIndex = renderSkillsIndex(managedSkillsFS, bespokeSkillsDir, deps.Tools)
	}

	return &Client{
		provider:         provider,
		model:            n.Model,
		systemPrompt:     systemPrompt,
		agentsDoc:        readAgentsDoc(workDir),
		skillsIndex:      skillsIndex,
		maxTurns:         n.MaxTurns,
		workDir:          workDir,
		root:             deps.Root,
		managedSkillsFS:  managedSkillsFS,
		bespokeSkillsDir: bespokeSkillsDir,
		registry:         deps.Registry,
		toolAllow:        deps.Tools,
		serverCfgs:       serverCfgs,
		contextLimit:     contextLimit,
		compaction:       parseCompaction(n.Compaction),
		cacheRetention:   resolveCacheRetention(n.CacheRetention),
		terminalApproval: terminal.ApprovalPolicy{Mode: strings.TrimSpace(profile.Approval.Terminal), Allow: profile.Approval.Allow},
		approver:         deps.Approver,
		logger:           logger,
		now:              time.Now,
		sessions:         make(map[string]*nativeSession),
		cancels:          make(map[string]*inflight),
	}, nil
}

// Initialize opens the agent's MCP servers and resolves its toolset, then builds
// the turn loop. Idempotent: a second call is a no-op. Matches the ACP contract
// where Initialize does the backend's startup work.
func (c *Client) Initialize(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.initialized {
		return nil
	}
	c.mcp = mcpclient.Open(ctx, c.serverCfgs, c.logger)
	ts, problems, err := toolset.Resolve(c.toolAllow, c.mcp.Tools(), toolset.Deps{
		Registry:         c.registry,
		Root:             c.root,
		ManagedSkillsFS:  c.managedSkillsFS,
		BespokeSkillsDir: c.bespokeSkillsDir,
		TerminalApproval: c.terminalApproval,
	})
	if err != nil {
		return fmt.Errorf("native: resolve toolset: %w", err)
	}
	// The toolAllow is already pruned at the build seam, so problems should be
	// empty here; log any that slip through (belt-and-suspenders) so a dropped
	// feature is never silent.
	for _, p := range problems {
		c.logger.Warn("native agent tool disabled", "tool", p.Group, "reason", p.Reason)
	}
	c.loop = NewLoop(c.provider, c.model, ts, c.maxTurns).
		WithCompaction(c.contextLimit, c.compaction).
		WithCache(c.cacheRetention).
		WithApprover(c.approver)
	c.initialized = true
	c.logger.Info("native agent initialized", "model", c.model, "tools", len(ts), "mcp_servers", len(c.serverCfgs),
		"context_limit", c.contextLimit, "compaction", c.compaction)
	return nil
}

// NewSession allocates an in-memory conversation and returns its id. The
// metadata is accepted for interface parity (the Slack location is carried per
// prompt and folded into the system prompt instead).
func (c *Client) NewSession(_ context.Context, _ agent.SessionMetadata) (agent.Session, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	id := fmt.Sprintf("native-%d", c.seq)
	c.sessions[id] = &nativeSession{conv: NewConversation()}
	return agent.Session{ID: id}, nil
}

// Prompt appends the user's turn to the session conversation and runs the loop,
// streaming agent.Event values on the returned channel until the turn completes.
// The system prompt stays static (base + AGENTS.md guidelines + skills index) so
// the provider caches it; the volatile per-turn context (time, cwd, Slack
// location) and a cold-session History backfill are folded into the SAME user
// message, so the array never gains a second consecutive message.
func (c *Client) Prompt(ctx context.Context, sessionID string, req agent.PromptRequest) (<-chan agent.Event, error) {
	c.mu.Lock()
	sess, ok := c.sessions[sessionID]
	loop := c.loop
	if ok {
		// Replace any prior in-flight cancel for this session.
		if prev := c.cancels[sessionID]; prev != nil {
			prev.cancel()
		}
	}
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("native: unknown session %q", sessionID)
	}
	if loop == nil {
		return nil, errors.New("native: client not initialized (call Initialize first)")
	}

	// The system prompt is STATIC (base + stable skills index) so the provider
	// caches it across turns and conversations. The volatile per-turn context
	// (time, cwd, Slack location) and the cold-start history backfill are folded
	// into THIS user message — never a standalone message, so the MOIM-safety
	// invariant holds. See the native-context-caching decision.
	system := BuildSystemPrompt(c.systemPrompt, c.agentsDoc, c.skillsIndex)

	var parts []string
	if ctxBlock := RenderTurnContext(VolatileContextFromRequest(req, c.now(), c.workDir)); ctxBlock != "" {
		parts = append(parts, ctxBlock)
	}
	if h := strings.TrimSpace(req.History); h != "" {
		parts = append(parts, "<thread-transcript>\n"+h+"\n</thread-transcript>")
	}
	parts = append(parts, req.Text)
	sess.conv.AppendUser(strings.Join(parts, "\n\n"))

	runCtx, cancel := context.WithCancel(ctx)
	// Carry the Slack conversation on the turn context so interactive tools (the
	// `ask` tool, and later the approval gate) post into this thread without
	// relying on the model to pass it. Empty for non-chat callers, which makes
	// those tools refuse rather than block.
	runCtx = agent.WithTurnLocation(runCtx, agent.TurnLocation{ChannelID: req.Channel, ThreadTS: req.Thread})
	inf := &inflight{cancel: cancel}
	c.mu.Lock()
	c.cancels[sessionID] = inf
	c.mu.Unlock()

	events := make(chan agent.Event, 32)
	go func() {
		defer close(events)
		defer func() {
			c.mu.Lock()
			if c.cancels[sessionID] == inf {
				delete(c.cancels, sessionID)
			}
			c.mu.Unlock()
			cancel()
		}()
		emit := func(ev agent.Event) {
			select {
			case events <- ev:
			case <-runCtx.Done():
			}
		}
		if _, err := loop.Run(runCtx, sess.conv, system, emit); err != nil {
			c.logger.Debug("native turn ended with error", "session", sessionID, "error", err)
		}
	}()
	return events, nil
}

// Cancel aborts the in-flight prompt for a session, if any. Best-effort.
func (c *Client) Cancel(_ context.Context, sessionID string) error {
	c.mu.Lock()
	inf := c.cancels[sessionID]
	c.mu.Unlock()
	if inf != nil {
		inf.cancel()
	}
	return nil
}

// SupportsCancel reports that the native client can interrupt an in-flight
// prompt (it cancels the turn's context). Satisfies the SessionManager's
// optional capability probe.
func (c *Client) SupportsCancel(context.Context) bool { return true }

// Close shuts down the agent's MCP servers. The in-process tools need no
// teardown.
func (c *Client) Close() error {
	c.mu.Lock()
	mcp := c.mcp
	c.mu.Unlock()
	if mcp != nil {
		return mcp.Close()
	}
	return nil
}

// resolveSystemPrompt returns the agent's base system prompt by precedence:
//  1. an inline system_prompt,
//  2. an explicit system_prompt_file (resolved against the config base dir),
//  3. the seeded default at <baseDir>/system-prompt.md (operator-editable),
//  4. the embedded default shipped in the binary (the floor).
//
// So every native agent has a sane base prompt even with no configuration, and
// an operator can override it per-agent or by editing the seeded file. Config
// validation guarantees 1 and 2 are not both set.
func resolveSystemPrompt(profile config.AgentProfile, baseDir string) (string, error) {
	if profile.Native != nil && strings.TrimSpace(profile.Native.SystemPrompt) != "" {
		return profile.Native.SystemPrompt, nil
	}
	var promptFile string
	if profile.Native != nil {
		promptFile = strings.TrimSpace(profile.Native.SystemPromptFile)
	}
	if file := promptFile; file != "" {
		if !filepath.IsAbs(file) {
			file = filepath.Join(baseDir, file)
		}
		data, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("native: read system_prompt_file %q: %w", file, err)
		}
		return string(data), nil
	}
	// Default: the seeded, operator-editable copy, then the embedded floor.
	if baseDir != "" {
		if data, err := os.ReadFile(filepath.Join(baseDir, config.DefaultSystemPromptFile)); err == nil {
			return string(data), nil
		}
	}
	if data, err := assets.FS.ReadFile(config.DefaultSystemPromptFile); err == nil {
		return string(data), nil
	}
	return "", nil
}

// agentsDocFile is the conventional per-agent guidelines file auto-loaded from
// the agent's working directory into the (static) system prompt.
const agentsDocFile = "AGENTS.md"

// readAgentsDoc loads <workDir>/AGENTS.md when present, for injection into the
// static system prompt as project guidelines. Best-effort: a missing or
// unreadable file yields "" (no guidelines), never an error — like a coding
// agent that simply finds no AGENTS.md in its cwd.
func readAgentsDoc(workDir string) string {
	if strings.TrimSpace(workDir) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(workDir, agentsDocFile))
	if err != nil {
		return ""
	}
	return string(data)
}

// soulFile is the conventional file holding Murtaugh's shared persona (name and
// personality, set up once). It lives in the config/workspace dir and is the
// single source of voice shared by native and ACP agents.
const soulFile = "SOUL.md"

// ReadSoul loads <dir>/SOUL.md when present. Best-effort: a missing or unreadable
// file yields "" (no persona), never an error. dir is the config/workspace dir
// (BaseDir), where the persona is set up — not the agent's per-agent workdir.
func ReadSoul(dir string) string {
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(dir, soulFile))
	if err != nil {
		return ""
	}
	return string(data)
}

// PrependPersona wraps soul in a <persona> block and prepends it to base,
// returning base unchanged when soul is empty. Used to fold the shared persona
// into the static system prompt (native) ahead of the harness code-of-conduct.
func PrependPersona(soul, base string) string {
	soul = strings.TrimSpace(soul)
	if soul == "" {
		return base
	}
	block := "<persona>\n" + soul + "\n</persona>"
	if strings.TrimSpace(base) == "" {
		return block
	}
	return block + "\n\n" + base
}

// renderSkillsIndex builds the compact "- name: description" listing of the
// agent's bundled skills, for the static system prompt. It lists only the skills
// the agent's `tools:` make visible (the L1 capability gate); filtering by the
// static profile tokens keeps the index stable per profile, so the system prompt
// stays cacheable. Returns "" when there are no visible skills or the directory
// is unreadable (the index is best-effort advertising, never a hard dependency).
func renderSkillsIndex(managed fs.FS, bespokeDir string, have []string) string {
	summaries, err := skills.ListVisible(managed, toolset.BespokeSkillsFS(bespokeDir), have)
	if err != nil || len(summaries) == 0 {
		return ""
	}
	var b strings.Builder
	for _, s := range summaries {
		if strings.TrimSpace(s.Description) != "" {
			fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
		} else {
			fmt.Fprintf(&b, "- %s\n", s.Name)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// containsString reports whether want appears in xs (trimmed).
func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if strings.TrimSpace(x) == want {
			return true
		}
	}
	return false
}

// MCPServerConfigs converts the global mcp_servers definitions into sorted,
// env-expanded mcpclient.ServerConfig values. Global servers are authoritative —
// every agent (native and the ACP aggregator) attaches all of them — so this
// takes the whole map rather than a per-agent selector. Sorted by name for
// deterministic ordering across runs.
func MCPServerConfigs(servers map[string]config.MCPServerConfig) []mcpclient.ServerConfig {
	if len(servers) == 0 {
		return nil
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]mcpclient.ServerConfig, 0, len(names))
	for _, name := range names {
		sc := servers[name]
		out = append(out, mcpclient.ServerConfig{
			Name:    name,
			Command: sc.Command,
			Args:    sc.Args,
			Env:     expandEnvMap(sc.Env),
			URL:     sc.URL,
		})
	}
	return out
}

// expandEnvMap expands ${VAR} references in an MCP server's env values against
// the process environment (the dotenv file is already loaded), returning nil for
// an empty map so callers can leave the inherited environment untouched.
func expandEnvMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = os.ExpandEnv(v)
	}
	return out
}

// Compile-time assertion that the native client satisfies the agent backend
// contract — the linchpin seam for the whole migration.
var _ agent.Client = (*Client)(nil)
