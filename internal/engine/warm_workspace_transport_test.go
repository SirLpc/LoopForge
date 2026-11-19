package engine

import (
	"context"
	"testing"

	"agentflow/internal/domain"
)

func TestWarmWorkspaceControlDirectTransportChecksReservation(t *testing.T) {
	e, _ := cvEEngine(t)
	run := cvERunnerRun("owner", "machine")
	run.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	if err := e.repo.PutRun(run); err != nil {
		t.Fatal(err)
	}
	called := false
	e.WorkspaceCheckpointTransport = func(context.Context, domain.Run, domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		called = true
		return domain.RunnerWorkspaceResponse{}, nil
	}
	req := domain.RunnerWorkspaceRequest{Operation: domain.RunnerWorkspaceOperationWarmPrepare, RunID: "owner", WarmWorkspaceLease: &domain.WarmWorkspaceLease{OwnerRunID: "owner", RunnerID: "machine", Generation: 1, Slots: map[string]string{"repo": "slot"}}}
	if _, err := e.requestWorkspaceCheckpoint(t.Context(), run, req); err == nil || called {
		t.Fatalf("unreserved RPC dispatched %v, error %v", called, err)
	}
}
