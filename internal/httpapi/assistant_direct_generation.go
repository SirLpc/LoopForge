package httpapi

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"agentflow/internal/domain"
)

// Direct chat occupies the same durable slot as /generations.
// Otherwise a second process can save a pending turn while the first CLI writes
// the shared transcript. A local file lock additionally fences stale writers.
//
// The caller must defer the returned finish right after a successful claim (as
// chat does), including when the handler panics.
func (h *assistantHandlers) claimDirectGeneration(ctx context.Context, req assistantRequest, ownerID int64, actor domain.User, pending domain.AssistantSession, cancel context.CancelFunc) (domain.AssistantGeneration, *assistantGenerationRecorder, func(bool), error) {
	g := domain.AssistantGeneration{ID: newAssistantSessionID(), SessionID: req.SessionID, UserID: ownerID, ActorID: actor.ID, ActorName: actor.LoginName, ProjectID: req.ProjectID, Status: "running", Direct: true, StartedAt: time.Now().UTC(), MessageCount: len(req.Messages)}
	if err := h.repo.BeginAssistantGeneration(g, pending); err != nil {
		return g, nil, nil, err
	}
	recorder := newAssistantGenerationRecorder(g, cancel)
	diagnostics := recorder.diagnostics
	untrack := inflightTasks.Add()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, ok := h.repo.PeekAssistantGeneration(ownerID, req.SessionID)
				if !ok || current.ID != g.ID {
					diagnostics.terminate("GENERATION_SUPERSEDED", "当前生成记录已被替换或移除", actor)
					cancel()
					return
				}
				if current.Status != "running" {
					diagnostics.terminate("GENERATION_STATUS_CHANGED", "生成状态已变更为 "+current.Status, actor)
					cancel()
					return
				}
				if current.CancelRequested {
					diagnostics.terminate("USER_CANCELLED", "用户主动停止了本轮生成", actor)
					cancel()
					return
				}
				if err := h.checkpointRunningGeneration(recorder.snapshot(), diagnostics); err != nil {
					diagnostics.terminate("CHECKPOINT_FAILED", "保存生成检查点失败："+err.Error(), actor)
					cancel()
					return
				}
			}
		}
	}()
	return g, recorder, func(completed bool) {
		defer untrack()
		defer recorder.close()
		close(stop)
		<-done
		g := recorder.snapshot()
		if dest, ok := ctx.Value(warmGenerationHandoffKey{}).(**warmConversationHandoff); ok && *dest != nil {
			g.MessageCount = len(req.Messages)
			if err := h.parkWarmGeneration(g, req, *dest, diagnostics.snapshot(g)); err == nil {
				return
			} else {
				g.Error = err.Error()
			}
		}
		g.Status = "failed"
		if completed {
			g.Status = "completed"
		} else {
			current, ok := h.repo.PeekAssistantGeneration(ownerID, req.SessionID)
			if ctx.Err() != nil || ok && current.ID == g.ID && current.CancelRequested {
				g.Status = "cancelled"
				g.Error = "生成已停止"
				g.Failure = nil
			}
		}
		if g.Status == "failed" {
			g.Failure = diagnostics.snapshot(g).Failure
			if g.Failure != nil {
				g.Error = g.Failure.Message
			}
		}
		if g.Status == "cancelled" {
			if err := h.commitTurnTranscript(g, nil, true); err != nil {
				log.Printf("keep direct assistant transcript id=%s: %v", g.ID, err)
			}
		}
		_ = h.repo.CheckpointAssistantGeneration(g, diagnostics.snapshot(g))
		recorder.mu.Lock()
		recorder.g = g
		recorder.mu.Unlock()
		recorder.live.publish()
	}, nil
}

func directAssistantTranscriptPrefix(saved, incoming []domain.AssistantMessage) bool {
	if len(incoming) < len(saved) {
		return false
	}
	for i, old := range saved {
		next := incoming[i]
		if old.Role != next.Role {
			return false
		}
		if old.Content == next.Content {
			continue
		}
		var reply assistantReply
		if old.Role != "assistant" || json.Unmarshal([]byte(old.Content), &reply) != nil || reply.Message != next.Content {
			return false
		}
	}
	return true
}
