package repo

import (
	"context"
	"errors"
	"maps"
	"slices"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrRunnerWarmWorkspaceForbidden = errors.New("runner warm workspace management requires the machine owner")
	ErrRunnerWarmWorkspaceSlotGone  = errors.New("runner warm workspace slot not found")
)

func (r *RunnersRepo) RunnerWarmWorkspacePool(ctx context.Context, runnerID string, userID int64) (domain.RunnerWarmWorkspacePool, error) {
	var out domain.RunnerWarmWorkspacePool
	err := r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		machine, err := ownedRunnerWarmWorkspaceInventory(tx, runnerID, userID)
		if err != nil {
			return err
		}
		out.RunnerStatus = machine.Status
		out.LastSeenAt = machine.LastSeenAt
		out.SupportsWarmWorkspace = runnerFromRow(machine).SupportsWarmWorkspace()
		out.Queue = []domain.RunnerWarmWorkspaceWait{}

		slots := make(map[string]domain.RunnerWarmWorkspaceSlot, len(machine.WarmSlots))
		for _, slot := range machine.WarmSlots {
			slots[slot.ID] = domain.RunnerWarmWorkspaceSlot{
				ID: slot.ID, ProjectID: slot.ProjectID, RepositoryID: slot.RepositoryID, InventoryState: slot.State,
			}
		}

		demands, err := runnerWarmWorkspaceDemands(tx, runnerID)
		if err != nil {
			return err
		}
		for _, demand := range demands {
			if demand.State == domain.WarmWorkspaceQueued {
				out.Queue = append(out.Queue, domain.RunnerWarmWorkspaceWait{
					ProjectID: demand.ProjectID, RepositoryIDs: demand.RepositoryIDs, QueuedAt: demand.QueuedAt,
				})
				continue
			}
			for repositoryID, id := range demand.Slots {
				slot, reported := slots[id]
				if !reported {
					slot = domain.RunnerWarmWorkspaceSlot{ID: id, ProjectID: demand.ProjectID, RepositoryID: repositoryID}
				}
				slot.LeaseState = demand.State
				slots[id] = slot
			}
		}
		for _, id := range machine.WarmPausedSlots {
			slot := slots[id]
			slot.ID = id
			slot.Paused = true
			slots[id] = slot
		}
		out.Slots = make([]domain.RunnerWarmWorkspaceSlot, 0, len(slots))
		for _, id := range slices.Sorted(maps.Keys(slots)) {
			out.Slots = append(out.Slots, slots[id])
		}
		return nil
	})
	return out, err
}

func (r *RunnersRepo) SetRunnerWarmWorkspaceSlotPaused(ctx context.Context, runnerID string, userID int64, slotID string, paused bool) error {
	return r.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Use the allocator's machine lock so a reservation either commits
		// before the pause or observes the new scheduling preference.
		machine, err := ownedRunnerWarmWorkspaceInventory(tx.Clauses(clause.Locking{Strength: "UPDATE"}), runnerID, userID)
		if err != nil {
			return err
		}
		index := slices.Index(machine.WarmPausedSlots, slotID)
		if index < 0 && !slices.ContainsFunc(machine.WarmSlots, func(slot domain.WarmWorkspaceSlot) bool {
			return slot.ID == slotID
		}) {
			// A durable lease can outlive the inventory that advertised its slot.
			demands, err := runnerWarmWorkspaceDemands(tx, runnerID)
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(demands, func(demand warmWorkspaceRow) bool {
				for _, id := range demand.Slots {
					if id == slotID {
						return true
					}
				}
				return false
			}) {
				return ErrRunnerWarmWorkspaceSlotGone
			}
		}
		if paused == (index >= 0) {
			return nil
		}
		pausedSlots := machine.WarmPausedSlots
		if paused {
			pausedSlots = append(pausedSlots, slotID)
		} else {
			pausedSlots = slices.Delete(pausedSlots, index, index+1)
		}
		return tx.Model(&machine).Update("warm_paused_slots", string(jsonBytes(pausedSlots))).Error
	})
}

func runnerWarmWorkspaceDemands(tx *gorm.DB, runnerID string) ([]warmWorkspaceRow, error) {
	var demands []warmWorkspaceRow
	states := []domain.WarmWorkspaceState{
		domain.WarmWorkspaceQueued, domain.WarmWorkspaceReserved, domain.WarmWorkspaceReady,
		domain.WarmWorkspaceReleasing, domain.WarmWorkspaceBlocked,
	}
	err := tx.Select("project_id", "repository_ids", "queued_at", "state", "slots").
		Where("runner_id = ? AND state IN ?", runnerID, states).
		Order("queued_at, owner_run_id").Find(&demands).Error
	return demands, err
}

func ownedRunnerWarmWorkspaceInventory(tx *gorm.DB, runnerID string, userID int64) (runnerRow, error) {
	var machine runnerRow
	err := tx.Select("id", "status", "last_seen_at", "runner_version", "owner_user_id", "warm_slots", "warm_paused_slots").
		Take(&machine, "id = ?", runnerID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return machine, ErrRunnerNotFound
	}
	if err != nil {
		return machine, err
	}
	if userID <= 0 || machine.OwnerUserID == nil || *machine.OwnerUserID != userID {
		return machine, ErrRunnerWarmWorkspaceForbidden
	}
	return machine, nil
}
