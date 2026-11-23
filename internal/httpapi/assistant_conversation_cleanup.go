package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"gorm.io/gorm"
	"os"
)

var errWarmConversationRetained = errors.New("对话工作区仍需保留")

// deleteConversation deletes a conversation together with what it left
// behind: the workspaces it opened and the requirements it created but never
// handed in. Manual deletion, scheduled cleanup and replaced scheduled runs
// all go through here so they settle a conversation the same way.
func (h *assistantHandlers) deleteConversation(userID int64, id string) error {
	conv, err := h.repo.AssistantSessionForDeletion(userID, id)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return os.ErrNotExist
	}
	if err != nil {
		return fmt.Errorf("%w: %w", errWarmConversationRetained, err)
	}
	if conv.End != nil && conv.End.State == domain.AssistantSessionEnding &&
		(conv.End.Deadline == nil || time.Now().Before(*conv.End.Deadline)) {
		return repo.ErrAssistantSessionEnding
	}
	leases, err := h.returnConversationWarmWorkspaces(context.Background(), conv, time.Time{})
	if err != nil {
		return fmt.Errorf("%w: %w", errWarmConversationRetained, err)
	}
	if err := h.repo.DeleteAssistantSessionAfterReturn(conv, leases); err != nil {
		return fmt.Errorf("%w: %w", errWarmConversationRetained, err)
	}
	deleteAssistantConversationFiles(userID, conv.ID)
	h.settleDeletedConversation(conv)
	h.queueConversationAttachments(conv)
	return nil
}

func (h *assistantHandlers) returnConversationWarmWorkspaces(ctx context.Context, conv domain.AssistantSession, drainUntil time.Time) (map[string]*domain.WarmWorkspaceLease, error) {
	leases := map[string]*domain.WarmWorkspaceLease{}
	for _, runID := range ownWorkspaceRunIDs(conv) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		run, err := h.repo.RunHeader(runID)
		if err != nil {
			return nil, err
		}
		if run.ID == "" || !run.IsWorkspace() || run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
			continue
		}
		if h.engine == nil {
			return nil, errors.New("工作区引擎不可用")
		}
		// Compare the same epoch at finalization, even if another generation
		// has since been acquired and returned between the two operations.
		l, _, found, err := h.repo.WarmWorkspaceAllocation(runID)
		if err != nil {
			return nil, err
		}
		if found {
			leases[runID] = &l
		} else {
			leases[runID] = nil
		}
		if err := h.returnWarmWorkspace(ctx, run, drainUntil); err != nil {
			return nil, err
		}
	}
	return leases, nil
}

