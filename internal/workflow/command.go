package workflow

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	"github.com/miere/murtaugh-dev-toolkit/internal/config"
)

type CommandRunner interface {
	Run(context.Context, config.RunTriggerConfig, []byte) ([]byte, error)
}

type OSCommandRunner struct{}

func (OSCommandRunner) Run(ctx context.Context, command config.RunTriggerConfig, input []byte) ([]byte, error) {
	timeout := commandTimeout(command.Timeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, command.Cmd, command.Args...)
	cmd.Dir = command.WorkDir
	cmd.Stdin = bytes.NewReader(input)

	var stdout bytes.Buffer
	cmd.Stdout = &stdout

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("command timed out after %s", timeout)
		}
		return nil, fmt.Errorf("command failed: %w", err)
	}
	return stdout.Bytes(), nil
}

func commandTimeout(raw string) time.Duration {
	if raw == "" {
		return 30 * time.Second
	}
	timeout, err := time.ParseDuration(raw)
	if err != nil || timeout <= 0 {
		return 30 * time.Second
	}
	return timeout
}
