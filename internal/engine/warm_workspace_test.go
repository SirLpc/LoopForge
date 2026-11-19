package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestWarmWorkspaceSOPWaitsBeforeDispatch(t *testing.T) {
	e, _ := cvEEngine(t)
	if err := e.repo.PutProject(domain.Project{ID: "project", OwnerID: 1}); err != nil {
		t.Fatal(err)
	}
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
		if req.Operation == domain.RunnerWorkspaceOperationWarmPrepare {
			stored, _ := e.repo.Run(r.ID)
			if !slices.ContainsFunc(stored.Events, func(event domain.RunEvent) bool { return event.Type == "workspace.pool_preparing" }) {
				t.Error("slot reservation must wake live views before preparation starts")
			}
		}
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
			claimed, ok, err := e.repo.ClaimRunnerTask("machine", []string{"terminal"}, now, time.Minute, true)
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
	observed := domain.RunnerWorkspaceRequest{RunID: child.ID, Operation: domain.RunnerWorkspaceOperationFile}
	if err := e.AuthorizeWarmWorkspaceControl("machine", &observed); err == nil {
		t.Error("inbound request minted current lease")
	}
	if observed.WarmWorkspaceLease != nil {
		t.Fatal("failed last-hop check mutated inbound lease")
	}
	foreign := domain.RunnerWorkspaceRequest{RunID: "unknown", WorkspaceRunID: owner.ID, Operation: domain.RunnerWorkspaceOperationFile}
	if err := e.AuthorizeWarmWorkspaceControl("machine", &foreign); err == nil {
		t.Fatal("untracked run reached allocated workspace")
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
	browser := domain.RunnerWorkspaceRequest{RunID: child.ID, Operation: domain.RunnerWorkspaceOperationBrowser, Browser: &domain.BrowserRequest{}, WarmWorkspaceLease: &l}
	e.controlToken = "test-internal"
	var seen *domain.WarmWorkspaceLease
	e.WorkspaceCheckpointTransport = func(_ context.Context, _ domain.Run, request domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		seen = request.WarmWorkspaceLease
		return domain.RunnerWorkspaceResponse{}, nil
	}
	sendBrowser := func() int {
		data, err := json.Marshal(browser)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest("POST", "/", bytes.NewReader(data))
		req.Header.Set(controlTokenHeader, e.controlToken)
		rec := httptest.NewRecorder()
		e.HandleWorkspaceCheckpoint(rec, req)
		return rec.Code
	}
	if status := sendBrowser(); status != 200 || seen == nil || seen.Generation != l.Generation {
		t.Fatalf("browser checkpoint lost its observed generation: status=%d lease=%v", status, seen)
	}
	browser.WarmWorkspaceLease = nil
	seen = nil
	if status := sendBrowser(); status == 200 || seen != nil {
		t.Fatal("browser accepted missing generation")
	}
	if err := e.repo.RunnerHeartbeat("machine", nil, "0.11.0", now, now.Add(time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	browser.WarmWorkspaceLease = &l
	if err := e.AuthorizeWarmWorkspaceControl("machine", &browser); err == nil {
		t.Fatal("ready operation delivered to downgraded runner")
	}
	if err := e.putWorkspaceRunnerTask(task); err == nil {
		t.Fatal("warm task submitted to downgraded runner")
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

func TestWarmWorkspaceFreezeRequiresReadableProjectMode(t *testing.T) {
	e, _ := cvEEngine(t)
	ec := domain.TelemetryContext{ProjectID: "missing"}
	if err := e.freezeWarmWorkspace(&ec, []domain.RepositoryContext{{RepositoryID: "a"}}, domain.SandboxProfile{}); err == nil {
		t.Fatal("missing project silently used checkout")
	}
}

func TestWarmWorkspaceSchedulerDurableCancelBeforeEnqueue(t *testing.T) {
	e, s := cvEEngine(t)
	e.instanceID = "test"
	e.executeRuns = true
	e.leaseTTL = time.Hour
	r := cvERunnerRun("durably-cancelled", "machine")
	r.Status = "running"
	r.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	r.ExecutionContext.ProjectID = "p"
	r.Repository = domain.RepositoryContext{RepositoryID: "repo", WorkspaceDir: "checkout"}
	r.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
	if err := s.PutRun(r); err != nil {
		t.Fatal(err)
	}
	if err := s.TryAcquireRunLease(r.ID, e.instanceID, 0, e.leaseTTL); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRunner(domain.Runner{ID: "machine"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "p", RepositoryID: "repo", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RequestRunCancel(r.ID); err != nil {
		t.Fatal(err)
	}
	e.execute(t.Context(), r.ID)
	got, _ := s.RunState(r.ID)
	if got.Status != "cancelled" {
		t.Fatalf("durably cancelled worker settled as %q", got.Status)
	}
}

func TestWarmSOPQueuedRunnerChanges(t *testing.T) {
	for _, change := range []string{"removed", "downgraded", "unregistered", "offline", "retry"} {
		t.Run(change, func(t *testing.T) {
			e, st := cvEEngine(t)
			r := cvERunnerRun("owner", "machine")
			r.ExecutionContext.ProjectID = "project"
			r.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
			r.Status = "running"
			r.Repository = domain.RepositoryContext{RepositoryID: "code", WorkspaceDir: "code"}
			r.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
			if err := st.PutRun(r); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			if err := st.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: r.ID, RunnerID: "machine", ProjectID: "project", RepositoryIDs: []string{"code"}}, now); err != nil {
				t.Fatal(err)
			}
			if change != "removed" {
				m := domain.Runner{ID: "machine", RunnerVersion: domain.RunnerVersionWarmWorkspace, Status: domain.RunnerStatusOnline}
				if change == "offline" {
					m.Status = domain.RunnerStatusOffline
				}
				if change == "downgraded" {
					m.RunnerVersion = "0.1.0"
				}
				if err := st.PutRunner(m); err != nil {
					t.Fatal(err)
				}
			}
			if change == "retry" {
				if err := st.CancelWarmWorkspaceEpoch(r.ID, 1, now); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
			defer cancel()
			_, err := e.EnsureWarmWorkspace(ctx, r, 1)
			l, state, _, _ := st.WarmWorkspaceAllocation(r.ID)
			if change == "offline" {
				if err != context.DeadlineExceeded || state != domain.WarmWorkspaceQueued {
					t.Fatalf("offline: %s %v", state, err)
				}
				if err := st.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "project", RepositoryID: "code", State: "available"}}); err != nil {
					t.Fatal(err)
				}
				if _, ok, err := st.ReserveNextWarmWorkspace("machine", now); !ok || err != nil {
					t.Fatalf("reconnect: %t %v", ok, err)
				}
				return
			}
			if err == nil || err == context.DeadlineExceeded || state != domain.WarmWorkspaceCanceled || l.Epoch != 1 {
				t.Fatalf("incompatible %s: %s epoch %d %v", change, state, l.Epoch, err)
			}
		})
	}
}
func TestWarmSOPGrantChecksResponsibleActor(t *testing.T) {
	e, st := cvEEngine(t)
	r := cvERunnerRun("owner", "machine")
	r.ExecutionContext.ProjectID = "project"
	r.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	r.Repository = domain.RepositoryContext{RepositoryID: "code", WorkspaceDir: "code"}
	r.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
	if err := st.PutRun(r); err != nil {
		t.Fatal(err)
	}
	project := domain.Project{ID: "project", OwnerID: 22, Members: []domain.ProjectMember{{UserID: 1, Role: "member"}}}
	if err := st.PutProject(project); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.PutRunner(domain.Runner{ID: "machine"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "project", RepositoryID: "code", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: r.ID, RunnerID: "machine", ProjectID: "project", RepositoryIDs: []string{"code"}}, now); err != nil {
		t.Fatal(err)
	}
	project.Members = []domain.ProjectMember{}
	if err := st.PutProject(project); err != nil {
		t.Fatal(err)
	}
	l, ok, err := st.ReserveNextWarmWorkspace("machine", now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	count := 0
	e.WorkspaceCheckpointTransport = func(context.Context, domain.Run, domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		count++
		return domain.RunnerWorkspaceResponse{}, nil
	}
	q := domain.RunnerWorkspaceRequest{RunID: r.ID, Operation: domain.RunnerWorkspaceOperationWarmCheckout, WarmWorkspaceLease: &l}
	if _, err := e.requestWorkspaceCheckpoint(t.Context(), r, q); err == nil || count != 0 {
		t.Fatalf("checkout after membership changed: %v, dispatched %d", err, count)
	}
}

func TestWarmCheckoutRetryableResponseRetainsEpoch(t *testing.T) {
	e, st := cvEEngine(t)
	r := cvERunnerRun("owner", "machine")
	r.Status = "running"
	r.ExecutionContext.ProjectID = "project"
	r.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	r.Repository = domain.RepositoryContext{RepositoryID: "code", WorkspaceDir: "code"}
	r.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
	if err := st.PutProject(domain.Project{ID: "project", OwnerID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRun(r); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.PutRunner(domain.Runner{ID: "machine"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "project", RepositoryID: "code", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	failed := false
	e.WorkspaceCheckpointTransport = func(_ context.Context, _ domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		if q.Operation == domain.RunnerWorkspaceOperationWarmPrepare {
			return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"placed"}`)}, nil
		}
		if !failed {
			failed = true
			return domain.RunnerWorkspaceResponse{Status: 409, Error: "frozen remote unavailable", Code: "warm_checkout_retryable"}, nil
		}
		return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"ready"}`)}, nil
	}
	if _, err := e.EnsureWarmWorkspace(t.Context(), r, 1); err == nil {
		t.Fatal("failed checkout should return its diagnostic")
	}
	first, state, _, _ := st.WarmWorkspaceAllocation(r.ID)
	if state != domain.WarmWorkspaceReserved {
		t.Fatal("lost replayable reservation", state)
	}
	next, err := e.EnsureWarmWorkspace(t.Context(), r, 1)
	if err != nil || next.Generation != first.Generation || next.Epoch != first.Epoch {
		t.Fatal(next, err)
	}
}
