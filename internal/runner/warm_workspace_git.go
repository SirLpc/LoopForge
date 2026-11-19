package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

var warmCommitID = regexp.MustCompile("^[0-9a-f]{40}([0-9a-f]{24})?$")

func (p *warmWorkspacePool) gitCacheRoot() string {
	return filepath.Join(p.journalDir.Name(), p.journalName+".git")
}

func (p *warmWorkspacePool) gitCache(slot string) string {
	digest := sha256.Sum256([]byte(slot))
	return filepath.Join(p.gitCacheRoot(), hex.EncodeToString(digest[:]))
}

func prepareWarmCheckout(ctx context.Context, checkout, cache string, repo domain.RunnerRepository, restore *domain.WarmWorkspaceRef) error {
	return prepareWarmProjectCheckout(ctx, checkout, cache, ".", repo, restore)
}

// The caller holds the exclusive, verified slot. cache belongs to the runner,
// outside every writable checkout; it is stable across renames of the slot.
func prepareWarmProjectCheckout(ctx context.Context, checkout, cache, project string, repo domain.RunnerRepository, restore *domain.WarmWorkspaceRef) error {
	if !filepath.IsAbs(cache) || !filepath.IsAbs(checkout) || filepath.Clean(cache) != cache || warmWorkspacePathsOverlap(checkout, cache) || strings.ContainsAny(cache, "\r\n") {
		return errors.New("invalid warm Git cache location")
	}
	if project != "." && !validWarmWorkspaceRelativePath(project) {
		return errors.New("invalid Unity project path")
	}
	if repo.RepositoryID == "" || (restore != nil && (restore.RepositoryID != repo.RepositoryID || !warmCommitID.MatchString(restore.Commit))) {
		return errors.New("invalid repository identity or restore commit")
	}
	if domain.URLHasCredentials(repo.CloneURL) {
		return errors.New("warm checkout requires a credential-free frozen origin")
	}
	if err := gitwork.ValidateRemoteURL(repo.CloneURL, true); err != nil {
		return err
	}
	if (repo.RequireExistingWorkBranch || repo.CreateNewWorkBranch) && (repo.WorkBranch == "" || repo.RequireExistingWorkBranch == repo.CreateNewWorkBranch) {
		return errors.New("invalid work branch mode")
	}
	old := filepath.Join(checkout, ".git")
	dir, err := openWarmWorkspaceDir(old, false)
	if err != nil {
		return err
	}
	dir.Close()
	parent, err := openWarmWorkspaceDir(filepath.Dir(cache), true)
	if err != nil {
		return err
	}
	parent.Close()
	// All local commands use a context with cancellation/deadline only. In
	// particular they never receive host credentials while inspecting the slot.
	local, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	private, err := os.MkdirTemp(filepath.Dir(cache), ".prepare-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(private)
	originConfig := filepath.Join(old, "config")
	if err := warmRegular(originConfig); err != nil {
		return err
	}
	origin, err := gitwork.C(local, private, "config", "--no-includes", "--file", originConfig, "--get-all", "remote.origin.url")
	if err != nil || domain.URLHasCredentials(origin) || gitwork.CanonicalRemote(origin) != gitwork.CanonicalRemote(repo.CloneURL) {
		return errors.New("warm checkout origin does not match the frozen repository URL")
	}
	head, err := warmCurrentHead(old)
	if err != nil {
		return err
	}
	auth := repositoryGitContext(ctx, repo)
	db, err := warmObjectCache(local, auth, private, cache, checkout, head, repo)
	if err != nil {
		return err
	}
	if restore == nil && repo.Ref == "" && repo.WorkBranch == "" {
		refs, err := gitwork.C(auth, db, "ls-remote", "--symref", "origin", "HEAD")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(refs, "\n") {
			if name, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
				repo.Ref, _, _ = strings.Cut(name, "\t")
				break
			}
		}
	}
	target, branch, err := warmCheckoutTarget(local, db, repo, restore)
	if err != nil {
		return err
	}
	library := filepath.Join(project, "Library")
	for _, rev := range []string{head, target} {
		if err := warmTreeProtectsLibrary(local, db, rev, library); err != nil {
			return err
		}
	}
	// Stage metadata on the checkout filesystem so publishing it is a rename.
	stage, err := os.MkdirTemp(filepath.Dir(checkout), ".warm-git-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if _, err := gitwork.C(local, stage, "init", "--template=", warmObjectFormat(head)); err != nil {
		return err
	}
	meta := filepath.Join(stage, ".git")
	if err := warmAlternate(meta, filepath.Join(db, "objects")); err != nil {
		return err
	}
	if _, err := gitwork.C(local, stage, "remote", "add", "origin", repo.CloneURL); err != nil {
		return err
	}
	// Fetch refs locally, but use alternates for the actual object data.
	if _, err := gitwork.C(local, stage, "fetch", "--no-tags", db, "+refs/remotes/origin/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return err
	}
	for _, pair := range [][2]string{{"filter.lfs.clean", "git-lfs clean -- %f"}, {"filter.lfs.smudge", "git-lfs smudge -- %f"}, {"filter.lfs.required", "true"}} {
		if _, err := gitwork.C(local, stage, "config", pair[0], pair[1]); err != nil {
			return err
		}
	}
	work := func(args ...string) (string, error) {
		return gitwork.C(local, stage, append([]string{"--work-tree=" + checkout}, args...)...)
	}
	if err := verifyWarmWorktree(local, checkout, stage, head, target, library, work); err != nil {
		return err
	}
	if _, err := gitwork.C(local, stage, "update-ref", "--no-deref", "HEAD", target); err != nil {
		return err
	}
	hasLFS := false
	if _, err := exec.LookPath("git-lfs"); err == nil {
		files, err := gitwork.C(local, stage, "lfs", "ls-files", target)
		if err != nil {
			return err
		}
		if files != "" {
			hasLFS = true
			if _, err := gitwork.C(local, stage, "config", "lfs.fetchrecentalways", "false"); err != nil {
				return err
			}
			// Resolve .lfsconfig from the target even while the incremental
			// index still holds the previous baseline.
			configMode, err := gitwork.C(local, stage, "ls-tree", target, "--", ".lfsconfig")
			if err != nil {
				return err
			}
			if strings.HasPrefix(configMode, "100") {
				if _, err := gitwork.C(local, stage, "restore", "--source="+target, "--worktree", "--", ".lfsconfig"); err != nil {
					return err
				}
			} else if err := os.WriteFile(filepath.Join(stage, ".lfsconfig"), nil, 0600); err != nil {
				return err
			}

			if _, err := gitwork.C(auth, stage, "lfs", "fetch", "--include=*", "--exclude=", "origin", target); err != nil {
				return err
			}
		}
	}
	if branch != "" {
		if _, err := gitwork.C(local, stage, "update-ref", "refs/heads/"+branch, target); err != nil {
			return err
		}
		if _, err := gitwork.C(local, stage, "symbolic-ref", "HEAD", "refs/heads/"+branch); err != nil {
			return err
		}
		if _, ok := gitwork.RevParse(local, stage, "refs/remotes/origin/"+branch); ok {
			if _, err := gitwork.C(local, stage, "branch", "--set-upstream-to=origin/"+branch, branch); err != nil {
				return err
			}
		}
	}
	if db != cache {
		if err := os.Rename(db, cache); err != nil {
			return err
		}
		if err := warmAlternate(meta, filepath.Join(cache, "objects")); err != nil {
			return err
		}
	}
	if _, err := work("read-tree", "-m", "-u", head, target); err != nil {
		return err
	}
	if err := exchangeWarmGitDirs(old, meta); err != nil {
		return err
	}
	if hasLFS {
		// read-tree retains unchanged files, including an admin clone prepared
		// with skip-smudge. Materialize such pointers using the prefetched store.
		if _, err := gitwork.C(local, checkout, "lfs", "checkout"); err != nil {
			return err
		}
	}
	return nil
}

