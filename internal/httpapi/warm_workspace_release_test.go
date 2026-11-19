package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"context"
	"errors"
	"testing"
	"time"
)

func TestWarmWorkspaceReleaseDeniedPreservesConversation(t *testing.T) {
	g, user, store := conversationIssueFixture(t)
	run, ok := store.RunState("ws-1")
	if !ok {
		t.Fatal("fixture missing")
	}
	run.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	run.ExecutionContext.ExecutionRunnerID = "machine"
	run.ExecutionContext.ExecutionMode = domain.ExecutionModeRunner
	run.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if err := store.SetAssistantSessionWorkspaceRun(user.UserID, "chat-1", "p", run.ID, true); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.PutRunner(domain.Runner{ID: "machine"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "p", RepositoryID: "repo-web", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: run.ID, ProjectID: "p", RunnerID: "machine", RepositoryIDs: []string{"repo-web"}}, now); err != nil {
		t.Fatal(err)
	}
	l, ok, err := store.ReserveNextWarmWorkspace("machine", now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	if err := store.SettleWarmWorkspace(l, domain.WarmWorkspaceReady); err != nil {
		t.Fatal(err)
	}
	g.engine = engine.New(store, nil)
	checked := false
	g.engine.WorkspaceCheckpointTransport = func(ctx context.Context, run domain.Run, req domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		checked = req.Operation == domain.RunnerWorkspaceOperationWarmReleaseCheck
		return domain.RunnerWorkspaceResponse{Error: "untracked file", Code: "warm_owner_retained"}, nil
	}
	h := assistantHandlers{deps: g.deps}
	if err := h.deleteConversation(user.UserID, "chat-1"); !errors.Is(err, errWarmConversationRetained) || !checked {
		t.Fatal(err, checked)
	}
	if conv, ok := store.AssistantSession(user.UserID, "chat-1"); !ok || conv.WorkspaceRunID != run.ID {
		t.Fatal("lost session binding")
	}
	if _, ok := store.RunState(run.ID); !ok {
		t.Fatal("lost workspace owner")
	}
}
