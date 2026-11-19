package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path/filepath"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

// The caller holds the owner gate for the duration of Git preparation.
// Keep the journal mutex available to inventories and other workspace owners.
func (p *warmWorkspacePool) prepareLeaseCheckout(ctx context.Context, l domain.WarmWorkspaceLease, inv *domain.RunnerInvocation) error {
	p.mu.Lock()
	a, err := p.loadLease(ctx, l)
	if err == nil && a.Phase != warmWorkspacePlaced {
		err = errors.New("warm workspace has not been placed")
	}
	if err == nil {
		_, err = p.inspectLease(a)
	}
	p.mu.Unlock()
	if err != nil || a.GitReady {
		return err
	}
	if inv == nil || len(inv.Repositories) != len(l.Slots) {
		return errors.New("incomplete warm checkout request")
	}
	ctx = gitwork.WithSSHKeys(ctx, inv.GitSSHKeys)
	ctx = invocationGitHTTPContext(ctx, inv)
	dirs := map[string]string{}
	for _, repo := range inv.Repositories {
		dirs[repo.RepositoryID] = repo.Dir
	}
	targets, err := p.ownerTargets(l, dirs)
	if err != nil || !maps.Equal(a.Targets, targets) {
		return errors.New("warm checkout differs from placed repositories")
	}
	for _, repo := range inv.Repositories {
		slotID := l.Slots[repo.RepositoryID]
		if err := prepareWarmProjectCheckout(ctx, targets[slotID], p.gitCache(slotID), p.slots[slotID].UnityProjectPath, repo, nil); err != nil {
			return err
		}
		if err := gitwork.RecordBaseline(targets[slotID], repo.Ref); err != nil {
			return err
		}
		if err := configureRepositoryGitIdentity(ctx, targets[slotID], inv.GitUserName, inv.GitUserEmail); err != nil {
			return err
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err = p.loadLease(ctx, l)
	if err != nil {
		return err
	}
	if _, err = p.inspectLease(a); err != nil {
		return err
	}
	a.GitReady = true
	return p.saveAllocation(a)
}

func (d *Daemon) checkWarmOperation(ctx context.Context, owner, operation string, observed *domain.WarmWorkspaceLease) error {
	p := d.warmWorkspacePool
	if p == nil {
		if observed != nil {
			return errors.New("warm workspace pool unavailable")
		}
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a, exists := p.state.Leases[owner]
	if !exists || a.Phase == warmWorkspaceReleased {
		if observed != nil {
			return errors.New("warm workspace lease no longer active")
		}
		return nil
	}
	if observed == nil || observed.RunnerID != d.runnerID() || !sameWarmWorkspaceLease(a.Lease, *observed) || a.Phase != warmWorkspacePlaced || !a.GitReady {
		return errors.New("missing, stale or unprepared warm workspace lease")
	}
	if _, err := p.loadLease(ctx, *observed); err != nil {
		return err
	}
	if _, err := p.inspectLease(a); err != nil {
		return err
	}
	if domain.IsWorkspaceCheckpointTransferOperation(operation) || operation == domain.RunnerWorkspaceOperationCheckpoint {
		return errors.New("whole-workspace checkpoints are unavailable for pooled workspaces")
	}
	switch operation {
	case domain.RunnerWorkspaceOperationReclaim, domain.RunnerTaskActionCleanupRun, domain.RunnerWorkspaceOperationRestore:
		return fmt.Errorf("operation %s cannot replace a pooled workspace", operation)
	}
	return nil
}

func (d *Daemon) checkWarmTask(ctx context.Context, task domain.RunnerTask) error {
	owner := d.taskGateKey(task)
	if err := d.checkWarmOperation(ctx, owner, task.Action, task.WarmWorkspaceLease); err != nil {
		return err
	}
	if task.WarmWorkspaceLease == nil {
		return nil
	}
	if d.taskWorkspace(task) != filepath.Join(d.warmWorkspacePool.workspaceRoot, "runs", owner, "workspace") {
		return errors.New("warm task must use the shared checkout")
	}
	if task.Action == "" && (task.Sandbox == nil || task.Sandbox.Profile.Driver != domain.SandboxDriverDocker || task.Invocation == nil && task.Command == "") {
		return errors.New("warm task requires Docker execution")
	}
	return nil
}

func checkoutForWorkspace(ctx context.Context, lease *domain.WarmWorkspaceLease, root string, inv *domain.RunnerInvocation, refresh ...bool) error {
	// Warm preparation has already checked the entire assignment under the owner gate.
	if lease != nil {
		return nil
	}
	return checkoutInvocationRepositories(ctx, root, inv, refresh...)
}
