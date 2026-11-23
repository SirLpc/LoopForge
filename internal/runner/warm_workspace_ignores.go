package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentflow/internal/gitwork"
	"golang.org/x/sys/unix"
)

// The Docker checkout replaces its Git metadata at each lease. Carry over
// only the repository-local ignore file, using a fresh Git config and index.
func copyWarmGitExclude(checkout, stage string) error {
	info, err := openWarmWorkspaceDir(filepath.Join(checkout, ".git", "info"), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer info.Close()
	fd, err := unix.Openat(int(info.Fd()), "exclude", unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	exclude := os.NewFile(uintptr(fd), filepath.Join(info.Name(), "exclude"))
	defer exclude.Close()
	stat, err := exclude.Stat()
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() {
		return fmt.Errorf("expected a regular file at %s", exclude.Name())
	}
	contents, err := io.ReadAll(exclude)
	if err != nil {
		return err
	}
	stageInfo := filepath.Join(stage, ".git", "info")
	if err := os.MkdirAll(stageInfo, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(stageInfo, "exclude"), contents, 0600)
}

func warmCheckIgnore(ctx context.Context, stage, checkout string, paths []string) (map[string]bool, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	var input strings.Builder
	for _, path := range paths {
		// Prefix real paths with ./ so Git treats a leading colon as a name.
		input.WriteString("./" + path + "\x00")
	}
	out, err := gitwork.CStdoutRaw(ctx, stage, strings.NewReader(input.String()), "--work-tree="+checkout, "check-ignore", "--stdin", "-z")
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return nil, nil // No path matched an ignore rule.
	}
	if err != nil {
		return nil, err
	}
	ignored := make(map[string]bool)
	for path := range strings.SplitSeq(strings.TrimSuffix(out, "\x00"), "\x00") {
		ignored[strings.TrimPrefix(path, "./")] = true
	}
	return ignored, nil
}

func checkWarmIgnoredCollisions(ctx context.Context, stage, checkout, cache, head, target string) error {
	added, err := gitwork.CStdoutRaw(ctx, cache, nil, "diff-tree", "-r", "--no-renames", "--name-only", "--diff-filter=A", "-z", head, target)
	if err != nil || added == "" {
		return err
	}
	// Inspect only paths the incoming checkout would write. The filesystem
	// resolves their actual case. Check parents before descending past links.
	var candidates []string
	for path := range strings.SplitSeq(added, "\x00") {
		if path == "" {
			continue
		}
		if !validWarmWorkspaceRelativePath(path) {
			return errors.New("invalid incoming checkout path")
		}
		var prefix string
		for part := range strings.SplitSeq(path, "/") {
			prefix = filepath.Join(prefix, part)
			abs := filepath.Join(checkout, prefix)
			info, err := os.Lstat(abs)
			if errors.Is(err, os.ErrNotExist) {
				break
			}
			if err != nil {
				return err
			}
			if !info.IsDir() {
				candidates = append(candidates, prefix)
				break
			}
			if prefix == path {
				if err := filepath.WalkDir(abs, func(entryPath string, entry fs.DirEntry, err error) error {
					if err != nil || entry.IsDir() {
						return err
					}
					rel, err := filepath.Rel(checkout, entryPath)
					if err == nil {
						candidates = append(candidates, rel)
					}
					return err
				}); err != nil {
					return err
				}
			}
		}
	}
	ignored, err := warmCheckIgnore(ctx, stage, checkout, candidates)
	if err != nil {
		return err
	}
	for _, path := range candidates {
		if ignored[path] {
			return fmt.Errorf("branch switch would overwrite ignored local file %q", path)
		}
	}
	return nil
}

// Old return proofs may include ignored artifacts or directories. Check the
// actual ignore rules before doing any cleanup from a persisted proof.
func (p *warmWorkspacePool) warmReturnIgnores(ctx context.Context, root string, a warmWorkspaceAllocation) (map[string]bool, error) {
	ignored := make(map[string]bool)
	for _, checkout := range a.Targets {
		var paths []string
		for _, rel := range a.ReturnRemove {
			if !validWarmWorkspaceRelativePath(rel) {
				return nil, errors.New("invalid return cleanup path")
			}
			path, err := filepath.Rel(checkout, filepath.Join(root, rel))
			if err == nil && validWarmWorkspaceRelativePath(path) {
				paths = append(paths, path)
			}
		}
		if len(paths) == 0 {
			continue
		}
		stage, err := os.MkdirTemp(p.gitCacheRoot(), ".return-ignore-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(stage)
		if _, err := gitwork.C(ctx, stage, "init", "--template="); err != nil {
			return nil, err
		}
		if err := copyWarmGitExclude(checkout, stage); err != nil {
			return nil, err
		}
		if p.native {
			if _, err := nativeWarmProofConfig(ctx, checkout, stage); err != nil {
				return nil, err
			}
		}
		matches, err := warmCheckIgnore(ctx, stage, checkout, paths)
		if err != nil {
			return nil, err
		}
		prefix, err := filepath.Rel(root, checkout)
		if err != nil {
			return nil, err
		}
		for path := range matches {
			ignored[filepath.Join(prefix, path)] = true
		}
	}
	return ignored, nil
}
