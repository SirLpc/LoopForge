package runner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
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
	if err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if got := runGitT(t, f.checkout, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD = %s, want %s", got, want)
	}
	warmPoolRequireSame(t, meta, filepath.Join(f.checkout, ".git"))
	warmPoolRequireSame(t, library, filepath.Join(f.checkout, "Library/sentinel"))
	runGitT(t, f.checkout, "rev-parse", "personal")
	warmGitWrite(t, f.checkout, "new-code", "local edit")
	if err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err == nil {
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
		return prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil })
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
	if err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
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
	if err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
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
	err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, warmWorkspaceSlot{UnityProjectPath: "."}, f.repo, nil, func(domain.WarmWorkspaceRef) error {
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
	if err := prepareNativeWarmCheckout(t.Context(), f.checkout, f.cache, slot, f.repo, nil, func(domain.WarmWorkspaceRef) error { return nil }); err != nil {
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
	if err := json.Unmarshal(proof.Payload, &request.WarmWorkspaceRefs); err != nil {
		t.Fatal(err)
	}
	control(domain.RunnerWorkspaceOperationWarmReleaseCommit)
	if path := filepath.Join(f.homeA, "dev.txt"); warmGitStat(t, path).Size() != 7 {
		t.Fatalf("local edit disappeared on return: %s", path)
	}
}
