// Command murtaugh is the single entry point for the Murtaugh dev toolkit.
// It can run as the Slack Socket Mode daemon (`murtaugh slack`, default),
// the MCP stdio server (`murtaugh mcp`), or invoke any of the registered
// CLI tools directly (e.g. `murtaugh ping`, `murtaugh jobs run --name X`).
//
// All three modes share the same loaded config and the same Tool registry,
// so adding a new tool exposes it to both the CLI and MCP frontends in a
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
	"strings"
	"syscall"

	"github.com/miere/murtaugh-dev-toolkit/internal/app"
	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

const usage = `Usage: murtaugh [--config PATH] <command> [args...]

Modes:
  slack                 Start the Slack Socket Mode daemon (default).
  mcp                   Start the MCP stdio server.
  <tool> [args...]      Invoke a registered CLI tool. Namespaced tools take
                        two tokens, e.g. ` + "`" + `murtaugh jobs run --name <n>` + "`" + `.

Flags:
  --config PATH         Slack configuration YAML
                        (default: ~/.config/murtaugh/slack.yaml).
`

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
	defaultPath, err := config.DefaultPath()
	if err != nil {
		return err
	}

	configPath, args, err := extractConfigFlag(rawArgs, defaultPath)
	if err != nil {
		return err
	}
	mode, rest := selectMode(args)

	if err := config.Bootstrap(configPath); err != nil {
		return err
	}
	// Setup tools are the bootstrap path: they may run before a valid config
	// has been written, and Validate() would reject the placeholder slack.yaml
	// the installer plans to overwrite. Skip Load() and hand the tool an
	// empty Config — every setup.* tool resolves its target path from the
	// config dir alone.
	var cfg config.Config
	if !isSetupInvocation(mode, rest) {
		loaded, err := config.Load(configPath)
		if err != nil {
			return err
		}
		cfg = loaded
	}

	logger := newLogger(cfg.Configuration.Debug, mode)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	application := app.New(mode, rest, cfg, configPath, version, logger)
	// The Slack daemon is the only long-running mode that needs a
	// user-triggered restart path. stop is reused as the cancel hook so
	// the coordinator's shutdown looks identical to a SIGTERM from the
	// outside (launchd, systemd) — process exits 0, supervisor respawns.
	if mode == app.ModeSlack {
		application = application.WithRestartCoordinator(
			app.NewRestartCoordinator(stop, logger, 0, 0),
		)
		if path, err := defaultResumeMarkerPath(); err != nil {
			logger.Warn("resume marker disabled: could not resolve state directory", "error", err)
		} else {
			application = application.WithResumeMarkerPath(path)
		}
		application = application.WithConfigWatchPaths(defaultConfigWatchPaths(configPath))
	}
	if mode == app.ModeCLI && len(rest) == 0 {
		return errors.New(application.UsageLine())
	}
	return application.Run(ctx)
}

// extractConfigFlag pulls the global --config flag out of args, supporting
// both `--config=VALUE` and `--config VALUE` (and the single-dash variants).
// Unknown flags are passed through to the selected frontend untouched.
func extractConfigFlag(args []string, fallback string) (string, []string, error) {
	out := make([]string, 0, len(args))
	configPath := fallback
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--help" || a == "-h" {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(0)
		}
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

// isSetupInvocation reports whether the CLI was asked to run a setup.* tool.
// Setup tools intentionally run before config.Load — they exist precisely to
// produce the file that Load would otherwise validate.
func isSetupInvocation(mode app.Mode, rest []string) bool {
	if mode != app.ModeCLI || len(rest) == 0 {
		return false
	}
	return rest[0] == "setup"
}

// selectMode resolves the top-level subcommand. `slack` is the default when
// nothing is supplied so the long-running daemon stays the no-argument
// behaviour the README documents.
func selectMode(args []string) (app.Mode, []string) {
	if len(args) == 0 {
		return app.ModeSlack, nil
	}
	switch args[0] {
	case "slack":
		return app.ModeSlack, args[1:]
	case "mcp":
		return app.ModeMCP, args[1:]
	default:
		return app.ModeCLI, args
	}
}

// defaultConfigWatchPaths returns the on-disk files whose mtime
// changes should make the Slack daemon suggest a restart. The
// canonical Murtaugh layout keeps slack.yaml, agents.yaml, and
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
