package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

var errNativeWarmCheckoutBusy = errors.New("fixed warm checkout is temporarily busy")

type nativeWarmCheckoutIntent struct {
	SlotID       string                  `json:"slotId"`
	Before       domain.WarmWorkspaceRef `json:"before"`
	Target       domain.WarmWorkspaceRef `json:"target"`
	BranchBefore string                  `json:"branchBefore,omitempty"`
	Baseline     string                  `json:"baseline"`
	Rewind       bool                    `json:"rewind,omitempty"`
	LFSStageID   string                  `json:"lfsStageId,omitempty"`
}

func (i nativeWarmCheckoutIntent) valid() bool {
	validBranch := func(branch string) bool {
		return branch == "" || validWarmWorkspaceRelativePath(branch) && !strings.HasPrefix(branch, "-")
	}
	return i.Before.RepositoryID == i.Target.RepositoryID && validBranch(i.Before.Branch) && validBranch(i.Target.Branch) &&
		warmCommitID.MatchString(i.Before.Commit) && warmCommitID.MatchString(i.Target.Commit) &&
		warmCommitID.MatchString(i.Baseline) && (i.BranchBefore == "" || warmCommitID.MatchString(i.BranchBefore)) &&
		(!i.Rewind || i.Target.Branch != "" && i.BranchBefore != "") &&
		(i.LFSStageID == "" || validNativeLFSStageID(i.LFSStageID))
}

