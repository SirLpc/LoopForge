package runner

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func windowsWarmGitFixture(t *testing.T) warmGitFixture {
	t.Helper()
	return windowsWarmGitFixtureWithFiles(t, 512)
}

func windowsWarmGitFixtureWithFiles(t *testing.T, count int) warmGitFixture {
	t.Helper()
	root := os.Getenv("LOOPFORGE_TEST_WINDOWS_WARM_ROOT")
	if root == "" {
		t.Skip("set LOOPFORGE_TEST_WINDOWS_WARM_ROOT to a writable Windows drive mount")
	}
	dir, err := os.MkdirTemp(root, "loopforge-warm-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	f := newWarmGitFixture(t)
	if count != 0 {
		for i := range count {
			warmGitWrite(t, f.source, fmt.Sprintf("files/%04d.meta", i), "fixture")
		}
		f.publish(t)
	}
	f.checkout = filepath.Join(dir, "checkout with spaces")
	runGitT(t, f.source, "clone", "-q", f.repo.CloneURL, f.checkout)
	runGitT(t, f.checkout, "config", "core.filemode", "false")
	return f
}

func TestWarmWindowsSmallCheckoutRetainsWSLStatus(t *testing.T) {
	f := windowsWarmGitFixtureWithFiles(t, 0)
	testNativeGitWrapper(t, `for arg do
	if [ "$arg" = status ]; then
		echo "small checkout uses WSL Git" >&2
		exit 42
	fi
done`)
	if err := checkNativeWarmClean(t.Context(), f.checkout, t.TempDir(), f.checkout); err == nil || !strings.Contains(err.Error(), "small checkout uses WSL Git") {
		t.Fatalf("small checkout paid for a Windows Git launch: %v", err)
	}
}

func TestWarmWindowsMountEligibilityPreservesLinuxMetadata(t *testing.T) {
	const mounts = `41 32 0:37 / /mnt/d rw,relatime - 9p D:\134 rw,aname=drvfs;path=D:\134;uid=1000,trans=fd
42 32 0:38 / /mnt/e rw,relatime - 9p E:\134 rw,aname=drvfs;path=E:\134;metadata;uid=1000,trans=fd
43 32 8:32 / /work rw,relatime - ext4 /dev/sdc rw
44 32 0:39 / /remote rw,relatime - 9p p9server rw,trans=virtio`
	for id, want := range map[string]bool{"41": true, "42": false, "43": false, "44": false, "99": false} {
		if got := warmWindowsMountWithoutMetadata(mounts, id); got != want {
			t.Errorf("mount %s acceleration = %v, want %v", id, got, want)
		}
	}
}

func TestWarmWindowsCheckoutUsesNativeStatus(t *testing.T) {
	f := windowsWarmGitFixture(t)
	testNativeGitWrapper(t, `for arg do
	if [ "$arg" = status ]; then
		echo "unexpected Linux status on Windows worktree" >&2
		exit 42
	fi
done`)
	slot := warmWorkspaceSlot{Path: f.checkout, UnityProjectPath: "."}
	prepare := func() error {
		return prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil })
	}
	if err := prepare(); err != nil {
		t.Fatalf("clean checkout on Windows: %v", err)
	}
	warmGitWrite(t, f.checkout, "README.md", "A local edit")
	if err := prepare(); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("dirty checkout on Windows: %v", err)
	}
}

func TestWarmWindowsFullProofUsesNativeInventory(t *testing.T) {
	f := windowsWarmGitFixture(t)
	warmGitWrite(t, f.checkout, ".git/info/exclude", "/Library/\n")
	warmGitWrite(t, f.checkout, "Library/keep", "ignored content")
	commit := runGitT(t, f.checkout, "rev-parse", "HEAD")
	testNativeGitWrapper(t, `for arg do
	if [ "$arg" = --ignored ]; then
		echo "unexpected Linux ignored-directory scan on Windows worktree" >&2
		exit 42
	fi
done`)
	ref := domain.WarmWorkspaceRef{RepositoryID: f.repo.RepositoryID, Branch: f.repo.Ref, Commit: commit}
	slot := warmWorkspaceSlot{Path: f.checkout, UnityProjectPath: "."}
	cache := filepath.Join(t.TempDir(), "cache")
	prove := func() error {
		proof := warmReturnTree{root: f.checkout, checkout: f.checkout, native: true}
		return proof.verifyLocal(t.Context(), slot, cache, f.repo, ref, warmProofLFSContent)
	}
	if err := prove(); err != nil {
		t.Fatalf("clean Windows proof: %v", err)
	}
	warmGitWrite(t, f.checkout, "README.md", "A local edit")
	if err := prove(); err == nil || !strings.Contains(err.Error(), "changes in README.md") {
		t.Fatalf("dirty Windows proof: %v", err)
	}
}

