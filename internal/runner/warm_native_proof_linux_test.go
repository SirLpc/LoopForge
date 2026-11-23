package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func TestWarmNativeLocalProofChecksBytesAndIndex(t *testing.T) {
	for _, mode := range []string{"clean", "hidden edit", "staged edit", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := nativeWarmTestClone(t, newWarmGitFixture(t))
			ref := domain.WarmWorkspaceRef{RepositoryID: f.repo.RepositoryID, Branch: "master", Commit: runGitT(t, f.checkout, "rev-parse", "HEAD")}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var want error
			switch mode {
			case "hidden edit":
				runGitT(t, f.checkout, "update-index", "--assume-unchanged", "README.md")
				warmGitWrite(t, f.checkout, "README.md", "uncommitted")
				want = errWarmTreeChanged
			case "staged edit":
				warmGitWrite(t, f.checkout, "README.md", "staged")
				runGitT(t, f.checkout, "add", "README.md")
				want = errWarmTreeChanged
			case "cancelled":
				cancel()
				want = context.Canceled
			}
			p := warmReturnTree{checkout: f.checkout, native: true}
			if err := p.verifyLocal(ctx, warmWorkspaceSlot{UnityProjectPath: "."}, filepath.Join(t.TempDir(), "cache"), f.repo, ref, warmProofLFSContent); !errors.Is(err, want) {
				t.Fatalf("local proof: got %v, want %v", err, want)
			}
		})
	}
}

func TestWarmNativeLocalProofUsesArchivedPaths(t *testing.T) {
	f := nativeWarmTestClone(t, newWarmGitFixture(t))
	ref := domain.WarmWorkspaceRef{RepositoryID: f.repo.RepositoryID, Branch: "master", Commit: runGitT(t, f.checkout, "rev-parse", "HEAD")}
	warmGitWrite(t, f.checkout, "result.txt", "archived")
	digest := runtime.WorkspaceContentSHA("archived")
	p := warmReturnTree{root: t.TempDir(), checkout: f.checkout, native: true, artifactPrefix: "checkout",
		archived: map[string]string{"checkout/result.txt": digest}, collected: map[string]string{}}
	if err := p.verifyLocal(t.Context(), warmWorkspaceSlot{UnityProjectPath: "."}, filepath.Join(t.TempDir(), "cache"), f.repo, ref, warmProofLFSContent); err != nil {
		t.Fatal(err)
	}
	if p.collected["checkout/result.txt"] != digest {
		t.Fatalf("incorrect logical archive paths: %v", p.collected)
	}
}

func TestWarmNativeLocalProofUsesOnlyCachedLFS(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("Git LFS is required")
	}
	f := nativeWarmTestClone(t, newWarmGitFixture(t))
	runGitT(t, f.source, "lfs", "install", "--local")
	runGitT(t, f.source, "lfs", "track", "*.bin")
	warmGitWrite(t, f.source, "asset.bin", "original asset")
	commit := f.publish(t)
	slot := warmWorkspaceSlot{UnityProjectPath: "."}
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Vanish the remote after it has been verified by preparation.
	if err := os.Rename(f.repo.CloneURL, f.repo.CloneURL+".offline"); err != nil {
		t.Fatal(err)
	}
	ref := domain.WarmWorkspaceRef{RepositoryID: f.repo.RepositoryID, Branch: "master", Commit: commit}
	p := warmReturnTree{checkout: f.checkout, native: true}
	verify := func() error { return p.verifyLocal(t.Context(), slot, f.cache, f.repo, ref, warmProofLFSCache) }
	if err := verify(); err != nil {
		t.Fatal(err)
	}
	pointer := runGitT(t, f.checkout, "show", "HEAD:asset.bin")
	warmGitWrite(t, f.checkout, "asset.bin", pointer+"\n")
	if err := verify(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(warmLFSCache(f.cache, slot.ProjectID, f.repo)); err != nil {
		t.Fatal(err)
	}
	if err := verify(); err == nil || errors.Is(err, errWarmTreeChanged) {
		t.Fatalf("expected unavailable cache, got %v", err)
	}
}
