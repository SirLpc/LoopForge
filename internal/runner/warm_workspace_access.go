package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/preview"
	"agentflow/internal/runtime"
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
	if err == nil && a.ReturnProven {
		return errors.New("workspace is frozen for return")
	}
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
	if !a.CheckoutComplete {
		p.mu.Lock()
		a.PreparingGit = true
		err = p.saveAllocation(a)
		p.mu.Unlock()
		if err != nil {
			return err
		}
		for _, repo := range inv.Repositories {
			slotID := l.Slots[repo.RepositoryID]
			var restore *domain.WarmWorkspaceRef
			for i := range inv.WarmWorkspaceRefs {
				if inv.WarmWorkspaceRefs[i].RepositoryID == repo.RepositoryID {
					restore = &inv.WarmWorkspaceRefs[i]
					break
				}
			}
			if len(inv.WarmWorkspaceRefs) > 0 && restore == nil {
				return errors.New("missing recorded repository revision")
			}
			if err := prepareWarmProjectCheckout(ctx, targets[slotID], p.gitCache(slotID), p.slots[slotID].UnityProjectPath, p.slots[slotID].ProjectID, repo, restore); err != nil {
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
		a.CheckoutComplete = true
		err = p.saveAllocation(a)
		p.mu.Unlock()
		if err != nil {
			return err
		}
	}
	root := filepath.Join(p.workspaceRoot, "runs", l.OwnerRunID, "workspace")
	if _, err := runtime.RestoreMissingWorkspaceArtifacts(root, inv.ArchivedFiles); err != nil {
		return err
	}
	for _, file := range inv.ArchivedFiles {
		path := filepath.Join(root, file.Path)
		parent, err := openWarmWorkspaceDir(filepath.Dir(path), false)
		if err != nil {
			return err
		}
		parent.Close()
		if err := warmRegular(path); err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil || runtime.WorkspaceContentSHA(string(content)) != file.SHA256 {
			return fmt.Errorf("restored artifact differs at %s", file.Path)
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
	if observed != nil && d.warmDockerUnavailable() {
		return errWarmDockerUnavailable
	}
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
	if exists && a.Phase == warmWorkspaceReleased && (domain.IsWholeWorkspaceOperation(operation) || operation == domain.RunnerTaskActionCleanupRun) {
		return errors.New("whole-workspace cleanup and archives are unavailable for a returned pool lease")
	}
	if !exists || a.Phase == warmWorkspaceReleased {
		if observed != nil {
			return errors.New("warm workspace lease no longer active")
		}
		return nil
	}
	if observed == nil || observed.RunnerID != d.runnerID() || !sameWarmWorkspaceLease(a.Lease, *observed) || a.Phase != warmWorkspacePlaced || !a.GitReady || a.ReturnProven {
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
	if task.Action == domain.RunnerTaskActionArchivePreview {
		dir, err := d.openWarmPreview(task)
		if err != nil {
			return err
		}
		dir.Close()
	}
	if task.Action == "" && (task.Sandbox == nil || task.Sandbox.Profile.Driver != domain.SandboxDriverDocker || task.Invocation == nil && task.Command == "") {
		return errors.New("warm task requires Docker execution")
	}
	return nil
}

// Pin the allowed physical output directory before walking or reading any members.
func (d *Daemon) openWarmPreview(task domain.RunnerTask) (*os.Root, error) {
	if task.PreviewArchive == nil {
		return nil, errors.New("missing preview path")
	}
	rel, err := preview.SanitizeRelPath(task.PreviewArchive.SourcePath)
	if err != nil || rel != task.PreviewArchive.SourcePath {
		return nil, errors.New("warm previews require a canonical output directory")
	}
	source := filepath.Join(d.taskWorkspace(task), rel)
	p := d.warmWorkspacePool
	p.mu.Lock()
	defer p.mu.Unlock()
	pinned, err := openWarmWorkspaceDir(source, false)
	if err != nil {
		return nil, err
	}
	defer pinned.Close()
	lineage, err := warmWorkspaceDirLineage(source)
	if err != nil {
		return nil, err
	}
	actual, err := warmWorkspaceDirIdentity(pinned)
	if err != nil || lineage[len(lineage)-1] != actual {
		return nil, errors.New("preview source changed")
	}
	for id, target := range p.state.Leases[d.taskGateKey(task)].Targets {
		checkout, err := warmWorkspaceDirLineage(target)
		if err != nil {
			return nil, err
		}
		if checkout[len(checkout)-1] != p.state.Slots[id].Identity {
			return nil, errors.New("warm preview checkout identity changed")
		}
		if warmPreviewProtected(lineage, checkout, false) {
			return nil, errors.New("warm previews cannot archive a checkout")
		}
		for _, cache := range []string{filepath.Join(target, ".git"), filepath.Join(target, p.slots[id].UnityProjectPath, "Library")} {
			protected, err := warmWorkspaceDirLineage(cache)
			if err != nil && !os.IsNotExist(err) {
				return nil, err
			}
			if warmPreviewProtected(lineage, protected, err == nil) {
				return nil, errors.New("warm previews cannot archive a protected cache")
			}
		}
	}
	expected, err := pinned.Stat()
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(source)
	if err != nil {
		return nil, err
	}
	observed, err := root.Stat(".")
	if err != nil || !os.SameFile(expected, observed) {
		root.Close()
		return nil, errors.New("preview source changed")
	}
	return root, nil
}

func warmPreviewProtected(source, protected []warmWorkspaceIdentity, coverChildren bool) bool {
	return slices.Contains(protected, source[len(source)-1]) || coverChildren && slices.Contains(source, protected[len(protected)-1])
}

func checkoutForWorkspace(ctx context.Context, lease *domain.WarmWorkspaceLease, root string, inv *domain.RunnerInvocation, refresh ...bool) error {
	// Warm preparation has already checked the entire assignment under the owner gate.
	if lease != nil {
		return nil
	}
	return checkoutInvocationRepositories(ctx, root, inv, refresh...)
}
