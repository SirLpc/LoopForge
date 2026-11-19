package repo

import (
	"agentflow/internal/domain"
	"testing"
)

func TestWarmWorkspaceReleaseRefsBeforeCommit(t *testing.T) {
	s := newTestRunner(t)
	now := warmQueueRunner(t, s, "machine")
	warmDemand(t, s, now, "machine", "owner", "a")
	l, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	if err = s.SettleWarmWorkspace(l, domain.WarmWorkspaceReady); err != nil {
		t.Fatal(err)
	}
	r := domain.Run{ID: "owner", ExecutionContext: domain.TelemetryContext{ProjectID: "project"}, Repositories: []domain.RepositoryContext{{RepositoryID: "a"}}}
	req := domain.RunnerWorkspaceRequest{Operation: domain.RunnerWorkspaceOperationWarmReleaseCommit, RunID: r.ID, WarmWorkspaceLease: &l}
	if err = s.SettleWarmWorkspace(l, domain.WarmWorkspaceReleasing); err != nil {
		t.Fatal(err)
	}
	if err = s.AuthorizeWarmWorkspaceControl("machine", r, &req); err == nil {
		t.Fatal("commit authorized without durable refs")
	}
	refs := []domain.WarmWorkspaceRef{{RepositoryID: "a", Branch: "work", Commit: "0123456789012345678901234567890123456789"}}
	if err = s.RecordWarmWorkspaceRefs(r.ID, l, refs); err != nil {
		t.Fatal(err)
	}
	if err = s.AuthorizeWarmWorkspaceControl("machine", r, &req); err != nil {
		t.Fatal(err)
	}
	refs[0].Branch = "modified"
	if err = s.RecordWarmWorkspaceRefs(r.ID, l, refs); err == nil {
		t.Fatal("overwrote committed proof")
	}
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueWarmWorkspace(l, now); err != nil {
		t.Fatal(err)
	}
	l, ok, err = s.ReserveNextWarmWorkspace("machine", now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReleasing); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordWarmWorkspaceRefs(r.ID, l, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	if yes, err := s.WarmWorkspaceRestorable(r.ID); err != nil || !yes {
		t.Fatalf("lost earlier exact pins: %v %v", yes, err)
	}
	got, err := s.WarmWorkspaceRefs(r.ID)
	if err != nil || len(got) != 1 || got[0].Branch != "work" {
		t.Fatal(got, err)
	}
}
