package runner

import (
	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The owning daemon holds both the operation and execution gates, and has
// stopped containers using the directory, before starting either return step.
func (p *warmWorkspacePool) proveReturn(ctx context.Context, lease domain.WarmWorkspaceLease, repos []domain.RunnerRepository) ([]domain.WarmWorkspaceRef, error) {
	return p.proveReturnWithArtifacts(ctx, lease, repos, nil)
}

func (p *warmWorkspacePool) proveReturnWithArtifacts(ctx context.Context, lease domain.WarmWorkspaceLease, repos []domain.RunnerRepository, files []domain.WorkspaceArtifactFile) ([]domain.WarmWorkspaceRef, error) {
	p.mu.Lock()
	a, err := p.loadLease(ctx, lease)
	if err == nil && a.Phase != warmWorkspaceReleased {
		_, err = p.inspectLease(a)
	}
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if a.ReturnProven {
		return a.ReturnRefs, nil
	}
	if p.native && a.NativeCheckout != nil {
		a, err = p.recoverNativeCheckoutForReturn(ctx, a, repos)
		if err != nil {
			return nil, err
		}
	}
	if a.Phase == warmWorkspaceReserved || a.Phase == warmWorkspacePlaced && !a.GitReady && !a.PreparingGit && (!a.CheckoutComplete || p.native) {
		// Some repositories may already be prepared when a later fetch fails.
		// Verify each finished checkout before returning the unadmitted group.
		for id, prepared := range a.PreparedRefs {
			i := slices.IndexFunc(repos, func(r domain.RunnerRepository) bool { return r.RepositoryID == prepared.RepositoryID })
			if i < 0 {
				return nil, errors.New("missing partially prepared repository")
			}
			check := warmReturnTree{root: filepath.Dir(a.Targets[id]), checkout: a.Targets[id], native: p.native}
			actual, err := check.verify(ctx, p.slots[id], p.gitCache(id), repos[i])
			if err != nil {
				return nil, err
			}
			if actual.Branch != prepared.Branch || actual.Commit != prepared.Commit || len(check.remove) > 0 {
				return nil, errors.New("partially prepared repository changed; inspect before return")
			}
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return nil, p.saveWarmReturnProof(a, nil, nil)
	}
	if a.Phase != warmWorkspacePlaced || !a.GitReady {
		return nil, errors.New("warm checkout is not prepared for return")
	}
	dirs := map[string]string{}
	if len(repos) != len(lease.Slots) {
		return nil, errors.New("incomplete return repository list")
	}
	for _, r := range repos {
		dirs[r.RepositoryID] = r.Dir
	}
	targets, err := p.ownerTargets(lease, dirs)
	if err != nil || !maps.Equal(targets, a.Targets) {
		return nil, errors.New("return does not match placed repositories")
	}
	root := filepath.Join(p.workspaceRoot, "runs", lease.OwnerRunID, "workspace")
	archived := map[string]string{}
	for _, f := range files {
		if !validWarmWorkspaceRelativePath(f.Path) || !utf8.ValidString(f.Content) || strings.ContainsRune(f.Content, 0) || len(f.Content) > 1<<20 || runtime.WorkspaceContentSHA(f.Content) != f.SHA256 {
			return nil, fmt.Errorf("invalid archived file %q", f.Path)
		}
		for _, part := range strings.Split(f.Path, "/") {
			if strings.HasPrefix(part, ".") {
				return nil, fmt.Errorf("invalid archived path %q", f.Path)
			}
		}
		archived[f.Path] = f.SHA256
	}
	var refs []domain.WarmWorkspaceRef
	var remove []string
	skip := map[string]bool{}
	for _, r := range repos {
		slot := p.slots[lease.Slots[r.RepositoryID]]
		target := targets[slot.ID]
		proof := warmReturnTree{root: root, checkout: target, archived: archived, collected: map[string]string{}, native: p.native}
		ref, err := proof.verify(ctx, slot, p.gitCache(slot.ID), r)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.Dir, err)
		}
		ref.Artifacts = proof.collected
		refs = append(refs, ref)
		remove = append(remove, proof.remove...)
		skip[target] = true
	}
	extra := warmReturnTree{root: root, archived: archived, collected: map[string]string{}}
	if err := extra.verifyExtra(ctx, skip); err != nil {
		return nil, err
	}
	maps.Copy(refs[0].Artifacts, extra.collected)
	remove = append(remove, extra.remove...)
	// Journal proof before responding. Until commit (including after a restart)
	// normal reads, terminals and execution cannot reopen the frozen checkout.
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err = p.loadLease(ctx, lease)
	if err != nil {
		return nil, err
	}
	return refs, p.saveWarmReturnProof(a, refs, remove)
}

