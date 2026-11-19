package repo

import (
	"agentflow/internal/domain"
	"errors"
	"slices"
)

func (r *RunnersRepo) WarmWorkspaceActor(l domain.WarmWorkspaceLease) (int64, error) {
	var row warmWorkspaceRow
	err := r.DB.Take(&row, "owner_run_id = ?", l.OwnerRunID).Error
	if err == nil && !matchesWarmLease(row, l) {
		return 0, errors.New("stale warm workspace actor")
	}
	return row.LastActorID, err
}
func (r *RunnersRepo) RecordWarmWorkspaceActor(l domain.WarmWorkspaceLease, id int64) error {
	if id <= 0 {
		return errors.New("missing workspace actor")
	}
	res := r.DB.Model(&warmWorkspaceRow{}).Where("owner_run_id = ? AND runner_id = ? AND generation = ? AND epoch = ?", l.OwnerRunID, l.RunnerID, l.Generation, l.Epoch).Update("last_actor_id", id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		current, err := r.WarmWorkspaceActor(l)
		if err != nil {
			return err
		}
		if current != id {
			return errors.New("workspace owner changed")
		}
	}
	return nil
}
func (r *RunnersRepo) WarmWorkspaceHasWaiter(runner, project string, repositories []domain.RepositoryContext) (bool, error) {
	var rows []warmWorkspaceRow
	if err := r.DB.Select("repository_ids").Where("runner_id = ? AND project_id = ? AND state = ?", runner, project, domain.WarmWorkspaceQueued).Find(&rows).Error; err != nil {
		return false, err
	}
	for _, queued := range rows {
		for _, r := range repositories {
			if slices.Contains(queued.RepositoryIDs, r.RepositoryID) {
				return true, nil
			}
		}
	}
	return false, nil
}
func (r *RunnersRepo) WarmWorkspaceReturnCandidates() ([]string, error) {
	var ids []string
	err := r.DB.Model(&warmWorkspaceRow{}).Where("state IN ?", []domain.WarmWorkspaceState{domain.WarmWorkspaceReady, domain.WarmWorkspaceReleasing}).Order("queued_at, owner_run_id").Pluck("owner_run_id", &ids).Error
	return ids, err
}

func (r *RunnersRepo) RetainWarmWorkspaceOwner(l domain.WarmWorkspaceLease) error {
	return r.DB.Model(&warmWorkspaceRow{}).Where("owner_run_id = ? AND runner_id = ? AND generation = ? AND epoch = ? AND state = ?", l.OwnerRunID, l.RunnerID, l.Generation, l.Epoch, domain.WarmWorkspaceReleasing).Where("ready_generation = generation").Where("NOT EXISTS (?)", r.DB.Model(&warmWorkspaceRefRow{}).Select("1").Where("run_id = ? AND generation = ? AND epoch = ?", l.OwnerRunID, l.Generation, l.Epoch)).Update("state", domain.WarmWorkspaceReady).Error
}
