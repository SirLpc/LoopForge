package repo

import (
	"agentflow/internal/domain"
	"bytes"
	"encoding/json"
	"errors"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"slices"
	"sort"
)

var ErrConversationWorkspaceChanged = errors.New("conversation workspace binding changed; retry deletion")

func (r *AssistantRepo) AssistantSessionForDeletion(userID int64, id string) (domain.AssistantSession, error) {
	var row assistantSessionRow
	err := r.DB.Take(&row, "user_id = ? AND id = ?", userID, assistantRecordID(id)).Error
	if err != nil {
		return domain.AssistantSession{}, err
	}
	return checkedSessionForDeletion(row)
}
func checkedSessionForDeletion(row assistantSessionRow) (domain.AssistantSession, error) {
	var session domain.AssistantSession
	if raw := bytes.TrimSpace(row.Payload); len(raw) == 0 || raw[0] != '{' {
		return session, ErrConversationWorkspaceChanged
	}
	if err := json.Unmarshal(row.Payload, &session); err != nil {
		return session, err
	}
	if session.ID != "" && session.ID != row.ID || session.UserID != 0 && session.UserID != row.UserID {
		return session, ErrConversationWorkspaceChanged
	}
	return decodeAssistantSession(row), nil
}
func (r *AssistantRepo) DeleteAssistantSessionAfterReturn(expected domain.AssistantSession, leases map[string]*domain.WarmWorkspaceLease) error {
	return r.deleteAssistantSession(expected.UserID, expected.ID, &expected, leases)
}
func (r *AssistantRepo) lockSessionReturn(tx *gorm.DB, expected domain.AssistantSession, leases map[string]*domain.WarmWorkspaceLease) error {
	var row assistantSessionRow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "user_id = ? AND id = ?", expected.UserID, expected.ID).Error; err != nil {
		return err
	}
	current, err := checkedSessionForDeletion(row)
	if err != nil {
		return err
	}
	if current.WorkspaceRunID != expected.WorkspaceRunID || current.RunID != expected.RunID || current.IssueID != expected.IssueID || current.ProjectID != expected.ProjectID || !slices.Equal(current.OwnWorkspaceRunIDs, expected.OwnWorkspaceRunIDs) {
		return ErrConversationWorkspaceChanged
	}
	ids := slices.Clone(current.OwnWorkspaceRunIDs)
	if current.WorkspaceRunID != "" && !slices.Contains(ids, current.WorkspaceRunID) {
		ids = append(ids, current.WorkspaceRunID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := r.runners.lockReturnedWorkspace(tx, id, leases[id]); err != nil {
			return err
		}
	}
	return nil
}

// AssistantWarmWait belongs to one generation and survives losing the process
// that requested the checkout. Request carries the original chat parameters.
type AssistantWarmWait struct {
	NativeSessionID string
	RunID           string
	Previous        string
	Epoch           uint64
	CancelDemand    bool
	Request         json.RawMessage
}

type WaitingAssistantGeneration struct {
	Lease      AssistantGenerationLease
	Generation domain.AssistantGeneration
	Wait       AssistantWarmWait
}

func cancelPendingWarmWorkspace(tx *gorm.DB, payload assistantGenerationPayload) error {
	if w := payload.WarmWait; w != nil && w.CancelDemand {
		return cancelWarmEpoch(tx, w.RunID, w.Epoch)
	}
	return nil
}

func (r *AssistantRepo) ParkWarmGeneration(g domain.AssistantGeneration, run domain.Run, wait AssistantWarmWait, diagnostics ...domain.AssistantDiagnostics) error {
	return r.DB.Transaction(func(tx *gorm.DB) error {
		var sess assistantSessionRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&sess, "user_id = ? AND id = ?", g.UserID, g.SessionID).Error; err != nil {
			return err
		}
		item := decodeAssistantSession(sess)
		if err := r.assistantSessionWriteError(tx, item); err != nil {
			return err
		}
		if item.ProjectID != run.ExecutionContext.ProjectID || item.RunID != "" || item.Execution().RunnerID != run.ExecutionContext.ExecutionRunnerID || item.WorkspaceRunID != wait.Previous || wait.RunID != run.ID || !run.IsWorkspace() || run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
			return ErrConversationWorkspaceChanged
		}
		row, err := lockRunningAssistantGeneration(tx, g.UserID, g.SessionID, g.ID)
		if err != nil {
			return err
		}
		if row.CancelRequested || row.WarmWaitRunID != "" {
			return errors.New("预热工作区请求已变更")
		}
		wait.Epoch, wait.CancelDemand, err = r.runners.joinConversationDemand(tx, run)
		if err != nil {
			return err
		}
		if !slices.Contains(item.OwnWorkspaceRunIDs, run.ID) && item.WorkspaceRunID != run.ID {
			return ErrConversationWorkspaceChanged
		}
		payload := generationPayload(row)
		payload.AssistantGeneration = g
		payload.WarmWait = &wait
		if err := tx.Model(&row).Updates(map[string]any{"warm_wait_run_id": run.ID, "payload": mustJSON(payload), "updated_at": r.generationClock()}).Error; err != nil {
			return err
		}
		return checkpointAssistantDiagnostics(tx, g, diagnostics)
	})
}

