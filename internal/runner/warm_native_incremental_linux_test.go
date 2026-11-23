package runner

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestWarmNativeCheckoutOnlyFetchesChangedLFS(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	f := newWarmGitFixture(t)
	runGitT(t, f.source, "lfs", "install", "--local")
	runGitT(t, f.source, "lfs", "track", "*.bin")
	stable := strings.Repeat("unchanged asset", 100)
	warmGitWrite(t, f.source, "stable.bin", stable)
	warmGitWrite(t, f.source, "changed.bin", "old asset")
	f.publish(t)
	runGitT(t, f.checkout, "lfs", "install", "--local")
	runGitT(t, f.checkout, "pull", "--ff-only")
	runGitT(t, f.checkout, "lfs", "pull")
	stablePath := filepath.Join(f.checkout, "stable.bin")
	stableInfo := warmGitStat(t, stablePath)

	warmGitWrite(t, f.source, "changed.bin", "new asset")
	target := f.publish(t)
	stableOID := fmt.Sprintf("%x", sha256.Sum256([]byte(stable)))
	if err := os.Remove(warmLFSObject(filepath.Join(f.repo.CloneURL, "lfs"), stableOID)); err != nil {
		t.Fatal(err)
	}

	slot := warmWorkspaceSlot{UnityProjectPath: "."}
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatalf("an already materialized LFS object should not be fetched again: %v", err)
	}
	if got := runGitT(t, f.checkout, "rev-parse", "HEAD"); got != target {
		t.Fatalf("checkout = %s, want %s", got, target)
	}
	warmPoolRequireSame(t, stableInfo, stablePath)
	storage := filepath.Join(warmLFSCache(f.cache, slot.ProjectID, f.repo), ".git", "lfs")
	if _, err := os.Lstat(warmLFSObject(storage, stableOID)); !os.IsNotExist(err) {
		t.Fatalf("unchanged asset entered the runner cache: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(f.checkout, "changed.bin")); err != nil || string(data) != "new asset" {
		t.Fatalf("changed LFS asset = %q, %v", data, err)
	}
	if err := checkNativeWarmClean(t.Context(), f.checkout, filepath.Dir(f.cache), ""); err != nil {
		t.Fatalf("materialized LFS asset should leave a clean checkout: %v", err)
	}
}

func TestWarmNativeUnchangedCheckoutSkipsLFSAndCheckout(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	f := newWarmGitFixture(t)
	testNativeGitWrapper(t, `for arg do
	case "$arg" in lfs|checkout) echo "unexpected $arg for unchanged worktree" >&2; exit 42;; esac
done`)
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestWarmNativeSameCommitBranchChangeSkipsWorktreeCheckout(t *testing.T) {
	f := newWarmGitFixture(t)
	commit := runGitT(t, f.checkout, "rev-parse", "HEAD")
	f.repo.WorkBranch = "run/checkout-test"
	testNativeGitWrapper(t, `for arg do
	if [ "$arg" = checkout ]; then
		echo "unexpected worktree checkout for a same-commit branch change" >&2
		exit 42
	fi
done`)
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, f.checkout, "symbolic-ref", "--short", "HEAD"); got != f.repo.WorkBranch {
		t.Fatalf("branch = %q, want %q", got, f.repo.WorkBranch)
	}
	if got := runGitT(t, f.checkout, "rev-parse", "HEAD"); got != commit {
		t.Fatalf("commit = %s, want %s", got, commit)
	}
}

func TestWarmNativeSameCommitBranchChangeRecoversAfterUpdatingRef(t *testing.T) {
	f := newWarmGitFixture(t)
	f.repo.WorkBranch = "run/recovery-test"
	slot := warmWorkspaceSlot{UnityProjectPath: "."}
	interrupted := errors.New("interrupted after updating the target branch")
	var pending nativeWarmCheckoutIntent
	_, err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, slot, f.repo, nil, nil, func(intent nativeWarmCheckoutIntent) error {
		pending = intent
		runGitT(t, f.checkout, "update-ref", "refs/heads/"+intent.Target.Branch, intent.Target.Commit)
		return interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	ref, err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, slot, f.repo, nil, &pending, nil)
	if err != nil {
		t.Fatalf("resume same-commit branch attachment: %v", err)
	}
	if ref.Branch != f.repo.WorkBranch || runGitT(t, f.checkout, "symbolic-ref", "--short", "HEAD") != f.repo.WorkBranch {
		t.Fatalf("branch was not attached after resuming: %+v", ref)
	}
}

