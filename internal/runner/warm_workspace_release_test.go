package runner

import (
	"agentflow/internal/domain"
	"agentflow/internal/runtime"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
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

func TestWarmWorkspaceReleaseWithCommittedLineEndingAttributes(t *testing.T) {
	for _, tc := range []struct {
		name, attributes, committed, content string
		wantReturn                           bool
	}{
		{"clean CRLF checkout", "text eol=crlf", "original\nsecond line\n", "original\r\nsecond line\r\n", true},
		{"mixed endings with CRLF policy", "text eol=crlf", "original\nsecond line\n", "original\r\nsecond line\n", true},
		{"mixed endings with LF policy", "text eol=lf", "original\nsecond line\n", "original\r\nsecond line\n", true},
		{"mixed endings with automatic text detection", "text=auto", "original\nsecond line\n", "original\r\nsecond line\n", true},
		{"uncommitted edit", "text eol=lf", "original\nsecond line\n", "edited\r\nsecond line\n", false},
		{"binary attribute", "-text eol=lf", "original\nsecond line\n", "original\r\nsecond line\n", false},
		{"automatic detection of binary content", "text=auto", "original\x00\n", "original\x00\r\n", false},
		{"different ident expansion", "text eol=lf ident", "$Id$\n", "$Id: other $\r\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, lease, g := warmReturnFixture(t)
			warmGitWrite(t, g.checkout, ".gitattributes", "file.txt "+tc.attributes+"\n")
			warmGitWrite(t, g.checkout, "file.txt", tc.committed)
			runGitT(t, g.checkout, "add", ".gitattributes", "file.txt")
			runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.test", "commit", "-qm", "tracked file")
			runGitT(t, g.checkout, "push", "origin", "HEAD")
			warmGitWrite(t, g.checkout, "file.txt", tc.content)

			refs, err := p.proveReturn(t.Context(), lease, []domain.RunnerRepository{g.repo})
			if !tc.wantReturn {
				if err == nil || !strings.Contains(err.Error(), "changes in file.txt") {
					t.Fatalf("changed file did not block return: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("checkout with equivalent line endings could not return: %v", err)
			}
			if err := p.commitReturn(t.Context(), lease, refs); err != nil {
				t.Fatal(err)
			}
			content, err := os.ReadFile(filepath.Join(p.slots["a"].Path, "file.txt"))
			if err != nil || string(content) != tc.content {
				t.Fatalf("returned file = %q, err = %v", content, err)
			}
			next := lease
			next.OwnerRunID = "next-owner"
			next.Generation++
			if err := p.reserve(next); err != nil {
				t.Fatal(err)
			}
			if err := p.place(t.Context(), next, map[string]string{"a": "checkout"}); err != nil {
				t.Fatal(err)
			}
			if err := p.prepareLeaseCheckout(t.Context(), next, &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}}); err != nil {
				t.Fatalf("the next lease could not use the returned checkout: %v", err)
			}
		})
	}
}

