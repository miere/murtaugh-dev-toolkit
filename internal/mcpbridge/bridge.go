// Package mcpbridge connects an external ACP agent to Murtaugh's own tools
// without leaking credentials or duplicating logic into the agent's process.
//
// The long-running gateway hosts a Server on a private unix socket. Each ACP
// session is Registered with the resolved toolset it should see (built-ins plus
// proxied external MCP tools) and the approver that gates them; Register returns
// an unguessable token. The gateway hands the agent a stdio MCP server in
// session/new whose command is `murtaugh mcp-bridge` with that token in its
// env. The agent spawns it; RunBridge dials the socket, presents the token, and
// then byte-pipes its stdio to the connection. The gateway runs the real MCP
// server (over the same socket connection) bound to that session's toolset.
//
// So: agent ⇄ (stdio) ⇄ bridge subprocess ⇄ (unix socket) ⇄ gateway aggregator.
// Third-party MCP credentials and the approval broker stay in the gateway; the
// agent only ever sees Murtaugh's curated tool surface.
package mcpbridge

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/miere/murtaugh-dev-toolkit/internal/frontends/mcp"
	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// Subcommand is the argv[1] that runs the bridge: `murtaugh mcp-bridge`.
const Subcommand = "mcp-bridge"

// EnvSocket and EnvToken name the environment variables the gateway sets on the
// Stdio McpServer it hands the agent, and that the bridge subcommand reads. Both
// sides reference these constants so the contract stays in one place.
const (
	EnvSocket = "MURTAUGH_BRIDGE_SOCKET"
	EnvToken  = "MURTAUGH_BRIDGE_TOKEN"
)

// handshake is the single newline-delimited JSON line the bridge sends before
// any MCP traffic. It authenticates the connection to a registered session.
type handshake struct {
	Token string `json:"token"`
}

// Session is what a registered ACP session exposes through the aggregator.
type Session struct {
	// Tools is the resolved per-agent toolset to serve (toolset.Resolve output).
	Tools []tools.Tool
	// Approver gates tool calls; nil means ungated.
	Approver mcp.Approver
	// WithContext, when set, decorates the context each tool Invoke runs under —
	// used to carry the session's Slack TurnLocation so the approver posts to the
	// right thread. nil is identity.
	WithContext func(context.Context) context.Context
}

// Server is the gateway-side aggregator: a unix-socket listener that serves each
// registered session's toolset as an MCP server.
type Server struct {
	socketPath string
	log        *slog.Logger

	mu       sync.Mutex
	sessions map[string]Session
	listener net.Listener
	closed   bool
}

// NewServer creates a Server that will listen on socketPath. The socket's parent
// directory is created with 0700 and the socket itself with 0600 on Start, so
// only Murtaugh's own user can connect — defence in depth alongside the token.
func NewServer(socketPath string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{socketPath: socketPath, log: log, sessions: make(map[string]Session)}
}

// Start binds the socket and runs the accept loop until ctx is cancelled or
// Close is called. It returns once the listener stops.
func (s *Server) Start(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o700); err != nil {
		return fmt.Errorf("create socket dir: %w", err)
	}
	// A stale socket from a previous run would make Listen fail with "address
	// already in use"; remove it first. It is in our private 0700 dir.
	_ = os.Remove(s.socketPath)
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.socketPath, err)
	}
	if err := os.Chmod(s.socketPath, 0o600); err != nil {
		_ = ln.Close()
		return fmt.Errorf("chmod socket: %w", err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = ln.Close()
		return errors.New("server is closed")
	}
	s.listener = ln
	s.mu.Unlock()

	// Stop the listener when the context is cancelled so Accept unblocks.
	go func() {
		<-ctx.Done()
		_ = s.Close()
	}()

	s.log.Info("mcp aggregator listening", "socket", s.socketPath)
	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed || errors.Is(err, net.ErrClosed) {
				return nil
			}
			s.log.Warn("mcp aggregator accept failed", "error", err)
			continue
		}
		go s.serveConn(ctx, conn)
	}
}

