package runner

import (
	"agentflow/internal/domain"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type warmGitFixture struct {
	checkout, source, cache string
	repo                    domain.RunnerRepository
}

func warmGitWrite(t *testing.T, dir, name, data string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}
func newWarmGitFixture(t *testing.T) warmGitFixture {
	t.Helper()
	remote := bareRemoteWithBaseline(t)
	root := t.TempDir()
	f := warmGitFixture{checkout: filepath.Join(root, "slot"), source: filepath.Join(root, "author"), cache: filepath.Join(root, "private", "cache"), repo: domain.RunnerRepository{RepositoryID: "repo", CloneURL: remote, Ref: "master"}}
	runGitT(t, root, "clone", "-q", remote, f.source)
	runGitT(t, f.source, "config", "user.name", "test")
	runGitT(t, f.source, "config", "user.email", "t@example.org")
	warmGitWrite(t, f.source, ".gitignore", "/Library/\n")
	f.publish(t)
	runGitT(t, root, "clone", "-q", remote, f.checkout)
	warmGitWrite(t, f.checkout, "Library/sentinel", "imported")
	return f
}
func (f warmGitFixture) publish(t *testing.T) string {
	t.Helper()
	runGitT(t, f.source, "add", "-A")
	runGitT(t, f.source, "commit", "-qm", "baseline")
	runGitT(t, f.source, "push", "-q", "origin", "HEAD")
	return runGitT(t, f.source, "rev-parse", "HEAD")
}
func (f warmGitFixture) prepare(t *testing.T, restore *domain.WarmWorkspaceRef) {
	t.Helper()
	if err := prepareWarmCheckout(context.Background(), f.checkout, f.cache, f.repo, restore); err != nil {
		t.Fatal(err)
	}
}
func warmGitStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	v, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestWarmCheckoutPreservesLibraryAndUsesLatestRef(t *testing.T) {
	f := newWarmGitFixture(t)
	stable := filepath.Join(f.checkout, "README.md")
	library := filepath.Join(f.checkout, "Library/sentinel")
	a, b := warmGitStat(t, stable), warmGitStat(t, library)
	warmGitWrite(t, f.source, "added", "new")
	want := f.publish(t)
	f.prepare(t, nil)
	if got := runGitT(t, f.checkout, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD = %s, want %s", got, want)
	}
	if !os.SameFile(a, warmGitStat(t, stable)) || !os.SameFile(b, warmGitStat(t, library)) {
		t.Fatal("unchanged worktree file replaced")
	}
}
func TestWarmCheckoutGitIsolation(t *testing.T) {
	f := newWarmGitFixture(t)
	f.prepare(t, nil)
	object := runGitT(t, f.checkout, "hash-object", "-w", "Library/sentinel")
	runGitT(t, f.checkout, "config", "previous.private", "previous")
	warmGitWrite(t, f.checkout, ".git/hooks/previous", "old")
	f.prepare(t, nil)
	for _, args := range [][]string{{"cat-file", "-e", object}, {"config", "--get", "previous.private"}} {
		if err := exec.Command("git", append([]string{"-C", f.checkout}, args...)...).Run(); err == nil {
			t.Fatalf("prior lease visible via %v", args)
		}
	}
	if _, err := os.Stat(filepath.Join(f.checkout, ".git/hooks/previous")); !os.IsNotExist(err) {
		t.Fatal("prior hook visible")
	}
	for _, bad := range []string{"origin", "tracked", "symlink", "dirty", "wrong repository"} {
		t.Run(bad, func(t *testing.T) {
			f := newWarmGitFixture(t)
			switch bad {
			case "origin":
				runGitT(t, f.checkout, "remote", "set-url", "origin", f.source)
			case "tracked":
				warmGitWrite(t, f.source, "Library/tracked", "conflict")
				runGitT(t, f.source, "add", "-f", "Library/tracked")
				f.publish(t)
			case "symlink":
				if err := os.Symlink(t.TempDir(), filepath.Dir(f.cache)); err != nil {
					t.Fatal(err)
				}
			case "dirty":
				warmGitWrite(t, f.checkout, "README.md", "pending edits")
			case "wrong repository":
				f.prepare(t, nil)
				f.repo.RepositoryID = "different"
			}
			if err := prepareWarmCheckout(context.Background(), f.checkout, f.cache, f.repo, nil); err == nil {
				t.Fatal("unsafe checkout accepted")
			}
		})
	}
}
func TestWarmCheckoutBranchModes(t *testing.T) {
	f := newWarmGitFixture(t)
	old := runGitT(t, f.checkout, "rev-parse", "HEAD")
	f.repo.WorkBranch = "feature"
	f.repo.CreateNewWorkBranch = true
	f.prepare(t, nil)
	if runGitT(t, f.checkout, "branch", "--show-current") != "feature" {
		t.Fatal("new branch missing")
	}
	runGitT(t, f.source, "push", "origin", "HEAD:refs/heads/feature")
	if err := prepareWarmCheckout(context.Background(), f.checkout, f.cache, f.repo, nil); err == nil {
		t.Fatal("new branch collision accepted")
	}
	f.repo.CreateNewWorkBranch = false
	f.repo.RequireExistingWorkBranch = true
	f.prepare(t, nil)
	if runGitT(t, f.checkout, "branch", "--show-current") != "feature" {
		t.Fatal("continued branch missing")
	}
	f.repo.WorkBranch = ""
	f.repo.RequireExistingWorkBranch = false
	warmGitWrite(t, f.source, "later", "later")
	f.publish(t)
	f.prepare(t, &domain.WarmWorkspaceRef{RepositoryID: "repo", Commit: old})
	if runGitT(t, f.checkout, "branch", "--show-current") != "" || runGitT(t, f.checkout, "rev-parse", "HEAD") != old {
		t.Fatal("detached restore mismatch")
	}
}
func TestWarmCheckoutLFS(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	f := newWarmGitFixture(t)
	runGitT(t, f.source, "lfs", "install", "--local")
	runGitT(t, f.source, "lfs", "track", "*.bin")
	warmGitWrite(t, f.source, "first.bin", "large asset 1")
	f.publish(t)
	f.prepare(t, nil)
	asset := filepath.Join(f.checkout, "first.bin")
	first := warmGitStat(t, asset)
	warmGitWrite(t, f.source, "second.bin", "large asset 2")
	f.publish(t)
	f.prepare(t, nil)
	for _, name := range []string{"first.bin", "second.bin"} {
		b, e := os.ReadFile(filepath.Join(f.checkout, name))
		if e != nil || string(b) != map[string]string{"first.bin": "large asset 1", "second.bin": "large asset 2"}[name] {
			t.Fatalf("%s not materialized: %q %v", name, b, e)
		}
	}
	if !os.SameFile(first, warmGitStat(t, asset)) {
		t.Fatal("unchanged LFS asset replaced")
	}
}

func TestWarmCheckoutProtectsStagedSeedAndLibrary(t *testing.T) {
	for _, mode := range []string{"staged only", "new ignore rules"} {
		t.Run(mode, func(t *testing.T) {
			f := newWarmGitFixture(t)
			if mode == "staged only" {
				warmGitWrite(t, f.checkout, "README.md", "only in index")
				runGitT(t, f.checkout, "add", "README.md")
				runGitT(t, f.checkout, "restore", "--source=HEAD", "--worktree", "README.md")
			} else {
				warmGitWrite(t, f.source, ".gitignore", "# no longer ignored\n")
				f.publish(t)
			}
			if err := prepareWarmCheckout(t.Context(), f.checkout, f.cache, f.repo, nil); err == nil {
				t.Fatal("prepared unsafe seed")
			}
		})
	}
}
func TestWarmCheckoutMaterializesExistingLFSPointer(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	f := newWarmGitFixture(t)
	runGitT(t, f.source, "lfs", "install", "--local")
	runGitT(t, f.source, "lfs", "track", "*.bin")
	warmGitWrite(t, f.source, "asset.bin", "actual asset")
	f.publish(t)
	t.Setenv("GIT_LFS_SKIP_SMUDGE", "1")
	runGitT(t, f.checkout, "pull", "--ff-only")
	t.Setenv("GIT_LFS_SKIP_SMUDGE", "")
	f.prepare(t, nil)
	b, err := os.ReadFile(filepath.Join(f.checkout, "asset.bin"))
	if err != nil || string(b) != "actual asset" {
		t.Fatalf("unmaterialized existing pointer: %q, %v", b, err)
	}
}

func TestWarmCheckoutUsesRemoteDefaultBranch(t *testing.T) {
	f := newWarmGitFixture(t)
	f.repo.Ref = ""
	f.prepare(t, nil)
	if branch := runGitT(t, f.checkout, "branch", "--show-current"); branch != "master" {
		t.Fatalf("default branch = %q", branch)
	}
}
func TestWarmCheckoutProtectsPopulatedGitlinks(t *testing.T) {
	f := newWarmGitFixture(t)
	head := runGitT(t, f.checkout, "rev-parse", "HEAD")
	runGitT(t, f.source, "update-index", "--add", "--cacheinfo", "160000,"+head+",nested")
	runGitT(t, f.source, "commit", "-qm", "track nested repo")
	runGitT(t, f.source, "push", "origin", "HEAD")
	runGitT(t, f.checkout, "pull", "--ff-only")
	warmGitWrite(t, f.checkout, "nested/private", "owner")
	if err := prepareWarmCheckout(t.Context(), f.checkout, f.cache, f.repo, nil); err == nil {
		t.Fatal("populated nested checkout retained in new lease")
	}
}

func TestWarmCheckoutSHA256(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "source")
	remote := filepath.Join(root, "remote.git")
	runGitT(t, root, "init", "-b", "main", "--object-format=sha256", src)
	warmGitWrite(t, src, "asset", "asset")
	runGitT(t, src, "add", "asset")
	runGitT(t, src, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-qm", "seed")
	runGitT(t, root, "clone", "--bare", src, remote)
	checkout := filepath.Join(root, "slot")
	runGitT(t, root, "clone", remote, checkout)
	head := runGitT(t, src, "rev-parse", "HEAD")
	repo := domain.RunnerRepository{RepositoryID: "r", CloneURL: remote, Ref: "main"}
	if err := prepareWarmCheckout(t.Context(), checkout, filepath.Join(root, "cache"), repo, &domain.WarmWorkspaceRef{RepositoryID: "r", Commit: head}); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, checkout, "rev-parse", "HEAD"); got != head {
		t.Fatal(got)
	}
}
func TestWarmCheckoutLFSDelivery(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	f := newWarmGitFixture(t)
	runGitT(t, f.source, "lfs", "install", "--local")
	runGitT(t, f.source, "lfs", "track", "*.bin")
	warmGitWrite(t, f.source, "asset.bin", "before")
	f.publish(t)
	f.prepare(t, nil)
	warmGitWrite(t, f.checkout, "asset.bin", "after")
	runGitT(t, f.checkout, "add", "asset.bin")
	runGitT(t, f.checkout, "-c", "user.name=t", "-c", "user.email=t@example.org", "commit", "-qm", "asset change")
	runGitT(t, f.checkout, "push", "origin", "HEAD")
	runGitT(t, f.source, "pull", "--ff-only")
	data, err := os.ReadFile(filepath.Join(f.source, "asset.bin"))
	if err != nil || string(data) != "after" {
		t.Fatalf("published LFS asset=%q err=%v", data, err)
	}
}
