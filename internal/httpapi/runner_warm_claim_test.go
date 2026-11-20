package httpapi

import (
	"net/http"
	"testing"

	"agentflow/internal/domain"
)

func TestDedicatedWarmClaimEndpointLeavesCheckoutWorkQueued(t *testing.T) {
	f := cvBNewHTTPFixture(t)
	id, token := registerTestRunner(t, f.h)
	machine, ok := f.s.Runner(id)
	if !ok {
		t.Fatal("registered Runner missing")
	}
	machine.RunnerVersion = domain.RunnerVersionWarmWorkspace
	if err := f.s.PutRunner(machine); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{"availableRuntimes": []string{"terminal"}, "capacity": 1, "noWait": true}
	if rec := requestWithHeaders(t, f.h, http.MethodPost, domain.WarmWorkspaceClaimPath, input, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated claim returned %d", rec.Code)
	}
	ordinary := domain.RunnerTask{ID: "ordinary", Provider: "terminal"}
	if err := f.s.PutRunnerTask(ordinary); err != nil {
		t.Fatal(err)
	}
	headers := map[string]string{"Authorization": "Bearer " + token}
	if rec := requestWithHeaders(t, f.h, http.MethodPost, domain.WarmWorkspaceClaimPath, input, headers); rec.Code != http.StatusNoContent {
		t.Fatalf("warm claim returned %d: %s", rec.Code, rec.Body.String())
	}
	warm := domain.RunnerTask{ID: "warm", RunnerID: id, Provider: "terminal", WarmWorkspaceLease: &domain.WarmWorkspaceLease{RunnerID: id}}
	if err := f.s.PutRunnerTask(warm); err != nil {
		t.Fatal(err)
	}
	if rec := requestWithHeaders(t, f.h, http.MethodPost, domain.WarmWorkspaceClaimPath, input, headers); rec.Code != http.StatusOK {
		t.Fatalf("warm claim returned %d: %s", rec.Code, rec.Body.String())
	}
	if row, ok := f.s.RunnerTask(warm.ID); !ok || row.LeaseOwner != id {
		t.Fatalf("targeted warm task was not leased: %+v", row)
	}
	if row, ok := f.s.RunnerTask(ordinary.ID); !ok || row.Status != domain.RunnerTaskQueued {
		t.Fatalf("ordinary task was modified: %+v", row)
	}
}
