package gateway

import (
	"context"
	"fmt"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/journal"
	"github.com/slack-go/slack/socketmode"
)

// This file makes the Slack Socket Mode connection self-healing.
//
// slack-go's socketmode.RunContext reconnects on disconnects it detects, but in
// the field a connection was observed to go "zombie": the daemon process stayed
// alive (scheduled jobs kept firing) while the websocket received nothing for
// two days — no pings, no events, no reconnect — until a manual restart. The
// likely cause is a half-open TCP connection (e.g. after the host sleeps) whose
// blocked read slack-go's internal deadman timer failed to recover, with
// RunContext neither returning nor reconnecting.
//
// The supervisor below owns the socket lifecycle so the daemon recovers without
// human intervention:
//   - It runs RunContext in a loop, rebuilding a fresh socketmode client and
//     reconnecting (with capped exponential backoff) whenever an attempt ends —
//     instead of giving up the one time RunContext returns.
//   - A watchdog goroutine forces a reconnect when the socket falls silent past
//     socketSilenceTimeout (the half-open case) or when an active auth.test
//     heartbeat fails repeatedly (Slack unreachable / token revoked).
//   - Every connect/disconnect/recycle is journalled as a gateway `connection`
//     event so the next incident is visible in the bundle instead of invisible.

const (
	// initialReconnectBackoff and maxReconnectBackoff bound the exponential
	// wait between reconnect attempts. The backoff resets after a connection
	// stays up at least healthyConnDuration, so a long-lived link that finally
	// drops reconnects promptly rather than at the previous attempt's long wait.
	initialReconnectBackoff = 1 * time.Second
	maxReconnectBackoff     = 30 * time.Second
	healthyConnDuration     = 60 * time.Second

	// heartbeatInterval is how often the watchdog actively probes Slack
	// reachability via auth.test; heartbeatTimeout bounds one probe;
	// heartbeatFailThreshold consecutive failures force a reconnect. The probe
	// uses the Web API (a separate HTTP path), so it catches an unreachable
	// Slack or a revoked token — but NOT a half-open websocket (the Web API
	// stays healthy then). The silence check below covers that case.
	heartbeatInterval      = 60 * time.Second
	heartbeatTimeout       = 10 * time.Second
	heartbeatFailThreshold = 3

	// socketSilenceTimeout is the heart of the zombie fix: if the daemon
	// believes it is connected but receives no inbound socketmode event of any
	// kind for this long, the websocket is assumed half-open and recycled.
	// Deliberately generous — a bot with any traffic (DMs, mentions, unfurls,
	// slash commands) never trips it; only a genuinely idle connection recycles
	// at this cadence, which is cheap and safe (Slack redelivers unacked events
	// and the gateway de-dupes them). A heartbeat success does NOT reset this
	// clock — only real socket traffic does — precisely so a half-open socket
	// with a still-healthy Web API path is still detected.
	socketSilenceTimeout = 10 * time.Minute

	// watchdogTick is how often the silence check runs.
	watchdogTick = 30 * time.Second
)

// socketStalled reports whether the gap between now and the last observed
// socket activity exceeds timeout, returning the measured silence. Extracted so
// the watchdog's core decision is unit-testable without real timers.
func socketStalled(now, last time.Time, timeout time.Duration) (bool, time.Duration) {
	silent := now.Sub(last)
	return silent > timeout, silent
}

// clock returns the current time, honouring the test override.
func (a *Gateway) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// setSocket swaps in the socket the supervisor is about to run; currentSocket
// reads the live socket for the ack path. The two are serialised so an ack on
// the event goroutine never races the supervisor's reconnect.
func (a *Gateway) setSocket(s *socketmode.Client) {
	a.connMu.Lock()
	a.socket = s
	a.connMu.Unlock()
}

func (a *Gateway) currentSocket() *socketmode.Client {
	a.connMu.Lock()
	defer a.connMu.Unlock()
	return a.socket
}

// stampActivity records that an inbound socket event just arrived; lastActivity
// reads it back. The watchdog compares it against socketSilenceTimeout.
func (a *Gateway) stampActivity()          { a.lastActivityNano.Store(a.clock().UnixNano()) }
func (a *Gateway) lastActivity() time.Time { return time.Unix(0, a.lastActivityNano.Load()) }

// recordConnection emits a gateway `connection` journal event carrying the
// transition state, so connects, disconnects, and forced recycles are visible.
func (a *Gateway) recordConnection(ctx context.Context, level journal.Level, state, summary string, extra map[string]any) {
	payload := map[string]any{"state": state}
	for k, v := range extra {
		payload[k] = v
	}
	a.record(ctx, "connection", level, summary, journal.Keys{}, payload)
}

