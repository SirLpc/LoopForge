package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"net/http/httptest"
	"testing"
	"time"
)

func TestWarmWorkspaceReleaseTransientSessionReadRetains(t *testing.T) {
	g, user, st := conversationIssueFixture(t)
	failed := false
	err := st.DB.Callback().Query().Before("gorm:query").Register("test:session-read", func(tx *gorm.DB) {
		if !failed && tx.Statement.Table == "assistant_sessions" {
			failed = true
			tx.AddError(errors.New("transient read"))
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer st.DB.Callback().Query().Remove("test:session-read")
	h := assistantHandlers{deps: g.deps}
	if err := h.deleteConversation(user.UserID, "chat-1"); err == nil || !failed {
		t.Fatalf("delete after failed read: %v", err)
	}
	if _, ok := st.AssistantSession(user.UserID, "chat-1"); !ok {
		t.Fatal("deleted session")
	}
}
func TestWarmWorkspaceReleaseChangedSessionBindingRetains(t *testing.T) {
	_, user, st := conversationIssueFixture(t)
	old, ok := st.AssistantSession(user.UserID, "chat-1")
	if !ok {
		t.Fatal("missing session")
	}
	if err := st.SetAssistantSessionWorkspaceRun(user.UserID, old.ID, "p", "ws-1", true); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAssistantSessionAfterReturn(old, map[string]*domain.WarmWorkspaceLease{}); !errors.Is(err, repo.ErrConversationWorkspaceChanged) {
		t.Fatal(err)
	}
}
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
	if err := store.DeleteAssistantSession(user.UserID, "chat-1"); !errors.Is(err, repo.ErrConversationWorkspaceChanged) {
		t.Fatalf("unguarded delete allowed active warm checkout: %v", err)
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
	if err := store.SettleWarmWorkspace(l, domain.WarmWorkspaceReleasing); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordWarmWorkspaceRefs(run.ID, l, []domain.WarmWorkspaceRef{{RepositoryID: "repo-web", Commit: "0123456789012345678901234567890123456789"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ConfirmWarmWorkspaceBinding(l, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	actor, _ := store.User(user.UserID)
	ctx.Request = httptest.NewRequest("GET", "/", nil).WithContext(context.WithValue(t.Context(), requestUserContextKey{}, domainUserToAuth(actor)))
	run, _ = store.RunState(run.ID)
	payload := map[string]any{}
	runs := runsHandlers{deps: g.deps}
	runs.workspaceLifecycleInfo(ctx, run, payload)
	if payload["restorable"] != true {
		t.Fatal("Git-only return absent from workspace lifecycle", payload)
	}
	if err := store.PutUser(domain.User{LoginName: "another-member", ChineseName: "Member"}); err != nil {
		t.Fatal(err)
	}
	member, _ := store.UserByLoginName("another-member")
	for _, prefer := range []string{"", "respond-async"} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Params = gin.Params{{Key: "id", Value: run.ID}}
		c.Request = httptest.NewRequest("POST", "/", nil).WithContext(context.WithValue(t.Context(), requestUserContextKey{}, domainUserToAuth(member)))
		c.Request.Header.Set("Prefer", prefer)
		probed := int64(0)
		g.engine.WorkspaceCheckpointTransport = func(_ context.Context, _ domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
			if q.Operation == domain.RunnerWorkspaceOperationWarmVerifyAccess {
				probed = q.WarmWorkspaceActorID
			}
			return domain.RunnerWorkspaceResponse{Status: 403, Error: "revoked"}, nil
		}
		runs.workspaceRestore(c)
		if recorder.Code < 400 || probed != member.ID {
			t.Fatalf("restore %q used actor %d, status %d", prefer, probed, recorder.Code)
		}
	}
	if err := store.RequeueWarmWorkspace(l, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteAssistantSessionAfterReturn(mustWarmSession(t, store, user.UserID), map[string]*domain.WarmWorkspaceLease{run.ID: &l}); !errors.Is(err, repo.ErrConversationWorkspaceChanged) {
		t.Fatalf("deleted session after reacquire: %v", err)
	}
}
func mustWarmSession(t *testing.T, st *repo.Store, user int64) domain.AssistantSession {
	t.Helper()
	s, ok := st.AssistantSession(user, "chat-1")
	if !ok {
		t.Fatal("missing session")
	}
	return s
}
