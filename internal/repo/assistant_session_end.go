package repo

import (
	"bytes"
	"errors"
	"slices"
	"time"

	"agentflow/internal/domain"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrAssistantSessionEnding         = errors.New("会话正在结束，请稍候")
	ErrAssistantSessionEnded          = errors.New("会话已结束，请新建会话继续")
	ErrAssistantSessionEndUnsupported = errors.New("此类会话由所属任务管理")
)

const (
	assistantSessionEndStopBudget   = 2 * time.Minute
	assistantSessionEndReturnBudget = 31 * time.Minute
)

func (r *AssistantRepo) assistantSessionWriteError(tx *gorm.DB, session domain.AssistantSession) error {
	if session.End == nil {
		return nil
	}
	switch session.End.State {
	case domain.AssistantSessionEnded:
		return ErrAssistantSessionEnded
	case domain.AssistantSessionEnding:
		now, ok := r.heartbeatNow(tx)
		if !ok || session.End.Deadline == nil || now.Before(*session.End.Deadline) {
			return ErrAssistantSessionEnding
		}
	}
	return nil
}

// assistantSessionTitleUpdate preserves the original data even when a client
// sends a complete session object to rename a read-only conversation.
func assistantSessionTitleUpdate(old, next domain.AssistantSession, writeErr error) (domain.AssistantSession, error) {
	if !bytes.Equal(mustJSON(old.Messages), mustJSON(next.Messages)) ||
		old.ProjectID != next.ProjectID || old.RunID != next.RunID ||
		old.WorkspaceRunID != next.WorkspaceRunID || old.IssueID != next.IssueID ||
		old.Model != next.Model || old.Effort != next.Effort ||
		old.Execution() != next.Execution() {
		return old, writeErr
	}
	old.Title = next.Title
	old.TitleEdited = true
	old.UpdatedAt = time.Time{}
	return old, nil
}

// BeginAssistantSessionEnd fences new turns and durably cancels the current
// generation, including a queued warm allocation, in one transaction.
func (r *AssistantRepo) BeginAssistantSessionEnd(userID int64, id, attemptID string) (domain.AssistantSession, string, error) {
	var session domain.AssistantSession
	var cancelled string
	err := r.retryAssistantDeadlock(func() error {
		return r.DB.Transaction(func(tx *gorm.DB) error {
			cancelled = ""
			var row assistantSessionRow
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "id = ? AND user_id = ?", assistantRecordID(id), userID).Error; err != nil {
				return err
			}
			var err error
			session, err = checkedSessionForDeletion(row)
			if err != nil {
				return err
			}
			if session.Shared || session.RunID != "" || inferredAssistantSessionSource(session) != domain.AssistantSourceChat {
				return ErrAssistantSessionEndUnsupported
			}
			if err := r.assistantSessionWriteError(tx, session); err != nil {
				return nil
			}
			now, ok := r.heartbeatNow(tx)
			if !ok {
				return errors.New("无法读取会话操作时间")
			}
			count := len(session.OwnWorkspaceRunIDs)
			if session.WorkspaceRunID != "" && !slices.Contains(session.OwnWorkspaceRunIDs, session.WorkspaceRunID) {
				count++
			}
			deadline := now.Add(assistantSessionEndStopBudget + time.Duration(count)*assistantSessionEndReturnBudget)
			session.End = &domain.AssistantSessionEnd{State: domain.AssistantSessionEnding, AttemptID: attemptID, Deadline: &deadline}
			var generation assistantGenerationRow
			err = lockAssistantSessionGeneration(tx, session, &generation)
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if generation.Status == "running" {
				payload := generationPayload(generation)
				if err := cancelPendingWarmWorkspace(tx, payload); err != nil {
					return err
				}
				payload.NativeApprovals = nil
				payload.SteeringClosed = true
				if err := tx.Model(&generation).UpdateColumns(map[string]any{"cancel_requested": true, "payload": mustJSON(payload)}).Error; err != nil {
					return err
				}
				cancelled = generation.GenerationID
			}
			return tx.Model(&row).UpdateColumn("payload", mustJSON(session)).Error
		})
	})
	return session, cancelled, err
}

