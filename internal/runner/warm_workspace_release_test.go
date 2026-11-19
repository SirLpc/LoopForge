package runner

import (
	"agentflow/internal/domain"
	"agentflow/internal/runtime"
	"os"
	"path/filepath"
	"testing"
)

func warmReturnFixture(t *testing.T) (warmPoolFixture, *warmWorkspacePool, domain.WarmWorkspaceLease, warmGitFixture) {
	t.Helper()
	f := newWarmPoolFixture(t)
	g := newWarmGitFixture(t)
	f.writeSlots(t, g.checkout)
	p := f.open(t)
	l := domain.WarmWorkspaceLease{OwnerRunID: "owner", RunnerID: "machine", Generation: 1, Epoch: 1, Slots: map[string]string{"a": "a"}}
	g.repo.RepositoryID = "a"
	g.repo.Dir = "checkout"
	if err := p.reserve(l); err != nil {
		t.Fatal(err)
	}
	if err := p.place(t.Context(), l, map[string]string{"a": "checkout"}); err != nil {
		t.Fatal(err)
	}
	if err := p.prepareLeaseCheckout(t.Context(), l, &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}}); err != nil {
		t.Fatal(err)
	}
	g.checkout = filepath.Join(f.workspace, "runs", "owner", "workspace", "checkout")
	return f, p, l, g
}

