package define

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/miere/murtaugh/internal/config"
)

const filePerm = 0o644

// stringSlice coerces an args value (nil, []string, or []any from JSON
// decoding) into []string. Any non-string element is rejected.
func stringSlice(v any) ([]string, error) {
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

// readJobs loads the existing jobs map from disk, returning a nil map when
// the file does not exist yet.
func readJobs(path string) (map[string]config.JobProfile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", path, err)
	}
	var doc struct {
		Jobs map[string]config.JobProfile `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %q: %w", path, err)
	}
	return doc.Jobs, nil
}

// writeJobs serialises the jobs map under a top-level `jobs:` key and
// replaces path.
func writeJobs(path string, jobs map[string]config.JobProfile) error {
	doc := struct {
		Jobs map[string]config.JobProfile `yaml:"jobs"`
	}{Jobs: jobs}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal jobs: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("ensure dir %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, out, filePerm); err != nil {
		return fmt.Errorf("write %q: %w", path, err)
	}
	return nil
}
