// Package version implements Murtaugh's version-reporting tool. It returns the
// compile-time version string of the running binary with no error, and is
// strictly read-only — it inspects nothing and changes nothing.
package version

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
)

// Tool reports the running Murtaugh binary version.
type Tool struct {
	version string
}

// New constructs a version Tool bound to the compile-time version string.
func New(version string) *Tool { return &Tool{version: version} }

// Name returns the tool's identifier.
func (t *Tool) Name() string { return "version" }

// Description returns a short, human-readable description of the tool.
func (t *Tool) Description() string {
	return "Report the running Murtaugh binary version. Read-only; takes no parameters."
}

// InputSchema returns nil — version takes no parameters.
func (t *Tool) InputSchema() *jsonschema.Schema { return nil }

// Result is the logical result the version tool returns.
type Result struct {
	Version string `json:"version"`
}

// String renders the result as the bare version string.
func (r Result) String() string { return r.Version }

// Invoke returns the configured version with no error.
func (t *Tool) Invoke(_ context.Context, _ map[string]any) (any, error) {
	return Result{Version: t.version}, nil
}
