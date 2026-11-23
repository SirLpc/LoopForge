package runner

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func nativeReturnTestDaemon(p *warmWorkspacePool, l domain.WarmWorkspaceLease) *Daemon {
	return &Daemon{cfg: Config{WarmNative: true}, warmWorkspacePool: p, state: daemonState{RunnerID: l.RunnerID}}
}

func nativeReturnTestProof(t *testing.T, d *Daemon, req domain.RunnerWorkspaceRequest) domain.RunnerWorkspaceRequest {
	t.Helper()
	req.Operation = domain.RunnerWorkspaceOperationWarmReleaseCheck
	resp := d.warmWorkspaceControl(t.Context(), req)
	if resp.Error != "" {
		t.Fatal(resp.Error)
	}
	if err := json.Unmarshal(resp.Payload, &req.WarmWorkspaceRefs); err != nil {
		t.Fatal(err)
	}
	req.WarmWorkspaceReturnID = resp.WarmWorkspaceReturnID
	req.Operation = domain.RunnerWorkspaceOperationWarmReleaseCommit
	return req
}

func testNativeReturnRequest(l domain.WarmWorkspaceLease, repos ...domain.RunnerRepository) domain.RunnerWorkspaceRequest {
	return domain.RunnerWorkspaceRequest{RunID: l.OwnerRunID, WarmWorkspaceLease: &l,
		PreparationInvocation: &domain.RunnerInvocation{Repositories: repos}}
}

func restartNativeLeaseTestPool(t *testing.T, p *warmWorkspacePool) *warmWorkspacePool {
	t.Helper()
	journal := filepath.Join(p.journalDir.Name(), p.journalName)
	config := filepath.Join(filepath.Dir(filepath.Dir(journal)), "pool.json")
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := openWarmWorkspacePoolWithinMode(config, p.workspaceRoot, journal, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.close() })
	return restarted
}

func TestWarmNativeReturnChecksTrackedContentsAtConfirmation(t *testing.T) {
	p, g := openNativeLeaseTestPool(t)
	l := placeNativeLeaseTest(t, p, "first", 1)
	if err := prepareNativeLeaseTest(t, p, g, l, nil); err != nil {
		t.Fatal(err)
	}
	d := nativeReturnTestDaemon(p, l)
	warmGitWrite(t, g.checkout, "README.md", "external edit")
	commit := nativeReturnTestProof(t, d, testNativeReturnRequest(l, g.repo))
	if got := d.warmWorkspaceControl(t.Context(), commit); got.Code != domain.WorkspaceErrorWarmReturnStale {
		t.Fatalf("modified checkout must retain the lease: %+v", got)
	}
	if got, err := os.ReadFile(filepath.Join(g.checkout, "README.md")); err != nil || string(got) != "external edit" {
		t.Fatalf("local change was lost: %q, %v", got, err)
	}
}

func TestWarmNativeReturnRetainsUnreadableTrackedFile(t *testing.T) {
	p, g := openNativeLeaseTestPool(t)
	l := placeNativeLeaseTest(t, p, "first", 1)
	if err := prepareNativeLeaseTest(t, p, g, l, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(g.checkout, "README.md")
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0644) })
	if f, err := os.Open(path); err == nil {
		f.Close()
		t.Skip("the current user can still read mode-000 files")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	d := nativeReturnTestDaemon(p, l)
	commit := nativeReturnTestProof(t, d, testNativeReturnRequest(l, g.repo))
	if got := d.warmWorkspaceControl(t.Context(), commit); got.Code != domain.WorkspaceErrorWarmReturnStale {
		t.Fatalf("unreadable tracked file must invalidate the receipt: %+v", got)
	}
	if a := p.state.Leases[l.OwnerRunID]; a.ReturnProven || a.Phase != warmWorkspacePlaced {
		t.Fatalf("unreadable file kept the lease frozen: %+v", a)
	}
}

