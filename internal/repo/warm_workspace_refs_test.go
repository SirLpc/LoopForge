package repo

import (
	"agentflow/internal/domain"
	"testing"
	"time"
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

func TestWarmNativeReturnReceiptKeepsConfirmedRefs(t *testing.T) {
	s := newTestRunner(t)
	now := warmQueueRunner(t, s, "machine")
	run := domain.Run{ID: "native-owner", Status: "running",
		ExecutionContext: domain.TelemetryContext{ProjectID: "project", ExecutionRunnerID: "machine", WorkspaceMode: domain.WorkspaceModeWarmPool},
		Repositories:     []domain.RepositoryContext{{RepositoryID: "a"}}}
	run.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: domain.SandboxDriverNative}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	warmDemand(t, s, now, "machine", run.ID, "a")
	reserve := func() domain.WarmWorkspaceLease {
		t.Helper()
		l, ok, err := s.ReserveNextWarmWorkspace("machine", now)
		if err != nil || !ok {
			t.Fatalf("reserve native workspace: %v", err)
		}
		return l
	}
	state := func(l domain.WarmWorkspaceLease, state domain.WarmWorkspaceState) {
		t.Helper()
		if err := s.SettleWarmWorkspace(l, state); err != nil {
			t.Fatal(err)
		}
	}
	old := []domain.WarmWorkspaceRef{{RepositoryID: "a", Commit: "0123456789012345678901234567890123456789"}}
	newRefs := []domain.WarmWorkspaceRef{{RepositoryID: "a", Commit: "1123456789012345678901234567890123456789"}}
	l := reserve()
	state(l, domain.WarmWorkspaceReady)
	state(l, domain.WarmWorkspaceReleasing)
	if err := s.RecordWarmWorkspaceRefs(run.ID, l, old); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmWarmWorkspaceBinding(l, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	if err := s.RequeueWarmWorkspace(l, now); err != nil {
		t.Fatal(err)
	}
	l = reserve()
	state(l, domain.WarmWorkspaceReleasing)
	token, err := s.BeginWorkspaceCheckpointSave(run.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := func() {
		t.Helper()
		refs, err := s.WarmWorkspaceRefs(run.ID)
		if err != nil || len(refs) != 1 || refs[0].Commit != old[0].Commit {
			t.Fatalf("lost previous confirmed ref: %v %v", refs, err)
		}
	}
	if err := s.RecordNativeWarmWorkspaceReturn(l, token, "receipt-a", newRefs, now); err != nil {
		t.Fatal(err)
	}
	confirmed()
	if err := s.InvalidateNativeWarmWorkspaceReturn(l, token, "receipt-a", now); err != nil {
		t.Fatal(err)
	}
	confirmed()
	if err := s.RecordNativeWarmWorkspaceReturn(l, token, "receipt-b", nil, now); err != nil {
		t.Fatal(err)
	}
	if err := s.ConfirmNativeWarmWorkspaceReturn(l, token, "receipt-b", now); err != nil {
		t.Fatal(err)
	}
	confirmed()
	if ok, err := s.WarmWorkspaceRestorable(run.ID); err != nil || !ok {
		t.Fatalf("unadmitted return lost earlier restore refs: %v", err)
	}
	if err := s.RequeueWarmWorkspace(l, now); err != nil {
		t.Fatal(err)
	}
	l = reserve()
	state(l, domain.WarmWorkspaceReleasing)
	if err := s.RecordWarmWorkspaceRefs(run.ID, l, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateNativeWarmWorkspaceReturn(l, token, "", now); err != nil {
		t.Fatal(err)
	}
	confirmed()
	if id, found, err := s.WarmWorkspaceReturnReceipt(l); err != nil || found {
		t.Fatalf("legacy receipt still active: %q %v %v", id, found, err)
	}
}

func TestWarmNativeReturnRejectsStaleReceipt(t *testing.T) {
	s := newTestRunner(t)
	now := warmQueueRunner(t, s, "machine")
	run := domain.Run{ID: "native-owner", Status: "running",
		ExecutionContext: domain.TelemetryContext{ProjectID: "project", WorkspaceMode: domain.WorkspaceModeWarmPool},
		Repositories:     []domain.RepositoryContext{{RepositoryID: "a"}}}
	run.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: domain.SandboxDriverNative}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	warmDemand(t, s, now, "machine", run.ID, "a")
	l, ok, err := s.ReserveNextWarmWorkspace("machine", now)
	if err != nil || !ok {
		t.Fatalf("reserve native workspace: %v", err)
	}
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReady); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReleasing); err != nil {
		t.Fatal(err)
	}
	token, err := s.BeginWorkspaceCheckpointSave(run.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	req := domain.RunnerWorkspaceRequest{RunID: run.ID, Operation: domain.RunnerWorkspaceOperationWarmReleaseCommit,
		WarmWorkspaceLease: &l, WarmWorkspaceReturnID: "caller-value"}
	if err := s.AuthorizeWarmWorkspaceControl("machine", run, &req); err == nil {
		t.Fatal("authorized a commit before a proof was durable")
	}
	refs := []domain.WarmWorkspaceRef{{RepositoryID: "a", Commit: "0123456789012345678901234567890123456789"}}
	if err := s.RecordNativeWarmWorkspaceReturn(l, token, "receipt-a", refs, now); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateNativeWarmWorkspaceReturn(l, token, "receipt-a", now.Add(16*time.Minute)); err == nil {
		t.Fatal("expired token invalidated proof")
	}
	stale := l
	stale.Generation++
	if err := s.InvalidateNativeWarmWorkspaceReturn(stale, token, "receipt-a", now); err == nil {
		t.Fatal("stale lease invalidated proof")
	}
	if err := s.InvalidateNativeWarmWorkspaceReturn(l, token, "receipt-a", now); err != nil {
		t.Fatal(err)
	}
	if _, state, _, _ := s.WarmWorkspaceAllocation(run.ID); state != domain.WarmWorkspaceReady {
		t.Fatalf("invalidated ready lease = %s", state)
	}
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReleasing); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordNativeWarmWorkspaceReturn(l, token, "receipt-b", refs, now); err != nil {
		t.Fatal(err)
	}
	if err := s.InvalidateNativeWarmWorkspaceReturn(l, token, "receipt-a", now); err == nil {
		t.Fatal("old ID invalidated replacement proof with the same refs")
	}
	if err := s.AuthorizeWarmWorkspaceControl("machine", run, &req); err != nil || req.WarmWorkspaceReturnID != "receipt-b" || len(req.WarmWorkspaceRefs) != 1 {
		t.Fatalf("failed to canonicalize receipt: %+v %v", req, err)
	}
	if err := s.ConfirmNativeWarmWorkspaceReturn(l, token, "receipt-b", now); err != nil {
		t.Fatal(err)
	}
}
