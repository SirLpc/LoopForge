package repo

import (
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestWarmWorkspaceControlUsesTrustedOwner(t *testing.T) {
	s := newTestRunner(t)
	now := warmQueueRunner(t, s, "machine")
	warmDemand(t, s, now, "machine", "owner", "a")
	l, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	run := domain.Run{ID: "owner", ExecutionContext: domain.TelemetryContext{ProjectID: "project"}, Repositories: []domain.RepositoryContext{{RepositoryID: "a"}}}
	req := domain.RunnerWorkspaceRequest{Operation: domain.RunnerWorkspaceOperationWarmPrepare, RunID: "owner", WarmWorkspaceLease: &l}
	if err := s.AuthorizeWarmWorkspaceControl("machine", run, &req); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*domain.WarmWorkspaceLease){
		func(l *domain.WarmWorkspaceLease) { l.OwnerRunID = "other" },
		func(l *domain.WarmWorkspaceLease) { l.RunnerID = "another" },
		func(l *domain.WarmWorkspaceLease) { l.Generation++ },
		func(l *domain.WarmWorkspaceLease) { l.Slots = map[string]string{"a": "b"} },
	} {
		bad := l
		change(&bad)
		req.WarmWorkspaceLease = &bad
		if err := s.AuthorizeWarmWorkspaceControl("machine", run, &req); err == nil {
			t.Fatalf("authorized %+v", bad)
		}
	}
	req.WarmWorkspaceLease = &l
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReady); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthorizeWarmWorkspaceControl("machine", run, &req); err == nil {
		t.Fatal("reopened ready lease")
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmStatus
	if err := s.RunnerHeartbeat("machine", nil, "0.11.0", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.AuthorizeWarmWorkspaceControl("machine", run, &req); err == nil {
		t.Fatal("authorized old runner")
	}
}
