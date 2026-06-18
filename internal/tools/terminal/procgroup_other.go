//go:build !unix

package terminal

import "os/exec"

// configureProcessGroup is a no-op where POSIX process groups are unavailable;
// exec.CommandContext's default kill still terminates the direct child, and
// cmd.WaitDelay bounds how long Run waits on a lingering pipe holder.
func configureProcessGroup(cmd *exec.Cmd) {}
