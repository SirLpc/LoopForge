package runtime

import (
	"bufio"
	"bytes"
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
)

// The qualified driver tears down its own containers. Leave an unrelated
// host editor or terminal intact and keep its checkout assigned. Enumerating
// open handles avoids walking the Unity Library to find their owners.
func CheckWarmWorkspaceProcesses(ctx context.Context, root string) error {
	return checkWarmHostProcesses(ctx, func(name string) bool {
		name = strings.TrimSuffix(name, " (deleted)")
		return name == root || strings.HasPrefix(name, root+string(filepath.Separator))
	})
}

// CheckWarmHostVisibility verifies the same host-wide process and handle
// coverage required at return, without treating any active checkout as busy.
func CheckWarmHostVisibility(ctx context.Context) error {
	return checkWarmHostProcesses(ctx, func(string) bool { return false })
}

func checkWarmHostProcesses(ctx context.Context, owns func(string) bool) error {
	switch runtime.GOOS {
	case "linux":
		// Linux reserves PROC_PID_INIT_INO for the initial PID namespace. A
		// nested procfs cannot enumerate workspace holders on the runner host.
		const initialPIDNamespace = 0xeffffffc
		ns, err := os.Stat("/proc/1/ns/pid")
		if err != nil || ns.Sys().(*syscall.Stat_t).Ino != initialPIDNamespace {
			return errors.New("workspace retained: host PID namespace visibility required")
		}
		mounts, err := os.ReadFile("/proc/self/mountinfo")
		if err != nil {
			return err
		}
		visible := false
		for _, line := range strings.Split(string(mounts), "\n") {
			parts := strings.Fields(line)
			if len(parts) > 7 && parts[3] == "/" && parts[4] == "/proc" && strings.Contains(line, " - proc ") {
				visible = true
				for _, option := range strings.Split(parts[len(parts)-1], ",") {
					if strings.HasPrefix(option, "hidepid=") && option != "hidepid=0" && option != "hidepid=off" {
						return errors.New("workspace retained: complete host procfs visibility is required")
					}
				}
			}
		}
		if !visible {
			return errors.New("workspace retained: cannot verify host procfs mount")
		}
		return checkWarmLinuxProcesses(ctx, "/proc", owns)
	case "darwin":
		if os.Geteuid() != 0 {
			return errors.New("workspace retained: host-wide lsof process visibility required")
		}
		// lsof may succeed while declaring protected files or processes unreadable.
		// Compare its inventory to live PIDs; fail on any partial diagnostic.
		before, err := exec.CommandContext(ctx, "ps", "-axo", "pid=").Output()
		if err != nil {
			return err
		}
		var stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, "lsof", "-nP", "+c", "0", "-F0pfn")
		cmd.Stderr = &stderr
		raw, err := cmd.Output()
		if err != nil || stderr.Len() != 0 {
			return errors.New("workspace retained: lsof cannot inspect every host process")
		}
		observed := map[string]bool{}
		for _, field := range strings.Split(string(raw), "\x00") {
			field = strings.TrimLeft(field, "\n")
			if id, ok := strings.CutPrefix(field, "p"); ok {
				observed[id] = true
			}
			if field == "fNOFD" || strings.Contains(field, "Permission denied") || strings.Contains(field, "Operation not permitted") {
				return errors.New("workspace retained: lsof reports inaccessible process handles")
			}
			if path, ok := strings.CutPrefix(field, "n"); ok && owns(path) {
				return errors.New("native process still uses this workspace")
			}
		}
		for _, id := range strings.Fields(string(before)) {
			pid, err := strconv.Atoi(id)
			if err != nil {
				return err
			}
			if pid > 0 && !observed[id] && !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
				return errors.New("workspace retained: lsof omitted a live process")
			}
		}
		return nil
	default:
		return errors.New("native workspace process inventory unavailable")
	}
}

func checkWarmLinuxProcesses(ctx context.Context, proc string, owns func(string) bool) error {
	dirs, err := os.ReadDir(proc)
	if err != nil {
		return err
	}
	for _, dir := range dirs {
		if _, err := strconv.Atoi(dir.Name()); err != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		base := filepath.Join(proc, dir.Name())
		threads, err := os.ReadDir(filepath.Join(base, "task"))
		if err != nil {
			if _, statErr := os.Stat(base); os.IsNotExist(statErr) {
				continue
			}
			return fmt.Errorf("workspace retained: cannot enumerate threads for process %s: %w", dir.Name(), err)
		}
		if len(threads) == 0 {
			if _, statErr := os.Stat(base); os.IsNotExist(statErr) {
				continue
			}
			return fmt.Errorf("workspace retained: empty thread inventory for live process %s", dir.Name())
		}
		for _, thread := range threads {
			if err := ctx.Err(); err != nil {
				return err
			}
			task := filepath.Join(base, "task", thread.Name())
			if err := checkWarmProcessHandles(task, owns); err != nil {
				if _, statErr := os.Stat(task); os.IsNotExist(statErr) {
					continue
				}
				return fmt.Errorf("workspace retained: verify host process %s thread %s handles: %w", dir.Name(), thread.Name(), err)
			}
		}
	}
	return nil
}
func checkWarmProcessHandles(base string, owns func(string) bool) error {
	pid := filepath.Base(base)
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
	if err != nil {
		return err
	}
	for _, f := range entries {
		if err := check(filepath.Join(base, "fd", f.Name())); err != nil {
			return err
		}
	}
	mapped, err := os.Open(filepath.Join(base, "maps"))
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
	return err
}
