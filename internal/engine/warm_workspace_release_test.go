package engine

import (
	"agentflow/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestWarmWorkspaceReclaimDuringRestart(t *testing.T) {
	e, _ := cvEEngine(t)
	now := time.Now()
	run := cvERunnerRun("return-owner", "machine")
	run.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	run.ExecutionContext.ProjectID = "release-project"
	run.Repository = domain.RepositoryContext{RepositoryID: "a", WorkspaceDir: "checkout"}
	run.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
	if err := e.repo.PutProject(domain.Project{ID: "release-project", Key: "release-project", Name: "Release", OwnerID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.PutRunner(domain.Runner{ID: "machine"}); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "release-project", RepositoryID: "a", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: run.ID, RunnerID: "machine", ProjectID: "release-project", RepositoryIDs: []string{"a"}}, now); err != nil {
		t.Fatal(err)
	}
	l, ok, err := e.repo.ReserveNextWarmWorkspace("machine", now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	if err = e.repo.SettleWarmWorkspace(l, domain.WarmWorkspaceReady); err != nil {
		t.Fatal(err)
	}
	step := "dirty"
	archiveCalls := 0
	e.WorkspaceCheckpointTransport = func(ctx context.Context, r domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		switch q.Operation {
		case domain.RunnerWorkspaceOperationWarmPrepare:
			return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"placed"}`)}, nil
		case domain.RunnerWorkspaceOperationWarmCheckout:
			if req := q.PreparationInvocation; req == nil || len(req.WarmWorkspaceRefs) != 1 || req.WarmWorkspaceRefs[0].Commit != "0123456789012345678901234567890123456789" {
				t.Errorf("reopen failed to pin old commit: %+v", req)
			}
			return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"ready"}`)}, nil
		case domain.RunnerWorkspaceOperationWarmReleaseCheck:
			if step == "dirty" {
				return domain.RunnerWorkspaceResponse{Code: "warm_owner_retained", Error: "private file"}, nil
			}
			b, _ := json.Marshal([]domain.WarmWorkspaceRef{{RepositoryID: "a", Commit: "0123456789012345678901234567890123456789"}})
			return domain.RunnerWorkspaceResponse{Payload: b}, nil
		case domain.RunnerWorkspaceOperationWarmReleaseCommit:
			refs, err := e.repo.WarmWorkspaceRefs(r.ID)
			if err != nil || len(refs) != 1 || len(q.WarmWorkspaceRefs) != 1 {
				t.Fatal("moved before durable proof", refs, q, err)
			}
			if step == "lost" {
				return domain.RunnerWorkspaceResponse{}, errors.New("ack lost")
			}
			return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"released"}`)}, nil
		default:
			archiveCalls++
			return domain.RunnerWorkspaceResponse{}, errors.New("unexpected workspace operation")
		}
	}
	child := cvERunnerRun("borrower", "machine")
	child.ExecutionContext.WorkspaceRunID = run.ID
	child.Status = "waiting"
	if err := e.repo.PutRun(child); err != nil {
		t.Fatal(err)
	}
	step = "clean"
	if err = e.ReleaseWarmWorkspace(t.Context(), run, 1); err == nil {
		t.Fatal("released while a borrowing SOP awaited approval")
	}
	child.Status = "succeeded"
	if err := e.repo.PutRun(child); err != nil {
		t.Fatal(err)
	}
	step = "dirty"
	if err = e.DeleteRun(run.ID); err == nil {
		t.Fatal("deleted dirty workspace")
	}
	if _, ok := e.repo.RunState(run.ID); !ok {
		t.Fatal("lost run")
	}
	if _, err = e.readyWarmLease(run); err != nil {
		t.Fatal(err)
	}
	if err = e.saveConversationWorkspaceAfterUse(t.Context(), run.ID, ""); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{domain.RunnerWorkspaceOperationCheckpointExport, domain.RunnerWorkspaceOperationCheckpointUpload, domain.RunnerWorkspaceOperationRestore, domain.RunnerWorkspaceOperationCheckpoint} {
		_, err = e.requestWorkspaceCheckpoint(t.Context(), run, domain.RunnerWorkspaceRequest{Operation: op})
		if err == nil {
			t.Fatal(op)
		}
	}
	step = "lost"
	if err = e.ReleaseWarmWorkspace(t.Context(), run, 1); err == nil {
		t.Fatal("lost acknowledgement ignored")
	}
	if state, _ := e.WarmWorkspaceStatus(run.ID); state != domain.WarmWorkspaceReleasing {
		t.Fatal(state)
	}
	step = "retry"
	if err = e.ReleaseWarmWorkspace(t.Context(), run, 1); err != nil {
		t.Fatal(err)
	}
	if state, _ := e.WarmWorkspaceStatus(run.ID); state != domain.WarmWorkspaceReleased || archiveCalls != 0 {
		t.Fatal(state, archiveCalls)
	}
	state, err := e.StartWorkspaceRestore(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for limit := time.Now().Add(5 * time.Second); state.Status == "restoring" && time.Now().Before(limit); {
		time.Sleep(10 * time.Millisecond)
		state, err = e.WorkspaceRestoreStatus(run.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if state.Status != "ready" || archiveCalls != 0 {
		t.Fatal(state, archiveCalls)
	}
	if recovered, err := e.readyWarmLease(run); err != nil || recovered.Generation <= l.Generation {
		t.Fatal("no new lease", recovered, err)
	}
}
