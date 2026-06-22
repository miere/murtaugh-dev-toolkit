// Package tools defines the Tool abstraction that every Murtaugh capability
// implements, together with a small in-memory registry shared by the CLI and
// MCP frontends.
package tools

import (
	"context"

	"github.com/google/jsonschema-go/jsonschema"
)

// Tool is the protocol-agnostic contract that every capability exposed by
// Murtaugh's CLI and MCP frontends must satisfy. Frontends consume tools only
// through this interface and never reach into a tool's concrete type.
//
// Args passed to Invoke are keyed by the property names declared in the tool's
// InputSchema. A nil InputSchema means the tool takes no parameters; frontends
// must invoke such tools with a nil or empty map.
type Tool interface {
	Name() string
	Description() string
	InputSchema() *jsonschema.Schema
	Invoke(ctx context.Context, args map[string]any) (any, error)
}

// ApprovalClassifier is implemented by tools whose invocations may be
// side-effecting and should be gated behind human approval. The native loop's
// approval gate type-asserts it: a tool that does not implement it is never
// gated, and a tool that does decides per-call (from its own policy and the
// args) whether THIS invocation needs the user's go-ahead before running.
type ApprovalClassifier interface {
	RequiresApproval(args map[string]any) bool
}

// Registry holds the set of tools available to the application. It is
// constructed by the composition root (internal/app) and handed to each
// frontend.
type Registry struct {
	tools map[string]Tool
	order []string
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds t to the registry. Registering two tools with the same name
// is treated as a programming error and panics; callers control the input set.
func (r *Registry) Register(t Tool) {
	name := t.Name()
	if _, exists := r.tools[name]; exists {
		panic("tools: duplicate tool registration: " + name)
	}
	r.tools[name] = t
	r.order = append(r.order, name)
}

// Get returns the tool registered under name, if any.
func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// All returns the registered tools in registration order.
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}
