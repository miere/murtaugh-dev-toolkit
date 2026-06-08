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
	"path/filepath"
	"sort"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
	"github.com/miere/murtaugh-dev-toolkit/internal/frontends/cli"
	"github.com/miere/murtaugh-dev-toolkit/internal/frontends/mcp"
	"github.com/miere/murtaugh-dev-toolkit/internal/slackapp"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/jobs/define"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/jobs/run"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools/ping"
	setupbootstrap "github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/bootstrap"
	setupslack "github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/slack"
)

// Mode selects which frontend Run starts.
type Mode int

const (
	// ModeCLI runs the human-facing CLI frontend.
	ModeCLI Mode = iota
	// ModeMCP runs the MCP stdio server frontend.
	ModeMCP
	// ModeSlack runs the Slack Socket Mode daemon.
	ModeSlack
)

// Application is the composition root for a single murtaugh invocation. It
// is constructed once per process and reused for the lifetime of the chosen
// frontend.
type Application struct {
	mode       Mode
	args       []string
	cfg        config.Config
	configPath string
	logger     *slog.Logger
	registry   *tools.Registry
	// restart is the optional graceful-restart coordinator. Only the Slack
	// daemon path attaches one; CLI and MCP modes leave it nil.
	restart *RestartCoordinator
	// resumeMarkerPath is the on-disk location of the cross-restart
	// marker. Empty disables the resume confirmation flow; the restart
	// still happens but no "back online" notice is posted.
	resumeMarkerPath string
	// configWatchPaths is the list of files whose mtime, when it
	// advances, makes the Slack daemon suggest a restart to the
	// admin via Block Kit. Empty disables the watcher entirely.
	configWatchPaths []string
}

// New constructs an Application for the given mode. cfg/configPath/logger
// come from the entry point so the same loaded state is shared across
// frontends. args is the list of positional arguments handed to the CLI
// frontend (Slack/MCP ignore it).
func New(mode Mode, args []string, cfg config.Config, configPath string, logger *slog.Logger) *Application {
	reg := buildRegistry(cfg, configPath)
	return &Application{
		mode:       mode,
		args:       args,
		cfg:        cfg,
		configPath: configPath,
		logger:     logger,
		registry:   reg,
	}
}

// Run starts the selected frontend and blocks until it returns. CLI and MCP
// share the same Registry; Slack ignores the registry and starts the Socket
// Mode daemon directly.
func (a *Application) Run(ctx context.Context) error {
	switch a.mode {
	case ModeMCP:
		return mcp.New(a.registry).Serve(ctx)
	case ModeSlack:
		sl := slackapp.New(a.cfg, a.logger)
		if rc := a.restart; rc != nil {
			// Adapt the coordinator's Request method into slackapp's
			// stringly-typed trigger so the slackapp package stays free
			// of any internal/app import (which would cycle).
			sl = sl.WithRestartTrigger(func(source, userID, channel, reason string) bool {
				return rc.Request(RestartRequest{
					Source:  RestartSource(source),
					UserID:  userID,
					Channel: channel,
					Reason:  reason,
				})
			})
		}
		if path := strings.TrimSpace(a.resumeMarkerPath); path != "" {
			sl = sl.WithResumeMarkerStore(slackapp.NewFileResumeMarkerStore(path))
			a.logger.Debug("resume marker store wired", "path", path)
		}
		if len(a.configWatchPaths) > 0 {
			sl = sl.WithConfigWatchPaths(a.configWatchPaths)
			a.logger.Debug("config watcher wired", "paths", a.configWatchPaths)
		}
		a.logger.Info("starting Slack Socket Mode service", "config", a.configPath)
		err := sl.Run(ctx)
		if err != nil && ctx.Err() != nil {
			err = nil
		}
		a.logger.Info("Slack Socket Mode service stopped")
		return err
	default:
		return cli.New(a.registry).Run(ctx, a.args)
	}
}

// UsageLine renders a human-readable usage string built from the registered
// tools. Flat tool names (e.g. `ping`) are listed first; namespaced tools
// (e.g. `jobs.run`) are grouped by their namespace and rendered as
// `<ns> <sub>`. The built-in `slack` and `mcp` modes (handled by main.go)
// are appended so callers see every entry point in one line.
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

	parts := append([]string{}, flat...)
	for _, ns := range groupOrder {
		subs := groups[ns]
		sort.Strings(subs)
		parts = append(parts, fmt.Sprintf("%s <%s>", ns, strings.Join(subs, "|")))
	}
	parts = append(parts, "slack", "mcp")
	return "usage: murtaugh <command>; commands: " + strings.Join(parts, ", ")
}

// Registry exposes the underlying registry. Intended for tests so the
// composition wiring can be inspected without standing up a frontend.
func (a *Application) Registry() *tools.Registry { return a.registry }

// WithRestartCoordinator attaches a restart coordinator to the application
// and returns the receiver to support a fluent wiring style at the entry
// point. Only the Slack daemon path currently consumes the coordinator;
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
// it advances, makes the Slack daemon ask the admin to confirm a
// restart via Block Kit. Empty disables the watcher entirely.
// Returns the receiver for fluent wiring.
func (a *Application) WithConfigWatchPaths(paths []string) *Application {
	a.configWatchPaths = paths
	return a
}

// buildRegistry wires every tool Murtaugh ships with. New tools must be
// registered here so they appear in both the CLI and MCP frontends.
func buildRegistry(cfg config.Config, configPath string) *tools.Registry {
	reg := tools.NewRegistry()
	reg.Register(ping.New())

	jobsLookup := func(name string) (config.JobProfile, bool) {
		j, ok := cfg.Jobs[name]
		return j, ok
	}
	reg.Register(run.New(jobsLookup))

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

	return reg
}
