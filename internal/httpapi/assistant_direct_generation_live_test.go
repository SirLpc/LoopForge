package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"github.com/gin-gonic/gin"
)

// A synchronous shared turn has no live stream, but it is just as alive while
// this process runs it: a poll or a session read must not reap it after one
// slow heartbeat, and the sidebar must keep showing it as running.
func TestDirectSharedGenerationIsNotReapedByItsOwnProcess(t *testing.T) {
	h, u := workbenchFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := assistantRequest{ProjectID: "workbench", SessionID: "direct-shared", Messages: []domain.AssistantMessage{{Role: "user", Content: "hi", CreatedAt: time.Now().UTC()}}}
	g, _, finish, err := h.claimDirectGeneration(ctx, req, u.ID, u, domain.AssistantSession{ID: req.SessionID, ProjectID: req.ProjectID, Messages: req.Messages}, cancel)
	if err != nil {
		t.Fatal(err)
	}
	if !generationExecutingHere(g.ID) {
		t.Fatal("direct shared turn not registered as executing here")
	}
	if err = h.repo.DB.Exec("UPDATE assistant_generations SET updated_at = ? WHERE session_id = ?", time.Now().Add(-time.Minute).UTC(), "direct-shared").Error; err != nil {
		t.Fatal(err)
	}
	c, rec := workbenchContext(u, "GET", "/api/assistant/sessions/direct-shared/generation", bytes.NewBuffer(nil))
	c.Params = gin.Params{{Key: "id", Value: "direct-shared"}}
	h.generation(c)
	var polled domain.AssistantGeneration
	if err = json.Unmarshal(rec.Body.Bytes(), &polled); err != nil || rec.Code != 200 || polled.Status != "running" {
		t.Fatalf("poll reaped a live direct turn: %d %s", rec.Code, rec.Body)
	}
	if got, ok := readAssistantGeneration(h.repo, u.ID, "direct-shared"); !ok || got.Status != "running" {
		t.Fatalf("read reaped a live direct turn: %+v", got)
	}
	c, rec = workbenchContext(u, "GET", "/api/assistant/sessions?projectId=workbench", bytes.NewBuffer(nil))
	h.listSessions(c)
	var list []domain.AssistantSession
	if err = json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	for _, s := range list {
		if s.ID == "direct-shared" && s.GenerationStatus != "running" {
			t.Fatalf("list shows %q for a live direct turn", s.GenerationStatus)
		}
	}
	finish(true)
	if generationExecutingHere(g.ID) {
		t.Fatal("finished direct shared turn still registered")
	}
	if stored, _ := h.repo.PeekAssistantGeneration(u.ID, "direct-shared"); stored.Status != "completed" {
		t.Fatalf("direct shared turn stored %q", stored.Status)
	}
}

func TestDirectChatClaimsGenerationForSessionEnd(t *testing.T) {
	h, user := workbenchFixture(t)
	entered := make(chan struct{})
	h.generate = func(ctx context.Context, _ string, _ domain.User) (string, error) {
		close(entered)
		<-ctx.Done()
		return "", ctx.Err()
	}
	const id = "direct-personal-end"
	body, _ := json.Marshal(assistantRequest{SessionID: id, Messages: []domain.AssistantMessage{{Role: "user", Content: "正在处理"}}})
	c, _ := workbenchContext(user, "POST", "/api/assistant/chat", bytes.NewBuffer(body))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	c.Request = c.Request.WithContext(context.WithValue(ctx, requestUserContextKey{}, domainUserToAuth(user)))
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.chat(c)
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("chat did not start")
	}
	g, ok := h.repo.PeekAssistantGeneration(user.ID, id)
	if !ok || g.Status != "running" {
		t.Fatal("direct personal chat did not claim the durable generation")
	}
	if err := h.repo.SteerAssistantGeneration(user.ID, id, g.ID, domain.AssistantSteer{Content: "稍后发送"}); err == nil {
		t.Fatal("synchronous turn acknowledged a follow-up it cannot run")
	}
	if err := h.repo.CancelAssistantGeneration(user.ID, id, g.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("direct chat did not observe cancellation")
	}
	g, _ = h.repo.PeekAssistantGeneration(user.ID, id)
	if g.Status != "cancelled" {
		t.Fatalf("stopped direct turn has status %q", g.Status)
	}
}

