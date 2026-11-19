package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"agentflow/internal/gitwork"
)

var errWarmRemoteTrackingChanged = errors.New("remote tracking reference differs from advertised origin")

func warmRefMismatch(name, reason string) error {
	err := fmt.Errorf("warm seed ref %s %s", name, reason)
	if strings.HasPrefix(name, "refs/remotes/origin/") {
		return errors.Join(errWarmRemoteTrackingChanged, err)
	}
	return err
}

// Read ref files as data without letting the seed select Git configuration.
// A local ref must have a corresponding remote name before its metadata
// can be exchanged, even when its object also belongs to another branch.
func verifyWarmRefs(ctx context.Context, meta, db, current string, returning bool) error {
	if _, err := warmSeedIdentity(filepath.Join(meta, "refs")); err != nil {
		return err
	}
	for _, name := range []string{"index.lock", "HEAD.lock", "packed-refs.lock", "MERGE_HEAD", "MERGE_MSG", "AUTO_MERGE", "MERGE_AUTOSTASH", "CHERRY_PICK_HEAD", "REVERT_HEAD", "REBASE_HEAD", "BISECT_START", "BISECT_LOG", "rebase-apply", "rebase-merge", "sequencer", "worktrees", "commondir"} {
		if _, err := os.Lstat(filepath.Join(meta, name)); !errors.Is(err, os.ErrNotExist) {
			if err != nil {
				return err
			}
			return fmt.Errorf("warm seed has pending Git state at %s", name)
		}
	}
	refs := make(map[string]string)
	packed := filepath.Join(meta, "packed-refs")
	if err := warmRegular(packed); err == nil {
		raw, err := os.ReadFile(packed)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if line == "" || line[0] == '#' || line[0] == '^' {
				continue
			}
			oid, name, ok := strings.Cut(line, " ")
			if !ok || !warmCommitID.MatchString(oid) {
				return errors.New("invalid packed seed ref")
			}
			refs[name] = oid
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := filepath.WalkDir(filepath.Join(meta, "refs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("invalid seed ref file")
		}
		name, err := filepath.Rel(meta, path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		refs[filepath.ToSlash(name)] = strings.TrimSpace(string(b))
		return nil
	}); err != nil {
		return err
	}
	advertised, err := gitwork.C(ctx, db, "for-each-ref", "--format=%(objectname) %(refname)", "refs/remotes/origin", "refs/tags")
	if err != nil {
		return err
	}
	remote := make(map[string]string)
	for _, line := range strings.Split(advertised, "\n") {
		oid, name, ok := strings.Cut(line, " ")
		if ok {
			remote[name] = oid
		}
	}
	for name, oid := range refs {
		if name == "refs/remotes/origin/HEAD" && strings.HasPrefix(oid, "ref: refs/remotes/origin/") {
			if remote[strings.TrimPrefix(oid, "ref: ")] == "" {
				return warmRefMismatch(name, "has no remote counterpart")
			}
			continue
		}
		if current != "" && name == "refs/heads/"+current {
			continue
		}
		peer := name
		switch {
		case strings.HasPrefix(name, "refs/heads/"):
			peer = "refs/remotes/origin/" + strings.TrimPrefix(name, "refs/heads/")
		case strings.HasPrefix(name, "refs/tags/"), strings.HasPrefix(name, "refs/remotes/origin/"):
		default:
			return fmt.Errorf("warm seed has private Git ref %s", name)
		}
		tip := remote[peer]
		if !warmCommitID.MatchString(oid) {
			return fmt.Errorf("invalid warm seed ref %s", name)
		}
		if tip == "" {
			return warmRefMismatch(name, "has no remote counterpart")
		}
		if oid == tip {
			continue
		}
		if strings.HasPrefix(name, "refs/tags/") || returning && strings.HasPrefix(name, "refs/heads/") {
			return fmt.Errorf("warm seed tag %s differs from the remote", name)
		}
		if _, err := gitwork.C(ctx, db, "merge-base", "--is-ancestor", oid, tip); err != nil {
			return warmRefMismatch(name, "has unpublished work")
		}
	}
	return nil
}

func checkWarmStoredModules(meta string) error {
	// A deinitialized submodule can still own unpublished branches or a
	// stash in the parent Git directory even when its checkout is empty.
	modules, err := openWarmWorkspaceDir(filepath.Join(meta, "modules"), false)
	if err == nil {
		_, readErr := modules.Readdirnames(1)
		modules.Close()
		if readErr != io.EOF {
			return errors.New("inspect stored submodule repositories before exchanging checkout metadata")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func warmSeedIdentity(meta string) (warmWorkspaceIdentity, error) {
	dir, err := openWarmWorkspaceDir(meta, false)
	if err != nil {
		return warmWorkspaceIdentity{}, err
	}
	defer dir.Close()
	return warmWorkspaceDirIdentity(dir)
}

func verifyWarmSeedIndex(local context.Context, db, private, checkout, head string, checkFiles bool) error {
	old := filepath.Join(checkout, ".git")
	// Compare the seed index in trusted metadata. Never let the original
	// repository config, hooks or index extensions choose the Git directory.
	index := filepath.Join(old, "index")
	if err := warmRegular(index); err != nil {
		return err
	}
	raw, err := os.ReadFile(index)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(db, "index"), raw, 0600); err != nil {
		return err
	}
	if _, err := gitwork.C(local, db, "--work-tree="+checkout, "diff-index", "--quiet", "--cached", head, "--"); err != nil {
		return fmt.Errorf("warm seed has staged work: %w", err)
	}
	// Reject dirty operator work before making a credentialed request.
	// The independent index built below verifies bytes again before promotion.
	if checkFiles {
		if _, err := gitwork.C(local, db, "--work-tree="+checkout, "-c", "filter.lfs.clean=git-lfs clean -- %f", "-c", "lfs.storage="+filepath.Join(private, "seed-lfs"), "diff-files", "--quiet", "--no-ext-diff", "--ignore-submodules=all", "--"); err != nil {
			return fmt.Errorf("warm seed contains working tree edits: %w", err)
		}
	}
	if err := os.Remove(filepath.Join(db, "index")); err != nil {
		return err
	}
	return nil
}
