package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
	"github.com/miere/murtaugh-dev-toolkit/internal/agent/native"
	"github.com/miere/murtaugh-dev-toolkit/internal/agentbuild"
	"github.com/miere/murtaugh-dev-toolkit/internal/agentdelegate"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
	"github.com/miere/murtaugh-dev-toolkit/internal/mcpbridge"
	slackclient "github.com/miere/murtaugh-dev-toolkit/internal/slack/client"
	askbroker "github.com/miere/murtaugh-dev-toolkit/internal/slack/interaction"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
	"github.com/miere/murtaugh-dev-toolkit/internal/unfurl"
	"github.com/miere/murtaugh-dev-toolkit/internal/workflow"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

// workflowDispatcher is the minimal surface needed to dispatch an interactive
// callback to a workflow engine. *workflow.Engine satisfies it.
type workflowDispatcher interface {
	Execute(ctx context.Context, interaction slack.InteractionCallback, rawPayload []byte) error
}

// RestartTrigger is the function the Gateway calls to request a graceful
// restart. The arguments mirror internal/app.RestartRequest field-by-field
// but stay stringly-typed so gateway does not need to import the
// composition root (which would create a cycle). Returns true when the
// shutdown sequence has begun, false when the request was declined
// (already firing, within cool-down, or no coordinator is wired).
type RestartTrigger func(source, userID, channel, reason string) bool

// TroubleshootBundler assembles a diagnostics bundle described by note and
// returns the path to the written zip plus any non-fatal collection warnings.
// The gateway owns Slack delivery; the composition root supplies this closure
// over the deterministic troubleshoot bundler. nil disables the
// `/murtaugh troubleshoot` slash command.
type TroubleshootBundler func(ctx context.Context, note string) (zipPath string, warnings []string, err error)

type Gateway struct {
	api      userDirectoryAPI
	socket   *socketmode.Client
	handler  SlashCommandHandler
	workflow workflowDispatcher
	// interactions routes broker prompt clicks (the `ask` tool, later the
	// approval gate) back to the blocked turn. nil leaves broker prompts
	// unrouted (CLI/MCP, or a gateway built without it).
	interactions *askbroker.Broker
	// bridge is the shared per-agent MCP aggregator. ACP agents are handed a
	// `murtaugh mcp-bridge` stdio server that proxies to it, so they can reach
	// Murtaugh's own tools. nil when ACP chat is disabled. Started in Run.
	bridge          *mcpbridge.Server
	chat            *ChatHandler
	chatSessions    map[string]ChatSessionManager
	chatWarmTimeout time.Duration
	// chatRouting and agentProfiles are config snapshots captured at construction
	// so the startup routing summary (logStartupRouting) can report the configured
	// agents and channel routing — and flag routes whose target agent failed to
	// build — without re-reading the full config.
	chatRouting   config.ChatConfig
	agentProfiles map[string]config.AgentProfile
	// cancelGrace is how long the interrupt path waits after asking the
	// ACP agent to cancel its in-flight prompt before hard-cancelling the
	// chat goroutine's context. Short enough that the user does not stare
	// at a stalled "_interrupted_" marker, long enough that trailing
	// chunks already on the wire can flush. Defaults to 2s via
	// ACPConfig.EffectiveCancelGracePeriod.
	cancelGrace time.Duration
	inFlight    *InFlightRegistry
	// recentEvents suppresses duplicate Slack event deliveries so a
	// redelivered message does not spawn a second chat that interrupts the
	// first. nil disables de-duplication (CLI/MCP and most tests).
	recentEvents    *eventDedup
	unfurl          *LinkUnfurlHandler
	unfurlTimeout   time.Duration
	startupNotifier StartupNotifier
	logger          *slog.Logger
	// cfg holds the configuration values consulted at runtime. Authz entries
	// (admin_user, allowed_users) start out as configured (IDs or handles) and
	// are mutated in place by resolveAllowSet at the start of Run so the rest
	// of the Gateway can rely on ID-only comparisons via cfg.IsAllowedUser.
	cfg config.AccessConfig
	// restart is the optional graceful-restart trigger. nil in CLI/MCP and
	// in tests that do not need to exercise the restart path; the slash
	// handler reports "not available" when nil.
	restart RestartTrigger
	// resumeStore persists the restart marker between processes. nil
	// disables the "restarting…" / "back online" Slack confirmation flow
	// — the restart still happens, just silently.
	resumeStore ResumeMarkerStore
	// messaging is the Slack surface used by the resume helpers. Set to
	// the same *slack.Client as api in New; kept as a separate field so
	// tests can substitute a narrow fake without re-implementing the
	// full Slack client.
	messaging slackMessagingAPI
	// connectHandled flips to true the first time the connect-time greeting
	// runs after a successful socket connect. Slack may emit multiple
	// EventTypeConnected events across the daemon's life (re-connects, flaky
	// links); we only want to greet — resume notice or startup ping — once per
	// process. See notifyConnected.
	connectHandled bool
	// configWatchPaths lists files whose mtime, when it advances,
	// triggers a restart suggestion to the admin. Empty (the default)
	// disables the watcher entirely. The composition root populates
	// this from the loaded config's sibling files (slack.yaml,
	// agents.yaml, jobs.yaml).
	configWatchPaths []string
	// scheduledJobs is the job set captured from the loaded config at
	// construction. The scheduler registers the entries whose ScheduleKind
	// is cron/every; manual jobs are ignored. Empty disables scheduling.
	scheduledJobs map[string]config.JobProfile
	// confirmedJobs records which held (agent-defined, unconfirmed) jobs have
	// been approved for their first run during this process. Session-scoped and
	// guarded by confirmedJobsMu; not persisted, so a restart re-asks.
	confirmedJobs   map[string]bool
	confirmedJobsMu sync.Mutex
	// runJob executes a job by name to completion. Injected by the
	// composition root (WithScheduledRunner) as a closure over the jobs.run
	// tool. nil disables the scheduler, so CLI/MCP and tests never pay for
	// it.
	runJob ScheduledRunner
	// recorder receives gateway-stream journal events for inbound interactions
	// (slash commands, interactive callbacks) and is threaded into the workflow
	// engine and unfurl handler. Never nil after New: a nil argument becomes a
	// no-op recorder so call sites never branch.
	recorder journal.Recorder
	// troubleshoot assembles a diagnostics bundle for the
	// `/murtaugh troubleshoot` slash command. nil disables the command.
	troubleshoot TroubleshootBundler
	// botToken is the bot OAuth token, retained so the troubleshoot handler can
	// build a Slack client that uploads the bundle to the admin DM (the narrow
	// api/messaging interfaces deliberately do not expose file upload).
	botToken string
	// journalSweep runs one retention pass over the journal; journalSweepEvery
	// is its cadence. Wired by the composition root (WithJournalSweeper) as a
	// closure over the daemon's store. nil disables the sweeper, so CLI/MCP and
	// tests never start it.
	journalSweep      func(context.Context) error
	journalSweepEvery time.Duration
	// webClient is the concrete Slack Web API client. It backs both the
	// active connection heartbeat (auth.test) and the construction of fresh
	// socketmode clients on every (re)connect. The Web API is stateless HTTP
	// and never goes "zombie", so the same client is reused across reconnects.
	// nil in struct-literal test gateways, which never run the supervisor.
	webClient *slack.Client
	// connMu guards socket across the supervisor's reconnects: the supervisor
	// swaps in a fresh socketmode.Client per attempt while the ack path reads
	// the current one. The single *socketmode.Client field is otherwise written
	// once at construction.
	connMu sync.Mutex
	// lastActivityNano is the UnixNano of the most recent inbound socketmode
	// event of ANY kind. The watchdog reads it to detect a half-open websocket
	// (the daemon believes it is connected but no frames arrive); the event loop
	// stamps it. Atomic so the watchdog goroutine reads it without the lock.
	lastActivityNano atomic.Int64
	// now supplies the current time; overridable in tests. nil ⇒ time.Now.
	now func() time.Time
	// channelCache maps Slack channel IDs to names so the chat resolver can
	// route channel→agent by NAME glob (chat.channel_agents) without doing any
	// Slack API I/O on the socket goroutine. Warmed at startup and refreshed on
	// a ticker by startChannelCache; nil disables name-based routing (only the
	// exact channel-ID keys still match), so CLI/MCP and tests never pay for it.
	channelCache *channelNameCache
	// noMentionPerChannel maps a channel-ID/channel-NAME glob (same key syntax as
	// chat.channel_agents) to the Slack user IDs whose plain channel messages the
	// bot replies to without an @mention. Captured from cfg.Chat at construction;
	// any handle entries are resolved to IDs by resolveAllowSet at the start of
	// Run, so the runtime membership test in handleEventsAPI is ID-only. The
	// effective no-mention set for a channel is the UNION of noMentionEverywhere
	// and the values of every pattern whose glob matches the channel.
	noMentionPerChannel map[string][]string
	// noMentionEverywhere is the global no-mention list (chat.no_mention.everywhere).
	// Captured from cfg.Chat at construction and resolved to IDs by resolveAllowSet.
	noMentionEverywhere []string
}

