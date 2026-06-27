// Command murtaugh is the single entry point for the Murtaugh dev toolkit.
// It can run as the Slack gateway — the Socket Mode daemon started by
// `murtaugh slack gateway` — the MCP stdio server (`murtaugh mcp`), or
// invoke any of the registered CLI tools directly (e.g. `murtaugh ping`,
// `murtaugh jobs run --name X`, `murtaugh slack send-msg --to ...`).
//
// All modes share the same loaded config and the same Tool registry, so
// adding a new tool exposes it to both the CLI and MCP frontends in a
// single change.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/miere/murtaugh/internal/app"
	"github.com/miere/murtaugh/internal/config"
	"github.com/miere/murtaugh/internal/config/migrate"
	"github.com/miere/murtaugh/internal/help"
	"github.com/miere/murtaugh/internal/journal"
	"github.com/miere/murtaugh/internal/mcpbridge"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "murtaugh:", err)
		os.Exit(1)
	}
}

// run parses the top-level flags and mode, loads the config, and delegates
// to the application layer. It is separated from main() so it can be
// exercised by tests.
func run(rawArgs []string) error {
	if len(rawArgs) > 0 && rawArgs[0] == "version" {
		fmt.Println(version)
		return nil
	}
	// `murtaugh mcp-bridge` is the transparent stdio↔socket proxy the gateway
	// hands an ACP agent to reach Murtaugh's per-agent MCP aggregator. It is
	// spawned by the agent (not a user), needs no config, and must keep stdout
	// clean for MCP — so it is dispatched here, before any config bootstrap or
	// logging is wired.
	if len(rawArgs) > 0 && rawArgs[0] == mcpbridge.Subcommand {
		return runMCPBridge()
	}
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}

	configPath, args, err := extractConfigFlag(rawArgs, defaultPath)
	if err != nil {
		return err
	}
	// `config migrate` runs the schema migration on demand (the same path the
	// daemon runs automatically at startup), so an operator can convert a config
	// dir without launching the gateway.
	if len(args) >= 2 && args[0] == "config" && args[1] == "migrate" {
		return runConfigMigrate(filepath.Dir(configPath))
	}
	// --json is a global, opt-in boolean stripped before help/mode selection
	// and tool dispatch. The tool flag parser requires every --flag to carry a
	// value, so a bare --json must not reach it; stripping here lets both
	// `murtaugh --json ping` and `murtaugh ping --json` work.
	jsonOutput, args, err := extractJSONFlag(args)
	if err != nil {
		return err
	}
	// Help is resolved before config bootstrap/load so `murtaugh help` (and
	// `murtaugh <command> --help`) work on a machine that has never been
	// configured. The single embedded reference is the source of truth.
	if tokens, ok := helpRequest(args); ok {
		fmt.Fprint(os.Stdout, help.Render(tokens))
		return nil
	}
	mode, rest := selectMode(args)
	setupInvocation := isSetupInvocation(mode, rest)

	// Convert a legacy config directory to the current schema before bootstrap
	// seeds a fresh template. Each step is backup/validate/rollback-guarded, so a
	// failure here leaves the original config intact. Skipped for setup tools:
	// they are actively constructing the config (it may be partial / token-less),
	// which is not a state to migrate or validate.
	if !setupInvocation {
		if applied, err := migrate.Run(filepath.Dir(configPath)); err != nil {
			return fmt.Errorf("config migration failed: %w", err)
		} else if len(applied) > 0 {
			fmt.Fprintf(os.Stderr, "murtaugh: migrated config to schema v%d\n", applied[len(applied)-1])
		}
	}

	if err := config.Bootstrap(configPath); err != nil {
		return err
	}
	// Setup tools are the bootstrap path: they may run before a valid config
	// has been written, and Validate() would reject the placeholder gateway.yaml
	// the installer plans to overwrite. Skip Load() and hand the tool an
	// empty Config — every setup.* tool resolves its target path from the
	// config dir alone.
	var cfg config.Config
	if !setupInvocation {
		loaded, err := config.Load(configPath)
		if err != nil {
			return err
		}
		cfg = loaded
	}

	logger := newLogger(cfg.Access.Debug, mode)

	// The journal records agent-facing domain events (gateway interactions, job
	// runs). It is opened here so its drain-on-shutdown is tied to process exit;
	// a failure to open degrades to a no-op recorder rather than blocking start.
	store, recorder, closeJournal := openJournal(cfg, mode, rest, logger)
	defer closeJournal()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application := app.New(mode, rest, cfg, configPath, version, logger, recorder).
		WithJSONOutput(jsonOutput)
	// The Slack gateway is the only long-running mode that needs a
	// user-triggered restart path. stop is reused as the cancel hook so
	// the coordinator's shutdown looks identical to a SIGTERM from the
	// outside (launchd, systemd) — process exits 0, supervisor respawns.
	if mode == app.ModeGateway {
		application = application.WithRestartCoordinator(
			app.NewRestartCoordinator(stop, logger, 0, 0),
		)
		if path, err := defaultResumeMarkerPath(); err != nil {
			logger.Warn("resume marker disabled: could not resolve state directory", "error", err)
		} else {
			application = application.WithResumeMarkerPath(path)
		}
		application = application.WithConfigWatchPaths(defaultConfigWatchPaths(configPath))
		// The daemon is the single writer, so the retention sweep runs here,
		// reusing the recorder's store (Prune serializes with the writer on the
		// one connection). The journal.prune tool is the manual equivalent.
		if store != nil {
			application = application.WithJournalSweeper(func(ctx context.Context) error {
				res, err := store.Prune(ctx, time.Now())
				if err != nil {
					return err
				}
				if res.Total > 0 {
					logger.Info("journal swept old events", "removed", res.Total, "by_stream", res.Removed)
				}
				return nil
			}, cfg.Journal.EffectiveSweepEvery())
		}
	}
	if mode == app.ModeCLI && len(rest) == 0 {
		return errors.New(application.UsageLine())
	}
	// A bare `murtaugh slack` (no subcommand) lists the slack subcommands
	// instead of trying to resolve a tool literally named "slack".
	if mode == app.ModeCLI && len(rest) == 1 && rest[0] == "slack" {
		return errors.New(application.SlackUsageLine())
	}
	return application.Run(ctx)
}

