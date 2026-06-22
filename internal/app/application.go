// Package app is Murtaugh's composition root. It owns the tool registry,
// picks the frontend based on the parsed mode, and starts it. Frontends know
// nothing about each other; the application knows about all of them but
// delegates execution entirely.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agentdelegate"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/frontends/cli"
	"github.com/miere/murtaugh-dev-toolkit/internal/frontends/mcp"
	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
	gateway "github.com/miere/murtaugh-dev-toolkit/internal/slack/gateway"
	"github.com/miere/murtaugh-dev-toolkit/internal/slack/interaction"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/ask"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/jobs/define"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/jobs/run"
	journalprune "github.com/miere/murtaugh-dev-toolkit/internal/tools/journal/prune"
	journalquery "github.com/miere/murtaugh-dev-toolkit/internal/tools/journal/query"
	journalstats "github.com/miere/murtaugh-dev-toolkit/internal/tools/journal/stats"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/ping"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/plan"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/restart"
	setupagents "github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/agents"
	setupbootstrap "github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/bootstrap"
	setupenv "github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/env"
	setuplaunchd "github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/launchd"
	setupmcpregister "github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/mcpregister"
	setupslack "github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/slack"
	setupupdate "github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/update"
	slackcreatechannel "github.com/miere/murtaugh-dev-toolkit/internal/tools/slack/createchannel"
	slackfetchmsgs "github.com/miere/murtaugh-dev-toolkit/internal/tools/slack/fetchmsgs"
	slackfetchreactions "github.com/miere/murtaugh-dev-toolkit/internal/tools/slack/fetchreactions"
	slacksendmsg "github.com/miere/murtaugh-dev-toolkit/internal/tools/slack/sendmsg"
	slackupdatemsg "github.com/miere/murtaugh-dev-toolkit/internal/tools/slack/updatemsg"
	troubleshootbundle "github.com/miere/murtaugh-dev-toolkit/internal/tools/troubleshoot/bundle"
	"github.com/miere/murtaugh-dev-toolkit/internal/troubleshoot"
)

// Mode selects which frontend Run starts.
type Mode int

const (
	// ModeCLI runs the human-facing CLI frontend.
	ModeCLI Mode = iota
	// ModeMCP runs the MCP stdio server frontend.
	ModeMCP
	// ModeGateway runs the Slack gateway: the Socket Mode daemon started
	// by `murtaugh slack gateway`.
	ModeGateway
)

// Application is the composition root for a single murtaugh invocation. It
// is constructed once per process and reused for the lifetime of the chosen
// frontend.
type Application struct {
	mode       Mode
	args       []string
	cfg        config.Config
	configPath string
	version    string
	logger     *slog.Logger
	registry   *tools.Registry
	// interactionBroker backs the `ask` tool and is shared with the gateway so a
	// prompt's button click is routed back to the blocked turn. Constructed once
	// here; only the gateway wires it as the click router.
	interactionBroker *interaction.Broker
	// restart is the optional graceful-restart coordinator. Only the
	// gateway path attaches one; CLI and MCP modes leave it nil.
	restart *RestartCoordinator
	// resumeMarkerPath is the on-disk location of the cross-restart
	// marker. Empty disables the resume confirmation flow; the restart
	// still happens but no "back online" notice is posted.
	resumeMarkerPath string
	// configWatchPaths is the list of files whose mtime, when it
	// advances, makes the gateway suggest a restart to the
	// admin via Block Kit. Empty disables the watcher entirely.
	configWatchPaths []string
	// recorder is the journal recorder shared by the registry tools
	// (jobs.run) and the gateway domains. Never nil: main passes a no-op
	// recorder when the journal is disabled or could not be opened.
	recorder journal.Recorder
	// journalSweep runs one retention pass over the journal; journalSweepEvery
	// is its cadence. Only the gateway path consumes them (the daemon is the
	// single writer that may delete). nil disables the sweeper.
	journalSweep      func(context.Context) error
	journalSweepEvery time.Duration
	// jsonOutput requests JSONL output from the CLI frontend (global --json
	// flag). Irrelevant to MCP/Gateway modes.
	jsonOutput bool
}

