// Package mcpclient is the client side of MCP: it connects to externally
// configured MCP servers and exposes their remote tools as Murtaugh
// tools.Tool values, so the native agent loop can call them in-process.
//
// It is the mirror of internal/frontends/mcp, which is the *server* side of
// the same go-sdk. Here we are the consumer: mcp.NewClient → Client.Connect →
// ClientSession.ListTools / ClientSession.CallTool.
//
// Two transports are supported:
//   - stdio child process, via ServerConfig.Command/Args/Env (the SDK's
//     CommandTransport wrapping an *exec.Cmd);
//   - a remote HTTP endpoint, via ServerConfig.URL (the SDK's
//     StreamableClientTransport).
//
// This package intentionally does NOT import internal/config: the wiring task
// maps config.MCPServerConfig → ServerConfig. The struct here is the only
// configuration surface mcpclient knows about.
package mcpclient

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// clientName and clientVersion identify Murtaugh to the remote MCP server
// during the initialize handshake.
const (
	clientName    = "murtaugh-mcpclient"
	clientVersion = "0.1.0"
)

// ServerConfig describes one MCP server to connect to. Exactly one transport
// must be selected: set Command for a stdio child process, or URL for a remote
// HTTP (streamable) server. Name is a stable, human-chosen identifier used to
// prefix the server's tool names (see tool.go).
type ServerConfig struct {
	// Name identifies the server and prefixes its tool names. Required.
	Name string
	// Command is the executable for a stdio child-process transport.
	Command string
	// Args are the arguments passed to Command.
	Args []string
	// Env are extra environment variables for the child process, merged onto
	// the parent environment. Ignored for the remote URL transport.
	Env map[string]string
	// URL is the endpoint for a remote streamable-HTTP transport.
	URL string
}

// transport selects and builds the go-sdk Transport implied by the config.
// It enforces that exactly one of Command / URL is set.
func (c ServerConfig) transport() (mcpsdk.Transport, error) {
	if c.Name == "" {
		return nil, fmt.Errorf("mcpclient: server config has no name")
	}
	hasCmd := c.Command != ""
	hasURL := c.URL != ""
	switch {
	case hasCmd && hasURL:
		return nil, fmt.Errorf("mcpclient: server %q sets both command and url; pick one", c.Name)
	case hasCmd:
		cmd := exec.Command(c.Command, c.Args...)
		cmd.Env = mergeEnv(os.Environ(), c.Env)
		return &mcpsdk.CommandTransport{Command: cmd}, nil
	case hasURL:
		return &mcpsdk.StreamableClientTransport{Endpoint: c.URL}, nil
	default:
		return nil, fmt.Errorf("mcpclient: server %q sets neither command nor url", c.Name)
	}
}

// mergeEnv overlays extra onto base (in KEY=VALUE form), with extra winning on
// key collisions. base is typically os.Environ().
func mergeEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		key, _, _ := strings.Cut(kv, "=")
		if _, override := extra[key]; override {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// Client is a live connection to a single MCP server. It owns one
// ClientSession for the lifetime of the connection and is safe to call
// ListTools / Close on. It is not safe for concurrent use by multiple
// goroutines.
type Client struct {
	name    string
	session *mcpsdk.ClientSession
}

// Connect dials the MCP server described by cfg and performs the initialize
// handshake, returning a ready Client. The caller owns the returned Client and
// must Close it. ctx bounds the connection/handshake.
func Connect(ctx context.Context, cfg ServerConfig) (*Client, error) {
	return connectWith(ctx, cfg, nil)
}

// connectWith is the test seam: it lets a caller (or a test) supply a
// pre-built transport instead of deriving one from cfg, while still going
// through the same NewClient/Connect path as production.
func connectWith(ctx context.Context, cfg ServerConfig, override mcpsdk.Transport) (*Client, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("mcpclient: server config has no name")
	}
	transport := override
	if transport == nil {
		t, err := cfg.transport()
		if err != nil {
			return nil, err
		}
		transport = t
	}

	client := mcpsdk.NewClient(&mcpsdk.Implementation{
		Name:    clientName,
		Version: clientVersion,
	}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, fmt.Errorf("mcpclient: connect to server %q: %w", cfg.Name, err)
	}
	return &Client{name: cfg.Name, session: session}, nil
}

// Name returns the server's configured name.
func (c *Client) Name() string { return c.name }

// ListTools fetches every tool the server advertises and wraps each as a
// tools.Tool whose name is prefixed with the server name (see tool.go). It
// follows pagination cursors so all tools are returned.
func (c *Client) ListTools(ctx context.Context) ([]tools.Tool, error) {
	var out []tools.Tool
	var cursor string
	for {
		res, err := c.session.ListTools(ctx, &mcpsdk.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, fmt.Errorf("mcpclient: list tools from server %q: %w", c.name, err)
		}
		for _, remote := range res.Tools {
			out = append(out, wrapTool(c.name, c.session, remote))
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// Close terminates the session (and, for stdio, the child process).
func (c *Client) Close() error {
	if c.session == nil {
		return nil
	}
	return c.session.Close()
}