func New(cfg config.Config, registry *tools.Registry, logger *slog.Logger, recorder journal.Recorder, broker *askbroker.Broker) *Gateway {
	if logger == nil {
		logger = slog.Default()
	}
	if recorder == nil {
		recorder = journal.NopRecorder{}
	}
	api := slack.New(cfg.OAuth.BotToken, slack.OptionAppLevelToken(cfg.OAuth.AppToken))
	socket := socketmode.New(api, socketmode.OptionDebug(cfg.Access.Debug))
	startupNotifier, err := NewSlackStartupNotifier(api, cfg.Access.AdminUser, logger)
	if err != nil {
		logger.Error("startup Slack ping disabled", "error", err)
	}
	// The channel-name cache backs name-glob routing in chat.channel_agents. It
	// needs a SlackAPI with ListChannels (the narrow socket api/webClient do
	// not expose it), so build a Web API client over the bot token. A build
	// failure (empty token — already rejected by config validation) just leaves
	// the cache nil, degrading to exact channel-ID matching.
	var channelCache *channelNameCache
	if channelAPI, err := slackclient.NewClient(cfg.OAuth.BotToken); err != nil {
		logger.Warn("channel-name routing disabled: could not build Slack client", "error", err)
	} else {
		channelCache = newChannelNameCache(channelAPI, 30*time.Second, logger)
	}

	var chat *ChatHandler
	var sessions map[string]ChatSessionManager
	if !cfg.Chat.Enabled {
		logger.Warn("chat disabled: set chat.enabled: true to enable DM and app_mention replies (delegation still runs)")
	}
	var bridge *mcpbridge.Server
	if cfg.Chat.Enabled {
		sessions = make(map[string]ChatSessionManager)
		// The aggregator lets ACP agents reach Murtaugh's own tools over a private
		// socket; built here, bound and torn down in Run. ACP agents that fail to
		// reach it simply get no Murtaugh tools.
		bridge = mcpbridge.NewServer(bridgeSocketPath(), logger)
		// Chat agents are gated: a side-effecting tool call asks the user for
		// approval in the thread. nil broker leaves them ungated. Headless and
		// delegated agents (built elsewhere) never get an approver.
		var approver native.Approver
		var acpPermissionAsker agent.PermissionAsker
		if broker != nil {
			approver = askbroker.NewApprover(broker)
			// ACP agents answer their own permission requests through the same
			// broker: a session/request_permission is posted as buttons in the
			// thread. nil (headless) leaves ACP agents to their auto-allow/deny
			// policy. Set only on this interactive path, like the native approver.
			acpPermissionAsker = askbroker.NewPermissionGate(broker)
		}
		for name, profile := range cfg.Agents {
			// Default an agent's working directory to the workspace (the
			// config dir, e.g. ~/.config/murtaugh) when it leaves workdir
			// unset, so agents start where the templates live.
			agentWorkDir := strings.TrimSpace(profile.WorkDir)
			if agentWorkDir == "" {
				agentWorkDir = cfg.BaseDir
			}
			// Mirror the bundled skills this agent opted to export into its
			// workdir so a filesystem-discovering backend can load them; the
			// default (empty) leaves them in-binary only. Non-fatal: a failure
			// just means no filesystem skills for this agent.
			if exported, err := config.ReconcileExportedSkills(agentWorkDir, profile.ExportSkillsToFS); err != nil {
				logger.Warn("skill export failed", "agent", name, "error", err)
			} else if len(exported) > 0 {
				logger.Info("exported bundled skills to workdir", "agent", name, "skills", exported, "dir", filepath.Join(agentWorkDir, ".agents", "skills"))
			}
			client, err := agentbuild.Client(profile, agentbuild.Deps{
				Registry:           registry,
				MCPServers:         cfg.MCPServers,
				BaseDir:            cfg.BaseDir,
				Logger:             logger.With("agent", name),
				Approver:           approver,
				ACPPermissionAsker: acpPermissionAsker,
				Bridge:             bridge,
			})
			if err != nil {
				logger.Error("agent disabled: could not build client", "agent", name, "kind", profile.ResolvedKind(), "error", err)
				continue
			}
			var interruptible *bool
			if profile.ACP != nil {
				interruptible = profile.ACP.Interruptible
			}
			sessions[name] = agent.NewSessionManager(
				client,
				cfg.Defaults.EffectiveSessionIdleTimeout(),
				cfg.Defaults.EffectiveMaxSessions(),
			).WithLogger(logger.With("agent", name)).WithCancelOverride(interruptible)
		}

		// The resolver runs on the Slack socket goroutine, so it must not do any
		// Slack API I/O: channel→agent routing consults the in-memory
		// channelCache (ID→name) and the pure matchChannelAgent matcher. A name
		// the cache has not learned yet (a brand-new channel) falls back to the
		// default agent and triggers a non-blocking refresh so the NEXT message
		// can match by name.
		resolver := func(req ChatRequest) string {
			if req.DM {
				if cfg.Chat.DMAgent != "" {
					return cfg.Chat.DMAgent
				}
				return cfg.Chat.DefaultAgent
			}
			channelName, known := channelCache.nameFor(req.ChannelID)
			if !known {
				channelCache.refreshAsync(context.Background())
			}
			if agent, ok := matchChannelAgent(req.ChannelID, channelName, cfg.Chat.ChannelAgents); ok {
				return agent
			}
			return cfg.Chat.DefaultAgent
		}

		// Record chat turns to the acp_session stream only when it is enabled,
		// so a disabled stream writes neither rows nor transcript files.
		var sessionLog *sessionLogger
		if cfg.Journal.EffectiveEnabled(journal.StreamACPSession) {
			sessionLog = newSessionLogger(recorder, cfg.Journal.EffectiveBlobDir(), logger)
		}
		// Resolve this bot's own Slack user id once so thread backfill can mark
		// the agent's prior replies as its own. Best-effort: a failed auth.test
		// only costs the "(you)" tagging, not the backfill itself.
		var botUserID string
		authCtx, cancelAuth := context.WithTimeout(context.Background(), 10*time.Second)
		if resp, err := api.AuthTestContext(authCtx); err != nil {
			logger.Warn("auth.test failed; thread backfill will not tag the bot's own replies", "error", err)
		} else {
			botUserID = resp.UserID
		}
		cancelAuth()

		chat = NewChatHandler(
			api,
			sessions,
			resolver,
			cfg.Defaults.EffectiveStreamAppendInterval(),
			cfg.Defaults.EffectiveStreamMinChunkChars(),
			logger,
		).WithIdleTimeout(cfg.Defaults.EffectiveRequestTimeout()).WithSessionLogger(sessionLog).
			WithProgressDisplay(cfg.EffectiveProgressDisplay).WithStatusMessenger(api).
			WithBackfiller(NewThreadBackfiller(api, botUserID, logger)).
			WithFileFetcher(api).
			WithUploader(slackAttachmentUploader{api: api})
	}
	// One shared runner backs every delegate-to-agent surface (jobs, workflow
	// triggers, unfurls). Each delegation spins its own isolated agent process,
	// so this is safe to share. Built only when agents are configured; config
	// validation guarantees any delegate-to-agent rule names a known agent.
	var unfurlDelegator UnfurlDelegator
	var workflowDelegator workflow.AgentDelegator
	if len(cfg.Agents) > 0 {
		runner := agentdelegate.NewRunner(cfg.Agents, cfg.Defaults, cfg.BaseDir, logger).
			WithBuildContext(registry, cfg.MCPServers)
		unfurlDelegator = runner
		workflowDelegator = runner
	}
	var unfurlHandler *LinkUnfurlHandler
	if len(cfg.UnfurlRules) > 0 {
		matcher, err := unfurl.NewMatcher(cfg.UnfurlRules)
		if err != nil {
			logger.Error("custom link unfurling disabled", "error", err)
		} else {
			renderer := unfurl.NewRenderer(cfg.BaseDir, nil)
			unfurlHandler = NewLinkUnfurlHandler(matcher, renderer, nil, unfurlDelegator, api, logger).WithRecorder(recorder)
		}
	}
	return &Gateway{
		api:             api,
		webClient:       api,
		socket:          socket,
		now:             time.Now,
		handler:         NewDefaultSlashCommandHandler(),
		workflow:        workflow.NewEngine(cfg, workflow.Options{Logger: logger, Delegator: workflowDelegator, Recorder: recorder}),
		interactions:    broker,
		bridge:          bridge,
		chat:            chat,
		chatSessions:    sessions,
		chatRouting:     cfg.Chat,
		agentProfiles:   cfg.Agents,
		chatWarmTimeout: cfg.Defaults.EffectiveStartupTimeout(),
		cancelGrace:     cfg.Defaults.EffectiveCancelGracePeriod(),
		inFlight:        NewInFlightRegistry(),
		recentEvents:    newEventDedup(0),
		unfurl:          unfurlHandler,
		unfurlTimeout:   2 * time.Minute,
		startupNotifier: startupNotifier,
		logger:          logger,
		cfg:             cfg.Access,
		messaging:       api,
		scheduledJobs:   cfg.Jobs,
		recorder:        recorder,
		botToken:        cfg.OAuth.BotToken,
		channelCache:    channelCache,
		// Captured here so the no-mention check in handleEventsAPI runs without
		// re-importing the full cfg.
		noMentionPerChannel: cfg.Chat.NoMention.ByChannel,
		noMentionEverywhere: cfg.Chat.NoMention.Everywhere,
	}
}