func TestWarmWorkspaceReleaseProtectsLocalData(t *testing.T) {
	changes := map[string]func(*testing.T, warmGitFixture){
		"worktree": func(t *testing.T, g warmGitFixture) { warmGitWrite(t, g.checkout, "README.md", "modified") },
		"Library missing committed ignore rule": func(t *testing.T, g warmGitFixture) {
			warmGitWrite(t, g.checkout, ".git/info/exclude", "/Library/\n")
			warmGitWrite(t, g.checkout, ".gitignore", "# No committed Library rule\n")
			runGitT(t, g.checkout, "add", ".gitignore")
			runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.test", "commit", "-qm", "remove Library ignore")
			runGitT(t, g.checkout, "push", "origin", "HEAD")
		},
		"staged": func(t *testing.T, g warmGitFixture) {
			warmGitWrite(t, g.checkout, "staged", "data")
			runGitT(t, g.checkout, "add", "staged")
		},
		"untracked": func(t *testing.T, g warmGitFixture) { warmGitWrite(t, g.checkout, "private", "data") },
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

func TestWarmWorkspaceReusesSlotWithIgnoredFiles(t *testing.T) {
	_, p, lease, g := warmReturnFixture(t)
	warmGitWrite(t, g.checkout, ".git/info/exclude", "/ local.txt\n/local.pipe\n")
	files := map[string]string{
		".superpowers/sdd/.gitignore":        "*\n",
		".superpowers/sdd/draft/progress.md": "draft",
		" local.txt":                         "locally excluded",
	}
	for name, contents := range files {
		warmGitWrite(t, g.checkout, name, contents)
	}
	if err := unix.Mkfifo(filepath.Join(g.checkout, "local.pipe"), 0600); err != nil {
		t.Fatal(err)
	}

	refs, err := p.proveReturn(t.Context(), lease, []domain.RunnerRepository{g.repo})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.commitReturn(t.Context(), lease, refs); err != nil {
		t.Fatal(err)
	}
	checkFiles := func(checkout string) {
		t.Helper()
		for name, want := range files {
			got, err := os.ReadFile(filepath.Join(checkout, name))
			if err != nil || string(got) != want {
				t.Fatalf("%s = %q, want %q, error %v", name, got, want, err)
			}
		}
		if info, err := os.Lstat(filepath.Join(checkout, "local.pipe")); err != nil || info.Mode()&os.ModeNamedPipe == 0 {
			t.Fatalf("ignored pipe changed or removed: %v", err)
		}
	}
	checkFiles(p.slots["a"].Path)

	warmGitWrite(t, g.source, ".superpowers/README.md", "tracked sibling")
	g.publish(t)
	next := lease
	next.OwnerRunID = "next-owner"
	next.Generation++
	if err := p.reserve(next); err != nil {
		t.Fatal(err)
	}
	if err := p.place(t.Context(), next, map[string]string{"a": "checkout"}); err != nil {
		t.Fatal(err)
	}
	if err := p.prepareLeaseCheckout(t.Context(), next, &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}}); err != nil {
		t.Fatal(err)
	}
	nextCheckout := filepath.Join(p.workspaceRoot, "runs", next.OwnerRunID, "workspace", "checkout")
	checkFiles(nextCheckout)
	if got := runGitT(t, nextCheckout, "status", "--porcelain"); got != "" {
		t.Fatalf("next checkout lists ignored files: %s", got)
	}
}

func TestWarmWorkspaceLegacyReturnKeepsIgnoredArtifact(t *testing.T) {
	_, p, lease, g := warmReturnFixture(t)
	const name, contents = "report.txt", "keep me"
	warmGitWrite(t, g.checkout, name, contents)
	files := []domain.WorkspaceArtifactFile{{
		Path: "checkout/" + name, Content: contents, SHA256: runtime.WorkspaceContentSHA(contents),
	}}
	refs, err := p.proveReturnWithArtifacts(t.Context(), lease, []domain.RunnerRepository{g.repo}, files)
	if err != nil {
		t.Fatal(err)
	}
	// The previous return policy could also record already ignored files as
	// artifacts. The current policy must preserve files in those proofs.
	warmGitWrite(t, g.checkout, ".git/info/exclude", "/"+name+"\n")
	if err := p.commitReturn(t.Context(), lease, refs); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(p.slots["a"].Path, name)); err != nil || string(got) != contents {
		t.Fatalf("ignored artifact = %q, err = %v", got, err)
	}
}

func TestWarmReturnVerifiesManifestLFSFileWithoutGitPerPath(t *testing.T) {
	const contents = "hydrated object"
	object := fmt.Sprintf("%x", sha256.Sum256([]byte(contents)))
	pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", object, len(contents))
	checkout := t.TempDir()
	warmGitWrite(t, checkout, "asset", contents)
	if err := os.Mkdir(filepath.Join(checkout, "proof"), 0700); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(checkout, "proof")
	cmd := exec.Command("git", "hash-object", "--stdin")
	cmd.Stdin = strings.NewReader(pointer)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	manifest := warmLFSManifest{byPath: map[string]warmLFSFile{"asset": {OID: object, Size: int64(len(contents))}}, verified: map[string]int64{object: int64(len(contents))}}
	check := func() bool {
		return warmBlobMatches(t.Context(), stage, "", manifest, filepath.Join(checkout, "asset"), "asset", strings.TrimSpace(string(out)), "100644")
	}
	if !check() {
		t.Fatal("verified hydrated bytes should match without a Git object lookup")
	}
	warmGitWrite(t, checkout, "asset", pointer)
	if !check() {
		t.Fatal("the exact committed pointer should also match")
	}
	warmGitWrite(t, checkout, "asset", strings.Repeat("x", len(contents)))
	if check() {
		t.Fatal("modified bytes matched the verified manifest")
	}
}

