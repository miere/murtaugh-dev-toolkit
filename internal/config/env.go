package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"
)

// EnvFileName is the dotenv file Murtaugh loads from the config directory. It
// holds every credential — provider API keys AND the Slack tokens — so the YAML
// files only ever reference them via ${VAR}. Keeping secrets out of YAML is what
// lets the troubleshoot bundler ship config files safely (it collects the YAML
// siblings, never .env).
const EnvFileName = ".env"

// LoadDotEnv loads <dir>/.env into the process environment if it exists, without
// overriding variables already set in the real environment (an operator-exported
// value wins over the file — standard dotenv precedence). A missing file is not
// an error: .env is optional and secrets may instead come from the ambient
// environment (e.g. launchd, a secrets manager). Returns an error only when the
// file exists but cannot be read/parsed.
func LoadDotEnv(dir string) error {
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, EnvFileName)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	if err := godotenv.Load(path); err != nil {
		return fmt.Errorf("load %s: %w", path, err)
	}
	return nil
}
