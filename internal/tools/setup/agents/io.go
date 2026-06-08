package agents

import "fmt"

// coerceStringSlice accepts the same shapes the JSON-MCP and CLI frontends
// hand over for an array-of-strings input: nil, []string, or []any. Any
// non-string element is rejected.
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
