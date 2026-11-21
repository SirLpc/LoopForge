package engine

import (
	"agentflow/internal/domain"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func warmSOPWithReadySlot(t *testing.T, status string) (*Engine, domain.Run) {
	t.Helper()
	e, _ := newRecoveryEngine(t)
	run := cvERunnerRun("run-warm-staging", "machine")
	run.Status = status
	run.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	run.ExecutionContext.ProjectID = "release-project"
	run.Repository = domain.RepositoryContext{RepositoryID: "repo", WorkspaceDir: "checkout"}
	run.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
	run.WorkspaceHost, run.WorkspaceRoot = e.workspaceHost, e.sandboxes.Root()
	if err := e.repo.PutProject(domain.Project{ID: "release-project", Key: "release-project", Name: "Release", OwnerID: 1}); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.PutRun(run); err != nil {
		t.Fatal(err)
	}
	runner := cvERunner(t, e, "machine", domain.RunnerStatusOnline, "terminal")
	now := time.Now()
	slots := []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "release-project", RepositoryID: "repo", State: "available"}}
	if err := e.repo.RunnerHeartbeat("machine", runner.Runtimes, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), slots); err != nil {
		t.Fatal(err)
	}
	if err := e.repo.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: run.ID, RunnerID: "machine", ProjectID: "release-project", RepositoryIDs: []string{"repo"}}, now); err != nil {
		t.Fatal(err)
	}
	lease, ok, err := e.repo.ReserveNextWarmWorkspace("machine", now)
	if !ok || err != nil {
		t.Fatalf("reserve slot: %v", err)
	}
	if err := e.repo.SettleWarmWorkspace(lease, domain.WarmWorkspaceReady); err != nil {
		t.Fatal(err)
	}
	return e, run
}

func TestWarmSOPExecutesWithoutStartingPlatformDockerSandbox(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho 'unexpected platform Docker command' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENTFLOW_LOCAL_SANDBOX", "docker")

	for _, withEnvironment := range []bool{false, true} {
		name := "plain"
		if withEnvironment {
			name = "with environment install"
		}
		t.Run(name, func(t *testing.T) {
			e, run := warmSOPWithReadySlot(t, "running")
			node := domain.WorkflowNode{ID: "script", Name: "Script", Type: "script", Script: "echo runner"}
			run.WorkflowSnapshot.Nodes = []domain.WorkflowNode{node}
			run.Nodes[node.ID] = &domain.NodeRun{NodeID: node.ID, Status: "pending"}
			if withEnvironment {
				run.WorkflowSnapshot.SandboxSnapshot.Environment = &domain.EnvironmentDefinition{ID: "test-env", InstallScript: "echo setup"}
			}
			if err := e.repo.PutRun(run); err != nil {
				t.Fatal(err)
			}
			if _, err := e.acquireLease(run); err != nil {
				t.Fatal(err)
			}
			e.SetRunnerDispatchNotify(func(string) {
				cvEFinishQueuedTask(t, e, domain.RunnerTaskSuccess, "runner output", "", nil)
			})

			e.executeNode(t.Context(), run.ID, run.WorkflowSnapshot, node)

			current, _ := e.repo.RunState(run.ID)
			if got := current.Nodes[node.ID]; got.Status != "succeeded" {
				t.Fatalf("node status = %q, error = %q", got.Status, got.Error)
			}
			tasks := e.repo.RunnerTasks()
			if len(tasks) != 1 || tasks[0].RunnerID != "machine" || tasks[0].WarmWorkspaceLease == nil {
				t.Fatalf("expected one task on the warm runner, got %+v", tasks)
			}
			if withEnvironment && (tasks[0].Sandbox.Environment == nil || tasks[0].Sandbox.Environment.ID != "test-env") {
				t.Fatal("runner did not receive the frozen environment")
			}
		})
	}
}

