package runtime

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func RunNativeWarmGit(ctx context.Context, workspace, executable string, input []byte, writable, network bool, isolation *NativeWarmIsolation) ([]byte, error) {
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
	cmd.Stdin = bytes.NewReader(input)
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
	out, err := cmd.Output()
	return []byte(strings.TrimSpace(string(out))), err
}