func TestWarmNativeReturnReusesVerifiedLFS(t *testing.T) {
	if _, err := exec.LookPath("git-lfs"); err != nil {
		t.Skip("git-lfs unavailable")
	}
	for _, mode := range []string{"unchanged", "cache changed", "runner restart"} {
		t.Run(mode, func(t *testing.T) {
			p, g := openNativeLeaseTestPool(t)
			runGitT(t, g.source, "lfs", "install", "--local")
			runGitT(t, g.source, "lfs", "track", "*.bin")
			const asset = "published LFS content"
			warmGitWrite(t, g.source, "asset.bin", asset)
			g.publish(t)
			l := placeNativeLeaseTest(t, p, "first", 1)
			if err := prepareNativeLeaseTest(t, p, g, l, nil); err != nil {
				t.Fatal(err)
			}
			d := nativeReturnTestDaemon(p, l)
			commit := nativeReturnTestProof(t, d, testNativeReturnRequest(l, g.repo))
			if mode == "runner restart" {
				p = restartNativeLeaseTestPool(t, p)
				d = nativeReturnTestDaemon(p, l)
			} else {
				testNativeGitWrapper(t, `previous=; for arg do
	if [ "$previous" = lfs ] && [ "$arg" = ls-files ]; then echo 'redundant LFS scan' >&2; exit 42; fi
	previous=$arg
done`)
			}
			if mode == "cache changed" {
				slot := p.slots[l.Slots[g.repo.RepositoryID]]
				storage := filepath.Join(warmLFSCache(p.gitCache(slot.ID), slot.ProjectID, g.repo), ".git", "lfs")
				path := warmLFSObject(storage, fmt.Sprintf("%x", sha256.Sum256([]byte(asset))))
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(strings.Repeat("X", len(asset))), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			got := d.warmWorkspaceControl(t.Context(), commit)
			if mode == "cache changed" {
				if !strings.Contains(got.Error, "verified Git LFS cache object unavailable") || p.state.Leases[l.OwnerRunID].Phase != warmWorkspacePlaced {
					t.Fatalf("invalid protected cache must block return: %+v", got)
				}
			} else if got.Error != "" || p.state.Leases[l.OwnerRunID].Phase != warmWorkspaceReleased {
				t.Fatalf("could not return workspace: %+v", got)
			}
		})
	}
}

func TestWarmNativeReturnRejectsChangeAfterProof(t *testing.T) {
	for _, mode := range []string{"tracked", "index", "unarchived"} {
		t.Run(mode, func(t *testing.T) {
			p, g := openNativeLeaseTestPool(t)
			l := placeNativeLeaseTest(t, p, "first", 1)
			if err := prepareNativeLeaseTest(t, p, g, l, nil); err != nil {
				t.Fatal(err)
			}
			d := nativeReturnTestDaemon(p, l)
			req := testNativeReturnRequest(l, g.repo)
			commit := nativeReturnTestProof(t, d, req)
			path := "README.md"
			if mode == "unarchived" {
				path = "new.txt"
			}
			warmGitWrite(t, g.checkout, path, "external edit")
			if mode == "index" {
				runGitT(t, g.checkout, "add", path)
			}
			checkStale := func(resp domain.RunnerWorkspaceResponse) {
				t.Helper()
				if resp.Status != http.StatusConflict || resp.Code != domain.WorkspaceErrorWarmReturnStale ||
					resp.WarmWorkspaceReturnID != commit.WarmWorkspaceReturnID || resp.WarmWorkspaceReturnID == "" {
					t.Fatalf("did not reject changed checkout with matching receipt: %+v", resp)
				}
			}
			checkStale(d.warmWorkspaceControl(t.Context(), commit))
			if got, err := os.ReadFile(filepath.Join(g.checkout, path)); err != nil || string(got) != "external edit" {
				t.Fatalf("source edit changed: %q %v", got, err)
			}
			p = restartNativeLeaseTestPool(t, p)
			d = nativeReturnTestDaemon(p, l)
			checkStale(d.warmWorkspaceControl(t.Context(), commit))
			if a := p.state.Leases[l.OwnerRunID]; a.Phase != warmWorkspacePlaced || a.ReturnProven || !a.GitReady {
				t.Fatalf("did not retain workspace for its owner: %+v", a)
			}
			if mode == "unarchived" {
				if err := os.Remove(filepath.Join(g.checkout, path)); err != nil {
					t.Fatal(err)
				}
			} else {
				runGitT(t, g.checkout, "restore", "--source=HEAD", "--staged", "--worktree", "--", path)
			}
			replacement := nativeReturnTestProof(t, d, req)
			if replacement.WarmWorkspaceReturnID == commit.WarmWorkspaceReturnID {
				t.Fatal("reproof reused an invalidated id")
			}
			if got := d.warmWorkspaceControl(t.Context(), commit); got.Error == "" {
				t.Fatal("old commit accepted against a new proof with identical refs")
			}
			if got := d.warmWorkspaceControl(t.Context(), replacement); got.Error != "" {
				t.Fatal(got.Error)
			}
		})
	}
}

func TestWarmNativeReturnLegacyReceiptRechecksFiles(t *testing.T) {
	for _, dirty := range []bool{false, true} {
		name := "clean"
		if dirty {
			name = "dirty"
		}
		t.Run(name, func(t *testing.T) {
			p, g := openNativeLeaseTestPool(t)
			l := placeNativeLeaseTest(t, p, "first", 1)
			if err := prepareNativeLeaseTest(t, p, g, l, nil); err != nil {
				t.Fatal(err)
			}
			d := nativeReturnTestDaemon(p, l)
			commit := nativeReturnTestProof(t, d, testNativeReturnRequest(l, g.repo))
			a := p.state.Leases[l.OwnerRunID]
			a.NativeReturn = nil
			if err := p.saveAllocation(a); err != nil {
				t.Fatal(err)
			}
			commit.WarmWorkspaceReturnID = ""
			if dirty {
				warmGitWrite(t, g.checkout, "README.md", "external edit")
			}
			resp := d.warmWorkspaceControl(t.Context(), commit)
			if dirty {
				if resp.Code != domain.WorkspaceErrorWarmReturnStale || resp.WarmWorkspaceReturnID != "" {
					t.Fatalf("legacy proof could not be invalidated: %+v", resp)
				}
			} else if resp.Error != "" || resp.WarmWorkspaceReturnID != "" {
				t.Fatalf("legacy proof did not commit: %+v", resp)
			}
		})
	}
}

func twoNativeReturnTestSlots(t *testing.T) (*warmWorkspacePool, warmGitFixture, warmGitFixture, domain.WarmWorkspaceLease) {
	t.Helper()
	if os.Getenv("LOOPFORGE_TEST_NATIVE_MOUNTS") != "1" {
		t.Skip("run inside a private user and mount namespace")
	}
	f := newWarmPoolFixture(t)
	a := nativeWarmTestClone(t, newWarmGitFixture(t))
	b := nativeWarmTestClone(t, newWarmGitFixture(t))
	a.repo.RepositoryID, a.repo.Dir = "a", "one"
	b.repo.RepositoryID, b.repo.Dir = "b", "two"
	f.writeSlots(t, a.checkout, b.checkout)
	p, err := openWarmWorkspacePoolWithinMode(f.config, f.workspace, f.journal, true)
	if err != nil {
		t.Fatal(err)
	}
	l := domain.WarmWorkspaceLease{OwnerRunID: "group", RunnerID: "runner", Epoch: 1, Generation: 1, Slots: map[string]string{"a": "a", "b": "b"}}
	t.Cleanup(func() { _ = p.close() })
	if err := p.reserve(l); err != nil {
		t.Fatal(err)
	}
	if err := p.place(t.Context(), l, map[string]string{"a": a.repo.Dir, "b": b.repo.Dir}); err != nil {
		t.Fatal(err)
	}
	for _, target := range p.state.Leases[l.OwnerRunID].Targets {
		t.Cleanup(func() { _ = unmountNativeWarmDir(target) })
	}
	return p, a, b, l
}

func TestWarmNativeReturnUnadmittedStillChecksPreparedSlots(t *testing.T) {
	p, a, b, l := twoNativeReturnTestSlots(t)
	b.repo.Ref = "missing-branch"
	req := testNativeReturnRequest(l, a.repo, b.repo)
	if err := p.prepareLeaseCheckout(t.Context(), l, req.PreparationInvocation); err == nil {
		t.Fatal("second repository unexpectedly prepared")
	}
	if _, ok := p.state.Leases[l.OwnerRunID].PreparedRefs["a"]; !ok {
		t.Fatal("missing already-prepared repository")
	}
	d := nativeReturnTestDaemon(p, l)
	commit := nativeReturnTestProof(t, d, req)
	if len(commit.WarmWorkspaceRefs) != 0 {
		t.Fatal("an unadmitted workspace must not publish new refs")
	}
	warmGitWrite(t, a.checkout, "README.md", "external edit")
	if got := d.warmWorkspaceControl(t.Context(), commit); got.Code != domain.WorkspaceErrorWarmReturnStale || got.WarmWorkspaceReturnID == "" {
		t.Fatalf("partially prepared return did not recheck prepared files: %+v", got)
	}
}

func TestWarmNativeReturnRecoversInterruptedGroup(t *testing.T) {
	for _, mode := range []string{"unchanged", "externally modified", "archive recreated"} {
		t.Run(mode, func(t *testing.T) {
			dirty := mode != "unchanged"
			p, one, two, l := twoNativeReturnTestSlots(t)
			req := testNativeReturnRequest(l, one.repo, two.repo)
			if err := p.prepareLeaseCheckout(t.Context(), l, req.PreparationInvocation); err != nil {
				t.Fatal(err)
			}
			warmGitWrite(t, one.checkout, "result.txt", "archived")
			req.ArtifactFiles = []domain.WorkspaceArtifactFile{
				{Path: "one/result.txt", Content: "archived", SHA256: runtime.WorkspaceContentSHA("archived")},
			}
			d := nativeReturnTestDaemon(p, l)
			commit := nativeReturnTestProof(t, d, req)
			a := p.state.Leases[l.OwnerRunID]
			if err := os.Remove(filepath.Join(one.checkout, "result.txt")); err != nil {
				t.Fatal(err)
			}
			a.Phase = warmWorkspaceReturning
			if err := p.saveAllocation(a); err != nil {
				t.Fatal(err)
			}
			if err := unmountNativeWarmDir(a.Targets["a"]); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "externally modified":
				warmGitWrite(t, one.checkout, "README.md", "external edit")
			case "archive recreated":
				warmGitWrite(t, one.checkout, "result.txt", "archived")
			}
			p = restartNativeLeaseTestPool(t, p)
			if active := p.state.activeSlots(); !active["a"] || !active["b"] {
				t.Fatalf("restart released slots before revalidation: %v", active)
			}
			got := nativeReturnTestDaemon(p, l).warmWorkspaceControl(t.Context(), commit)
			if dirty {
				if got.Code != domain.WorkspaceErrorWarmReturnStale {
					t.Fatalf("did not detect changes after partial unmount: %+v", got)
				}
				if got, err := os.ReadFile(filepath.Join(one.checkout, "result.txt")); err != nil || string(got) != "archived" {
					t.Fatalf("interrupted release lost an archived file: %q %v", got, err)
				}
				for id, target := range a.Targets {
					if !p.inspectSlot(id, target).atTarget {
						t.Fatalf("slot %s was not restored to its owner", id)
					}
				}
			} else if got.Error != "" || p.state.Leases[l.OwnerRunID].Phase != warmWorkspaceReleased {
				t.Fatalf("could not resume interrupted return: %+v", got)
			}
		})
	}
}

