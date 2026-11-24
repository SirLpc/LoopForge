package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
	"golang.org/x/sys/unix"
)

func TestWarmNativeRunnerAcceptsClaimResolvedDriver(t *testing.T) {
	d := &Daemon{cfg: Config{WarmNative: true}}
	for _, test := range []struct {
		driver string
		match  bool
	}{
		{domain.SandboxDriverNative, true},
		{domain.SandboxDriverBwrap, true},
		{domain.SandboxDriverDocker, false},
		{domain.SandboxDriverSeatbelt, false},
		{domain.SandboxDriverLocal, false},
	} {
		t.Run(test.driver, func(t *testing.T) {
			spec := &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: test.driver}}
			if got := d.matchesWarmSandbox(spec); got != test.match {
				t.Fatalf("matchesWarmSandbox = %t, want %t", got, test.match)
			}
		})
	}
}

func TestWarmNativePoolKeepsOriginalCheckout(t *testing.T) {
	if os.Getenv("LOOPFORGE_TEST_NATIVE_MOUNTS") != "1" {
		t.Skip("run inside a private user and mount namespace")
	}
	f := newWarmPoolFixture(t)
	if parent := os.Getenv("LOOPFORGE_TEST_NATIVE_SLOT_PARENT"); parent != "" {
		dir, err := os.MkdirTemp(parent, ".loopforge-test-slot-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		f.homeA = dir
		f.writeSlots(t, f.homeA)
	}
	before := warmPoolStat(t, f.homeA)
	p, err := openWarmWorkspacePoolWithinMode(f.config, f.workspace, f.journal, true)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	alternate, err := openWarmWorkspacePoolWithinMode(f.config, filepath.Join(f.root, "second-work"), filepath.Join(f.root, "second-state", "journal"), true)
	if err == nil {
		alternate.close()
		t.Fatal("a different Runner journal acquired the same source slot")
	}
	l := domain.WarmWorkspaceLease{OwnerRunID: "run-one", RunnerID: "runner", Generation: 1, Slots: map[string]string{"a": "a"}}
	warmGitWrite(t, f.homeA, "Temp/UnityLockfile", "Editor")
	if err := p.reserve(l); err == nil {
		t.Fatal("leased a checkout while Unity held its lock")
	}
	if err := os.RemoveAll(filepath.Join(f.homeA, "Temp")); err != nil {
		t.Fatal(err)
	}
	if err := p.reserve(l); err != nil {
		t.Fatal(err)
	}
	if err := p.place(t.Context(), l, map[string]string{"a": "pet"}); err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(f.workspace, "runs", "run-one", "workspace", "pet")
	defer func() { _ = unmountNativeWarmDir(checkout) }()
	warmPoolRequireSame(t, before, f.homeA)
	warmPoolRequireSame(t, before, checkout)
	state := filepath.Join(filepath.Dir(f.journal), "session.json")
	warmGitWrite(t, filepath.Dir(f.journal), "session.json", "private runner state")
	warmGitWrite(t, f.workspace, "sibling/private.txt", "another run")
	warmGitWrite(t, f.homeA, "source-only", "same inode")
	t.Setenv("AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN", "private-bootstrap")
	d := &Daemon{cfg: Config{WarmNative: true, StatePath: state}, warmWorkspacePool: p, state: daemonState{RunnerID: l.RunnerID}}
	if err := d.nativeWarmReady(t.Context()); err != nil {
		t.Fatal(err)
	}
	isolation := d.nativeWarmIsolation()
	sb, err := runtime.OpenRemoteSandbox(t.Context(), "native-slot-test", filepath.Dir(checkout),
		domain.SandboxProfile{Driver: domain.SandboxDriverNative}, runtime.SandboxRequirements{NativeWarm: isolation}, runtime.SandboxResources{})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close(t.Context())
	out, err := sb.Exec(t.Context(), runtime.CommandSpec{Argv: []string{"/bin/sh", "-ec", `
		test -z "${AGENTFLOW_RUNNER_BOOTSTRAP_TOKEN-}"
		test ! -s "$1"
		test ! -e "$2/source-only"
		test ! -e "$3/sibling/private.txt"
		test "$(cat pet/source-only)" = "same inode"
		printf edited > pet/marker
	`, "test", state, f.homeA, f.workspace}}, nil)
	if err != nil {
		t.Fatalf("native sandbox write: %v: %s", err, out)
	}
	p.mu.Lock()
	allocation := p.state.Leases[l.OwnerRunID]
	allocation.GitReady = true
	err = p.saveAllocation(allocation)
	p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	warmGitWrite(t, f.homeA, "Temp/UnityLockfile", "Editor")
	if err := d.checkWarmOperation(t.Context(), l.OwnerRunID, domain.RunnerWorkspaceOperationWrite, &l); err == nil {
		t.Fatal("allowed a ready lease to write while Unity was open")
	}
	if err := d.checkWarmOperation(t.Context(), l.OwnerRunID, domain.RunnerWorkspaceOperationFile, &l); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(f.homeA, "Temp")); err != nil {
		t.Fatal(err)
	}
	if err := unmountNativeWarmDir(checkout); err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	p, err = openWarmWorkspacePoolWithinMode(f.config, f.workspace, f.journal, true)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	warmPoolRequireSame(t, before, checkout)
	if err := p.returnSlots(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if contents, err := os.ReadFile(filepath.Join(f.homeA, "marker")); err != nil || string(contents) != "edited" {
		t.Fatalf("fixed checkout lost edits: %s %v", contents, err)
	}
}

func TestWarmNativeCheckoutKeepsGitAndLibrary(t *testing.T) {
	f := newWarmGitFixture(t)
	f = nativeWarmTestClone(t, f)
	runGitT(t, f.checkout, "branch", "personal")
	meta := warmGitStat(t, filepath.Join(f.checkout, ".git"))
	library := warmGitStat(t, filepath.Join(f.checkout, "Library/sentinel"))
	warmGitWrite(t, f.source, "new-code", "updated")
	want := f.publish(t)
	repo := f.repo
	repo.WorkBranch, repo.CreateNewWorkBranch = "task", true
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, f.checkout, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD = %s, want %s", got, want)
	}
	warmPoolRequireSame(t, meta, filepath.Join(f.checkout, ".git"))
	warmPoolRequireSame(t, library, filepath.Join(f.checkout, "Library/sentinel"))
	runGitT(t, f.checkout, "rev-parse", "personal")
	warmGitWrite(t, f.checkout, "new-code", "local edit")
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err == nil {
		t.Fatal("accepted a modified source file")
	}
}

