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
