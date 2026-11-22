package engine

import (
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

// A warm SOP can use another registered machine without changing the user's default.
func (e *Engine) selectPersonalWarmRunner(ec *domain.TelemetryContext, repositories []domain.RepositoryContext, driver string) {
	if ec.ResponsibleUserID <= 0 || ec.ExecutionPool == domain.ExecutionModeCloud || e.cloudPoolRequired(ec.ProjectID) {
		return
	}
	if ec.ExecutionRunnerID != "" {
		r, ok := e.repo.Runner(ec.ExecutionRunnerID)
		if ok && r.SupportsWarmWorkspaceDriver(driver) && r.Status == domain.RunnerStatusOnline && repo.CheckWarmWorkspaceRegistration(r, ec.ProjectID, repositories) == nil {
			return
		}
	}
	// Automatic selection requires a matching origin for every frozen repository.
	for _, source := range repositories {
		if strings.TrimSpace(source.CloneURL) == "" {
			return
		}
	}
	for _, r := range e.repo.Runners() {
		if r.SupportsWarmWorkspaceDriver(driver) && r.OwnerUserID == ec.ResponsibleUserID && r.Status == domain.RunnerStatusOnline && repo.CheckWarmWorkspaceRegistration(r, ec.ProjectID, repositories) == nil {
			ec.ExecutionRunnerID = r.ID
			return
		}
	}
}
