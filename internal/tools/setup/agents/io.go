package agents

import (
	"fmt"
	"strconv"
	"strings"
)

// coerceInt accepts the shapes the CLI (string) and MCP-JSON (float64/int)
// frontends produce for an integer property. Empty/nil yields 0.
func coerceInt(v any) (int, error) {
	switch n := v.(type) {
	case nil:
		return 0, nil
	case int:
		return n, nil
	case int64:
		return int(n), nil
	case float64:
		return int(n), nil
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, nil
		}
		i, err := strconv.Atoi(s)
		if err != nil {
			return 0, fmt.Errorf("expected an integer, got %q", n)
		}
		return i, nil
	default:
		return 0, fmt.Errorf("expected an integer, got %T", v)
	}
}

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
