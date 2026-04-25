package providerutil

import (
	"bytes"
	"context"
	"os"
	"os/exec"
)

// CommandResult is the captured output from a completed CLI command.
type CommandResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// RunCommand runs a CLI command with stdout and stderr captured separately.
func RunCommand(ctx context.Context, name string, args ...string) (CommandResult, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return CommandResult{ExitCode: -1}, err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	hideConsoleWindow(cmd)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	result := CommandResult{
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		ExitCode: 0,
	}
	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return result, nil
		}
		return result, err
	}
	return result, nil
}