func TestWarmWindowsStatusDoesNotStoreUncommittedLFSBytes(t *testing.T) {
	f := windowsWarmGitFixture(t)
	runGitT(t, f.source, "lfs", "install", "--local")
	runGitT(t, f.source, "lfs", "track", "*.bin")
	warmGitWrite(t, f.source, "data.bin", strings.Repeat("committed asset", 100))
	f.publish(t)
	runGitT(t, f.checkout, "lfs", "install", "--local")
	runGitT(t, f.checkout, "pull", "--ff-only")
	runGitT(t, f.checkout, "lfs", "pull")
	testNativeGitWrapper(t, `for arg do
	if [ "$arg" = status ]; then
		echo "unexpected Linux status on Windows worktree" >&2
		exit 42
	fi
done`)
	check := func() error {
		return checkNativeWarmClean(t.Context(), f.checkout, filepath.Dir(f.cache), f.checkout)
	}
	if err := os.MkdirAll(filepath.Dir(f.cache), 0700); err != nil {
		t.Fatal(err)
	}
	if err := check(); err != nil {
		t.Fatalf("clean materialized LFS asset: %v", err)
	}
	dirty := strings.Repeat("uncommitted asset", 100)
	warmGitWrite(t, f.checkout, "data.bin", dirty)
	if err := check(); err == nil || !strings.Contains(err.Error(), "not clean") {
		t.Fatalf("dirty materialized LFS asset: %v", err)
	}
	oid := fmt.Sprintf("%x", sha256.Sum256([]byte(dirty)))
	object := warmLFSObject(filepath.Join(f.checkout, ".git", "lfs"), oid)
	if _, err := os.Lstat(object); !os.IsNotExist(err) {
		t.Fatalf("status populated checkout LFS storage with an uncommitted asset: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Dir(f.cache)); err != nil || len(entries) != 0 {
		t.Fatalf("status left temporary Git or LFS storage: %v, %v", entries, err)
	}
}

func TestWarmWindowsProofHashesGitBlobsAndMaterializedLFS(t *testing.T) {
	f := windowsWarmGitFixture(t)
	tree, ok := openWarmWindowsTree(t.Context(), f.checkout, f.checkout)
	if !ok {
		t.Fatal("fixture is not on a Windows drive")
	}
	const asset = "asset 光.bin"
	lfs := strings.Repeat("LFS content", 170)
	warmGitWrite(t, f.checkout, asset, lfs)
	tracked := map[string][]string{
		"README.md": {"100644", "blob", runGitT(t, f.checkout, "rev-parse", "HEAD:README.md")},
		asset:       {"100644", "blob", strings.Repeat("0", 40)},
	}
	manifest := warmLFSManifest{
		contentOnly: true,
		byPath: map[string]warmLFSFile{
			asset: {OID: fmt.Sprintf("%x", sha256.Sum256([]byte(lfs))), Size: int64(len(lfs))},
		},
	}
	verified := tree.verifiedFiles(t.Context(), tracked, manifest)
	if !verified["README.md"] || !verified[asset] {
		t.Fatalf("Windows did not prove both committed byte streams: %v", verified)
	}
	warmGitWrite(t, f.checkout, asset, strings.Repeat("LFS change!", 170))
	verified = tree.verifiedFiles(t.Context(), tracked, manifest)
	if !verified["README.md"] || verified[asset] {
		t.Fatalf("Windows accepted modified LFS bytes: %v", verified)
	}
}

func TestWarmWindowsLeaseUsesHostChecksThroughReturnAndReopen(t *testing.T) {
	if os.Getenv("LOOPFORGE_TEST_NATIVE_MOUNTS") != "1" {
		t.Skip("run inside a private user and mount namespace")
	}
	g := windowsWarmGitFixture(t)
	f := newWarmPoolFixture(t)
	f.writeSlots(t, g.checkout)
	p, err := openWarmWorkspacePoolWithinMode(f.config, f.workspace, f.journal, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.close() })
	g.repo.RepositoryID, g.repo.Dir = "a", "checkout"
	warmGitWrite(t, g.checkout, "Library/keep", "persist across leases")
	testNativeGitWrapper(t, `for arg do
	case "$arg" in
	    status|--ignored)
	        echo "unexpected Linux worktree scan during Windows lease" >&2
	        exit 42
	        ;;
	esac
done`)

	lease := placeNativeLeaseTest(t, p, "windows-first", 1)
	if err := prepareNativeLeaseTest(t, p, g, lease, nil); err != nil {
		t.Fatalf("prepare Windows lease: %v", err)
	}
	refs := returnNativeLeaseTest(t, p, g, lease)
	reopened := placeNativeLeaseTest(t, p, "windows-reopened", 2)
	if err := prepareNativeLeaseTest(t, p, g, reopened, refs); err != nil {
		t.Fatalf("reopen returned Windows lease: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(g.checkout, "Library", "keep")); err != nil || string(data) != "persist across leases" {
		t.Fatalf("ignored workspace file = %q, %v", data, err)
	}
}
