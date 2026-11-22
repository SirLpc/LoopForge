package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
)

// NativeWarmIsolation is local to a dedicated Runner. It is never supplied by
// a workflow or sent over the wire.
type NativeWarmIsolation struct {
	WorkspaceRoot string
	Protected     []string
}

func nativeWarmMasks(isolation *NativeWarmIsolation) ([]string, error) {
	if isolation == nil {
		return nil, nil
	}
	args := []string{"--unsetenv", "WSL_INTEROP", "--unsetenv", "WSLENV", "--unsetenv", "DOCKER_HOST"}
	var paths []string
	for _, name := range isolation.Protected {
		if name == "" {
			continue
		}
		path, err := filepath.EvalSymlinks(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
	}
	slices.Sort(paths)
	var masked []string
	for _, path := range slices.Compact(paths) {
		if slices.ContainsFunc(masked, func(parent string) bool {
			rel, err := filepath.Rel(parent, path)
			return err == nil && filepath.IsLocal(rel)
		}) {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			args = append(args, "--tmpfs", path, "--remount-ro", path)
			masked = append(masked, path)
		} else {
			args = append(args, "--ro-bind", "/dev/null", path)
		}
	}
	return args, nil
}
