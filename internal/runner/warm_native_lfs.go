package runner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

// Only Git objects changed by this checkout can need LFS materialization. Give
// Git LFS a tree of those files so it uses its normal pointer parser and transfer
// logic without rescanning the whole target or accessing the writable checkout.
func prepareNativeWarmChangedLFS(local, auth context.Context, stage, scope, before, target string, repo domain.RunnerRepository) (warmLFSManifest, error) {
	var empty warmLFSManifest
	diff, err := gitwork.CStdoutRaw(local, stage, nil, "diff-tree", "--raw", "-r", "-z", "--no-abbrev", "--no-renames", "--diff-filter=AMT", before, target)
	if err != nil || diff == "" {
		return empty, err
	}
	var index strings.Builder
	for diff != "" {
		var header, path string
		var ok bool
		header, diff, _ = strings.Cut(diff, "\x00")
		path, diff, ok = strings.Cut(diff, "\x00")
		fields := strings.Fields(header)
		if !ok || len(fields) != 5 || !validWarmWorkspaceRelativePath(path) {
			return empty, errors.New("invalid native warm Git tree change")
		}
		if fields[1] != "100644" && fields[1] != "100755" {
			continue
		}
		if !warmCommitID.MatchString(fields[3]) {
			return empty, errors.New("invalid native warm Git blob ID")
		}
		index.WriteString(fields[1] + " " + fields[3] + "\t" + path + "\x00")
	}
	if index.Len() == 0 {
		return empty, nil
	}
	if _, err := gitwork.CStdoutRaw(local, stage, strings.NewReader(index.String()), "update-index", "-z", "--index-info"); err != nil {
		return empty, err
	}
	tree, err := gitwork.C(local, stage, "write-tree")
	if err != nil {
		return empty, err
	}
	return prepareWarmLFSManifest(local, stage, scope, tree, (*os.File).Sync, warmProofLFSFetch, func() error {
		if err := stageWarmLFSConfig(local, stage, target); err != nil {
			return err
		}
		storage := filepath.Join(scope, ".git", "lfs")
		dir, err := openWarmWorkspaceDir(storage, true)
		if err != nil {
			return err
		}
		dir.Close()
		// The standalone file transfer also reads lfs.storage in the remote
		// repository. A relative value keeps that repository's store separate.
		stageStorage := filepath.Join(stage, ".git", "lfs")
		if err := os.RemoveAll(stageStorage); err != nil {
			return err
		}
		if err := os.Symlink(storage, stageStorage); err != nil {
			return err
		}
		return fetchWarmLFSRef(auth, stage, "lfs", tree, repo.CloneURL)
	})
}
