package engine

import (
	"agentflow/internal/domain"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
)

func (e *Engine) authorizeWarmActor(run domain.Run, actor int64) error {
	p, ok := e.repo.Project(run.ExecutionContext.ProjectID)
	if actor <= 0 || !ok || p.ArchivedAt != nil {
		return errors.New("workspace repository access unavailable")
	}
	if p.OwnerID == actor || p.OwnerID == 0 && len(p.Members) == 0 || slices.ContainsFunc(p.Members, func(m domain.ProjectMember) bool { return m.UserID == actor }) {
		return nil
	}
	if user, ok := e.repo.User(actor); ok && e.repo.IsPlatformAdmin(user.LoginName) {
		return nil
	}
	return errors.New("workspace repository access revoked")
}

// RecordWarmWorkspaceActor is called after authenticated conversation repository
// use. Automatic return then resolves that user's current project and Git access.
func (e *Engine) RecordWarmWorkspaceActor(run domain.Run, l domain.WarmWorkspaceLease, actor int64) error {
	if !run.IsWorkspace() || run.ID != l.OwnerRunID {
		return errors.New("unexpected conversation workspace owner")
	}
	if err := e.authorizeWarmActor(run, actor); err != nil {
		return err
	}
	return e.repo.RecordWarmWorkspaceActor(l, actor)
}

func warmRestoreFiles(run domain.Run, refs []domain.WarmWorkspaceRef) ([]domain.WorkspaceArtifactFile, error) {
	want := map[string]string{}
	for _, r := range refs {
		for path, sha := range r.Artifacts {
			want[path] = sha
		}
	}
	var out []domain.WorkspaceArtifactFile
	for _, a := range run.Artifacts {
		path, ok := artifactRelPath(a.RepositoryDir, a.Path)
		sha := runtime.WorkspaceContentSHA(a.Content)
		if ok && want[path] == sha && (a.SHA256 == "" || a.SHA256 == sha) {
			out = append(out, domain.WorkspaceArtifactFile{Path: path, SHA256: sha, Content: a.Content})
			delete(want, path)
		}
	}
	for path := range want {
		return nil, fmt.Errorf("cannot recover recorded artifact %s", path)
	}
	return out, nil
}

func (e *Engine) ReleaseWarmWorkspace(ctx context.Context, run domain.Run, actorID int64) error {
	if sharesWorkspace(run) {
		return errors.New("only the workspace owner can release its lease")
	}
	current, ok := e.repo.RunState(run.ID)
	if !ok {
		return errors.New("workspace owner unavailable")
	}
	if current.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
		return nil
	}
	l, state, found, err := e.repo.WarmWorkspaceAllocation(run.ID)
	if err != nil || !found || state == domain.WarmWorkspaceReleased || state == domain.WarmWorkspaceCanceled {
		return err
	}
	token := ConversationWorkspaceToken(ctx, run.ID)
	handoff := token != "" && actorID > 0 && current.IsWorkspace()
	if token == "" {
		token, err = e.repo.BeginWorkspaceCheckpointSave(run.ID, time.Now())
		if err != nil {
			return err
		}
		defer e.repo.FinishWorkspaceUse(run.ID, token, time.Now(), nil, "")
	} else if err = e.repo.RenewWorkspaceUse(run.ID, token, time.Now()); err != nil {
		return err
	}
	current, ok = e.repo.RunState(run.ID)
	if !ok || !e.conversationWorkspaceIdle(current) || !handoff && e.conversationGenerationRunning(current) {
		return repo.ErrWorkspaceInUse
	}
	borrowed, err := e.repo.WarmWorkspaceBorrowed(current.ID, time.Now())
	if err != nil {
		return err
	}
	if borrowed || current.WorkspaceEditUntil.After(time.Now()) {
		return repo.ErrWorkspaceInUse
	}
	if state == domain.WarmWorkspaceQueued {
		return e.repo.CancelWarmWorkspaceEpoch(run.ID, l.Epoch, time.Now())
	}
	req := domain.RunnerWorkspaceRequest{RunID: run.ID, WarmWorkspaceLease: &l, Operation: domain.RunnerWorkspaceOperationWarmReleaseCheck}
	receipt, err := e.repo.HasWarmWorkspaceReturnReceipt(l)
	if err != nil {
		return err
	}
	if !receipt {
		if actorID == 0 {
			if current.IsWorkspace() {
				actorID, err = e.repo.WarmWorkspaceActor(l)
				if err != nil {
					return err
				}
			}
			if actorID == 0 {
				actorID = current.ExecutionContext.ResponsibleUserID
			}
		}
		if err := e.authorizeWarmActor(current, actorID); err != nil {
			return err
		}
		if err := e.repo.RecordWarmWorkspaceActor(l, actorID); err != nil {
			return err
		}
		if state != domain.WarmWorkspaceReleasing {
			if err = e.repo.SettleWarmWorkspace(l, domain.WarmWorkspaceReleasing); err != nil {
				return err
			}
		}
		proof, err := e.requestWorkspaceCheckpoint(ctx, current, req)
		if err != nil {
			return err
		}
		if proof.Error != "" || proof.Status >= 400 {
			err := fmt.Errorf("workspace retained: %s", proof.Error)
			if proof.Code == "warm_owner_retained" || proof.Code == domain.WorkspaceErrorBusy || proof.Code == domain.WorkspaceErrorTerminalActive {
				return errors.Join(err, e.repo.RetainWarmWorkspaceOwner(l))
			}
			return err
		}
		var refs []domain.WarmWorkspaceRef
		if err = json.Unmarshal(proof.Payload, &refs); err != nil {
			return err
		}
		if _, err = warmRestoreFiles(current, refs); err != nil {
			return err
		}
		if err = e.repo.RecordWarmWorkspaceRefs(run.ID, l, refs); err != nil {
			return err
		}
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmReleaseCommit
	ack, err := e.requestWorkspaceCheckpoint(ctx, current, req)
	if err != nil {
		return err
	}
	var result struct{ Phase string }
	if ack.Status >= 400 || ack.Error != "" || json.Unmarshal(ack.Payload, &result) != nil || result.Phase != "released" {
		return fmt.Errorf("workspace return pending: %s", ack.Error)
	}
	return e.repo.ConfirmWarmWorkspaceBinding(l, domain.WarmWorkspaceReleased)
}