// New constructs an Application for the given mode. cfg/configPath/logger
// come from the entry point so the same loaded state is shared across
// frontends. args is the list of positional arguments handed to the CLI
// frontend (Slack/MCP ignore it). version is the binary's compile-time
// version string (e.g. "v0.4.1" or "dev") and is consumed by setup.update.
func New(mode Mode, args []string, cfg config.Config, configPath, version string, logger *slog.Logger, recorder journal.Recorder) *Application {
	if recorder == nil {
		recorder = journal.NopRecorder{}
	}
	// The broker is shared between the `ask` tool (registered below) and the
	// gateway (which routes clicks back). Construct it first so both see the same
	// instance and its pending registry.
	broker := interaction.New(cfg.OAuth.BotToken)
	reg := buildRegistry(cfg, configPath, version, recorder, broker)
	return &Application{
		mode:              mode,
		args:              args,
		cfg:               cfg,
		configPath:        configPath,
		version:           version,
		logger:            logger,
		registry:          reg,
		interactionBroker: broker,
		recorder:          recorder,
	}
}

// Run starts the selected frontend and blocks until it returns. CLI and MCP
// share the same Registry; the gateway ignores the registry and starts the
// Socket Mode daemon directly.
func (a *Application) Run(ctx context.Context) error {
	switch a.mode {
	case ModeMCP:
		return mcp.New(a.registry).Serve(ctx)
	case ModeGateway:
		gw := gateway.New(a.cfg, a.registry, a.logger, a.recorder, a.interactionBroker)
		if rc := a.restart; rc != nil {
			// Adapt the coordinator's Request method into the gateway's
			// stringly-typed trigger so the gateway package stays free
			// of any internal/app import (which would cycle).
			gw = gw.WithRestartTrigger(func(source, userID, channel, reason string) bool {
				return rc.Request(RestartRequest{
					Source:  RestartSource(source),
					UserID:  userID,
					Channel: channel,
					Reason:  reason,
				})
			})
		}
		if path := strings.TrimSpace(a.resumeMarkerPath); path != "" {
			gw = gw.WithResumeMarkerStore(gateway.NewFileResumeMarkerStore(path))
			a.logger.Debug("resume marker store wired", "path", path)
		}
		if len(a.configWatchPaths) > 0 {
			gw = gw.WithConfigWatchPaths(a.configWatchPaths)
			a.logger.Debug("config watcher wired", "paths", a.configWatchPaths)
		}
		// Scheduled jobs reuse the jobs.run execution path so a cron/every
		// run behaves identically to a manual one (same timeout, workdir, and
		// exit-code handling). Output streams to the daemon's stdout/stderr,
		// which launchd captures into the Murtaugh log files.
		gw = gw.WithScheduledRunner(newScheduledRunner(a.cfg, a.recorder, a.registry))
		if a.journalSweep != nil {
			gw = gw.WithJournalSweeper(a.journalSweep, a.journalSweepEvery)
		}
		// `/murtaugh troubleshoot <symptoms>` assembles a redacted diagnostics
		// bundle and DMs it to the admin. The gateway owns Slack delivery; the
		// deterministic file assembly is this closure over the same bundler the
		// troubleshoot.bundle tool uses. Always attempts to include known
		// providers (e.g. Goose) — absent files are simply skipped.
		gw = gw.WithTroubleshootBundler(func(ctx context.Context, note string) (string, []string, error) {
			res, err := troubleshoot.Build(ctx, troubleshoot.Options{
				Note:      note,
				Providers: effectiveTroubleshootProviders(a.cfg),
			}, troubleshoot.ResolveSources(
				a.cfg.Journal.EffectivePath(),
				a.cfg.Journal.EffectiveBlobDir(),
				baseDirFor(a.cfg, a.configPath),
				a.version,
			))
			if err != nil {
				return "", nil, err
			}
			return res.Path, res.Manifest.Errors, nil
		})
		a.logger.Info("starting Slack gateway (Socket Mode)", "config", a.configPath)
		err := gw.Run(ctx)
		if err != nil && ctx.Err() != nil {
			err = nil
		}
		a.logger.Info("Slack gateway stopped")
		return err
	default:
		return cli.New(a.registry).WithJSON(a.jsonOutput).Run(ctx, a.args)
	}
}