// A new cache can borrow the admin seed during its first fetch. Promotion
// happens only after the remote refs and worktree have both been verified.
func warmObjectCache(local, auth context.Context, private, cache, checkout, head string, repo domain.RunnerRepository) (string, error) {
	old := filepath.Join(checkout, ".git")
	db := cache
	_, err := os.Lstat(cache)
	fresh := errors.Is(err, os.ErrNotExist)
	if fresh {
		db = filepath.Join(private, "cache")
		if _, err := gitwork.C(local, private, "init", "--bare", "--template=", warmObjectFormat(head), db); err != nil {
			return "", err
		}
		if _, err := gitwork.C(local, db, "config", "warm.repositoryID", repo.RepositoryID); err != nil {
			return "", err
		}
		if _, err := gitwork.C(local, db, "remote", "add", "origin", repo.CloneURL); err != nil {
			return "", err
		}
		objects := filepath.Join(old, "objects")
		if err := warmSeedObjects(objects); err != nil {
			return "", err
		}
		if err := warmAlternate(db, objects); err != nil {
			return "", err
		}
		// Offer the seed commit for negotiation but retain only objects reachable
		// from refs the authenticated remote actually advertised.
		// Compare the seed index in trusted metadata. Never let the original
		// repository config, hooks or index extensions choose the Git directory.
		index := filepath.Join(old, "index")
		if err := warmRegular(index); err != nil {
			return "", err
		}
		raw, err := os.ReadFile(index)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(db, "index"), raw, 0600); err != nil {
			return "", err
		}
		if _, err := gitwork.C(local, db, "--work-tree="+checkout, "diff-index", "--quiet", "--cached", head, "--"); err != nil {
			return "", fmt.Errorf("warm seed has staged work: %w", err)
		}
		// Reject dirty operator work before making a credentialed request.
		// The independent index built below verifies bytes again before promotion.
		if _, err := gitwork.C(local, db, "--work-tree="+checkout, "-c", "filter.lfs.clean=git-lfs clean -- %f", "-c", "lfs.storage="+filepath.Join(private, "seed-lfs"), "diff-files", "--quiet", "--no-ext-diff", "--ignore-submodules=all", "--"); err != nil {
			return "", fmt.Errorf("warm seed contains working tree edits: %w", err)
		}
		if err := os.Remove(filepath.Join(db, "index")); err != nil {
			return "", err
		}
		if _, err := gitwork.C(local, db, "config", "core.alternateRefsCommand", "/bin/true"); err != nil {
			return "", err
		}
		if _, err := gitwork.C(local, db, "update-ref", "refs/seed/head", head); err != nil {
			return "", err
		}
	} else {
		if err != nil {
			return "", err
		}
		dir, err := openWarmWorkspaceDir(filepath.Join(cache, "objects"), false)
		if err != nil {
			return "", err
		}
		dir.Close()
		id, err := gitwork.C(local, cache, "config", "--get", "warm.repositoryID")
		if err != nil || id != repo.RepositoryID {
			return "", errors.New("warm Git cache repository ID mismatch")
		}
		remote, err := gitwork.C(local, cache, "remote", "get-url", "origin")
		if err != nil || remote != repo.CloneURL {
			return "", errors.New("warm Git cache origin mismatch")
		}
	}
	if _, err := gitwork.C(auth, db, "fetch", "--no-auto-gc", "--prune", "--no-tags", "origin", "+HEAD:refs/warm/default", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return "", err
	}
	if fresh {
		if _, err := gitwork.C(local, db, "update-ref", "-d", "refs/seed/head"); err != nil {
			return "", err
		}
		reachable, err := gitwork.C(local, db, "for-each-ref", "--contains", head, "--format=%(refname)")
		if err != nil || reachable == "" {
			return "", errors.New("seed HEAD is not reachable from the frozen remote")
		}
		if _, err := gitwork.C(local, db, "repack", "-a", "-d"); err != nil {
			return "", err
		}
		if err := os.Remove(filepath.Join(db, "objects", "info", "alternates")); err != nil {
			return "", err
		}
		if _, err := gitwork.C(local, db, "fsck", "--full", "--no-reflogs"); err != nil {
			return "", err
		}
	}
	return db, nil
}