func TestWarmNativeCheckoutAndReturnUseGitExcludesForLibrary(t *testing.T) {
	f := newWarmGitFixture(t)
	warmGitWrite(t, f.checkout, ".git/info/exclude", "/Library/\n")
	warmGitWrite(t, f.source, ".gitignore", "/Temp/\n")
	target := f.publish(t)
	marker := filepath.Join(f.checkout, "Library/sentinel")
	markerInfo := warmGitStat(t, marker)
	slot := warmWorkspaceSlot{UnityProjectPath: "."}
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatalf("checkout should respect local Git excludes: %v", err)
	}
	proof := warmReturnTree{root: f.checkout, checkout: f.checkout, native: true}
	if ref, err := proof.verify(t.Context(), slot, f.cache, f.repo); err != nil || ref.Commit != target {
		t.Fatalf("return proof = %+v, %v", ref, err)
	}
	warmPoolRequireSame(t, markerInfo, marker)
}

func TestWarmNativeCheckoutRejectsHiddenIndexEdits(t *testing.T) {
	f := newWarmGitFixture(t)
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			runGitT(t, f.checkout, "update-index", flag, "README.md")
			warmGitWrite(t, f.checkout, "README.md", "local change")
			err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil })
			if err == nil {
				t.Fatal("accepted an edit hidden from git status")
			}
			if data, err := os.ReadFile(filepath.Join(f.checkout, "README.md")); err != nil || string(data) != "local change" {
				t.Fatalf("local edit changed: %q, %v", data, err)
			}
			runGitT(t, f.checkout, "update-index", "--no-"+strings.TrimPrefix(flag, "--"), "README.md")
			runGitT(t, f.checkout, "restore", "--", "README.md")
		})
	}
}

func TestWarmNativeCheckoutPreservesUnpublishedDetachedCommit(t *testing.T) {
	f := newWarmGitFixture(t)
	runGitT(t, f.checkout, "checkout", "--detach")
	runGitT(t, f.checkout, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "unpublished commit")
	before := runGitT(t, f.checkout, "rev-parse", "HEAD")
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err == nil {
		t.Fatal("preparation dropped an unpublished detached commit")
	}
	if got := runGitT(t, f.checkout, "rev-parse", "HEAD"); got != before {
		t.Fatalf("HEAD = %s, want local commit %s", got, before)
	}
}

func TestWarmNativeLeaseRejectsEditsBeforeReadiness(t *testing.T) {
	p, g := openNativeLeaseTestPool(t)
	lease := placeNativeLeaseTest(t, p, "new-lease", 1)
	file := filepath.Join(g.checkout, "README.md")
	testNativeGitWrapper(t, `for arg do
	if [ "$arg" = user.email ]; then
		"$git" "$@" || exit $?
		printf 'local edit' > `+strconv.Quote(file)+`
		exit $?
	fi
done`)
	inv := &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}, GitUserName: "Test", GitUserEmail: "test@example.invalid"}
	if err := p.prepareLeaseCheckout(t.Context(), lease, inv); err == nil || p.state.Leases[lease.OwnerRunID].GitReady {
		t.Fatalf("lease admitted a worktree edited during preparation: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "local edit" {
		t.Fatalf("local edit = %q, %v", data, err)
	}
}

func TestWarmNativeCheckoutDoesNotUseLocalGitAttributes(t *testing.T) {
	f := newWarmGitFixture(t)
	warmGitWrite(t, f.source, "ident.txt", "$Id$\n")
	f.publish(t)
	runGitT(t, f.checkout, "pull", "--ff-only")
	warmGitWrite(t, f.checkout, ".git/info/attributes", "ident.txt ident\n")
	const local = "$Id: local $\n"
	warmGitWrite(t, f.checkout, "ident.txt", local)
	runGitT(t, f.checkout, "add", "ident.txt")
	if status := runGitT(t, f.checkout, "status", "--porcelain"); status != "" {
		t.Fatalf("fixture edit must be hidden by local attributes: %s", status)
	}
	warmGitWrite(t, f.source, "ident.txt", "remote update\n")
	f.publish(t)
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err == nil {
		t.Fatal("accepted a checkout with a local Git attributes override")
	}
	if data, err := os.ReadFile(filepath.Join(f.checkout, "ident.txt")); err != nil || string(data) != local {
		t.Fatalf("local edit = %q, %v", data, err)
	}
}

func TestWarmNativeCheckoutIgnoresLocalReplacementRefs(t *testing.T) {
	f := newWarmGitFixture(t)
	before := runGitT(t, f.checkout, "rev-parse", "HEAD")
	warmGitWrite(t, f.source, "README.md", "published update\n")
	target := f.publish(t)
	runGitT(t, f.checkout, "fetch", "origin")
	runGitT(t, f.checkout, "replace", target, before)
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(f.checkout, "README.md")); err != nil || string(data) != "published update\n" {
		t.Fatalf("authorized checkout contents = %q, %v", data, err)
	}
}
