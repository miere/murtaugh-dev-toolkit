// Package cli implements Murtaugh's human-facing frontend. It maps
// subcommands to tools registered in the shared tool registry and writes
// their results to stdout. Diagnostics go to stderr; stdout is reserved for
// tool output.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools"
)

// ErrUsage is returned when the user invokes the CLI without arguments or
// with an unknown command. Callers map it to a non-zero exit and a usage
// message on stderr.
var ErrUsage = errors.New("usage: murtaugh <command>")

// Frontend is the CLI adapter.
type Frontend struct {
	registry *tools.Registry
	stdout   io.Writer
	stderr   io.Writer
	// json toggles JSONL output: when set, results are JSON-marshalled
	// (one value per line) instead of rendered for humans. Driven by the
	// global --json flag stripped in main.
	json bool
}

// New constructs a CLI Frontend that writes to os.Stdout and os.Stderr.
func New(reg *tools.Registry) *Frontend {
	return &Frontend{registry: reg, stdout: os.Stdout, stderr: os.Stderr}
}

// WithOutput overrides the output streams; intended for tests.
func (f *Frontend) WithOutput(stdout, stderr io.Writer) *Frontend {
	f.stdout, f.stderr = stdout, stderr
	return f
}

// WithJSON enables JSONL output when on is true. Returns the receiver for
// fluent wiring.
func (f *Frontend) WithJSON(on bool) *Frontend {
	f.json = on
	return f
}

// Run executes the command described by args. It first tries to resolve
// args[0] as a flat tool name; if that misses and args[1] is present, it
// retries with the dotted form "<args[0]>.<args[1]>" — the convention for
// namespaced subcommands (e.g. `murtaugh jobs run` → "jobs.run").
//
// Remaining tokens after the resolved name are parsed as --flag VALUE pairs
// against the tool's InputSchema and passed as the args map. The tool's
// result is rendered via Render and written to stdout followed by a newline.
// Diagnostics go to stderr; stdout is reserved for tool output.
func (f *Frontend) Run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return ErrUsage
	}
	name, rest, err := f.resolve(args)
	if err != nil {
		return err
	}
	tool, _ := f.registry.Get(name)
	parsed, err := parseFlags(tool.InputSchema(), rest)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	result, err := tool.Invoke(ctx, parsed)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if f.json {
		return f.renderJSON(result)
	}
	if _, err := fmt.Fprintln(f.stdout, Render(result)); err != nil {
		return err
	}
	return nil
}

// renderJSON writes the tool result as JSONL: a nil result prints nothing; a
// slice/array (other than []byte) prints one compact JSON value per line; any
// other value prints as a single compact JSON line. Marshal errors are
// returned so they surface on stderr with a non-zero exit.
func (f *Frontend) renderJSON(result any) error {
	if result == nil {
		return nil
	}
	rv := reflect.ValueOf(result)
	if rv.Kind() == reflect.Slice && rv.Type().Elem().Kind() != reflect.Uint8 {
		for i := 0; i < rv.Len(); i++ {
			if err := f.writeJSONLine(rv.Index(i).Interface()); err != nil {
				return err
			}
		}
		return nil
	}
	return f.writeJSONLine(result)
}

// writeJSONLine marshals v compactly and writes it followed by a newline.
func (f *Frontend) writeJSONLine(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f.stdout, string(b)); err != nil {
		return err
	}
	return nil
}

// resolve picks the registered tool name out of args and returns the
// remaining tokens to pass to flag parsing. Flat lookup is tried first; if
// it misses and a second token is present, a dotted "<a>.<b>" lookup is
// tried.
func (f *Frontend) resolve(args []string) (string, []string, error) {
	if _, ok := f.registry.Get(args[0]); ok {
		return args[0], args[1:], nil
	}
	if len(args) >= 2 {
		dotted := args[0] + "." + args[1]
		if _, ok := f.registry.Get(dotted); ok {
			return dotted, args[2:], nil
		}
	}
	return "", nil, fmt.Errorf("unknown command: %s (run `murtaugh help` to list commands)", args[0])
}