// UsageLine renders a human-readable usage string built from the registered
// tools. Flat tool names (e.g. `ping`) are listed first; namespaced tools
// (e.g. `jobs.run`) are grouped by their namespace and rendered as
// `<ns> <sub>`. The `gateway` subcommand is injected into the `slack`
// namespace (it starts the daemon and is not a registry tool), and the
// built-in `mcp` mode (handled by main.go) is appended, so callers see
// every entry point in one line.
func (a *Application) UsageLine() string {
	var flat []string
	groups := map[string][]string{}
	var groupOrder []string

	for _, t := range a.registry.All() {
		name := t.Name()
		if i := strings.Index(name, "."); i >= 0 {
			ns, sub := name[:i], name[i+1:]
			if _, seen := groups[ns]; !seen {
				groupOrder = append(groupOrder, ns)
			}
			groups[ns] = append(groups[ns], sub)
			continue
		}
		flat = append(flat, name)
	}

	// `slack gateway` starts the Socket Mode daemon. It is not a registry
	// tool, so surface it as a slack subcommand alongside the slack.* tools.
	if _, seen := groups["slack"]; !seen {
		groupOrder = append(groupOrder, "slack")
	}
	groups["slack"] = append(groups["slack"], "gateway")

	parts := append([]string{}, flat...)
	for _, ns := range groupOrder {
		subs := groups[ns]
		sort.Strings(subs)
		parts = append(parts, fmt.Sprintf("%s <%s>", ns, strings.Join(subs, "|")))
	}
	parts = append(parts, "mcp")
	return "usage: murtaugh <command>; commands: " + strings.Join(parts, ", ") +
		"\nrun `murtaugh help` for full command docs, or `murtaugh help <command>` for one."
}

// SlackUsageLine renders the help shown for a bare `murtaugh slack`
// invocation: the `slack.*` tools (without their namespace prefix) plus the
// `gateway` daemon subcommand, sorted. It exists so the slack namespace lists
// its own subcommands instead of falling through to the generic CLI error.
func (a *Application) SlackUsageLine() string {
	subs := []string{"gateway"}
	for _, t := range a.registry.All() {
		if name := t.Name(); strings.HasPrefix(name, "slack.") {
			subs = append(subs, strings.TrimPrefix(name, "slack."))
		}
	}
	sort.Strings(subs)
	return "usage: murtaugh slack <subcommand>; subcommands: " + strings.Join(subs, ", ") +
		"\n  gateway starts the Slack Socket Mode daemon; the rest are one-shot tools." +
		"\n  run `murtaugh help slack <subcommand>` for flags and examples."
}

// Registry exposes the underlying registry. Intended for tests so the
// composition wiring can be inspected without standing up a frontend.
func (a *Application) Registry() *tools.Registry { return a.registry }

// WithJSONOutput toggles JSONL output for the CLI frontend (driven by the
// global --json flag). MCP and Gateway modes ignore it. Returns the receiver
// for fluent wiring.
func (a *Application) WithJSONOutput(on bool) *Application {
	a.jsonOutput = on
	return a
}

// WithRestartCoordinator attaches a restart coordinator to the application
// and returns the receiver to support a fluent wiring style at the entry
// point. Only the gateway path currently consumes the coordinator;
// other modes may safely skip this call.
func (a *Application) WithRestartCoordinator(rc *RestartCoordinator) *Application {
	a.restart = rc
	return a
}

// RestartCoordinator returns the attached coordinator, or nil if none was
// configured. Exposed so downstream frontends (Slack slash handler, Block
// Kit interactions) can locate the trigger without re-importing the
// composition root.
func (a *Application) RestartCoordinator() *RestartCoordinator { return a.restart }

// WithResumeMarkerPath configures the on-disk path where the Slack
// daemon persists its cross-restart marker. Empty disables the resume
// notice flow entirely. Returns the receiver for fluent wiring.
func (a *Application) WithResumeMarkerPath(path string) *Application {
	a.resumeMarkerPath = path
	return a
}

// WithConfigWatchPaths configures the list of files whose mtime, when
// it advances, makes the gateway ask the admin to confirm a
// restart via Block Kit. Empty disables the watcher entirely.
// Returns the receiver for fluent wiring.
func (a *Application) WithConfigWatchPaths(paths []string) *Application {
	a.configWatchPaths = paths
	return a
}

// WithJournalSweeper attaches the retention sweep (one pass) and its cadence,
// consumed only by the gateway path. Empty/nil disables the sweeper. Returns
// the receiver for fluent wiring.
func (a *Application) WithJournalSweeper(sweep func(context.Context) error, every time.Duration) *Application {
	a.journalSweep = sweep
	a.journalSweepEvery = every
	return a
}

