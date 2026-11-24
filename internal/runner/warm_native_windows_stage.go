package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// Windows Git stages into a disposable index and LFS directory. Publish the
// staged objects with directory-relative operations before replacing the real
// index, so a failed or canceled staging step cannot leave missing LFS objects.
func stageWarmWindowsGit(ctx context.Context, w warmWindowsTree, args []string, spec warmWindowsRequest, verb string) warmWindowsHelperReply {
	fail := func(err error) warmWindowsHelperReply {
		return warmWindowsHelperReply{Handled: true, Error: err.Error(), Code: -1}
	}
	meta := filepath.Join(w.checkout, ".git")
	index := filepath.Join(meta, "index")
	if err := warmRegular(index); err != nil {
		return fail(err)
	}
	before, err := os.ReadFile(index)
	if err != nil {
		return fail(err)
	}
	stage, err := os.MkdirTemp(meta, ".loopforge-stage-")
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(stage)
	if err := os.Mkdir(filepath.Join(stage, "lfs"), 0o700); err != nil {
		return fail(err)
	}
	stageIndex := filepath.Join(stage, "index")
	if err := os.WriteFile(stageIndex, before, 0o600); err != nil {
		return fail(err)
	}
	spec.StageLFS = `.git\` + filepath.Base(stage) + `\lfs`
	spec.IndexFile = w.root + `\.git\` + filepath.Base(stage) + `\index`
	spec.FinishWrite = true
	reply := executeWarmWindowsDispatch(ctx, w, args, spec, verb)
	if !reply.Handled || reply.Error != "" {
		return reply
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := w.verifyPinnedPaths(); err != nil {
		return fail(err)
	}
	after, err := os.ReadFile(stageIndex)
	if err != nil {
		return fail(err)
	}
	if err := publishWarmWindowsStagedLFS(filepath.Join(stage, "lfs", "objects"), filepath.Join(meta, "lfs", "objects")); err != nil {
		return fail(err)
	}
	if err := publishWarmWindowsIndex(index, before, after); err != nil {
		return fail(err)
	}
	return reply
}

func publishWarmWindowsStagedLFS(source, destination string) error {
	if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		oid := entry.Name()
		if len(oid) != 64 || strings.IndexFunc(oid, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) >= 0 ||
			path != filepath.Join(source, oid[:2], oid[2:4], oid) || !entry.Type().IsRegular() {
			return errors.New("invalid Windows staging LFS object")
		}
		return linkWarmWindowsStagedLFS(path, filepath.Join(destination, oid[:2], oid[2:4]), oid)
	})
}

func linkWarmWindowsStagedLFS(path, destination, oid string) error {
	src, err := openWarmWorkspaceDir(filepath.Dir(path), false)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := openWarmWorkspaceDir(destination, true)
	if err != nil {
		return err
	}
	defer dst.Close()
	err = unix.Linkat(int(src.Fd()), oid, int(dst.Fd()), oid, 0)
	if err == nil {
		return dst.Sync()
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	fd, err := unix.Openat(int(dst.Fd()), oid, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), oid)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.Join(errors.New("Windows staging destination is not a regular LFS object"), err)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != oid {
		return errors.New("existing Windows staging LFS object does not match its object ID")
	}
	return nil
}