// record emits a gateway-stream journal event, stamping the correlation id
// carried on ctx (minted at interaction ingress). A nil recorder (struct-literal
// Gateways in tests) is a no-op, matching how the gateway treats its other
// optional collaborators.
func (a *Gateway) record(ctx context.Context, kind string, level journal.Level, summary string, keys journal.Keys, payload any) {
	if a.recorder == nil {
		return
	}
	a.recorder.Record(ctx, journal.Event{
		Stream:  journal.StreamGateway,
		Kind:    kind,
		Level:   level,
		Summary: summary,
		CorrID:  journal.CorrIDFromContext(ctx),
		Keys:    keys,
		Payload: payload,
	})
}

// WithResumeMarkerStore attaches the persistent store used to bridge
// restart notices across process restarts. When nil (the default) the
// restart flow runs silently — the "restarting…" / "back online"
// confirmation messages are skipped.
func (a *Gateway) WithResumeMarkerStore(store ResumeMarkerStore) *Gateway {
	a.resumeStore = store
	return a
}

// WithRestartTrigger attaches the graceful-restart trigger and returns the
// receiver for fluent wiring at the composition root. When nil (the
// default) the restart slash verb reports the feature as unavailable.
func (a *Gateway) WithRestartTrigger(trigger RestartTrigger) *Gateway {
	a.restart = trigger
	return a
}