func (e *Engine) cleanupWarmWorkspace(run domain.Run, now time.Time) {
	if sharesWorkspace(run) || !e.conversationWorkspaceIdle(run) {
		return
	}
	state, err := e.WarmWorkspaceStatus(run.ID)
	if err != nil {
		return
	}
	// Stop may have cancelled a reservation already placed on disk. Finish
	// that exact return immediately, including when nobody else is waiting.
	if run.IsWorkspace() && state != domain.WarmWorkspaceReleasing {
		meta, ok := e.repo.WorkspaceCheckpoint(run.ID)
		last := run.UpdatedAt
		if meta.LastUsedAt.After(last) {
			last = meta.LastUsedAt
		}
		if !ok || !meta.Released() {
			if now.Before(last.Add(ConversationWorkspaceIdleTTL)) {
				waiting, err := e.repo.WarmWorkspaceHasWaiter(e.defaultExecutionRunner(run), run.ExecutionContext.ProjectID, run.RepositoryContexts())
				if err != nil || !waiting {
					return
				}
			}
		}
	}
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Minute)
	defer stop()
	if err := e.ReleaseWarmWorkspace(ctx, run, 0); err != nil && !errors.Is(err, repo.ErrWorkspaceInUse) {
		// Keep the diagnostic visible to the owner and avoid duplicate events
		// from recurring housekeeping.
		if n := len(run.Events); n == 0 || run.Events[n-1].Type != "workspace.pool_retained" || run.Events[n-1].Message != err.Error() {
			e.updateRun(run.ID, func(r *domain.Run) { e.event(r, "", "workspace.pool_retained", err.Error()) })
		}
	}
}

var errWarmAccessUnavailable = errors.New("暂时无法连接执行机器验证仓库权限")

type workspaceRestoreActorKey struct{}

func WithWorkspaceRestoreActor(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, workspaceRestoreActorKey{}, id)
}
func workspaceRestoreActor(ctx context.Context) int64 {
	id, _ := ctx.Value(workspaceRestoreActorKey{}).(int64)
	return id
}
func (e *Engine) CheckWorkspaceRestoreAccess(ctx context.Context, run domain.Run) error {
	owner, err := e.warmWorkspaceOwner(run)
	if err != nil {
		return err
	}
	if owner.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
		return nil
	}
	actor := workspaceRestoreActor(ctx)
	if err := e.authorizeWarmActor(owner, actor); err != nil {
		return err
	}
	l, state, found, err := e.repo.WarmWorkspaceAllocation(owner.ID)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("warm workspace has no allocation to restore")
	}
	resp, err := e.requestWorkspaceCheckpoint(ctx, owner, domain.RunnerWorkspaceRequest{Operation: domain.RunnerWorkspaceOperationWarmVerifyAccess, RunID: owner.ID, WarmWorkspaceLease: &l, WarmWorkspaceActorID: actor})
	if err != nil {
		if errors.Is(err, ErrWorkspaceControlConfiguration) {
			return err
		}
		return errors.Join(errWarmAccessUnavailable, err)
	}
	if resp.Status == 429 || resp.Status == 503 || resp.Code == domain.WorkspaceErrorBusy {
		return errWarmAccessUnavailable
	}

	var result struct{ Phase string }
	if resp.Error != "" || resp.Status >= 400 || json.Unmarshal(resp.Payload, &result) != nil || result.Phase != "authorized" {
		return errors.New("current actor cannot verify access to the workspace repositories")
	}
	if owner.IsWorkspace() && state == domain.WarmWorkspaceReady {
		return e.RecordWarmWorkspaceActor(owner, l, actor)
	}
	return nil
}

// First SOP execution checks its frozen remotes during normal admission. An
// exact restore or an owner which already has a lease must use its own proof.
func (e *Engine) WarmWorkspaceFirstActivation(ctx context.Context, run domain.Run) (bool, error) {
	if run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool || run.IsWorkspace() || sharesWorkspace(run) || !run.WorkspaceCleanedAt.IsZero() {
		return false, nil
	}
	if err := e.authorizeWarmActor(run, workspaceRestoreActor(ctx)); err != nil {
		return false, err
	}
	_, _, found, err := e.repo.WarmWorkspaceAllocation(run.ID)
	if err != nil || found {
		return false, err
	}
	refs, err := e.repo.WarmWorkspaceRefs(run.ID)
	return err == nil && len(refs) == 0, err
}