func TestWarmNativeCheckoutDistinguishesGitStatusFromWarnings(t *testing.T) {
	f := nativeWarmTestClone(t, newWarmGitFixture(t))
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := `#!/bin/sh
for arg do
	if [ "$arg" = "status" ]; then
		printf '%s\n' 'warning: untracked cache is disabled on this system or location' >&2
		if [ "$FAIL_TEST_GIT_STATUS" = 1 ]; then
			printf '%s\n' 'fatal: cannot read index' >&2
			exit 128
		fi
		break
	fi
done
exec ` + strconv.Quote(git) + ` "$@"
`
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prepare := func() error {
		return prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil })
	}
	if err := prepare(); err != nil {
		t.Fatalf("clean checkout with a Git warning: %v", err)
	}
	t.Setenv("FAIL_TEST_GIT_STATUS", "1")
	if err := prepare(); err == nil || !strings.Contains(err.Error(), "fatal: cannot read index") {
		t.Fatalf("failed Git status should preserve its diagnostic: %v", err)
	}
	t.Setenv("FAIL_TEST_GIT_STATUS", "0")
	warmGitWrite(t, f.checkout, "local-edit", "keep me")
	if err := prepare(); err == nil || !strings.Contains(err.Error(), "?? local-edit") {
		t.Fatalf("dirty checkout with a Git warning should still be rejected: %v", err)
	}
}

