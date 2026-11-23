package runner

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

var errWarmTreeChanged = errors.New("warm checkout changed")

func warmTreeChanged(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errWarmTreeChanged, fmt.Sprintf(format, args...))
}

func warmCheckoutRef(checkout, repositoryID string) (domain.WarmWorkspaceRef, error) {
	meta := filepath.Join(checkout, ".git")
	head, err := warmCurrentHead(meta)
	if err != nil {
		return domain.WarmWorkspaceRef{}, err
	}
	raw, err := os.ReadFile(filepath.Join(meta, "HEAD"))
	if err != nil {
		return domain.WarmWorkspaceRef{}, err
	}
	branch, _ := strings.CutPrefix(strings.TrimSpace(string(raw)), "ref: refs/heads/")
	if branch == head {
		branch = ""
	}
	return domain.WarmWorkspaceRef{RepositoryID: repositoryID, Branch: branch, Commit: head}, nil
}

func (w *warmReturnTree) verifyLocal(ctx context.Context, slot warmWorkspaceSlot, cache string, r domain.RunnerRepository, ref domain.WarmWorkspaceRef, lfsMode warmProofLFSMode) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Prefer the workspace request's cancellation cause to a Git subprocess exit.
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	auth := repositoryGitContext(ctx, r)
	local, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	meta := filepath.Join(w.checkout, ".git")
	if _, err := warmSeedIdentity(meta); err != nil {
		return err
	}
	head := ref.Commit
	private, err := os.MkdirTemp(filepath.Dir(cache), ".local-proof-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(private)
	if w.native {
		current, err := warmCheckoutRef(w.checkout, r.RepositoryID)
		if err != nil {
			return err
		}
		if current.Commit != head || current.Branch != ref.Branch {
			return warmTreeChanged("checked out Git reference differs from the proof")
		}
		if err := verifyNativeWarmConfig(local, w.checkout, private); err != nil {
			return err
		}
		origin, err := gitwork.C(local, private, "config", "--no-includes", "--file", filepath.Join(meta, "config"), "--get-all", "remote.origin.url")
		if err != nil {
			return err
		}
		if domain.URLHasCredentials(origin) || gitwork.CanonicalRemote(origin) != gitwork.CanonicalRemote(r.CloneURL) {
			return warmTreeChanged("checkout origin differs from frozen repository")
		}
	}
	stage := filepath.Join(private, "check")
	if _, err := gitwork.C(local, private, "init", "--template=", warmObjectFormat(head), stage); err != nil {
		return err
	}
	rules := warmCheckoutFileRules{fileMode: true, symlinks: true}
	if w.native {
		rules, err = nativeWarmProofConfig(local, w.checkout, stage)
		if err != nil {
			return err
		}
	}
	var alternateErr error
	if w.native {
		if lfsMode == warmProofLFSContent {
			alternateErr = warmAlternate(filepath.Join(stage, ".git"), filepath.Join(meta, "objects"))
		} else {
			alternateErr = nativeWarmProofAlternates(filepath.Join(stage, ".git"), cache, w.checkout)
		}
	} else {
		alternateErr = warmAlternate(filepath.Join(stage, ".git"), filepath.Join(cache, "objects"))
	}
	if alternateErr != nil {
		return alternateErr
	}
	lfs, err := prepareWarmLFSForProof(local, auth, stage, cache, warmLFSCache(cache, slot.ProjectID, r), head, r, (*os.File).Sync, lfsMode)
	if err != nil {
		return err
	}
	if err := warmRegular(filepath.Join(meta, "index")); err != nil {
		return err
	}
	index, err := os.ReadFile(filepath.Join(meta, "index"))
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(stage, ".git", "index"), index, 0600); err != nil {
		return err
	}
	if _, err := gitwork.C(local, stage, "diff-index", "--no-ext-diff", "--cached", "--quiet", head, "--"); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return warmTreeChanged("commit or unstage pending index changes")
		}
		return err
	}
	library := filepath.Join(slot.UnityProjectPath, "Library")
	if !w.native {
		if err := warmTreeProtectsLibrary(local, stage, head, library); err != nil {
			return err
		}
		if info, err := os.Lstat(filepath.Join(w.checkout, library)); err == nil {
			if !info.IsDir() {
				return warmTreeChanged("Library path %s is not a directory", library)
			}
			if err := warmTargetIgnoresLibrary(local, stage, head, library); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if err := copyWarmGitExclude(w.checkout, stage); err != nil {
		return err
	}
	entries, err := gitwork.CStdoutRaw(local, stage, nil, "ls-tree", "-rz", head)
	if err != nil {
		return err
	}
	tracked := map[string][]string{}
	var attributes []string
	for _, line := range strings.Split(entries, "\x00") {
		if line == "" {
			continue
		}
		header, path, ok := strings.Cut(line, "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || !validWarmWorkspaceRelativePath(path) {
			return errors.New("invalid tracked entry")
		}
		tracked[path] = fields
		if filepath.Base(path) == ".gitattributes" && strings.HasPrefix(fields[0], "100") {
			attributes = append(attributes, path)
		}
	}
	if len(attributes) > 0 {
		if err := restoreWarmProofFiles(local, stage, head, attributes...); err != nil {
			return err
		}
	}
	var ignoredDirs map[string]bool
	if w.native {
		ignoredDirs, err = warmIgnoredDirectories(local, stage, w.checkout)
		if err != nil {
			return err
		}
		for path := range tracked {
			for parent := path; parent != "."; parent = filepath.Dir(parent) {
				delete(ignoredDirs, parent)
			}
		}
	}
	untracked := make(map[string]fs.DirEntry)
	var untrackedPaths []string
	err = filepath.WalkDir(w.checkout, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(w.checkout, path)
		if err != nil {
			return err
		}
		if rel == ".git" {
			return filepath.SkipDir
		}
		if !w.native && rel == library {
			if !d.IsDir() {
				return warmTreeChanged("Library path %s is not a directory", library)
			}
			return filepath.SkipDir
		}
		if d.IsDir() && ignoredDirs[rel] {
			return filepath.SkipDir
		}
		fields, ok := tracked[rel]
		if ok {
			delete(tracked, rel)
			if fields[0] == "160000" {
				children, err := os.ReadDir(path)
				if err != nil {
					return err
				}
				if len(children) != 0 {
					return warmTreeChanged("populated submodule %s needs inspection", rel)
				}
				return filepath.SkipDir
			}
			matches, err := warmBlobMatchesWithRules(local, stage, head, lfs, path, rel, fields[2], fields[0], rules)
			if err != nil {
				return err
			}
			if !matches {
				return warmTreeChanged("commit or restore changes in %s", rel)
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		untracked[rel] = d
		untrackedPaths = append(untrackedPaths, rel)
		return nil
	})
	if err != nil {
		return err
	}
	for path, fields := range tracked {
		if fields[0] != "160000" {
			return warmTreeChanged("tracked file %s is missing", path)
		}
	}
	ignored, err := warmCheckIgnore(local, stage, w.checkout, untrackedPaths)
	if err != nil {
		return err
	}
	for _, path := range untrackedPaths {
		if !ignored[path] {
			if err := w.archive(filepath.Join(w.checkout, path), untracked[path]); err != nil {
				return err
			}
		}
	}
	return nil
}
