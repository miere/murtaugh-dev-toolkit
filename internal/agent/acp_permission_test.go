package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeAsker struct {
	called bool
	loc    TurnLocation
	req    PermissionRequest
	ret    string
}

func (f *fakeAsker) AskPermission(_ context.Context, loc TurnLocation, req PermissionRequest) (string, error) {
	f.called = true
	f.loc = loc
	f.req = req
	return f.ret, nil
}

// runAgentRequest feeds one agent→client request line through readLoop on a client
// configured with opts (and an optional seeded per-session scope map), then returns
// the single JSON-RPC response the client writes back to the agent.
func runAgentRequest(t *testing.T, opts ProcessOptions, dests map[string]promptScope, line string) map[string]any {
	t.Helper()
	pr, pw := io.Pipe()
	c := &ProcessClient{
		opts:        opts,
		log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		pending:     make(map[int64]chan rpcResponse),
		subscribers: make(map[string]chan Event),
		dests:       make(map[string]promptScope),
		stdin:       pw,
		started:     true,
	}
	for k, v := range dests {
		c.dests[k] = v
	}
	go c.readLoop(strings.NewReader(line + "\n"))

	type result struct {
		m   map[string]any
		err error
	}
	done := make(chan result, 1)
	go func() {
		var m map[string]any
		err := json.NewDecoder(pr).Decode(&m)
		done <- result{m, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("decode ACP response: %v", r.err)
		}
		return r.m
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ACP response")
		return nil
	}
}

func outcomeOf(t *testing.T, resp map[string]any) map[string]any {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %v", resp)
	}
	out, ok := res["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("result has no outcome object: %v", res)
	}
	return out
}

const permReqAllowDeny = `{"jsonrpc":"2.0","id":1,"method":"session/request_permission",` +
	`"params":{"sessionId":"S1","toolCall":{"title":"Edit agents.yaml"},` +
	`"options":[{"optionId":"a","name":"Allow","kind":"allow_once"},` +
	`{"optionId":"d","name":"Reject","kind":"reject_once"}]}}`

func TestACPPermissionAutoAllow(t *testing.T) {
	resp := runAgentRequest(t, ProcessOptions{PermissionPolicy: "auto-allow"}, nil, permReqAllowDeny)
	out := outcomeOf(t, resp)
	if out["outcome"] != "selected" || out["optionId"] != "a" {
		t.Fatalf("auto-allow: expected selected a, got %v", out)
	}
}

func TestACPPermissionAutoDenyCancelsWhenNoRejectOption(t *testing.T) {
	line := `{"jsonrpc":"2.0","id":2,"method":"session/request_permission",` +
		`"params":{"sessionId":"S1","options":[{"optionId":"a","name":"Allow","kind":"allow_once"}]}}`
	resp := runAgentRequest(t, ProcessOptions{PermissionPolicy: "auto-deny"}, nil, line)
	out := outcomeOf(t, resp)
	if out["outcome"] != "cancelled" {
		t.Fatalf("auto-deny without a reject option should cancel, got %v", out)
	}
}

func TestACPPermissionAskRoutesToAsker(t *testing.T) {
	asker := &fakeAsker{ret: "a"}
	dests := map[string]promptScope{"S1": {loc: TurnLocation{ChannelID: "C1", ThreadTS: "T1"}, ctx: context.Background()}}
	resp := runAgentRequest(t, ProcessOptions{PermissionPolicy: "ask", PermissionAsker: asker}, dests, permReqAllowDeny)
	if !asker.called {
		t.Fatal("ask policy did not call the asker")
	}
	if asker.loc.ChannelID != "C1" || asker.loc.ThreadTS != "T1" {
		t.Fatalf("asker got wrong location: %+v", asker.loc)
	}
	if asker.req.ToolName != "Edit agents.yaml" || len(asker.req.Options) != 2 {
		t.Fatalf("asker got wrong request: %+v", asker.req)
	}
	out := outcomeOf(t, resp)
	if out["outcome"] != "selected" || out["optionId"] != "a" {
		t.Fatalf("ask: expected selected a, got %v", out)
	}
}

func TestACPPermissionAskWithoutAskerDenies(t *testing.T) {
	// ask policy on a headless path (no asker) must deny (cancelled), not hang.
	resp := runAgentRequest(t, ProcessOptions{PermissionPolicy: "ask"}, nil, permReqAllowDeny)
	out := outcomeOf(t, resp)
	if out["outcome"] != "cancelled" {
		t.Fatalf("ask without an asker should cancel, got %v", out)
	}
}

func TestACPPermissionEmptyPolicyDefaultsToAsk(t *testing.T) {
	asker := &fakeAsker{ret: "a"}
	dests := map[string]promptScope{"S1": {loc: TurnLocation{ChannelID: "C1"}, ctx: context.Background()}}
	resp := runAgentRequest(t, ProcessOptions{PermissionAsker: asker}, dests, permReqAllowDeny)
	if !asker.called {
		t.Fatal("empty policy should default to ask and call the asker")
	}
	if out := outcomeOf(t, resp); out["optionId"] != "a" {
		t.Fatalf("expected selected a, got %v", out)
	}
}

