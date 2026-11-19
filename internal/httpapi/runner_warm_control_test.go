package httpapi

import (
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
)

func TestWarmWorkspaceControlReconnect(t *testing.T) {
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	const id = "warm-control"
	if err := s.PutRunner(domain.Runner{ID: id, RunnerVersion: domain.RunnerVersionWarmWorkspace}); err != nil {
		t.Fatal(err)
	}
	if err := s.RunnerHeartbeat(id, nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "a", ProjectID: "p", RepositoryID: "repo", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{RunnerID: id, OwnerRunID: "owner", ProjectID: "p", RepositoryIDs: []string{"repo"}}, now); err != nil {
		t.Fatal(err)
	}
	l, ok, err := s.ReserveNextWarmWorkspace(id, now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	hub := quietRunnerHub()
	run := domain.Run{ID: "owner", Status: "running", ExecutionContext: domain.TelemetryContext{ProjectID: "p", ExecutionMode: domain.ExecutionModeRunner, ExecutionRunnerID: id, WorkspaceMode: domain.WorkspaceModeWarmPool}, Repositories: []domain.RepositoryContext{{RepositoryID: "repo", WorkspaceDir: "trusted", BaseBranch: "main"}}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	hub.authorizeWarm = engine.New(s, nil).AuthorizeWarmWorkspaceControl
	req := domain.RunnerWorkspaceRequest{RunID: "owner", Operation: domain.RunnerWorkspaceOperationWarmStatus, WarmWorkspaceLease: &l}
	if _, err := hub.requestWorkspace(t.Context(), id, req); err == nil {
		t.Fatal("offline request succeeded")
	}
	conn := testRunnerConn(id)
	registerTestConn(hub, conn)
	done := requestAsync(hub, id, req)
	got := answerOnce(t, hub, conn, domain.RunnerWorkspaceResponse{})
	if result := <-done; result.err != nil || got.WarmWorkspaceLease.Generation != l.Generation {
		t.Fatalf("reconnected %+v %v", got, result.err)
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmPrepare
	req.Repositories = []domain.RepositoryContext{{RepositoryID: "repo", WorkspaceDir: "spoof", BaseBranch: "spoof"}}
	doneRelay := make(chan error, 1)
	go func() { _, err := hub.requestRelayed(t.Context(), id, req); doneRelay <- err }()
	got = answerOnce(t, hub, conn, domain.RunnerWorkspaceResponse{})
	if err := <-doneRelay; err != nil || len(got.Repositories) != 1 || got.Repositories[0] != run.Repositories[0] {
		t.Fatalf("untrusted relay fields %+v %v", got, err)
	}
	// A second run freezes the same repository ID in another project. Its
	// malformed demand reserved project p; the mismatch must never hit the socket.
	if err := s.CancelWarmWorkspace("owner", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	run.ID = "other-owner"
	run.ExecutionContext.ProjectID = "other-project"
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{RunnerID: id, OwnerRunID: run.ID, ProjectID: "p", RepositoryIDs: []string{"repo"}}, now); err != nil {
		t.Fatal(err)
	}
	other, ok, err := s.ReserveNextWarmWorkspace(id, now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	otherReq := req
	otherReq.RunID = other.OwnerRunID
	otherReq.WarmWorkspaceLease = &other
	if _, err := hub.requestRelayed(t.Context(), id, otherReq); err == nil || len(conn.send) != 0 {
		t.Fatalf("cross-project relay reached runner: %v", err)
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmStatus
	l.Generation++
	if _, err := hub.requestRelayed(t.Context(), id, req); err == nil {
		t.Fatal("relay accepted stale lease")
	}
}