func TestWarmNativeReturnRetriesReleasedAck(t *testing.T) {
	p, g := openNativeLeaseTestPool(t)
	l := placeNativeLeaseTest(t, p, "first", 1)
	if err := prepareNativeLeaseTest(t, p, g, l, nil); err != nil {
		t.Fatal(err)
	}
	d := nativeReturnTestDaemon(p, l)
	commit := nativeReturnTestProof(t, d, testNativeReturnRequest(l, g.repo))
	if err := os.Rename(g.repo.CloneURL, g.repo.CloneURL+".offline"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got := d.warmWorkspaceControl(t.Context(), commit)
		if got.Error != "" || got.WarmWorkspaceReturnID == "" || got.WarmWorkspaceReturnID != commit.WarmWorkspaceReturnID {
			t.Fatalf("did not return a durable acknowledgment: %+v", got)
		}
	}
}

func TestWarmNativeReturnFromInterruptedCheckout(t *testing.T) {
	for _, atTarget := range []bool{false, true} {
		name := "before checkout"
		if atTarget {
			name = "after checkout"
		}
		t.Run(name, func(t *testing.T) {
			p, g := openNativeLeaseTestPool(t)
			l := placeNativeLeaseTest(t, p, "first", 1)
			before := runGitT(t, g.checkout, "rev-parse", "HEAD")
			warmGitWrite(t, g.source, "updated", "published")
			want := g.publish(t)
			mode := "before"
			if atTarget {
				mode = "after"
			}
			t.Setenv("LOOPFORGE_TEST_INTERRUPT", mode)
			testNativeGitWrapper(t, `for arg do
	if [ "$arg" = --detach ] && [ -n "$LOOPFORGE_TEST_INTERRUPT" ]; then
		if [ "$LOOPFORGE_TEST_INTERRUPT" = after ]; then
			"$git" "$@" || exit $?
		fi
		exit 42
	fi
done`)
			if err := prepareNativeLeaseTest(t, p, g, l, nil); err == nil {
				t.Fatal("expected checkout to be interrupted")
			}
			t.Setenv("LOOPFORGE_TEST_INTERRUPT", "")
			p = restartNativeLeaseTestPool(t, p)
			d := nativeReturnTestDaemon(p, l)
			commit := nativeReturnTestProof(t, d, testNativeReturnRequest(l, g.repo))
			if len(commit.WarmWorkspaceRefs) != 0 || commit.WarmWorkspaceReturnID == "" {
				t.Fatal("interrupted checkout did not produce an unadmitted receipt")
			}
			if ack := d.warmWorkspaceControl(t.Context(), commit); ack.Error != "" {
				t.Fatal(ack.Error)
			}
			if !atTarget {
				want = before
			}
			if ref := runGitT(t, g.checkout, "rev-parse", "HEAD"); ref != want {
				t.Fatalf("return touched the wrong revision: %s", ref)
			}
		})
	}
}