type directChatObservation struct {
	gin.ResponseWriter
	once  sync.Once
	ready chan struct{}
}

func (w *directChatObservation) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if bytes.Contains(p, []byte("同步输出片段")) {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func TestEndKeepsDirectChatStreamedAnswer(t *testing.T) {
	h, user := workbenchFixture(t)
	bin := filepath.Join(t.TempDir(), "codebuddy")
	script := `#!/bin/sh
printf '%s\n' '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"同步输出片段"}}}'
exec sleep 30
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_CODEBUDDY_BIN", bin)
	const id = "direct-sse-end"
	body, _ := json.Marshal(assistantRequest{SessionID: id, Messages: []domain.AssistantMessage{{Role: "user", Content: "保留已输出内容"}}})
	c, _ := workbenchContext(user, "POST", "/api/assistant/chat", bytes.NewBuffer(body))
	ctx, cancel := context.WithCancel(c.Request.Context())
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	c.Request.Header.Set("Accept", "text/event-stream")
	w := &directChatObservation{ResponseWriter: c.Writer, ready: make(chan struct{})}
	c.Writer = w
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.chat(c)
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-w.ready:
	case <-time.After(5 * time.Second):
		t.Fatal("the direct SSE stream did not emit the partial answer")
	}
	requestChatEnd(t, h, user, id, 202)
	ended := waitForChatEnd(t, h, domain.AssistantGeneration{SessionID: id, UserID: user.ID}, "ended")
	if len(ended.Messages) != 2 || !bytes.Contains([]byte(ended.Messages[1].Content), []byte("同步输出片段")) {
		t.Fatalf("partial answer lost from ended history: %+v", ended.Messages)
	}
}

func TestDirectChatLinksFirstMentionedIssue(t *testing.T) {
	h, user := workbenchFixture(t)
	h.generate = func(context.Context, string, domain.User) (string, error) {
		return `{"message":"已读","drafts":[]}`, nil
	}
	const id = "direct-issue-link"
	issue := domain.Issue{ID: "first-mention", ProjectID: "workbench", Title: "第一项需求"}
	if err := h.repo.PutIssue(issue); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.PutAssistantSession(user.ID, domain.AssistantSession{ID: id, ProjectID: issue.ProjectID}); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(assistantRequest{SessionID: id, ProjectID: issue.ProjectID, Messages: []domain.AssistantMessage{{
		Role: "user", Content: "@第一项需求 请查看",
		References: []domain.AssistantReference{{Type: "issue", ID: issue.ID, ProjectID: issue.ProjectID}},
	}}})
	c, w := workbenchContext(user, "POST", "/api/assistant/chat", bytes.NewBuffer(body))
	h.chat(c)
	s, _ := h.repo.AssistantSession(user.ID, id)
	if w.Code != 200 || s.IssueID != issue.ID || len(s.Messages) != 2 {
		t.Fatalf("direct issue chat failed: %d %s, issue %q", w.Code, w.Body.String(), s.IssueID)
	}
}

func TestDirectPersonalChatAcceptsPlainHistory(t *testing.T) {
	h, user := workbenchFixture(t)
	h.generate = func(context.Context, string, domain.User) (string, error) {
		return `{"message":"第一轮回复","drafts":[]}`, nil
	}
	const id = "direct-plain-history"
	for _, messages := range [][]domain.AssistantMessage{
		{{Role: "user", Content: "第一轮"}},
		{{Role: "user", Content: "第一轮"}, {Role: "assistant", Content: "第一轮回复"}, {Role: "user", Content: "第二轮"}},
	} {
		body, _ := json.Marshal(assistantRequest{SessionID: id, Messages: messages})
		c, w := workbenchContext(user, "POST", "/api/assistant/chat", bytes.NewBuffer(body))
		h.chat(c)
		if w.Code != 200 {
			t.Fatalf("plain direct chat failed: %d %s", w.Code, w.Body.String())
		}
	}
}
