package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"agentflow/internal/domain"
)

func (e *Engine) warmWorkspaceOwner(run domain.Run) (domain.Run, error) {
	if !sharesWorkspace(run) {
		return run, nil
	}
	owner, ok := e.repo.RunState(workspaceRunID(run))
	if !ok && run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
		// Older shared runs may outlive the parent's run row. A durable pool
		// demand, if any, still prevents an unguarded historical checkout.
		_, _, found, err := e.repo.WarmWorkspaceAllocation(workspaceRunID(run))
		if err != nil {
			return owner, err
		}
		if !found {
			return run, nil
		}
	}
	if !ok || sharesWorkspace(owner) {
		return owner, errors.New("shared workspace owner unavailable")
	}
	return owner, nil
}

func (e *Engine) WarmWorkspaceStatus(runID string) (domain.WarmWorkspaceState, error) {
	run, ok := e.repo.RunState(runID)
	if !ok {
		return "", errors.New("run not found")
	}
	_, state, _, err := e.repo.WarmWorkspaceAllocation(workspaceRunID(run))
	return state, err
}

func supportsWarmDemand(runner domain.Runner, project string, repos []domain.RepositoryContext) bool {
	if !runner.SupportsWarmWorkspace() || len(repos) == 0 {
		return false
	}
	supported := map[string]bool{}
	for _, s := range runner.WarmWorkspaceSlots {
		if s.ProjectID == project && s.State != "blocked" {
			supported[s.RepositoryID] = true
		}
	}
	for _, r := range repos {
		if !supported[r.RepositoryID] {
			return false
		}
	}
	return true
}

func (e *Engine) readyWarmLease(run domain.Run) (*domain.WarmWorkspaceLease, error) {
	owner, err := e.warmWorkspaceOwner(run)
	if err != nil {
		return nil, err
	}
	if owner.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
		return nil, nil
	}
	l, state, found, err := e.repo.WarmWorkspaceAllocation(owner.ID)
	if err != nil {
		return nil, err
	}
	if !found || state != domain.WarmWorkspaceReady || l.RunnerID != e.defaultExecutionRunner(run) {
		return nil, errors.New("warm workspace is not ready on the bound runner")
	}
	return &l, nil
}

// BindWorkspaceLease adds the persisted owner and generation before a workspace
// operation enters a transaction or is sent over the runner transport.
func (e *Engine) BindWorkspaceLease(run domain.Run, req *domain.RunnerWorkspaceRequest) error {
	if err := e.guardWarmRequest(run, req); err != nil {
		return err
	}
	if req.WarmWorkspaceLease != nil {
		req.RunID = run.ID
	}
	return nil
}

func (e *Engine) guardWarmRequest(run domain.Run, req *domain.RunnerWorkspaceRequest) error {
	lease, err := e.readyWarmLease(run)
	if err != nil {
		return err
	}
	if observed := req.WarmWorkspaceLease; observed != nil && (lease == nil || observed.OwnerRunID != lease.OwnerRunID || observed.RunnerID != lease.RunnerID || observed.Generation != lease.Generation || observed.Epoch != lease.Epoch || !maps.Equal(observed.Slots, lease.Slots)) {
		return errors.New("stale workspace operation")
	}
	if lease != nil {
		req.WorkspaceRunID = lease.OwnerRunID
		req.WorkspaceLayout = domain.RunnerWorkspaceLayoutShared
	}
	req.WarmWorkspaceLease = lease
	return nil
}

func (e *Engine) putWorkspaceRunnerTask(task domain.RunnerTask) error {
	if task.RunID != "" {
		if run, ok := e.repo.RunState(task.RunID); ok {
			l, err := e.readyWarmLease(run)
			if err != nil {
				return err
			}
			if l != nil && (task.RunnerID != l.RunnerID || task.Action == domain.RunnerTaskActionCleanupRun || task.Workspace != runnerWorkspaceRel(run, task.NodeID)) {
				return errors.New("invalid pooled workspace task")
			}
			task.WarmWorkspaceLease = l
		}
	}
	return e.repo.PutRunnerTask(task)
}

