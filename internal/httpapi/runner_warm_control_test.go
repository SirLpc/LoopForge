package httpapi

import (
	"testing"
	"time"

	"agentflow/internal/domain"
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
	hub.authorizeWarm = s.AuthorizeWarmWorkspaceControl
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
	l.Generation++
	if _, err := hub.requestRelayed(t.Context(), id, req); err == nil {
		t.Fatal("relay accepted stale lease")
	}
}