func TestCancelWarmSOPClosesPlatformSandboxBeforeReturningSlot(t *testing.T) {
	e, run := warmSOPWithReadySlot(t, "waiting")
	root := e.RunWorkspace(run.ID)
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	localFile := filepath.Join(root, "keep.txt")
	if err := os.WriteFile(localFile, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	sandbox := &recordingSandbox{workspace: root}
	e.runEnvs[run.ID] = &runEnvironment{sandbox: sandbox}
	e.WorkspaceCheckpointTransport = func(_ context.Context, _ domain.Run, req domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		switch req.Operation {
		case domain.RunnerWorkspaceOperationWarmReleaseCheck:
			if !sandbox.closed {
				return domain.RunnerWorkspaceResponse{Code: "warm_owner_retained", Error: "platform sandbox still running"}, nil
			}
			return domain.RunnerWorkspaceResponse{Payload: []byte(`[{"repositoryId":"repo","commit":"0123456789012345678901234567890123456789"}]`)}, nil
		case domain.RunnerWorkspaceOperationWarmReleaseCommit:
			return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"released"}`)}, nil
		default:
			t.Fatalf("unexpected runner operation %q", req.Operation)
			return domain.RunnerWorkspaceResponse{}, nil
		}
	}

	if err := e.Cancel(run.ID); err != nil {
		t.Fatal(err)
	}
	if state, err := e.WarmWorkspaceStatus(run.ID); err != nil || state != domain.WarmWorkspaceReleased {
		t.Fatalf("cancelled SOP slot state = %q, sandbox closed = %t, err = %v", state, sandbox.closed, err)
	}
	if data, err := os.ReadFile(localFile); err != nil || string(data) != "keep" {
		t.Fatalf("platform workspace file = %q, err = %v", data, err)
	}
}

func TestCancelWarmChildClosesOnlyItsPlatformSandbox(t *testing.T) {
	e, owner := warmSOPWithReadySlot(t, "waiting")
	child := owner
	child.ID = "run-warm-child"
	child.ExecutionContext.ParentRunID = owner.ID
	child.ExecutionContext.WorkspaceRunID = owner.ID
	if err := e.repo.PutRun(child); err != nil {
		t.Fatal(err)
	}
	parentSandbox, childSandbox := &recordingSandbox{}, &recordingSandbox{}
	e.runEnvs[owner.ID] = &runEnvironment{sandbox: parentSandbox}
	e.runEnvs[child.ID] = &runEnvironment{sandbox: childSandbox}

	if err := e.Cancel(child.ID); err != nil {
		t.Fatal(err)
	}
	if !childSandbox.closed || parentSandbox.closed {
		t.Fatalf("closed sandboxes: child=%t parent=%t", childSandbox.closed, parentSandbox.closed)
	}
	if state, err := e.WarmWorkspaceStatus(owner.ID); err != nil || state != domain.WarmWorkspaceReady {
		t.Fatalf("parent slot state = %q, err = %v", state, err)
	}
}

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
	actorProbe := int64(0)
	archiveCalls := 0
	e.WorkspaceCheckpointTransport = func(ctx context.Context, r domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		switch q.Operation {
		case domain.RunnerWorkspaceOperationWarmVerifyAccess:
			actorProbe = q.WarmWorkspaceActorID
			if actorProbe == 2 {
				return domain.RunnerWorkspaceResponse{Error: "Git access revoked", Status: 403}, nil
			}
			return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"authorized"}`)}, nil
		case domain.RunnerWorkspaceOperationWarmPrepare:
			return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"placed"}`)}, nil
		case domain.RunnerWorkspaceOperationWarmCheckout:
			if q.WarmWorkspaceActorID != 1 {
				t.Fatalf("lost authenticated restore identity: %d", q.WarmWorkspaceActorID)
			}
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
	if err := e.repo.PutProject(domain.Project{ID: "release-project", Key: "release-project", Name: "Release", OwnerID: 1, Members: []domain.ProjectMember{{UserID: 2}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.StartWorkspaceRestoreContext(WithWorkspaceRestoreActor(t.Context(), 2), run.ID); err == nil || actorProbe != 2 {
		t.Fatalf("ready restore bypassed actor's Git access: %v %d", err, actorProbe)
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
	state, err := e.StartWorkspaceRestoreContext(WithWorkspaceRestoreActor(t.Context(), 1), run.ID)
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
