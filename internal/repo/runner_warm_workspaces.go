package repo

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type warmWorkspaceRow struct {
	OwnerRunID    string                    `gorm:"primaryKey;size:64"`
	RunnerID      string                    `gorm:"index:idx_warm_queue,priority:1;size:64;not null"`
	ProjectID     string                    `gorm:"size:64;not null"`
	RepositoryIDs []string                  `gorm:"serializer:json;type:json;not null"`
	QueuedAt      time.Time                 `gorm:"index:idx_warm_queue,priority:3;not null"`
	State         domain.WarmWorkspaceState `gorm:"index:idx_warm_queue,priority:2;size:16;not null"`
	Generation    uint64
	Slots         map[string]string `gorm:"serializer:json;type:json"`
}

func validateWarmInventory(slots []domain.WarmWorkspaceSlot) error {
	seen := make(map[string]bool, len(slots))
	for _, slot := range slots {
		if strings.TrimSpace(slot.ID) == "" || strings.TrimSpace(slot.ProjectID) == "" || strings.TrimSpace(slot.RepositoryID) == "" || seen[slot.ID] {
			return errors.New("invalid or duplicate warm workspace inventory slot")
		}
		seen[slot.ID] = true
	}
	return nil
}

func (warmWorkspaceRow) TableName() string { return "runner_warm_workspaces" }
func (row warmWorkspaceRow) lease() domain.WarmWorkspaceLease {
	return domain.WarmWorkspaceLease{OwnerRunID: row.OwnerRunID, RunnerID: row.RunnerID, Generation: row.Generation, Slots: row.Slots}
}

func matchesWarmLease(row warmWorkspaceRow, l domain.WarmWorkspaceLease) bool {
	return row.OwnerRunID == l.OwnerRunID && row.RunnerID == l.RunnerID && row.Generation != 0 && row.Generation == l.Generation && maps.Equal(row.Slots, l.Slots)
}

func (r *RunnersRepo) EnqueueWarmWorkspace(d domain.WarmWorkspaceDemand, now time.Time) error {
	ids := slices.Clone(d.RepositoryIDs)
	slices.Sort(ids)
	if strings.TrimSpace(d.OwnerRunID) == "" || strings.TrimSpace(d.RunnerID) == "" || strings.TrimSpace(d.ProjectID) == "" || len(ids) == 0 || ids[0] == "" || len(slices.Compact(slices.Clone(ids))) != len(ids) {
		return errors.New("invalid warm workspace demand")
	}
	row := warmWorkspaceRow{OwnerRunID: d.OwnerRunID, RunnerID: d.RunnerID, ProjectID: d.ProjectID, RepositoryIDs: ids, QueuedAt: now, State: domain.WarmWorkspaceQueued}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		var old warmWorkspaceRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&old, "owner_run_id = ?", d.OwnerRunID).Error; err != nil {
			return err
		}
		if old.RunnerID != d.RunnerID || old.ProjectID != d.ProjectID || !slices.Equal(old.RepositoryIDs, ids) {
			return errors.New("workspace owner already has another demand")
		}
		return nil
	})
}

func (r *RunnersRepo) ReserveNextWarmWorkspace(runnerID string, now time.Time) (domain.WarmWorkspaceLease, bool, error) {
	var lease domain.WarmWorkspaceLease
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var machine runnerRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&machine, "id = ?", runnerID).Error; err != nil {
			return err
		}
		if machine.Status != domain.RunnerStatusOnline || !(domain.Runner{RunnerVersion: machine.RunnerVersion}).SupportsWarmWorkspace() {
			return nil
		}
		var rows []warmWorkspaceRow
		if err := tx.Where("runner_id = ? AND state NOT IN ?", runnerID, []domain.WarmWorkspaceState{domain.WarmWorkspaceReleased, domain.WarmWorkspaceCanceled}).Order("queued_at,owner_run_id").Find(&rows).Error; err != nil {
			return err
		}
		used := map[string]bool{}
		for _, row := range rows {
			for _, id := range row.Slots {
				used[id] = true
			}
		}
		for _, row := range rows {
			if row.State != domain.WarmWorkspaceQueued {
				continue
			}
			slots := map[string]string{}
			for _, slot := range machine.WarmSlots {
				if slot.State == "available" && slot.ProjectID == row.ProjectID && !used[slot.ID] && slices.Contains(row.RepositoryIDs, slot.RepositoryID) && slots[slot.RepositoryID] == "" {
					slots[slot.RepositoryID] = slot.ID
				}
			}
			if len(slots) != len(row.RepositoryIDs) {
				continue
			}
			// Explicit deregistration removes the machine row. Its durable leases
			// still fence the original journal if it registers under the same ID.
			if machine.WarmGeneration == 0 {
				if err := tx.Model(&warmWorkspaceRow{}).Where("runner_id = ?", runnerID).Select("COALESCE(MAX(generation), 0)").Scan(&machine.WarmGeneration).Error; err != nil {
					return err
				}
			}
			generation := machine.WarmGeneration + 1
			if generation == 0 {
				return errors.New("warm workspace generation exhausted")
			}
			res := tx.Model(&row).Where("state = ?", domain.WarmWorkspaceQueued).Updates(map[string]any{"state": domain.WarmWorkspaceReserved, "generation": generation, "slots": string(jsonBytes(slots))})
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected == 0 {
				continue
			}
			if err := tx.Model(&machine).Update("warm_generation", generation).Error; err != nil {
				return err
			}
			lease = row.lease()
			lease.Generation = generation
			lease.Slots = slots
			break
		}
		return nil
	})
	if err != nil {
		return domain.WarmWorkspaceLease{}, false, err
	}
	return lease, lease.Generation != 0, nil
}

