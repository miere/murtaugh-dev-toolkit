package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/assets"
	"gopkg.in/yaml.v3"
)

const defaultRelativePath = ".config/murtaugh/slack.yaml"
const defaultAgentsRelativePath = ".config/murtaugh/agents.yaml"
const defaultJobsRelativePath = ".config/murtaugh/jobs.yaml"
const defaultJournalRelativePath = ".config/murtaugh/journal.yaml"

type Config struct {
	BaseDir       string                        `yaml:"-"`
	OAuth         OAuthConfig                   `yaml:"oauth"`
	Configuration ConfigurationConfig           `yaml:"configuration"`
	Chat          ChatConfig                    `yaml:"chat"`
	Defaults      RuntimeDefaults               `yaml:"-"`
	Agents        map[string]AgentProfile       `yaml:"-"`
	MCPServers    map[string]MCPServerConfig    `yaml:"-"`
	Jobs          map[string]JobProfile         `yaml:"-"`
	Journal       JournalConfig                 `yaml:"-"`
	Troubleshoot  TroubleshootConfig            `yaml:"-"`
	WorkflowRules map[string]WorkflowRuleConfig `yaml:"workflow-rules"`
	UnfurlRules   map[string]UnfurlRuleConfig   `yaml:"unfurl-rules"`
}

// TroubleshootConfig is the machine-managed troubleshoot.yaml sibling. It
// records which downstream providers' on-disk diagnostics the bundler should
// include by default. setup.mcp-register appends to Providers when it registers
// Murtaugh into a client that is also a known diagnostics provider (e.g. goose).
type TroubleshootConfig struct {
	Providers []string `yaml:"providers"`
}

type OAuthConfig struct {
	AppToken string `yaml:"app_token"`
	BotToken string `yaml:"bot_token"`
	// UserToken is the admin's Slack user token (xoxp-…) carrying the
	// user-scope chat:write. It is optional; when set it enables posting
	// "as admin" so a message shows the admin's real identity instead of
	// the app/bot.
	UserToken string `yaml:"user_token"`
}

type ConfigurationConfig struct {
	AdminUser    string   `yaml:"admin_user"`
	AllowedUsers []string `yaml:"allowed_users"`
	// DoNotRequireMentionFrom lists Slack users (IDs or handles) whose plain
	// channel messages the bot replies to WITHOUT an @mention. It waives the
	// mention requirement only — a listed user must still pass IsAllowedUser. The
	// gateway startup layer resolves any handle entries to IDs (see
	// resolveAllowSet) so the runtime check is ID-only.
	DoNotRequireMentionFrom []string `yaml:"do_not_require_mention_from"`
	Debug                   bool     `yaml:"debug"`
}

type ChatConfig struct {
	// Enabled gates the Slack chat surface: DM and @mention replies. It does NOT
	// gate agent delegation (jobs, workflow rules, unfurls) — those run whenever
	// the target agent is defined, independent of the chat surface.
	Enabled bool `yaml:"enabled"`
	// ChannelAgents routes a channel to a specific agent. Each key is either an
	// exact Slack channel ID (C…/G…, for back-compat) or a channel-NAME glob
	// that may contain `*` (e.g. "feature-*", "*-prod"), matched against the
	// channel's name. The value is the agent name. Precedence on a match is
	// exact-ID, then exact-name, then longest-literal-prefix glob (see
	// gateway.matchChannelAgent).
	ChannelAgents map[string]string `yaml:"channel_agents"`
	// ChannelDoNotRequireMention lists, per channel, the Slack users (IDs or
	// handles) whose plain messages the bot replies to without an @mention. The
	// keys use the SAME channel-ID/channel-NAME glob syntax as ChannelAgents
	// (e.g. "feature-*"); the effective no-mention set for a channel is the union
	// of configuration.do_not_require_mention_from and the values of every
	// pattern whose glob matches the channel. It waives the mention requirement
	// only — listed users must still pass IsAllowedUser.
	ChannelDoNotRequireMention map[string][]string `yaml:"channel_do_not_require_mention"`
	DMAgent                    string              `yaml:"dm_agent"`
	DefaultAgent               string              `yaml:"default_agent"`
}

// RuntimeDefaults are the agent-runtime defaults applied to every agent, split
// by the concern each knob serves. Parsed from the agents.yaml `defaults:` block.
type RuntimeDefaults struct {
	Session   SessionDefaults   `yaml:"session"`
	Rendering RenderingDefaults `yaml:"rendering"`
	ACP       ACPDefaults       `yaml:"acp"`
	// Approval is the global default approval policy, overridden per agent.
	Approval ApprovalConfig `yaml:"approval"`
}

// SessionDefaults tune chat-session lifecycle (both backends).
type SessionDefaults struct {
	IdleTimeout string `yaml:"idle_timeout"`
	// RequestTimeout bounds a chat turn by INACTIVITY, not total wall-clock: the
	// timer resets on every chunk or task update the agent emits, so a long turn
	// that keeps making progress is never killed mid-flight. Only an agent that
	// goes silent for this long is treated as stalled.
	RequestTimeout string `yaml:"request_timeout"`
	MaxConcurrent  int    `yaml:"max_concurrent"`
}

// RenderingDefaults tune how a streaming turn renders in Slack (both backends).
type RenderingDefaults struct {
	// ProgressDisplay is the default rendering for tool/step progress across all
	// agents. Empty means simplified. Per-agent profiles may override it.
	ProgressDisplay      string `yaml:"progress_display"`
	StreamMinChunkChars  int    `yaml:"stream_min_chunk_chars"`
	StreamAppendInterval string `yaml:"stream_append_interval"`
}

// ACPDefaults tune the ACP child-process lifecycle (native agents ignore these).
type ACPDefaults struct {
	StartupTimeout    string `yaml:"startup_timeout"`
	CancelGracePeriod string `yaml:"cancel_grace_period"`
}

// ProgressDisplay selects how an agent's tool/step progress renders in Slack
// while a turn is streaming.
type ProgressDisplay string