// WithConfigWatchPaths attaches the list of on-disk files whose mtime
// changes should produce a restart suggestion to the admin. Blank
// entries are filtered out; an empty list disables the watcher
// entirely (which is the default, so CLI/MCP modes and tests never
// pay for the polling goroutine).
func (a *Gateway) WithConfigWatchPaths(paths []string) *Gateway {
	cleaned := make([]string, 0, len(paths))
	for _, p := range paths {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	a.configWatchPaths = cleaned
	return a
}

// WithScheduledRunner attaches the executor used to run cron/every-scheduled
// jobs and returns the receiver for fluent wiring. When nil (the default) the
// scheduler is disabled entirely, so CLI/MCP modes and tests never start it.
func (a *Gateway) WithScheduledRunner(runner ScheduledRunner) *Gateway {
	a.runJob = runner
	return a
}

// WithJournalSweeper attaches the retention sweep and its cadence. The sweep
// runs once at startup and then every interval, inside the daemon (the single
// writer that may delete). nil disables it, so CLI/MCP and tests never sweep.
// Returns the receiver for fluent wiring.
func (a *Gateway) WithJournalSweeper(sweep func(context.Context) error, every time.Duration) *Gateway {
	a.journalSweep = sweep
	a.journalSweepEvery = every
	return a
}

// WithTroubleshootBundler attaches the diagnostics-bundle assembler that backs
// the `/murtaugh troubleshoot` slash command. nil (the default) disables the
// command. Returns the receiver for fluent wiring.
func (a *Gateway) WithTroubleshootBundler(bundler TroubleshootBundler) *Gateway {
	a.troubleshoot = bundler
	return a
}

func (a *Gateway) Run(ctx context.Context) error {
	resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := a.resolveAllowSet(resolveCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("resolve allowed users: %w", err)
	}

	a.startBridge(ctx)
	a.logStartupRouting(ctx)
	a.warmChat(ctx)
	a.startChannelCache(ctx)
	a.startConfigWatcher(ctx)
	a.startJournalSweeper(ctx)
	stopScheduler := a.startScheduler(ctx)
	defer stopScheduler()

	// The Slack socket is owned by a supervisor that reconnects on failure and
	// recycles a wedged (half-open) connection via a heartbeat watchdog, rather
	// than running socketmode.RunContext once and giving up when it returns.
	return a.superviseSocket(ctx)
}

// startBridge binds the MCP aggregator socket and tears it down when ctx ends.
// A bind failure is logged and degrades to ACP agents having no Murtaugh tools,
// never blocking gateway startup.
func (a *Gateway) startBridge(ctx context.Context) {
	if a.bridge == nil {
		return
	}
	go func() {
		if err := a.bridge.Start(ctx); err != nil {
			a.logger.Warn("mcp aggregator disabled: could not start", "error", err)
		}
	}()
}

// bridgeSocketPath returns the per-process aggregator socket path. It lives under
// the temp dir (kept short — unix socket paths are length-capped) and carries the
// pid so concurrent gateways do not collide.
func bridgeSocketPath() string {
	return filepath.Join(os.TempDir(), "murtaugh", fmt.Sprintf("mcp-agg-%d.sock", os.Getpid()))
}

func (a *Gateway) warmChat(ctx context.Context) {
	if a.chat == nil {
		return
	}
	go func() {
		warmCtx, cancel := context.WithTimeout(ctx, a.chatWarmTimeout)
		defer cancel()
		if err := a.chat.Warm(warmCtx); err != nil {
			a.logger.Error("ACP warmup failed", "error", err)
			return
		}
		a.logger.Info("ACP warmup completed")
	}()
}

// startConfigWatcher launches the on-disk config file watcher in a
// goroutine that lives for the lifetime of the Run context. No-op
// when no paths are configured so the cost (one ticker, one
// goroutine) is paid only by the Slack daemon path. The watcher's
// callback posts a restart suggestion to the admin DM via
// SuggestRestart, which is itself a best-effort no-op when no
// messaging surface or admin user is available.
func (a *Gateway) startConfigWatcher(ctx context.Context) {
	if len(a.configWatchPaths) == 0 {
		return
	}
	watcher := newConfigWatcher(a.configWatchPaths, defaultConfigWatchInterval, a.onConfigFileChanged, a.logger)
	a.logger.Info("config watcher started", "paths", a.configWatchPaths, "interval", defaultConfigWatchInterval.String())
	go watcher.Run(ctx)
}

// onConfigFileChanged is the watcher's callback. It builds an
// operator-facing reason that names the changed file and its new
// mtime, then asks the bot to surface the restart suggestion via
// the standard Block Kit path. Errors from SuggestRestart are
// logged and never propagated since the watcher's contract is
// best-effort: a missed suggestion is preferable to a noisy stall.
func (a *Gateway) onConfigFileChanged(ctx context.Context, path string, mtime time.Time) {
	reason := fmt.Sprintf("`%s` changed on disk at %s; restart Murtaugh to pick up the new config.",
		filepath.Base(path), mtime.UTC().Format(time.RFC3339))
	a.logger.Info("config file change detected", "path", path, "mtime", mtime)
	if _, _, err := a.SuggestRestart(ctx, "", reason); err != nil {
		a.logger.Error("config watcher: restart suggestion failed", "path", path, "error", err)
	}
}

// startChannelCache launches the channel-name cache lifecycle in a goroutine
// that lives for the Run context. It warms the ID→name map once at startup (so
// the first channel messages can route by name) and refreshes it on a ticker.
// No-op when no cache is wired (no bot token, CLI/MCP, struct-literal test
// gateways), so only the Slack daemon path pays for it.
func (a *Gateway) startChannelCache(ctx context.Context) {
	if a.channelCache == nil {
		return
	}
	a.logger.Info("channel-name cache started", "every", defaultChannelCacheRefresh.String())
	go a.channelCache.run(ctx, defaultChannelCacheRefresh)
}

// startJournalSweeper launches the retention sweeper in a goroutine that lives
// for the Run context. It sweeps once at startup (the daemon is not always up,
// so a bare interval timer would drift across sleeps/restarts) and then every
// configured interval. No-op when no sweep is wired, so only the journal-enabled
// daemon path pays for it.
func (a *Gateway) startJournalSweeper(ctx context.Context) {
	if a.journalSweep == nil {
		return
	}
	every := a.journalSweepEvery
	if every <= 0 {
		every = 24 * time.Hour
	}
	a.logger.Info("journal sweeper started", "every", every.String())
	go func() {
		a.runJournalSweep(ctx)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				a.runJournalSweep(ctx)
			}
		}
	}()
}

// runJournalSweep performs one bounded sweep, logging (never propagating) any
// error so a transient failure does not stop the ticker.
func (a *Gateway) runJournalSweep(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := a.journalSweep(sweepCtx); err != nil {
		a.logger.Warn("journal sweep failed", "error", err)
	}
}

func (a *Gateway) handleEvent(ctx context.Context, event socketmode.Event) {
	switch event.Type {
	case socketmode.EventTypeConnected:
		a.logger.Info("Slack socket connected")
		a.recordConnection(ctx, journal.LevelInfo, "connected", "Slack socket connected", nil)
		a.notifyConnected(ctx)
	case socketmode.EventTypeConnecting, socketmode.EventTypeHello:
		a.logger.Debug("socket mode lifecycle event", "type", event.Type)
	case socketmode.EventTypeDisconnect:
		a.logger.Warn("Slack socket disconnected", "type", event.Type)
		a.recordConnection(ctx, journal.LevelWarn, "disconnected", "Slack socket disconnected", nil)
	case socketmode.EventTypeConnectionError, socketmode.EventTypeInvalidAuth, socketmode.EventTypeIncomingError:
		a.logger.Warn("Slack socket error", "type", event.Type)
		a.recordConnection(ctx, journal.LevelWarn, "error", fmt.Sprintf("Slack socket error: %s", event.Type), map[string]any{"event_type": string(event.Type)})
	case socketmode.EventTypeSlashCommand:
		a.handleSlashCommand(ctx, event)
	case socketmode.EventTypeInteractive:
		a.handleInteractive(event)
	case socketmode.EventTypeEventsAPI:
		a.handleEventsAPI(event)
	default:
		a.logger.Debug("ignored socket mode event", "type", event.Type)
	}
}

// resolveAllowSet resolves configuration.admin_user and configuration.allowed_users
// (each may be a Slack user ID or a handle) into IDs and rewrites a.cfg with
// the resolved values, so subsequent IsAllowedUser checks are ID-only. A
// single users.list call is made when any entry is a handle. Unresolvable
// entries are fatal (fail-closed). When both admin_user and allowed_users are
// empty the Gateway is effectively locked down and direct interactions will be
// denied; a warning is logged in that case.
func (a *Gateway) resolveAllowSet(ctx context.Context) error {
	// The admin and allowed_users entries plus the no-mention lists (global and
	// per-channel) may each be handles or IDs. They are resolved in one batched
	// users.list call by concatenating every reference, resolving once, and
	// slicing the result back into place — so a workspace with no handles makes
	// zero Slack calls and one with handles makes exactly one.
	hasAdmin := strings.TrimSpace(a.cfg.AdminUser) != ""

	// Deterministic per-channel key order so the resolved slices line up with the
	// keys when we split the result back out.
	channelKeys := make([]string, 0, len(a.noMentionPerChannel))
	for key := range a.noMentionPerChannel {
		channelKeys = append(channelKeys, key)
	}
	sort.Strings(channelKeys)

	refs := make([]string, 0, 1+len(a.cfg.AllowedUsers)+len(a.noMentionEverywhere))
	if hasAdmin {
		refs = append(refs, a.cfg.AdminUser)
	}
	refs = append(refs, a.cfg.AllowedUsers...)
	refs = append(refs, a.noMentionEverywhere...)
	for _, key := range channelKeys {
		refs = append(refs, a.noMentionPerChannel[key]...)
	}
	if len(refs) == 0 {
		a.logger.Warn("authorization locked down: configuration.admin_user and configuration.allowed_users are both empty; direct interactions will be ignored")
		return nil
	}
	ids, err := resolveUserIDs(ctx, a.api, refs)
	if err != nil {
		return err
	}

	// Slice the resolved IDs back into the same shapes, in the same order they
	// were appended above.
	cursor := 0
	if hasAdmin {
		a.cfg.AdminUser = ids[cursor]
		cursor++
	}
	a.cfg.AllowedUsers = ids[cursor : cursor+len(a.cfg.AllowedUsers)]
	cursor += len(a.cfg.AllowedUsers)
	a.noMentionEverywhere = ids[cursor : cursor+len(a.noMentionEverywhere)]
	cursor += len(a.noMentionEverywhere)
	if len(channelKeys) > 0 {
		resolvedPerChannel := make(map[string][]string, len(channelKeys))
		for _, key := range channelKeys {
			n := len(a.noMentionPerChannel[key])
			resolvedPerChannel[key] = ids[cursor : cursor+n]
			cursor += n
		}
		a.noMentionPerChannel = resolvedPerChannel
	}

	a.logger.Info("resolved authorized Slack users", "admin_user", a.cfg.AdminUser, "allowed_users", len(a.cfg.AllowedUsers))
	return nil
}

