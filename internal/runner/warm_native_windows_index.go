package runner

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"

	"agentflow/internal/gitwork"
)

// Git records the pointer size during checkout and treats the hydrated size
// as a data change. Verify and stage just those paths in a disposable index
// before publishing their new stat data. The real index remains untouched if
// Git is interrupted, the staged contents change, or another writer wins.
func (w warmWindowsTree) planIndexRefresh(ctx context.Context, materialized []warmLFSFile) (func() error, error) {
	unchanged := func() error { return nil }
	path := filepath.Join(w.checkout, ".git", "index")
	if err := warmRegular(path); err != nil {
		return nil, err
	}
	before, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	stage, err := os.CreateTemp(filepath.Dir(path), ".loopforge-index-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(stage.Name())
	defer os.Remove(stage.Name() + ".lock")
	_, err = stage.Write(before)
	if err := errors.Join(err, stage.Close()); err != nil {
		return nil, err
	}
	name := filepath.Base(stage.Name())
	if len(materialized) != 0 {
		expected, err := w.readIndexEntries(ctx, stage.Name())
		if err != nil {
			return nil, err
		}
		args := []string{"update-index", "--info-only", "--"}
		prefix := len(args)
		size := 0
		flush := func() error {
			if len(args) == prefix {
				return nil
			}
			_, err := w.checkoutGitIndex(ctx, false, name, args...)
			args, size = args[:prefix], 0
			return err
		}
		for _, file := range materialized {
			if !warmWindowsProofPath(file.Name) {
				return nil, errors.New("unsupported Windows path in changed LFS files")
			}
			if size+len(file.Name) > 8192 {
				if err := flush(); err != nil {
					return nil, err
				}
			}
			args = append(args, file.Name)
			size += len(file.Name) + 3
		}
		if err := flush(); err != nil {
			return nil, err
		}
		actual, err := w.readIndexEntries(ctx, stage.Name())
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(actual, expected) {
			return nil, warmTreeChanged("Git index entries changed while refreshing materialized LFS files")
		}
	} else if err := nativeWarmStatus(func(args ...string) (string, error) {
		return w.checkoutGitIndex(ctx, true, name, args...)
	}); err != nil {
		return nil, err
	}
	after, err := os.ReadFile(stage.Name())
	if err != nil {
		return nil, err
	}
	if bytes.Equal(before, after) {
		return unchanged, nil
	}
	return func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.verifyPinnedPaths(); err != nil {
			return err
		}
		return publishWarmWindowsIndex(path, before, after)
	}, nil
}

// Index entries are Git object IDs and flags. Reading the disposable index on
// WSL avoids another interop launch and does not inspect the Windows worktree.
func (w warmWindowsTree) readIndexEntries(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", w.checkout, "ls-files", "--stage", "-v", "-z")
	cmd.Env = append(gitwork.Env(""),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0", "GIT_INDEX_FILE="+path)
	return cmd.Output()
}

func publishWarmWindowsIndex(path string, before, after []byte) error {
	entry, err := os.Lstat(path)
	if err != nil || !entry.Mode().IsRegular() {
		return warmTreeChanged("checkout index is no longer a regular file")
	}
	lock := path + ".lock"
	file, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, entry.Mode().Perm())
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.Remove(lock)
		}
	}()
	defer file.Close()
	current, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, before) {
		return warmTreeChanged("checkout index was updated while refreshing Windows Git file state")
	}
	if _, err := file.Write(after); err != nil {
		return err
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(lock, path); err != nil {
		return err
	}
	published = true
	return nil
}
