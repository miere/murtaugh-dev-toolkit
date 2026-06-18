//go:build unix

package terminal

import (
	"os/exec"
	"syscall"
)

// configureProcessGroup puts the command in its own process group and, on
// timeout/cancel, SIGKILLs the entire group. Killing the group (negative PID)
// reaps grandchildren the shell may have left behind — a backgrounded process,
// or dash on Linux forking the command instead of exec-ing it — which would
// otherwise hold the output pipe open and block cmd.Run past the deadline.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
}
