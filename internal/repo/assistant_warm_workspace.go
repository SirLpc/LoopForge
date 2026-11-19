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