func lockAssistantSessionGeneration(tx *gorm.DB, session domain.AssistantSession, generation *assistantGenerationRow) error {
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(generation, "session_id = ? AND user_id = ?", session.ID, session.UserID).Error
}

var ErrAssistantSessionEndChanged = errors.New("会话结束操作已更新，请刷新后重试")

func (r *AssistantRepo) AssistantSessionEndReady(userID int64, id, attemptID string) (domain.AssistantSession, bool, error) {
	session, err := r.AssistantSessionForDeletion(userID, id)
	if err != nil {
		return session, false, err
	}
	if !r.currentAssistantSessionEnd(r.DB, session, attemptID) {
		return session, false, ErrAssistantSessionEndChanged
	}
	var active int64
	err = r.DB.Model(&assistantGenerationRow{}).Where("session_id = ? AND user_id = ? AND status = ?", session.ID, session.UserID, "running").Count(&active).Error
	return session, active == 0 && err == nil, err
}

func (r *AssistantRepo) currentAssistantSessionEnd(tx *gorm.DB, session domain.AssistantSession, attemptID string) bool {
	return attemptID != "" && session.End != nil && session.End.AttemptID == attemptID &&
		errors.Is(r.assistantSessionWriteError(tx, session), ErrAssistantSessionEnding)
}

func (r *AssistantRepo) FinishAssistantSessionEnd(expected domain.AssistantSession, leases map[string]*domain.WarmWorkspaceLease, endError string) error {
	if expected.End == nil {
		return ErrAssistantSessionEndChanged
	}
	return r.retryAssistantDeadlock(func() error {
		return r.DB.Transaction(func(tx *gorm.DB) error {
			var row assistantSessionRow
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Take(&row, "id = ? AND user_id = ?", expected.ID, expected.UserID).Error; err != nil {
				return err
			}
			current, err := checkedSessionForDeletion(row)
			if err != nil {
				return err
			}
			if !r.currentAssistantSessionEnd(tx, current, expected.End.AttemptID) {
				return ErrAssistantSessionEndChanged
			}
			if endError != "" {
				current.End = &domain.AssistantSessionEnd{State: domain.AssistantSessionEndFailed, Error: endError}
				return tx.Model(&row).UpdateColumn("payload", mustJSON(current)).Error
			}
			now, ok := r.heartbeatNow(tx)
			if !ok {
				return errors.New("无法读取会话操作时间")
			}
			var generation assistantGenerationRow
			err = lockAssistantSessionGeneration(tx, current, &generation)
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			if generation.Status == "running" {
				return ErrAssistantSessionEnding
			}
			if err := r.lockReturnedAssistantWorkspaces(tx, current, expected, leases); err != nil {
				return err
			}
			// Older sessions only had the active binding. Retain the history
			// of a workspace the conversation opened before clearing it.
			if current.WorkspaceRunID != "" && !slices.Contains(current.OwnWorkspaceRunIDs, current.WorkspaceRunID) {
				var workspace runRow
				err := tx.Select("id", "kind", "workflow_id", "execution_context").Take(&workspace, "id = ?", current.WorkspaceRunID).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if err == nil && runFromRow(workspace).IsWorkspace() {
					current.OwnWorkspaceRunIDs = append(current.OwnWorkspaceRunIDs, current.WorkspaceRunID)
				}
			}
			current.WorkspaceRunID = ""
			current.RuntimeMode = domain.AssistantModePlatform
			current.End = &domain.AssistantSessionEnd{State: domain.AssistantSessionEnded, EndedAt: &now}
			return tx.Model(&row).UpdateColumn("payload", mustJSON(current)).Error
		})
	})
}