// notifyConnected runs the once-per-process, connect-time Slack greeting.
// Exactly one of two things happens, decided by whether a fresh restart marker
// is waiting on disk:
//
//   - Resuming from a restart: the "restarting…" notice is edited in place into
//     the back-online ping card, and the standalone startup ping is suppressed
//     (the operator already has a card to click — see point 2a of the redesign).
//   - Fresh boot / crash / no marker: the normal startup ping card is posted.
//
// Resolving the two against one loaded marker is what makes them mutually
// exclusive — a clean single greeting instead of three stacked messages. Slack
// may emit several Connected events per process (re-connects, flaky links);
// connectHandled guards against repeating the greeting.
func (a *Gateway) notifyConnected(ctx context.Context) {
	if a.connectHandled {
		return
	}
	a.connectHandled = true
	go func() {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		// A consumed marker renders the back-online ping card itself, so the
		// standalone startup ping would be redundant — skip it.
		if a.consumeResumeMarker(c) {
			return
		}
		if a.startupNotifier == nil {
			return
		}
		if err := a.startupNotifier.NotifyStartup(c); err != nil {
			a.logger.Error("startup Slack ping failed", "error", err)
		}
	}()
}

func (a *Gateway) handleInteractive(event socketmode.Event) {
	interaction, ok := event.Data.(slack.InteractionCallback)
	if !ok {
		a.ack(event)
		a.logger.Warn("unexpected interactive payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}

	a.ack(event)
	if !a.cfg.IsAllowedUser(interaction.User.ID) {
		a.logger.Info("denied interactive callback from unauthorized user", "user", interaction.User.ID, "channel", interaction.Channel.ID, "callback_id", interaction.CallbackID)
		return
	}
	if isRestartSuggestionInteraction(interaction) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a.handleRestartSuggestionInteraction(ctx, interaction)
		}()
		return
	}
	// The built-in communication self-test ("Test communication" button) is
	// handled here, before the workflow engine, so the ping → pong round-trip is
	// owned entirely by the binary and cannot be redirected by a configured
	// workflow rule or an on-disk template.
	if isPingInteraction(interaction) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			a.handlePingInteraction(ctx, interaction)
		}()
		return
	}
	// Broker prompts (the `ask` tool, later the approval gate) are routed back to
	// the blocked turn waiting on the click, before the workflow engine sees it.
	// The blocked Ask owns editing the message to its terminal state.
	if a.interactions != nil && askbroker.IsInteraction(interaction) {
		if corr, decision, ok := askbroker.ParseClick(interaction); ok {
			a.interactions.Resolve(corr, decision)
		}
		return
	}
	// Modal-form path (the `ask` tool's multi-question / multi-select / free-text
	// mode). A click on the "Answer" button opens the modal against the click's
	// trigger_id; a view_submission carries the answers back to the blocked
	// AskForm. The trigger_id is short-lived, so OpenForm runs promptly.
	if a.interactions != nil {
		if corr, ok := askbroker.IsFormAnswerClick(interaction); ok {
			triggerID := interaction.TriggerID
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := a.interactions.OpenForm(ctx, corr, triggerID); err != nil {
					a.logger.Error("opening ask modal failed", "error", err, "correlation", corr)
				}
			}()
			return
		}
		if interaction.Type == slack.InteractionTypeViewSubmission {
			if corr, resp, ok := askbroker.ParseViewSubmission(interaction); ok {
				a.interactions.ResolveForm(corr, resp)
				return
			}
		}
	}
	// Mint a correlation id for this interaction and record its arrival. The
	// same id is propagated into the workflow engine via the context so the
	// match/no-match/trigger events all tie back to this one click.
	corrID := journal.NewCorrID("gw")
	a.record(journal.WithCorrID(context.Background(), corrID), "interactive.received", journal.LevelInfo,
		"interactive callback received",
		journal.Keys{TeamID: interaction.Team.ID, ChannelID: interaction.Channel.ID, UserID: interaction.User.ID},
		map[string]any{"interaction_type": string(interaction.Type), "callback_id": interaction.CallbackID})
	// The raw Slack callback bytes (as delivered) are what a `run` trigger gets
	// on stdin — full fidelity, exactly what Slack sent. Falls back to a
	// marshaled form inside the engine when absent.
	var rawPayload []byte
	if event.Request != nil {
		rawPayload = event.Request.Payload
	}
	go func() {
		// No total wall-clock deadline here: a delegate-to-agent step (e.g. a
		// code review) is legitimately long-running and is already bounded by
		// the delegate Runner's idle watchdog (acpCfg.EffectiveRequestTimeout()).
		// The other trigger steps are independently bounded too — a run command
		// by its own commandTimeout, a reply-to-slack by the Slack HTTP client —
		// so a fixed 5m cap here only ever guillotines a productive long turn.
		// cancel() still tears everything down once Execute returns.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ctx = journal.WithCorrID(ctx, corrID)
		if err := a.workflow.Execute(ctx, interaction, rawPayload); err != nil {
			a.logger.Error("interactive workflow failed", "error", err)
		}
	}()
}