func nativeWarmTestClone(t *testing.T, f warmGitFixture) warmGitFixture {
	t.Helper()
	if parent := os.Getenv("LOOPFORGE_TEST_NATIVE_SLOT_PARENT"); parent != "" {
		dir, err := os.MkdirTemp(parent, ".loopforge-test-git-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		runGitT(t, dir, "clone", "-q", f.repo.CloneURL, dir)
		runGitT(t, dir, "config", "core.filemode", "false")
		warmGitWrite(t, dir, "Library/sentinel", "imported")
		f.checkout = dir
	}
	return f
}

func TestWarmNativeReturnAcceptsUnchangedDrvFSModeAndPreservesPrivateRefs(t *testing.T) {
	f := newWarmGitFixture(t)
	runGitT(t, f.checkout, "branch", "personal")
	runGitT(t, f.checkout, "config", "core.fileMode", "false")
	if err := os.Chmod(filepath.Join(f.checkout, "README.md"), 0755); err != nil {
		t.Fatal(err)
	}
	slot := warmWorkspaceSlot{UnityProjectPath: "."}
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	proof := warmReturnTree{root: filepath.Dir(f.checkout), checkout: f.checkout, native: true}
	if _, err := proof.verify(t.Context(), slot, f.cache, f.repo); err != nil {
		t.Fatal(err)
	}
	warmGitWrite(t, f.checkout, "README.md", "modified")
	if _, err := proof.verify(t.Context(), slot, f.cache, f.repo); err == nil {
		t.Fatal("returned a fixed checkout with uncommitted edits")
	}
}

func TestWarmNativeLFSUsesOriginalCheckout(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	f := nativeWarmTestClone(t, newWarmGitFixture(t))
	runGitT(t, f.source, "lfs", "install", "--local")
	runGitT(t, f.source, "lfs", "track", "*.bin")
	warmGitWrite(t, f.source, "asset.bin", "original asset")
	f.publish(t)
	slot := warmWorkspaceSlot{UnityProjectPath: "."}
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(f.checkout, "asset.bin")); err != nil || string(data) != "original asset" {
		t.Fatalf("asset not materialized: %q %v", data, err)
	}
	warmGitWrite(t, f.checkout, "asset.bin", "new asset")
	runGitT(t, f.checkout, "add", "asset.bin")
	if pointer := runGitT(t, f.checkout, "show", ":asset.bin"); !strings.HasPrefix(pointer, "version https://git-lfs.github.com/spec/v1\n") {
		t.Fatalf("Git did not stage the asset as an LFS pointer: %q", pointer)
	}
}