func TestWarmWorkspaceReleasePreservesUnprovenGeneratedAndLFS(t *testing.T) {
	t.Run("generated", func(t *testing.T) {
		_, p, l, g := warmReturnFixture(t)
		slot := p.slots["a"]
		slot.DiscardGenerated = []string{"Temp"}
		p.slots["a"] = slot
		warmGitWrite(t, g.checkout, "Temp/user-work", "unique")
		if _, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo}); err == nil {
			t.Fatal("accepted unignored generated directory")
		}
	})
	for _, tc := range []struct {
		name, rule string
		ignored    bool
	}{{"ignored directory", "/Temp/\n", true}, {"negated child", "/Temp/*\n!/Temp/user-work\n", false}} {
		t.Run(tc.name, func(t *testing.T) {
			_, p, l, g := warmReturnFixture(t)
			slot := p.slots["a"]
			slot.DiscardGenerated = []string{"Temp"}
			p.slots["a"] = slot
			warmGitWrite(t, g.checkout, ".gitignore", "/Library/\n"+tc.rule)
			runGitT(t, g.checkout, "add", ".gitignore")
			runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.test", "commit", "-m", "ignore generated files")
			runGitT(t, g.checkout, "push", "origin", "HEAD")
			warmGitWrite(t, g.checkout, "Temp/user-work", "unique")
			refs, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo})
			if !tc.ignored {
				if err == nil {
					t.Fatal("accepted re-included user file")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := p.commitReturn(t.Context(), l, refs); err != nil {
				t.Fatal(err)
			}
			if content, err := os.ReadFile(filepath.Join(p.slots["a"].Path, "Temp", "user-work")); err != nil || string(content) != "unique" {
				t.Fatalf("ignored generated file = %q, err = %v", content, err)
			}
		})
	}
	t.Run("resumed removal", func(t *testing.T) {
		_, p, l, g := warmReturnFixture(t)
		slot := p.slots["a"]
		slot.DiscardGenerated = []string{"Temp"}
		p.slots["a"] = slot
		refs, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo})
		if err != nil {
			t.Fatal(err)
		}
		a := p.state.Leases[l.OwnerRunID]
		a.ReturnRemove = []string{"checkout/Temp"}
		if err := p.saveAllocation(a); err != nil {
			t.Fatal(err)
		}
		warmGitWrite(t, g.checkout, "Temp/user-work", "unique")
		if err := p.commitReturn(t.Context(), l, refs); err == nil {
			t.Fatal("replayed unproven recursive cleanup")
		}
		if _, err := os.Stat(filepath.Join(g.checkout, "Temp", "user-work")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("remote LFS", func(t *testing.T) {
		if _, err := exec.LookPath("git-lfs"); err != nil {
			t.Skip("requires git-lfs")
		}
		_, p, l, g := warmReturnFixture(t)
		runGitT(t, g.checkout, "lfs", "install", "--local")
		runGitT(t, g.checkout, "lfs", "track", "*.bin")
		warmGitWrite(t, g.checkout, "asset.bin", "verified large asset")
		runGitT(t, g.checkout, "add", "asset.bin", ".gitattributes")
		runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.test", "commit", "-m", "asset")
		runGitT(t, g.checkout, "push", "origin", "HEAD")
		runGitT(t, g.checkout, "lfs", "push", "--all", "origin")
		if _, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unavailable LFS", func(t *testing.T) {
		_, p, l, g := warmReturnFixture(t)
		content := "unique large asset"
		pointer := fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%x\nsize %d\n", sha256.Sum256([]byte(content)), len(content))
		warmGitWrite(t, g.checkout, "asset", pointer)
		runGitT(t, g.checkout, "add", "asset")
		runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.test", "commit", "-m", "asset")
		runGitT(t, g.checkout, "push", "origin", "HEAD")
		warmGitWrite(t, g.checkout, "asset", content)
		git, err := exec.LookPath("git")
		if err != nil {
			t.Fatal(err)
		}
		bin := t.TempDir()
		if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		if _, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo}); err == nil {
			t.Fatal("accepted hydrated asset without remote proof")
		}
	})
}