func (r *RunnersRepo) WarmWorkspaceAllocation(ownerRunID string) (domain.WarmWorkspaceLease, domain.WarmWorkspaceState, bool, error) {
	var row warmWorkspaceRow
	err := r.DB.Take(&row, "owner_run_id = ?", ownerRunID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return domain.WarmWorkspaceLease{}, "", false, nil
	}
	if err != nil {
		return domain.WarmWorkspaceLease{}, "", false, err
	}
	return row.lease(), row.State, true, nil
}

func (r *RunnersRepo) CancelWarmWorkspace(ownerRunID string, now time.Time) error {
	// A reservation may already be on disk, even if its acknowledgement was lost.
	return r.DB.Model(&warmWorkspaceRow{}).Where("owner_run_id = ? AND state IN ?", ownerRunID, []domain.WarmWorkspaceState{domain.WarmWorkspaceQueued, domain.WarmWorkspaceReserved, domain.WarmWorkspaceReady, domain.WarmWorkspaceBlocked}).Update("state", gorm.Expr("CASE WHEN state = ? THEN ? ELSE ? END", domain.WarmWorkspaceQueued, domain.WarmWorkspaceCanceled, domain.WarmWorkspaceReleasing)).Error
}

func (r *RunnersRepo) SettleWarmWorkspace(l domain.WarmWorkspaceLease, state domain.WarmWorkspaceState) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var row warmWorkspaceRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "owner_run_id = ?", l.OwnerRunID).Error; err != nil {
			return err
		}
		if !matchesWarmLease(row, l) {
			return errors.New("stale warm workspace lease")
		}
		if row.State == state {
			return nil
		}
		var allowed bool
		switch state {
		case domain.WarmWorkspaceReady:
			allowed = row.State == domain.WarmWorkspaceReserved
		case domain.WarmWorkspaceBlocked:
			allowed = row.State == domain.WarmWorkspaceReserved || row.State == domain.WarmWorkspaceReleasing || row.State == domain.WarmWorkspaceReady
		case domain.WarmWorkspaceReleasing:
			allowed = row.State == domain.WarmWorkspaceReady || row.State == domain.WarmWorkspaceBlocked || row.State == domain.WarmWorkspaceReserved
		case domain.WarmWorkspaceReleased:
			allowed = row.State == domain.WarmWorkspaceReleasing
		}
		if !allowed {
			return fmt.Errorf("invalid warm workspace transition %s to %s", row.State, state)
		}
		return tx.Model(&row).Update("state", state).Error
	})
}

func (r *RunnersRepo) AuthorizeWarmWorkspaceControl(runnerID string, req *domain.RunnerWorkspaceRequest) error {
	if req.WarmWorkspaceLease == nil || req.WorkspaceRunID != "" && req.WorkspaceRunID != req.RunID {
		return errors.New("missing or mismatched workspace owner")
	}
	runner, ok := r.Runner(runnerID)
	if !ok || !runner.SupportsWarmWorkspace() {
		return errors.New("runner lacks warm workspace support")
	}
	var row warmWorkspaceRow
	if err := r.DB.Take(&row, "owner_run_id = ?", req.RunID).Error; err != nil {
		return err
	}
	if runnerID != row.RunnerID || !matchesWarmLease(row, *req.WarmWorkspaceLease) {
		return errors.New("stale warm workspace control request")
	}
	var allowed bool
	switch req.Operation {
	case domain.RunnerWorkspaceOperationWarmPrepare:
		allowed = row.State == domain.WarmWorkspaceReserved
	case domain.RunnerWorkspaceOperationWarmStatus:
		allowed = row.Generation != 0
	case domain.RunnerWorkspaceOperationWarmReleaseCheck, domain.RunnerWorkspaceOperationWarmReleaseCommit:
		allowed = row.State == domain.WarmWorkspaceReleasing
	}
	if !allowed {
		return fmt.Errorf("workspace operation %q unavailable in %s", req.Operation, row.State)
	}
	canonical := row.lease()
	req.WarmWorkspaceLease = &canonical
	return nil
}
