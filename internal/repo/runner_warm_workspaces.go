package repo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type warmWorkspaceRow struct {
	OwnerRunID      string                    `gorm:"primaryKey;size:64"`
	RunnerID        string                    `gorm:"index:idx_warm_queue,priority:1;size:64;not null"`
	ProjectID       string                    `gorm:"size:64;not null"`
	RepositoryIDs   []string                  `gorm:"serializer:json;type:json;not null"`
	QueuedAt        time.Time                 `gorm:"index:idx_warm_queue,priority:3;not null"`
	State           domain.WarmWorkspaceState `gorm:"index:idx_warm_queue,priority:2;size:16;not null"`
	Epoch           uint64                    `gorm:"not null;default:1"`
	Generation      uint64
	LastActorID     int64
	ReadyGeneration uint64
	Slots           map[string]string `gorm:"serializer:json;type:json"`
}

// WarmSlotMatchesRepository binds a frozen checkout to the same origin the runner
// inspected. An ID-only lookup is retained for callers without a frozen URL.
func WarmSlotMatchesRepository(slot domain.WarmWorkspaceSlot, projectID string, repository domain.RepositoryContext) bool {
	return slot.ProjectID == projectID && slot.RepositoryID == repository.RepositoryID &&
		(repository.CloneURL == "" || slot.OriginSHA256 == gitwork.RemoteFingerprint(repository.CloneURL))
}

// CheckWarmWorkspaceRegistration accepts occupied slots. The allocator checks availability.
func CheckWarmWorkspaceRegistration(r domain.Runner, projectID string, repositories []domain.RepositoryContext) error {
	if !r.SupportsWarmWorkspace() {
		return fmt.Errorf("当前执行机器需升级至 %s，或在已登记仓库的机器新开对话", domain.RunnerVersionWarmWorkspace)
	}
	for _, repository := range repositories {
		if !slices.ContainsFunc(r.WarmWorkspaceSlots, func(slot domain.WarmWorkspaceSlot) bool {
			return slot.State != "blocked" && WarmSlotMatchesRepository(slot, projectID, repository)
		}) {
			return fmt.Errorf("请在当前执行机器登记仓库 %s 的预热工作区，或在已登记的机器新开对话", repository.RepositoryID)
		}
	}
	if len(repositories) == 0 {
		return fmt.Errorf("请指定已登记的项目仓库")
	}
	return nil
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
	return domain.WarmWorkspaceLease{OwnerRunID: row.OwnerRunID, RunnerID: row.RunnerID, Epoch: row.Epoch, Generation: row.Generation, Slots: row.Slots}
}

func matchesWarmLease(row warmWorkspaceRow, l domain.WarmWorkspaceLease) bool {
	return row.OwnerRunID == l.OwnerRunID && row.RunnerID == l.RunnerID && row.Epoch == l.Epoch && row.Generation != 0 && row.Generation == l.Generation && maps.Equal(row.Slots, l.Slots)
}

// A run-backed demand and its cancellation take the owner row first.
func warmOwnerCanQueue(tx *gorm.DB, id string) error {
	var owner runRow
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("status", "cancel_requested").Take(&owner, "id = ?", id).Error
	// Allocator-only owners have no run control row.
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if owner.CancelRequested || owner.Status == "cancelled" {
		return fmt.Errorf("warm workspace owner cancelled: %w", context.Canceled)
	}
	return nil
}

