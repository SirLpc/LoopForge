package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"github.com/gin-gonic/gin"
	"net/http"
)

// A child SOP can have an ordinary-looking run header while using its parent's
// pooled checkout. Resolve that ownership in the engine before the operation.
func checkWarmWorkspaceAccess(c *gin.Context, d *deps, run domain.Run) bool {
	if run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool && engine.WorkspaceRunID(run) == run.ID {
		return true
	}
	var actor int64
	if user, ok := d.requestUser(c.Request, true); ok {
		actor = user.ID
	}
	ctx := engine.WithWorkspaceRestoreActor(c.Request.Context(), actor)
	if err := d.engine.CheckWorkspaceRestoreAccess(ctx, run); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
		return false
	}
	c.Request = c.Request.WithContext(ctx)
	return true
}
