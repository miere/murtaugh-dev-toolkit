package update

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/miere/murtaugh/internal/tools/setup/internal/backup"
)

// releaseURL builds the GitHub API URL for either a specific tag or the
// "latest" release when target is blank.
func releaseURL(owner, repo, target string) string {
	if strings.TrimSpace(target) == "" {
		return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/latest", owner, repo)
	}
	return fmt.Sprintf("https://api.github.com/repos/%s/%s/releases/tags/%s", owner, repo, target)
}

// findAsset returns the (tag, asset URL) pair matching the platform suffix
// `${GOOS}-${GOARCH}` exposed by the release JSON's tag_name + assets.
func findAsset(body []byte, goos, goarch string) (string, string, error) {
	var doc struct {
		TagName string `json:"tag_name"`
		Assets  []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", fmt.Errorf("parse release JSON: %w", err)
	}
	want := fmt.Sprintf("murtaugh-%s-%s-%s", doc.TagName, goos, goarch)
	for _, a := range doc.Assets {
		if a.Name == want {
			return doc.TagName, a.URL, nil
		}
	}
	return "", "", fmt.Errorf("release %s has no asset for %s-%s (want %q)", doc.TagName, goos, goarch, want)
}

// installAsset writes asset to disk, runs the verify hook against it, and on
// success swaps it into the running binary path with a backup. A verify
// failure leaves the original binary untouched.
func (t *Tool) installAsset(current, tag string, asset []byte) (Result, error) {
	binPath, err := t.deps.CurrentBinary()
	if err != nil {
		return Result{}, fmt.Errorf("locate current binary: %w", err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(binPath), "murtaugh-update-*")
	if err != nil {
		return Result{}, fmt.Errorf("stage update: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }
	if _, err := tmp.Write(asset); err != nil {
		tmp.Close()
		cleanup()
		return Result{}, fmt.Errorf("write staged update: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return Result{}, fmt.Errorf("close staged update: %w", err)
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		cleanup()
		return Result{}, fmt.Errorf("chmod staged update: %w", err)
	}
	if t.deps.VerifyBinary != nil {
		if err := t.deps.VerifyBinary(tmpPath); err != nil {
			cleanup()
			return Result{}, fmt.Errorf("verify update: %w", err)
		}
	}

	backupPath, err := backup.IfExists(binPath)
	if err != nil {
		cleanup()
		return Result{}, err
	}
	if err := os.Rename(tmpPath, binPath); err != nil {
		cleanup()
		return Result{}, fmt.Errorf("install %q: %w", binPath, err)
	}
	return Result{
		CurrentVersion: current,
		TargetVersion:  tag,
		BinaryPath:     binPath,
		BackupPath:     backupPath,
		Skipped:        false,
	}, nil
}

// equalVersions reports whether two version tags refer to the same release,
// tolerating a leading "v" on either side.
func equalVersions(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}
