package gitwork

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

var ErrDiffIndexSnapshot = errors.New("could not create a private Git diff index")

// DisableOptionalIndexWrites configures Git queries in a child environment
// while preserving task-scoped configuration such as URL rewrites.
func DisableOptionalIndexWrites(environment map[string]string) {
	count, _ := strconv.Atoi(environment["GIT_CONFIG_COUNT"])
	suffix := strconv.Itoa(count)
	environment["GIT_CONFIG_COUNT"] = strconv.Itoa(count + 1)
	environment["GIT_CONFIG_KEY_"+suffix] = "diff.autoRefreshIndex"
	environment["GIT_CONFIG_VALUE_"+suffix] = "false"
	environment["GIT_OPTIONAL_LOCKS"] = "0"
}

// LocalDiff reads the worktree without refreshing the shared index.
func LocalDiff(ctx context.Context, workspace string, args ...string) ([]byte, error) {
	args = append([]string{"--no-optional-locks", "-c", "diff.autoRefreshIndex=true", "diff"}, args...)
	if out, err, handled := alternateLocalGit(ctx, workspace, nil, args); handled {
		return out, err
	}
	// Git uses the same setting for suppressing stat-only diffs and writing
	// refreshed stat data. Let it do both against a disposable index.
	path, err := LocalGit(ctx, workspace, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "loopforge-diff-index-")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDiffIndexSnapshot, err)
	}
	defer os.RemoveAll(dir)
	index := filepath.Join(dir, "index")
	if err := copyDiffIndex(index, strings.TrimSuffix(string(path), "\n")); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDiffIndexSnapshot, err)
	}
	cmd := localGitCommand(ctx, workspace, nil, args)
	cmd.Env = WithEnvValue(os.Environ(), "GIT_INDEX_FILE", index)
	return localGitOutput(cmd)
}

func copyDiffIndex(path, sourcePath string) error {
	fd, err := unix.Open(sourcePath, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	source := os.NewFile(uintptr(fd), sourcePath)
	defer source.Close()
	info, err := source.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("Git index is not a regular file")
	}
	index, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(index, source)
	closeErr := index.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	// Git uses the index mtime to detect files changed within the index's
	// timestamp granularity, even if their size and mtime still match.
	return os.Chtimes(path, info.ModTime(), info.ModTime())
}

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
	return localGitOutput(localGitCommand(ctx, workspace, input, args))
}

func localGitCommand(ctx context.Context, workspace string, input io.Reader, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = workspace
	cmd.Stdin = input
	return cmd
}

func localGitOutput(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); ok && len(exit.Stderr) > 0 {
		return out, fmt.Errorf("%w: %s", err, exit.Stderr)
	}
	return out, err
}