func (r *AssistantRepo) WaitingWarmGenerations() ([]WaitingAssistantGeneration, error) {
	var rows []assistantGenerationRow
	if err := r.DB.Where("status = ? AND warm_wait_run_id <> ''", "running").Find(&rows).Error; err != nil {
		return nil, err
	}
	var out []WaitingAssistantGeneration
	for _, row := range rows {
		if w := generationPayload(row).WarmWait; w != nil {
			out = append(out, WaitingAssistantGeneration{Generation: decodeGeneration(row), Wait: *w, Lease: AssistantGenerationLease{RunID: row.WorkspaceRunID, Token: row.WorkspaceLease}})
		}
	}
	return out, nil
}

// Exactly one controller can consume the parked request. Stop and the binding
// change serialize on the generation row, including the ready-but-unbound gap.
func (r *AssistantRepo) ClaimWarmContinuation(p WaitingAssistantGeneration, bind bool) (domain.AssistantGeneration, bool, error) {
	var g domain.AssistantGeneration
	claimed := false
	err := r.DB.Transaction(func(tx *gorm.DB) error {
		var sess assistantSessionRow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&sess, "user_id = ? AND id = ?", p.Generation.UserID, p.Generation.SessionID).Error; err != nil {
			return err
		}
		row, err := lockRunningAssistantGeneration(tx, p.Generation.UserID, p.Generation.SessionID, p.Generation.ID)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		payload := generationPayload(row)
		w := payload.WarmWait
		if w == nil || w.RunID != p.Wait.RunID || w.Epoch != p.Wait.Epoch {
			return nil
		}
		if bind && !row.CancelRequested {
			item := decodeAssistantSession(sess)
			if item.WorkspaceRunID != w.Previous || item.ProjectID != p.Generation.ProjectID {
				return ErrConversationWorkspaceChanged
			}
			if err := r.runners.lockReadyWarmEpoch(tx, w.RunID, w.Epoch); err != nil {
				return err
			}
			item.WorkspaceRunID = w.RunID
			item.RuntimeMode = domain.AssistantModeWorkspace
			if err = tx.Model(&sess).UpdateColumn("payload", mustJSON(item)).Error; err != nil {
				return err
			}
			payload.RuntimeMode = item.RuntimeMode
		} else if err = cancelPendingWarmWorkspace(tx, payload); err != nil {
			return err
		}
		payload.WarmWait = nil
		row.WarmWaitRunID = ""
		row.Payload = mustJSON(payload)
		g = decodeGeneration(row)
		if err := tx.Model(&row).Updates(map[string]any{"warm_wait_run_id": "", "payload": row.Payload, "updated_at": r.generationClock()}).Error; err != nil {
			return err
		}
		claimed = true
		return checkpointAssistantDiagnostics(tx, g, nil)
	})
	return g, claimed && err == nil, err
}

// Keep the idle owner reachable even if the previous directory is dirty or
// the turn stops before it can enqueue. Recording ownership never binds it.
func (r *AssistantRepo) RememberWarmConversationWorkspace(userID int64, sessionID, projectID, runID string) error {
	return r.updateAssistantSessionWorkspace(userID, sessionID, projectID, false, func(s *domain.AssistantSession) {
		if !slices.Contains(s.OwnWorkspaceRunIDs, runID) {
			s.OwnWorkspaceRunIDs = append(s.OwnWorkspaceRunIDs, runID)
		}
	})
}
