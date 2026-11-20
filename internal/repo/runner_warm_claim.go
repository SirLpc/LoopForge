package repo

import (
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
)

// RunnerWarmTaskAvailable checks the queue for a dedicated preheated Runner.
func (r *RunnersRepo) RunnerWarmTaskAvailable(runnerID string, providers []string) (bool, error) {
	return r.runnerTaskAvailable(runnerID, providers, true, true)
}

// ClaimRunnerWarmTask leaves checkout jobs for ordinary Runners.
func (r *RunnersRepo) ClaimRunnerWarmTask(runnerID string, providers []string, now time.Time, leaseTTL time.Duration) (domain.RunnerTask, bool, error) {
	return r.claimRunnerTask(runnerID, providers, now, leaseTTL, true, true)
}

func warmRunnerTaskQuery(query *gorm.DB, runnerID string) *gorm.DB {
	return query.Where("runner_id = ? AND LOWER(JSON_TYPE(JSON_EXTRACT(payload, '$.warmWorkspaceLease'))) = 'object'", runnerID)
}
