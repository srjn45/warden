package backendusage

import (
	"context"
	"os/exec"
)

type CommandRunner interface {
	Output(ctx context.Context, binary string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Output(ctx context.Context, binary string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, binary, args...).Output()
}
