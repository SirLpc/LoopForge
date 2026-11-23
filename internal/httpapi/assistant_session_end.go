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
	c.JSON(status, assistantSessionEndView(session))
}

func (h *assistantHandlers) completeSessionEnd(ctx context.Context, session domain.AssistantSession) {
	ctx, stop := context.WithDeadline(ctx, *session.End.Deadline)
	defer stop()
	drainUntil := time.Now().Add(assistantEndDrainBudget)
	drainCtx, stopDrain := context.WithDeadline(ctx, drainUntil)
	settled, err := h.waitForSessionEndDrain(drainCtx, session)
	stopDrain()
	if err == nil {
		var leases map[string]*domain.WarmWorkspaceLease
		leases, err = h.returnConversationWarmWorkspaces(ctx, settled, drainUntil)
		if err == nil {
			err = h.repo.FinishAssistantSessionEnd(settled, leases, "")
		}
	}
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

func (h *assistantHandlers) waitForSessionEndDrain(ctx context.Context, session domain.AssistantSession) (domain.AssistantSession, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		// This also reaps stale generations and saves their partial replies.
		g, _ := h.currentGeneration(session.UserID, session.ID)
		settled, ready, err := h.repo.AssistantSessionEndReady(session.UserID, session.ID, session.End.AttemptID)
		if err != nil {
			return settled, err
		}
		_, admitting := h.warmAdmissions.Load(g.ID)
		if ready && !generationExecutingHere(g.ID) && !admitting {
			return settled, nil
		}
		select {
		case <-ctx.Done():
			return settled, errors.New("回复仍在退出，请稍后重试结束会话")
		case <-ticker.C:
		}
	}
}

func assistantSessionEndView(session domain.AssistantSession) domain.AssistantSession {
	if session.End == nil {
		return session
	}
	end := *session.End
	end.AttemptID = ""
	if end.State == domain.AssistantSessionEnding && end.Deadline != nil && !time.Now().Before(*end.Deadline) {
		end = domain.AssistantSessionEnd{State: domain.AssistantSessionEndFailed, Error: "结束会话超时，请重试"}
	}
	session.End = &end
	return session
}
