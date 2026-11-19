package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"agentflow/internal/domain"
)

// Rebuild each control message from the frozen owner. The caller supplies
// the generation it observed so stale messages fail after reassignment.
func (e *Engine) warmWorkspaceControlRequest(run domain.Run, input domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceRequest, error) {
	var req domain.RunnerWorkspaceRequest
	runnerID := e.defaultExecutionRunner(run)
	if runnerID == "" || input.RunID != run.ID || sharesWorkspace(run) || run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool || input.WorkspaceRunID != "" && input.WorkspaceRunID != run.ID {
		return req, errors.New("invalid warm workspace owner")
	}
	req = domain.RunnerWorkspaceRequest{RunID: run.ID, WorkspaceRunID: run.ID, Operation: input.Operation, WarmWorkspaceLease: input.WarmWorkspaceLease, WarmWorkspaceActorID: input.WarmWorkspaceActorID}
	if err := e.repo.AuthorizeWarmWorkspaceControl(runnerID, run, &req); err != nil {
		return req, err
	}
	if req.Operation == domain.RunnerWorkspaceOperationWarmCheckout || req.Operation == domain.RunnerWorkspaceOperationWarmReleaseCheck || req.Operation == domain.RunnerWorkspaceOperationWarmVerifyAccess {
		req.Sandbox = run.WorkflowSnapshot.SandboxSnapshot
		if req.Sandbox == nil || req.Sandbox.Profile.Driver != domain.SandboxDriverDocker {
			return req, errors.New("warm checkout requires Docker")
		}
		inv := new(domain.RunnerInvocation)
		actor := run.ExecutionContext.ResponsibleUserID
		if req.Operation == domain.RunnerWorkspaceOperationWarmVerifyAccess || req.Operation == domain.RunnerWorkspaceOperationWarmCheckout && req.WarmWorkspaceActorID != 0 {
			actor = req.WarmWorkspaceActorID
		} else if req.Operation == domain.RunnerWorkspaceOperationWarmReleaseCheck || run.IsWorkspace() {
			selected, err := e.repo.WarmWorkspaceActor(*req.WarmWorkspaceLease)
			if err != nil {
				return req, err
			}
			if selected != 0 {
				actor = selected
			}
		}
		if err := e.authorizeWarmActor(run, actor); err != nil {
			return req, errors.Join(errWarmControlRejected, err)
		}
		identity := resolveGitIdentity(e.repo, actor)
		bindRunnerRepositories(inv, run, identity.name, identity.email)
		creds := e.WorkspaceTerminalGitCredentials(context.Background(), run, actor)
		inv.GitHTTPUserID, inv.GitSSHUserID = 0, 0
		inv.GitHTTPCredentials, inv.GitSSHKeys = creds.HTTPS, creds.SSHKeys
		if req.Operation == domain.RunnerWorkspaceOperationWarmReleaseCheck {
			req.ArtifactFiles = archivedWorkspaceFiles(run, "")
		} else if req.Operation == domain.RunnerWorkspaceOperationWarmCheckout {
			refs, err := e.repo.WarmWorkspaceRefs(run.ID)
			if err != nil {
				return req, err
			}
			inv.WarmWorkspaceRefs = refs
			inv.ArchivedFiles, err = warmRestoreFiles(run, refs)
			if err != nil {
				return req, err
			}
		}
		req.PreparationInvocation = inv
	}
	if req.Operation == domain.RunnerWorkspaceOperationWarmPrepare {
		req.Repositories = run.RepositoryContexts()
	}
	return req, nil
}

// AuthorizeWarmWorkspaceControl rebuilds a socket-bound message at the last
// possible point, including messages relayed during a process handover.
func (e *Engine) AuthorizeWarmWorkspaceControl(runnerID string, req *domain.RunnerWorkspaceRequest) error {
	owner, ok := e.repo.RunState(req.RunID)
	if req.WarmWorkspaceLease == nil && !domain.IsWarmWorkspaceOperation(req.Operation) {
		if target := req.WorkspaceRunID; target != "" && (!ok || target != workspaceRunID(owner)) {
			_, _, found, err := e.repo.WarmWorkspaceAllocation(target)
			if err != nil {
				return err
			}
			if found {
				return errors.New("workspace target requires an observed warm lease")
			}
		}
		if req.RunID == "" {
			return nil
		}
		if !ok {
			_, _, found, err := e.repo.WarmWorkspaceAllocation(req.RunID)
			if err != nil {
				return err
			}
			if !found {
				return nil
			}
		} else {
			return e.verifyObservedWarmRequest(owner, req)
		}
	}
	if !ok || e.defaultExecutionRunner(owner) != runnerID {
		return errors.New("warm workspace owner or runner changed")
	}
	if !domain.IsWarmWorkspaceOperation(req.Operation) {
		if err := e.verifyObservedWarmRequest(owner, req); err != nil {
			return err
		}
		req.Repositories = owner.RepositoryContexts()
		if domain.IsWorkspaceGitRead(req.Operation, req.Ref) {
			spec, err := e.runnerSandboxSpec(owner)
			if err != nil {
				return err
			}
			req.Sandbox = spec
		}
		switch req.Operation {
		case domain.RunnerWorkspaceOperationPrepare, domain.RunnerWorkspaceOperationPreparationRecord, domain.RunnerWorkspaceOperationPreparationStatus, domain.RunnerWorkspaceOperationTerminal, domain.RunnerWorkspaceOperationBrowser:
			if req.Operation == domain.RunnerWorkspaceOperationTerminal && req.Terminal == nil {
				return errors.New("missing terminal operation")
			}
			canonical, err := e.preparedWorkspaceRequest(owner, *req)
			if err != nil {
				return err
			}
			*req = canonical
		}
		return nil
	}
	canonical, err := e.warmWorkspaceControlRequest(owner, *req)
	if err != nil {
		return err
	}
	*req = canonical
	return nil
}

func (e *Engine) handleWarmWorkspaceCheckpoint(w http.ResponseWriter, r *http.Request, run domain.Run, input domain.RunnerWorkspaceRequest) {
	req, err := e.warmWorkspaceControlRequest(run, input)
	if err != nil {
		http.Error(w, "invalid warm workspace reservation", http.StatusConflict)
		return
	}
	if e.WorkspaceCheckpointTransport == nil {
		http.Error(w, "runner transport unavailable", http.StatusServiceUnavailable)
		return
	}
	result, err := e.WorkspaceCheckpointTransport(r.Context(), run, req)
	if err != nil {
		result = domain.RunnerWorkspaceResponse{Status: 503, Error: err.Error()}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}