func TestWarmWorkspaceLFSPrepareRequiresVerifier(t *testing.T) {
	g := newWarmGitFixture(t)
	warmGitWrite(t, g.source, "asset", "version https://git-lfs.github.com/spec/v1\noid sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nsize 10\n")
	g.publish(t)
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if err := prepareWarmCheckout(t.Context(), g.checkout, g.cache, g.repo, nil); err == nil {
		t.Fatal("prepared a pointer with no verified LFS content")
	}
}
func TestWarmWorkspaceLFSRetrySyncsExistingObjectDirectories(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("requires git-lfs")
	}
	g := newWarmGitFixture(t)
	runGitT(t, g.source, "lfs", "install", "--local")
	runGitT(t, g.source, "lfs", "track", "*.bin")
	const content = "cached Unity asset"
	warmGitWrite(t, g.source, "asset.bin", content)
	head := g.publish(t)
	scope := t.TempDir()
	storage := filepath.Join(scope, ".git", "lfs")
	interrupted := errors.New("injected directory sync failure")
	synced := map[string]bool{}
	fail := true
	syncDir := func(dir *os.File) error {
		if fail && dir.Name() == storage {
			return interrupted
		}
		if err := dir.Sync(); err != nil {
			return err
		}
		synced[dir.Name()] = true
		return nil
	}
	prepare := func() error {
		_, err := prepareWarmLFS(t.Context(), t.Context(), g.source, filepath.Join(g.source, ".git"), scope, head, g.repo, syncDir)
		return err
	}
	if err := prepare(); !errors.Is(err, interrupted) {
		t.Fatalf("first publication: %v", err)
	}
	remote := filepath.Join(g.repo.CloneURL, "lfs", "objects")
	if err := os.Rename(remote, remote+".held"); err != nil {
		t.Fatal(err)
	}
	if err := prepare(); !errors.Is(err, interrupted) {
		t.Fatalf("all-hit retry bypassed failed directory publication: %v", err)
	}
	clear(synced)
	fail = false
	if err := prepare(); err != nil {
		t.Fatal(err)
	}
	oid := fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
	for dir := filepath.Dir(warmLFSObject(storage, oid)); ; dir = filepath.Dir(dir) {
		if !synced[dir] {
			t.Errorf("cache returned without syncing %s", dir)
		}
		if dir == scope {
			break
		}
	}
}
func TestWarmWorkspaceLFSTransfersOnceAcrossReturnsAndSlots(t *testing.T) {
	realLFS, err := exec.LookPath("git-lfs")
	if err != nil {
		t.Skip("requires git-lfs")
	}
	g := newWarmGitFixture(t)
	runGitT(t, g.source, "lfs", "install", "--local")
	runGitT(t, g.source, "lfs", "track", "*.bin")
	warmGitWrite(t, g.source, "asset.bin", "imported Unity asset")
	g.publish(t)
	second := filepath.Join(t.TempDir(), "slot")
	runGitT(t, g.source, "clone", g.repo.CloneURL, second)
	bin := t.TempDir()
	log := filepath.Join(bin, "fetches")
	t.Setenv("WARM_TEST_LFS_LOG", log)
	t.Setenv("WARM_TEST_REAL_LFS", realLFS)
	if err := os.WriteFile(filepath.Join(bin, "git-lfs"), []byte("#!/bin/sh\nif [ \"$1\" = fetch ]; then echo fetch >> \"$WARM_TEST_LFS_LOG\"; fi\nexec \"$WARM_TEST_REAL_LFS\" \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	const project = "unity-game"
	prep := func(path, cache string, pin *domain.WarmWorkspaceRef) error {
		return prepareWarmProjectCheckout(t.Context(), path, cache, ".", project, g.repo, pin)
	}
	if err := prep(g.checkout, g.cache, nil); err != nil {
		t.Fatal(err)
	}
	// Git stays authorized and its refs can advance. Further transfers would
	// fail, making reuse independent of the wrapper's command counter.
	remoteObjects := filepath.Join(g.repo.CloneURL, "lfs", "objects")
	if err := os.Rename(remoteObjects, remoteObjects+".held"); err != nil {
		t.Fatal(err)
	}
	verify := func(path, cache string) domain.WarmWorkspaceRef {
		tree := warmReturnTree{root: filepath.Dir(path), checkout: path, collected: map[string]string{}}
		proof, err := tree.verify(t.Context(), warmWorkspaceSlot{ProjectID: project, UnityProjectPath: "."}, cache, g.repo)
		if err != nil {
			t.Fatal(err)
		}
		return proof
	}
	pin := verify(g.checkout, g.cache)
	if err := prep(g.checkout, g.cache, nil); err != nil {
		t.Fatal(err)
	}
	warmGitWrite(t, g.source, "advanced", "yes")
	g.publish(t)
	nextCache := filepath.Join(filepath.Dir(g.cache), "second")
	if err := prep(second, nextCache, &pin); err != nil {
		t.Fatal(err)
	}
	if runGitT(t, second, "rev-parse", "HEAD") != pin.Commit {
		t.Fatal("lost pinned commit")
	}
	b, err := os.ReadFile(filepath.Join(second, "asset.bin"))
	if err != nil || string(b) != "imported Unity asset" {
		t.Fatal("lost LFS bytes", err)
	}
	verify(second, nextCache)
	count, err := os.ReadFile(log)
	if err != nil || strings.Count(string(count), "fetch") != 1 {
		t.Fatalf("LFS transfers %q: %v", count, err)
	}
	if config := runGitT(t, second, "config", "--local", "--list"); strings.Contains(config, "lfs.storage") {
		t.Fatal("exposed durable LFS store")
	}
	if err := prepareWarmProjectCheckout(t.Context(), second, nextCache, ".", "other-project", g.repo, &pin); err == nil {
		t.Fatal("borrowed another project's LFS bytes")
	}
	cache := warmLFSCache(g.cache, project, g.repo)
	other := g.repo
	other.RepositoryID += "x"
	if warmLFSCache(g.cache, project, other) == cache {
		t.Fatal("repository isolation missing")
	}
	other = g.repo
	other.CloneURL += "x"
	if warmLFSCache(g.cache, project, other) == cache {
		t.Fatal("origin isolation missing")
	}
	oid := fmt.Sprintf("%x", sha256.Sum256(b))
	object := warmLFSObject(filepath.Join(cache, ".git", "lfs"), oid)
	if err := os.WriteFile(object, []byte(strings.Repeat("x", len(b))), 0600); err != nil {
		t.Fatal(err)
	}
	tree := warmReturnTree{root: filepath.Dir(second), checkout: second, collected: map[string]string{}}
	if _, err := tree.verify(t.Context(), warmWorkspaceSlot{ProjectID: project, UnityProjectPath: "."}, nextCache, g.repo); err == nil {
		t.Fatal("trusted a corrupted same-size cached asset")
	}
	kept, err := os.ReadFile(filepath.Join(second, "asset.bin"))
	if err != nil || string(kept) != string(b) {
		t.Fatal("cache damage reached working asset", err)
	}
}
func TestWarmWorkspaceReleaseQualifiedInventory(t *testing.T) {
	f, p, l, g := warmReturnFixture(t)
	d := Daemon{cfg: Config{DefaultWorkspaceRoot: f.workspace}, state: daemonState{RunnerID: l.RunnerID}, warmWorkspacePool: p}
	req := domain.RunnerWorkspaceRequest{RunID: l.OwnerRunID, WarmWorkspaceLease: &l, Operation: domain.RunnerWorkspaceOperationWarmReleaseCheck, PreparationInvocation: &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}}}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\n[ \"$1\" = ps ] || exit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	uncertain := true
	inspections := 0
	inspect := func(_ context.Context, root string) error {
		inspections++
		if root != filepath.Dir(g.checkout) {
			t.Fatal(root)
		}
		if uncertain {
			return errors.New("workspace retained: incomplete process inventory")
		}
		return nil
	}
	res := d.warmReturnWithProcessInventory(t.Context(), req, inspect)
	if res.Code != "warm_owner_retained" || p.state.Leases[l.OwnerRunID].ReturnProven {
		t.Fatal(res)
	}
	uncertain = false
	res = d.warmReturnWithProcessInventory(t.Context(), req, inspect)
	if res.Error != "" || json.Unmarshal(res.Payload, &req.WarmWorkspaceRefs) != nil {
		t.Fatal(res)
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmReleaseCommit
	res = d.warmReturnWithProcessInventory(t.Context(), req, inspect)
	if res.Error != "" || inspections != 3 || p.state.Leases[l.OwnerRunID].Phase != warmWorkspaceReleased {
		t.Fatal(res, inspections)
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

func TestReturnedWarmCheckoutInspectsLiveMetadata(t *testing.T) {
	for _, change := range []string{"stash", "branch", "index", "pending", "clean"} {
		t.Run(change, func(t *testing.T) {
			_, p, l, g := warmReturnFixture(t)
			runGitT(t, g.checkout, "switch", "-c", "saved-work-branch")
			refs, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo})
			if err != nil {
				t.Fatal(err)
			}
			if err = p.commitReturn(t.Context(), l, refs); err != nil {
				t.Fatal(err)
			}
			home := p.slots["a"].Path
			switch change {
			case "stash":
				warmGitWrite(t, home, "README.md", "operator notes")
				runGitT(t, home, "stash")
			case "branch":
				runGitT(t, home, "branch", "extra")
			case "index":
				warmGitWrite(t, home, "README.md", "staged only")
				runGitT(t, home, "add", "README.md")
				runGitT(t, home, "restore", "--source=HEAD", "--worktree", "README.md")
			case "pending":
				warmGitWrite(t, home, ".git/MERGE_HEAD", refs[0].Commit)
			}
			old := warmGitStat(t, filepath.Join(home, ".git"))
			l.Generation++
			if err = p.reserve(l); err != nil {
				t.Fatal(err)
			}
			if err = p.place(t.Context(), l, map[string]string{"a": "checkout"}); err != nil {
				t.Fatal(err)
			}
			err = p.prepareLeaseCheckout(t.Context(), l, &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}})
			if change == "clean" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !os.SameFile(old, warmGitStat(t, filepath.Join(g.checkout, ".git"))) {
				t.Errorf("operator metadata was exchanged following %s", change)
			}
		})
	}
}

