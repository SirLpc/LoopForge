package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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

// On a DrvFS metadata mount Linux permissions can deny an otherwise readable
// Windows file. Keep those worktrees on the Linux verification path.
func warmWindowsDriveWithoutMetadata(path string) bool {
	var info unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &info); err != nil || info.Mask&unix.STATX_MNT_ID == 0 {
		return false
	}
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	return warmWindowsMountWithoutMetadata(string(raw), strconv.FormatUint(info.Mnt_id, 10))
}

func warmWindowsMountWithoutMetadata(mounts, id string) bool {
	for line := range strings.SplitSeq(mounts, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[0] != id {
			continue
		}
		separator := slices.Index(fields[6:], "-") + 6
		if separator < 6 || separator+3 >= len(fields) || fields[separator+1] != "9p" {
			return false
		}
		options := strings.FieldsFunc(fields[5]+","+fields[separator+3], func(c rune) bool {
			return c == ',' || c == ';'
		})
		return slices.Contains(options, "aname=drvfs") && !slices.Contains(options, "metadata")
	}
	return false
}
