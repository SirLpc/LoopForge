package repo

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type nativeWarmReturnReceipt struct {
	Epoch      uint64                    `json:"epoch"`
	Generation uint64                    `json:"generation"`
	ID         string                    `json:"id"`
	Refs       []domain.WarmWorkspaceRef `json:"refs,omitempty"`
}

func warmReturnReceipt(db *gorm.DB, l domain.WarmWorkspaceLease) (warmWorkspaceRefRow, nativeWarmReturnReceipt, bool, error) {
	var proof warmWorkspaceRefRow
	err := db.Take(&proof, "run_id = ?", l.OwnerRunID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = nil
		proof.RunID = l.OwnerRunID
		proof.Refs = []domain.WarmWorkspaceRef{}
	}
	if err != nil {
		return proof, nativeWarmReturnReceipt{}, false, err
	}
	if native := proof.PendingNativeReturn; native != nil && native.Epoch == l.Epoch && native.Generation == l.Generation {
		return proof, *native, true, nil
	}
	if l.Generation != 0 && proof.Epoch == l.Epoch && proof.Generation == l.Generation {
		receipt := nativeWarmReturnReceipt{Epoch: l.Epoch, Generation: l.Generation}
		if !proof.Unplaced {
			receipt.Refs = proof.Refs
		}
		return proof, receipt, true, nil
	}
	return proof, nativeWarmReturnReceipt{}, false, nil
}

func (r *RunsRepo) WarmWorkspaceReturnReceipt(l domain.WarmWorkspaceLease) (id string, found bool, err error) {
	_, receipt, found, err := warmReturnReceipt(r.DB, l)
	return receipt.ID, found, err
}

// Lock in the same order as workspace checkpoints and warm binding confirmation.
func (r *RunsRepo) nativeWarmReturnTransaction(l domain.WarmWorkspaceLease, token string, now time.Time, update func(*gorm.DB, warmWorkspaceRow, warmWorkspaceRefRow, nativeWarmReturnReceipt, bool) error) error {
	if token == "" {
		return ErrWorkspaceInUse
	}
	return r.DB.Transaction(func(tx *gorm.DB) error {
		run, err := r.takeRunForUpdate(tx, l.OwnerRunID)
		if err != nil {
			return err
		}
		snapshot := runFromRow(run).WorkflowSnapshot.SandboxSnapshot
		if snapshot == nil || snapshot.Profile.Driver != domain.SandboxDriverNative {
			return errors.New("return receipt requires a native warm workspace")
		}
		q := tx.Where("run_id = ? AND lease_token = ? AND lease_until IS NOT NULL", l.OwnerRunID, token)
		if r.isMySQL() {
			q = q.Where(`(lease_renewed_at > UTC_TIMESTAMP(3) - INTERVAL ? MICROSECOND OR (lease_renewed_at IS NULL AND lease_until > ?))`, workspaceUseTerm.Microseconds(), now)
		} else {
			q = q.Where("lease_until > ?", now)
		}
		var hold WorkspaceCheckpoint
		if err := q.Select("run_id").Take(&hold).Error; errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrWorkspaceInUse
		} else if err != nil {
			return err
		}
		var row warmWorkspaceRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "owner_run_id = ?", l.OwnerRunID).Error; err != nil {
			return err
		}
		if !matchesWarmLease(row, l) || row.State != domain.WarmWorkspaceReleasing {
			return errors.New("stale native warm return lease")
		}
		proof, receipt, found, err := warmReturnReceipt(tx, l)
		if err != nil {
			return err
		}
		return update(tx, row, proof, receipt, found)
	})
}

func (r *RunsRepo) RecordNativeWarmWorkspaceReturn(l domain.WarmWorkspaceLease, token, id string, refs []domain.WarmWorkspaceRef, now time.Time) error {
	if id == "" || strings.TrimSpace(id) != id || len(id) > 64 {
		return errors.New("invalid native warm return receipt ID")
	}
	if err := validateWarmWorkspaceRefs(l, refs); err != nil {
		return err
	}
	if len(refs) == 0 {
		refs = nil
	}
	return r.nativeWarmReturnTransaction(l, token, now, func(tx *gorm.DB, row warmWorkspaceRow, proof warmWorkspaceRefRow, current nativeWarmReturnReceipt, found bool) error {
		if len(refs) == 0 && row.ReadyGeneration == l.Generation {
			return errors.New("missing return references for prepared workspace")
		}
		if found {
			if current.ID != id || !reflect.DeepEqual(current.Refs, refs) {
				return errors.New("different native warm return receipt already recorded")
			}
			return nil
		}
		proof.PendingNativeReturn = &nativeWarmReturnReceipt{Epoch: l.Epoch, Generation: l.Generation, ID: id, Refs: refs}
		return tx.Save(&proof).Error
	})
}

func (r *RunsRepo) InvalidateNativeWarmWorkspaceReturn(l domain.WarmWorkspaceLease, token, id string, now time.Time) error {
	return r.nativeWarmReturnTransaction(l, token, now, func(tx *gorm.DB, row warmWorkspaceRow, proof warmWorkspaceRefRow, current nativeWarmReturnReceipt, found bool) error {
		if !found || current.ID != id {
			return errors.New("stale native warm return receipt")
		}
		if id != "" {
			proof.PendingNativeReturn = nil
		} else {
			// Legacy placed receipts overwrote the previous recovery refs
			// before confirmation. Those unconfirmed refs must not be reused.
			if !proof.Unplaced {
				proof.Refs = []domain.WarmWorkspaceRef{}
			}
			proof.Generation, proof.Epoch = 0, 0
		}
		if err := tx.Save(&proof).Error; err != nil {
			return err
		}
		if row.ReadyGeneration == l.Generation {
			return tx.Model(&row).Update("state", domain.WarmWorkspaceReady).Error
		}
		return nil
	})
}

func (r *RunsRepo) ConfirmNativeWarmWorkspaceReturn(l domain.WarmWorkspaceLease, token, id string, now time.Time) error {
	if id == "" {
		return errors.New("missing native warm return receipt ID")
	}
	return r.nativeWarmReturnTransaction(l, token, now, func(tx *gorm.DB, row warmWorkspaceRow, proof warmWorkspaceRefRow, current nativeWarmReturnReceipt, found bool) error {
		if !found || current.ID != id {
			return errors.New("stale native warm return receipt")
		}
		proof.Epoch, proof.Generation = l.Epoch, l.Generation
		proof.Unplaced = row.ReadyGeneration != l.Generation
		if !proof.Unplaced {
			proof.Refs = current.Refs
		}
		proof.PendingNativeReturn = nil
		if err := tx.Save(&proof).Error; err != nil {
			return err
		}
		if err := r.runners.settleWarmWorkspace(tx, l, domain.WarmWorkspaceReleased); err != nil {
			return err
		}
		return tx.Model(&runRow{}).Where("id = ?", l.OwnerRunID).Update("workspace_cleaned_at", now.UTC()).Error
	})
}

func canonicalWarmReturnReceipt(db *gorm.DB, l domain.WarmWorkspaceLease, req *domain.RunnerWorkspaceRequest) error {
	_, receipt, found, err := warmReturnReceipt(db, l)
	if err != nil {
		return fmt.Errorf("read durable warm return proof: %w", err)
	}
	if !found {
		return errors.New("no matching durable return proof")
	}
	req.WarmWorkspaceReturnID = receipt.ID
	req.WarmWorkspaceRefs = receipt.Refs
	return nil
}
