package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
)

func requestChatEnd(t *testing.T, h *assistantHandlers, user domain.User, id string, status int) domain.AssistantSession {
	t.Helper()
	c, w := workbenchContext(user, "POST", "/api/assistant/sessions/"+id+"/end", bytes.NewBuffer(nil))
	c.Params = gin.Params{{Key: "id", Value: id}}
	h.endSession(c)
	if w.Code != status {
		t.Fatalf("end session: status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var session domain.AssistantSession
	if err := json.Unmarshal(w.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	return session
}

func waitForChatEnd(t *testing.T, h *assistantHandlers, g domain.AssistantGeneration, want string) domain.AssistantSession {
	t.Helper()
	var s domain.AssistantSession
	for until := time.Now().Add(5 * time.Second); time.Now().Before(until); time.Sleep(15 * time.Millisecond) {
		s, _ = h.repo.AssistantSession(g.UserID, g.SessionID)
		if s.End != nil && s.End.State == want {
			return s
		}
	}
	t.Fatalf("end state did not reach %q: %+v", want, s.End)
	return s
}

func reserveChatEndWorkspace(t *testing.T, h *assistantHandlers, s *AgentToolSession) domain.Run {
	t.Helper()
	registered, _ := h.repo.Repository("code")
	run, err := h.engine.StartRepositoryConversationWorkspace("end fixture", domain.TelemetryContext{
		ProjectID: s.ProjectID, ResponsibleUserID: s.UserID, ExecutionPinned: true, ExecutionRunnerID: "machine",
	}, repo.GenericDockerSandboxID, repositoryRunContext(registered))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.repo.SetAssistantSessionWorkspaceRun(s.ChatOwnerID, s.ChatSessionID, s.ProjectID, run.ID, true); err != nil {
		t.Fatal(err)
	}
	if err = h.repo.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: run.ID, RunnerID: "machine", ProjectID: s.ProjectID, RepositoryIDs: []string{"code"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	l, ok, err := h.repo.ReserveNextWarmWorkspace("machine", time.Now())
	if err != nil || !ok || l.OwnerRunID != run.ID {
		t.Fatalf("reserve: %+v, %v", l, err)
	}
	if err := h.repo.SettleWarmWorkspace(l, domain.WarmWorkspaceReady); err != nil {
		t.Fatal(err)
	}
	return run
}

func chatEndProof(req domain.RunnerWorkspaceRequest) domain.RunnerWorkspaceResponse {
	switch req.Operation {
	case domain.RunnerWorkspaceOperationWarmReleaseCheck:
		return domain.RunnerWorkspaceResponse{Payload: []byte(`[{"repositoryId":"code","commit":"0123456789012345678901234567890123456789"}]`)}
	case domain.RunnerWorkspaceOperationWarmReleaseCommit:
		return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"released"}`)}
	default:
		return domain.RunnerWorkspaceResponse{Status: 400, Error: "unexpected workspace operation"}
	}
}

func TestAssistantSessionEndReturnsWarmWorkspaces(t *testing.T) {
	h, s, g, _ := warmChatFixture(t)
	g.Status = "completed"
	if err := h.repo.CheckpointAssistantGeneration(g); err != nil {
		t.Fatal(err)
	}
	conv, _ := h.repo.AssistantSession(g.UserID, g.SessionID)
	conv.Messages[0].Attachments = []domain.AssistantAttachment{{ID: "kept", Name: "notes.txt"}}
	if err := h.repo.PutAssistantSession(g.UserID, conv); err != nil {
		t.Fatal(err)
	}
	owned := reserveChatEndWorkspace(t, h, s)
	borrowed := domain.Run{ID: "borrowed-sop", Status: "waiting", ExecutionContext: domain.TelemetryContext{
		ProjectID: s.ProjectID, WorkspaceMode: domain.WorkspaceModeWarmPool,
	}}
	if err := h.repo.PutRun(borrowed); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.SetAssistantSessionWorkspaceRun(s.ChatOwnerID, s.ChatSessionID, s.ProjectID, borrowed.ID, false); err != nil {
		t.Fatal(err)
	}
	calls := make(chan string, 4)
	h.engine.WorkspaceCheckpointTransport = func(_ context.Context, r domain.Run, req domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		calls <- r.ID + "/" + req.Operation
		return chatEndProof(req), nil
	}
	if err := h.repo.PutUser(domain.User{LoginName: "outsider"}); err != nil {
		t.Fatal(err)
	}
	outsider, _ := h.repo.UserByLoginName("outsider")
	requestChatEnd(t, h, outsider, g.SessionID, 403)
	requestChatEnd(t, h, s.User, g.SessionID, 202)
	ended := waitForChatEnd(t, h, g, "ended")
	if ended.End.EndedAt == nil || ended.WorkspaceRunID != "" || len(ended.Messages) != 1 ||
		len(ended.Messages[0].Attachments) != 1 || ended.Messages[0].Attachments[0].ID != "kept" ||
		len(ended.OwnWorkspaceRunIDs) != 1 || ended.OwnWorkspaceRunIDs[0] != owned.ID {
		t.Fatalf("ended session lost history or kept an active binding: %+v", ended)
	}
	if len(calls) != 2 || <-calls != owned.ID+"/warm_release_check" || <-calls != owned.ID+"/warm_release_commit" {
		t.Fatal("return was performed on the wrong owner or out of order")
	}
	if state, _ := h.engine.WarmWorkspaceStatus(owned.ID); state != domain.WarmWorkspaceReleased {
		t.Fatalf("slot still assigned: %s", state)
	}
	requestChatEnd(t, h, s.User, g.SessionID, 200)
}

func TestAssistantSessionEndDrainsGeneration(t *testing.T) {
	t.Run("running", func(t *testing.T) {
		h, s, g, _ := warmChatFixture(t)
		cancelled := make(chan struct{}, 1)
		rec := newAssistantGenerationRecorder(g, func() { cancelled <- struct{}{} })
		defer rec.close()
		requestChatEnd(t, h, s.User, g.SessionID, 202)
		select {
		case <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("end did not signal the local generation")
		}
		requestChatEnd(t, h, s.User, g.SessionID, 202)
		g.Status = "cancelled"
		conv, _ := h.repo.AssistantSession(g.UserID, g.SessionID)
		conv.Messages = append(conv.Messages, domain.AssistantMessage{Role: "assistant", Content: "已生成的内容"})
		if err := h.repo.CommitAssistantTranscript(g.UserID, g.ID, conv); err != nil {
			t.Fatal(err)
		}
		if err := h.repo.CheckpointAssistantGeneration(g); err != nil {
			t.Fatal(err)
		}
		current, _ := h.repo.AssistantSession(g.UserID, g.SessionID)
		if current.End.State != "ending" {
			t.Fatal("completed while the local generation was registered")
		}
		rec.close()
		ended := waitForChatEnd(t, h, g, "ended")
		if len(ended.Messages) != 2 || ended.Messages[1].Content != "已生成的内容" {
			t.Fatal("lost stopped reply")
		}
	})
	t.Run("queued", func(t *testing.T) {
		h, s, g, req := warmChatFixture(t)
		gateway := &agentToolGateway{deps: h.deps}
		if _, err := gateway.openConversationWorkspace(t.Context(), s, map[string]any{"repositoryId": "code"}); err != nil {
			t.Fatal(err)
		}
		var parked *warmConversationHandoff
		if err := s.modeAction(t.Context()); !errors.As(err, &parked) {
			t.Fatal(err)
		}
		if err := h.parkWarmGeneration(g, req, parked); err != nil {
			t.Fatal(err)
		}
		requestChatEnd(t, h, s.User, g.SessionID, 202)
		h.reconcileWarmGenerations(t.Context())
		waitForChatEnd(t, h, g, "ended")
		if state, _ := h.engine.WarmWorkspaceStatus(parked.run.ID); state != domain.WarmWorkspaceCanceled {
			t.Fatalf("queue left in state %q", state)
		}
	})
}

func TestAssistantSessionEndRetainsOnReturnFailure(t *testing.T) {
	h, s, g, _ := warmChatFixture(t)
	g.Status = "completed"
	if err := h.repo.CheckpointAssistantGeneration(g); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	slots := []domain.WarmWorkspaceSlot{
		{ID: "slot", ProjectID: s.ProjectID, RepositoryID: "code", State: "available", OriginSHA256: gitwork.RemoteFingerprint(warmChatFixtureURL)},
		{ID: "slot2", ProjectID: s.ProjectID, RepositoryID: "code", State: "available", OriginSHA256: gitwork.RemoteFingerprint(warmChatFixtureURL)},
	}
	if err := h.repo.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), slots); err != nil {
		t.Fatal(err)
	}
	first := reserveChatEndWorkspace(t, h, s)
	second := reserveChatEndWorkspace(t, h, s)
	requests := make(chan string, 8)
	var rejectSecond atomic.Bool
	rejectSecond.Store(true)
	h.engine.WorkspaceCheckpointTransport = func(_ context.Context, r domain.Run, req domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		requests <- r.ID
		if r.ID == second.ID && rejectSecond.Load() {
			return domain.RunnerWorkspaceResponse{Status: 409, Code: "warm_owner_retained", Error: "untracked notes.txt"}, nil
		}
		return chatEndProof(req), nil
	}
	requestChatEnd(t, h, s.User, g.SessionID, 202)
	failed := waitForChatEnd(t, h, g, "failed")
	if !strings.Contains(failed.End.Error, "untracked notes.txt") || failed.WorkspaceRunID != second.ID || len(failed.Messages) != 1 {
		t.Fatalf("work was lost or error was hidden: %+v", failed)
	}
	if state, _ := h.engine.WarmWorkspaceStatus(first.ID); state != domain.WarmWorkspaceReleased {
		t.Fatalf("first return not kept: %s", state)
	}
	if len(requests) != 3 || <-requests != first.ID || <-requests != first.ID || <-requests != second.ID {
		t.Fatal("unexpected partial return sequence")
	}
	rejectSecond.Store(false)
	requestChatEnd(t, h, s.User, g.SessionID, 202)
	waitForChatEnd(t, h, g, "ended")
	if len(requests) != 2 || <-requests != second.ID || <-requests != second.ID {
		t.Fatal("retry checked more than the remaining owner")
	}
}