// buildRegistry wires every tool Murtaugh ships with. New tools must be
// registered here so they appear in both the CLI and MCP frontends.
func buildRegistry(cfg config.Config, configPath, version string, recorder journal.Recorder, broker *interaction.Broker) *tools.Registry {
	reg := tools.NewRegistry()
	reg.Register(ping.New())

	jobsLookup := func(name string) (config.JobProfile, bool) {
		j, ok := cfg.Jobs[name]
		return j, ok
	}
	reg.Register(run.New(jobsLookup).WithDelegator(newJobDelegator(cfg, reg)).WithRecorder(recorder))

	// Journal read/maintenance tools open the event store on demand from the
	// configured path; one opener (carrying per-stream retention for prune)
	// backs all three. They are how Gateway Debug Mode and admins inspect and
	// trim the journal over CLI and MCP.
	journalOpener := func() (*journal.Store, error) {
		return journal.Open(cfg.Journal.EffectivePath(), cfg.Journal.RetentionByStream(),
			journal.WithBlobDir(cfg.Journal.EffectiveBlobDir()))
	}
	reg.Register(journalquery.New(journalOpener))
	reg.Register(journalstats.New(journalOpener))
	reg.Register(journalprune.New(journalOpener))

	jobsPath := func() string {
		baseDir := cfg.BaseDir
		if baseDir == "" && configPath != "" {
			baseDir = filepath.Dir(configPath)
		}
		if baseDir == "" {
			if home, err := os.UserHomeDir(); err == nil {
				baseDir = filepath.Join(home, ".config", "murtaugh")
			}
		}
		return filepath.Join(baseDir, "jobs.yaml")
	}
	reg.Register(define.New(jobsPath))

	bootstrapPath := func() string {
		if strings.TrimSpace(configPath) != "" {
			return configPath
		}
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, ".config", "murtaugh", "slack.yaml")
		}
		return ""
	}
	reg.Register(setupbootstrap.New(bootstrapPath))
	reg.Register(setupslack.New(bootstrapPath))

	agentsPath := func() string {
		if base := baseDirFor(cfg, configPath); base != "" {
			return filepath.Join(base, "agents.yaml")
		}
		return ""
	}
	reg.Register(setupagents.New(agentsPath))
	envPath := func() string {
		if base := baseDirFor(cfg, configPath); base != "" {
			return filepath.Join(base, ".env")
		}
		return ""
	}
	reg.Register(setupenv.New(envPath))
	troubleshootConfigPath := func() string {
		if base := baseDirFor(cfg, configPath); base != "" {
			return filepath.Join(base, "troubleshoot.yaml")
		}
		return ""
	}
	reg.Register(setupmcpregister.New(os.UserHomeDir, troubleshootConfigPath, troubleshoot.KnownProviders()))
	reg.Register(setuplaunchd.New(setuplaunchd.Deps{
		Home:      os.UserHomeDir,
		GOOS:      runtime.GOOS,
		Plutil:    execRunner,
		Launchctl: execRunner,
	}))
	reg.Register(setupupdate.New(setupupdate.Deps{
		CurrentVersion: func() string { return version },
		CurrentBinary:  os.Executable,
		GOOS:           runtime.GOOS,
		GOARCH:         runtime.GOARCH,
		HTTPGet:        setupupdate.HTTPGetter(),
		VerifyBinary:   verifyBinary,
		Owner:          "miere",
		Repo:           "murtaugh-dev-toolkit",
	}))

	// Slack tools share the daemon's bot token (oauth.bot_token in
	// slack.yaml). The client is built lazily on first Invoke, so an
	// unconfigured token only surfaces when a tool is actually called.
	botToken := cfg.OAuth.BotToken
	// send-msg can additionally post "as admin" via the admin's user token
	// (oauth.user_token); empty disables that path.
	reg.Register(slacksendmsg.New(botToken, cfg.OAuth.UserToken))
	reg.Register(slackcreatechannel.New(botToken))
	reg.Register(slackfetchmsgs.New(botToken))
	reg.Register(slackfetchreactions.New(botToken))
	reg.Register(slackupdatemsg.New(botToken))

	// `restart` only *requests* a restart: it posts the approval card the
	// gateway already understands. The real restart fires when the admin
	// confirms in Slack (or via the admin-only slash command), never from
	// this tool. With no channel it asks the configured admin in their DM.
	reg.Register(restart.New(botToken, cfg.Configuration.AdminUser))

	// `ask` lets an agent put a question with options to the user as clickable
	// Slack buttons and wait for the answer, instead of assuming one. It shares
	// the interaction broker with the gateway (which routes the click back). An
	// agent opts in by adding `ask` to its `tools:` list.
	reg.Register(ask.New(broker))

	// `present_plan` lets an agent lay a plan in front of the user with
	// Proceed / Revise / Cancel buttons and WAIT for sign-off before doing
	// multi-step work. It shares the same interaction broker as `ask`; an
	// agent opts in by adding `present_plan` to its `tools:` list.
	reg.Register(plan.New(broker))

	// `troubleshoot.bundle` assembles a redacted diagnostics zip. It resolves
	// its read paths (journal, blobs, config dir) from the loaded config on
	// every call, mirroring the journal/jobs path closures above.
	troubleshootSources := func() troubleshoot.Sources {
		return troubleshoot.ResolveSources(
			cfg.Journal.EffectivePath(),
			cfg.Journal.EffectiveBlobDir(),
			baseDirFor(cfg, configPath),
			version,
		)
	}
	reg.Register(troubleshootbundle.New(troubleshootSources, func() []string { return effectiveTroubleshootProviders(cfg) }))

	return reg
}