func TestWarmNativeGitHelperReadsWithoutDocker(t *testing.T) {
	if os.Getenv("LOOPFORGE_TEST_NATIVE_MOUNTS") != "1" {
		t.Skip("run inside a private user and mount namespace")
	}
	f := newWarmGitFixture(t)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(warmGitCommand{Read: &domain.RunnerWorkspaceRequest{Operation: domain.RunnerWorkspaceOperationInfo}})
	if err != nil {
		t.Fatal(err)
	}
	output, err := runtime.RunNativeWarmGit(t.Context(), filepath.Dir(f.checkout), executable, input, false, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	var reply domain.RunnerWorkspaceResponse
	if err := json.Unmarshal(output, &reply); err != nil || reply.Error != "" || !strings.Contains(string(reply.Payload), "\"available\":true") {
		t.Fatalf("workspace Git read failed: %s, %v", output, err)
	}
}

func TestWarmNativeCheckoutKeepsIgnoredPersonalFile(t *testing.T) {
	f := newWarmGitFixture(t)
	warmGitWrite(t, f.source, ".gitignore", "/Library/\n/personal.txt\n")
	f.publish(t)
	runGitT(t, f.checkout, "pull", "--ff-only")
	warmGitWrite(t, f.checkout, "personal.txt", "keep me")
	warmGitWrite(t, f.source, "personal.txt", "tracked on target")
	runGitT(t, f.source, "add", "-f", "personal.txt")
	f.publish(t)
	wrote := false
	err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error {
		wrote = true
		return nil
	})
	if err == nil || wrote {
		t.Fatalf("ignored file collision was not detected before checkout: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(f.checkout, "personal.txt")); err != nil || string(data) != "keep me" {
		t.Fatalf("ignored local data changed: %q %v", data, err)
	}
}

func TestWarmNativeCheckoutAndReturnWithNestedIgnore(t *testing.T) {
	f := nativeWarmTestClone(t, newWarmGitFixture(t))
	slot := warmWorkspaceSlot{UnityProjectPath: "."}
	warmGitWrite(t, f.checkout, ".superpowers/sdd/.gitignore", "*\n")
	warmGitWrite(t, f.checkout, ".superpowers/sdd/progress.md", "draft")
	warmGitWrite(t, f.source, ".superpowers/README.md", "tracked sibling")
	commit := f.publish(t)
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	proof := warmReturnTree{root: filepath.Dir(f.checkout), checkout: f.checkout, native: true}
	ref, err := proof.verify(t.Context(), slot, f.cache, f.repo)
	if err != nil || ref.Commit != commit {
		t.Fatalf("return proof = %+v, err = %v", ref, err)
	}
	if content, err := os.ReadFile(filepath.Join(f.checkout, ".superpowers/sdd/progress.md")); err != nil || string(content) != "draft" {
		t.Fatalf("ignored file = %q, err = %v", content, err)
	}
}

func TestWarmNativeReturnReadsWindowsCheckoutTextAndSymlinks(t *testing.T) {
	f := newWarmGitFixture(t)
	warmGitWrite(t, f.source, "lines.txt", "first\nsecond\n")
	if err := os.Symlink("lines.txt", filepath.Join(f.source, "link")); err != nil {
		t.Fatal(err)
	}
	f.publish(t)
	runGitT(t, f.checkout, "config", "core.autocrlf", "true")
	runGitT(t, f.checkout, "config", "core.symlinks", "false")
	slot := warmWorkspaceSlot{UnityProjectPath: "."}
	if err := prepareNativeWarmCheckoutForTest(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(f.checkout, "lines.txt")); err != nil || string(got) != "first\r\nsecond\r\n" {
		t.Fatalf("fixture did not create CRLF worktree: %q %v", got, err)
	}
	proof := warmReturnTree{root: filepath.Dir(f.checkout), checkout: f.checkout, native: true}
	if _, err := proof.verify(t.Context(), slot, f.cache, f.repo); err != nil {
		t.Fatal(err)
	}
	warmGitWrite(t, f.checkout, "link", "different-target")
	if _, err := proof.verify(t.Context(), slot, f.cache, f.repo); err == nil {
		t.Fatal("accepted changed symlink contents")
	}
}

func TestWarmNativeLeasePreparesAndReturnsOriginalRepository(t *testing.T) {
	if os.Getenv("LOOPFORGE_TEST_NATIVE_MOUNTS") != "1" {
		t.Skip("run inside a private user and mount namespace")
	}
	f := newWarmPoolFixture(t)
	gitRepo := nativeWarmTestClone(t, newWarmGitFixture(t))
	f.homeA = gitRepo.checkout
	f.writeSlots(t, f.homeA)
	p, err := openWarmWorkspacePoolWithinMode(f.config, f.workspace, f.journal, true)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	lease := &domain.WarmWorkspaceLease{OwnerRunID: "run-one", RunnerID: "runner", Generation: 1, Slots: map[string]string{"a": "a"}}
	d := &Daemon{cfg: Config{WarmNative: true, DefaultWorkspaceRoot: f.workspace}, warmWorkspacePool: p, state: daemonState{RunnerID: lease.RunnerID}}
	d.warmHostVisible.Store(true)
	repo := gitRepo.repo
	repo.RepositoryID, repo.Dir = "a", "pet"
	sandbox := &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: domain.SandboxDriverNative}}
	request := domain.RunnerWorkspaceRequest{RunID: lease.OwnerRunID, WarmWorkspaceLease: lease, Sandbox: sandbox,
		Repositories:          []domain.RepositoryContext{{RepositoryID: repo.RepositoryID, WorkspaceDir: repo.Dir}},
		PreparationInvocation: &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{repo}}}
	control := func(operation string) domain.RunnerWorkspaceResponse {
		t.Helper()
		request.Operation = operation
		reply := d.warmWorkspaceControl(t.Context(), request)
		if reply.Error != "" {
			t.Fatalf("%s: %s", operation, reply.Error)
		}
		return reply
	}
	control(domain.RunnerWorkspaceOperationWarmPrepare)
	root := filepath.Join(f.workspace, "runs", lease.OwnerRunID, "workspace")
	defer func() { _ = unmountNativeWarmDir(filepath.Join(root, repo.Dir)) }()
	control(domain.RunnerWorkspaceOperationWarmCheckout)
	claimedSandbox := *sandbox
	claimedSandbox.Profile.Driver = domain.SandboxDriverBwrap
	task := domain.RunnerTask{
		RunID: lease.OwnerRunID, RunnerID: lease.RunnerID, Workspace: "runs/run-one/workspace",
		Command: "true", Sandbox: &claimedSandbox, WarmWorkspaceLease: lease,
	}
	if err := d.checkWarmTask(t.Context(), task); err != nil {
		t.Fatalf("claimed warm task: %v", err)
	}
	requirements, _ := d.localTaskSandboxSpec(task)
	sb, err := runtime.OpenRemoteSandbox(t.Context(), "native-checkout-run", root, claimedSandbox.Profile, requirements, runtime.SandboxResources{})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close(t.Context())
	output, err := sb.Exec(t.Context(), runtime.CommandSpec{Argv: []string{
		"/bin/sh", "-ec", "printf changed > pet/dev.txt",
	}}, nil)
	if err != nil {
		t.Fatalf("native command failed: %s %v", output, err)
	}
	if contents, err := os.ReadFile(filepath.Join(f.homeA, "dev.txt")); err != nil || string(contents) != "changed" {
		t.Fatalf("D: did not see command output: %s %v", contents, err)
	}
	runGitT(t, f.homeA, "add", "dev.txt")
	runGitT(t, f.homeA, "-c", "user.name=test", "-c", "user.email=test@example.org", "commit", "-m", "sandbox edit")
	runGitT(t, f.homeA, "push", "origin", "HEAD")
	proof := control(domain.RunnerWorkspaceOperationWarmReleaseCheck)
	request.WarmWorkspaceReturnID = proof.WarmWorkspaceReturnID
	if err := json.Unmarshal(proof.Payload, &request.WarmWorkspaceRefs); err != nil {
		t.Fatal(err)
	}
	control(domain.RunnerWorkspaceOperationWarmReleaseCommit)
	if path := filepath.Join(f.homeA, "dev.txt"); warmGitStat(t, path).Size() != 7 {
		t.Fatalf("local edit disappeared on return: %s", path)
	}
}