func (r *RunnersRepo) cancelWarmOwnerInTx(tx *gorm.DB, owner string) error {
	var row warmWorkspaceRow
	err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("owner_run_id", "epoch").Take(&row, "owner_run_id = ?", owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return cancelWarmEpoch(tx, owner, row.Epoch)
}

func (r *RunnersRepo) EnqueueWarmWorkspace(d domain.WarmWorkspaceDemand, now time.Time) error {
	ids := slices.Clone(d.RepositoryIDs)
	slices.Sort(ids)
	if strings.TrimSpace(d.OwnerRunID) == "" || strings.TrimSpace(d.RunnerID) == "" || strings.TrimSpace(d.ProjectID) == "" || len(ids) == 0 || ids[0] == "" || len(slices.Compact(slices.Clone(ids))) != len(ids) {
		return errors.New("invalid warm workspace demand")
	}
	row := warmWorkspaceRow{OwnerRunID: d.OwnerRunID, RunnerID: d.RunnerID, ProjectID: d.ProjectID, RepositoryIDs: ids, QueuedAt: now, State: domain.WarmWorkspaceQueued}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if err := warmOwnerCanQueue(tx, d.OwnerRunID); err != nil {
			return err
		}
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

// RequeueWarmWorkspace starts another attempt for the same frozen demand. The
// caller must have observed this particular terminal epoch. Ordinary enqueue
// retries do not resurrect completed or canceled work.
func (r *RunnersRepo) RequeueWarmWorkspace(previous domain.WarmWorkspaceLease, now time.Time) error {
	return r.requeueWarmWorkspace(previous, now, "")
}
func (r *RunnersRepo) ReopenReturnedWarmWorkspace(previous domain.WarmWorkspaceLease, now time.Time, token string) error {
	if token == "" {
		return ErrWorkspaceInUse
	}
	return r.requeueWarmWorkspace(previous, now, token)
}
func (r *RunnersRepo) requeueWarmWorkspace(previous domain.WarmWorkspaceLease, now time.Time, restoreToken string) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		if restoreToken != "" {
			if err := holdsWorkspaceLease(tx, previous.OwnerRunID, restoreToken); err != nil {
				return err
			}
		} else if err := warmOwnerCanQueue(tx, previous.OwnerRunID); err != nil {
			return err
		}
		var row warmWorkspaceRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "owner_run_id = ?", previous.OwnerRunID).Error; err != nil {
			return err
		}
		terminal := row.State == domain.WarmWorkspaceReleased || row.State == domain.WarmWorkspaceCanceled
		if !terminal || restoreToken != "" && row.State != domain.WarmWorkspaceReleased || row.Epoch != previous.Epoch || row.RunnerID != previous.RunnerID || row.Generation != previous.Generation || !maps.Equal(row.Slots, previous.Slots) {
			return errors.New("stale warm workspace requeue")
		}
		if row.Epoch+1 == 0 {
			return errors.New("warm workspace queue epoch exhausted")
		}
		return tx.Model(&row).Updates(map[string]any{"epoch": row.Epoch + 1, "state": domain.WarmWorkspaceQueued, "slots": nil, "generation": 0, "queued_at": now}).Error
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
		// Any poller can reserve the oldest eligible owner. Read each queued
		// owner's frozen repository projection, including pre-upgrade demands.
		var rows []struct {
			Demand                   warmWorkspaceRow `gorm:"embedded"`
			Repository, Repositories []byte
		}
		err := tx.Table("runner_warm_workspaces AS demand").Select("demand.*, frozen_run.repository, frozen_run.repositories").
			Joins("LEFT JOIN runs AS frozen_run ON frozen_run.id = demand.owner_run_id AND demand.state = ?", domain.WarmWorkspaceQueued).
			Where("demand.runner_id = ? AND demand.state NOT IN ?", runnerID, []domain.WarmWorkspaceState{domain.WarmWorkspaceReleased, domain.WarmWorkspaceCanceled}).
			Order("demand.queued_at,demand.owner_run_id").Find(&rows).Error
		if err != nil {
			return err
		}
		used := map[string]bool{}
		for _, id := range machine.WarmPausedSlots {
			used[id] = true
		}
		for _, row := range rows {
			for _, id := range row.Demand.Slots {
				used[id] = true
			}
		}
		for _, entry := range rows {
			row := entry.Demand
			if row.State != domain.WarmWorkspaceQueued {
				continue
			}
			frozen := runFromRow(runRow{Repository: entry.Repository, Repositories: entry.Repositories}).RepositoryContexts()
			required := make(map[string]domain.RepositoryContext, len(row.RepositoryIDs))
			for _, repository := range frozen {
				if slices.Contains(row.RepositoryIDs, repository.RepositoryID) {
					required[repository.RepositoryID] = repository
				}
			}
			if len(frozen) == 0 {
				for _, id := range row.RepositoryIDs {
					required[id] = domain.RepositoryContext{RepositoryID: id}
				}
			}
			slots := map[string]string{}
			for _, slot := range machine.WarmSlots {
				repository, needed := required[slot.RepositoryID]
				if needed && slot.State == "available" && !used[slot.ID] && slots[slot.RepositoryID] == "" && WarmSlotMatchesRepository(slot, row.ProjectID, repository) {
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
			// Registration can issue a new Runner ID for the same disk. Honor
			// every slot's durable generation, including occupied slots.
			for _, slot := range machine.WarmSlots {
				machine.WarmGeneration = max(machine.WarmGeneration, slot.Generation)
			}
			generation := machine.WarmGeneration + 1
			if generation == 0 {
				return errors.New("warm workspace generation exhausted")
			}
			res := tx.Model(&row).Where("state = ? AND epoch = ?", domain.WarmWorkspaceQueued, row.Epoch).Updates(map[string]any{"state": domain.WarmWorkspaceReserved, "generation": generation, "slots": string(jsonBytes(slots))})
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

// CancelWarmWorkspace cancels the initial demand. Callers resuming an owner
// use CancelWarmWorkspaceEpoch with the epoch their request observed.
func (r *RunnersRepo) CancelWarmWorkspace(ownerRunID string, now time.Time) error {
	return r.CancelWarmWorkspaceEpoch(ownerRunID, 1, now)
}

func (r *RunnersRepo) CancelWarmWorkspaceEpoch(ownerRunID string, epoch uint64, now time.Time) error {
	return cancelWarmEpoch(r.DB, ownerRunID, epoch)
}
func cancelWarmEpoch(tx *gorm.DB, ownerRunID string, epoch uint64) error {
	// A reservation may already be on disk, even if its acknowledgement was lost.
	return tx.Model(&warmWorkspaceRow{}).Where("owner_run_id = ? AND epoch = ? AND state IN ?", ownerRunID, epoch, []domain.WarmWorkspaceState{domain.WarmWorkspaceQueued, domain.WarmWorkspaceReserved, domain.WarmWorkspaceReady, domain.WarmWorkspaceBlocked}).Update("state", gorm.Expr("CASE WHEN state = ? THEN ? ELSE ? END", domain.WarmWorkspaceQueued, domain.WarmWorkspaceCanceled, domain.WarmWorkspaceReleasing)).Error
}

func (r *RunnersRepo) SettleWarmWorkspace(l domain.WarmWorkspaceLease, state domain.WarmWorkspaceState) error {
	return r.DB.Transaction(func(tx *gorm.DB) error { return r.settleWarmWorkspace(tx, l, state) })
}
func (r *RunnersRepo) settleWarmWorkspace(tx *gorm.DB, l domain.WarmWorkspaceLease, state domain.WarmWorkspaceState) error {
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
	changes := map[string]any{"state": state}
	if state == domain.WarmWorkspaceReady || row.State == domain.WarmWorkspaceReady {
		changes["ready_generation"] = l.Generation
	}
	return tx.Model(&row).Updates(changes).Error
}

func (r *RunnersRepo) AuthorizeWarmWorkspaceControl(runnerID string, run domain.Run, req *domain.RunnerWorkspaceRequest) error {
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
	if run.ID != row.OwnerRunID || run.ExecutionContext.ProjectID != row.ProjectID {
		return errors.New("warm workspace demand differs from frozen owner")
	}
	frozen := run.RepositoryContexts()
	ids := make([]string, 0, len(frozen))
	for _, repo := range frozen {
		ids = append(ids, repo.RepositoryID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, row.RepositoryIDs) {
		return errors.New("warm workspace demand differs from frozen repositories")
	}
	if runnerID != row.RunnerID || !matchesWarmLease(row, *req.WarmWorkspaceLease) {
		return errors.New("stale warm workspace control request")
	}
	var allowed bool
	switch req.Operation {
	case domain.RunnerWorkspaceOperationWarmCheckout, domain.RunnerWorkspaceOperationWarmPrepare:
		allowed = row.State == domain.WarmWorkspaceReserved
	case domain.RunnerWorkspaceOperationWarmVerifyAccess:
		allowed = true // read-only probe of the frozen remote, independent of placement
	case domain.RunnerWorkspaceOperationWarmStatus:
		allowed = row.Generation != 0
	case domain.RunnerWorkspaceOperationWarmReleaseCheck:
		allowed = row.State == domain.WarmWorkspaceReleasing
	case domain.RunnerWorkspaceOperationWarmReleaseCommit:
		if err := canonicalWarmReturnReceipt(r.DB, row.lease(), req); err != nil {
			return err
		}
		allowed = row.State == domain.WarmWorkspaceReleasing
	}
	if !allowed {
		return fmt.Errorf("workspace operation %q unavailable in %s", req.Operation, row.State)
	}
	canonical := row.lease()
	req.WarmWorkspaceLease = &canonical
	return nil
}

// Existing placements stay on their original machine. Recheck compatibility
// when joining the queue and while waiting for the first placement.
var errWarmTargetIncompatible = errors.New("执行机器不兼容")

// A competing reservation or requeue wins over a stale poller's compatibility observation.
func (r *RunnersRepo) CheckQueuedWarmTarget(run domain.Run, l domain.WarmWorkspaceLease) error {
	err := r.CheckWarmTarget(run, domain.WarmWorkspaceQueued)
	if !errors.Is(err, errWarmTargetIncompatible) {
		return err
	}
	res := r.DB.Model(&warmWorkspaceRow{}).Where("owner_run_id = ? AND epoch = ? AND state = ?", run.ID, l.Epoch, domain.WarmWorkspaceQueued).Update("state", domain.WarmWorkspaceCanceled)
	if res.Error != nil || res.RowsAffected == 0 {
		return res.Error
	}
	return err
}

func (r *RunnersRepo) CheckWarmTarget(run domain.Run, state domain.WarmWorkspaceState) error {
	switch state {
	case "", domain.WarmWorkspaceQueued, domain.WarmWorkspaceReleased, domain.WarmWorkspaceCanceled:
	default:
		return nil
	}
	var machine runnerRow
	err := r.DB.Select("id", "status", "runner_version", "warm_slots", "labels", "owner_user_id").Take(&machine, "id = ?", run.ExecutionContext.ExecutionRunnerID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("%w：执行机器已移除，请在已登记仓库的机器新开对话", errWarmTargetIncompatible)
	}
	if err != nil {
		return err
	}
	if machine.Status != domain.RunnerStatusOnline && state == domain.WarmWorkspaceQueued {
		return nil // preserve an existing wait across a temporary disconnect
	}
	target := runnerFromRow(machine)
	if snapshot := run.WorkflowSnapshot.SandboxSnapshot; snapshot != nil {
		driver := snapshot.Profile.Driver
		if !target.SupportsWarmWorkspaceDriver(driver) || (driver == domain.SandboxDriverNative && (target.OwnerUserID <= 0 || target.OwnerUserID != run.ExecutionContext.ResponsibleUserID)) {
			return fmt.Errorf("%w：当前执行机器不支持该预热沙箱或未绑定本人", errWarmTargetIncompatible)
		}
	}
	if err := CheckWarmWorkspaceRegistration(target, run.ExecutionContext.ProjectID, run.RepositoryContexts()); err != nil {
		return errors.Join(errWarmTargetIncompatible, err)
	}
	return nil
}

// joinConversationDemand executes inside the transaction that parks a turn.
// A turn joining an IDE restore shares capacity but cannot cancel its demand.
func (r *RunnersRepo) joinConversationDemand(tx *gorm.DB, run domain.Run) (uint64, bool, error) {
	repositories := run.RepositoryContexts()
	if len(repositories) != 1 {
		return 0, false, errors.New("预热对话需要一个已登记的仓库")
	}
	d := domain.WarmWorkspaceDemand{OwnerRunID: run.ID, RunnerID: run.ExecutionContext.ExecutionRunnerID, ProjectID: run.ExecutionContext.ProjectID, RepositoryIDs: []string{repositories[0].RepositoryID}}
	var owner runRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("id").Take(&owner, "id = ?", d.OwnerRunID).Error; err != nil {
		return 0, false, err
	}
	scoped := *r
	base := *r.deps
	base.DB = tx
	scoped.deps = &base
	l, state, found, err := scoped.WarmWorkspaceAllocation(d.OwnerRunID)
	if err != nil {
		return 0, false, err
	}
	if err = scoped.CheckWarmTarget(run, state); err != nil {
		return 0, false, err
	}
	if err = scoped.EnqueueWarmWorkspace(d, time.Now()); err != nil {
		return 0, false, err
	}
	if !found {
		l, _, _, err = scoped.WarmWorkspaceAllocation(d.OwnerRunID)
		return l.Epoch, true, err
	}
	if state == domain.WarmWorkspaceReleased || state == domain.WarmWorkspaceCanceled {
		if err = scoped.RequeueWarmWorkspace(l, time.Now()); err != nil {
			return 0, false, err
		}
		return l.Epoch + 1, true, nil
	}
	return l.Epoch, false, nil
}

func (r *RunnersRepo) lockReadyWarmEpoch(tx *gorm.DB, id string, epoch uint64) error {
	var row warmWorkspaceRow
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Select("owner_run_id").Take(&row, "owner_run_id = ? AND epoch = ? AND state = ?", id, epoch, domain.WarmWorkspaceReady).Error
}
