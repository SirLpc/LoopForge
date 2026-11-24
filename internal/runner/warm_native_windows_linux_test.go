package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

func TestWarmWindowsCheckoutSwitchesCommitWithoutLinuxCheckout(t *testing.T) {
	f := windowsWarmGitFixture(t)
	runGitT(t, f.source, "lfs", "install", "--local")
	runGitT(t, f.source, "lfs", "track", "*.bin")
	runGitT(t, f.checkout, "config", "core.eol", "native")
	warmGitWrite(t, f.source, ".gitattributes", "*.bin filter=lfs diff=lfs merge=lfs -text\n*.txt text\n")
	warmGitWrite(t, f.source, "new text.txt", "one\ntwo\n")
	asset := strings.Repeat("new real asset", 100)
	warmGitWrite(t, f.source, "asset 光.bin", asset)
	warmGitWrite(t, f.source, "README.md", "updated on the remote")
	want := f.publish(t)
	testNativeGitWrapper(t, `for arg do
	if [ "$arg" = checkout ] || [ "$arg" = symbolic-ref ]; then
		echo "Linux Git attempted to change the Windows checkout" >&2
		exit 42
	fi
done`)
	slot := warmWorkspaceSlot{Path: f.checkout, UnityProjectPath: "."}
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, f.checkout, "rev-parse", "HEAD"); got != want {
		t.Fatalf("checked out %s, want %s", got, want)
	}
	if b, err := os.ReadFile(filepath.Join(f.checkout, "README.md")); err != nil || string(b) != "updated on the remote" {
		t.Fatalf("unexpected worktree after checkout: %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(f.checkout, "asset 光.bin")); err != nil || string(b) != asset {
		t.Fatalf("Windows checkout did not materialize the changed LFS asset: %v", err)
	}
	indexStat := runGitT(t, f.checkout, "ls-files", "--debug", "--", "asset 光.bin")
	if !strings.Contains(indexStat, fmt.Sprintf("size: %d\t", len(asset))) {
		t.Fatalf("index did not record the hydrated asset's size: %s", indexStat)
	}
	if b, err := os.ReadFile(filepath.Join(f.checkout, "new text.txt")); err != nil || string(b) != "one\ntwo\n" {
		t.Fatalf("Windows checkout changed the existing text EOL policy: %q, %v", b, err)
	}
}

func TestWarmWindowsCheckoutCanSwitchToFewerTrackedFiles(t *testing.T) {
	f := windowsWarmGitFixture(t)
	if err := os.RemoveAll(filepath.Join(f.source, "files")); err != nil {
		t.Fatal(err)
	}
	want := f.publish(t)
	slot := warmWorkspaceSlot{Path: f.checkout, UnityProjectPath: "."}
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatalf("Windows checkout of smaller target: %v", err)
	}
	if got := runGitT(t, f.checkout, "rev-parse", "HEAD"); got != want {
		t.Fatalf("checked out %s, want %s", got, want)
	}
}

func TestWarmWindowsRecoveryRefreshesAlreadyHydratedIndex(t *testing.T) {
	f := windowsWarmGitFixture(t)
	runGitT(t, f.source, "lfs", "install", "--local")
	runGitT(t, f.source, "lfs", "track", "*.bin")
	runGitT(t, f.checkout, "lfs", "install", "--local")
	asset := strings.Repeat("materialized data", 100)
	warmGitWrite(t, f.source, "asset.bin", asset)
	target := f.publish(t)
	slot := warmWorkspaceSlot{Path: f.checkout, UnityProjectPath: "."}
	interrupted := errors.New("simulate interruption after materialization")
	var intent nativeWarmCheckoutIntent
	_, err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, slot, f.repo, nil, nil, func(pending nativeWarmCheckoutIntent) error {
		intent = pending
		return interrupted
	})
	if !errors.Is(err, interrupted) {
		t.Fatal(err)
	}
	t.Setenv("GIT_LFS_SKIP_SMUDGE", "1")
	runGitT(t, f.checkout, "checkout", "--detach", target)
	runGitT(t, f.checkout, "update-ref", "refs/heads/"+intent.Target.Branch, target)
	runGitT(t, f.checkout, "symbolic-ref", "HEAD", "refs/heads/"+intent.Target.Branch)
	warmGitWrite(t, f.checkout, "asset.bin", asset)

	if _, err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, slot, f.repo, nil, &intent, nil); err != nil {
		t.Fatalf("resume after LFS is already materialized: %v", err)
	}
	indexStat := runGitT(t, f.checkout, "ls-files", "--debug", "--", "asset.bin")
	if !strings.Contains(indexStat, fmt.Sprintf("size: %d\t", len(asset))) {
		t.Fatalf("resumed Windows checkout did not refresh the asset's cached size: %s", indexStat)
	}
}

