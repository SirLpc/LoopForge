package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
)

type warmConversationHandoff struct {
	run             domain.Run
	previous        string
	nativeSessionID string
}

func (*warmConversationHandoff) Error() string { return "等待预热工作区" }

type retainedWarmWorkspaceError struct{ error }

type warmGenerationHandoffKey struct{}
type warmContinuationErrorKey struct{}
type warmNativeSessionKey struct{}
type warmAdmittedTurnKey struct{}

func (h *agentToolGateway) openWarmRepository(s *AgentToolSession, c workspaceOpenChoice) (string, error) {
	if !s.canOpenConversationWorkspace() || s.ProjectID == "" || s.UserID != s.User.ID || h.engine == nil {
		return "", errAgentToolForbidden
	}
	s.workspaceMu.Lock()
	defer s.workspaceMu.Unlock()
	if s.requestedMode() != "" {
		return "", errors.New("已有工作区交接请求")
	}
	project, ok := h.repo.Project(s.ProjectID)
	if !ok || project.ArchivedAt != nil || !workbenchProjectAllowsUser(project, s.UserID, h.repo.IsPlatformAdmin(s.User.LoginName)) {
		return "", errAgentToolForbidden
	}
	conv, ok := h.repo.AssistantSession(s.ChatOwnerID, s.ChatSessionID)
	if !ok || conv.ProjectID != s.ProjectID || conv.RunID != "" {
		return "", errAgentToolForbidden
	}
	registered, ok := h.repo.Repository(c.RepositoryID)
	if !ok || registered.ProjectID != s.ProjectID {
		return "", fmt.Errorf("仓库 %s 未登记在当前项目", c.RepositoryID)
	}
	sandboxID := s.SandboxProfileID
	if sandboxID == "" {
		sandboxID = c.SandboxID
	}
	if sandboxID == "" && conv.WorkspaceRunID != "" {
		if current, ok := h.repo.RunState(conv.WorkspaceRunID); ok {
			sandboxID = current.WorkflowSnapshot.SandboxProfileID
		}
	}
	sandbox, err := h.repo.ResolveConversationSandbox(s.ProjectID, sandboxID)
	if err != nil {
		return "", err
	}
	var target domain.Run
	own := ownWorkspaceRunIDs(conv)
	for i := len(own) - 1; i >= 0; i-- {
		run, ok := h.repo.RunState(own[i])
		rs := run.RepositoryContexts()
		if ok && run.IsWorkspace() && run.ExecutionContext.WorkspaceMode == domain.WorkspaceModeWarmPool && run.WorkflowSnapshot.SandboxProfileID == sandbox.ID && len(rs) == 1 && rs[0].RepositoryID == registered.ID && conversationMayBind(conv, run, s.ProjectID) {
			if target.ID == "" || run.ID == conv.WorkspaceRunID {
				target = run
			}
			if run.ID == conv.WorkspaceRunID {
				break
			}
		}
	}
	if target.ID == "" {
		mode, err := h.repo.WorkspaceMode(s.ProjectID)
		if err != nil {
			return "", err
		}
		if mode != domain.WorkspaceModeWarmPool {
			return "", errors.New("当前项目未启用预热池，请打开普通工作区")
		}
		target, err = h.engine.StartRepositoryConversationWorkspace("对话工作区："+registered.Name, domain.TelemetryContext{ProjectID: s.ProjectID, ResponsibleUserID: s.UserID, ExecutionPinned: true, ExecutionMode: conv.Execution().Pool, ExecutionRunnerID: conv.Execution().RunnerID}, sandbox.ID, repositoryRunContext(registered))
		if err != nil {
			return "", err
		}
	}
	if !slices.Contains(ownWorkspaceRunIDs(conv), target.ID) {
		if err := h.repo.RememberWarmConversationWorkspace(s.ChatOwnerID, s.ChatSessionID, s.ProjectID, target.ID); err != nil {
			h.cancelUnpreparedConversationWorkspace(target.ID)
			return "", err
		}
	}
	return h.requestWarmHandoff(s, conv, target)
}

