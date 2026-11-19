package runner

import (
	"encoding/json"
	"testing"

	"agentflow/internal/domain"
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