func openNativeLeaseTestPool(t *testing.T) (*warmWorkspacePool, warmGitFixture) {
	t.Helper()
	if os.Getenv("LOOPFORGE_TEST_NATIVE_MOUNTS") != "1" {
		t.Skip("run inside a private user and mount namespace")
	}
	f := newWarmPoolFixture(t)
	g := nativeWarmTestClone(t, newWarmGitFixture(t))
	f.writeSlots(t, g.checkout)
	p, err := openWarmWorkspacePoolWithinMode(f.config, f.workspace, f.journal, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.close() })
	g.repo.RepositoryID, g.repo.Dir = "a", "checkout"
	return p, g
}

func placeNativeLeaseTest(t *testing.T, p *warmWorkspacePool, owner string, generation uint64) domain.WarmWorkspaceLease {
	t.Helper()
	l := domain.WarmWorkspaceLease{OwnerRunID: owner, RunnerID: "runner", Epoch: generation, Generation: generation, Slots: map[string]string{"a": "a"}}
	if err := p.reserve(l); err != nil {
		t.Fatal(err)
	}
	if err := p.place(t.Context(), l, map[string]string{"a": "checkout"}); err != nil {
		t.Fatal(err)
	}
	target := p.state.Leases[owner].Targets["a"]
	t.Cleanup(func() { _ = unmountNativeWarmDir(target) })
	return l
}

func prepareNativeLeaseTest(t *testing.T, p *warmWorkspacePool, g warmGitFixture, l domain.WarmWorkspaceLease, refs []domain.WarmWorkspaceRef) error {
	t.Helper()
	return p.prepareLeaseCheckout(t.Context(), l, &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}, WarmWorkspaceRefs: refs})
}

func returnNativeLeaseTest(t *testing.T, p *warmWorkspacePool, g warmGitFixture, l domain.WarmWorkspaceLease) []domain.WarmWorkspaceRef {
	t.Helper()
	d := nativeReturnTestDaemon(p, l)
	req := nativeReturnTestProof(t, d, testNativeReturnRequest(l, g.repo))
	if resp := d.warmWorkspaceControl(t.Context(), req); resp.Error != "" {
		t.Fatal(resp.Error)
	}
	return req.WarmWorkspaceRefs
}

func TestWarmNativeLeaseRestoresPublishedHistoricalBranch(t *testing.T) {
	p, g := openNativeLeaseTestPool(t)
	first := placeNativeLeaseTest(t, p, "first", 1)
	if err := prepareNativeLeaseTest(t, p, g, first, nil); err != nil {
		t.Fatal(err)
	}
	sentinel := warmGitStat(t, filepath.Join(g.checkout, "Library/sentinel"))
	refs := returnNativeLeaseTest(t, p, g, first)
	warmGitWrite(t, g.source, "advanced", "published")
	latest := g.publish(t)
	second := placeNativeLeaseTest(t, p, "second", 2)
	if err := prepareNativeLeaseTest(t, p, g, second, nil); err != nil {
		t.Fatal(err)
	}
	_ = returnNativeLeaseTest(t, p, g, second)
	reopened := placeNativeLeaseTest(t, p, "first", 3)
	if err := prepareNativeLeaseTest(t, p, g, reopened, refs); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, g.checkout, "rev-parse", "HEAD"); got != refs[0].Commit {
		t.Fatalf("no exact restore: %s", got)
	}
	if got := runGitT(t, g.checkout, "branch", "--show-current"); got != refs[0].Branch {
		t.Fatalf("restore left the wrong branch checked out: %s", got)
	}
	if got := runGitT(t, g.source, "ls-remote", "origin", "refs/heads/master"); !strings.HasPrefix(got, latest+"\t") {
		t.Fatalf("remote branch changed: %s", got)
	}
	warmPoolRequireSame(t, sentinel, filepath.Join(g.checkout, "Library/sentinel"))
}