func verifyWarmWorktree(local context.Context, checkout, stage, head, target, library string, work func(...string) (string, error)) error {
	if _, err := work("read-tree", head); err != nil {
		return err
	}
	// A populated gitlink has its own metadata and possibly private files.
	// Git's top-level ls-files --others deliberately does not descend into it.
	entries, err := work("ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	for _, entry := range strings.Split(entries, "\x00") {
		if !strings.HasPrefix(entry, "160000 ") {
			continue
		}
		_, name, _ := strings.Cut(entry, "\t")
		sub, err := openWarmWorkspaceDir(filepath.Join(checkout, name), false)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		names, err := sub.Readdirnames(1)
		sub.Close()
		if len(names) > 0 {
			return fmt.Errorf("populated submodule %s requires inspection", name)
		}
		if !errors.Is(err, io.EOF) {
			return err
		}
	}
	if _, err := work("update-index", "--really-refresh"); err != nil {
		return fmt.Errorf("warm checkout differs from its verified baseline: %w", err)
	}
	extra, err := work("ls-files", "-z", "--others", "--directory", "--no-empty-directory", "--", ".", ":(exclude,literal)"+library)
	if err != nil {
		return err
	}
	if extra != "" {
		return fmt.Errorf("warm checkout contains unowned files: %s", extra)
	}
	if _, err := os.Lstat(filepath.Join(checkout, library)); err == nil {
		lib, err := openWarmWorkspaceDir(filepath.Join(checkout, library), false)
		if err != nil {
			return err
		}
		lib.Close()
		if err := warmTargetIgnoresLibrary(local, stage, target, library); err != nil {
			return err
		}
		if _, err := work("check-ignore", "-q", "--", library+"/"); err != nil {
			return fmt.Errorf("%s must be ignored", library)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Inspect one tree level at a time so a case-folded alias on a macOS
// volume cannot overwrite an imported Library or replace a parent by a link.
func warmTreeProtectsLibrary(ctx context.Context, db, tree, library string) error {
	components := strings.Split(filepath.ToSlash(library), "/")
	for i, component := range components {
		entries, err := gitwork.C(ctx, db, "ls-tree", "-z", tree)
		if err != nil {
			return err
		}
		next := ""
		for _, entry := range strings.Split(entries, "\x00") {
			header, name, ok := strings.Cut(entry, "\t")
			if !ok || !strings.EqualFold(name, component) {
				continue
			}
			fields := strings.Fields(header)
			if i == len(components)-1 || name != component || len(fields) != 3 || fields[1] != "tree" {
				return fmt.Errorf("commit conflicts with protected %s", library)
			}
			next = fields[2]
		}
		if next == "" {
			return nil
		}
		tree = next
	}
	return nil
}

// Git's ignore matcher consults the worktree. Materialize only ancestor
// .gitignore blobs in the empty stage to check the incoming tree's rules.
func warmTargetIgnoresLibrary(ctx context.Context, stage, target, library string) error {
	project := filepath.Dir(library)
	for dir := project; ; dir = filepath.Dir(dir) {

		name := filepath.Join(dir, ".gitignore")
		mode, err := gitwork.C(ctx, stage, "ls-tree", target, "--", name)
		if err != nil {
			return err
		}
		if strings.HasPrefix(mode, "100") {
			if _, err := gitwork.C(ctx, stage, "restore", "--source="+target, "--worktree", "--", name); err != nil {
				return err
			}
		}
		if dir == "." {
			break
		}
	}
	if _, err := gitwork.C(ctx, stage, "check-ignore", "-q", "--", library+"/"); err != nil {
		return fmt.Errorf("target commit must ignore %s", library)
	}
	return nil
}

func warmObjectFormat(commit string) string {
	if len(commit) == 64 {
		return "--object-format=sha256"
	}
	return "--object-format=sha1"
}

func warmAlternate(meta, objects string) error {
	return os.WriteFile(filepath.Join(meta, "objects", "info", "alternates"), []byte(objects+"\n"), 0600)
}
func warmRegular(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("expected a regular file at %s", path)
	}
	return nil
}

func warmSeedObjects(objects string) error {
	return filepath.WalkDir(objects, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && !entry.Type().IsRegular() {
			return fmt.Errorf("invalid seed object entry at %s", path)
		}
		if path == filepath.Join(objects, "info", "alternates") || path == filepath.Join(objects, "info", "http-alternates") {
			return errors.New("seed depends on another object store")
		}
		return nil
	})
}

// Resolve HEAD as data. Never run Git in the previous lease's Git directory.
func warmCurrentHead(meta string) (string, error) {
	read := func(name string) (string, error) {
		path := filepath.Join(meta, name)
		dir, err := openWarmWorkspaceDir(filepath.Dir(path), false)
		if err != nil {
			return "", err
		}
		dir.Close()
		if err := warmRegular(path); err != nil {
			return "", err
		}
		b, err := os.ReadFile(path)
		return strings.TrimSpace(string(b)), err
	}
	value, err := read("HEAD")
	if err != nil {
		return "", err
	}
	if ref, ok := strings.CutPrefix(value, "ref: "); ok {
		if !strings.HasPrefix(ref, "refs/heads/") || !validWarmWorkspaceRelativePath(ref) {
			return "", errors.New("invalid checkout HEAD")
		}
		value, err = read(ref)
		if errors.Is(err, os.ErrNotExist) {
			packed, err := read("packed-refs")
			if err != nil {
				return "", err
			}
			for _, line := range strings.Split(packed, "\n") {
				oid, name, _ := strings.Cut(line, " ")
				if name == ref {
					value = oid
					break
				}
			}
		} else if err != nil {
			return "", err
		}
	}
	if !warmCommitID.MatchString(value) {
		return "", errors.New("invalid warm checkout commit ID")
	}
	return value, nil
}

func warmCheckoutTarget(ctx context.Context, db string, repo domain.RunnerRepository, restore *domain.WarmWorkspaceRef) (string, string, error) {
	branch := repo.WorkBranch
	ref := repo.Ref
	if restore != nil {
		branch = restore.Branch
		ref = restore.Commit
	}
	if branch != "" {
		if _, err := gitwork.C(ctx, db, "check-ref-format", "--branch", branch); err != nil || strings.HasPrefix(branch, "-") || strings.Contains(branch, "@{") {
			return "", "", errors.New("invalid warm work branch")
		}
	}
	if restore != nil {
		if commit, ok := gitwork.RevParse(ctx, db, ref); ok {
			return commit, branch, nil
		}
		return "", "", errors.New("restore commit missing from verified cache")
	}
	if branch != "" {
		commit, exists := gitwork.RevParse(ctx, db, "refs/remotes/origin/"+branch)
		if exists && repo.CreateNewWorkBranch {
			return "", "", fmt.Errorf("remote branch %s already exists", branch)
		}
		if !exists && repo.RequireExistingWorkBranch {
			return "", "", fmt.Errorf("remote branch %s does not exist", branch)
		}
		if exists {
			return commit, branch, nil
		}
	}
	if ref == "" {
		ref = "refs/warm/default"
	} else {
		if _, err := gitwork.C(ctx, db, "check-ref-format", "refs/heads/"+ref); err != nil {
			return "", "", err
		}
		if commit, ok := gitwork.RevParse(ctx, db, "refs/remotes/origin/"+ref); ok {
			if branch == "" {
				branch = ref
			}
			return commit, branch, nil
		}
		ref = "refs/tags/" + ref
	}
	if commit, ok := gitwork.RevParse(ctx, db, ref); ok {
		return commit, branch, nil
	}
	return "", "", errors.New("requested ref missing from frozen remote")
}
