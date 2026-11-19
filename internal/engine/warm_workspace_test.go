package engine

import (
	"context"
	"slices"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestWarmWorkspaceSOPWaitsBeforeDispatch(t *testing.T) {
	e, _ := cvEEngine(t)
	now := time.Now()
	if err := e.repo.PutRunner(domain.Runner{ID: "machine"}); err != nil {
		t.Fatal(err)
	}
	slots := []domain.WarmWorkspaceSlot{{ID: "slot1", ProjectID: "project", RepositoryID: "repo", State: "available"}, {ID: "slot2", ProjectID: "project", RepositoryID: "repo", State: "available"}}
	if err := e.repo.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), slots); err != nil {
		t.Fatal(err)
	}
	e.WorkspaceCheckpointTransport = func(ctx context.Context, r domain.Run, req domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		phase := "placed"
		if req.Operation == domain.RunnerWorkspaceOperationWarmCheckout {
			phase = "ready"
		}
		return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"` + phase + `"}`)}, nil
	}
	for _, id := range []string{"first", "second", "third"} {
		r := cvERunnerRun(id, "machine")
		r.Status = "running"
		r.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
		r.ExecutionContext.ProjectID = "project"
		r.Repository = domain.RepositoryContext{RepositoryID: "repo", WorkspaceDir: "checkout"}
		r.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
		if err := e.repo.PutRun(r); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		lease, err := e.EnsureWarmWorkspace(ctx, r, 1)
		cancel()
		task := domain.RunnerTask{ID: "task-" + id, RunID: id, RunnerID: "machine", Workspace: runnerWorkspaceRel(r, "script"), Provider: "terminal", Command: "true", Status: domain.RunnerTaskQueued, MaxAttempts: 1}
		dispatchErr := e.putWorkspaceRunnerTask(task)
		if id != "third" {
			if err != nil || lease.Generation == 0 {
				t.Fatalf("%s: %+v %v", id, lease, err)
			}
			if dispatchErr != nil {
				t.Fatal(dispatchErr)
			}
			claimed, ok, err := e.repo.ClaimRunnerTask("machine", []string{"terminal"}, now, time.Minute)
			if err != nil || !ok || claimed.RunID != id || claimed.WarmWorkspaceLease == nil || claimed.WarmWorkspaceLease.Generation != lease.Generation {
				t.Fatalf("claim %s: %+v %v", id, claimed, err)
			}
			continue
		}
		if err == nil || dispatchErr == nil {
			t.Fatal("third owner dispatched")
		}
		if tasks, err := e.repo.ActiveRunnerTasksForRun(id); err != nil || len(tasks) != 0 {
			t.Fatalf("queued owner has tasks: %+v %v", tasks, err)
		}
		full, _ := e.repo.Run(id)
		if !slices.ContainsFunc(full.Events, func(e domain.RunEvent) bool { return e.Type == "workspace.pool_waiting" }) {
			t.Fatal("missing waiting event")
		}
		l, state, _, err := e.repo.WarmWorkspaceAllocation(id)
		if err != nil || state != domain.WarmWorkspaceQueued || len(l.Slots) != 0 {
			t.Fatalf("worker shutdown lost demand: %+v %s %v", l, state, err)
		}
		if err := e.Cancel(id); err != nil {
			t.Fatal(err)
		}
		if state, err := e.WarmWorkspaceStatus(id); err != nil || state != domain.WarmWorkspaceCanceled {
			t.Fatalf("cancel: %s %v", state, err)
		}
	}
}

func TestWarmWorkspaceSharedOwnerAndNoFallback(t *testing.T) {
	e, _ := cvEEngine(t)
	owner := cvERunnerRun("owner", "machine")
	owner.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	owner.ExecutionContext.ProjectID = "p"
	owner.Repository = domain.RepositoryContext{RepositoryID: "repo", WorkspaceDir: "code"}
	owner.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
	if err := e.repo.PutRun(owner); err != nil {
		t.Fatal(err)
	}
	child := cvERunnerRun("child", "machine")
	child.WorkflowSnapshot.SandboxSnapshot = owner.WorkflowSnapshot.SandboxSnapshot
	child.ExecutionContext.WorkspaceRunID = owner.ID
	if _, err := e.EnsureWarmWorkspace(t.Context(), child, 1); err == nil {
		t.Fatal("child ran without parent's lease")
	}

	if err := e.repo.PutRun(child); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := e.repo.PutRunner(domain.Runner{ID: "machine"}); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "p", RepositoryID: "repo", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: owner.ID, ProjectID: "p", RunnerID: "machine", RepositoryIDs: []string{"repo"}}, now); err != nil {
		t.Fatal(err)
	}
	l, ok, err := e.repo.ReserveNextWarmWorkspace("machine", now)
	if err != nil || !ok {
		t.Fatal("reserve", err)
	}
	if err := e.repo.SettleWarmWorkspace(l, domain.WarmWorkspaceReady); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{domain.RunnerWorkspaceOperationFile, domain.RunnerWorkspaceOperationTerminal} {
		req := domain.RunnerWorkspaceRequest{RunID: child.ID, Operation: op, Terminal: &domain.WorkspaceTerminalRequest{Operation: "input"}}
		if err := e.BindWorkspaceLease(child, &req); err != nil {
			t.Fatal(err)
		}
		if err := e.AuthorizeWarmWorkspaceControl("machine", &req); err != nil {
			t.Fatal(err)
		}
		if req.RunID != child.ID || req.WorkspaceRunID != owner.ID || req.WarmWorkspaceLease == nil || req.WarmWorkspaceLease.Generation != l.Generation {
			t.Fatal(req)
		}
	}
	task := domain.RunnerTask{ID: "child-script", RunID: child.ID, RunnerID: "machine", Workspace: runnerWorkspaceRel(child, "script"), Provider: "terminal", Command: "true", Status: domain.RunnerTaskQueued, MaxAttempts: 1}
	if err := e.putWorkspaceRunnerTask(task); err != nil {
		t.Fatal(err)
	}
	queued, _ := e.repo.RunnerTask(task.ID)
	if queued.WarmWorkspaceLease == nil || queued.WarmWorkspaceLease.OwnerRunID != owner.ID {
		t.Fatal(queued)
	}
	task.RunnerID = "other-machine"
	if err := e.putWorkspaceRunnerTask(task); err == nil {
		t.Fatal("dispatched to a different runner")
	}
	queued, _ = e.repo.RunnerTask(task.ID)
	if queued.RunnerID != "machine" {
		t.Fatal("existing task switched machines")
	}
}
