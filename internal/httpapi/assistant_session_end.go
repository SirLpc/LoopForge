package httpapi

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const assistantEndDrainBudget = 2 * time.Minute

func (h *assistantHandlers) endSession(c *gin.Context) {
	user, ok := h.requestUser(c.Request, true)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录"})
		return
	}
	id := c.Param("id")
	ownerID, _, err := h.conversationScope(c, id, "", "", user.ID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	}
	attemptID := newAssistantSessionID()
	session, generationID, err := h.repo.BeginAssistantSessionEnd(ownerID, id, attemptID)
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
		return
	case errors.Is(err, repo.ErrAssistantSessionEndUnsupported):
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return
	case err != nil:
		log.Printf("begin assistant session end id=%s: %v", id, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法开始结束会话，请重试"})
		return
	}

	status := http.StatusAccepted
	if session.End.State == domain.AssistantSessionEnded {
		status = http.StatusOK
	}
	if session.End.AttemptID == attemptID {
		cancelLiveAssistantGeneration(generationID)
		parent := context.WithoutCancel(c.Request.Context())
		done := inflightTasks.Add()
		go func() {
			defer done()
			h.completeSessionEnd(parent, session)
		}()
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(status, h.assistantSessionEndView(session))
}

func (h *assistantHandlers) completeSessionEnd(ctx context.Context, session domain.AssistantSession) {
	err := h.finalizeSessionEnd(ctx, session)
	if err == nil || errors.Is(err, repo.ErrAssistantSessionEndChanged) {
		return
	}
	message := err.Error()
	user, _ := h.repo.User(session.UserID)
	message = truncateRunes(redactAssistantSecrets(message, user), 500)
	if saveErr := h.repo.FinishAssistantSessionEnd(session, nil, message); saveErr != nil &&
		!errors.Is(saveErr, repo.ErrAssistantSessionEndChanged) {
		log.Printf("complete assistant session end id=%s: %v", session.ID, saveErr)
	}
}

func (h *assistantHandlers) finalizeSessionEnd(ctx context.Context, session domain.AssistantSession) error {
	remaining, err := h.repo.AssistantSessionEndRemaining(session.End)
	if err != nil {
		return err
	}
	if remaining <= 0 {
		return repo.ErrAssistantSessionEndChanged
	}
	ctx, stop := context.WithTimeout(ctx, remaining)
	defer stop()
	drainUntil := time.Now().Add(assistantEndDrainBudget)
	drainCtx, stopDrain := context.WithDeadline(ctx, drainUntil)
	settled, err := h.waitForSessionEndDrain(drainCtx, session)
	stopDrain()
	if err != nil {
		return err
	}
	checkAttempt := func() error {
		_, _, err := h.repo.AssistantSessionEndReady(session.UserID, session.ID, session.End.AttemptID)
		return err
	}
	leases, err := h.returnConversationWarmWorkspaces(ctx, settled, drainUntil, checkAttempt)
	if err != nil {
		return err
	}
	if err := checkAttempt(); err != nil {
		return err
	}
	// Legacy replies may still refer to files kept in the assistant's local
	// workspace. Persist their attachment IDs before making reads immutable.
	materialized := settled
	materialized.Messages = append([]domain.AssistantMessage(nil), settled.Messages...)
	if materializeWorkspaceImages(settled.UserID, &materialized) {
		return h.repo.FinishAssistantSessionEnd(settled, leases, "", materialized.Messages)
	}
	return h.repo.FinishAssistantSessionEnd(settled, leases, "")
}

func (h *assistantHandlers) waitForSessionEndDrain(ctx context.Context, session domain.AssistantSession) (domain.AssistantSession, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		// Allow the existing stale-turn recovery to reap the process, then
		// inspect the authoritative generation under this attempt.
		h.readGeneration(session.UserID, session.ID)
		settled, g, err := h.repo.AssistantSessionEndReady(session.UserID, session.ID, session.End.AttemptID)
		if err != nil {
			return settled, err
		}
		_, admitting := h.warmAdmissions.Load(g.ID)
		if g.Status != "running" && !generationExecutingHere(g.ID) && !admitting {
			if g.Status == "interrupted" || g.Status == "cancelled" || g.Status == "failed" {
				appended, err := h.persistInterruptedReply(settled, g)
				if err != nil {
					return settled, err
				}
				if appended {
					return h.repo.AssistantSessionForDeletion(session.UserID, session.ID)
				}
			}
			return settled, nil
		}
		select {
		case <-ctx.Done():
			return settled, errors.New("回复仍在退出，请稍后重试结束会话")
		case <-ticker.C:
		}
	}
}

func (h *assistantHandlers) assistantSessionEndView(session domain.AssistantSession) domain.AssistantSession {
	if session.End != nil && session.End.State == domain.AssistantSessionEnding {
		remaining, err := h.repo.AssistantSessionEndRemaining(session.End)
		if err == nil && remaining <= 0 {
			session.End = &domain.AssistantSessionEnd{State: domain.AssistantSessionEndFailed,
				AttemptID: session.End.AttemptID, Error: "结束会话超时，请重试"}
		}
	}
	return session
}

// Browser-confirmed operations carry their originating chat in a header.
// Admit them before MCP dispatch so a stale page cannot execute an old draft.
func (h *assistantHandlers) admitChatOperation(c *gin.Context, id string) bool {
	user, ok := h.requestUser(c.Request, true)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录"})
		return false
	}
	ownerID, _, err := h.conversationScope(c, id, "", "", user.ID)
	if err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return false
	}
	err = h.repo.AssistantSessionWriteAccess(ownerID, id)
	switch {
	case err == nil:
		return true
	case errors.Is(err, repo.ErrAssistantSessionEnding), errors.Is(err, repo.ErrAssistantSessionEnded):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "会话不存在"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取会话状态"})
	}
	return false
}