func TestWarmNativeLeaseRefreshesBaseline(t *testing.T) {
	p, g := openNativeLeaseTestPool(t)
	first := placeNativeLeaseTest(t, p, "first", 1)
	if err := prepareNativeLeaseTest(t, p, g, first, nil); err != nil {
		t.Fatal(err)
	}
	refs := returnNativeLeaseTest(t, p, g, first)
	warmGitWrite(t, g.source, "advanced", "published")
	latest := g.publish(t)
	second := placeNativeLeaseTest(t, p, "second", 2)
	if err := prepareNativeLeaseTest(t, p, g, second, nil); err != nil {
		t.Fatal(err)
	}
	check := func(want string) {
		t.Helper()
		content, err := os.ReadFile(gitwork.BaselinePath(g.checkout))
		if err != nil || strings.TrimSpace(string(content)) != want {
			t.Fatalf("baseline = %q, want %s, err = %v", content, want, err)
		}
	}
	check(latest)
	warmGitWrite(t, g.source, "later", "third revision")
	g.publish(t)
	if err := prepareNativeLeaseTest(t, p, g, second, nil); err != nil {
		t.Fatal(err)
	}
	check(latest)
	_ = returnNativeLeaseTest(t, p, g, second)
	reopened := placeNativeLeaseTest(t, p, "first", 3)
	if err := prepareNativeLeaseTest(t, p, g, reopened, refs); err != nil {
		t.Fatal(err)
	}
	check(refs[0].Commit)
}

func TestWarmNativeCheckoutResumesIntent(t *testing.T) {
	for _, mode := range []string{"before", "after", "lfs", "lfs changed"} {
		t.Run(mode, func(t *testing.T) {
			p, g := openNativeLeaseTestPool(t)
			lease := placeNativeLeaseTest(t, p, "first", 1)
			before := runGitT(t, g.checkout, "rev-parse", "HEAD")
			if strings.HasPrefix(mode, "lfs") {
				if _, err := exec.LookPath("git-lfs"); err != nil {
					t.Skip("Git LFS is required")
				}
				runGitT(t, g.source, "lfs", "install", "--local")
				runGitT(t, g.source, "lfs", "track", "*.bin")
				warmGitWrite(t, g.source, "asset.bin", strings.Repeat("abcd", 400))
			}
			warmGitWrite(t, g.source, "advanced", "published")
			want := g.publish(t)
			if mode == "lfs changed" {
				// Simulate a post-checkout, same-size edit during materialization.
				t.Setenv("LOOPFORGE_TEST_CHECKOUT_MODE", "lfs")
			} else {
				t.Setenv("LOOPFORGE_TEST_CHECKOUT_MODE", mode)
			}
			testNativeGitWrapper(t, `for arg do
	if [ "$arg" = checkout ] && [ -n "$LOOPFORGE_TEST_CHECKOUT_MODE" ]; then
		if [ "$LOOPFORGE_TEST_CHECKOUT_MODE" != before ]; then
			"$git" "$@" || exit $?
		fi
		echo "injected checkout failure" >&2
		exit 42
	fi
done`)
			err := prepareNativeLeaseTest(t, p, g, lease, nil)
			if err == nil || !strings.Contains(err.Error(), "injected checkout failure") {
				t.Fatalf("wanted injected preparation error, got %v", err)
			}
			t.Setenv("LOOPFORGE_TEST_CHECKOUT_MODE", "")
			if mode == "lfs changed" {
				warmGitWrite(t, g.checkout, "asset.bin", strings.Repeat("wxyz", 400))
			}
			target := p.state.Leases[lease.OwnerRunID].Targets["a"]
			journal := filepath.Join(p.journalDir.Name(), p.journalName)
			config := filepath.Join(filepath.Dir(filepath.Dir(journal)), "pool.json")
			if err := unmountNativeWarmDir(target); err != nil {
				t.Fatal(err)
			}
			if err := p.close(); err != nil {
				t.Fatal(err)
			}
			restarted, err := openWarmWorkspacePoolWithinMode(config, p.workspaceRoot, journal, true)
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.close()
			intent := restarted.state.Leases[lease.OwnerRunID].NativeCheckout
			if intent == nil || intent.SlotID != "a" || intent.Before.Commit != before || intent.Target.Commit != want || intent.Baseline != want {
				t.Fatalf("restart lost checkout intent: %+v", intent)
			}
			warmGitWrite(t, g.source, "still-later", "new remote head")
			g.publish(t)
			err = prepareNativeLeaseTest(t, restarted, g, lease, nil)
			if mode == "lfs changed" {
				if err == nil {
					t.Fatal("accepted a modified LFS file")
				}
				if got, _ := os.ReadFile(filepath.Join(g.checkout, "asset.bin")); string(got) != strings.Repeat("wxyz", 400) {
					t.Fatal("modified LFS file was replaced")
				}
				return
			}
			if err != nil {
				t.Fatalf("retry after checkout interruption: %v", err)
			}
			if got := runGitT(t, g.checkout, "rev-parse", "HEAD"); got != want || got == before {
				t.Fatalf("recovered the wrong commit: %s", got)
			}
			if mode == "lfs" {
				if got, _ := os.ReadFile(filepath.Join(g.checkout, "asset.bin")); string(got) != strings.Repeat("abcd", 400) {
					t.Fatal("recovery did not materialize the original LFS asset")
				}
			}
		})
	}
}