const (
	// ProgressDisplaySimplified collapses progress into a single, last-write-wins
	// status line that resolves to a check when the turn ends. It is the default:
	// non-intrusive, ideal when only the outcome matters.
	ProgressDisplaySimplified ProgressDisplay = "simplified"
	// ProgressDisplayTasks keeps the full multi-card task list grouped under a
	// Plan block — useful for coding sessions where watching the plan is the point.
	ProgressDisplayTasks ProgressDisplay = "tasks"
)

// AgentKind selects which backend drives an agent profile.
type AgentKind string

const (
	// AgentKindNative is the in-process LLM agent loop (the default). It talks
	// to a provider directly via internal/llm and owns the conversation array.
	AgentKindNative AgentKind = "native"
	// AgentKindACP is the legacy external-process backend driven over ACP
	// (kind: acp). Requires Command.
	AgentKindACP AgentKind = "acp"
)

// ApprovalConfig holds how an agent's tool-call approvals are handled. It spans
// both backends under one block: Terminal/Allow govern the NATIVE terminal-tool
// gate, while Requests governs how an ACP agent's own permission prompts are
// answered. A field for the wrong backend is simply unused, never an error.
type ApprovalConfig struct {
	// Terminal selects the gating posture for the native terminal tool:
	//   "allowlist" (default) — auto-run a recognized read-only command; ask for
	//                           anything else (fail closed)
	//   "prompt"              — ask before every terminal command
	//   "off"                 — never ask (the pre-gate behaviour)
	// Gating is only active in a Slack chat (where there is a human to ask);
	// headless runs (scheduled jobs, delegated agents) are never gated.
	Terminal string `yaml:"terminal"`
	// Allow extends the built-in read-only allowlist with extra command keys:
	// an argv0 ("kubectl") or a "binary subcommand" pair ("docker ps").
	Allow []string `yaml:"allow"`
	// Requests governs how an ACP agent's own permission requests
	// (session/request_permission) are answered: "ask" (default — route to a
	// human in the Slack thread), "auto-allow", or "auto-deny". Headless/CLI
	// callers have no human, so "ask" there denies; set "auto-allow" for
	// unattended ACP automation. Unused by native agents.
	Requests string `yaml:"requests"`
}

// AgentProfile defines one agent. Shared knobs live at the top; the backend is
// selected by which sub-block is present — exactly one of Native or ACP. There
// is no separate `kind`: a profile carrying a `native:` block is a native
// agent, one carrying an `acp:` block is an ACP agent (see ResolvedKind).
type AgentProfile struct {
	// WorkDir roots the agent: the files/terminal tools for a native agent, or
	// the spawned process's cwd for an ACP agent.
	WorkDir string `yaml:"workdir"`
	// Tools is the allowlist of registry/native tool groups exposed to this
	// agent (e.g. "files", "terminal", "skills", "slack", "jobs"). Empty means
	// no tools beyond the always-on set the toolset resolver decides.
	Tools []string `yaml:"tools"`
	// ExportSkillsToFS lists bundled (murtaugh-*) skills to write into this
	// agent's workdir so an external, filesystem-discovering agent (e.g. a
	// Claude-based ACP backend) can load them. Empty (the default) keeps the
	// bundled skills in-binary only — readable solely through the gated `skills`
	// tool, never by the file/terminal tools. The sentinel "all" exports every
	// bundled skill. The list is the source of truth: on each build, listed
	// skills are (re)written and any previously-exported murtaugh-* skill not
	// listed is removed (bespoke skills are never touched). Exporting a skill
	// opts it out of the in-binary blind for this agent.
	ExportSkillsToFS []string `yaml:"export_skills_to_fs"`
	// MCPServers names additional MCP servers (defined in the top-level
	// mcp_servers block) to attach to this agent on top of the authoritative
	// global set. Empty attaches just the global set; names must exist.
	MCPServers []string `yaml:"mcp_servers"`
	// Approval governs tool-call approvals for whichever backend applies (the
	// terminal gate for native, request answering for ACP). Empty defaults to
	// allowlist (native gating on) / ask (ACP).
	Approval ApprovalConfig `yaml:"approval"`
	// ProgressDisplay overrides the global rendering default for this agent.
	// Empty inherits it (which itself defaults to simplified).
	ProgressDisplay string `yaml:"progress_display"`

	// Native carries the in-process backend config; non-nil selects kind native.
	Native *NativeProfile `yaml:"native"`
	// ACP carries the external-process backend config; non-nil selects kind acp.
	ACP *ACPProfile `yaml:"acp"`
}

// NativeProfile is the in-process LLM backend config (the `native:` sub-block).
type NativeProfile struct {
	// Provider selects the litellm provider family: "gemini", "anthropic"
	// (Anthropic-compatible, incl. base_url overrides), or "openai"
	// (OpenAI-compatible, incl. GLM/DeepSeek/Kimi via base_url).
	Provider string `yaml:"provider"`
	// Model is the provider model id (e.g. "gemini-2.5-pro", "glm-4.6").
	Model string `yaml:"model"`
	// BaseURL overrides the provider endpoint for compatible third parties
	// (Z.ai, DeepSeek, Kimi, self-hosted). Empty uses the provider default.
	BaseURL string `yaml:"base_url"`
	// APIKeyEnv names the environment variable (loaded from ~/.config/murtaugh/.env)
	// holding the provider credential. The key value itself never lives in YAML.
	APIKeyEnv string `yaml:"api_key_env"`
	// SystemPrompt is the inline system prompt. Mutually exclusive with
	// SystemPromptFile; when both are empty the loop uses a built-in default.
	SystemPrompt string `yaml:"system_prompt"`
	// SystemPromptFile is a path (resolved against the config dir) to a file
	// holding the system prompt. Mutually exclusive with SystemPrompt.
	SystemPromptFile string `yaml:"system_prompt_file"`
	// MaxTurns bounds tool-call iterations in a single prompt. 0 uses a default.
	MaxTurns int `yaml:"max_turns"`
	// ContextLimit is the conversation token budget that drives compaction. 0
	// uses a per-provider-family default. The loop compacts the message array
	// before a turn would exceed this.
	ContextLimit int `yaml:"context_limit"`
	// Compaction selects how the conversation is kept within ContextLimit:
	// "truncate" (default — drop oldest turn-groups) or "summarize" (LLM-compress
	// the oldest groups, with truncation as the fallback). Empty means truncate.
	Compaction string `yaml:"compaction"`
	// CacheRetention overrides the prompt-cache TTL: "5m" (default) or "1h";
	// "off"/"none" disables caching. Empty uses the default. Applied for
	// Anthropic/OpenAI; Gemini caches a static prefix implicitly regardless.
	CacheRetention string `yaml:"cache_retention"`
}

