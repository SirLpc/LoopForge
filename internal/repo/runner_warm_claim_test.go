package repo

import (
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestDedicatedWarmRunnerClaimsOnlyTargetedLeases(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	now := time.Now().UTC()
	if err := s.PutRunner(domain.Runner{ID: "warm", RunnerVersion: domain.RunnerVersionWarmWorkspace}); err != nil {
		t.Fatal(err)
	}
	providers := []string{"terminal"}
	add := func(id, runner string, lease *domain.WarmWorkspaceLease) {
		t.Helper()
		if err := s.PutRunnerTask(domain.RunnerTask{
			ID: id, RunnerID: runner, Provider: providers[0], CreatedAt: now,
			WarmWorkspaceLease: lease,
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("ordinary", "", nil)
	add("wrong-mode", "warm", nil)
	add("elsewhere", "another-runner", &domain.WarmWorkspaceLease{Generation: 1})
	if found, err := s.RunnerWarmTaskAvailable("warm", providers); err != nil || found {
		t.Fatalf("dedicated Runner preflight for unrelated work: %t, %v", found, err)
	}
	if task, found, err := s.ClaimRunnerWarmTask("warm", providers, now, time.Minute); err != nil || found {
		t.Fatalf("dedicated Runner claim for unrelated work: %s, %t, %v", task.ID, found, err)
	}
	add("leased", "warm", &domain.WarmWorkspaceLease{RunnerID: "warm", Generation: 1})
	if found, err := s.RunnerWarmTaskAvailable("warm", providers); err != nil || !found {
		t.Fatalf("dedicated Runner preflight for leased work: %t, %v", found, err)
	}
	if task, found, err := s.ClaimRunnerWarmTask("warm", providers, now, time.Minute); err != nil || !found || task.ID != "leased" {
		t.Fatalf("dedicated Runner claim for leased work: %s, %t, %v", task.ID, found, err)
	}
	if task, found, err := s.ClaimRunnerTask("ordinary-machine", providers, now, time.Minute, false); err != nil || !found || task.ID != "ordinary" {
		t.Fatalf("ordinary Runner claim: %s, %t, %v", task.ID, found, err)
	}
}
