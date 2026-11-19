package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"agentflow/internal/domain"
)

// AdvanceConversationWarmWorkspace performs one admission step for a parked
// generation. A late controller can never reopen an epoch cancelled by Stop.
func (e *Engine) AdvanceConversationWarmWorkspace(ctx context.Context, run domain.Run, epoch uint64, actor int64) (bool, error) {
	l, state, ok, err := e.repo.WarmWorkspaceAllocation(run.ID)
	if err != nil {
		return false, err
	}
	if !ok || l.Epoch != epoch {
		return false, errors.New("预热工作区预约已变更")
	}
	switch state {
	case domain.WarmWorkspaceQueued:
		if _, _, err := e.repo.ReserveNextWarmWorkspace(l.RunnerID, time.Now()); err != nil {
			return false, err
		}
		return false, nil
	case domain.WarmWorkspaceReserved:
		runner, ok := e.repo.Runner(l.RunnerID)
		if !ok || runner.Status != domain.RunnerStatusOnline {
			return false, nil
		}
		if err = e.RecordWarmWorkspaceActor(run, l, actor); err != nil {
			return false, err
		}
		ready, err := e.confirmWarmWorkspace(WithWorkspaceRestoreActor(ctx, actor), run, l)
		if err != nil || !ready {
			return false, err
		}
		if err = e.repo.ConfirmWarmWorkspaceBinding(l, domain.WarmWorkspaceReady); err != nil {
			return false, err
		}
	case domain.WarmWorkspaceReady:
		if r, ok := e.repo.Runner(l.RunnerID); !ok || r.Status != domain.RunnerStatusOnline {
			return false, nil
		}
	default:
		return false, fmt.Errorf("预热工作区预约状态为 %s", state)
	}
	err = e.AuthorizeConversationWarmWorkspace(ctx, run, actor)
	if errors.Is(err, errWarmAccessUnavailable) {
		return false, nil
	}
	return err == nil, err
}

// Validate the current speaker against every frozen remote before revealing
// the checkout, even if a different speaker already prepared this lease.
func (e *Engine) AuthorizeConversationWarmWorkspace(ctx context.Context, run domain.Run, actor int64) error {
	if run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
		return nil
	}
	ctx = WithWorkspaceRestoreActor(ctx, actor)
	if err := e.CheckWorkspaceRestoreAccess(ctx, run); err != nil {
		return err
	}
	if run.IsWorkspace() {
		l, state, found, err := e.repo.WarmWorkspaceAllocation(run.ID)
		if err != nil {
			return err
		}
		if found && state == domain.WarmWorkspaceReady {
			return e.RecordWarmWorkspaceActor(run, l, actor)
		}
	}
	return nil
}
