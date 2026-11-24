package runner

import (
	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
	"context"
	"errors"
	"path/filepath"
	"time"
)

func (d *Daemon) warmReturnControl(ctx context.Context, req domain.RunnerWorkspaceRequest) (response domain.RunnerWorkspaceResponse) {
	if d.cfg.WarmNative {
		start := time.Now()
		defer func() {
			var err error
			if response.Error != "" {
				err = errors.New(response.Error)
			}
			recordWarmGitDuration(ctx, req.Operation, "native", start, err)
		}()
	}
	return d.warmReturnWithProcessInventory(ctx, req, runtime.CheckWarmWorkspaceProcesses)
}
func (d *Daemon) warmReturnWithProcessInventory(ctx context.Context, req domain.RunnerWorkspaceRequest, checkProcesses func(context.Context, string) error) domain.RunnerWorkspaceResponse {
	p := d.warmWorkspacePool
	l := *req.WarmWorkspaceLease
	gate := d.requestGate(req)
	if !gate.TryLock() {
		return workspaceBusyResponse("close active terminals and finish workspace execution before return")
	}
	defer gate.Unlock()
	p.mu.Lock()
	a, err := p.loadLease(ctx, l)
	prior, exists := p.state.Leases[l.OwnerRunID]
	p.mu.Unlock()
	if err != nil && req.Operation == domain.RunnerWorkspaceOperationWarmReleaseCheck && (!exists || prior.Phase == warmWorkspaceReleased && prior.Lease.Generation < l.Generation) {
		if err = p.reserve(l); err == nil {
			p.mu.Lock()
			a, err = p.loadLease(ctx, l)
			p.mu.Unlock()
		}
	}
	if err != nil {
		return runnerWorkspaceError(err)
	}
	if p.native && req.Operation == domain.RunnerWorkspaceOperationWarmReleaseCommit &&
		(a.Phase == warmWorkspaceReleased || a.NativeReturn != nil && a.NativeReturn.Invalidated) {
		return p.nativeReturnResponse(ctx, l, req)
	}
	if d.workspaceTerminals.HasRunScope(l.OwnerRunID) {
		return workspaceBusyResponse("close the workspace terminal before return")
	}
	if a.Phase != warmWorkspaceReleased && a.Phase != warmWorkspaceReserved {
		d.runSandboxMu.Lock()
		for _, entry := range d.runSandboxes {
			if entry.warmOwner == l.OwnerRunID && entry.active > 0 {
				d.runSandboxMu.Unlock()
				return workspaceBusyResponse("workspace sandbox is in use")
			}
		}
		d.runSandboxMu.Unlock()
		d.closeSharedSandboxes(ctx, func(entry *runSandbox) bool { return entry.warmOwner == l.OwnerRunID })
		root := filepath.Join(p.workspaceRoot, "runs", l.OwnerRunID, "workspace")
		err := d.fenceWarmWorkspaceReturn(ctx, root, checkProcesses)
		if err != nil {
			r := runnerWorkspaceError(err)
			if a.GitReady && !a.ReturnProven {
				r.Code = "warm_owner_retained"
			}
			return r
		}
	}
	if req.Operation == domain.RunnerWorkspaceOperationWarmReleaseCommit {
		if p.native {
			return p.nativeReturnResponse(ctx, l, req)
		}
		if err := p.commitReturn(ctx, l, req.WarmWorkspaceRefs); err != nil {
			return runnerWorkspaceError(err)
		}
		return runnerWorkspacePayload(map[string]string{"phase": "released"})
	}
	inv := req.PreparationInvocation
	if inv == nil {
		return runnerWorkspaceError(errors.New("missing frozen return repositories"))
	}
	ctx = gitwork.WithSSHKeys(ctx, inv.GitSSHKeys)
	ctx = invocationGitHTTPContext(ctx, inv)
	refs, err := p.proveReturnWithArtifacts(ctx, l, inv.Repositories, req.ArtifactFiles)
	if err != nil {
		r := runnerWorkspaceError(err)
		if a.Phase == warmWorkspacePlaced && a.GitReady && !a.ReturnProven {
			r.Code = "warm_owner_retained"
		}
		return r
	}
	resp := runnerWorkspacePayload(refs)
	if p.native {
		p.mu.Lock()
		resp.WarmWorkspaceReturnID = nativeWarmReturnID(p.state.Leases[l.OwnerRunID])
		p.mu.Unlock()
	}
	return resp
}

func (d *Daemon) fenceWarmWorkspaceReturn(ctx context.Context, root string, checkProcesses func(context.Context, string) error) error {
	if d.cfg.WarmNative {
		return d.fenceNativeWarmReturn(ctx, root)
	}
	if err := runtime.FenceWarmWorkspaceContainers(ctx, root); err != nil {
		return err
	}
	return checkProcesses(ctx, root)
}