func nativeWarmBranchTip(ctx context.Context, checkout, branch string) (string, error) {
	if branch == "" {
		return "", nil
	}
	name := "refs/heads/" + branch
	out, err := nativeWarmGit(ctx, checkout, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads")
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		ref, tip, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		if ref == name {
			return tip, nil
		}
		if strings.HasPrefix(ref, name+"/") || strings.HasPrefix(name, ref+"/") {
			return "", fmt.Errorf("fixed warm checkout branch %s conflicts with local branch %s", branch, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	return "", nil
}

func checkNativeWarmRewind(ctx context.Context, cache string, intent nativeWarmCheckoutIntent) error {
	tip, ok := gitwork.RevParse(ctx, cache, "refs/remotes/origin/"+intent.Target.Branch)
	if !ok {
		return fmt.Errorf("fixed warm checkout cannot restore %s without a published branch tip", intent.Target.Branch)
	}
	for _, pair := range [][2]string{{intent.Target.Commit, intent.BranchBefore}, {intent.BranchBefore, tip}} {
		if _, err := gitwork.C(ctx, cache, "merge-base", "--is-ancestor", pair[0], pair[1]); err != nil {
			return fmt.Errorf("fixed warm checkout has unpublished or divergent commits on %s", intent.Target.Branch)
		}
	}
	return nil
}

func nativeWarmBaseline(ctx context.Context, cache string, repo domain.RunnerRepository, restore *domain.WarmWorkspaceRef, target string) string {
	if restore == nil && repo.WorkBranch != "" && repo.WorkBranch != repo.Ref && repo.Ref != "" {
		if base, ok := gitwork.RevParse(ctx, cache, "refs/remotes/origin/"+repo.Ref); ok {
			return base
		}
	}
	return target
}

func inspectNativeWarmCheckout(ctx context.Context, checkout, cache string, slot warmWorkspaceSlot, repo domain.RunnerRepository, intent nativeWarmCheckoutIntent) (bool, error) {
	if err := verifyNativeWarmConfig(ctx, checkout, filepath.Dir(cache)); err != nil {
		return false, err
	}
	current, err := warmCheckoutRef(checkout, repo.RepositoryID)
	if err != nil {
		return false, err
	}
	tip, err := nativeWarmBranchTip(ctx, checkout, intent.Target.Branch)
	if err != nil {
		return false, err
	}
	proof := warmReturnTree{root: checkout, checkout: checkout, native: true}
	if current.Commit == intent.Before.Commit && current.Branch == intent.Before.Branch && tip == intent.BranchBefore {
		return false, proof.verifyLocal(ctx, slot, cache, repo, intent.Before, warmProofLFSContent)
	}
	if current.Commit == intent.Target.Commit &&
		(current.Branch == intent.Target.Branch && (current.Branch == "" || tip == current.Commit) ||
			current.Branch == "" && intent.Target.Branch != "" && (tip == intent.BranchBefore || tip == current.Commit)) {
		return true, proof.verifyLocal(ctx, slot, cache, repo, current, warmProofLFSCache)
	}
	return false, warmTreeChanged("interrupted warm checkout references differ from the journal")
}

func applyNativeWarmCheckout(ctx context.Context, checkout string, slot warmWorkspaceSlot, intent nativeWarmCheckoutIntent, atTarget bool, lfs warmLFSManifest) error {
	if slot.Path != "" && nativeWarmEditorBusy(slot) {
		return fmt.Errorf("%w: close Unity before changing the leased checkout", errNativeWarmCheckoutBusy)
	}
	if len(lfs.Files) > 0 {
		if _, err := gitwork.C(ctx, checkout, "lfs", "install", "--local"); err != nil {
			return fmt.Errorf("prepare fixed checkout LFS filters: %w", err)
		}
	}
	ref := intent.Target
	checkoutArgs := []string{"checkout", "--no-overwrite-ignore", "--no-recurse-submodules"}
	if !atTarget {
		if _, err := nativeWarmGit(ctx, checkout, append(checkoutArgs, "--detach", ref.Commit)...); err != nil {
			return nativeWarmGitWriteError(ctx, err)
		}
	}
	if ref.Branch != "" {
		name := "refs/heads/" + ref.Branch
		tip, err := nativeWarmBranchTip(ctx, checkout, ref.Branch)
		if err != nil {
			return err
		}
		if tip != ref.Commit {
			if tip != intent.BranchBefore {
				return warmTreeChanged("fixed warm branch %s changed during checkout", ref.Branch)
			}
			if tip == "" {
				tip = strings.Repeat("0", len(ref.Commit))
			}
			if _, err := nativeWarmGit(ctx, checkout, "update-ref", "--create-reflog", "-m", "LoopForge warm checkout", name, ref.Commit, tip); err != nil {
				return nativeWarmGitWriteError(ctx, err)
			}
		}
		current, err := warmCheckoutRef(checkout, ref.RepositoryID)
		if err != nil {
			return err
		}
		if current.Branch != ref.Branch {
			if _, err := nativeWarmGit(ctx, checkout, append(checkoutArgs, ref.Branch, "--")...); err != nil {
				return nativeWarmGitWriteError(ctx, err)
			}
		}
	}
	current, err := warmCheckoutRef(checkout, ref.RepositoryID)
	if err != nil {
		return err
	}
	if current.Commit != ref.Commit || current.Branch != ref.Branch {
		return warmTreeChanged("fixed warm checkout changed while attaching the branch")
	}
	if err := hydrateNativeWarmLFS(checkout, intent, lfs); err != nil {
		return fmt.Errorf("materialize fixed checkout Git LFS files: %w", err)
	}
	return writeNativeWarmBaseline(checkout, intent.Baseline)
}

func nativeWarmGitWriteError(ctx context.Context, err error) error {
	var exit *exec.ExitError
	if ctx.Err() != nil || errors.As(err, &exit) && exit.ExitCode() < 0 {
		return errors.Join(errNativeWarmCheckoutBusy, err)
	}
	return err
}

func validNativeLFSStageID(id string) bool {
	return len(id) == 26 && strings.IndexFunc(id, func(r rune) bool {
		return (r < 'A' || r > 'Z') && (r < '2' || r > '7')
	}) == -1
}

func nativeWarmLFSStage(meta, id string) string {
	return filepath.Join(meta, "agentflow-warm-lfs-"+id)
}

func hydrateNativeWarmLFS(checkout string, intent nativeWarmCheckoutIntent, manifest warmLFSManifest) error {
	if len(manifest.Files) == 0 {
		return nil
	}
	meta, err := openWarmWorkspaceDir(filepath.Join(checkout, ".git"), false)
	if err != nil {
		return err
	}
	defer meta.Close()
	stage := meta.Name()
	if validNativeLFSStageID(intent.LFSStageID) {
		stage = nativeWarmLFSStage(stage, intent.LFSStageID)
		if err := os.RemoveAll(stage); err != nil {
			return err
		}
		if err := os.Mkdir(stage, 0700); err != nil {
			return err
		}
		defer os.RemoveAll(stage)
	}
	// The temporary file must be on the Windows filesystem for atomic
	// publication. A stopped copy is invisible to later worktree proofs.
	return manifest.hydrate(checkout, stage)
}

func writeNativeWarmBaseline(checkout, commit string) error {
	meta, err := openWarmWorkspaceDir(filepath.Join(checkout, ".git"), false)
	if err != nil {
		return err
	}
	defer meta.Close()
	path := gitwork.BaselinePath(checkout)
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return errors.New("fixed warm checkout baseline is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !warmCommitID.MatchString(commit) {
		return errors.New("invalid fixed warm checkout baseline")
	}
	tmp, err := os.CreateTemp(meta.Name(), ".agentflow-baseline-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, writeErr := tmp.WriteString(commit)
	if err := errors.Join(writeErr, tmp.Sync(), tmp.Close()); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return meta.Sync()
}

func (p *warmWorkspacePool) recoverNativeCheckoutForReturn(ctx context.Context, a warmWorkspaceAllocation, repos []domain.RunnerRepository) (warmWorkspaceAllocation, error) {
	intent := a.NativeCheckout
	if intent == nil {
		return a, nil
	}
	slot := p.slots[intent.SlotID]
	for _, repo := range repos {
		if repo.RepositoryID != slot.RepositoryID {
			continue
		}
		target := a.Targets[slot.ID]
		cache := p.gitCache(slot.ID)
		if nativeWarmEditorBusy(slot) {
			return a, fmt.Errorf("%w: close Unity before recovering the checkout", errNativeWarmCheckoutBusy)
		}
		atTarget, err := inspectNativeWarmCheckout(ctx, target, cache, slot, repo, *intent)
		if err != nil {
			return a, err
		}
		if !atTarget && validNativeLFSStageID(intent.LFSStageID) {
			meta, err := openWarmWorkspaceDir(filepath.Join(target, ".git"), false)
			if err != nil {
				return a, err
			}
			err = os.RemoveAll(nativeWarmLFSStage(meta.Name(), intent.LFSStageID))
			meta.Close()
			if err != nil {
				return a, err
			}
		}
		if atTarget {
			prepared, err := prepareNativeWarmCheckout(ctx, target, cache, slot, repo, nil, intent, nil)
			if err != nil {
				return a, err
			}
			a.PreparedRefs = maps.Clone(a.PreparedRefs)
			if a.PreparedRefs == nil {
				a.PreparedRefs = make(map[string]domain.WarmWorkspaceRef)
			}
			a.PreparedRefs[slot.ID] = prepared
		}
		a.PreparingGit, a.NativeCheckout = false, nil
		p.mu.Lock()
		err = p.saveAllocation(a)
		p.mu.Unlock()
		return a, err
	}
	return a, errors.New("missing repository for interrupted native checkout")
}

func (p *warmWorkspacePool) checkNativeLeaseBeforeReady(ctx context.Context, a warmWorkspaceAllocation, root string, inv *domain.RunnerInvocation) error {
	if err := p.checkNativeWarmEditors(a); err != nil {
		return errors.Join(errNativeWarmCheckoutBusy, err)
	}
	archived := make(map[string]string, len(inv.ArchivedFiles))
	for _, file := range inv.ArchivedFiles {
		archived[file.Path] = file.SHA256
	}
	for _, repo := range inv.Repositories {
		id := a.Lease.Slots[repo.RepositoryID]
		ref, ok := a.PreparedRefs[id]
		if !ok {
			return errors.New("missing prepared native warm workspace reference")
		}
		proof := warmReturnTree{root: root, checkout: a.Targets[id], native: true, archived: archived, collected: make(map[string]string)}
		if err := proof.verifyLocal(ctx, p.slots[id], p.gitCache(id), repo, ref, warmProofLFSCache); err != nil {
			return fmt.Errorf("%s: verify prepared native checkout: %w", repo.Dir, err)
		}
	}
	return nil
}