// Register binds a resolved toolset to a fresh token and returns it. The caller
// passes the token to the bridge (via env) so the agent's spawned bridge can
// claim this session. Unregister when the session ends.
func (s *Server) Register(sess Session) (token string, err error) {
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.sessions[tok] = sess
	s.mu.Unlock()
	return tok, nil
}

// Unregister drops a session's token so no further bridge connections can claim
// it. In-flight connections are unaffected.
func (s *Server) Unregister(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// SocketPath is where the server listens; the value to give the bridge.
func (s *Server) SocketPath() string { return s.socketPath }

// Close stops the listener and removes the socket file.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	ln := s.listener
	s.mu.Unlock()
	var err error
	if ln != nil {
		err = ln.Close()
	}
	_ = os.Remove(s.socketPath)
	return err
}

// serveConn reads the handshake, resolves the session, and runs an MCP server
// over the connection bound to that session's toolset.
func (s *Server) serveConn(ctx context.Context, conn net.Conn) {
	// Read exactly the handshake line, keeping any buffered MCP bytes for the
	// transport by reusing the same bufio.Reader below.
	br := bufio.NewReader(conn)
	line, err := br.ReadBytes('\n')
	if err != nil {
		s.log.Warn("mcp aggregator handshake read failed", "error", err)
		_ = conn.Close()
		return
	}
	var hs handshake
	if err := json.Unmarshal(line, &hs); err != nil {
		s.log.Warn("mcp aggregator handshake decode failed", "error", err)
		_ = conn.Close()
		return
	}
	s.mu.Lock()
	sess, ok := s.sessions[hs.Token]
	s.mu.Unlock()
	if !ok {
		// Unknown or already-unregistered token: refuse silently (no oracle).
		s.log.Warn("mcp aggregator rejected unknown token")
		_ = conn.Close()
		return
	}

	runCtx := ctx
	if sess.WithContext != nil {
		runCtx = sess.WithContext(runCtx)
	}
	transport := &mcpsdk.IOTransport{
		Reader: connReader{r: br, c: conn},
		Writer: conn,
	}
	server := mcp.NewFromTools(sess.Tools, sess.Approver).Server()
	if err := server.Run(runCtx, transport); err != nil && !errors.Is(err, io.EOF) {
		s.log.Warn("mcp aggregator session ended", "error", err)
	}
}

// connReader adapts a buffered reader plus the underlying closer into the
// io.ReadCloser the IOTransport wants, so handshake-buffered bytes are not lost.
type connReader struct {
	r io.Reader
	c io.Closer
}

func (cr connReader) Read(p []byte) (int, error) { return cr.r.Read(p) }
func (cr connReader) Close() error               { return cr.c.Close() }

// RunBridge is the `murtaugh mcp-bridge` subcommand body. It dials the gateway
// socket, presents token, and byte-pipes in (the agent's stdin) and out (the
// agent's stdout) to the connection until either side closes. It speaks no MCP
// itself — it is a transparent pipe, so the gateway's MCP server and the agent's
// MCP client talk directly.
func RunBridge(ctx context.Context, socketPath, token string, in io.Reader, out io.Writer) error {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return fmt.Errorf("dial aggregator socket: %w", err)
	}
	defer func() { _ = conn.Close() }()

	line, err := json.Marshal(handshake{Token: token})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("send handshake: %w", err)
	}

	// Close the connection when ctx is cancelled so the copies unblock.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	errc := make(chan error, 2)
	go func() { _, err := io.Copy(conn, in); errc <- err }()  // agent -> gateway
	go func() { _, err := io.Copy(out, conn); errc <- err }() // gateway -> agent
	// Return as soon as either direction ends; the deferred Close tears down the
	// other copy.
	err = <-errc
	if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// newToken returns an unguessable session token.
func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
