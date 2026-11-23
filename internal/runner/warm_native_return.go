package runner

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
)

var errWarmReturnStale = errors.New("fixed warm checkout changed after return verification")

type nativeWarmReturnState struct {
	ID             string                     `json:"id,omitempty"`
	CleanupStarted bool                       `json:"cleanupStarted,omitempty"`
	Invalidated    bool                       `json:"invalidated,omitempty"`
	lfs            map[string]warmLFSManifest // An optimization for active receipts. A restarted Runner rebuilds the proof.
}

func (p *warmWorkspacePool) saveWarmReturnProof(a warmWorkspaceAllocation, refs []domain.WarmWorkspaceRef, remove []string, lfs map[string]warmLFSManifest) error {
	a.ReturnProven, a.ReturnRefs, a.ReturnRemove = true, refs, remove
	if p.native {
		a.NativeReturn = &nativeWarmReturnState{ID: rand.Text(), lfs: lfs}
	}
	return p.saveAllocation(a)
}

func nativeWarmReturnID(a warmWorkspaceAllocation) string {
	if a.NativeReturn != nil {
		return a.NativeReturn.ID
	}
	return ""
}

func (p *warmWorkspacePool) verifyNativeReturn(ctx context.Context, a warmWorkspaceAllocation) error {
	root := filepath.Join(p.workspaceRoot, "runs", a.Lease.OwnerRunID, "workspace")
	archived := map[string]string{}
	skip := map[string]bool{}
	for _, ref := range a.ReturnRefs {
		maps.Copy(archived, ref.Artifacts)
	}
	proof := warmReturnTree{root: root, native: true, archived: archived, collected: map[string]string{}}
	var lfsProofs map[string]warmLFSManifest
	if a.NativeReturn != nil {
		lfsProofs = a.NativeReturn.lfs
	}
	expected := a.PreparedRefs
	if a.GitReady {
		expected = make(map[string]domain.WarmWorkspaceRef, len(a.ReturnRefs))
		for _, ref := range a.ReturnRefs {
			expected[a.Lease.Slots[ref.RepositoryID]] = ref
		}
		if len(expected) != len(a.Lease.Slots) {
			return errors.New("incomplete native warm return proof")
		}
	}
	for id, ref := range expected {
		slot := p.slots[id]
		cache := p.gitCache(id)
		prefix, err := filepath.Rel(root, a.Targets[id])
		if err != nil || !validWarmWorkspaceRelativePath(prefix) {
			return errors.New("invalid native warm return destination")
		}
		cacheID, err := gitwork.C(ctx, cache, "config", "--get", "warm.repositoryID")
		if err != nil {
			return err
		}
		if cacheID != ref.RepositoryID || cacheID != slot.RepositoryID {
			return errors.New("native warm return cache does not match the leased repository")
		}
		origin, err := gitwork.C(ctx, cache, "remote", "get-url", "origin")
		if err != nil {
			return err
		}
		if domain.URLHasCredentials(origin) || gitwork.ValidateRemoteURL(origin, true) != nil {
			return errors.New("invalid native warm return cache origin")
		}
		r := domain.RunnerRepository{RepositoryID: cacheID, CloneURL: origin}
		proof.checkout, proof.artifactPrefix = slot.Path, prefix
		proof.lfsReceipt = nil
		if lfs, ok := lfsProofs[id]; ok {
			proof.lfsReceipt = &lfs
		}
		if err := proof.verifyLocal(ctx, slot, cache, r, ref, warmProofLFSCache); err != nil {
			return fmt.Errorf("%s: %w", prefix, err)
		}
		skip[a.Targets[id]] = true
	}
	if !a.GitReady {
		return nil
	}
	proof.checkout, proof.artifactPrefix = "", ""
	if err := proof.verifyExtra(ctx, skip); err != nil {
		return err
	}
	cleanupStarted := a.Phase == warmWorkspaceReturning || a.NativeReturn != nil && a.NativeReturn.CleanupStarted
	for _, rel := range a.ReturnRemove {
		if !validWarmWorkspaceRelativePath(rel) || archived[rel] == "" {
			return errors.New("invalid native warm return cleanup proof")
		}
		if proof.collected[rel] == archived[rel] {
			if a.Phase == warmWorkspaceReturning {
				return warmTreeChanged("archived file reappeared at %s", rel)
			}
			continue
		}
		if cleanupStarted {
			path := filepath.Join(root, rel)
			for id, target := range a.Targets {
				inner, err := filepath.Rel(target, path)
				if err == nil && validWarmWorkspaceRelativePath(inner) {
					path = filepath.Join(p.slots[id].Path, inner)
					break
				}
			}
			_, err := os.Lstat(path)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
		}
		return warmTreeChanged("archived file changed at %s", rel)
	}
	return nil
}