func TestACPUnhandledAgentRequestRepliesMethodNotFound(t *testing.T) {
	// terminal/create is a real ACP method Murtaugh does not serve; it must be
	// rejected fast so the agent fails instead of blocking on a reply we'd never
	// send. (fs/* is handled — see the filesystem tests below.)
	line := `{"jsonrpc":"2.0","id":5,"method":"terminal/create","params":{}}`
	resp := runAgentRequest(t, ProcessOptions{}, nil, line)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error response, got %v", resp)
	}
	if code, _ := errObj["code"].(float64); int(code) != jsonRPCMethodNotFound {
		t.Fatalf("expected method-not-found (%d), got %v", jsonRPCMethodNotFound, errObj["code"])
	}
}

func TestACPReadTextFileWithinWorkDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("line1\nline2\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	line := `{"jsonrpc":"2.0","id":7,"method":"fs/read_text_file","params":{"path":"` + filepath.Join(dir, "a.txt") + `"}}`
	resp := runAgentRequest(t, ProcessOptions{WorkDir: dir}, nil, line)
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected a result, got %v", resp)
	}
	if res["content"] != "line1\nline2\nline3\n" {
		t.Fatalf("unexpected content: %q", res["content"])
	}
}

func TestACPReadTextFileHonoursLineAndLimit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("l1\nl2\nl3\nl4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	line := `{"jsonrpc":"2.0","id":8,"method":"fs/read_text_file","params":{"path":"` + filepath.Join(dir, "a.txt") + `","line":2,"limit":2}}`
	resp := runAgentRequest(t, ProcessOptions{WorkDir: dir}, nil, line)
	res := resp["result"].(map[string]any)
	if res["content"] != "l2\nl3" {
		t.Fatalf("line/limit window wrong: %q", res["content"])
	}
}

func TestACPReadTextFileOutsideWorkDirRejected(t *testing.T) {
	dir := t.TempDir()
	// Escape the workdir via the parent — must be refused with invalid-params,
	// not served, so a read can never exfiltrate host files outside the project.
	line := `{"jsonrpc":"2.0","id":9,"method":"fs/read_text_file","params":{"path":"` + filepath.Join(dir, "..", "secret.txt") + `"}}`
	resp := runAgentRequest(t, ProcessOptions{WorkDir: dir}, nil, line)
	errObj, ok := resp["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected an error response, got %v", resp)
	}
	if code, _ := errObj["code"].(float64); int(code) != jsonRPCInvalidParams {
		t.Fatalf("expected invalid-params (%d), got %v", jsonRPCInvalidParams, errObj["code"])
	}
}

func TestACPWriteTextFileWithinWorkDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "out.txt")
	line := `{"jsonrpc":"2.0","id":10,"method":"fs/write_text_file","params":{"path":"` + target + `","content":"hello"}}`
	resp := runAgentRequest(t, ProcessOptions{WorkDir: dir}, nil, line)
	if _, isErr := resp["error"]; isErr {
		t.Fatalf("write returned an error: %v", resp["error"])
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("file was not written: %v", err)
	}
	if string(got) != "hello" {
		t.Fatalf("unexpected written content: %q", got)
	}
}

func TestPickOptionByKind(t *testing.T) {
	opts := []PermissionOption{
		{ID: "ao", Kind: "allow_once"},
		{ID: "aa", Kind: "allow_always"},
		{ID: "ro", Kind: "reject_once"},
	}
	if got := pickOptionByKind(opts, "allow"); got != "ao" {
		t.Fatalf("allow should prefer allow_once, got %q", got)
	}
	if got := pickOptionByKind(opts, "reject"); got != "ro" {
		t.Fatalf("reject should pick reject_once, got %q", got)
	}
	if got := pickOptionByKind([]PermissionOption{{ID: "x", Kind: "allow_once"}}, "reject"); got != "" {
		t.Fatalf("no reject option should yield \"\", got %q", got)
	}
}

func TestParsePermissionRequest(t *testing.T) {
	raw := json.RawMessage(`{"sessionId":"S9","toolCall":{"kind":"execute"},"options":[{"optionId":"o1","name":"Yes","kind":"allow_once"}]}`)
	sid, tool, opts := parsePermissionRequest(raw)
	if sid != "S9" {
		t.Fatalf("sessionID: got %q", sid)
	}
	if tool != "execute" { // falls back to kind when title is absent
		t.Fatalf("toolName: got %q", tool)
	}
	if len(opts) != 1 || opts[0].ID != "o1" || opts[0].Kind != "allow_once" {
		t.Fatalf("options: got %+v", opts)
	}
}