func TestWarmWindowsManagedGitStopsOnCancellation(t *testing.T) {
	f := windowsWarmGitFixtureWithFiles(t, 0)
	windows, ok := openWarmWindowsTree(t.Context(), f.checkout, f.checkout)
	if !ok {
		t.Fatal("fixture is not on a Windows drive")
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	dir := strings.ReplaceAll(windows.root, `\`, "/")
	go func() {
		_, err := windows.checkoutGit(ctx, false, "-c", "alias.test-wait=!cd '"+dir+"' && for i in $(seq 1 300); do echo alive >> running; sleep 0.1; done", "test-wait")
		done <- err
	}()
	marker := filepath.Join(f.checkout, "running")
	deadline := time.After(8 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("Windows Git exited before the test command started: %v", err)
		case <-deadline:
			t.Fatal("Windows Git did not start the test command")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("expected cancellation, got %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("Windows Git was still running after cancellation")
	}
	before := warmGitStat(t, marker).Size()
	time.Sleep(350 * time.Millisecond)
	if got := warmGitStat(t, marker).Size(); got != before {
		t.Fatalf("Windows Git descendant is still writing after cancellation: %d → %d", before, got)
	}
}

func TestWarmWindowsGitRejectsReplacedCheckoutPath(t *testing.T) {
	f := windowsWarmGitFixtureWithFiles(t, 0)
	windows, ok := openWarmWindowsTree(t.Context(), f.checkout, f.checkout)
	if !ok {
		t.Fatal("fixture is not on a Windows drive")
	}
	moved := f.checkout + "-moved"
	if err := os.Rename(f.checkout, moved); err != nil {
		t.Fatal(err)
	}
	runGitT(t, f.source, "clone", "-q", f.repo.CloneURL, f.checkout)
	if _, err := windows.changeCheckout(t.Context(), "symbolic-ref", "HEAD", "refs/heads/unexpected"); err == nil {
		t.Fatal("Windows Git accepted a replacement at the registered path")
	}
	for _, dir := range []string{f.checkout, moved} {
		if head := runGitT(t, dir, "symbolic-ref", "HEAD"); head == "refs/heads/unexpected" {
			t.Fatalf("Windows Git modified a checkout after its path changed")
		}
	}
}

func TestWarmWindowsIndexPublicationKeepsConcurrentChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index")
	warmGitWrite(t, filepath.Dir(path), "index", "concurrent staged entries")
	if err := publishWarmWindowsIndex(path, []byte("before"), []byte("refreshed")); err == nil {
		t.Fatal("replaced a concurrently updated index")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "concurrent staged entries" {
		t.Fatalf("concurrent index = %q, %v", got, err)
	}
	warmGitWrite(t, filepath.Dir(path), "index.lock", "another Git operation")
	if err := publishWarmWindowsIndex(path, []byte("concurrent staged entries"), []byte("refreshed")); err == nil {
		t.Fatal("published through another Git operation's index lock")
	}
	if got, err := os.ReadFile(path + ".lock"); err != nil || string(got) != "another Git operation" {
		t.Fatalf("concurrent index lock = %q, %v", got, err)
	}
}

func TestWarmWindowsFullProofUsesNativeInventory(t *testing.T) {
	f := windowsWarmGitFixture(t)
	warmGitWrite(t, f.checkout, ".git/info/exclude", "/Library/\n/ignored 光.txt\n")
	warmGitWrite(t, f.checkout, "Library/keep", "ignored content")
	warmGitWrite(t, f.checkout, "ignored 光.txt", "ignored file")
	commit := runGitT(t, f.checkout, "rev-parse", "HEAD")
	testNativeGitWrapper(t, `for arg do
	if [ "$arg" = --ignored ] || [ "$arg" = check-ignore ]; then
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