func (a *Gateway) handleSlashCommand(ctx context.Context, event socketmode.Event) {
	command, ok := event.Data.(slack.SlashCommand)
	if !ok {
		a.ack(event, ephemeralText("Unsupported slash command payload."))
		a.logger.Warn("unexpected slash command payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}

	if !a.cfg.IsAllowedUser(command.UserID) {
		a.logger.Info("denied slash command from unauthorized user", "command", command.Command, "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralText("Sorry, you are not authorized to use this command."))
		return
	}

	ctx = journal.WithCorrID(ctx, journal.NewCorrID("gw"))
	a.record(ctx, "slash.command", journal.LevelInfo, "slash command received",
		journal.Keys{TeamID: command.TeamID, ChannelID: command.ChannelID, UserID: command.UserID},
		map[string]any{"command": command.Command, "text": command.Text})

	response, err := a.handler.HandleSlashCommand(ctx, command)
	if isRestartSlashCommand(command.Text) {
		a.handleRestartSlashCommand(ctx, event, command)
		return
	}
	if isChatSlashCommand(command.Text) {
		a.handleChatSlashCommand(ctx, event, command)
		return
	}
	if isStopSlashCommand(command) {
		a.handleStopSlashCommand(event, command, slashCommandThreadTS(event))
		return
	}
	if isTroubleshootSlashCommand(command.Text) {
		a.handleTroubleshootSlashCommand(ctx, event, command)
		return
	}
	if err != nil {
		a.logger.Error("slash command failed", "command", command.Command, "error", err)
		response = ephemeralText("Murtaugh hit an error while handling that command.")
	}
	a.ack(event, response)
}

// handleRestartSlashCommand is invoked when an allowed user issues the
// `restart` verb. Authorization is two-layered: the outer
// handleSlashCommand has already checked IsAllowedUser, and this method
// additionally requires IsAdminUser. Non-admin allowed users receive an
// ephemeral deny so the failure mode is discoverable (unlike DMs or
// mentions, where silent ignore is the policy).
//
// On accept, the "restarting…" notice is posted to the originating
// channel and a resume marker is written to disk before the coordinator
// is signalled. The notice + marker are best-effort: any failure is
// logged but never blocks the restart itself (see resume.go).
func (a *Gateway) handleRestartSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
	if !a.cfg.IsAdminUser(command.UserID) {
		a.logger.Info("denied restart slash command from non-admin user", "command", command.Command, "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralText("Sorry, only the configured admin can restart Murtaugh."))
		return
	}
	if a.restart == nil {
		a.logger.Warn("restart slash command received but no coordinator is wired", "user", command.UserID, "channel", command.ChannelID)
		a.ack(event, ephemeralText("Restart is not available in this deployment."))
		return
	}
	reason := fmt.Sprintf("user requested via %s restart", command.Command)
	// Post + persist must happen before the coordinator fires so the
	// marker is durable when the grace timer expires and the process
	// exits. Use a fresh bounded context so a slow Slack API call does
	// not stall the slash ack.
	noticeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	a.postRestartNoticeAndSaveMarker(noticeCtx, command.ChannelID, "", command.UserID, restartSourceSlash, reason)
	cancel()
	if !a.restart(string(restartSourceSlash), command.UserID, command.ChannelID, reason) {
		a.ack(event, ephemeralText("A restart is already in progress (or the cool-down has not elapsed). Try again shortly."))
		return
	}
	a.ack(event, ephemeralText("Restarting Murtaugh now. I'll be back in a moment."))
}

func (a *Gateway) handleChatSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
	text := slashChatPrompt(command.Text)
	if text == "" {
		a.ack(event, ephemeralText("Usage: `/murtaugh chat <prompt>`"))
		return
	}
	if a.chat == nil {
		a.ack(event, ephemeralText("ACP chat is not enabled. Configure `agent.enabled: true` first."))
		return
	}
	a.ack(event, ephemeralText("Murtaugh is answering in the channel."))
	a.startChat(ctx, ChatRequest{TeamID: command.TeamID, ChannelID: command.ChannelID, UserID: command.UserID, Text: text, Source: "slash_command"})
}

// handleTroubleshootSlashCommand assembles a diagnostics bundle and uploads it
// to the admin's DM. Any allowed user may trigger it (the outer
// handleSlashCommand already enforced IsAllowedUser); the bundle is delivered
// only to the admin — never echoed to the invoking channel — because it can
// contain sensitive data. The bundle is built and uploaded in a goroutine so
// the slash command is acked within Slack's ~3s window.
func (a *Gateway) handleTroubleshootSlashCommand(ctx context.Context, event socketmode.Event, command slack.SlashCommand) {
	if a.troubleshoot == nil {
		a.ack(event, ephemeralText("Troubleshooting bundles are not available in this deployment."))
		return
	}
	admin := strings.TrimSpace(a.cfg.AdminUser)
	if admin == "" {
		a.ack(event, ephemeralText("No admin user is configured, so there is nowhere private to send the bundle."))
		return
	}
	if strings.TrimSpace(a.botToken) == "" {
		a.ack(event, ephemeralText("The bot token is not configured, so I cannot upload a bundle."))
		return
	}
	note := slashTroubleshootText(command.Text)
	a.ack(event, ephemeralText("Assembling a diagnostics bundle and sending it to the admin's DM. This can take a moment."))

	go func() {
		bgCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()

		api, err := slackclient.NewClient(a.botToken)
		if err != nil {
			a.logger.Error("troubleshoot: build slack client failed", "error", err)
			return
		}
		dm, err := api.OpenDM(bgCtx, admin)
		if err != nil {
			a.logger.Error("troubleshoot: open admin DM failed", "error", err, "admin", admin)
			return
		}

		zipPath, warnings, err := a.troubleshoot(bgCtx, note)
		if err != nil {
			a.logger.Error("troubleshoot bundle failed", "error", err, "user", command.UserID)
			_, _ = api.PostMessage(bgCtx, slackclient.PostMessageParams{
				ChannelID: dm,
				Text:      fmt.Sprintf(":warning: Troubleshooting bundle requested by <@%s> failed: %v", command.UserID, err),
			})
			return
		}
		defer os.Remove(zipPath)

		if _, err := api.UploadFile(bgCtx, slackclient.UploadFileParams{
			ChannelID:      dm,
			FilePath:       zipPath,
			Filename:       filepath.Base(zipPath),
			Title:          "Murtaugh troubleshooting bundle",
			InitialComment: troubleshootComment(command, note, warnings),
		}); err != nil {
			a.logger.Error("troubleshoot: upload bundle failed", "error", err)
			_, _ = api.PostMessage(bgCtx, slackclient.PostMessageParams{
				ChannelID: dm,
				Text:      fmt.Sprintf(":warning: Assembled the diagnostics bundle but the upload failed: %v", err),
			})
			return
		}
		a.logger.Info("troubleshoot bundle delivered", "user", command.UserID, "warnings", len(warnings))
	}()
}

// troubleshootComment builds the message that accompanies the uploaded bundle,
// carrying who asked, their symptom description, the redaction caveat, and any
// non-fatal collection warnings.
func troubleshootComment(command slack.SlashCommand, note string, warnings []string) string {
	var b strings.Builder
	b.WriteString(":card_file_box: *Murtaugh troubleshooting bundle*\n")
	fmt.Fprintf(&b, "Requested by <@%s>.\n", command.UserID)
	if strings.TrimSpace(note) != "" {
		fmt.Fprintf(&b, "*Symptoms:* %s\n", strings.TrimSpace(note))
	} else {
		b.WriteString("_No symptom description provided._\n")
	}
	b.WriteString(":warning: Secrets are redacted best-effort (Slack tokens + obvious config secrets). Transcripts and `*.db` files are NOT scrubbed — treat as sensitive.\n")
	if len(warnings) > 0 {
		b.WriteString("\n*Collection notes:*\n")
		for _, w := range warnings {
			fmt.Fprintf(&b, "• %s\n", w)
		}
	}
	return b.String()
}

// handleStopSlashCommand cancels the in-flight chat for the
// conversation the command was invoked in. Slack's slash command
// payload carries `thread_ts` when the command was issued from inside
// a thread (the slack-go SlashCommand struct does not surface it, so
// the caller re-parses the raw socketmode payload via
// slashCommandThreadTS and passes it in). Sessions are bound to their
// thread (DMs included), so the key mirrors conversationKey: it carries
// the thread context and flags DM channels. A /stop must therefore be
// issued from inside the target thread to cancel it.
//
// Authorisation: the outer handleSlashCommand has already enforced
// IsAllowedUser, so no extra admin gate is required here.
func (a *Gateway) handleStopSlashCommand(event socketmode.Event, command slack.SlashCommand, threadTS string) {
	key := agent.ConversationKey{
		TeamID:    command.TeamID,
		ChannelID: command.ChannelID,
		ThreadTS:  threadTS,
		DM:        strings.HasPrefix(command.ChannelID, "D"),
	}
	if a.inFlight.Cancel(key) {
		a.logger.Info("stop slash command cancelled in-flight chat", "user", command.UserID, "channel", command.ChannelID, "thread_ts", threadTS)
		a.ack(event, ephemeralText("Stopped."))
		return
	}
	a.ack(event, ephemeralText("Nothing to stop."))
}

func (a *Gateway) handleEventsAPI(event socketmode.Event) {
	eventsAPI, ok := event.Data.(slackevents.EventsAPIEvent)
	if !ok {
		a.ack(event)
		a.logger.Warn("unexpected Events API payload", "type", fmt.Sprintf("%T", event.Data))
		return
	}
	a.ack(event)
	switch inner := eventsAPI.InnerEvent.Data.(type) {
	case *slackevents.LinkSharedEvent:
		a.handleLinkShared(eventsAPI.TeamID, inner)
	case *slackevents.AppMentionEvent:
		if a.chat == nil {
			a.logger.Debug("ignored app_mention because ACP chat is disabled")
			return
		}
		if inner.BotID != "" {
			return
		}
		if !a.cfg.IsAllowedUser(inner.User) {
			a.logger.Debug("ignored app_mention from unauthorized user", "user", inner.User, "channel", inner.Channel)
			return
		}
		if a.isDuplicateEvent(eventsAPI.TeamID, inner.Channel, inner.TimeStamp) {
			a.logger.Info("ignored duplicate app_mention", "channel", inner.Channel, "ts", inner.TimeStamp)
			return
		}
		text := stripSlackMentions(inner.Text)
		a.startChat(context.Background(), ChatRequest{TeamID: eventsAPI.TeamID, ChannelID: inner.Channel, UserID: inner.User, ThreadTS: inner.ThreadTimeStamp, MessageTS: inner.TimeStamp, Text: text, Files: inner.Files, Source: "app_mention"})
	case *slackevents.MessageEvent:
		if a.chat == nil {
			a.logger.Debug("ignored message because ACP chat is disabled")
			return
		}
		// Bot/self messages are never answered, in DMs or channels.
		if inner.BotID != "" {
			return
		}
		// Allow plain messages and file uploads ("file_share"); drop other
		// subtypes (edits, deletes, joins, bot messages, …). Without this a DM
		// that carries an attachment is silently ignored.
		if inner.SubType != "" && inner.SubType != "file_share" {
			return
		}
		if inner.ChannelType == "im" {
			a.handleDirectMessage(eventsAPI, inner, event)
			return
		}
		// A plain (non-mention) channel message: the bot only answers it when the
		// author is waived from the mention requirement for this channel. Users not
		// on the no-mention list still reach the bot via the app_mention path.
		a.handleChannelMessage(eventsAPI, inner, event)
	default:
		a.logger.Debug("ignored Events API event", "inner_type", eventsAPI.InnerEvent.Type)
	}
}

// handleDirectMessage answers a DM (channel_type "im"). It is the verbatim DM
// path lifted out of handleEventsAPI: authorize the author, drop redelivered
// duplicates, then start the chat. DMs never require a mention.
func (a *Gateway) handleDirectMessage(eventsAPI slackevents.EventsAPIEvent, inner *slackevents.MessageEvent, event socketmode.Event) {
	if !a.cfg.IsAllowedUser(inner.User) {
		a.logger.Debug("ignored DM from unauthorized user", "user", inner.User, "channel", inner.Channel)
		return
	}
	if a.isDuplicateEvent(eventsAPI.TeamID, inner.Channel, inner.TimeStamp) {
		a.logger.Info("ignored duplicate DM", "channel", inner.Channel, "ts", inner.TimeStamp)
		return
	}
	a.startChat(context.Background(), ChatRequest{TeamID: eventsAPI.TeamID, ChannelID: inner.Channel, UserID: inner.User, ThreadTS: inner.ThreadTimeStamp, MessageTS: inner.TimeStamp, Text: inner.Text, Files: eventFiles(event), DM: true, Source: "dm"})
}

// handleChannelMessage answers a plain (non-mention) message posted in a public
// channel or private group. The bot normally only replies to explicit
// @mentions there; this path waives the mention requirement for authors listed
// in the effective no-mention set — the UNION of configuration.do_not_require_mention_from
// and chat.channel_do_not_require_mention entries whose glob matches this
// channel. The waiver is mention-only: the author must STILL pass IsAllowedUser.
//
// The channel name is resolved from the in-memory cache (no Slack I/O on the
// socket goroutine); a cache miss triggers a non-blocking refresh so a
// brand-new channel's name-glob rules can match the NEXT message. Dedup relies
// on the shared message ts: an author who DID @mention is handled by the
// app_mention path, and isDuplicateEvent keys on (team, channel, ts), so the
// twin plain-message delivery is dropped here rather than double-firing.
func (a *Gateway) handleChannelMessage(eventsAPI slackevents.EventsAPIEvent, inner *slackevents.MessageEvent, event socketmode.Event) {
	if !a.cfg.IsAllowedUser(inner.User) {
		a.logger.Debug("ignored channel message from unauthorized user", "user", inner.User, "channel", inner.Channel)
		return
	}
	channelName, known := a.channelCache.nameFor(inner.Channel)
	if !known {
		a.channelCache.refreshAsync(context.Background())
	}
	allowed := usersAllowedWithoutMention(inner.Channel, channelName, a.noMentionEverywhere, a.noMentionPerChannel)
	if !allowed[inner.User] {
		a.logger.Debug("ignored channel message: author not waived from mention requirement", "user", inner.User, "channel", inner.Channel)
		return
	}
	if a.isDuplicateEvent(eventsAPI.TeamID, inner.Channel, inner.TimeStamp) {
		a.logger.Info("ignored duplicate channel message", "channel", inner.Channel, "ts", inner.TimeStamp)
		return
	}
	// Strip any mentions so the prompt is clean whether or not the user @mentioned
	// the bot (a listed user who also mentions is de-duped above via the shared ts).
	text := stripSlackMentions(inner.Text)
	a.startChat(context.Background(), ChatRequest{TeamID: eventsAPI.TeamID, ChannelID: inner.Channel, UserID: inner.User, ThreadTS: inner.ThreadTimeStamp, MessageTS: inner.TimeStamp, Text: text, Files: eventFiles(event), Source: "channel_no_mention"})
}

func (a *Gateway) handleLinkShared(teamID string, inner *slackevents.LinkSharedEvent) {
	if a.unfurl == nil {
		a.logger.Debug("ignored link_shared because no unfurl-rules are configured")
		return
	}
	req := LinkSharedRequest{
		TeamID:    teamID,
		ChannelID: inner.Channel,
		UserID:    inner.User,
		MessageTS: inner.MessageTimeStamp,
		ThreadTS:  inner.ThreadTimeStamp,
		Links:     inner.Links,
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), a.unfurlTimeout)
		defer cancel()
		ctx = journal.WithCorrID(ctx, journal.NewCorrID("gw"))
		if err := a.unfurl.Handle(ctx, req); err != nil {
			a.logger.Error("link unfurl failed", "channel", inner.Channel, "error", err)
		}
	}()
}

