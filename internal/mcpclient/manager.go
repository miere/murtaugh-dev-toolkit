package mcpclient

import (
	"context"
	"log/slog"

	"github.com/miere/murtaugh/internal/tools"
)

// Manager owns the connections to a set of MCP servers, reusing one session
// per server, and aggregates their wrapped tools as a single []tools.Tool for
// the native agent loop. A connection failure for one server is isolated:
// it is logged and skipped, never failing the whole set.
//
// A Manager is constructed by Open and torn down by Close; it is not intended
// for concurrent mutation after Open returns.
type Manager struct {
	clients []*Client
	tools   []tools.Tool
	logger  *slog.Logger
}

// Open connects to each server in cfgs, listing and wrapping their tools.
// Servers that fail to connect or list are logged and skipped so a single bad
// server cannot take down the agent. The returned Manager owns every
// successful connection; the caller must Close it. logger may be nil.
func Open(ctx context.Context, cfgs []ServerConfig, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	m := &Manager{logger: logger}
	for _, cfg := range cfgs {
		c, err := Connect(ctx, cfg)
		if err != nil {
			logger.Warn("mcpclient: skipping server, connect failed",
				"server", cfg.Name, "error", err)
			continue
		}
		toolset, err := c.ListTools(ctx)
		if err != nil {
			logger.Warn("mcpclient: skipping server, list tools failed",
				"server", cfg.Name, "error", err)
			// Best-effort cleanup of the half-open connection.
			if cerr := c.Close(); cerr != nil {
				logger.Warn("mcpclient: error closing failed server",
					"server", cfg.Name, "error", cerr)
			}
			continue
		}
		m.clients = append(m.clients, c)
		m.tools = append(m.tools, toolset...)
		logger.Info("mcpclient: connected server",
			"server", cfg.Name, "tools", len(toolset))
	}
	return m
}

// Tools returns the aggregated wrapped tools across every connected server,
// in server order. The slice is owned by the Manager; do not mutate it.
func (m *Manager) Tools() []tools.Tool { return m.tools }

// Close shuts down every connected server, attempting all of them even if some
// fail. It returns the first error encountered, if any.
func (m *Manager) Close() error {
	var firstErr error
	for _, c := range m.clients {
		if err := c.Close(); err != nil {
			m.logger.Warn("mcpclient: error closing server",
				"server", c.Name(), "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	m.clients = nil
	return firstErr
}

// discard is a no-op io.Writer used as the sink for the fallback logger when
// the caller passes nil.
type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
