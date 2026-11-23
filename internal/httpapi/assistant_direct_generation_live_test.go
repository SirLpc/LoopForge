package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
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
