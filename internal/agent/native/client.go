// Package native is the in-process, LLM-backed agent.Client (kind: native). It
// owns the conversation array per session and runs the tool-calling turn loop
// itself — no external agent process, no ACP. It exists to satisfy the same
// agent.Client interface ProcessClient does, so SessionManager, the Slack
// ChatHandler, streaming, the journal, and agentdelegate consume it unchanged.
//
// This file is the T0b skeleton: it pins the interface so the loop (T7), the
// provider layer (T1), the toolset resolver (T8), and the wiring (T9) can be
// built against a stable seam. Methods return errNotImplemented until the loop
// lands.
package native

import (
	"context"
	"errors"

	"github.com/miere/murtaugh-dev-toolkit/internal/agent"
)

// errNotImplemented is returned by skeleton methods before the loop (T7) lands.
var errNotImplemented = errors.New("native agent: not implemented yet")

// Client is the native LLM-backed agent.Client. Dependencies (llm.Provider, the
// resolved []tools.Tool toolset, system-prompt builder, max-turns, logger) are
// wired by the constructor in T9; the skeleton holds none yet.
type Client struct{}

// New constructs a skeleton native Client. The real constructor (taking a
// provider, toolset, and prompt config) replaces this in T9.
func New() *Client { return &Client{} }

func (c *Client) Initialize(context.Context) error { return errNotImplemented }

func (c *Client) NewSession(context.Context, agent.SessionMetadata) (agent.Session, error) {
	return agent.Session{}, errNotImplemented
}

func (c *Client) Prompt(context.Context, string, agent.PromptRequest) (<-chan agent.Event, error) {
	return nil, errNotImplemented
}

func (c *Client) Cancel(context.Context, string) error { return errNotImplemented }

func (c *Client) Close() error { return nil }

// Compile-time assertion that the native client satisfies the agent backend
// contract — the linchpin seam for the whole migration.
var _ agent.Client = (*Client)(nil)