// ACPProfile is the external-process backend config (the `acp:` sub-block).
type ACPProfile struct {
	// Command and Args launch the ACP agent process. Command is required.
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	// Interruptible overrides auto-detection of session/cancel support. When
	// nil (the default) Murtaugh probes the agent at warmup; set it explicitly
	// to skip the probe or to correct a wrong verdict.
	Interruptible *bool `yaml:"interruptible"`
	// Env injects environment variables into the agent process. Each value is
	// expanded against Murtaugh's own environment first (so "${HOME}/bin" and
	// "$PATH" resolve), then the resulting KEY=VALUE pairs are layered on top of
	// the inherited environment — the agent sees Murtaugh's env plus these, with
	// these winning on a duplicate key. Empty (the default) leaves the inherited
	// environment untouched.
	Env map[string]string `yaml:"env"`
}

// EnvOverrides renders the ACP profile's Env map into the KEY=VALUE slice exec
// expects, expanding each value against the host environment. It returns nil
// when no variables are configured (or the profile is not ACP) so callers can
// leave cmd.Env unset and keep the plain inherited environment. Blank keys are
// skipped; keys are emitted in sorted order so the result is deterministic.
func (p AgentProfile) EnvOverrides() []string {
	if p.ACP == nil || len(p.ACP.Env) == 0 {
		return nil
	}
	env := p.ACP.Env
	keys := make([]string, 0, len(env))
	for key := range env {
		if strings.TrimSpace(key) == "" {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+os.ExpandEnv(env[key]))
	}
	return out
}

type JobProfile struct {
	Command string   `yaml:"command"`
	Args    []string `yaml:"args"`
	WorkDir string   `yaml:"workdir"`
	Timeout string   `yaml:"timeout"`
	// Agent and Prompt turn the job into an agent delegation: instead of
	// running Command, Murtaugh starts the named agent in an isolated one-shot
	// session and sends the rendered Prompt. Mutually exclusive with Command.
	// The prompt supports positional placeholders ({{ 1 }}, {{ 2 }}, ...) that
	// expand to the runtime args passed to `jobs run`.
	Agent  string `yaml:"agent"`
	Prompt string `yaml:"prompt"`
	// Schedule, when set, runs the job automatically on a cron schedule
	// using standard 5-field cron syntax (e.g. "0 2 * * *" for 02:00 daily).
	// Mutually exclusive with Every.
	Schedule string `yaml:"schedule"`
	// Every, when set, runs the job automatically at a fixed interval
	// expressed as a Go duration (e.g. "1h", "30m"). Mutually exclusive with
	// Schedule. When both Schedule and Every are empty the job is
	// manual-only: it runs solely when invoked via jobs.run or a workflow.
	Every string `yaml:"every"`
	// Confirmed tracks whether a job may auto-run. nil = operator-defined/trusted
	// (runs). A non-nil false marks an agent-defined job awaiting first-run
	// confirmation (held, not auto-run). true = confirmed. Uses a pointer so an
	// absent field (hand-written jobs) is distinguishable from an explicit false.
	Confirmed *bool `yaml:"confirmed"`
}

// AwaitingConfirmation reports whether the job is an agent-defined job that has
// not yet been confirmed for its first run. A nil Confirmed (hand-written or
// operator-trusted job) and an explicit true both return false; only a non-nil
// false — the stamp jobs.define applies — returns true. The gateway scheduler
// uses this to hold such jobs back from auto-running.
func (p JobProfile) AwaitingConfirmation() bool {
	return p.Confirmed != nil && !*p.Confirmed
}

// ScheduleKind classifies how a job is triggered.
type ScheduleKind int

const (
	// ScheduleManual is a job with neither schedule nor every set: it runs
	// only on explicit invocation (jobs.run, MCP, or a workflow trigger).
	ScheduleManual ScheduleKind = iota
	// ScheduleCron is a job driven by a cron expression (Schedule).
	ScheduleCron
	// ScheduleEvery is a job driven by a fixed interval duration (Every).
	ScheduleEvery
)

// ScheduleKind reports how the job is triggered. Schedule takes precedence
// over Every if both are set, but Validate rejects that combination so the
// ambiguity never reaches a running scheduler.
func (p JobProfile) ScheduleKind() ScheduleKind {
	switch {
	case strings.TrimSpace(p.Schedule) != "":
		return ScheduleCron
	case strings.TrimSpace(p.Every) != "":
		return ScheduleEvery
	default:
		return ScheduleManual
	}
}

type WorkflowRuleConfig struct {
	RequestEvent string          `yaml:"request_event"`
	Match        map[string]any  `yaml:"match"`
	Triggers     []TriggerConfig `yaml:"trigger"`
}

type TriggerConfig struct {
	Type            string
	ReplyToSlack    *ReplyToSlackTriggerConfig
	Run             *RunTriggerConfig
	DelegateToAgent *DelegateToAgentConfig
}

type ReplyToSlackTriggerConfig struct {
	Template        string                 `yaml:"template"`
	Run             *RunTriggerConfig      `yaml:"run"`
	DelegateToAgent *DelegateToAgentConfig `yaml:"delegate-to-agent"`
}