func TestWarmNativeCheckoutRechecksPreviouslyPreparedSlots(t *testing.T) {
	for _, mode := range []string{"hidden tracked edit", "unity lock"} {
		t.Run(mode, func(t *testing.T) {
			p, a, b, l := twoNativeReturnTestSlots(t)
			b.repo.Ref = "missing"
			prepare := func() error {
				return p.prepareLeaseCheckout(t.Context(), l, &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{a.repo, b.repo}})
			}
			if err := prepare(); err == nil || p.state.Leases[l.OwnerRunID].PreparedRefs["a"].Commit == "" {
				t.Fatalf("expected partial two-slot preparation: %v", err)
			}
			want := errWarmTreeChanged
			if mode == "unity lock" {
				warmGitWrite(t, a.checkout, "Temp/UnityLockfile", "Editor")
				want = errNativeWarmCheckoutBusy
			} else {
				runGitT(t, a.checkout, "update-index", "--assume-unchanged", "README.md")
				warmGitWrite(t, a.checkout, "README.md", "not part of prepared ref")
			}
			b.repo.Ref = "master"
			if err := prepare(); !errors.Is(err, want) {
				t.Fatalf("reused invalid prepared slot: got %v, want %v", err, want)
			}
			if p.state.Leases[l.OwnerRunID].GitReady {
				t.Fatal("admitted partially prepared group")
			}
		})
	}
}

func TestWarmNativeReturnAfterCheckoutBeforeReady(t *testing.T) {
	p, g := openNativeLeaseTestPool(t)
	l := placeNativeLeaseTest(t, p, "first", 1)
	if err := prepareNativeLeaseTest(t, p, g, l, nil); err != nil {
		t.Fatal(err)
	}
	a := p.state.Leases[l.OwnerRunID]
	a.GitReady = false
	if err := p.saveAllocation(a); err != nil {
		t.Fatal(err)
	}
	d := nativeReturnTestDaemon(p, l)
	commit := nativeReturnTestProof(t, d, testNativeReturnRequest(l, g.repo))
	if len(commit.WarmWorkspaceRefs) != 0 {
		t.Fatal("published recovery refs for a lease that never became ready")
	}
	if ack := d.warmWorkspaceControl(t.Context(), commit); ack.Error != "" {
		t.Fatal(ack.Error)
	}
}
