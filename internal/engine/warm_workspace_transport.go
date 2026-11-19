package engine

import (
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
	req = domain.RunnerWorkspaceRequest{RunID: run.ID, WorkspaceRunID: run.ID, Operation: input.Operation, WarmWorkspaceLease: input.WarmWorkspaceLease}
	if err := e.repo.AuthorizeWarmWorkspaceControl(runnerID, run, &req); err != nil {
		return req, err
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
	if !ok || e.defaultExecutionRunner(owner) != runnerID {
		return errors.New("warm workspace owner or runner changed")
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
