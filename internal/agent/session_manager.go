package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

type SessionManager struct {
	client      Client
	idleTimeout time.Duration
	maxSessions int
	now         func() time.Time
	logger      *slog.Logger

	// cancelOverride, when non-nil, forces the interruptible verdict and
	// skips the startup probe. It is populated from the agent's
	// `interruptible:` config flag.
	cancelOverride *bool

	mu          sync.Mutex
	initialized bool
	// interruptible caches whether the agent implements session/cancel.
	// interruptibleKnown is false until Warm has resolved it (via the
	// override or the probe); callers treat unknown as interruptible so a
	// missing/failed probe never silently changes behaviour.
	interruptible      bool
	interruptibleKnown bool
	sessions           map[ConversationKey]managedSession
}

// cancelCapabilityProber is the optional capability surface a Client can
// implement so the manager can detect, at warmup, whether the agent supports
// interruption. *ProcessClient satisfies it.
type cancelCapabilityProber interface {
	SupportsCancel(ctx context.Context) bool
}

type managedSession struct {
	session  Session
	lastUsed time.Time
}

func NewSessionManager(client Client, idleTimeout time.Duration, maxSessions int) *SessionManager {
	if idleTimeout <= 0 {
		idleTimeout = 30 * time.Minute
	}
	if maxSessions <= 0 {
		maxSessions = 100
	}
	return &SessionManager{client: client, idleTimeout: idleTimeout, maxSessions: maxSessions, now: time.Now, logger: slog.Default(), sessions: make(map[ConversationKey]managedSession)}
}

func (m *SessionManager) WithLogger(logger *slog.Logger) *SessionManager {
	if logger != nil {
		m.logger = logger
	}
	return m
}

// WithCancelOverride forces the interruptible verdict from configuration,
// bypassing the startup probe. nil leaves auto-detection in place.
func (m *SessionManager) WithCancelOverride(override *bool) *SessionManager {
	m.cancelOverride = override
	return m
}

// Interruptible reports whether the agent can have an in-flight prompt
// cancelled via session/cancel. Until Warm has resolved the capability it
// returns true so behaviour is unchanged when detection has not (yet) run.
func (m *SessionManager) Interruptible() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.interruptibleKnown {
		return true
	}
	return m.interruptible
}

// resolveInterruptible determines and caches whether the agent supports
// cancellation: the config override wins; otherwise it probes the client when
// the client exposes the capability surface. Defaults to interruptible when no
// signal is available. Logs the verdict once so operators see it at deployment.
func (m *SessionManager) resolveInterruptible(ctx context.Context) {
	interruptible := true
	source := "default"
	if m.cancelOverride != nil {
		interruptible = *m.cancelOverride
		source = "config"
	} else if prober, ok := m.client.(cancelCapabilityProber); ok {
		interruptible = prober.SupportsCancel(ctx)
		source = "probe"
	}
	m.mu.Lock()
	m.interruptible = interruptible
	m.interruptibleKnown = true
	m.mu.Unlock()
	if interruptible {
		m.logger.Info("ACP agent is interruptible", "source", source)
		return
	}
	m.logger.Warn("ACP agent does not support session/cancel; new messages will not interrupt an in-flight response", "source", source)
}

func (m *SessionManager) Warm(ctx context.Context) error {
	m.mu.Lock()
	if m.initialized {
		m.mu.Unlock()
		return nil
	}
	startedAt := m.now()
	if err := m.client.Initialize(ctx); err != nil {
		m.mu.Unlock()
		return fmt.Errorf("initialize ACP client: %w", err)
	}
	m.initialized = true
	m.mu.Unlock()
	m.logger.Info("warmed ACP client", "duration", m.now().Sub(startedAt))
	// Resolve interruptibility after releasing the lock: the probe performs a
	// round-trip to the agent and resolveInterruptible takes the lock itself.
	m.resolveInterruptible(ctx)
	return nil
}

func (m *SessionManager) Prompt(ctx context.Context, key ConversationKey, metadata SessionMetadata, request PromptRequest) (<-chan Event, error) {
	session, err := m.session(ctx, key, metadata)
	if err != nil {
		return nil, err
	}
	// Tell the agent which Slack conversation it is in so it can target the
	// `restart` tool's approval card here instead of the admin DM. Only fill
	// what the caller has not already set, so explicit callers win.
	if request.Channel == "" {
		request.Channel = metadata.ChannelID
	}
	if request.Thread == "" {
		request.Thread = metadata.ThreadTS
	}
	return m.client.Prompt(ctx, session.ID, request)
}

// Lookup returns the cached session ID for the conversation without
// creating a new session. The second return value is false when the
// conversation has no live session — callers must treat that case as
// "nothing to cancel" and skip the ACP-level cancel call.
func (m *SessionManager) Lookup(key ConversationKey) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[key]
	if !ok {
		return "", false
	}
	return session.session.ID, true
}

// Cancel asks the underlying ACP client to abort the given session's
// in-flight prompt. It is a thin pass-through so callers (e.g. the Slack
// frontend's interrupt registry) do not need to hold a reference to the
// raw Client. The cancel call is best-effort: the agent is free to
// finish flushing trailing chunks before honouring the request.
func (m *SessionManager) Cancel(ctx context.Context, sessionID string) error {
	return m.client.Cancel(ctx, sessionID)
}

func (m *SessionManager) session(ctx context.Context, key ConversationKey, metadata SessionMetadata) (Session, error) {
	m.mu.Lock()
	if session, ok := m.sessions[key]; ok {
		session.lastUsed = m.now()
		m.sessions[key] = session
		m.mu.Unlock()
		m.logger.Info("reusing ACP session", "team", key.TeamID, "channel", key.ChannelID, "thread", key.ThreadTS, "dm", key.DM)
		return session.session, nil
	}
	m.mu.Unlock()

	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[key]; ok {
		session.lastUsed = m.now()
		m.sessions[key] = session
		m.logger.Info("reusing ACP session", "team", key.TeamID, "channel", key.ChannelID, "thread", key.ThreadTS, "dm", key.DM)
		return session.session, nil
	}
	if !m.initialized {
		if err := m.client.Initialize(ctx); err != nil {
			return Session{}, fmt.Errorf("initialize ACP client: %w", err)
		}
		m.initialized = true
	}
	m.evictLocked()
	startedAt := m.now()
	session, err := m.client.NewSession(ctx, metadata)
	if err != nil {
		return Session{}, fmt.Errorf("create ACP session: %w", err)
	}
	m.sessions[key] = managedSession{session: session, lastUsed: m.now()}
	m.logger.Info("created ACP session", "team", key.TeamID, "channel", key.ChannelID, "thread", key.ThreadTS, "dm", key.DM, "duration", m.now().Sub(startedAt))
	return session, nil
}

func (m *SessionManager) evictLocked() {
	now := m.now()
	for key, session := range m.sessions {
		if now.Sub(session.lastUsed) > m.idleTimeout {
			delete(m.sessions, key)
		}
	}
	for len(m.sessions) >= m.maxSessions {
		var oldestKey ConversationKey
		var oldest time.Time
		first := true
		for key, session := range m.sessions {
			if first || session.lastUsed.Before(oldest) {
				oldestKey = key
				oldest = session.lastUsed
				first = false
			}
		}
		delete(m.sessions, oldestKey)
	}
}

func (m *SessionManager) Close() error {
	return m.client.Close()
}
