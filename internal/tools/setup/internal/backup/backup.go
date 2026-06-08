// Package backup centralises the "copy file aside with a timestamp suffix
// before overwriting" convention shared by every setup.* tool that writes a
// user-managed config file. Keeping this in one place ensures the suffix
// format (".bak.YYYYMMDDHHMMSS") matches what install.sh produced before the
// installer logic was moved into Murtaugh proper.
package backup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"
)

// Now is overridden in tests so the suffix is deterministic. It defaults to
// time.Now and must always be set to a non-nil value.
var Now = time.Now

// IfExists copies path to "<path>.bak.<timestamp>" when path is a regular
// file, returning the new backup path. It returns an empty string with a nil
// error when path does not exist (a fresh install).
//
// The destination's permission bits are preserved so the backup is as
// restrictive as the original (config files are written with 0600).
func IfExists(path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("backup target %q is not a regular file", path)
	}

	suffix := Now().Format("20060102150405")
	backupPath := path + ".bak." + suffix
	if err := copyFile(path, backupPath, info.Mode().Perm()); err != nil {
		return "", err
	}
	return backupPath, nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %q: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return fmt.Errorf("create %q: %w", dst, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return fmt.Errorf("copy to %q: %w", dst, err)
	}
	if err := out.Close(); err != nil {
		return fmt.Errorf("close %q: %w", dst, err)
	}
	return nil
}
