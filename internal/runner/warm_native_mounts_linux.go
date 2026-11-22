package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func qualifyNativeWarmMounts(workspace string) error {
	// Require a separate mount namespace from the distro init process. An
	// inaccessible init namespace is normal in WSL for non-root operators.
	self, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return err
	}
	if init, err := os.Readlink("/proc/1/ns/mnt"); err == nil && init == self {
		return errors.New("native warm Runner requires a private mount namespace")
	}
	probe, err := os.MkdirTemp(workspace, ".native-warm-mount-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probe)
	info, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(info), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		mount := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(fields[4])
		if mount != "/" && !warmWorkspacePathsOverlap(mount, probe) {
			continue
		}
		for _, field := range fields[6:] {
			if field == "-" {
				break
			}
			if strings.HasPrefix(field, "shared:") {
				return errors.New("native warm Runner requires private mount propagation")
			}
		}
	}
	src, dst := filepath.Join(probe, "source"), filepath.Join(probe, "target")
	for _, dir := range []string{src, dst} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return err
		}
	}
	if err := bindNativeWarmDir(src, dst); err != nil {
		return fmt.Errorf("native warm Runner requires a private user and mount namespace: %w", err)
	}
	return unmountNativeWarmDir(dst)
}

func bindNativeWarmDir(source, target string) error {
	return unix.Mount(source, target, "", unix.MS_BIND, "")
}

func unmountNativeWarmDir(target string) error {
	return unix.Unmount(target, 0)
}
