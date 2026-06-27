// Package bundle implements the `troubleshoot.bundle` tool: assemble a
// redacted diagnostics bundle (journal snapshot, logs, configs, optional
// provider artifacts, manifest, and AI instructions) into a zip. It is a thin
// wrapper over internal/troubleshoot so the CLI, MCP, and the Slack gateway all
// share one deterministic bundler.
package bundle

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh/internal/troubleshoot"
)

// SourcesProvider returns the resolved on-disk read locations for the bundle.
// The composition root supplies a closure over the loaded config so the tool
// always observes current paths.
type SourcesProvider func() troubleshoot.Sources

// Tool is the `troubleshoot.bundle` capability.
type Tool struct {
	sources SourcesProvider
	// defaultProviders supplies the provider list used when the caller passes no
	// `include` argument (the configured set from troubleshoot.yaml, else all
	// known providers). nil yields no default.
	defaultProviders func() []string
}

// New constructs a Tool that resolves its read locations via sources and falls
// back to defaultProviders when no `include` argument is given. defaultProviders
// may be nil.
func New(sources SourcesProvider, defaultProviders func() []string) *Tool {
	return &Tool{sources: sources, defaultProviders: defaultProviders}
}

// Name returns the registry key.
func (t *Tool) Name() string { return "troubleshoot.bundle" }

// Description returns the human/MCP-facing summary.
func (t *Tool) Description() string {
	return "Assemble a redacted diagnostics bundle (journal snapshot, logs, configs, optional provider files, manifest) into a zip for troubleshooting."
}

// InputSchema describes the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"note":          {Type: "string", Description: "Free-text description of the symptoms; recorded in the manifest."},
			"include":       {Type: "array", Items: &jsonschema.Schema{Type: "string"}, Description: fmt.Sprintf("Downstream providers whose on-disk diagnostics to include (known: %s). Repeat to add several.", strings.Join(troubleshoot.KnownProviders(), ", "))},
			"out":           {Type: "string", Description: "Optional output path for the zip. Defaults to a timestamped file in the temp dir."},
			"max_log_bytes": {Type: "integer", Description: "Tail cap per log file in bytes. Defaults to 5 MiB when omitted or zero."},
			"redact":        {Type: "boolean", Description: "Redact known secrets (Slack tokens, secret-looking config values). Defaults to true; only set false for local-only use."},
		},
	}
}

// Result is the structured payload returned by Invoke.
type Result struct {
	Path      string   `json:"path"`
	Bytes     int64    `json:"bytes"`
	FileCount int      `json:"file_count"`
	Providers []string `json:"providers,omitempty"`
	Redacted  bool     `json:"redacted"`
	Warnings  []string `json:"warnings,omitempty"`
}

// String renders a one-line (plus warnings) CLI confirmation.
func (r Result) String() string {
	var b strings.Builder
	redaction := "redacted"
	if !r.Redacted {
		redaction = "UNREDACTED"
	}
	fmt.Fprintf(&b, "wrote troubleshooting bundle to %s (%d files, %d bytes, %s)", r.Path, r.FileCount, r.Bytes, redaction)
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "\n  ! %s", w)
	}
	return b.String()
}

// Invoke builds the bundle and returns where it landed.
func (t *Tool) Invoke(ctx context.Context, args map[string]any) (any, error) {
	if t.sources == nil {
		return nil, fmt.Errorf("troubleshoot.bundle is not configured")
	}
	note, _ := args["note"].(string)
	out, _ := args["out"].(string)

	redact := true
	if v, ok := args["redact"].(bool); ok {
		redact = v
	}

	providers := stringSlice(args["include"])
	if len(providers) == 0 && t.defaultProviders != nil {
		providers = t.defaultProviders()
	}

	opts := troubleshoot.Options{
		Note:        note,
		Providers:   providers,
		MaxLogBytes: toInt64(args["max_log_bytes"]),
		NoRedact:    !redact,
		OutPath:     strings.TrimSpace(out),
	}

	res, err := troubleshoot.Build(ctx, opts, t.sources())
	if err != nil {
		return nil, err
	}
	return Result{
		Path:      res.Path,
		Bytes:     res.Bytes,
		FileCount: len(res.Manifest.Files),
		Providers: res.Manifest.Providers,
		Redacted:  res.Manifest.RedactionApplied,
		Warnings:  res.Manifest.Errors,
	}, nil
}

// stringSlice coerces an arg into []string, tolerating the []any the MCP
// frontend delivers, the []string the CLI delivers, and a lone string.
func stringSlice(v any) []string {
	switch t := v.(type) {
	case nil:
		return nil
	case []string:
		return trimmedNonEmpty(t)
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return trimmedNonEmpty(out)
	case string:
		return trimmedNonEmpty([]string{t})
	default:
		return nil
	}
}

func trimmedNonEmpty(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// toInt64 coerces a numeric arg, tolerating int (CLI) and float64 (MCP JSON).
func toInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	default:
		return 0
	}
}
