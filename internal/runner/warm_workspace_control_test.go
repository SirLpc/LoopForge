package runner

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

func TestWarmWorkspaceControlPlacementIsNotReady(t *testing.T) {
	pool := newWarmPoolFixture(t).open(t)
	d := Daemon{warmWorkspacePool: pool, state: daemonState{RunnerID: "machine"}}
	l := domain.WarmWorkspaceLease{OwnerRunID: "owner", RunnerID: "machine", Generation: 4, Slots: map[string]string{"a": "a"}}
	req := domain.RunnerWorkspaceRequest{RunID: "owner", Operation: domain.RunnerWorkspaceOperationWarmPrepare, WarmWorkspaceLease: &l, Repositories: []domain.RepositoryContext{{RepositoryID: "a", WorkspaceDir: "checkout"}}}
	for _, op := range []string{req.Operation, domain.RunnerWorkspaceOperationWarmStatus, req.Operation} {
		req.Operation = op
		got := d.readRunnerWorkspace(t.Context(), req)
		var result struct{ Phase string }
		if got.Error != "" || json.Unmarshal(got.Payload, &result) != nil || result.Phase != "placed" {
			t.Fatalf("%s: %+v", op, got)
		}
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmReleaseCommit
	if got := d.readRunnerWorkspace(t.Context(), req); got.Error == "" {
		t.Fatal("release without proof")
	}
	for _, slot := range pool.inventory() {
		if slot.ID == "a" && slot.State == "available" {
			t.Fatal("active slot advertised as available")
		}
	}
}

func TestWarmWorkspaceControlPrepareRespectsOwnerGate(t *testing.T) {
	p := newWarmPoolFixture(t).open(t)
	d := Daemon{warmWorkspacePool: p, state: daemonState{RunnerID: "new"}}
	req := domain.RunnerWorkspaceRequest{RunID: "owner", Operation: domain.RunnerWorkspaceOperationWarmPrepare, WarmWorkspaceLease: &domain.WarmWorkspaceLease{OwnerRunID: "owner", RunnerID: "new", Generation: 4, Slots: map[string]string{"a": "a"}}, Repositories: []domain.RepositoryContext{{RepositoryID: "a", WorkspaceDir: "safe"}}}
	gate := d.workspaceGate("owner")
	gate.RLock()
	got := d.readRunnerWorkspace(t.Context(), req)
	gate.RUnlock()
	if got.Code != domain.WorkspaceErrorBusy || len(p.state.Leases) != 0 {
		t.Fatalf("busy owner changed %+v", got)
	}
}

func TestWarmWorkspaceControlFreshRunnerResumesDiskGeneration(t *testing.T) {
	f := newWarmPoolFixture(t)
	f.writeSlots(t, newWarmGitFixture(t).checkout, newWarmGitFixture(t).checkout)
	p := f.open(t)
	released := domain.WarmWorkspaceLease{OwnerRunID: "returned", RunnerID: "former", Generation: 6, Slots: map[string]string{"a": "a"}}
	if err := p.reserve(released); err != nil {
		t.Fatal(err)
	}
	if err := p.returnSlots(t.Context(), released); err != nil {
		t.Fatal(err)
	}
	active := domain.WarmWorkspaceLease{OwnerRunID: "active", RunnerID: "former", Generation: 11, Slots: map[string]string{"b": "b"}}
	if err := p.reserve(active); err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	p = f.open(t)
	store, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const id = "new-registration"
	now := time.Now()
	if err := store.PutRunner(domain.Runner{ID: id}); err != nil {
		t.Fatal(err)
	}
	if err := store.RunnerHeartbeat(id, nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), p.inventory()); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: "new-owner", RunnerID: id, ProjectID: "project", RepositoryIDs: []string{"a"}}, now); err != nil {
		t.Fatal(err)
	}
	next, ok, err := store.ReserveNextWarmWorkspace(id, now)
	if !ok || err != nil || next.Generation <= active.Generation {
		t.Fatalf("journal floor lost %+v %v", next, err)
	}
	if err := p.reserve(next); err != nil {
		t.Fatal(err)
	}
	if got := p.state.Leases["active"]; !sameWarmWorkspaceLease(got.Lease, active) || got.Phase != warmWorkspaceReserved {
		t.Fatalf("old lease changed %+v", got)
	}
}

func TestWarmInventoryQualifiesLiveCheckout(t *testing.T) {
	f := newWarmPoolFixture(t)
	g := newWarmGitFixture(t)
	f.writeSlots(t, g.checkout, f.homeB)
	p := f.open(t)
	slots := p.inventory()
	if slots[0].State != "available" || slots[1].State != "blocked" {
		t.Errorf("initial checkout qualification: %+v", slots)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	runGitT(t, g.checkout, "worktree", "add", "--detach", linked)
	if slot := p.inventory()[0]; slot.State != "blocked" {
		t.Errorf("checkout with linked worktree advertised as %s", slot.State)
	}
	runGitT(t, g.checkout, "worktree", "remove", linked)
	if slot := p.inventory()[0]; slot.State != "available" {
		t.Errorf("checkout stayed %s after linked worktree was removed", slot.State)
	}
	runGitT(t, g.checkout, "remote", "set-url", "origin", "https://user:password@example.invalid/path")
	if p.inventory()[0].State != "blocked" {
		t.Fatal("credentialed idle checkout advertised")
	}
	runGitT(t, g.checkout, "remote", "set-url", "origin", g.repo.CloneURL)
	if p.inventory()[0].State != "available" {
		t.Fatal("repaired idle registration stayed blocked")
	}
	l := domain.WarmWorkspaceLease{OwnerRunID: "owner", RunnerID: "machine", Generation: 4, Epoch: 1, Slots: map[string]string{"a": "a"}}
	if err := p.reserve(l); err != nil {
		t.Fatal(err)
	}
	record := p.state.Slots["a"]
	record.NeedsInspection = true
	p.state.Slots["a"] = record
	if p.inventory()[0].State != "blocked" {
		t.Fatal("inspection-blocked owner advertised as compatible")
	}
}