type warmReturnTree struct {
	root, checkout      string
	artifactPrefix      string
	native              bool
	archived, collected map[string]string
	remove              []string
}

func (w *warmReturnTree) verifyExtra(ctx context.Context, skip map[string]bool) error {
	return filepath.WalkDir(w.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if skip[path] || path == filepath.Join(w.root, ".agentflow") && d.IsDir() {
			return filepath.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		return w.archive(path, d)
	})
}

func (w *warmReturnTree) archive(path string, d fs.DirEntry) error {
	aliasRoot := w.root
	if w.artifactPrefix != "" {
		aliasRoot = w.checkout
	}
	rel, err := filepath.Rel(aliasRoot, path)
	if err != nil {
		return err
	}
	rel = filepath.Join(w.artifactPrefix, rel)
	digest, ok := w.archived[rel]
	if !ok || !d.Type().IsRegular() {
		return warmTreeChanged("commit, archive or remove %s before returning this workspace", rel)
	}
	info, err := d.Info()
	if err != nil {
		return err
	}
	if info.Size() > 1<<20 || !w.native && info.Mode().Perm()&0111 != 0 {
		return warmTreeChanged("archive cannot restore %s exactly", rel)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if runtime.WorkspaceContentSHA(string(b)) != digest {
		return warmTreeChanged("archived content differs at %s", rel)
	}
	w.collected[rel] = digest
	w.remove = append(w.remove, rel)
	return nil
}

func (w *warmReturnTree) verify(ctx context.Context, slot warmWorkspaceSlot, cache string, r domain.RunnerRepository) (domain.WarmWorkspaceRef, error) {
	var ref domain.WarmWorkspaceRef
	meta := filepath.Join(w.checkout, ".git")
	if _, err := warmSeedIdentity(meta); err != nil {
		return ref, err
	}
	if err := checkWarmStoredModules(meta); err != nil {
		return ref, err
	}
	ref, err := warmCheckoutRef(w.checkout, r.RepositoryID)
	if err != nil {
		return ref, err
	}
	head, branch := ref.Commit, ref.Branch
	private, err := os.MkdirTemp(filepath.Dir(cache), ".return-")
	if err != nil {
		return ref, err
	}
	defer os.RemoveAll(private)
	// The cache was created outside all sandbox mounts except its read-only
	// objects directory. Remote and origin are bound to the frozen registration.
	id, err := gitwork.C(ctx, cache, "config", "--get", "warm.repositoryID")
	if err != nil || id != r.RepositoryID {
		return ref, errors.New("warm cache repository changed")
	}
	origin, err := gitwork.C(ctx, cache, "remote", "get-url", "origin")
	if err != nil || origin != r.CloneURL {
		return ref, errors.New("warm cache origin changed")
	}
	if err := warmRegular(filepath.Join(meta, "config")); err != nil {
		return ref, err
	}
	local, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	origin, err = gitwork.C(local, private, "config", "--no-includes", "--file", filepath.Join(meta, "config"), "--get-all", "remote.origin.url")
	if err != nil || domain.URLHasCredentials(origin) || gitwork.CanonicalRemote(origin) != gitwork.CanonicalRemote(r.CloneURL) {
		return ref, errors.New("checkout origin differs from frozen repository")
	}
	auth := repositoryGitContext(ctx, r)
	if _, err = gitwork.C(auth, cache, "fetch", "--no-auto-gc", "--prune", "--no-tags", "origin", "+HEAD:refs/warm/default", "+refs/heads/*:refs/remotes/origin/*", "+refs/tags/*:refs/tags/*"); err != nil {
		return ref, err
	}
	if err := authorizeWarmRestore(local, auth, private, cache, r, head); err != nil {
		return ref, err
	}
	if branch != "" {
		if _, err := gitwork.C(local, cache, "check-ref-format", "--branch", branch); err != nil {
			return ref, err
		}
	}
	if err := validateWarmLFSConfig(local, cache, head, r.CloneURL); err != nil {
		return ref, err
	}
	if !w.native {
		if err := verifyWarmRefs(local, meta, cache, branch, true); err != nil {
			return ref, err
		}
	}

	return ref, w.verifyLocal(ctx, slot, cache, r, ref, warmProofLFSFetch)
}

// Read actual bytes independently of index stat hints, excludes, filters,
// hooks and submodule configuration in the writable checkout.
func warmBlobMatches(ctx context.Context, stage, head string, lfs warmLFSManifest, path, rel, oid, mode string) bool {
	ok, err := warmBlobMatchesWithRules(ctx, stage, head, lfs, path, rel, oid, mode, warmCheckoutFileRules{fileMode: true, symlinks: true})
	return err == nil && ok
}

type warmCheckoutFileRules struct {
	fileMode, symlinks bool
}

func warmBlobMatchesWithRules(ctx context.Context, stage, head string, lfs warmLFSManifest, path, rel, oid, mode string, rules warmCheckoutFileRules) (bool, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	var h hash.Hash = sha1.New()
	if len(oid) == 64 {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d%c", st.Size(), 0)
	if mode == "120000" {
		if st.Mode()&os.ModeSymlink == 0 {
			if rules.symlinks || !st.Mode().IsRegular() {
				return false, nil
			}
			f, err := os.Open(path)
			if err != nil {
				return false, err
			}
			defer f.Close()
			_, err = io.Copy(h, f)
			return hex.EncodeToString(h.Sum(nil)) == oid, err
		}
		dest, err := os.Readlink(path)
		if err != nil {
			return false, err
		}
		io.WriteString(h, dest)
		return hex.EncodeToString(h.Sum(nil)) == oid, nil
	}
	if !st.Mode().IsRegular() || rules.fileMode && (st.Mode()&0100 != 0) != (mode == "100755") {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	digest := sha256.New()
	if _, err = io.Copy(io.MultiWriter(h, digest), f); err != nil {
		return false, err
	}
	matches := hex.EncodeToString(h.Sum(nil)) == oid
	if item, ok := lfs.byPath[rel]; ok {
		// The manifest was read from the committed tree, and its cached
		// objects already passed SHA-256 and size verification.
		size, verified := lfs.verified[item.OID]
		return (lfs.contentOnly || verified && size == item.Size) && (matches || st.Size() == item.Size && hex.EncodeToString(digest.Sum(nil)) == item.OID), nil
	}
	if matches {
		const prefix = "version https://git-lfs.github.com/spec/v1\n"
		if st.Size() < int64(len(prefix)) || st.Size() > 1024 {
			return true, nil
		}
		var start [len(prefix)]byte
		if _, err := f.ReadAt(start[:], 0); err != nil {
			return false, err
		}
		if string(start[:]) != prefix {
			return true, nil
		}
	}
	size, err := gitwork.C(ctx, stage, "cat-file", "-s", oid)
	n, parseErr := strconv.ParseInt(size, 10, 64)
	if err != nil || parseErr != nil {
		return false, errors.Join(err, parseErr)
	}
	if n > 1024 {
		return warmCheckoutBlobMatches(ctx, stage, head, path, rel, oid, digest.Sum(nil))
	}
	pointer, err := gitwork.C(ctx, stage, "cat-file", "blob", oid)
	if err != nil {
		return false, err
	}
	if !strings.HasPrefix(pointer, "version https://git-lfs.github.com/spec/v1\n") {
		if matches {
			return true, nil
		}
		return warmCheckoutBlobMatches(ctx, stage, head, path, rel, oid, digest.Sum(nil))
	}
	var object string
	var length int64
	if _, err := fmt.Sscanf(pointer, "version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d", &object, &length); err != nil || len(object) != 64 || length < 0 || strings.ContainsAny(object, "/\\") {
		return false, nil
	}
	if _, err := hex.DecodeString(object); err != nil {
		return false, nil
	}
	if !matches && (hex.EncodeToString(digest.Sum(nil)) != object || st.Size() != length) {
		return false, nil
	}
	objectSize, verified := lfs.verified[object]
	return lfs.contentOnly || verified && objectSize == length, nil
}

func restoreWarmProofFiles(ctx context.Context, stage, head string, paths ...string) error {
	args := []string{"--literal-pathspecs", "-c", "core.attributesFile=" + os.DevNull, "restore", "--source=" + head, "--worktree", "--"}
	_, err := gitwork.C(ctx, stage, append(args, paths...)...)
	return err
}

// Compare the reconstructed checkout bytes, then allow Git's text line-ending
// normalization. Attributes and Git config come from the private proof checkout.
func warmCheckoutBlobMatches(ctx context.Context, stage, head, actualPath, rel, oid string, actual []byte) (bool, error) {
	if err := restoreWarmProofFiles(ctx, stage, head, rel); err != nil {
		return false, err
	}
	path := filepath.Join(stage, rel)
	if filepath.Base(rel) != ".gitattributes" {
		defer os.Remove(path)
	}
	expected, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer expected.Close()
	h := sha256.New()
	if _, err = io.Copy(h, expected); err != nil {
		return false, err
	}
	if bytes.Equal(h.Sum(nil), actual) {
		return true, nil
	}

	// Other check-in conversions can discard changes beyond line endings.
	attributesFile := "core.attributesFile=" + os.DevNull
	attrs, err := gitwork.C(ctx, stage, "-c", attributesFile, "check-attr", "-z", "filter", "ident", "working-tree-encoding", "--", rel)
	fields := strings.Split(attrs, "\x00")
	if err != nil {
		return false, err
	}
	if len(fields) != 10 {
		return false, errors.New("invalid Git attributes for proof file")
	}
	for _, value := range []string{fields[2], fields[5], fields[8]} {
		if value != "unspecified" && value != "unset" {
			return false, nil
		}
	}
	cleanOID, err := gitwork.C(ctx, stage, "-c", attributesFile, "-c", "core.safecrlf=false", "hash-object", "--path="+rel, "--", actualPath)
	return cleanOID == oid, err
}

func sameWarmReturnRefs(a, b []domain.WarmWorkspaceRef) bool {
	return slices.EqualFunc(a, b, func(a, b domain.WarmWorkspaceRef) bool {
		return a.RepositoryID == b.RepositoryID && a.Commit == b.Commit && a.Branch == b.Branch && maps.Equal(a.Artifacts, b.Artifacts)
	})
}

func (p *warmWorkspacePool) commitReturn(ctx context.Context, l domain.WarmWorkspaceLease, refs []domain.WarmWorkspaceRef) error {
	p.mu.Lock()
	a, err := p.loadLease(ctx, l)
	if err == nil && a.Phase != warmWorkspaceReleased {
		_, err = p.inspectLease(a)
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if !a.ReturnProven || !sameWarmReturnRefs(a.ReturnRefs, refs) {
		return errors.New("missing durable acknowledgement of this return proof")
	}
	if a.Phase == warmWorkspacePlaced && len(a.ReturnRemove) != 0 {
		root, err := os.OpenRoot(filepath.Join(p.workspaceRoot, "runs", l.OwnerRunID, "workspace"))
		if err != nil {
			return err
		}
		defer root.Close()
		ignored, err := p.warmReturnIgnores(ctx, root.Name(), a)
		if err != nil {
			return err
		}
		for _, rel := range a.ReturnRemove {
			if !validWarmWorkspaceRelativePath(rel) {
				return errors.New("invalid return cleanup path")
			}
			if ignored[rel] {
				continue
			}
			var digest string
			for _, ref := range refs {
				if d, ok := ref.Artifacts[rel]; ok {
					digest = d
					break
				}
			}
			info, err := root.Lstat(rel)
			if os.IsNotExist(err) {
				if err := syncWarmWorkspaceParent(filepath.Join(root.Name(), rel)); err != nil {
					return err
				}
				continue
			}
			if err != nil {
				return err
			}
			if digest == "" {
				return fmt.Errorf("missing archived artifact digest for %s", rel)
			}
			if !info.Mode().IsRegular() || info.Size() > 1<<20 {
				return fmt.Errorf("archived artifact changed at %s", rel)
			}
			b, err := root.ReadFile(rel)
			if err != nil || runtime.WorkspaceContentSHA(string(b)) != digest {
				return fmt.Errorf("archived artifact changed at %s", rel)
			}
			if err := root.Remove(rel); err != nil {
				return err
			}
			if err := syncWarmWorkspaceParent(filepath.Join(root.Name(), rel)); err != nil {
				return err
			}
		}
	}
	return p.returnSlots(ctx, l)
}