// runMCPBridge runs the `murtaugh mcp-bridge` subcommand: a transparent pipe
// between the spawning agent's stdio and the gateway's aggregator socket. The
// socket path and session token arrive via the environment so no argument
// parsing (which could collide with config flags) is needed. It blocks until the
// agent closes the pipe or the process is signalled.
func runMCPBridge() error {
	socket := os.Getenv(mcpbridge.EnvSocket)
	token := os.Getenv(mcpbridge.EnvToken)
	if socket == "" || token == "" {
		return fmt.Errorf("%s requires %s and %s in the environment", mcpbridge.Subcommand, mcpbridge.EnvSocket, mcpbridge.EnvToken)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return mcpbridge.RunBridge(ctx, socket, token, os.Stdin, os.Stdout)
}

// extractConfigFlag pulls the global --config flag out of args, supporting
// both `--config=VALUE` and `--config VALUE` (and the single-dash variants).
// Unknown flags are passed through to the selected frontend untouched.
func extractConfigFlag(args []string, fallback string) (string, []string, error) {
	out := make([]string, 0, len(args))
	configPath := fallback
	for i := 0; i < len(args); i++ {
		a := args[i]
		name, value, hasValue := parseConfigToken(a)
		if name != "config" {
			out = append(out, a)
			continue
		}
		if hasValue {
			configPath = value
			continue
		}
		if i+1 >= len(args) {
			return "", nil, errors.New("--config requires a value")
		}
		configPath = args[i+1]
		i++
	}
	return configPath, out, nil
}

// parseConfigToken inspects a token for the --config / -config flag form.
// It returns the bare flag name (without dashes), the embedded value when
// the token uses the --key=value form, and whether such a value was present.
// Tokens that do not look like the config flag return name="".
func parseConfigToken(a string) (string, string, bool) {
	if !strings.HasPrefix(a, "-") {
		return "", "", false
	}
	trimmed := strings.TrimLeft(a, "-")
	name, value, hasValue := trimmed, "", false
	if i := strings.IndexByte(trimmed, '='); i >= 0 {
		name = trimmed[:i]
		value = trimmed[i+1:]
		hasValue = true
	}
	return name, value, hasValue
}

// extractJSONFlag pulls the global --json boolean out of args and returns
// whether it was set along with the remaining tokens. A bare `--json` enables
// it; the `--json=true` / `--json=false` form is also honoured. It is stripped
// before help/mode selection and tool dispatch because the tool flag parser
// rejects value-less flags. Single-dash `-json` is accepted to match the
// config flag's leniency.
func extractJSONFlag(args []string) (bool, []string, error) {
	out := make([]string, 0, len(args))
	enabled := false
	for _, a := range args {
		name, value, hasValue := parseConfigToken(a)
		if name != "json" {
			out = append(out, a)
			continue
		}
		if !hasValue {
			enabled = true
			continue
		}
		b, err := strconv.ParseBool(value)
		if err != nil {
			return false, nil, fmt.Errorf("--json: expected boolean, got %q", value)
		}
		enabled = b
	}
	return enabled, out, nil
}

// helpRequest reports whether args asks for help and, if so, returns the
// command tokens that scope it (empty means the full document). A leading
// `help` subcommand consumes the rest of the tokens as the command to look up
// (`murtaugh help slack send-msg`); otherwise a `--help`/`-h` flag anywhere
// triggers help scoped to the surrounding command (`murtaugh slack send-msg
// --help`). The `--config` flag has already been stripped by the caller.
func helpRequest(args []string) ([]string, bool) {
	if len(args) > 0 && args[0] == "help" {
		return args[1:], true
	}
	tokens := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == "--help" || a == "-h" {
			found = true
			continue
		}
		tokens = append(tokens, a)
	}
	if found {
		return tokens, true
	}
	return nil, false
}

