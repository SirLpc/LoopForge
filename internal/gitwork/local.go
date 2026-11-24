package gitwork

import (
	"context"
	"fmt"
	"io"
	"os/exec"
)

type localGitKey struct{}

// WithLocalGit scopes an alternate local Git implementation to a workflow.
// Returning handled=false lets the existing Git path execute the command.
func WithLocalGit(ctx context.Context, run func(context.Context, string, io.Reader, []string) ([]byte, error, bool)) context.Context {
	return context.WithValue(ctx, localGitKey{}, run)
}

func alternateLocalGit(ctx context.Context, workspace string, input io.Reader, args []string) ([]byte, error, bool) {
	if run, ok := ctx.Value(localGitKey{}).(func(context.Context, string, io.Reader, []string) ([]byte, error, bool)); ok {
		return run(ctx, workspace, input, args)
	}
	return nil, nil, false
}

// LocalGit executes repository-local commands while preserving their raw output.
func LocalGit(ctx context.Context, workspace string, input io.Reader, args ...string) ([]byte, error) {
	if out, err, handled := alternateLocalGit(ctx, workspace, input, args); handled {
		return out, err
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	cmd.Stdin = input
	out, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); ok && len(exit.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, exit.Stderr)
	}
	return out, err
}