func testNativeGitWrapper(t *testing.T, script string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := "#!/bin/sh\ngit=" + strconv.Quote(git) + "\n" + script + "\nexec \"$git\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestWarmNativeCheckoutLegacyIntentNeedsInspection(t *testing.T) {
	p, g := openNativeLeaseTestPool(t)
	lease := placeNativeLeaseTest(t, p, "first", 1)
	a := p.state.Leases[lease.OwnerRunID]
	a.PreparingGit = true
	if err := p.saveAllocation(a); err != nil {
		t.Fatal(err)
	}
	before := runGitT(t, g.checkout, "rev-parse", "HEAD")
	if err := prepareNativeLeaseTest(t, p, g, lease, nil); err == nil || !strings.Contains(err.Error(), "requires inspection") {
		t.Fatalf("legacy intent was not retained: %v", err)
	}
	if got := runGitT(t, g.checkout, "rev-parse", "HEAD"); got != before {
		t.Fatalf("legacy intent changed checkout: %s", got)
	}
}

func prepareNativeWarmCheckoutForTest(ctx context.Context, checkout, cache string, slot warmWorkspaceSlot, repo domain.RunnerRepository, restore *domain.WarmWorkspaceRef, beforeWrite func(domain.WarmWorkspaceRef) error) error {
	_, err := prepareNativeWarmCheckout(ctx, checkout, cache, slot, repo, restore, nil, func(intent nativeWarmCheckoutIntent) error {
		return beforeWrite(intent.Target)
	})
	return err
}

func TestWarmNativeHistoricalBranchGuards(t *testing.T) {
	for _, mode := range []string{"unpublished branch tip", "concurrent branch update"} {
		t.Run(mode, func(t *testing.T) {
			g := nativeWarmTestClone(t, newWarmGitFixture(t))
			slot := warmWorkspaceSlot{UnityProjectPath: "."}
			old := runGitT(t, g.checkout, "rev-parse", "HEAD")
			warmGitWrite(t, g.source, "advanced", "published")
			tip := g.publish(t)
			noOp := func(domain.WarmWorkspaceRef) error { return nil }
			if err := prepareNativeWarmCheckoutForTest(t.Context(), g.checkout, g.cache, slot, g.repo, nil, noOp); err != nil {
				t.Fatal(err)
			}
			changed := runGitT(t, g.checkout, "-c", "user.email=git@example.test", "-c", "user.name=Test", "commit-tree", "HEAD^{tree}", "-p", tip, "-m", "private commit")
			if mode == "unpublished branch tip" {
				runGitT(t, g.checkout, "update-ref", "refs/heads/master", changed, tip)
			} else {
				t.Setenv("LOOPFORGE_TEST_NATIVE_BRANCH_TIP", changed)
				testNativeGitWrapper(t, `for arg do
	if [ "$arg" = --detach ]; then
		"$git" -C "$PWD" update-ref refs/heads/master "$LOOPFORGE_TEST_NATIVE_BRANCH_TIP" || exit $?
	fi
done`)
			}
			restore := &domain.WarmWorkspaceRef{RepositoryID: g.repo.RepositoryID, Branch: "master", Commit: old}
			err := prepareNativeWarmCheckoutForTest(t.Context(), g.checkout, g.cache, slot, g.repo, restore, noOp)
			if err == nil {
				t.Fatal("restore accepted a private or concurrent branch update")
			}
			if got := runGitT(t, g.checkout, "rev-parse", "refs/heads/master"); got != changed {
				t.Fatalf("lost local branch tip %s", got)
			}
		})
	}
}

