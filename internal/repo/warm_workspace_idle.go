package repo

import (
	"agentflow/internal/domain"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row warmWorkspaceRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "owner_run_id = ?", l.OwnerRunID).Error; err != nil {
			return err
		}
		if !matchesWarmLease(row, l) || row.State != domain.WarmWorkspaceReleasing || row.ReadyGeneration != l.Generation {
			return nil
		}
		_, _, found, err := warmReturnReceipt(tx, l)
		if err != nil || found {
			return err
		}
		return tx.Model(&row).Update("state", domain.WarmWorkspaceReady).Error
	})
}

func (r *RunnersRepo) lockReturnedWorkspace(tx *gorm.DB, id string, expected *domain.WarmWorkspaceLease) error {
	var owner runRow
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id", "kind", "workflow_id", "execution_context").Take(&owner, "id = ?", id).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if err == nil && !runFromRow(owner).IsWorkspace() {
		if expected != nil {
			return ErrConversationWorkspaceChanged
		}
		return nil
	}
	var row warmWorkspaceRow
	err = tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "owner_run_id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) && expected == nil {
		return nil
	}
	if err != nil {
		return err
	}
	if expected == nil || !matchesWarmLease(row, *expected) || row.State != domain.WarmWorkspaceReleased && row.State != domain.WarmWorkspaceCanceled {
		return ErrConversationWorkspaceChanged
	}
	if row.State == domain.WarmWorkspaceReleased {
		var receipt warmWorkspaceRefRow
		return tx.Select("run_id").Take(&receipt, "run_id = ? AND epoch = ? AND generation = ?", id, row.Epoch, row.Generation).Error
	}
	return nil
}
