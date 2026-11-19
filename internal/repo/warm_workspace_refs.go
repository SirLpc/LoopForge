package repo

import (
	"agentflow/internal/domain"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"reflect"
	"regexp"
	"time"
)

type warmWorkspaceRefRow struct {
	RunID             string `gorm:"primaryKey;size:64"`
	Epoch, Generation uint64
	Unplaced          bool
	Refs              []domain.WarmWorkspaceRef `gorm:"serializer:json;type:json;not null"`
}

func (warmWorkspaceRefRow) TableName() string { return "run_warm_workspace_refs" }

var warmRefOID = regexp.MustCompile("^[a-f0-9]{40}([a-f0-9]{24})?$")

func (r *RunsRepo) RecordWarmWorkspaceRefs(id string, l domain.WarmWorkspaceLease, refs []domain.WarmWorkspaceRef) error {
	if id != l.OwnerRunID || len(refs) > 0 && len(refs) != len(l.Slots) {
		return errors.New("incomplete warm workspace proof")
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref.RepositoryID] || l.Slots[ref.RepositoryID] == "" || !warmRefOID.MatchString(ref.Commit) {
			return errors.New("invalid warm workspace reference")
		}
		seen[ref.RepositoryID] = true
	}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row warmWorkspaceRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "owner_run_id = ?", id).Error; err != nil {
			return err
		}
		if !matchesWarmLease(row, l) || row.State != domain.WarmWorkspaceReleasing {
			return errors.New("stale warm workspace proof")
		}
		if len(refs) == 0 && row.ReadyGeneration == l.Generation {
			return errors.New("missing return references for prepared workspace")
		}
		var existing warmWorkspaceRefRow
		err := tx.Take(&existing, "run_id = ?", id).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && existing.Generation == l.Generation && existing.Epoch == l.Epoch {
			if existing.Unplaced != (len(refs) == 0) || !existing.Unplaced && !reflect.DeepEqual(existing.Refs, refs) {
				return errors.New("different return proof already recorded")
			}
			return nil
		}
		receipt := warmWorkspaceRefRow{RunID: id, Generation: l.Generation, Epoch: l.Epoch, Refs: refs}
		if len(refs) == 0 {
			receipt.Unplaced = true
			receipt.Refs = existing.Refs
			if receipt.Refs == nil {
				receipt.Refs = []domain.WarmWorkspaceRef{}
			}
		}
		return tx.Save(&receipt).Error
	})
}

func (r *RunsRepo) WarmWorkspaceRefs(id string) ([]domain.WarmWorkspaceRef, error) {
	var row warmWorkspaceRefRow
	err := r.DB.Take(&row, "run_id = ?", id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return row.Refs, err
}

// A confirmed directory move and its run-visible cleanup marker share one
// transaction. Restore status and a subsequent admission see the same epoch.
func (r *RunsRepo) ConfirmWarmWorkspaceBinding(l domain.WarmWorkspaceLease, state domain.WarmWorkspaceState) error {
	if state != domain.WarmWorkspaceReady && state != domain.WarmWorkspaceReleased {
		return errors.New("invalid binding phase")
	}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if _, err := r.takeRunForUpdate(tx, l.OwnerRunID); err != nil {
			return err
		}
		var cleaned *time.Time
		if state == domain.WarmWorkspaceReleased {
			var proof warmWorkspaceRefRow
			if err := tx.Take(&proof, "run_id = ? AND epoch = ? AND generation = ?", l.OwnerRunID, l.Epoch, l.Generation).Error; err != nil {
				return err
			}
			now := time.Now().UTC()
			cleaned = &now
		}
		if err := r.runners.settleWarmWorkspace(tx, l, state); err != nil {
			return err
		}
		return tx.Model(&runRow{}).Where("id = ?", l.OwnerRunID).Update("workspace_cleaned_at", cleaned).Error
	})
}

func (r *RunsRepo) HasWarmWorkspaceReturnReceipt(l domain.WarmWorkspaceLease) (bool, error) {
	var count int64
	err := r.DB.Model(&warmWorkspaceRefRow{}).Where("run_id = ? AND epoch = ? AND generation = ?", l.OwnerRunID, l.Epoch, l.Generation).Count(&count).Error
	return count == 1, err
}

func (r *RunsRepo) WarmWorkspaceBorrowed(id string, now time.Time) (bool, error) {
	var count int64
	err := r.DB.Model(&runRow{}).Where(r.sqlExecutionContextJSONText("workspaceRunId")+" = ?", id).
		Where("status IN ? OR workspace_edit_until > ?", []string{"running", "queued", "waiting"}, now).Count(&count).Error
	return count != 0, err
}

func (r *RunsRepo) WarmWorkspaceRestorable(id string) (bool, error) {
	var proof warmWorkspaceRefRow
	err := r.DB.Table("run_warm_workspace_refs AS proof").Select("proof.refs").Joins("JOIN "+(warmWorkspaceRow{}).TableName()+" AS slot ON slot.owner_run_id = proof.run_id AND slot.epoch = proof.epoch AND slot.generation = proof.generation").Where("proof.run_id = ? AND slot.state = ?", id, domain.WarmWorkspaceReleased).Take(&proof).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return len(proof.Refs) > 0, err
}