// superviseSocket owns the Slack socket for the daemon's lifetime: it builds a
// fresh socketmode client, runs one connection attempt, and on any end (clean
// return, error, or watchdog-forced recycle) reconnects with capped backoff,
// until ctx is cancelled. Returns nil on shutdown.
func (a *Gateway) superviseSocket(ctx context.Context) error {
	backoff := initialReconnectBackoff
	for {
		if ctx.Err() != nil {
			return nil
		}

		socket := socketmode.New(a.webClient, socketmode.OptionDebug(a.cfg.Debug))
		a.setSocket(socket)
		a.stampActivity() // start the silence clock for this attempt

		a.recordConnection(ctx, journal.LevelInfo, "connecting", "Connecting to Slack socket mode", nil)
		started := a.clock()
		runErr := a.runSocketAttempt(ctx, socket)
		lasted := a.clock().Sub(started)

		if ctx.Err() != nil {
			return nil // daemon shutting down
		}

		if lasted >= healthyConnDuration {
			backoff = initialReconnectBackoff
		}
		extra := map[string]any{"lasted_ms": lasted.Milliseconds(), "backoff_ms": backoff.Milliseconds()}
		if runErr != nil {
			extra["error"] = runErr.Error()
		}
		a.recordConnection(ctx, journal.LevelWarn, "reconnecting", "Slack socket attempt ended; reconnecting", extra)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxReconnectBackoff {
			backoff = maxReconnectBackoff
		}
	}
}

// runSocketAttempt runs one RunContext alongside the event loop and the
// watchdog, all under a child context. It returns when the daemon context is
// cancelled, when the watchdog forces a recycle, or when RunContext returns.
//
// Crucially, on cancellation it returns WITHOUT waiting for RunContext: a
// half-open RunContext may never return, so the supervisor abandons it (and its
// now-orphaned read goroutine, which unwinds once the dead socket finally
// errors) and builds a fresh socket.
func (a *Gateway) runSocketAttempt(parent context.Context, socket *socketmode.Client) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- socket.RunContext(ctx) }()
	go a.runConnectionWatchdog(ctx, cancel, watchdogTick, heartbeatInterval)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-runErrCh:
			return err
		case event := <-socket.Events:
			a.stampActivity()
			a.handleEvent(ctx, event)
		}
	}
}

// runConnectionWatchdog forces a reconnect (by cancelling the attempt) when the
// socket falls silent past socketSilenceTimeout or when the auth.test heartbeat
// fails heartbeatFailThreshold times in a row. It exits when the attempt ends.
func (a *Gateway) runConnectionWatchdog(ctx context.Context, forceReconnect context.CancelFunc, tick, heartbeatEvery time.Duration) {
	silence := time.NewTicker(tick)
	defer silence.Stop()
	heartbeat := time.NewTicker(heartbeatEvery)
	defer heartbeat.Stop()

	var hbFailures int
	for {
		select {
		case <-ctx.Done():
			return
		case <-silence.C:
			if stalled, silent := socketStalled(a.clock(), a.lastActivity(), socketSilenceTimeout); stalled {
				a.recordConnection(ctx, journal.LevelWarn, "stalled",
					"No Slack socket activity within the silence window; recycling the connection",
					map[string]any{"silent_ms": silent.Milliseconds()})
				a.logger.Warn("Slack socket stalled; recycling", "silent", silent.String())
				forceReconnect()
				return
			}
		case <-heartbeat.C:
			if a.heartbeatOK(ctx) {
				hbFailures = 0
				continue
			}
			hbFailures++
			a.logger.Warn("Slack heartbeat failed", "consecutive_failures", hbFailures)
			if hbFailures >= heartbeatFailThreshold {
				a.recordConnection(ctx, journal.LevelWarn, "heartbeat_failed",
					"Slack heartbeat failed repeatedly; recycling the connection",
					map[string]any{"consecutive_failures": hbFailures})
				forceReconnect()
				return
			}
		}
	}
}

// heartbeatOK performs one bounded auth.test probe. It reports healthy when no
// Web API client is wired (struct-literal test gateways) so tests never trigger
// reconnects.
func (a *Gateway) heartbeatOK(ctx context.Context) bool {
	if a.webClient == nil {
		return true
	}
	probeCtx, cancel := context.WithTimeout(ctx, heartbeatTimeout)
	defer cancel()
	if _, err := a.webClient.AuthTestContext(probeCtx); err != nil {
		a.logger.Warn("Slack heartbeat probe failed", "error", fmt.Sprintf("%v", err))
		return false
	}
	return true
}