func (p *warmWorkspacePool) commitNativeReturn(ctx context.Context, l domain.WarmWorkspaceLease, id string, refs []domain.WarmWorkspaceRef, files []domain.WorkspaceArtifactFile) error {
	p.mu.Lock()
	a, err := p.loadLease(ctx, l)
	if err == nil && a.Phase != warmWorkspaceReleased {
		_, err = p.inspectLease(a)
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if id != nativeWarmReturnID(a) || !sameWarmReturnRefs(a.ReturnRefs, refs) {
		return errors.New("native warm return receipt does not match the journal")
	}
	if a.NativeReturn != nil && a.NativeReturn.Invalidated {
		return errWarmReturnStale
	}
	if !a.ReturnProven {
		return errors.New("missing durable acknowledgment of this return proof")
	}
	if a.Phase == warmWorkspaceReleased {
		return nil
	}
	if err := p.verifyNativeReturn(ctx, a); err != nil {
		if !errors.Is(err, errWarmTreeChanged) {
			return err
		}
		if invalidationErr := p.invalidateNativeReturn(ctx, a, files); invalidationErr != nil {
			return invalidationErr
		}
		return errors.Join(errWarmReturnStale, err)
	}
	p.mu.Lock()
	err = p.checkNativeWarmEditors(a)
	if err == nil && len(a.ReturnRemove) != 0 && (a.NativeReturn == nil || !a.NativeReturn.CleanupStarted) {
		state := nativeWarmReturnState{ID: id, CleanupStarted: true}
		if a.NativeReturn != nil {
			state.lfs = a.NativeReturn.lfs
		}
		a.NativeReturn = &state
		err = p.saveAllocation(a)
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	return p.commitReturn(ctx, l, refs)
}

func (p *warmWorkspacePool) invalidateNativeReturn(ctx context.Context, a warmWorkspaceAllocation, files []domain.WorkspaceArtifactFile) error {
	cleanupStarted := a.Phase == warmWorkspaceReturning || a.NativeReturn != nil && a.NativeReturn.CleanupStarted
	if a.Phase == warmWorkspaceReturning {
		a.Phase = warmWorkspacePlaced
		p.mu.Lock()
		err := p.bindNativeAllocation(ctx, a)
		p.mu.Unlock()
		if err != nil {
			return err
		}
	}
	if cleanupStarted {
		if err := p.restoreNativeReturnArchives(ctx, a, files); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	a.ReturnProven = false
	a.NativeReturn = &nativeWarmReturnState{ID: nativeWarmReturnID(a), Invalidated: true}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveAllocation(a)
}

func (p *warmWorkspacePool) restoreNativeReturnArchives(ctx context.Context, a warmWorkspaceAllocation, files []domain.WorkspaceArtifactFile) error {
	if len(a.ReturnRemove) == 0 {
		return nil
	}
	root := filepath.Join(p.workspaceRoot, "runs", a.Lease.OwnerRunID, "workspace")
	base, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer base.Close()
	archived := make(map[string]string)
	for _, ref := range a.ReturnRefs {
		maps.Copy(archived, ref.Artifacts)
	}
	byPath := make(map[string]domain.WorkspaceArtifactFile, len(files))
	for _, file := range files {
		byPath[file.Path] = file
	}
	var missing []domain.WorkspaceArtifactFile
	for _, path := range a.ReturnRemove {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !validWarmWorkspaceRelativePath(path) || archived[path] == "" {
			return errors.New("invalid native warm return cleanup path")
		}
		_, err := base.Lstat(path)
		if err == nil {
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		file := byPath[path]
		if file.SHA256 != archived[path] {
			return fmt.Errorf("missing archived bytes to restore %s", path)
		}
		missing = append(missing, file)
	}
	if _, err := runtime.RestoreMissingWorkspaceArtifacts(root, missing); err != nil {
		return err
	}
	for _, file := range missing {
		if _, err := base.Lstat(file.Path); err != nil {
			return fmt.Errorf("restore archived file %s: %w", file.Path, err)
		}
	}
	return nil
}

func (p *warmWorkspacePool) nativeReturnResponse(ctx context.Context, l domain.WarmWorkspaceLease, req domain.RunnerWorkspaceRequest) domain.RunnerWorkspaceResponse {
	id := req.WarmWorkspaceReturnID
	err := p.commitNativeReturn(ctx, l, id, req.WarmWorkspaceRefs, req.ArtifactFiles)
	resp := runnerWorkspacePayload(map[string]string{"phase": "released"})
	if err != nil {
		resp = runnerWorkspaceError(err)
		if errors.Is(err, errWarmReturnStale) {
			resp.Status, resp.Code = http.StatusConflict, domain.WorkspaceErrorWarmReturnStale
		}
	}
	resp.WarmWorkspaceReturnID = id
	return resp
}

func (s nativeWarmReturnState) valid() bool {
	return len(s.ID) <= 64 && strings.TrimSpace(s.ID) == s.ID && (!s.Invalidated || !s.CleanupStarted)
}
