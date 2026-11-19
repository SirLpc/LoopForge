package repo

import (
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

func warmQueueRunner(t *testing.T, s *Store, id string) time.Time {
	t.Helper()
	now := time.Now().UTC()
	if err := s.PutRunner(domain.Runner{ID: id, RunnerVersion: domain.RunnerVersionWarmWorkspace}); err != nil {
		t.Fatal(err)
	}
	slots := []domain.WarmWorkspaceSlot{{ID: "a", ProjectID: "project", RepositoryID: "a", State: "available"}, {ID: "b", ProjectID: "project", RepositoryID: "b", State: "available"}}
	if err := s.RunnerHeartbeat(id, nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), slots); err != nil {
		t.Fatal(err)
	}
	return now
}

func warmDemand(t *testing.T, s *Store, now time.Time, machine, owner string, ids ...string) {
	t.Helper()
	if err := s.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: owner, RunnerID: machine, ProjectID: "project", RepositoryIDs: ids}, now); err != nil {
		t.Fatal(err)
	}
}

func TestWarmWorkspaceQueueOldestEligible(t *testing.T) {
	s := newTestRunner(t)
	now := warmQueueRunner(t, s, "machine")
	warmDemand(t, s, now, "machine", "too-large", "a", "missing")
	warmDemand(t, s, now.Add(time.Second), "machine", "conversation", "a", "b")
	warmDemand(t, s, now.Add(2*time.Second), "machine", "task", "a")
	lease, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if err != nil || !ok || lease.OwnerRunID != "conversation" || len(lease.Slots) != 2 {
		t.Fatalf("reservation %+v %t %v", lease, ok, err)
	}
	warmDemand(t, s, now, "machine", "conversation", "a", "b")
	if _, ok, err := s.ReserveNextWarmWorkspace("machine", now); ok || err != nil {
		t.Fatalf("double reservation %t %v", ok, err)
	}
	if err := s.CancelWarmWorkspace("conversation", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleWarmWorkspace(lease, domain.WarmWorkspaceReady); err == nil {
		t.Fatal("canceled preparation acknowledged as ready")
	}
	got, state, ok, err := s.WarmWorkspaceAllocation("conversation")
	if err != nil || !ok || state != domain.WarmWorkspaceReleasing || got.Generation != lease.Generation {
		t.Fatalf("late ready %+v %s %v", got, state, err)
	}
	if _, ok, err := s.ReserveNextWarmWorkspace("machine", now); ok || err != nil {
		t.Fatalf("early reuse %t %v", ok, err)
	}
	if err := s.SettleWarmWorkspace(lease, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	next, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if err != nil || !ok || next.OwnerRunID != "task" || next.Generation <= lease.Generation {
		t.Fatalf("next %+v %v", next, err)
	}
}

func TestWarmWorkspaceQueueMySQLConcurrentCancel(t *testing.T) {
	stores, id := admissionMySQLStores(t)
	now := warmQueueRunner(t, stores[0], id)
	t.Cleanup(func() {
		for _, result := range []error{stores[0].DB.Where("runner_id = ?", id).Delete(&warmWorkspaceRow{}).Error, stores[0].DB.Where("id = ?", id).Delete(&runnerRow{}).Error} {
			if result != nil {
				t.Error(result)
			}
		}
	})
	warmDemand(t, stores[0], now, id, id+"-1", "a")
	warmDemand(t, stores[0], now, id, id+"-2", "a")
	start := make(chan struct{})
	type result struct {
		lease domain.WarmWorkspaceLease
		ok    bool
		err   error
	}
	results := make(chan result, 2)
	for _, s := range stores {
		go func() {
			<-start
			lease, ok, err := s.ReserveNextWarmWorkspace(id, now)
			results <- result{lease, ok, err}
		}()
	}
	close(start)
	var leases []domain.WarmWorkspaceLease
	for range stores {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.ok {
			leases = append(leases, r.lease)
		}
	}
	if len(leases) != 1 {
		t.Fatalf("reservations %+v", leases)
	}
	l := leases[0]
	if err := stores[1].CancelWarmWorkspace(l.OwnerRunID, now); err != nil {
		t.Fatal(err)
	}
	if err := stores[0].SettleWarmWorkspace(l, domain.WarmWorkspaceReady); err == nil {
		t.Fatal("canceled preparation acknowledged as ready")
	}
	if _, state, _, err := stores[1].WarmWorkspaceAllocation(l.OwnerRunID); err != nil || state != domain.WarmWorkspaceReleasing {
		t.Fatalf("late ready %s %v", state, err)
	}
	if err := stores[0].SettleWarmWorkspace(l, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	// Two control instances observe the same released generation and race
	// to reacquire. Only one may advance the queue epoch.
	reacquire := make(chan error, 2)
	for _, store := range stores {
		go func() { reacquire <- store.RequeueWarmWorkspace(l, now) }()
	}
	successes := 0
	for range stores {
		if err := <-reacquire; err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent requeues succeeded %d times", successes)
	}
	pending, state, ok, err := stores[1].WarmWorkspaceAllocation(l.OwnerRunID)
	if err != nil || !ok || state != domain.WarmWorkspaceQueued || pending.Epoch != l.Epoch+1 {
		t.Fatalf("reacquired epoch %+v %s %v", pending, state, err)
	}

}

func TestWarmWorkspaceQueueInventoryAndGenerationSurviveReconnect(t *testing.T) {
	s := newTestRunner(t)
	now := warmQueueRunner(t, s, "machine")
	warmDemand(t, s, now, "machine", "first", "a")
	l, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if err != nil || !ok {
		t.Fatal(err)
	}
	if err := s.CancelWarmWorkspace("first", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	if err := s.DeregisterRunner("machine", now); err != nil {
		t.Fatal(err)
	}
	warmQueueRunner(t, s, "machine")
	warmDemand(t, s, now, "machine", "second", "a")
	next, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if err != nil || !ok || next.Generation <= l.Generation {
		t.Fatalf("generation after re-register %+v %v", next, err)
	}
	machine, ok := s.Runner("machine")
	if !ok || len(machine.WarmWorkspaceSlots) != 2 {
		t.Fatal("missing inventory")
	}
	slots := []domain.WarmWorkspaceSlot{{ID: "same", ProjectID: "project", RepositoryID: "a", State: "available"}, {ID: "same", ProjectID: "project", RepositoryID: "b", State: "available"}}
	if err := s.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), slots); err == nil {
		t.Fatal("accepted duplicate physical slot")
	}
}

func TestWarmWorkspaceQueueReacquire(t *testing.T) {
	s := newTestRunner(t)
	now := warmQueueRunner(t, s, "machine")
	warmDemand(t, s, now, "machine", "owner", "a")
	first, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	if err := s.CancelWarmWorkspace("owner", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleWarmWorkspace(first, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueWarmWorkspace(first, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueWarmWorkspace(first, now); err == nil {
		t.Fatal("stale reacquire succeeded")
	}
	if err := s.CancelWarmWorkspace("owner", now); err != nil {
		t.Fatal(err)
	}
	next, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if err != nil || !ok || next.Generation <= first.Generation || next.Epoch <= first.Epoch {
		t.Fatalf("new assignment %+v %v", next, err)
	}
	if err := s.SettleWarmWorkspace(first, domain.WarmWorkspaceReady); err == nil {
		t.Fatal("old generation changed new assignment")
	}
	warmDemand(t, s, now, "machine", "canceled", "b")
	if err := s.CancelWarmWorkspace("canceled", now); err != nil {
		t.Fatal(err)
	}
	canceled, _, _, err := s.WarmWorkspaceAllocation("canceled")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueWarmWorkspace(canceled, now); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueWarmWorkspace(canceled, now); err == nil {
		t.Fatal("stale canceled demand resurrected")
	}
}
func TestWarmWorkspaceQueueUsesNewRunnerJournalFloor(t *testing.T) {
	s := newTestRunner(t)
	now := warmQueueRunner(t, s, "new")
	slots := []domain.WarmWorkspaceSlot{{ID: "a", ProjectID: "project", RepositoryID: "a", State: "available", Generation: 7}, {ID: "b", ProjectID: "project", RepositoryID: "b", State: "reserved", Generation: 10}}
	if err := s.RunnerHeartbeat("new", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), slots); err != nil {
		t.Fatal(err)
	}
	warmDemand(t, s, now, "new", "next", "a")
	next, ok, err := s.ReserveNextWarmWorkspace("new", now)
	if err != nil || !ok || next.Generation <= 10 {
		t.Fatalf("reservation used old journal generation %+v %v", next, err)
	}
}

func TestWarmGrantRequiresFrozenOriginAfterRebind(t *testing.T) {
	s := newTestRunner(t)
	now := warmQueueRunner(t, s, "machine")
	a := domain.RepositoryContext{RepositoryID: "code", CloneURL: "https://git.example.org/team/a.git"}
	b := a
	b.CloneURL = "https://git.example.org/team/b.git"
	old := domain.Run{ID: "old", Status: "running", Repository: a, ExecutionContext: domain.TelemetryContext{ProjectID: "project", ExecutionRunnerID: "machine", WorkspaceMode: domain.WorkspaceModeWarmPool}}
	if err := s.PutRun(old); err != nil {
		t.Fatal(err)
	}
	slots := []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "project", RepositoryID: "code", State: "available", OriginSHA256: gitwork.RemoteFingerprint(a.CloneURL)}}
	if err := s.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), slots); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Runner("machine")
	if CheckWarmWorkspaceRegistration(m, "project", []domain.RepositoryContext{b}) == nil {
		t.Error("new origin selected old seed heartbeat")
	}
	m.WarmWorkspaceSlots[0].OriginSHA256 = ""
	if CheckWarmWorkspaceRegistration(m, "project", []domain.RepositoryContext{a}) == nil {
		t.Fatal("digest-less heartbeat accepted for a frozen origin")
	}
	warmDemand(t, s, now, "machine", "old", "code")
	slots[0].OriginSHA256 = gitwork.RemoteFingerprint(b.CloneURL)
	if err := s.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), slots); err != nil {
		t.Fatal(err)
	}
	fresh := old
	fresh.ID = "new"
	fresh.Repository = b
	if err := s.PutRun(fresh); err != nil {
		t.Fatal(err)
	}
	warmDemand(t, s, now.Add(time.Second), "machine", "new", "code")
	l, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if err != nil || !ok || l.OwnerRunID != "new" {
		t.Errorf("rebound seed granted to %q: %v", l.OwnerRunID, err)
	}
	if err = s.CheckQueuedWarmTarget(old, domain.WarmWorkspaceLease{OwnerRunID: "old", Epoch: 1}); err == nil {
		t.Error("old queued owner considered compatible")
	}
}
