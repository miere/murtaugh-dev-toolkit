// Package envfile is the shared writer for ~/.config/murtaugh/.env, the single
// home for Murtaugh's secrets (Slack tokens and LLM provider keys). YAML config
// only ever references these via ${VAR}, so the troubleshoot bundler — which
// collects the .yaml siblings, never .env — cannot leak them.
//
// Merge upserts keys while preserving the rest of the file (comments, blank
// lines, unrelated keys, and their order), so re-running setup never clobbers a
// hand-added variable.
package envfile

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/miere/murtaugh-dev-toolkit/internal/tools/setup/internal/backup"
)

// keyLine matches a dotenv assignment, capturing the variable name. An optional
// leading `export ` is tolerated. Indentation is not expected in a .env but is
// accepted defensively.
var keyLine = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)

// Merge upserts kv into the dotenv file at path. Existing keys are replaced in
// place (preserving their position); new keys are appended in sorted order. The
// file is written 0600. When it already exists it is backed up first and the
// backup path returned. A blank key is rejected; a value must not contain a
// newline.
func Merge(path string, kv map[string]string) (backupPath string, err error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("envfile: path is empty")
	}
	for k, v := range kv {
		if strings.TrimSpace(k) == "" {
			return "", fmt.Errorf("envfile: blank key")
		}
		if strings.ContainsAny(v, "\r\n") {
			return "", fmt.Errorf("envfile: value for %q contains a newline", k)
		}
	}

	var existing []byte
	if data, readErr := os.ReadFile(path); readErr == nil {
		existing = data
	} else if !os.IsNotExist(readErr) {
		return "", fmt.Errorf("envfile: read %q: %w", path, readErr)
	}

	pending := make(map[string]string, len(kv))
	for k, v := range kv {
		pending[k] = v
	}

	var out bytes.Buffer
	if len(existing) > 0 {
		scanner := bufio.NewScanner(bytes.NewReader(existing))
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if m := keyLine.FindStringSubmatch(line); m != nil {
				if v, ok := pending[m[1]]; ok {
					out.WriteString(m[1] + "=" + v + "\n")
					delete(pending, m[1])
					continue
				}
			}
			out.WriteString(line + "\n")
		}
		if scanErr := scanner.Err(); scanErr != nil {
			return "", fmt.Errorf("envfile: scan %q: %w", path, scanErr)
		}
	}

	if len(pending) > 0 {
		// Keep a trailing separator only when appending to existing content.
		if out.Len() > 0 && !bytes.HasSuffix(out.Bytes(), []byte("\n\n")) {
			out.WriteString("\n")
		}
		keys := make([]string, 0, len(pending))
		for k := range pending {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out.WriteString(k + "=" + pending[k] + "\n")
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("envfile: ensure dir: %w", err)
	}
	backupPath, err = backup.IfExists(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		return "", fmt.Errorf("envfile: write %q: %w", path, err)
	}
	return backupPath, nil
}
