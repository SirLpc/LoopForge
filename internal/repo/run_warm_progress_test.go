package repo

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestWarmWorkspaceWaitingInRunViews(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	run := domain.Run{ID: "queued-owner", Status: "running", ExecutionContext: domain.TelemetryContext{WorkspaceMode: domain.WorkspaceModeWarmPool}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	demand := domain.WarmWorkspaceDemand{OwnerRunID: run.ID, RunnerID: "machine", ProjectID: "project", RepositoryIDs: []string{"repo"}}
	if err := s.EnqueueWarmWorkspace(demand, time.Now()); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		runs, err := s.RunSummaries()
		if err != nil || len(runs) != 1 {
			t.Fatal(runs, err)
		}
		detail, ok := s.RunView(run.ID)
		if !ok {
			t.Fatal("no detail")
		}
		live, ok := s.RunStreamView(run.ID)
		if !ok {
			t.Fatal("no live view")
		}
		for _, v := range []domain.Run{runs[0], detail, live} {
			raw, _ := json.Marshal(v)
			if strings.Contains(string(raw), `"warmWorkspaceWaiting":true`) != want {
				t.Errorf("waiting=%v projection missing or stale", want)
			}
		}
	}
	check(true)
	if err := s.CancelWarmWorkspace(run.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	check(false)
}
