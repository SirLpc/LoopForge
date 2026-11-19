package runner

import "golang.org/x/sys/unix"

func renameWarmWorkspaceDir(fromFD int, from string, toFD int, to string) error {
	return unix.RenameatxNp(fromFD, from, toFD, to, unix.RENAME_EXCL)
}

func exchangeWarmGitDirs(old, fresh string) error {
	return unix.RenamexNp(old, fresh, unix.RENAME_SWAP)
}
