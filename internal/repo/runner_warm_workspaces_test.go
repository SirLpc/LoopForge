package repo

import (
	"testing"
	"time"

	"agentflow/internal/domain"
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