func TestWarmNativeCheckoutRejectsBranchNameConflictBeforeWriting(t *testing.T) {
	p, g := openNativeLeaseTestPool(t)
	l := placeNativeLeaseTest(t, p, "first", 1)
	before := runGitT(t, g.checkout, "rev-parse", "HEAD")
	runGitT(t, g.checkout, "branch", "feature")
	warmGitWrite(t, g.source, "incoming", "remote work")
	g.publish(t)
	g.repo.WorkBranch, g.repo.CreateNewWorkBranch = "feature/nested", true
	err := prepareNativeLeaseTest(t, p, g, l, nil)
	if err == nil || errors.Is(err, errNativeWarmCheckoutBusy) || !strings.Contains(err.Error(), "feature") {
		t.Fatalf("wanted an actionable branch conflict, got %v", err)
	}
	if got := runGitT(t, g.checkout, "rev-parse", "HEAD"); got != before {
		t.Fatalf("branch conflict changed HEAD to %s", got)
	}
	if got := runGitT(t, g.checkout, "branch", "--show-current"); got != "master" {
		t.Fatalf("branch conflict detached the original checkout: %s", got)
	}
}

func TestWarmNativeLFSStagingSurvivesCrash(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("Git LFS is required")
	}
	oid := strings.Repeat("a", 64)
	if checkout := os.Getenv("LOOPFORGE_TEST_LFS_CRASH_CHECKOUT"); checkout != "" {
		manifest := warmLFSManifest{storage: os.Getenv("LOOPFORGE_TEST_LFS_CRASH_STORAGE"),
			Files: []warmLFSFile{{Name: "asset.bin", OID: oid, Size: 4096}}}
		ref, err := warmCheckoutRef(checkout, "repo")
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal(applyNativeWarmCheckout(t.Context(), checkout, warmWorkspaceSlot{}, nativeWarmCheckoutIntent{Target: ref, LFSStageID: strings.Repeat("A", 26)}, true, manifest, warmWindowsTree{}))
	}
	g := nativeWarmTestClone(t, newWarmGitFixture(t))
	pointer := "version https://git-lfs.github.com/spec/v1\noid sha256:" + oid + "\nsize 4096\n"
	warmGitWrite(t, g.checkout, "asset.bin", pointer)
	runGitT(t, g.checkout, "add", "asset.bin")
	runGitT(t, g.checkout, "-c", "user.name=Test", "-c", "user.email=git@example.test", "commit", "-qm", "asset")
	storage := t.TempDir()
	pipe := warmLFSObject(storage, oid)
	if err := os.MkdirAll(filepath.Dir(pipe), 0700); err != nil {
		t.Fatal(err)
	}
	if err := unix.Mkfifo(pipe, 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestWarmNativeLFSStagingSurvivesCrash$")
	cmd.Env = append(os.Environ(), "LOOPFORGE_TEST_LFS_CRASH_CHECKOUT="+g.checkout, "LOOPFORGE_TEST_LFS_CRASH_STORAGE="+storage)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	deadline := time.After(10 * time.Second)
	var writer *os.File
	for writer == nil {
		var err error
		writer, err = os.OpenFile(pipe, os.O_WRONLY|unix.O_NONBLOCK, 0)
		if err != nil && !errors.Is(err, unix.ENXIO) {
			t.Fatal(err)
		}
		if writer == nil {
			select {
			case err := <-done:
				t.Fatalf("hydration exited before beginning to copy: %v", err)
			case <-deadline:
				t.Fatal("hydration never opened the source")
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	defer writer.Close()
	if _, err := writer.WriteString("partial asset bytes"); err != nil {
		t.Fatal(err)
	}
	for {
		staged, err := filepath.Glob(filepath.Join(g.checkout, ".warm-lfs-*"))
		if err != nil {
			t.Fatal(err)
		}
		insideGit, err := filepath.Glob(filepath.Join(g.checkout, ".git", "agentflow-warm-lfs-*", ".warm-lfs-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(staged)+len(insideGit) > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("hydration exited before staging bytes: %v", err)
		case <-deadline:
			t.Fatal("hydration never created a staging file")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-done
	if status := runGitT(t, g.checkout, "status", "--porcelain"); status != "" {
		t.Fatalf("interrupted LFS copy left an untracked worktree file: %s", status)
	}
}
