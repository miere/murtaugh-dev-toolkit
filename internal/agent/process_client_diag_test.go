package agent

import (
	"encoding/json"
	"testing"
)

func TestExtractStopReason(t *testing.T) {
	cases := map[string]string{
		`{"stopReason":"end_turn"}`:      "end_turn",
		`{"stop_reason":"max_tokens"}`:   "max_tokens",
		`{"stopReason":""}`:              "",
		`{"other":"x"}`:                  "",
		``:                               "",
		`{"stopReason":"refusal","x":1}`: "refusal",
	}
	for raw, want := range cases {
		if got := extractStopReason(json.RawMessage(raw)); got != want {
			t.Errorf("extractStopReason(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestSessionUpdateKind(t *testing.T) {
	cases := map[string]string{
		`{"sessionId":"s","update":{"sessionUpdate":"agent_message_chunk"}}`: "agent_message_chunk",
		`{"update":{"sessionUpdate":"tool_call"}}`:                           "tool_call",
		`{"update":{}}`:   "",
		`{"no":"update"}`: "",
	}
	for raw, want := range cases {
		if got := sessionUpdateKind(json.RawMessage(raw)); got != want {
			t.Errorf("sessionUpdateKind(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestKnownSilentUpdate(t *testing.T) {
	for _, k := range []string{"agent_thought_chunk", "tool_call", "tool_call_update", "plan"} {
		if !knownSilentUpdate(k) {
			t.Errorf("%q should be a known silent update", k)
		}
	}
	for _, k := range []string{"agent_message_chunk", "some_new_kind", ""} {
		if knownSilentUpdate(k) {
			t.Errorf("%q should NOT be treated as silent", k)
		}
	}
}

func TestCarriesText(t *testing.T) {
	if !carriesText(json.RawMessage(`{"update":{"content":{"type":"text","text":"hi"}}}`)) {
		t.Error("expected carriesText=true for update.content with text")
	}
	if carriesText(json.RawMessage(`{"update":{"sessionUpdate":"plan"}}`)) {
		t.Error("expected carriesText=false when update.content is absent")
	}
	if carriesText(json.RawMessage(`{"update":{"content":{"type":"text","text":"   "}}}`)) {
		t.Error("expected carriesText=false for whitespace-only content")
	}
}