// isDuplicateEvent reports whether a message event (identified by its
// immutable Slack timestamp) has already been handled. Slack may deliver the
// same event more than once; without this guard a redelivery spawns a second
// chat that interrupts the first. An empty timestamp or unconfigured dedup
// cache is never treated as a duplicate.
func (a *Gateway) isDuplicateEvent(teamID, channelID, ts string) bool {
	if ts == "" {
		return false
	}
	return a.recentEvents.seenBefore(teamID + "|" + channelID + "|" + ts)
}

// agentInterruptible reports whether the resolved agent supports interrupting
// an in-flight prompt. Unknown agents, and session managers that do not expose
// the capability, are treated as interruptible so behaviour is unchanged unless
// detection explicitly says otherwise.
func (a *Gateway) agentInterruptible(agent string) bool {
	sessions, ok := a.chatSessions[agent]
	if !ok {
		return true
	}
	checker, ok := sessions.(interface{ Interruptible() bool })
	if !ok {
		return true
	}
	return checker.Interruptible()
}

// followUpDeferredText is the thread note posted when a follow-up is held back
// because the agent cannot be interrupted.
const followUpDeferredText = ":hourglass_flowing_sand: Still working on your previous message — this agent can't be interrupted, so I'll finish that first before picking this up."

// notifyFollowUpDeferred posts a brief, best-effort thread note so a user whose
// follow-up was dropped (non-interruptible agent, response in flight) is not
// left wondering why nothing happened. Failures are logged, never propagated;
// a missing messaging surface (CLI/MCP, some tests) makes this a no-op.
func (a *Gateway) notifyFollowUpDeferred(parent context.Context, req ChatRequest) {
	if a.messaging == nil {
		return
	}
	threadTS := streamThreadTS(req)
	if threadTS == "" {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	options := []slack.MsgOption{slack.MsgOptionText(followUpDeferredText, false), slack.MsgOptionTS(threadTS)}
	if _, _, err := a.messaging.PostMessageContext(ctx, req.ChannelID, options...); err != nil {
		a.logger.Warn("failed to post follow-up deferred note", "channel", req.ChannelID, "thread_ts", threadTS, "error", err)
	}
}

// startChat launches a chat goroutine for the request and wires it into
// the in-flight registry so subsequent messages on the same
// conversation interrupt the previous response, and so the /stop slash
// command can cancel it. The cancellation closure stored in the
// registry runs a two-step graceful-then-hard sequence: ask the ACP
// agent to cancel its prompt (best-effort, non-blocking) and, after
// cancelGrace, hard-cancel the chat goroutine's context. The grace
// window lets trailing chunks already on the wire flush as
// "_interrupted_" rather than vanish, which is what ChatHandler.Handle
// renders when it sees context.Canceled (vs DeadlineExceeded).
func (a *Gateway) startChat(parent context.Context, req ChatRequest) {
	key := conversationKey(req)
	agent := ""
	if a.chat != nil && a.chat.resolver != nil {
		agent = a.chat.resolver(req)
	}
	// When the agent cannot be interrupted (session/cancel unsupported), a
	// follow-up must neither cancel the in-flight response (the cancel is a
	// no-op at the agent and only yields a misleading "_interrupted_") nor run
	// concurrently against the same ACP session. Let the first finish; log and
	// drop the follow-up.
	if !a.agentInterruptible(agent) && a.inFlight.Active(key) {
		a.logger.Info("ignoring follow-up while a response is in flight; agent is not interruptible", "channel", req.ChannelID, "thread_ts", key.ThreadTS, "agent", agent)
		a.notifyFollowUpDeferred(parent, req)
		return
	}
	// No total wall-clock deadline here: a turn is bounded by inactivity inside
	// ChatHandler (WithIdleTimeout), so a long-but-progressing response is never
	// killed mid-flight. This context stays cancellable purely for the interrupt
	// and /stop paths.
	ctx, cancelCtx := context.WithCancel(parent)
	cancelFunc := a.buildInterruptCancel(key, agent, cancelCtx)
	_, previous := a.inFlight.Register(key, cancelFunc, agent)
	if previous != nil {
		a.logger.Info("interrupting previous in-flight chat", "channel", req.ChannelID, "thread_ts", key.ThreadTS, "agent", agent)
		previous()
	}
	go func() {
		defer cancelCtx()
		err := a.chat.Handle(ctx, req)
		a.inFlight.Cancel(key) // best-effort self-unregister; no-op if already replaced
		if err != nil {
			a.logger.Error("ACP chat failed", "source", req.Source, "channel", req.ChannelID, "error", err)
		}
	}()
}

// buildInterruptCancel returns the cancellation closure stored in the
// in-flight registry for one chat goroutine. It is invoked either by
// the next message on the same conversation (interrupt path) or by the
// /stop slash command. The sequence:
//
//  1. Look up the live ACP session ID for the conversation. If there is
//     one, fire a non-blocking session/cancel — it tells the agent to
//     stop generating but keeps the session alive for the follow-up.
//  2. Wait cancelGrace, then hard-cancel the chat goroutine's context.
//     The grace timer runs in its own goroutine so the registry call
//     returns immediately; the chat goroutine itself will see the
//     context cancellation and unwind through ChatHandler.Handle's
//     interrupted path.
//
// Resolution of agent name → session manager uses chatSessions, which
// the Gateway captured at construction time. When ACP is disabled the
// closure degenerates to a plain cancelCtx call.
func (a *Gateway) buildInterruptCancel(key agent.ConversationKey, agent string, cancelCtx context.CancelFunc) context.CancelFunc {
	return func() {
		go func() {
			if a.chatSessions != nil {
				if sessions, ok := a.chatSessions[agent]; ok {
					if sessionID, live := sessions.Lookup(key); live {
						cancelReqCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						if err := sessions.Cancel(cancelReqCtx, sessionID); err != nil {
							a.logger.Warn("ACP session cancel failed", "agent", agent, "session_id", sessionID, "error", err)
						}
						cancel()
					}
				}
			}
			time.Sleep(a.cancelGrace)
			cancelCtx()
		}()
	}
}

func isChatSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "chat")
}

func isTroubleshootSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "troubleshoot")
}

// slashTroubleshootText returns the free-text symptom description following the
// `troubleshoot` verb, or "" when the verb is absent. An empty description is
// allowed (the bundle is still useful) — it is not an error.
func slashTroubleshootText(text string) string {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.EqualFold(fields[0], "troubleshoot") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
}

// isStopSlashCommand recognises both the standalone `/stop` slash
// command (carried in command.Command) and the `<command> stop` verb
// form (carried in command.Text), so operators can wire either shape
// in the Slack app config. Matching is case-insensitive and tolerant
// of leading/trailing whitespace.
func isStopSlashCommand(command slack.SlashCommand) bool {
	if strings.EqualFold(strings.TrimSpace(command.Command), "/stop") {
		return true
	}
	fields := strings.Fields(command.Text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "stop")
}

// slashCommandThreadTS extracts thread_ts from the raw socketmode
// payload. Slack includes the field on slash command invocations made
// from inside a thread, but slack-go's SlashCommand struct does not
// surface it, so we re-parse the JSON. Returns "" when the field is
// absent (channel-root invocations and DMs), which the caller treats
// as a channel-scoped lookup.
func slashCommandThreadTS(event socketmode.Event) string {
	if event.Request == nil {
		return ""
	}
	var payload struct {
		ThreadTS string `json:"thread_ts"`
	}
	if err := json.Unmarshal(event.Request.Payload, &payload); err != nil {
		return ""
	}
	return payload.ThreadTS
}

// eventFiles extracts the file attachments from the raw Events API payload.
// slack-go's MessageEvent struct does not surface `files` (only AppMentionEvent
// does), so we re-parse the JSON — the same approach as slashCommandThreadTS.
// Returns nil when there is no payload or no files.
func eventFiles(event socketmode.Event) []slack.File {
	if event.Request == nil {
		return nil
	}
	var payload struct {
		Event struct {
			Files []slack.File `json:"files"`
		} `json:"event"`
	}
	if err := json.Unmarshal(event.Request.Payload, &payload); err != nil {
		return nil
	}
	return payload.Event.Files
}

// restartSourceSlash mirrors internal/app.RestartSourceSlash. It is
// duplicated here to keep gateway independent of the composition root
// (importing internal/app would cycle back). Compatibility is enforced by
// keeping both string values identical.
const restartSourceSlash = "slash"

func isRestartSlashCommand(text string) bool {
	fields := strings.Fields(text)
	return len(fields) > 0 && strings.EqualFold(fields[0], "restart")
}

func slashChatPrompt(text string) string {
	fields := strings.Fields(text)
	if len(fields) <= 1 || !strings.EqualFold(fields[0], "chat") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), fields[0]))
}

func stripSlackMentions(text string) string {
	fields := strings.Fields(text)
	kept := fields[:0]
	for _, field := range fields {
		if strings.HasPrefix(field, "<@") && strings.HasSuffix(field, ">") {
			continue
		}
		kept = append(kept, field)
	}
	return strings.Join(kept, " ")
}

func (a *Gateway) ack(event socketmode.Event, response ...any) {
	if event.Request == nil {
		a.logger.Warn("cannot acknowledge event without request", "type", event.Type)
		return
	}
	socket := a.currentSocket()
	if socket == nil {
		return // no-op when no socket is wired (struct-literal test gateways)
	}
	if err := socket.Ack(*event.Request, response...); err != nil {
		a.logger.Error("failed to acknowledge Slack request", "error", err)
	}
}