func (e *Engine) EnsureWarmWorkspace(ctx context.Context, run domain.Run, actorID int64) (domain.WarmWorkspaceLease, error) {
	var empty domain.WarmWorkspaceLease
	owner, err := e.warmWorkspaceOwner(run)
	if err != nil {
		return empty, err
	}
	if owner.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
		return empty, nil
	}
	if owner.ID != run.ID {
		if snapshot := run.WorkflowSnapshot.SandboxSnapshot; snapshot == nil || snapshot.Profile.Driver != domain.SandboxDriverDocker {
			return empty, errors.New("共享预热工作区需要 Docker 沙箱")
		}
		lease, err := e.readyWarmLease(run)
		if err != nil {
			return empty, err
		}
		return *lease, nil
	}
	if run.WorkflowSnapshot.SandboxSnapshot == nil || run.WorkflowSnapshot.SandboxSnapshot.Profile.Driver != domain.SandboxDriverDocker {
		return empty, errors.New("预热工作区需要 Docker 沙箱")
	}
	runnerID := e.defaultExecutionRunner(owner)
	repos := owner.RepositoryContexts()
	_, _, alreadyQueued, err := e.repo.WarmWorkspaceAllocation(owner.ID)
	if err != nil {
		return empty, err
	}
	runner, ok := e.repo.Runner(runnerID)
	if !alreadyQueued && (!ok || !supportsWarmDemand(runner, owner.ExecutionContext.ProjectID, repos)) {
		return empty, errors.New("执行机器缺少项目所需的预热工作区")
	}
	ids := make([]string, 0, len(repos))
	for _, r := range repos {
		ids = append(ids, r.RepositoryID)
	}
	if err = e.repo.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: owner.ID, RunnerID: runnerID, ProjectID: owner.ExecutionContext.ProjectID, RepositoryIDs: ids}, time.Now()); err != nil {
		return empty, err
	}
	announced := false
	for {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		l, state, _, err := e.repo.WarmWorkspaceAllocation(owner.ID)
		if err != nil {
			return empty, err
		}
		current, ok := e.repo.RunState(owner.ID)
		if !ok {
			return empty, errors.New("workspace owner disappeared")
		}
		if current.CancelRequested || current.Status == "cancelled" {
			return empty, errors.Join(context.Canceled, e.repo.CancelWarmWorkspaceEpoch(owner.ID, l.Epoch, time.Now()))
		}
		if run.Generation != current.Generation {
			return empty, errors.New("run generation changed")
		}
		switch state {
		case domain.WarmWorkspaceReady:
			return l, nil
		case domain.WarmWorkspaceQueued:
			var allocated domain.WarmWorkspaceLease
			var ok bool
			if runner, found := e.repo.Runner(runnerID); found && runner.Status == domain.RunnerStatusOnline {
				allocated, ok, err = e.repo.ReserveNextWarmWorkspace(runnerID, time.Now())
			}
			if err != nil {
				return empty, err
			}
			if ok && allocated.OwnerRunID == owner.ID {
				continue
			}
			if !announced {
				announced = true
				e.updateRun(owner.ID, func(r *domain.Run) {
					e.event(r, "", "workspace.pool_waiting", "正在等待执行机器上的预热工作区")
				})
			}
		case domain.WarmWorkspaceReserved:
			runner, online := e.repo.Runner(runnerID)
			if !online || runner.Status != domain.RunnerStatusOnline {
				break
			}
			if actorID != owner.ExecutionContext.ResponsibleUserID {
				return empty, errors.New("warm checkout actor differs from frozen owner")
			}
			ready, err := e.confirmWarmWorkspace(ctx, owner, l)
			if err != nil {
				return empty, err
			}
			if ready {
				return l, e.repo.SettleWarmWorkspace(l, domain.WarmWorkspaceReady)
			}

		case domain.WarmWorkspaceReleased, domain.WarmWorkspaceCanceled:
			if current.Status != "running" {
				return empty, fmt.Errorf("workspace owner is %s", current.Status)
			}
			if err := e.repo.RequeueWarmWorkspace(l, time.Now()); err != nil {
				return empty, err
			}
			continue
		default:
			return empty, fmt.Errorf("warm workspace is %s", state)
		}
		select {
		case <-ctx.Done():
			return empty, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (e *Engine) freezeWarmWorkspace(ec *domain.TelemetryContext, repositories []domain.RepositoryContext, profile domain.SandboxProfile) error {
	if ec.WorkspaceRunID != "" { // A pinned SOP borrows its existing owner.
		owner, ok := e.repo.RunState(ec.WorkspaceRunID)
		if !ok {
			return errors.New("shared workspace owner unavailable")
		}
		ec.WorkspaceMode = owner.ExecutionContext.WorkspaceMode
		if ec.WorkspaceMode == domain.WorkspaceModeWarmPool && (profile.Driver != domain.SandboxDriverDocker || ec.ExecutionRunnerID != e.defaultExecutionRunner(owner)) {
			return errors.New("borrowed warm workspace requires the owner's Docker runner")
		}
		return nil
	}
	ec.WorkspaceMode = domain.WorkspaceModeCheckout
	if len(repositories) == 0 {
		return nil
	}
	if project, ok := e.repo.Project(ec.ProjectID); ok {
		ec.WorkspaceMode = project.WorkspaceMode
	}
	if ec.WorkspaceMode != domain.WorkspaceModeWarmPool {
		return nil
	}
	if profile.Driver != domain.SandboxDriverDocker {
		return errors.New("预热工作区需要 Docker 沙箱")
	}
	if ec.ExecutionRunnerID == "" && !ec.ExecutionPinned {
		e.mu.Lock()
		pool := e.cloudPool
		e.mu.Unlock()
		if pool.serves(ec.ProjectID) {
			for _, r := range e.eligiblePoolRunners(pool.Label) {
				if supportsWarmDemand(r, ec.ProjectID, repositories) {
					ec.ExecutionRunnerID = r.ID
					ec.ExecutionPool = domain.ExecutionModeCloud
					break
				}
			}
		}
	}
	r, ok := e.repo.Runner(ec.ExecutionRunnerID)
	if !ok || !supportsWarmDemand(r, ec.ProjectID, repositories) {
		return errors.New("执行机器缺少项目所需的预热工作区")
	}
	return nil
}

// confirmWarmWorkspace returns false on a transient transport or owner-gate
// conflict. Replaying either RPC against the same generation is idempotent.
func (e *Engine) confirmWarmWorkspace(ctx context.Context, owner domain.Run, l domain.WarmWorkspaceLease) (bool, error) {
	for _, op := range []string{domain.RunnerWorkspaceOperationWarmPrepare, domain.RunnerWorkspaceOperationWarmCheckout} {
		resp, err := e.requestWorkspaceCheckpoint(ctx, owner, domain.RunnerWorkspaceRequest{Operation: op, RunID: owner.ID, WarmWorkspaceLease: &l})
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrWorkspaceControlConfiguration) {
				return false, err
			}
			return false, nil
		}
		if resp.Status == 429 || resp.Status == 503 || resp.Code == domain.WorkspaceErrorBusy {
			return false, nil
		}
		var result struct{ Phase string }
		valid := resp.Error == "" && resp.Status < 400 && json.Unmarshal(resp.Payload, &result) == nil
		ready := result.Phase == "ready"
		placed := op == domain.RunnerWorkspaceOperationWarmPrepare && result.Phase == "placed"
		if !valid || !ready && !placed {
			err := fmt.Errorf("warm workspace %s returned phase %q: %s", op, result.Phase, resp.Error)
			return false, errors.Join(err, e.repo.SettleWarmWorkspace(l, domain.WarmWorkspaceBlocked))
		}
	}
	return true, nil
}
