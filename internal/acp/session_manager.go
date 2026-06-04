package acp

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

	mu          sync.Mutex
	initialized bool
	sessions    map[ConversationKey]managedSession
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

func (m *SessionManager) Warm(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.initialized {
		return nil
	}
	startedAt := m.now()
	if err := m.client.Initialize(ctx); err != nil {
		return fmt.Errorf("initialize ACP client: %w", err)
	}
	m.initialized = true
	m.logger.Info("warmed ACP client", "duration", m.now().Sub(startedAt))
	return nil
}

func (m *SessionManager) Prompt(ctx context.Context, key ConversationKey, metadata SessionMetadata, request PromptRequest) (<-chan Event, error) {
	session, err := m.session(ctx, key, metadata)
	if err != nil {
		return nil, err
	}
	return m.client.Prompt(ctx, session.ID, request)
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
