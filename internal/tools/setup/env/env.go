// Package env implements the `setup.env` tool: upsert secrets into
// ~/.config/murtaugh/.env without disturbing existing entries. It is the generic
// credential writer the installer uses for LLM provider keys (and anything else
// that must live in the dotenv rather than in YAML).
package env

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/internal/envfile"
)

// PathProvider returns the absolute path of the .env file.
type PathProvider func() string

// Tool is the `setup.env` capability.
type Tool struct {
	path PathProvider
}

// New constructs a Tool that writes the .env at the path returned by path.
func New(path PathProvider) *Tool { return &Tool{path: path} }

// Name returns the registry key.
func (t *Tool) Name() string { return "setup.env" }

// Description returns the human-facing summary used by MCP clients.
func (t *Tool) Description() string {
	return "Upsert KEY=VALUE secrets into ~/.config/murtaugh/.env (other entries preserved)."
}

// InputSchema returns the JSON Schema for the tool's arguments.
func (t *Tool) InputSchema() *jsonschema.Schema {
	return &jsonschema.Schema{
		Type: "object",
		Properties: map[string]*jsonschema.Schema{
			"set": {
				Type:        "array",
				Items:       &jsonschema.Schema{Type: "string"},
				Description: "KEY=VALUE pair to upsert. Repeat for several. The value is written verbatim.",
			},
		},
		Required: []string{"set"},
	}
}

// Result is the structured payload returned by Invoke. Only key NAMES are
// reported — never the secret values.
type Result struct {
	Path       string   `json:"path"`
	Created    bool     `json:"created"`
	BackupPath string   `json:"backup_path,omitempty"`
	Keys       []string `json:"keys"`
}

// String renders a one-line CLI confirmation that never echoes a secret value.
func (r Result) String() string {
	verb := "updated"
	if r.Created {
		verb = "created"
	}
	msg := fmt.Sprintf("%s %s (%s)", verb, r.Path, strings.Join(r.Keys, ", "))
	if r.BackupPath != "" {
		msg += " (backup: " + r.BackupPath + ")"
	}
	return msg
}

// Invoke parses the KEY=VALUE pairs and merges them into the .env file.
func (t *Tool) Invoke(_ context.Context, args map[string]any) (any, error) {
	pairs, err := coerceStringSlice(args["set"])
	if err != nil {
		return nil, fmt.Errorf("set: %w", err)
	}
	if len(pairs) == 0 {
		return nil, errors.New("set is required (one or more KEY=VALUE pairs)")
	}

	kv := make(map[string]string, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return nil, fmt.Errorf("invalid pair %q: want KEY=VALUE", pair)
		}
		kv[key] = value
	}

	path := t.path()
	if strings.TrimSpace(path) == "" {
		return nil, errors.New(".env path is not configured")
	}
	backupPath, err := envfile.Merge(path, kv)
	if err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return Result{Path: path, Created: backupPath == "", BackupPath: backupPath, Keys: keys}, nil
}

// coerceStringSlice accepts the array shapes the CLI/MCP frontends produce.
func coerceStringSlice(v any) ([]string, error) {
	switch xs := v.(type) {
	case nil:
		return nil, nil
	case []string:
		return xs, nil
	case []any:
		out := make([]string, 0, len(xs))
		for i, e := range xs {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("element %d is %T, want string", i, e)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("expected array of strings, got %T", v)
	}
}