func (h *assistantHandlers) returnWarmWorkspace(ctx context.Context, run domain.Run, drainUntil time.Time) error {
	for {
		err := h.engine.ReleaseWarmWorkspace(ctx, run, 0)
		if !errors.Is(err, repo.ErrWorkspaceInUse) || !time.Now().Before(drainUntil) {
			return err
		}
		// The turn or a concurrent return may still be handing back its use
		// lease. A terminal or external process is reported by the Runner.
		meta, ok := h.repo.WorkspaceCheckpoint(run.ID)
		if !ok || meta.LeaseUntil == nil || !meta.LeaseUntil.After(time.Now()) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// settleDeletedConversation handles every requirement a deleted conversation
// worked on, one requirement at a time because a conversation may work on
// several:
//
//   - handed in (see conversationIssueDelivered): the requirement stays, and
//     its workspaces only end so their sandboxes are reclaimed. The workspace
//     records carry the conversation's usage and delivery for analytics; they
//     have no nodes and are not listed anywhere.
//   - not handed in: its workspaces are deleted like before, and when this
//     conversation created the requirement and it was never dispatched to an
//     SOP the requirement is deleted as well.
//
// Requirements created elsewhere (TAPD, the requirement list, a dispatched
// SOP) are never deleted here.
func (h *assistantHandlers) settleDeletedConversation(conv domain.AssistantSession) {
	workspaces := map[string][]string{}
	issues := []string{}
	track := func(issueID string) {
		if _, ok := workspaces[issueID]; !ok {
			workspaces[issueID] = nil
			issues = append(issues, issueID)
		}
	}
	for _, runID := range ownWorkspaceRunIDs(conv) {
		run, ok := h.repo.RunState(runID)
		if !ok || !run.IsWorkspace() {
			continue
		}
		issueID := strings.TrimSpace(run.ExecutionContext.IssueID)
		if issueID == "" {
			h.discardConversationWorkspace(runID)
			continue
		}
		track(issueID)
		workspaces[issueID] = append(workspaces[issueID], runID)
	}
	if issueID := strings.TrimSpace(conv.IssueID); issueID != "" {
		track(issueID)
	}
	for _, issueID := range issues {
		issue, ok := h.repo.Issue(issueID)
		if ok && conversationIssueDelivered(issue) {
			for _, runID := range workspaces[issueID] {
				h.endConversationWorkspace(runID)
			}
			continue
		}
		for _, runID := range workspaces[issueID] {
			h.discardConversationWorkspace(runID)
		}
		if ok && issue.ConversationSessionID == conv.ID && issue.ProjectID == conv.ProjectID {
			h.deleteConversationIssue(issue)
		}
	}
}

// conversationIssueDelivered reports whether the work on a requirement was
// handed in: an MR was submitted (whether still open, merged or closed), it
// was pushed straight to a shared branch (which leaves it in review), or the
// requirement is already done.
func conversationIssueDelivered(issue domain.Issue) bool {
	return issue.Completed() || issue.DeliveryLocked() || len(issue.DeliveryMRList()) > 0 || strings.EqualFold(strings.TrimSpace(issue.Status), "in_review")
}

// deleteConversationIssue deletes a requirement a deleted conversation
// created and never handed in. A requirement that was dispatched to an SOP
// has become a regular one and stays, as does one another conversation's
// workspace is still working on.
func (h *assistantHandlers) deleteConversationIssue(issue domain.Issue) {
	dispatched, err := h.repo.IssueHasTasks(issue.ID)
	if err != nil || dispatched {
		if err != nil {
			log.Printf("keep requirement %s of a deleted conversation: %v", issue.ID, err)
		}
		return
	}
	runIDs, err := h.repo.IssueDeletionRuns(issue.ID, repo.IssueDeleteUnsuccessful)
	if err != nil {
		log.Printf("keep requirement %s of a deleted conversation: %v", issue.ID, err)
		return
	}
	for _, runID := range runIDs {
		if run, ok := h.repo.RunState(runID); ok && !run.IsWorkspace() {
			return
		}
	}
	if len(runIDs) > 0 && h.engine == nil {
		return
	}
	for _, runID := range runIDs {
		if err := h.engine.DeleteRun(runID); err != nil {
			log.Printf("keep requirement %s of a deleted conversation: delete workspace run=%s: %v", issue.ID, runID, err)
			return
		}
	}
	if err := h.repo.DeleteIssue(issue.ID); err != nil {
		log.Printf("delete requirement %s of a deleted conversation: %v", issue.ID, err)
	}
}

// endConversationWorkspace ends a workspace a deleted conversation opened,
// which reclaims its sandbox, and keeps the record. Something still running in
// it, or a paused or queued run, is left for the run page instead of being cut
// off; it reports whether the workspace has ended.
func (h *assistantHandlers) endConversationWorkspace(runID string) bool {
	runID = strings.TrimSpace(runID)
	if runID == "" || h.engine == nil {
		return false
	}
	run, ok := h.repo.RunState(runID)
	if !ok || !run.IsWorkspace() || run.PauseRequested {
		return false
	}
	if domain.TerminalRunStatus(run.Status) {
		return true
	}
	busy := run.Status != "waiting"
	for _, node := range run.Nodes {
		busy = busy || node != nil && (node.Status == "running" || node.Status == "waiting_approval")
	}
	if busy {
		log.Printf("keep workspace run=%s of a deleted conversation: it is %s or has work in progress", runID, run.Status)
		return false
	}
	if err := h.engine.Cancel(runID); err != nil {
		log.Printf("end workspace run=%s of a deleted conversation: %v", runID, err)
		return false
	}
	return true
}

// discardConversationWorkspace ends a workspace a deleted conversation opened
// and deletes its record. Only that conversation could reach it (forks do not
// inherit it), and a workspace run has no nodes, so its record would only be
// an empty page.
func (h *assistantHandlers) discardConversationWorkspace(runID string) {
	if !h.endConversationWorkspace(runID) {
		return
	}
	if err := h.engine.DeleteRun(runID); err != nil {
		log.Printf("delete workspace run=%s of a deleted conversation: %v", runID, err)
	}
}
