package runner

import "golang.org/x/sys/unix"

func renameWarmWorkspaceDir(fromFD int, from string, toFD int, to string) error {
	return unix.Renameat2(fromFD, from, toFD, to, unix.RENAME_NOREPLACE)
}

func exchangeWarmGitDirs(old, fresh string) error {
	return unix.Renameat2(unix.AT_FDCWD, old, unix.AT_FDCWD, fresh, unix.RENAME_EXCHANGE)
}