func (h *agentToolGateway) requestWarmHandoff(s *AgentToolSession, conv domain.AssistantSession, run domain.Run) (string, error) {
	if run.ExecutionContext.ExecutionRunnerID != conv.Execution().RunnerID {
		return "", errors.New("工作区与会话不在同一执行机器")
	}
	pending := &warmConversationHandoff{run: run, previous: conv.WorkspaceRunID}
	s.mu.Lock()
	if s.pendingMode != "" {
		s.mu.Unlock()
		return "", errors.New("已有工作区交接请求")
	}
	s.pendingMode = domain.AssistantModeWorkspace
	s.modeAction = func(ctx context.Context) error {
		state, err := h.engine.WarmWorkspaceStatus(run.ID)
		if err != nil {
			return err
		}
		if pending.previous == run.ID && state == domain.WarmWorkspaceReady {
			return nil
		}
		if state == domain.WarmWorkspaceReleased {
			if err := h.engine.AuthorizeConversationWarmWorkspace(ctx, run, s.UserID); err != nil {
				return err
			}
		}
		if err := h.finishPreviousWarmUse(ctx, s, pending); err != nil {
			return err
		}
		return pending
	}
	s.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"runId": run.ID, "handoff": true, "notice": assistantHandoffNotice})
	return string(raw), err
}

// Only the drained phase holds the old turn's write capability. On a one-slot
// machine the previous binding stays usable if safety checks retain its files.
func (h *agentToolGateway) finishPreviousWarmUse(ctx context.Context, s *AgentToolSession, p *warmConversationHandoff) error {
	if p.previous == "" || p.previous == p.run.ID {
		return nil
	}
	if t := s.workspaceTurn; t != nil {
		t.operations.Lock()
		defer t.operations.Unlock()
	}
	old, ok := h.repo.RunState(p.previous)
	if !ok {
		return errors.New("原工作区已变更")
	}
	if old.IsWorkspace() && old.ExecutionContext.WorkspaceMode == domain.WorkspaceModeWarmPool {
		l, state, found, err := h.repo.WarmWorkspaceAllocation(old.ID)
		if err != nil {
			return err
		}
		if found && state != domain.WarmWorkspaceReleased && state != domain.WarmWorkspaceCanceled {
			releaseCtx := ctx
			if s.workspaceTurn != nil {
				releaseCtx, err = s.workspaceTurn.hold(ctx, old.ID)
				if err != nil {
					return retainedWarmWorkspaceError{err}
				}
			} else {
				token, err := h.engine.BeginConversationWorkspaceUse(old.ID)
				if err != nil {
					return retainedWarmWorkspaceError{fmt.Errorf("原工作区仍在使用：%w", err)}
				}
				defer h.repo.FinishWorkspaceUse(old.ID, token, time.Now(), nil, "")
				releaseCtx = engine.WithConversationWorkspaceToken(ctx, token, old.ID)
			}
			if err = h.engine.ReleaseWarmWorkspace(releaseCtx, old, s.UserID); err != nil {
				// An independent slot can satisfy the new owner while the old owner
				// retains its files. Keep both owners in the conversation history.
				if !h.hasOtherWarmSlot(p.run, l) {
					return retainedWarmWorkspaceError{fmt.Errorf("请先处理原工作区的改动或关闭终端：%w", err)}
				}
			}
		}
	}
	if s.workspaceTurn != nil {
		return s.workspaceTurn.checkpoint(ctx)
	}
	return nil
}
func (h *agentToolGateway) hasOtherWarmSlot(run domain.Run, old domain.WarmWorkspaceLease) bool {
	r, ok := h.repo.Runner(run.ExecutionContext.ExecutionRunnerID)
	if !ok {
		return false
	}
	repos := run.RepositoryContexts()
	if len(repos) != 1 {
		return false
	}
	for _, slot := range r.WarmWorkspaceSlots {
		if slot.ProjectID == run.ExecutionContext.ProjectID && slot.RepositoryID == repos[0].RepositoryID && slot.State != "blocked" && old.Slots[slot.RepositoryID] != slot.ID {
			return true
		}
	}
	return false
}

func (h *agentToolGateway) reopenWarmOwner(s *AgentToolSession, c workspaceOpenChoice) (string, bool, error) {
	id := c.WorkspaceRunID
	if id == "" {
		id = s.conversationWorkspace()
	}
	if id == "" {
		return "", false, nil
	}
	run, ok := h.repo.RunState(id)
	if !ok || !run.IsWorkspace() || run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
		return "", false, nil
	}
	conv, ok := h.repo.AssistantSession(s.ChatOwnerID, s.ChatSessionID)
	if !ok || !conversationMayBind(conv, run, s.ProjectID) || conv.ProjectID != s.ProjectID {
		return "", true, errAgentToolForbidden
	}
	wanted := s.SandboxProfileID
	if wanted == "" {
		wanted = c.SandboxID
	}
	if wanted != "" && wanted != run.WorkflowSnapshot.SandboxProfileID {
		if c.WorkspaceRunID != "" {
			return "", true, errors.New("已有工作区使用其冻结的运行环境")
		}
		return "", false, nil
	}
	raw, err := h.requestWarmHandoff(s, conv, run)
	return raw, true, err
}
