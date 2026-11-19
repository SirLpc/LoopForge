package runner

import (
	"agentflow/internal/gitwork"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"slices"

	"agentflow/internal/domain"
)

func (p *warmWorkspacePool) inventory() []domain.WarmWorkspaceSlot {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ready(); err != nil {
		return nil
	}
	active := p.state.activeSlots()
	var inventory []domain.WarmWorkspaceSlot
	for _, id := range slices.Sorted(maps.Keys(p.slots)) {
		slot := p.slots[id]
		state := "blocked"
		if active[id] {
			state = "reserved"
		} else if !p.state.Slots[id].NeedsInspection && p.inspectSlot(id, "").atHome {
			state = "available"
		}
		inventory = append(inventory, domain.WarmWorkspaceSlot{ID: id, ProjectID: slot.ProjectID, RepositoryID: slot.RepositoryID, State: state, Generation: p.state.Slots[id].Generation})
	}
	return inventory
}

func (d *Daemon) warmWorkspaceControl(ctx context.Context, req domain.RunnerWorkspaceRequest) domain.RunnerWorkspaceResponse {
	p := d.warmWorkspacePool
	l := req.WarmWorkspaceLease
	if p == nil || l == nil || l.RunnerID != d.runnerID() || l.OwnerRunID != req.RunID || req.WorkspaceRunID != "" && req.WorkspaceRunID != req.RunID {
		return runnerWorkspaceError(errors.New("invalid warm workspace owner or runner"))
	}
	// Git preparation and release proofs have their own lifetimes. A short
	// control call may report placement, never readiness from placement alone.
	if req.Operation != domain.RunnerWorkspaceOperationWarmStatus {
		fence := d.workspaceGate("warm/" + l.OwnerRunID)
		if !fence.TryLock() {
			return workspaceBusyResponse("workspace is still in use")
		}
		defer fence.Unlock()
	}
	switch req.Operation {
	case domain.RunnerWorkspaceOperationWarmVerifyAccess:
		inv := req.PreparationInvocation
		if req.WarmWorkspaceActorID <= 0 || inv == nil || len(inv.Repositories) == 0 {
			return runnerWorkspaceError(errors.New("missing current repository actor or repositories"))
		}
		dir, err := os.MkdirTemp(p.journalDir.Name(), ".verify-access-")
		if err != nil {
			return runnerWorkspaceError(err)
		}
		defer os.RemoveAll(dir)
		if _, err = gitwork.C(ctx, dir, "init", "--bare", "--template="); err != nil {
			return runnerWorkspaceError(err)
		}
		auth := invocationGitHTTPContext(gitwork.WithSSHKeys(ctx, inv.GitSSHKeys), inv)
		for _, r := range inv.Repositories {
			if domain.URLHasCredentials(r.CloneURL) {
				return runnerWorkspaceError(errors.New("warm access check requires a credential-free remote"))
			}
			if err := gitwork.ValidateRemoteURL(r.CloneURL, true); err != nil {
				return runnerWorkspaceError(err)
			}
			if _, err := gitwork.C(repositoryGitContext(auth, r), dir, "ls-remote", "--exit-code", "--", r.CloneURL, "HEAD"); err != nil {
				return runnerWorkspaceError(errors.New("current actor cannot read the frozen remote"))
			}
		}
		return runnerWorkspacePayload(map[string]string{"phase": "authorized"})
	case domain.RunnerWorkspaceOperationWarmCheckout:
		if req.Sandbox == nil || req.Sandbox.Profile.Driver != domain.SandboxDriverDocker {
			return runnerWorkspaceError(errors.New("warm checkout requires Docker"))
		}
		gate := d.requestGate(req)
		if !gate.TryLock() {
			return workspaceBusyResponse("workspace is still in use")
		}
		defer gate.Unlock()
		if err := p.prepareLeaseCheckout(ctx, *l, req.PreparationInvocation); err != nil {
			return runnerWorkspaceError(err)
		}

	case domain.RunnerWorkspaceOperationWarmPrepare:
		gate := d.requestGate(req)
		if !gate.TryLock() {
			return workspaceBusyResponse("workspace is still in use")
		}
		defer gate.Unlock()
		dirs := map[string]string{}
		for _, r := range req.Repositories {
			dirs[r.RepositoryID] = r.WorkspaceDir
		}
		if _, err := p.ownerTargets(*l, dirs); err != nil {
			return runnerWorkspaceError(err)
		}
		if err := p.reserve(*l); err != nil {
			return runnerWorkspaceError(err)
		}
		if err := p.place(ctx, *l, dirs); err != nil {
			return runnerWorkspaceError(err)
		}
	case domain.RunnerWorkspaceOperationWarmStatus:
	case domain.RunnerWorkspaceOperationWarmReleaseCheck, domain.RunnerWorkspaceOperationWarmReleaseCommit:
		return d.warmReturnControl(ctx, req)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.loadLease(ctx, *l)
	if err != nil {
		return runnerWorkspaceError(err)
	}
	if _, err = p.inspectLease(a); err != nil {
		return runnerWorkspaceError(err)
	}
	if a.Phase == warmWorkspacePlaced && a.GitReady {
		a.Phase = "ready"
	}
	payload, _ := json.Marshal(map[string]warmWorkspacePhase{"phase": a.Phase})
	return domain.RunnerWorkspaceResponse{Payload: payload}
}