func TestWarmCheckoutPreservesDeinitializedSubmodule(t *testing.T) {
	g := newWarmGitFixture(t)
	runGitT(t, g.checkout, "-c", "protocol.file.allow=always", "submodule", "add", g.repo.CloneURL, "nested")
	runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.org", "commit", "-qm", "submodule")
	runGitT(t, g.checkout, "push", "origin", "HEAD")
	runGitT(t, filepath.Join(g.checkout, "nested"), "branch", "local")
	runGitT(t, g.checkout, "submodule", "deinit", "-f", "nested")
	ref := filepath.Join(g.checkout, ".git", "modules", "nested", "refs", "heads", "local")
	old := warmGitStat(t, ref)
	if err := prepareWarmCheckout(t.Context(), g.checkout, g.cache, g.repo, nil); err == nil {
		t.Fatal("exchanged stored submodule metadata")
	}
	if !os.SameFile(old, warmGitStat(t, ref)) {
		t.Fatal("lost nested branch")
	}
}

func TestWarmCheckoutReusesLegacyReturnedBranch(t *testing.T) {
	_, p, l, g := warmReturnFixture(t)
	runGitT(t, g.checkout, "switch", "-c", "returned-work")
	refs, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.commitReturn(t.Context(), l, refs); err != nil {
		t.Fatal(err)
	}
	// Previous journals recorded the exact ref on the released allocation only.
	next := p.state.clone()
	record := next.Slots["a"]
	record.ReturnedRef = nil
	next.Slots["a"] = record
	if err := p.save(next); err != nil {
		t.Fatal(err)
	}
	l.OwnerRunID = "next"
	l.Generation++
	if err := p.reserve(l); err != nil {
		t.Fatal(err)
	}
	if err := p.place(t.Context(), l, map[string]string{"a": "checkout"}); err != nil {
		t.Fatal(err)
	}
	if err := p.prepareLeaseCheckout(t.Context(), l, &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}}); err != nil {
		t.Fatal(err)
	}
}

