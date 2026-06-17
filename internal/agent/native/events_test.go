package native

import "testing"

func TestToolResultString(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"nil", nil, ""},
		{"string passthrough", "plain text", "plain text"},
		{"map marshals to json", map[string]any{"ok": true}, `{"ok":true}`},
		{"slice marshals to json", []int{1, 2}, "[1,2]"},
		{"number marshals", 42, "42"},
		{"unmarshalable falls back to stringer", failJSON{}, "from-stringer"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toolResultString(tt.in); got != tt.want {
				t.Fatalf("toolResultString(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// failJSON cannot be JSON-marshalled (channels are unsupported) but is a
// fmt.Stringer, exercising the fallback path.
type failJSON struct{}

func (failJSON) String() string { return "from-stringer" }
func (failJSON) MarshalJSON() ([]byte, error) {
	return nil, errMarshal
}

var errMarshal = errMarshalT("boom")

type errMarshalT string

func (e errMarshalT) Error() string { return string(e) }

func TestDecodeToolArgs(t *testing.T) {
	if m, err := decodeToolArgs(nil); err != nil || m != nil {
		t.Fatalf("nil args: m=%v err=%v", m, err)
	}
	if m, err := decodeToolArgs([]byte("null")); err != nil || m != nil {
		t.Fatalf("null args: m=%v err=%v", m, err)
	}
	m, err := decodeToolArgs([]byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if m["x"].(float64) != 1 {
		t.Fatalf("got %#v", m)
	}
	if _, err := decodeToolArgs([]byte(`{bad`)); err == nil {
		t.Fatal("expected error on invalid json")
	}
}