// DelegateToAgentConfig hands work to an agent in an isolated one-shot session.
// Where it sits decides how its output is treated: nested in a reply-to-slack
// trigger or an unfurl action, the agent's final output must be a valid JSON
// Slack message and is rendered; as a top-level workflow trigger it is
// fire-and-forget (the agent acts through its own tools). The prompt is
// rendered with the same template data the surrounding surface's templates
// receive (the interaction Payload for workflow rules, the URL/Captures for
// unfurls).
type DelegateToAgentConfig struct {
	Agent  string `yaml:"agent"`
	Prompt string `yaml:"prompt"`
}

type RunTriggerConfig struct {
	Cmd     string   `yaml:"cmd"`
	Args    []string `yaml:"args"`
	Timeout string   `yaml:"timeout"`
	WorkDir string   `yaml:"workdir"`
}

type UnfurlRuleConfig struct {
	Match  UnfurlMatchConfig  `yaml:"match"`
	Unfurl UnfurlActionConfig `yaml:"unfurl"`
}

type UnfurlMatchConfig struct {
	Channels   []string `yaml:"channels"`
	Domain     string   `yaml:"domain"`
	URLPrefix  string   `yaml:"url_prefix"`
	URLPattern string   `yaml:"url_pattern"`
}

type UnfurlActionConfig struct {
	Template        string                 `yaml:"template"`
	Run             *RunTriggerConfig      `yaml:"run"`
	DelegateToAgent *DelegateToAgentConfig `yaml:"delegate-to-agent"`
}

func (t *TriggerConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode || len(value.Content) != 2 {
		return errors.New("trigger must be a mapping with exactly one action")
	}

	action := value.Content[0].Value
	switch action {
	case "reply-to-slack":
		var cfg ReplyToSlackTriggerConfig
		if err := value.Content[1].Decode(&cfg); err != nil {
			return err
		}
		t.Type = action
		t.ReplyToSlack = &cfg
	case "run":
		var cfg RunTriggerConfig
		if err := value.Content[1].Decode(&cfg); err != nil {
			return err
		}
		t.Type = action
		t.Run = &cfg
	case "delegate-to-agent":
		var cfg DelegateToAgentConfig
		if err := value.Content[1].Decode(&cfg); err != nil {
			return err
		}
		t.Type = action
		t.DelegateToAgent = &cfg
	default:
		return fmt.Errorf("unsupported trigger action %q", action)
	}
	return nil
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, defaultRelativePath), nil
}

func DefaultAgentsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, defaultAgentsRelativePath), nil
}

func DefaultJobsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, defaultJobsRelativePath), nil
}

func DefaultJournalPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}
	return filepath.Join(home, defaultJournalRelativePath), nil
}

