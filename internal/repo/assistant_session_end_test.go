package repo

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func endSessionFixture(t *testing.T) (*Store, domain.AssistantSession) {
	t.Helper()
	st, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	session := domain.AssistantSession{ID: "chat-end", UserID: 1, Title: "原会话", Messages: []domain.AssistantMessage{{Role: "user", Content: "原消息"}}}
	if err := st.PutAssistantSession(session.UserID, session); err != nil {
		t.Fatal(err)
	}
	session, _ = st.AssistantSession(session.UserID, session.ID)
	return st, session
}

func TestAssistantSessionEndAdmission(t *testing.T) {
	st, session := endSessionFixture(t)
	stale := session
	stale.End = &domain.AssistantSessionEnd{State: "ended"}
	if err := st.PutAssistantSession(session.UserID, stale); err != nil {
		t.Fatal(err)
	}
	stored, _ := st.AssistantSession(session.UserID, session.ID)
	if stored.End != nil {
		t.Fatal("client-controlled end state was accepted")
	}
	if _, _, err := st.BeginAssistantSessionEnd(session.UserID, session.ID, "attempt"); err != nil {
		t.Fatal(err)
	}
	g := domain.AssistantGeneration{ID: "new-turn", SessionID: session.ID, UserID: session.UserID, Status: "running"}
	if err := st.BeginAssistantGeneration(g); !errors.Is(err, ErrAssistantSessionEnding) {
		t.Fatalf("new turn during end: %v", err)
	}
	stale.Messages = []domain.AssistantMessage{{Role: "user", Content: "覆盖"}}
	if err := st.PutAssistantSession(session.UserID, stale); !errors.Is(err, ErrAssistantSessionEnding) {
		t.Fatalf("stale transcript write during end: %v", err)
	}
	stale.Messages = session.Messages
	stale.Title = "新标题"
	if err := st.PutAssistantSession(session.UserID, stale); err != nil {
		t.Fatal(err)
	}
	stored, _ = st.AssistantSession(session.UserID, session.ID)
	if stored.End == nil || stored.End.State != "ending" || stored.Title != "新标题" || stored.Messages[0].Content != "原消息" {
		t.Fatalf("rename lost history or end state: %+v", stored)
	}
}

func TestAssistantSessionEndCancel(t *testing.T) {
	st, session := endSessionFixture(t)
	g := domain.AssistantGeneration{ID: "active-turn", SessionID: session.ID, UserID: session.UserID, ActorID: session.UserID, Status: "running"}
	if err := st.BeginAssistantGeneration(g, session); err != nil {
		t.Fatal(err)
	}
	item := domain.AssistantNativeApproval{
		ID: "approval", GenerationID: g.ID, ActorID: g.ActorID, Tool: "test", Input: json.RawMessage("{}"),
	}
	if err := st.CreateNativeApproval(session.UserID, session.ID, "phase", item); err != nil {
		t.Fatal(err)
	}
	_, stopped, err := st.BeginAssistantSessionEnd(session.UserID, session.ID, "attempt")
	if err != nil || stopped != g.ID {
		t.Fatalf("cancelled generation = %q, err = %v", stopped, err)
	}
	stored, _ := st.PeekAssistantGeneration(session.UserID, session.ID)
	if !stored.CancelRequested || len(st.PendingNativeApprovals(session.UserID, session.ID)) != 0 {
		t.Fatalf("generation or approvals were not cancelled: %+v", stored)
	}
	if err := st.SteerAssistantGeneration(session.UserID, session.ID, g.ID, domain.AssistantSteer{Content: "下一轮"}); err == nil {
		t.Fatal("steering was accepted during end")
	}
	session.Messages = append(session.Messages, domain.AssistantMessage{Role: "assistant", Content: "已生成的片段", CreatedAt: time.Now()})
	if err := st.CommitAssistantTranscript(session.UserID, g.ID, session); err != nil {
		t.Fatal(err)
	}
	saved, _ := st.AssistantSession(session.UserID, session.ID)
	if len(saved.Messages) != 2 || saved.Messages[1].Content != "已生成的片段" || saved.End == nil || saved.End.State != "ending" {
		t.Fatalf("lost partial transcript or end state: %+v", saved)
	}
}

func TestAssistantSessionEndRejectsStaleAttempt(t *testing.T) {
	st, session := endSessionFixture(t)
	run := domain.Run{ID: "queued-workspace", Kind: domain.RunKindWorkspace, Status: "waiting",
		ExecutionContext: domain.TelemetryContext{WorkspaceMode: domain.WorkspaceModeWarmPool}}
	if err := st.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if err := st.SetAssistantSessionWorkspaceRun(session.UserID, session.ID, session.ProjectID, run.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := st.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{
		OwnerRunID: run.ID, ProjectID: "p", RunnerID: "runner", RepositoryIDs: []string{"repo"},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	lease, _, found, err := st.WarmWorkspaceAllocation(run.ID)
	if err != nil || !found {
		t.Fatalf("no queued demand: %v", err)
	}
	if err := st.CancelWarmWorkspaceEpoch(run.ID, lease.Epoch, time.Now()); err != nil {
		t.Fatal(err)
	}
	old, _, err := st.BeginAssistantSessionEnd(session.UserID, session.ID, "old")
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().Add(-time.Minute)
	old.End.Deadline = &expired
	if err = st.DB.Model(&assistantSessionRow{}).Where("id = ?", old.ID).UpdateColumn("payload", mustJSON(old)).Error; err != nil {
		t.Fatal(err)
	}
	current, _, err := st.BeginAssistantSessionEnd(session.UserID, session.ID, "new")
	if err != nil || current.End.AttemptID != "new" {
		t.Fatalf("expired attempt could not be retried: %+v %v", current.End, err)
	}
	if err := st.FinishAssistantSessionEnd(old, nil, ""); err == nil {
		t.Fatal("an old worker ended a newer attempt")
	}
	staleLease := lease
	staleLease.Epoch++
	if err := st.FinishAssistantSessionEnd(current, map[string]*domain.WarmWorkspaceLease{run.ID: &staleLease}, ""); !errors.Is(err, ErrConversationWorkspaceChanged) {
		t.Fatalf("changed epoch was not rejected: %v", err)
	}
	if err := st.FinishAssistantSessionEnd(current, map[string]*domain.WarmWorkspaceLease{run.ID: &lease}, ""); err != nil {
		t.Fatal(err)
	}
	stored, _ := st.AssistantSession(session.UserID, session.ID)
	if stored.End.State != "ended" || len(stored.Messages) != 1 {
		t.Fatalf("retry lost session: %+v", stored)
	}
}