// openJournal opens the event journal and returns the store, a recorder, and a
// cleanup that drains and closes them. The store is returned so the gateway can
// reuse it for the retention sweep (the daemon is the single writer). It
// degrades to a nil store + no-op recorder (with a no-op cleanup) for setup
// invocations — which run before a valid config exists — and whenever the store
// cannot be opened, so journaling never blocks startup. The caller must invoke
// the returned cleanup before exit so buffered events flush.
func openJournal(cfg config.Config, mode app.Mode, rest []string, logger *slog.Logger) (*journal.Store, journal.Recorder, func()) {
	if isSetupInvocation(mode, rest) {
		return nil, journal.NopRecorder{}, func() {}
	}
	path := cfg.Journal.EffectivePath()
	store, err := journal.Open(path, cfg.Journal.RetentionByStream(),
		journal.WithBlobDir(cfg.Journal.EffectiveBlobDir()))
	if err != nil {
		logger.Warn("journal disabled: could not open event store", "path", path, "error", err)
		return nil, journal.NopRecorder{}, func() {}
	}
	recorder := journal.NewRecorder(store, cfg.Journal.EnabledStreams(), logger)
	cleanup := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := recorder.Close(ctx); err != nil {
			logger.Warn("journal recorder did not drain cleanly", "error", err)
		}
		if err := store.Close(); err != nil {
			logger.Warn("journal store close failed", "error", err)
		}
	}
	return store, recorder, cleanup
}

// isSetupInvocation reports whether the CLI was asked to run a setup.* tool.
// Setup tools intentionally run before config.Load — they exist precisely to
// produce the file that Load would otherwise validate.
func isSetupInvocation(mode app.Mode, rest []string) bool {
	if mode != app.ModeCLI || len(rest) == 0 {
		return false
	}
	return rest[0] == "setup"
}

// selectMode resolves the top-level subcommand. `slack gateway` starts the
// long-running Socket Mode daemon; `slack <tool>` and every other token are
// CLI tools resolved by the registry. No subcommand prints usage (ModeCLI
// with an empty arg list), so the gateway is always launched explicitly.
// runConfigMigrate converts the config directory to the current schema and
// prints what it did. It is the manual entrypoint for the same migration the
// daemon runs at startup; both are backup/validate/rollback-guarded.
func runConfigMigrate(dir string) error {
	applied, err := migrate.Run(dir)
	if err != nil {
		return err
	}
	if len(applied) == 0 {
		fmt.Fprintln(os.Stdout, "config is already at the current schema; nothing to migrate")
		return nil
	}
	fmt.Fprintf(os.Stdout, "migrated %s to schema v%d\n", dir, applied[len(applied)-1])
	return nil
}

func selectMode(args []string) (app.Mode, []string) {
	if len(args) == 0 {
		return app.ModeCLI, nil
	}
	switch args[0] {
	case "slack":
		// `slack gateway` is the daemon; `slack <tool>` falls through to
		// the CLI, where resolve() forms the dotted name "slack.<tool>".
		// A bare `slack` also falls through; run() then prints the slack
		// subcommand list.
		if len(args) >= 2 && args[1] == "gateway" {
			return app.ModeGateway, args[2:]
		}
		return app.ModeCLI, args
	case "mcp":
		return app.ModeMCP, args[1:]
	default:
		return app.ModeCLI, args
	}
}

// defaultConfigWatchPaths returns the on-disk files whose mtime
// changes should make the gateway suggest a restart. The
// canonical Murtaugh layout keeps gateway.yaml, agents.yaml, and
// jobs.yaml as siblings under ~/.config/murtaugh, so we derive the
// list from the main config path's parent dir rather than hard-
// coding home-relative locations (which would break --config
// overrides used in tests and staging deployments).
func defaultConfigWatchPaths(configPath string) []string {
	configPath = strings.TrimSpace(configPath)
	if configPath == "" {
		return nil
	}
	baseDir := filepath.Dir(configPath)
	return []string{
		configPath,
		filepath.Join(baseDir, "agents.yaml"),
		filepath.Join(baseDir, "jobs.yaml"),
	}
}

// defaultResumeMarkerPath resolves the on-disk location for the
// cross-restart resume marker. Follows the XDG state convention
// (XDG_STATE_HOME overrides; falls back to ~/.local/state/murtaugh)
// because the marker is runtime state, not config.
func defaultResumeMarkerPath() (string, error) {
	if v := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); v != "" {
		return filepath.Join(v, "murtaugh", "restart.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "murtaugh", "restart.json"), nil
}

// newLogger builds the slog logger Murtaugh uses for daemon-style modes.
// CLI invocations get a quieter logger so tool output dominates stdout/
// stderr.
func newLogger(debug bool, mode app.Mode) *slog.Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	if mode == app.ModeCLI {
		level = slog.LevelWarn
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}