// effectiveTroubleshootProviders resolves which downstream providers a bundle
// should include by default: the set configured in troubleshoot.yaml (written
// by setup.mcp-register) when non-empty, otherwise every provider Murtaugh
// knows how to collect diagnostics for. Missing files are skipped at collection
// time, so the all-known fallback is safe on a machine that only runs some of
// them.
func effectiveTroubleshootProviders(cfg config.Config) []string {
	if len(cfg.Troubleshoot.Providers) > 0 {
		return cfg.Troubleshoot.Providers
	}
	return troubleshoot.KnownProviders()
}

// newJobDelegator builds the agent runner that backs agent-delegated jobs
// (jobs with `agent`/`prompt` instead of `command`). It returns nil when no
// agents are configured, leaving such jobs to fail with a clear error; config
// validation already guarantees a job's agent is defined when one is set.
func newJobDelegator(cfg config.Config, registry *tools.Registry) run.AgentDelegator {
	if len(cfg.Agents) == 0 {
		return nil
	}
	return agentdelegate.NewRunner(cfg.Agents, cfg.ACP, cfg.BaseDir, slog.Default()).
		WithBuildContext(registry, cfg.MCPServers)
}

// newScheduledRunner builds the executor the gateway scheduler uses to fire
// cron/every-scheduled jobs. It wraps the same jobs.run tool the CLI and MCP
// frontends use (streaming child output to the process stdout/stderr, which
// launchd captures), and maps a non-zero exit code onto an error so the
// gateway logs the run as failed.
func newScheduledRunner(cfg config.Config, recorder journal.Recorder, registry *tools.Registry) gateway.ScheduledRunner {
	lookup := func(name string) (config.JobProfile, bool) {
		j, ok := cfg.Jobs[name]
		return j, ok
	}
	runTool := run.New(lookup).WithDelegator(newJobDelegator(cfg, registry)).WithRecorder(recorder)
	return func(ctx context.Context, name string) error {
		result, err := runTool.Invoke(ctx, map[string]any{"name": name})
		if err != nil {
			return err
		}
		if r, ok := result.(run.Result); ok && r.ExitCode != 0 {
			return fmt.Errorf("exited with code %d", r.ExitCode)
		}
		return nil
	}
}

// verifyBinary runs `<path> version` to confirm the staged binary is
// executable on this host. A non-zero exit or unparseable output means we
// refuse to swap it into place.
func verifyBinary(path string) error {
	out, err := exec.Command(path, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s version: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) == "" {
		return fmt.Errorf("%s version produced no output", path)
	}
	return nil
}

// execRunner runs name with args, surfacing combined stdout/stderr only when
// the command fails so successful invocations stay quiet on the CLI.
func execRunner(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// baseDirFor resolves the config directory the same way jobsPath/bootstrapPath
// do: cfg.BaseDir wins, then filepath.Dir(configPath), then $HOME/.config/murtaugh.
func baseDirFor(cfg config.Config, configPath string) string {
	if cfg.BaseDir != "" {
		return cfg.BaseDir
	}
	if configPath != "" {
		return filepath.Dir(configPath)
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "murtaugh")
	}
	return ""
}
