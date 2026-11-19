package repo

import (
	"testing"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

func TestWarmWorkspaceCancellationAndClaimFences(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	r := domain.Run{ID: "owner", Status: "running", ExecutionContext: domain.TelemetryContext{WorkspaceMode: domain.WorkspaceModeWarmPool}}
	d := domain.WarmWorkspaceDemand{OwnerRunID: r.ID, RunnerID: "machine", ProjectID: "p", RepositoryIDs: []string{"repo"}}
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestRunCancel(r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueWarmWorkspace(d, now); err == nil {
		t.Error("late enqueue after durable cancel")
	}
	if err := s.ClearRunControl(r.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueWarmWorkspace(d, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestRunCancel(r.ID); err != nil {
		t.Fatal(err)
	}
	if _, state, _, err := s.WarmWorkspaceAllocation(r.ID); err != nil || state != domain.WarmWorkspaceCanceled {
		t.Fatalf("demand survived durable cancellation: %s, %v", state, err)
	}
}

func TestWarmWorkspaceOldRunnerCannotClaimQueuedTask(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.PutRunner(domain.Runner{ID: "machine", RunnerVersion: "0.11.0"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a-warm", "b-normal"} {
		task := domain.RunnerTask{ID: id, RunnerID: "machine", Provider: "terminal", Status: domain.RunnerTaskQueued, CreatedAt: now}
		if id == "a-warm" {
			task.WarmWorkspaceLease = &domain.WarmWorkspaceLease{Generation: 1}
		}
		if err := s.PutRunnerTask(task); err != nil {
			t.Fatal(err)
		}
	}
	result, ok, err := s.ClaimRunnerTask("machine", []string{"terminal"}, now, time.Minute, true)
	if err != nil || !ok || result.ID != "b-normal" {
		t.Fatal("downgraded runner claimed", result.ID, err)
	}
}

func TestWorkspaceModeReadRequiresAnObjectWithKnownMode(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, tc := range []struct {
		id, payload string
		valid       bool
	}{
		{"legacy", "{}", true}, {"null", "null", false}, {"invalid", `{"workspaceMode":{"invalid":true}}`, false}, {"unknown", `{"workspaceMode":"future"}`, false},
	} {
		if err := s.DB.Create(&projectRow{ID: tc.id, ProjectKey: tc.id, Payload: []byte(tc.payload)}).Error; err != nil {
			t.Fatal(err)
		}
		mode, err := s.WorkspaceMode(tc.id)
		if (err == nil) != tc.valid || tc.valid && mode != domain.WorkspaceModeCheckout {
			t.Errorf("%s: mode=%q err=%v", tc.id, mode, err)
		}
	}
	if mode, err := s.WorkspaceMode(""); err != nil || mode != domain.WorkspaceModeCheckout {
		t.Fatalf("projectless mode %q: %v", mode, err)
	}
}

func TestWarmWorkspaceMySQLConcurrentRunCancelAndEnqueue(t *testing.T) {
	stores, owner := admissionMySQLStores(t)
	admissionMySQLCleanup(t, stores[0], []string{owner})
	t.Cleanup(func() {
		if err := stores[0].DB.Where("owner_run_id = ?", owner).Delete(&warmWorkspaceRow{}).Error; err != nil {
			t.Error(err)
		}
	})
	now := time.Now()
	run := admissionMySQLRun(owner, owner)
	run.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	if err := stores[0].PutRun(run); err != nil {
		t.Fatal(err)
	}
	started, release := make(chan struct{}), make(chan struct{})
	// Hold the enqueue transaction after its owner-row SELECT FOR UPDATE.
	if err := stores[0].DB.Callback().Query().After("gorm:query").Register("test:hold-warm-owner", func(tx *gorm.DB) {
		if tx.Statement.Table == (runRow{}).TableName() {
			close(started)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	enqueued := make(chan error, 1)
	go func() {
		enqueued <- stores[0].EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: owner, RunnerID: owner, ProjectID: owner, RepositoryIDs: []string{"repo"}}, now)
	}()
	select {
	case <-started:
	case err := <-enqueued:
		t.Fatalf("enqueue skipped owner lock: %v", err)
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("no owner lock")
	}
	canceled := make(chan error, 1)
	go func() { _, err := stores[1].RequestRunCancel(owner); canceled <- err }()
	select {
	case err := <-canceled:
		close(release)
		t.Fatalf("cancel completed while enqueue held owner lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-enqueued; err != nil {
		t.Fatal(err)
	}
	if err := <-canceled; err != nil {
		t.Fatal(err)
	}
	if _, state, found, err := stores[1].WarmWorkspaceAllocation(owner); err != nil || !found || state != domain.WarmWorkspaceCanceled {
		t.Fatalf("racing demand: %v %s %v", found, state, err)
	}
}