func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}
	cfg, err := Parse(data)
	if err != nil {
		return Config{}, err
	}
	cfg.BaseDir = filepath.Dir(path)

	// Load <config-dir>/.env before expanding any ${VAR} references so that
	// credentials live only in the dotenv file (or the ambient environment),
	// never in the YAML the troubleshoot bundler ships. A missing .env is fine.
	if err := LoadDotEnv(cfg.BaseDir); err != nil {
		return Config{}, err
	}
	// Slack tokens are referenced from slack.yaml as ${VAR}; expand them against
	// the now-loaded environment. A literal token (no $) expands to itself, so
	// pre-.env configs keep working.
	cfg.OAuth.AppToken = os.ExpandEnv(cfg.OAuth.AppToken)
	cfg.OAuth.BotToken = os.ExpandEnv(cfg.OAuth.BotToken)
	cfg.OAuth.UserToken = os.ExpandEnv(cfg.OAuth.UserToken)

	agentsPath := filepath.Join(cfg.BaseDir, "agents.yaml")
	agentsData, err := os.ReadFile(agentsPath)
	if err == nil {
		var agents struct {
			Defaults   RuntimeDefaults            `yaml:"defaults"`
			Agents     map[string]AgentProfile    `yaml:"agents"`
			MCPServers map[string]MCPServerConfig `yaml:"mcp_servers"`
		}
		if err := yaml.Unmarshal(agentsData, &agents); err != nil {
			return Config{}, fmt.Errorf("parse agents config %q: %w", agentsPath, err)
		}
		cfg.Defaults = agents.Defaults
		cfg.Agents = agents.Agents
		cfg.MCPServers = agents.MCPServers
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read agents config %q: %w", agentsPath, err)
	}

	jobsPath := filepath.Join(cfg.BaseDir, "jobs.yaml")
	jobsData, err := os.ReadFile(jobsPath)
	if err == nil {
		var jobs struct {
			Jobs map[string]JobProfile `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(jobsData, &jobs); err != nil {
			return Config{}, fmt.Errorf("parse jobs config %q: %w", jobsPath, err)
		}
		cfg.Jobs = jobs.Jobs
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read jobs config %q: %w", jobsPath, err)
	}

	journalPath := filepath.Join(cfg.BaseDir, "journal.yaml")
	journalData, err := os.ReadFile(journalPath)
	if err == nil {
		var journal struct {
			Journal JournalConfig `yaml:"journal"`
		}
		if err := yaml.Unmarshal(journalData, &journal); err != nil {
			return Config{}, fmt.Errorf("parse journal config %q: %w", journalPath, err)
		}
		cfg.Journal = journal.Journal
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read journal config %q: %w", journalPath, err)
	}

	troubleshootPath := filepath.Join(cfg.BaseDir, "troubleshoot.yaml")
	troubleshootData, err := os.ReadFile(troubleshootPath)
	if err == nil {
		var ts struct {
			Troubleshoot TroubleshootConfig `yaml:"troubleshoot"`
		}
		if err := yaml.Unmarshal(troubleshootData, &ts); err != nil {
			return Config{}, fmt.Errorf("parse troubleshoot config %q: %w", troubleshootPath, err)
		}
		cfg.Troubleshoot = ts.Troubleshoot
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read troubleshoot config %q: %w", troubleshootPath, err)
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	// Bake the global defaults.approval into each agent so every downstream
	// consumer sees the resolved (per-agent over default) policy.
	for name, p := range cfg.Agents {
		p.Approval = cfg.EffectiveApproval(p)
		cfg.Agents[name] = p
	}
	return cfg, nil
}

func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	return cfg, nil
}

func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.OAuth.AppToken) == "" {
		errs = append(errs, errors.New("oauth.app_token is required"))
	}
	if strings.TrimSpace(c.OAuth.BotToken) == "" {
		errs = append(errs, errors.New("oauth.bot_token is required"))
	}
	if err := c.Journal.Validate(); err != nil {
		errs = append(errs, err)
	}
	for i, allowed := range c.Configuration.AllowedUsers {
		if strings.TrimSpace(allowed) == "" {
			errs = append(errs, fmt.Errorf("configuration.allowed_users[%d] must not be blank", i))
		}
	}
	if err := c.Defaults.Validate(); err != nil {
		errs = append(errs, err)
	}
	for name, profile := range c.Agents {
		if err := validateProgressDisplay(fmt.Sprintf("agents[%s].progress_display", name), profile.ProgressDisplay); err != nil {
			errs = append(errs, err)
		}
		if profile.ACP != nil {
			for key := range profile.ACP.Env {
				if strings.ContainsRune(key, '=') {
					errs = append(errs, fmt.Errorf("agents[%s].acp.env key %q must not contain '='", name, key))
				}
			}
		}
		errs = append(errs, validateExportSkills(name, profile.ExportSkillsToFS)...)
		// Exactly one backend sub-block must be present; a clear error beats a
		// silent backend flip or a half-configured agent.
		switch {
		case profile.Native != nil && profile.ACP != nil:
			errs = append(errs, fmt.Errorf("agents[%s] sets both native and acp; use exactly one", name))
		case profile.Native != nil:
			errs = append(errs, validateNativeAgent(name, profile, c.MCPServers)...)
		case profile.ACP != nil:
			if err := profile.Validate(); err != nil {
				errs = append(errs, fmt.Errorf("agents[%s]: %w", name, err))
			}
		default:
			errs = append(errs, fmt.Errorf("agents[%s] needs a native or acp block", name))
		}
		errs = append(errs, validateMCPRefs(name, profile.MCPServers, c.MCPServers)...)
	}

	for name, server := range c.MCPServers {
		if err := validateMCPServer(name, server); err != nil {
			errs = append(errs, err)
		}
	}

	if c.Chat.Enabled {
		if len(c.Agents) == 0 {
			errs = append(errs, errors.New("chat is enabled but no agents are defined in agents.yaml"))
		}
		if strings.TrimSpace(c.Chat.DefaultAgent) == "" {
			errs = append(errs, errors.New("chat.default_agent is required when chat is enabled"))
		} else if _, ok := c.Agents[c.Chat.DefaultAgent]; !ok {
			errs = append(errs, fmt.Errorf("chat.default_agent %q not found in agents.yaml", c.Chat.DefaultAgent))
		}
		if c.Chat.DMAgent != "" {
			if _, ok := c.Agents[c.Chat.DMAgent]; !ok {
				errs = append(errs, fmt.Errorf("chat.dm_agent %q not found in agents.yaml", c.Chat.DMAgent))
			}
		}
		for channel, agent := range c.Chat.ChannelAgents {
			if _, ok := c.Agents[agent]; !ok {
				errs = append(errs, fmt.Errorf("chat.channel_agents[%s] references unknown agent %q", channel, agent))
			}
			// Keys may be exact channel IDs (C…/G…) or channel-NAME globs that
			// contain `*` (e.g. "feature-*"). A glob is matched via path.Match at
			// runtime, so reject a malformed pattern here rather than letting it
			// silently never match.
			if strings.ContainsRune(channel, '*') {
				if _, err := path.Match(channel, "probe"); err != nil {
					errs = append(errs, fmt.Errorf("chat.channel_agents[%s] is not a valid channel-name glob: %w", channel, err))
				}
			}
		}
		// channel_do_not_require_mention shares the channel_agents key syntax, so
		// validate its glob keys the same way; the user lists themselves need no
		// validation (a stray entry simply never matches an author ID).
		for channel := range c.Chat.ChannelDoNotRequireMention {
			if strings.ContainsRune(channel, '*') {
				if _, err := path.Match(channel, "probe"); err != nil {
					errs = append(errs, fmt.Errorf("chat.channel_do_not_require_mention[%s] is not a valid channel-name glob: %w", channel, err))
				}
			}
		}
	}

	for name, job := range c.Jobs {
		hasCommand := strings.TrimSpace(job.Command) != ""
		hasAgent := strings.TrimSpace(job.Agent) != ""
		hasPrompt := strings.TrimSpace(job.Prompt) != ""
		switch {
		case hasCommand && (hasAgent || hasPrompt):
			errs = append(errs, fmt.Errorf("jobs[%s] sets both command and agent/prompt; use exactly one", name))
		case hasCommand:
			// Plain command job: nothing more to check here.
		case hasAgent || hasPrompt:
			if !hasAgent {
				errs = append(errs, fmt.Errorf("jobs[%s].agent is required when prompt is set", name))
			}
			if !hasPrompt {
				errs = append(errs, fmt.Errorf("jobs[%s].prompt is required when agent is set", name))
			}
			if hasAgent {
				if _, ok := c.Agents[job.Agent]; !ok {
					errs = append(errs, fmt.Errorf("jobs[%s].agent references unknown agent %q", name, job.Agent))
				}
			}
		default:
			errs = append(errs, fmt.Errorf("jobs[%s] requires either command or agent + prompt", name))
		}
		if job.Timeout != "" {
			if _, err := time.ParseDuration(job.Timeout); err != nil {
				errs = append(errs, fmt.Errorf("jobs[%s].timeout must be a valid duration: %w", name, err))
			}
		}
		if strings.TrimSpace(job.Schedule) != "" && strings.TrimSpace(job.Every) != "" {
			errs = append(errs, fmt.Errorf("jobs[%s] sets both schedule and every; use exactly one", name))
		}
		if every := strings.TrimSpace(job.Every); every != "" {
			if d, err := time.ParseDuration(every); err != nil {
				errs = append(errs, fmt.Errorf("jobs[%s].every must be a valid duration: %w", name, err))
			} else if d <= 0 {
				errs = append(errs, fmt.Errorf("jobs[%s].every must be greater than zero", name))
			}
		}
	}

	for name, rule := range c.WorkflowRules {
		if strings.TrimSpace(rule.RequestEvent) != "interactive" {
			errs = append(errs, fmt.Errorf("workflow-rules[%s].request_event must be interactive", name))
		}
		if len(rule.Match) == 0 {
			errs = append(errs, fmt.Errorf("workflow-rules[%s].match is required", name))
		}
		if len(rule.Triggers) == 0 {
			errs = append(errs, fmt.Errorf("workflow-rules[%s].trigger must contain at least one action", name))
		}
		for i, trigger := range rule.Triggers {
			if err := validateTrigger(trigger, c.Agents); err != nil {
				errs = append(errs, fmt.Errorf("workflow-rules[%s].trigger[%d]: %w", name, i, err))
			}
		}
	}
	for name, rule := range c.UnfurlRules {
		if err := validateUnfurlRule(rule, c.Agents); err != nil {
			errs = append(errs, fmt.Errorf("unfurl-rules[%s]: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// IsAllowedUser reports whether the given Slack user ID is permitted to
// interact directly with the bot via slash commands, mentions, or DMs.
//
// The check is ID-only: a user is allowed when their ID matches
// configuration.admin_user (only when admin_user is configured as a Slack
// user ID, not a handle) or any entry in configuration.allowed_users. The
// gateway startup layer is responsible for resolving any handle entries in
// allowed_users to IDs before this helper is consulted, and for separately
// checking against the resolved admin user ID when admin_user is a handle.
//
// With a tight default, an empty allowed_users list means only the admin user
// may interact with the bot.
func (c ConfigurationConfig) IsAllowedUser(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	admin := strings.TrimPrefix(strings.TrimSpace(c.AdminUser), "@")
	if looksLikeSlackUserID(admin) && admin == userID {
		return true
	}
	for _, allowed := range c.AllowedUsers {
		if strings.TrimSpace(allowed) == userID {
			return true
		}
	}
	return false
}

// IsAdminUser reports whether the given Slack user ID matches the
// resolved configuration.admin_user. Like IsAllowedUser this is ID-only,
// so admin_user must already have been resolved from a handle to a Slack
// user ID (gateway.resolveAllowSet does this at daemon start). A blank
// or handle-shaped admin_user will never match.
func (c ConfigurationConfig) IsAdminUser(userID string) bool {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return false
	}
	admin := strings.TrimPrefix(strings.TrimSpace(c.AdminUser), "@")
	return looksLikeSlackUserID(admin) && admin == userID
}

func looksLikeSlackUserID(value string) bool {
	if len(value) < 4 {
		return false
	}
	if !(strings.HasPrefix(value, "U") || strings.HasPrefix(value, "W")) {
		return false
	}
	for _, r := range value[1:] {
		if !((r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

func (c RuntimeDefaults) Validate() error {
	var errs []error
	for field, value := range map[string]string{
		"defaults.acp.startup_timeout":              c.ACP.StartupTimeout,
		"defaults.session.request_timeout":          c.Session.RequestTimeout,
		"defaults.session.idle_timeout":             c.Session.IdleTimeout,
		"defaults.rendering.stream_append_interval": c.Rendering.StreamAppendInterval,
		"defaults.acp.cancel_grace_period":          c.ACP.CancelGracePeriod,
	} {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, err := time.ParseDuration(value); err != nil {
			errs = append(errs, fmt.Errorf("%s must be a valid duration: %w", field, err))
		}
	}
	if c.Session.MaxConcurrent < 0 {
		errs = append(errs, errors.New("defaults.session.max_concurrent must be greater than or equal to zero"))
	}
	if c.Rendering.StreamMinChunkChars < 0 {
		errs = append(errs, errors.New("defaults.rendering.stream_min_chunk_chars must be greater than or equal to zero"))
	}
	if err := validateProgressDisplay("defaults.rendering.progress_display", c.Rendering.ProgressDisplay); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Validate checks an ACP profile (callers gate this on p.ACP != nil).
func (p AgentProfile) Validate() error {
	if p.ACP == nil {
		return nil
	}
	if strings.TrimSpace(p.ACP.Command) == "" {
		return errors.New("agent acp.command is required")
	}
	switch p.ResolvedACPPermission() {
	case "ask", "auto-allow", "auto-deny":
	default:
		return fmt.Errorf("agent approval.requests must be ask, auto-allow, or auto-deny (got %q)", p.Approval.Requests)
	}
	return nil
}

// ResolvedACPPermission reports the effective permission policy for an ACP agent,
// defaulting an empty value to "ask".
func (p AgentProfile) ResolvedACPPermission() string {
	if v := strings.ToLower(strings.TrimSpace(p.Approval.Requests)); v != "" {
		return v
	}
	return "ask"
}

// ResolvedKind reports the backend selected by the present sub-block: an `acp:`
// block is ACP, otherwise native. (Validate enforces exactly one sub-block, so
// the native default only applies to an already-validated native profile.)
func (p AgentProfile) ResolvedKind() AgentKind {
	if p.ACP != nil {
		return AgentKindACP
	}
	return AgentKindNative
}

// MCPServerConfig describes one external MCP server the native agent can attach
// to. Exactly one transport is used: a stdio child process (Command/Args/Env)
// or a remote endpoint (URL). The full wiring + validation lands in T5/T6; this
// is the config contract Wave-1 tasks share.
type MCPServerConfig struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	URL     string            `yaml:"url"`
}

// EffectiveProgressDisplay resolves how the given agent's progress renders:
// the agent profile's setting wins, then the global rendering default, then
// simplified. Unknown values are rejected at load time (Validate), so this
// only ever observes valid or empty strings.
func (c Config) EffectiveProgressDisplay(agent string) ProgressDisplay {
	if p, ok := c.Agents[agent]; ok {
		if m := normalizeProgressDisplay(p.ProgressDisplay); m != "" {
			return m
		}
	}
	if m := normalizeProgressDisplay(c.Defaults.Rendering.ProgressDisplay); m != "" {
		return m
	}
	return ProgressDisplaySimplified
}

// EffectiveApproval resolves an agent's approval policy: each field of the
// per-agent block wins when set, falling back to the global defaults.approval.
func (c Config) EffectiveApproval(profile AgentProfile) ApprovalConfig {
	out := c.Defaults.Approval
	if t := strings.TrimSpace(profile.Approval.Terminal); t != "" {
		out.Terminal = profile.Approval.Terminal
	}
	if len(profile.Approval.Allow) > 0 {
		out.Allow = profile.Approval.Allow
	}
	if r := strings.TrimSpace(profile.Approval.Requests); r != "" {
		out.Requests = profile.Approval.Requests
	}
	return out
}

// normalizeProgressDisplay maps a raw config string to a known mode, or "" when
// it is blank/unrecognised (callers treat "" as "inherit"/"default").
func normalizeProgressDisplay(s string) ProgressDisplay {
	switch ProgressDisplay(strings.ToLower(strings.TrimSpace(s))) {
	case ProgressDisplaySimplified:
		return ProgressDisplaySimplified
	case ProgressDisplayTasks:
		return ProgressDisplayTasks
	default:
		return ""
	}
}

// validateProgressDisplay rejects a non-empty progress_display value that is
// not one of the known modes. Empty is always allowed (it means "inherit").
func validateProgressDisplay(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if normalizeProgressDisplay(value) == "" {
		return fmt.Errorf("%s must be %q or %q", field, ProgressDisplaySimplified, ProgressDisplayTasks)
	}
	return nil
}

func (c RuntimeDefaults) EffectiveStartupTimeout() time.Duration {
	return durationOrDefault(c.ACP.StartupTimeout, 10*time.Second)
}

// EffectiveRequestTimeout is the per-turn idle timeout: the longest a chat turn
// may go with no agent activity before it is treated as stalled. It is reset by
// every event, so it bounds inactivity rather than total turn duration.
func (c RuntimeDefaults) EffectiveRequestTimeout() time.Duration {
	return durationOrDefault(c.Session.RequestTimeout, 10*time.Minute)
}

func (c RuntimeDefaults) EffectiveSessionIdleTimeout() time.Duration {
	return durationOrDefault(c.Session.IdleTimeout, 30*time.Minute)
}

func (c RuntimeDefaults) EffectiveStreamAppendInterval() time.Duration {
	return durationOrDefault(c.Rendering.StreamAppendInterval, 250*time.Millisecond)
}

func (c RuntimeDefaults) EffectiveMaxSessions() int {
	if c.Session.MaxConcurrent > 0 {
		return c.Session.MaxConcurrent
	}
	return 100
}

func (c RuntimeDefaults) EffectiveStreamMinChunkChars() int {
	if c.Rendering.StreamMinChunkChars > 0 {
		return c.Rendering.StreamMinChunkChars
	}
	return 24
}

func (c RuntimeDefaults) EffectiveCancelGracePeriod() time.Duration {
	return durationOrDefault(c.ACP.CancelGracePeriod, 2*time.Second)
}

func durationOrDefault(value string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return duration
}

func validateTrigger(trigger TriggerConfig, agents map[string]AgentProfile) error {
	switch trigger.Type {
	case "reply-to-slack":
		if trigger.ReplyToSlack == nil {
			return errors.New("reply-to-slack config is required")
		}
		rts := trigger.ReplyToSlack
		hasTemplate := strings.TrimSpace(rts.Template) != ""
		hasRun := rts.Run != nil
		hasDelegate := rts.DelegateToAgent != nil
		if countTrue(hasTemplate, hasRun, hasDelegate) != 1 {
			return errors.New("reply-to-slack requires exactly one of template, run, or delegate-to-agent")
		}
		if hasRun {
			return validateRun(*rts.Run)
		}
		if hasDelegate {
			return validateDelegate(rts.DelegateToAgent, agents)
		}
	case "run":
		if trigger.Run == nil {
			return errors.New("run config is required")
		}
		return validateRun(*trigger.Run)
	case "delegate-to-agent":
		return validateDelegate(trigger.DelegateToAgent, agents)
	default:
		return fmt.Errorf("unsupported trigger action %q", trigger.Type)
	}
	return nil
}

func validateRun(run RunTriggerConfig) error {
	if strings.TrimSpace(run.Cmd) == "" {
		return errors.New("cmd is required")
	}
	return nil
}

// validateDelegate checks a delegate-to-agent block: it needs both an agent and
// a prompt, and the agent must be defined in agents.yaml.
func validateDelegate(d *DelegateToAgentConfig, agents map[string]AgentProfile) error {
	if d == nil {
		return errors.New("delegate-to-agent config is required")
	}
	if strings.TrimSpace(d.Agent) == "" {
		return errors.New("delegate-to-agent requires an agent")
	}
	if strings.TrimSpace(d.Prompt) == "" {
		return errors.New("delegate-to-agent requires a prompt")
	}
	if _, ok := agents[d.Agent]; !ok {
		return fmt.Errorf("delegate-to-agent references unknown agent %q", d.Agent)
	}
	return nil
}

// exportSkillsAll is the sentinel that exports every bundled skill to the
// agent's workdir.
const exportSkillsAll = "all"

// validateExportSkills checks an agent's export_skills_to_fs list: every entry
// must be the sentinel "all" or the name of a bundled skill. Fail-closed — an
// unknown name is a config error, so a typo can't silently export nothing (or
// the wrong thing).
func validateExportSkills(agent string, list []string) []error {
	if len(list) == 0 {
		return nil
	}
	known := make(map[string]bool)
	for _, n := range assets.SkillNames() {
		known[n] = true
	}
	var errs []error
	for i, raw := range list {
		s := strings.TrimSpace(raw)
		if s == "" {
			errs = append(errs, fmt.Errorf("agents[%s].export_skills_to_fs[%d] must not be blank", agent, i))
			continue
		}
		if s == exportSkillsAll || known[s] {
			continue
		}
		errs = append(errs, fmt.Errorf("agents[%s].export_skills_to_fs[%d]: unknown skill %q (valid: %q or one of %s)",
			agent, i, s, exportSkillsAll, strings.Join(assets.SkillNames(), ", ")))
	}
	return errs
}

// nativeProviders is the set of litellm provider families a native agent may
// select. Compatible third parties (Z.ai/GLM, DeepSeek, Kimi) ride the
// anthropic or openai families via a base_url override.
var nativeProviders = map[string]struct{}{"gemini": {}, "anthropic": {}, "openai": {}}

// validateNativeAgent checks a native profile: it needs a known provider,
// a model, and an api_key_env; system_prompt and system_prompt_file are mutually
// exclusive; the terminal-approval mode must be known.
func validateNativeAgent(name string, p AgentProfile, servers map[string]MCPServerConfig) []error {
	var errs []error
	n := p.Native
	provider := strings.ToLower(strings.TrimSpace(n.Provider))
	if provider == "" {
		errs = append(errs, fmt.Errorf("agents[%s].native.provider is required", name))
	} else if _, ok := nativeProviders[provider]; !ok {
		errs = append(errs, fmt.Errorf("agents[%s].native.provider %q must be one of gemini, anthropic, openai", name, n.Provider))
	}
	if strings.TrimSpace(n.Model) == "" {
		errs = append(errs, fmt.Errorf("agents[%s].native.model is required", name))
	}
	if strings.TrimSpace(n.APIKeyEnv) == "" {
		errs = append(errs, fmt.Errorf("agents[%s].native.api_key_env is required (the .env variable holding the credential)", name))
	}
	if strings.TrimSpace(n.SystemPrompt) != "" && strings.TrimSpace(n.SystemPromptFile) != "" {
		errs = append(errs, fmt.Errorf("agents[%s].native sets both system_prompt and system_prompt_file; use exactly one", name))
	}
	if n.ContextLimit < 0 {
		errs = append(errs, fmt.Errorf("agents[%s].native.context_limit must be greater than or equal to zero", name))
	}
	switch strings.ToLower(strings.TrimSpace(n.Compaction)) {
	case "", "truncate", "summarize":
	default:
		errs = append(errs, fmt.Errorf("agents[%s].native.compaction must be %q or %q", name, "truncate", "summarize"))
	}
	switch strings.ToLower(strings.TrimSpace(n.CacheRetention)) {
	case "", "off", "none", "5m", "short", "1h", "long":
	default:
		errs = append(errs, fmt.Errorf("agents[%s].native.cache_retention must be one of 5m, 1h, or off (got %q)", name, n.CacheRetention))
	}
	switch strings.ToLower(strings.TrimSpace(p.Approval.Terminal)) {
	case "", "allowlist", "prompt", "off":
	default:
		errs = append(errs, fmt.Errorf("agents[%s].approval.terminal must be one of allowlist, prompt, or off (got %q)", name, p.Approval.Terminal))
	}
	return errs
}

// validateMCPRefs checks that every per-agent mcp_servers entry names a defined
// server. The list is additive on top of the authoritative global set, so it is
// validated for both backends.
func validateMCPRefs(name string, refs []string, servers map[string]MCPServerConfig) []error {
	var errs []error
	for _, ref := range refs {
		if _, ok := servers[ref]; !ok {
			errs = append(errs, fmt.Errorf("agents[%s].mcp_servers references unknown server %q (define it under mcp_servers)", name, ref))
		}
	}
	return errs
}

// validateMCPServer requires exactly one transport: a stdio child process
// (command) or a remote endpoint (url).
func validateMCPServer(name string, s MCPServerConfig) error {
	hasCommand := strings.TrimSpace(s.Command) != ""
	hasURL := strings.TrimSpace(s.URL) != ""
	if hasCommand == hasURL {
		return fmt.Errorf("mcp_servers[%s] requires exactly one of command or url", name)
	}
	for key := range s.Env {
		if strings.ContainsRune(key, '=') {
			return fmt.Errorf("mcp_servers[%s].env key %q must not contain '='", name, key)
		}
	}
	return nil
}

func countTrue(vals ...bool) int {
	n := 0
	for _, v := range vals {
		if v {
			n++
		}
	}
	return n
}

func validateUnfurlRule(rule UnfurlRuleConfig, agents map[string]AgentProfile) error {
	var errs []error
	match := rule.Match
	if strings.TrimSpace(match.Domain) == "" &&
		strings.TrimSpace(match.URLPrefix) == "" &&
		strings.TrimSpace(match.URLPattern) == "" {
		errs = append(errs, errors.New("match requires at least one of domain, url_prefix, url_pattern"))
	}
	if pattern := strings.TrimSpace(match.URLPattern); pattern != "" {
		if _, err := regexp.Compile(pattern); err != nil {
			errs = append(errs, fmt.Errorf("match.url_pattern is not a valid regexp: %w", err))
		}
	}
	for i, channel := range match.Channels {
		if strings.TrimSpace(channel) == "" {
			errs = append(errs, fmt.Errorf("match.channels[%d] must not be blank", i))
		}
	}
	hasTemplate := strings.TrimSpace(rule.Unfurl.Template) != ""
	hasRun := rule.Unfurl.Run != nil
	hasDelegate := rule.Unfurl.DelegateToAgent != nil
	if countTrue(hasTemplate, hasRun, hasDelegate) != 1 {
		errs = append(errs, errors.New("unfurl requires exactly one of template, run, or delegate-to-agent"))
	}
	if hasRun {
		if err := validateRun(*rule.Unfurl.Run); err != nil {
			errs = append(errs, err)
		}
	}
	if hasDelegate {
		if err := validateDelegate(rule.Unfurl.DelegateToAgent, agents); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
