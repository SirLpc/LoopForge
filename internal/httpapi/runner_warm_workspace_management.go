package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"agentflow/internal/repo"
	"github.com/gin-gonic/gin"
)

func (h *runnersHandlers) runnerWarmWorkspacePool(c *gin.Context) {
	user, ok := h.requestUser(c.Request, true)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "请先登录后管理预热工作区"})
		return
	}
	c.Header("Cache-Control", "no-store")
	ctx, runnerID := c.Request.Context(), c.Param("id")
	if c.Request.Method == http.MethodGet {
		pool, err := h.repo.RunnerWarmWorkspacePool(ctx, runnerID, user.ID)
		if err != nil {
			writeRunnerWarmWorkspaceManagementError(c, err)
			return
		}
		c.JSON(http.StatusOK, pool)
		return
	}

	var input struct {
		SlotID string `json:"slotId"`
		Paused *bool  `json:"paused"`
	}
	if !decode(c, &input) {
		return
	}
	if strings.TrimSpace(input.SlotID) == "" || input.Paused == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请指定预热槽位和暂停状态"})
		return
	}
	if err := h.repo.SetRunnerWarmWorkspaceSlotPaused(ctx, runnerID, user.ID, input.SlotID, *input.Paused); err != nil {
		writeRunnerWarmWorkspaceManagementError(c, err)
		return
	}
	c.Status(http.StatusNoContent)
}

func writeRunnerWarmWorkspaceManagementError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, repo.ErrRunnerNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "执行机器不存在"})
	case errors.Is(err, repo.ErrRunnerWarmWorkspaceForbidden):
		c.JSON(http.StatusForbidden, gin.H{"error": "只能管理自己绑定的执行机器上的预热工作区"})
	case errors.Is(err, repo.ErrRunnerWarmWorkspaceSlotGone):
		c.JSON(http.StatusNotFound, gin.H{"error": "预热槽位未登记"})
	default:
		_ = c.Error(err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "无法读取或更新预热工作区，请稍后重试"})
	}
}
