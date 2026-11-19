package runtime

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// The qualified driver tears down its own containers. Leave an unrelated
// host editor or terminal intact and keep its checkout assigned. Enumerating
// open handles avoids walking the Unity Library to find their owners.
func CheckWarmWorkspaceProcesses(ctx context.Context, root string) error {
	owns := func(name string) bool {
		name = strings.TrimSuffix(name, " (deleted)")
		return name == root || strings.HasPrefix(name, root+string(filepath.Separator))
	}
	if runtime.GOOS != "linux" {
		raw, err := exec.CommandContext(ctx, "lsof", "-nP", "-u", strconv.Itoa(os.Geteuid()), "-F0pn").Output()
		if err != nil {
			return fmt.Errorf("cannot inspect native workspace handles: %w", err)
		}
		for _, field := range strings.Split(string(raw), "\x00") {
			if path, ok := strings.CutPrefix(strings.TrimLeft(field, "\n"), "n"); ok && owns(path) {
				return errors.New("close native processes using this workspace")
			}
		}
		return nil
	}
	dirs, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	session, err := unix.Getsid(0)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		pid, err := strconv.Atoi(dir.Name())
		if err != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := dir.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if int(st.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
			continue
		}
		err = checkWarmProcessHandles(dir.Name(), owns)
		if errors.Is(err, os.ErrPermission) {
			// Procfs can hide the handles of unrelated login sessions. The
			// Docker mount inventory accounts for detached qualified workloads.
			otherSession, sidErr := unix.Getsid(pid)
			if errors.Is(sidErr, syscall.ESRCH) || (sidErr == nil && otherSession != session) {
				continue
			}
		}
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	return nil
}

func checkWarmProcessHandles(pid string, owns func(string) bool) error {
	base := filepath.Join("/proc", pid)
	check := func(file string) error {
		path, err := os.Readlink(file)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if owns(path) {
			return fmt.Errorf("native process %s still uses this workspace", pid)
		}
		return nil
	}
	for _, name := range []string{"cwd", "root", "exe"} {
		if err := check(filepath.Join(base, name)); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Join(base, "fd"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, f := range entries {
		if err := check(filepath.Join(base, "fd", f.Name())); err != nil {
			return err
		}
	}
	mapped, err := os.Open(filepath.Join(base, "maps"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(mapped)
	for scanner.Scan() {
		_, name, ok := strings.Cut(scanner.Text(), "/")
		if ok && owns("/"+name) {
			mapped.Close()
			return fmt.Errorf("native process %s still maps this workspace", pid)
		}
	}
	err = scanner.Err()
	mapped.Close()
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