func TestWarmWorkspaceReleaseProtectsLocalData(t *testing.T) {
	changes := map[string]func(*testing.T, warmGitFixture){
		"worktree": func(t *testing.T, g warmGitFixture) { warmGitWrite(t, g.checkout, "README.md", "modified") },
		"staged": func(t *testing.T, g warmGitFixture) {
			warmGitWrite(t, g.checkout, "staged", "data")
			runGitT(t, g.checkout, "add", "staged")
		},
		"untracked": func(t *testing.T, g warmGitFixture) { warmGitWrite(t, g.checkout, "private", "data") },
		"ignored": func(t *testing.T, g warmGitFixture) {
			warmGitWrite(t, g.checkout, ".git/info/exclude", "private")
			warmGitWrite(t, g.checkout, "private", "data")
		},
		"unpublished detached HEAD": func(t *testing.T, g warmGitFixture) {
			runGitT(t, g.checkout, "checkout", "--detach")
			runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.test", "commit", "--allow-empty", "-qm", "unpublished")
		},
		"stash": func(t *testing.T, g warmGitFixture) {
			warmGitWrite(t, g.checkout, "README.md", "work")
			runGitT(t, g.checkout, "stash")
		},
		"branch": func(t *testing.T, g warmGitFixture) { runGitT(t, g.checkout, "branch", "private") },
		"tag":    func(t *testing.T, g warmGitFixture) { runGitT(t, g.checkout, "tag", "private") },
		"submodule metadata": func(t *testing.T, g warmGitFixture) {
			warmGitWrite(t, g.checkout, ".git/modules/child/refs/heads/local-only", "unpublished")
		},
		"pending": func(t *testing.T, g warmGitFixture) { warmGitWrite(t, g.checkout, ".git/MERGE_HEAD", "pending") },
		"submodule": func(t *testing.T, g warmGitFixture) {
			head := runGitT(t, g.checkout, "rev-parse", "HEAD")
			runGitT(t, g.checkout, "update-index", "--add", "--cacheinfo", "160000,"+head+",child")
			runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.test", "commit", "-qm", "submodule")
			runGitT(t, g.checkout, "push", "origin", "HEAD")
			warmGitWrite(t, g.checkout, "child/unsubmitted", "work")
		},
		"tracked Library": func(t *testing.T, g warmGitFixture) {
			runGitT(t, g.checkout, "add", "-f", "Library/sentinel")
			runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.test", "commit", "-qm", "tracked")
			runGitT(t, g.checkout, "push", "origin", "HEAD")
		},
		"hidden edit": func(t *testing.T, g warmGitFixture) {
			runGitT(t, g.checkout, "update-index", "--assume-unchanged", "README.md")
			warmGitWrite(t, g.checkout, "README.md", "modified")
		},
		"outside": func(t *testing.T, g warmGitFixture) { warmGitWrite(t, filepath.Dir(g.checkout), "outside", "data") },
		"revoked": func(t *testing.T, g warmGitFixture) {
			if err := os.Rename(g.repo.CloneURL, g.repo.CloneURL+".offline"); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			_, p, l, g := warmReturnFixture(t)
			change(t, g)
			if _, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo}); err == nil {
				t.Error("released unproven checkout")
			}
			if p.state.Leases[l.OwnerRunID].Phase != warmWorkspacePlaced {
				t.Error("lost owner")
			}
			if _, err := os.Stat(g.checkout); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWarmWorkspaceReleaseRequiresMatchingArtifactBytes(t *testing.T) {
	f, p, l, g := warmReturnFixture(t)
	file := domain.WorkspaceArtifactFile{Path: "checkout/out/report.bin", Content: "verified"}
	file.SHA256 = runtime.WorkspaceContentSHA(file.Content)
	warmGitWrite(t, g.checkout, "out/report.bin", "different")
	inv := []domain.RunnerRepository{g.repo}
	if _, err := p.proveReturnWithArtifacts(t.Context(), l, inv, []domain.WorkspaceArtifactFile{file}); err == nil {
		t.Fatal("accepted different bytes")
	}
	warmGitWrite(t, g.checkout, "out/report.bin", "verified")
	refs, err := p.proveReturnWithArtifacts(t.Context(), l, inv, []domain.WorkspaceArtifactFile{file})
	if err != nil || refs[0].Artifacts[file.Path] != file.SHA256 {
		t.Fatal(refs, err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	p = f.open(t)
	d := Daemon{warmWorkspacePool: p, state: daemonState{RunnerID: l.RunnerID}}
	if err := d.checkWarmOperation(t.Context(), l.OwnerRunID, "file", &l); err == nil {
		t.Fatal("reopened a proven checkout")
	}
	other := l
	other.OwnerRunID = "next"
	other.Generation++
	if err := p.reserve(other); err == nil {
		t.Fatal("reserved before acknowledgement")
	}
	if err := p.commitReturn(t.Context(), l, nil); err == nil {
		t.Fatal("moved without proof")
	}
	if err := p.commitReturn(t.Context(), l, refs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(p.slots["a"].Path, "out/report.bin")); !os.IsNotExist(err) {
		t.Fatal("artifact leaked to next owner", err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	p = f.open(t)
	if err := p.commitReturn(t.Context(), l, refs); err != nil {
		t.Fatal("lost acknowledgement not retryable", err)
	}
	if err := p.reserve(other); err != nil {
		t.Fatal(err)
	}
}
func TestWarmWorkspaceReleaseKeepsLibraryAndRestoresExactCommit(t *testing.T) {
	_, p, l, g := warmReturnFixture(t)
	runGitT(t, g.checkout, "checkout", "--detach")
	pin := runGitT(t, g.checkout, "rev-parse", "HEAD")
	refs, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo})
	if err != nil || len(refs) != 1 || refs[0].Branch != "" || refs[0].Commit != pin {
		t.Fatalf("proof %+v: %v", refs, err)
	}
	before := warmPoolStat(t, filepath.Join(g.checkout, "Library", "sentinel"))
	if err := p.commitReturn(t.Context(), l, refs); err != nil {
		t.Fatal(err)
	}
	home := p.slots["a"].Path
	warmGitWrite(t, g.source, "advanced", "data")
	g.publish(t)
	if err := prepareWarmCheckout(t.Context(), home, p.gitCache("a"), g.repo, &refs[0]); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, home, "rev-parse", "HEAD"); got != pin {
		t.Fatal(got)
	}
	warmPoolRequireSame(t, before, filepath.Join(home, "Library", "sentinel"))
}
