package repo

import (
	"maps"
	"slices"

	"agentflow/internal/domain"
)

// Status is projected from the durable owner queue. Lists use one query, and
// ordinary checkout runs need no additional query.
func (r *RunnersRepo) populateWarmWorkspaceWaiting(runs []domain.Run) error {
	owners := map[string][]int{}
	for i, run := range runs {
		if run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool || run.Status != "running" || run.CancelRequested {
			continue
		}
		id := run.ExecutionContext.WorkspaceRunID
		if id == "" {
			id = run.ID
		}
		owners[id] = append(owners[id], i)
	}
	if len(owners) == 0 {
		return nil
	}
	ids := slices.Collect(maps.Keys(owners))
	var waiting []string
	if err := r.DB.Model(&warmWorkspaceRow{}).Where("owner_run_id IN ? AND state = ?", ids, domain.WarmWorkspaceQueued).Pluck("owner_run_id", &waiting).Error; err != nil {
		return err
	}
	for _, id := range waiting {
		for _, i := range owners[id] {
			runs[i].WarmWorkspaceWaiting = true
		}
	}
	return nil
}
