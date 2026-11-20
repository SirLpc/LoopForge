package httpapi

import (
	"time"

	"agentflow/internal/domain"
	"github.com/gin-gonic/gin"
)

func (h *runnersHandlers) runnerWarmTaskClaim(c *gin.Context) {
	h.runnerTaskClaimScoped(c, true)
}

func (h *runnersHandlers) claimDedicatedWarmTask(runnerID string, providers []string, preflight bool) (domain.RunnerTask, bool, error) {
	if preflight {
		available, err := h.repo.RunnerWarmTaskAvailable(runnerID, providers)
		return domain.RunnerTask{}, available, err
	}
	return h.repo.ClaimRunnerWarmTask(runnerID, providers, time.Now().UTC(), runnerTaskLeaseTTL)
}
