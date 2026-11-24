package runtime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func RunNativeWarmGit(ctx context.Context, workspace, executable string, input []byte, writable, network bool, isolation *NativeWarmIsolation) ([]byte, error) {
	return runNativeWarmGit(ctx, workspace, executable, input, writable, network, isolation, nil)
}

// RunNativeWarmGitSession exchanges requests with the trusted helper over its
// own standard streams. No Windows executable or interop endpoint is mounted.
func RunNativeWarmGitSession(ctx context.Context, workspace, executable string, writable, network bool, isolation *NativeWarmIsolation, exchange func(io.Reader, io.Writer) ([]byte, error)) ([]byte, error) {
	return runNativeWarmGit(ctx, workspace, executable, nil, writable, network, isolation, exchange)
}

func runNativeWarmGit(ctx context.Context, workspace, executable string, input []byte, writable, network bool, isolation *NativeWarmIsolation, exchange func(io.Reader, io.Writer) ([]byte, error)) ([]byte, error) {
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, err
	}
	bind := "--ro-bind"
	if writable {
		bind = "--bind"
	}
	args := []string{"--die-with-parent", "--new-session", "--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts",
		"--ro-bind", "/", "/", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp", "--dir", "/tmp/home"}
	if isolation != nil {
		args = append(args, "--tmpfs", isolation.WorkspaceRoot)
	}
	args = append(args, bind, workspace, workspace, "--chdir", workspace)
	if !network {
		args = append(args, "--unshare-net")
	}
	if isolation != nil {
		masks, err := nativeWarmMasks(isolation)
		if err != nil {
			return nil, err
		}
		args = append(args, masks...)
		args = append(args, "--remount-ro", isolation.WorkspaceRoot)
	}
	// The compiled helper can be outside PATH and may itself live under /tmp.
	helper := "/tmp/loopforge-warm-git"
	args = append(args, "--ro-bind", executable, helper, "--", helper, "warm-git-helper-native", workspace)
	cmd := exec.CommandContext(ctx, bwrap, args...)
	cmd.Dir = workspace
	env := controlledBaseEnvironment(os.Environ())
	for _, key := range []string{"HOME", "TMPDIR", "TMP", "TEMP", "PWD"} {
		value := "/tmp"
		if key == "HOME" {
			value = filepath.Join("/tmp", "home")
		}
		if key == "PWD" {
			value = workspace
		}
		env = upsertEnv(env, key, value)
	}
	cmd.Env = env
	if exchange != nil {
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		defer stdout.Close()
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		defer stdin.Close()
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		out, err := exchange(stdout, stdin)
		_ = stdin.Close()
		if err != nil {
			_ = cmd.Process.Kill()
		}
		return out, errors.Join(err, cmd.Wait())
	}
	cmd.Stdin = bytes.NewReader(input)
	out, err := cmd.Output()
	return []byte(strings.TrimSpace(string(out))), err
}