func TestWarmCheckoutReturnedPinRecoversOnSameLease(t *testing.T) {
	for _, pruneTracking := range []bool{false, true} {
		t.Run(fmt.Sprintf("pruned=%t", pruneTracking), func(t *testing.T) {
			f, p, l, g := warmReturnFixture(t)
			old := runGitT(t, g.checkout, "rev-parse", "HEAD")
			warmGitWrite(t, g.checkout, "new-file", "published")
			runGitT(t, g.checkout, "add", "new-file")
			runGitT(t, g.checkout, "-c", "user.name=test", "-c", "user.email=t@example.org", "commit", "-qm", "new pin")
			runGitT(t, g.checkout, "push", "origin", "HEAD")
			refs, err := p.proveReturn(t.Context(), l, []domain.RunnerRepository{g.repo})
			if err != nil {
				t.Fatal(err)
			}
			if err = p.commitReturn(t.Context(), l, refs); err != nil {
				t.Fatal(err)
			}
			// Substitute a remote with the same frozen URL and only the earlier history.
			runGitT(t, g.source, "push", "--force", "origin", old+":refs/heads/master")
			remote := g.repo.CloneURL
			copy := filepath.Join(t.TempDir(), "replacement.git")
			runGitT(t, g.source, "clone", "--bare", "--no-local", remote, copy)
			if err = os.Rename(remote, remote+".original"); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(copy, remote); err != nil {
				t.Fatal(err)
			}
			if pruneTracking {
				runGitT(t, p.slots["a"].Path, "update-ref", "-d", "refs/remotes/origin/master")
			}
			l.Generation++
			l.Epoch++
			if err = p.reserve(l); err != nil {
				t.Fatal(err)
			}
			if err = p.place(t.Context(), l, map[string]string{"a": "checkout"}); err != nil {
				t.Fatal(err)
			}
			before := warmGitStat(t, filepath.Join(g.checkout, ".git"))
			d := Daemon{cfg: Config{DefaultWorkspaceRoot: f.workspace}, state: daemonState{RunnerID: l.RunnerID}, warmWorkspacePool: p}
			req := domain.RunnerWorkspaceRequest{RunID: l.OwnerRunID, Operation: domain.RunnerWorkspaceOperationWarmCheckout, WarmWorkspaceLease: &l, Sandbox: &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}, PreparationInvocation: &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}, WarmWorkspaceRefs: refs}}
			if res := d.readRunnerWorkspace(t.Context(), req); res.Code != domain.WorkspaceErrorWarmCheckoutRetryable || res.Error == "" {
				t.Fatalf("missing pin should retain retryable lease: %+v", res)
			}
			if !os.SameFile(before, warmGitStat(t, filepath.Join(g.checkout, ".git"))) {
				t.Fatal("metadata changed while pin unavailable")
			}
			runGitT(t, remote+".original", "push", remote, refs[0].Commit+":refs/heads/master")
			if res := d.readRunnerWorkspace(t.Context(), req); res.Error != "" || !strings.Contains(string(res.Payload), "ready") {
				t.Fatalf("restored pin: %+v", res)
			}
			if got := runGitT(t, g.checkout, "rev-parse", "HEAD"); got != refs[0].Commit {
				t.Fatal(got)
			}
		})
	}
}
