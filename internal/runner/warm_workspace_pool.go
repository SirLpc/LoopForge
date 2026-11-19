package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"agentflow/internal/domain"
	"golang.org/x/sys/unix"
)

type warmWorkspaceSlot struct {
	ID               string   `json:"id"`
	ProjectID        string   `json:"projectId"`
	RepositoryID     string   `json:"repositoryId"`
	Path             string   `json:"path"`
	UnityProjectPath string   `json:"unityProjectPath"`
	DiscardGenerated []string `json:"discardGenerated,omitempty"`
}

type warmWorkspacePool struct {
	mu                 sync.Mutex
	idleOrigins        map[string]string
	slots              map[string]warmWorkspaceSlot
	workspaceRoot      string
	gitRoot            string
	journalDir         *os.File
	journalName        string
	journalSyncPending bool
	lock               *os.File
	state              warmWorkspaceJournal
}

func openWarmWorkspacePool(configPath, workspaceRoot, statePath string) (*warmWorkspacePool, error) {
	configPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, err
	}
	workspaceRoot, err = filepath.Abs(workspaceRoot)
	if err != nil {
		return nil, err
	}
	statePath, err = filepath.Abs(statePath)
	if err != nil {
		return nil, err
	}
	if configPath == statePath || configPath == statePath+".lock" {
		return nil, errors.New("warm workspace config overlaps its journal")
	}
	slots, err := readWarmWorkspaceSlots(configPath, workspaceRoot, filepath.Dir(statePath))
	if err != nil {
		return nil, err
	}
	workspace, err := openWarmWorkspaceDir(workspaceRoot, true)
	if err != nil {
		return nil, fmt.Errorf("open warm workspace root: %w", err)
	}
	defer workspace.Close()
	workspaceID, err := warmWorkspaceDirIdentity(workspace)
	if err != nil {
		return nil, err
	}
	pool := &warmWorkspacePool{slots: slots, workspaceRoot: workspaceRoot, journalName: filepath.Base(statePath)}
	if err := pool.openJournal(filepath.Dir(statePath), workspaceID); err != nil {
		_ = pool.close()
		return nil, err
	}
	journalID, err := warmWorkspaceDirIdentity(pool.journalDir)
	if err != nil {
		pool.close()
		return nil, err
	}
	cacheRoot := filepath.Join(pool.journalDir.Name(), pool.journalName+".git")
	if warmWorkspacePathsOverlap(cacheRoot, workspaceRoot) {
		pool.close()
		return nil, errors.New("warm Git cache overlaps the runner workspace root")
	}
	if journalID.Dev != workspaceID.Dev {
		// Atomic metadata exchanges require the slot's filesystem. Keep this
		// runner-only root outside all run directories, covered by the same
		// native sandbox masks as a cache next to the journal.
		digest := sha256.Sum256([]byte(statePath))
		cacheRoot = filepath.Join(workspaceRoot, fmt.Sprintf(".warm-git-private-%x", digest))
	}
	pool.gitRoot = cacheRoot
	cacheDir, err := openWarmWorkspaceDir(cacheRoot, true)
	if err != nil {
		pool.close()
		return nil, err
	}
	cacheDir.Close()
	return pool, nil
}

func readWarmWorkspaceSlots(configPath, workspaceRoot, stateDir string) (map[string]warmWorkspaceSlot, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var config struct {
		Slots []warmWorkspaceSlot `json:"slots"`
	}
	if err := decodeWarmWorkspaceJSON(file, &config); err != nil {
		return nil, fmt.Errorf("read warm workspace pool config: %w", err)
	}
	if len(config.Slots) == 0 {
		return nil, errors.New("warm workspace pool configuration has no slots")
	}
	slots := make(map[string]warmWorkspaceSlot, len(config.Slots))
	for _, slot := range config.Slots {
		if strings.TrimSpace(slot.ID) == "" || strings.TrimSpace(slot.ProjectID) == "" || strings.TrimSpace(slot.RepositoryID) == "" {
			return nil, errors.New("warm workspace slot requires id, projectId and repositoryId")
		}
		if _, exists := slots[slot.ID]; exists {
			return nil, fmt.Errorf("duplicate warm workspace slot ID %q", slot.ID)
		}
		if !filepath.IsAbs(slot.Path) || filepath.Clean(slot.Path) != slot.Path ||
			warmWorkspacePathsOverlap(slot.Path, workspaceRoot) || warmWorkspacePathsOverlap(slot.Path, stateDir) ||
			warmWorkspacePathsOverlap(slot.Path, configPath) {
			return nil, fmt.Errorf("invalid or overlapping path for warm workspace slot %q: %s", slot.ID, slot.Path)
		}
		if slot.UnityProjectPath != "." && !validWarmWorkspaceRelativePath(slot.UnityProjectPath) {
			return nil, fmt.Errorf("invalid Unity project path for warm workspace slot %q", slot.ID)
		}
		for _, dir := range slot.DiscardGenerated {
			if dir != "Temp" && dir != "Logs" && dir != "obj" {
				return nil, fmt.Errorf("invalid generated directory %q for warm workspace slot %q", dir, slot.ID)
			}
		}
		for _, prior := range slots {
			if warmWorkspacePathsOverlap(prior.Path, slot.Path) {
				return nil, fmt.Errorf("overlapping paths for warm workspace slots %q and %q", prior.ID, slot.ID)
			}
		}
		slots[slot.ID] = slot
	}
	return slots, nil
}

func validWarmWorkspaceRelativePath(path string) bool {
	return path != "" && path != "." && path != ".." && filepath.IsLocal(path) && filepath.Clean(path) == path
}

func warmWorkspacePathsOverlap(a, b string) bool {
	contains := func(root, child string) bool {
		relative, err := filepath.Rel(root, child)
		return err == nil && filepath.IsLocal(relative)
	}
	return contains(a, b) || contains(b, a)
}

func (p *warmWorkspacePool) ownerTargets(lease domain.WarmWorkspaceLease, repoDirs map[string]string) (map[string]string, error) {
	if len(repoDirs) != len(lease.Slots) {
		return nil, errors.New("warm workspace placement requires one directory per leased repository")
	}
	root := filepath.Join(p.workspaceRoot, "runs", lease.OwnerRunID, "workspace")
	targets := make(map[string]string, len(lease.Slots))
	for repository, slot := range lease.Slots {
		target := repoDirs[repository]
		if !filepath.IsAbs(target) {
			if !validWarmWorkspaceRelativePath(target) {
				return nil, fmt.Errorf("invalid warm workspace destination for repository %q", repository)
			}
			target = filepath.Join(root, target)
		}
		relative, err := filepath.Rel(root, target)
		if err != nil || !validWarmWorkspaceRelativePath(relative) || filepath.Clean(target) != target {
			return nil, fmt.Errorf("invalid warm workspace destination for repository %q", repository)
		}
		for _, other := range targets {
			if warmWorkspacePathsOverlap(target, other) {
				return nil, fmt.Errorf("overlapping warm workspace destination for repository %q", repository)
			}
		}
		targets[slot] = target
	}
	return targets, nil
}

func (p *warmWorkspacePool) validateLease(lease domain.WarmWorkspaceLease) error {
	if lease.OwnerRunID == "" || lease.OwnerRunID == "." || lease.OwnerRunID == ".." ||
		safeWorkspaceComponent(lease.OwnerRunID) != lease.OwnerRunID || strings.TrimSpace(lease.RunnerID) == "" ||
		lease.Generation == 0 || len(lease.Slots) == 0 {
		return errors.New("invalid warm workspace lease")
	}
	var project string
	for repository, id := range lease.Slots {
		slot, ok := p.slots[id]
		if !ok || slot.RepositoryID != repository {
			return fmt.Errorf("warm workspace slot %q does not belong to repository %q", id, repository)
		}
		if project != "" && project != slot.ProjectID {
			return errors.New("warm workspace lease contains slots from different projects")
		}
		project = slot.ProjectID
	}
	return nil
}

func sameWarmWorkspaceLease(a, b domain.WarmWorkspaceLease) bool {
	return a.Epoch == b.Epoch && a.OwnerRunID == b.OwnerRunID && a.RunnerID == b.RunnerID &&
		a.Generation == b.Generation && maps.Equal(a.Slots, b.Slots)
}

func (p *warmWorkspacePool) reserve(lease domain.WarmWorkspaceLease) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.ready(); err != nil {
		return err
	}
	if err := p.validateLease(lease); err != nil {
		return err
	}
	if current, exists := p.state.Leases[lease.OwnerRunID]; exists {
		if sameWarmWorkspaceLease(current.Lease, lease) && (current.Phase == warmWorkspaceReserved ||
			current.Phase == warmWorkspacePlacing || current.Phase == warmWorkspacePlaced) {
			_, err := p.inspectLease(current)
			return err
		}
		if current.Phase != warmWorkspaceReleased || current.Lease.Generation >= lease.Generation {
			return fmt.Errorf("warm workspace owner %q has a newer or active lease", lease.OwnerRunID)
		}
	}

	active := p.state.activeSlots()
	next := p.state.clone()
	for _, id := range lease.Slots {
		record := p.state.Slots[id]
		if record.NeedsInspection || active[id] || record.Generation >= lease.Generation {
			return fmt.Errorf("warm workspace slot %q is unavailable for generation %d", id, lease.Generation)
		}
		record.OriginSHA256 = p.idleOrigins[id]
		record.Generation = lease.Generation
		next.Slots[id] = record
	}
	// Released leases make a retry idempotent until one of their slots is
	// leased again. Each slot independently retains its generation fence.
	for owner, old := range next.Leases {
		if old.Phase != warmWorkspaceReleased {
			continue
		}
		for repository, id := range old.Lease.Slots {
			if lease.Slots[repository] == id {
				// Earlier journals kept the previous proof only on the released
				// group. Carry it forward for each still-matching slot.
				for _, ref := range old.ReturnRefs {
					previousID := old.Lease.Slots[ref.RepositoryID]
					slot := next.Slots[previousID]
					if slot.ReturnedRef == nil && p.state.Slots[previousID].Generation == old.Lease.Generation {
						slot.ReturnedRef = &domain.WarmWorkspaceRef{RepositoryID: ref.RepositoryID, Branch: ref.Branch, Commit: ref.Commit}
						next.Slots[previousID] = slot
					}
				}
				delete(next.Leases, owner)
				break
			}
		}
	}
	lease.Slots = maps.Clone(lease.Slots)
	allocation := warmWorkspaceAllocation{Lease: lease, Phase: warmWorkspaceReserved}
	next.Leases[lease.OwnerRunID] = allocation
	if err := p.save(next); err != nil {
		return err
	}
	_, err := p.inspectLease(allocation)
	return err
}

func (p *warmWorkspacePool) place(ctx context.Context, lease domain.WarmWorkspaceLease, repoDirs map[string]string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	allocation, err := p.loadLease(ctx, lease)
	if err != nil {
		return err
	}
	if allocation.Phase != warmWorkspaceReserved && allocation.Phase != warmWorkspacePlacing && allocation.Phase != warmWorkspacePlaced {
		return errors.New("warm workspace lease cannot be placed")
	}
	targets, err := p.ownerTargets(lease, repoDirs)
	if err != nil {
		return err
	}
	if allocation.Phase == warmWorkspaceReserved {
		if _, err := p.inspectLease(allocation); err != nil {
			return err
		}
		allocation.Targets = targets
		allocation.Phase = warmWorkspacePlacing
		if err := p.saveAllocation(allocation); err != nil {
			return err
		}
	} else if !maps.Equal(allocation.Targets, targets) {
		return errors.New("warm workspace placement differs from the journal")
	}
	if allocation.Phase == warmWorkspacePlaced {
		_, err := p.inspectLease(allocation)
		return err
	}
	return p.move(ctx, allocation)
}

// returnSlots commits a return that has already been authorized by the
// workspace lifecycle while holding the owner gate and process fence.
func (p *warmWorkspacePool) returnSlots(ctx context.Context, lease domain.WarmWorkspaceLease) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	allocation, err := p.loadLease(ctx, lease)
	if err != nil {
		return err
	}
	if allocation.Phase == warmWorkspaceReleased {
		return nil
	}
	if allocation.Phase != warmWorkspaceReturning {
		if _, err := p.inspectLease(allocation); err != nil {
			return err
		}
		allocation.Phase = warmWorkspaceReturning
		if err := p.saveAllocation(allocation); err != nil {
			return err
		}
	}
	return p.move(ctx, allocation)
}

func (p *warmWorkspacePool) loadLease(ctx context.Context, lease domain.WarmWorkspaceLease) (warmWorkspaceAllocation, error) {
	if err := ctx.Err(); err != nil {
		return warmWorkspaceAllocation{}, err
	}
	if err := p.ready(); err != nil {
		return warmWorkspaceAllocation{}, err
	}
	allocation, exists := p.state.Leases[lease.OwnerRunID]
	if !exists || !sameWarmWorkspaceLease(allocation.Lease, lease) {
		return warmWorkspaceAllocation{}, errors.New("unknown or stale warm workspace lease")
	}
	return allocation, nil
}

// inspectLease validates the entire assignment before any move. An unrelated
// entry in the way is retryable when the slot's own inode is accounted for.
func (p *warmWorkspacePool) inspectLease(allocation warmWorkspaceAllocation) (map[string]warmWorkspacePosition, error) {
	positions := make(map[string]warmWorkspacePosition, len(allocation.Lease.Slots))
	next := p.state.clone()
	var inspectionErr error
	for _, id := range allocation.Lease.Slots {
		record := p.state.Slots[id]
		position := p.inspectSlot(id, allocation.Targets[id])
		positions[id] = position
		known := position.atHome != position.atTarget
		if allocation.Phase == warmWorkspaceReserved || allocation.Phase == warmWorkspaceReleased {
			known = position.atHome
		} else if allocation.Phase == warmWorkspacePlaced {
			known = position.atTarget && !position.atHome
		}
		if known {
			checkout := allocation.Targets[id]
			if position.atHome {
				checkout = p.slots[id].Path
			}
			known = p.checkUnityProject(p.slots[id], checkout) == nil
		}
		if !known || record.NeedsInspection {
			inspectionErr = errors.Join(inspectionErr, fmt.Errorf("warm workspace slot %q needs inspection", id))
			record.NeedsInspection = true
			next.Slots[id] = record
		}
	}
	if !maps.Equal(next.Slots, p.state.Slots) {
		if err := p.save(next); err != nil {
			return nil, err
		}
	}
	return positions, inspectionErr
}

func (p *warmWorkspacePool) move(ctx context.Context, allocation warmWorkspaceAllocation) error {
	positions, err := p.inspectLease(allocation)
	if err != nil {
		return err
	}
	returning := allocation.Phase == warmWorkspaceReturning
	ids := slices.Sorted(maps.Keys(positions))
	for _, id := range ids {
		position := positions[id]
		if returning && position.atHome {
			continue
		}
		if position.homeErr != nil || position.targetErr != nil {
			return fmt.Errorf("inspect warm workspace locations for slot %q: %w", id, errors.Join(position.homeErr, position.targetErr))
		}
		if position.homeExists && position.targetExists {
			return fmt.Errorf("warm workspace location for slot %q is occupied", id)
		}
		if !returning && !position.atTarget {
			parent, err := openWarmWorkspaceDir(filepath.Dir(allocation.Targets[id]), true)
			if err != nil {
				return err
			}
			identity, err := warmWorkspaceDirIdentity(parent)
			parent.Close()
			if err != nil {
				return err
			}
			if identity.Dev != p.state.Workspace.Dev {
				return fmt.Errorf("warm workspace destination for slot %q is on another filesystem", id)
			}
		}
	}
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return err
		}
		position := positions[id]
		if (!returning && position.atTarget) || (returning && position.atHome) {
			continue
		}
		if err := moveWarmWorkspaceDir(p.slots[id].Path, allocation.Targets[id], p.state.Slots[id], returning); err != nil {
			return fmt.Errorf("move warm workspace slot %q: %w", id, err)
		}
	}
	return p.finishMove(allocation)
}

func (p *warmWorkspacePool) finishMove(allocation warmWorkspaceAllocation) error {
	positions, err := p.inspectLease(allocation)
	if err != nil {
		return err
	}
	if !warmWorkspaceMoveComplete(allocation, positions) {
		return errors.New("warm workspace move is incomplete")
	}
	returning := allocation.Phase == warmWorkspaceReturning
	// The rename may have happened immediately before an interruption. Re-sync
	// both parents before removing the write-ahead intent, even on recovery.
	for id, position := range positions {
		if err := syncWarmWorkspaceParent(p.slots[id].Path); err != nil {
			return err
		}
		// Cancellation can finish with a verified home inode even when a
		// blocked target ancestor prevents opening the workspace path.
		if returning && position.targetErr != nil {
			continue
		}
		if target := allocation.Targets[id]; target != "" {
			if err := syncWarmWorkspaceTargetParents(p.workspaceRoot, target, (*os.File).Sync); err != nil &&
				!(returning && errors.Is(err, os.ErrNotExist)) {
				return err
			}
		}
	}
	allocation.Phase = warmWorkspacePlaced
	if returning {
		allocation.Phase = warmWorkspaceReleased
		allocation.Targets = nil
		next := p.state.clone()
		for id, ref := range allocation.PreparedRefs {
			record := next.Slots[id]
			record.ReturnedRef = &ref
			next.Slots[id] = record
		}
		for _, ref := range allocation.ReturnRefs {
			id := allocation.Lease.Slots[ref.RepositoryID]
			record := next.Slots[id]
			record.ReturnedRef = &domain.WarmWorkspaceRef{RepositoryID: ref.RepositoryID, Branch: ref.Branch, Commit: ref.Commit}
			next.Slots[id] = record
		}
		next.Leases[allocation.Lease.OwnerRunID] = allocation
		return p.save(next)
	}
	return p.saveAllocation(allocation)
}

func warmWorkspaceMoveComplete(allocation warmWorkspaceAllocation, positions map[string]warmWorkspacePosition) bool {
	for _, position := range positions {
		if allocation.Phase == warmWorkspaceReturning {
			if !position.atHome {
				return false
			}
		} else if !position.atTarget || position.homeExists || position.homeErr != nil {
			return false
		}
	}
	return true
}

type warmWorkspacePosition struct {
	atHome, atTarget         bool
	homeExists, targetExists bool
	homeErr, targetErr       error
}

func (p *warmWorkspacePool) inspectSlot(id, target string) warmWorkspacePosition {
	record := p.state.Slots[id]
	home, homeExists, homeErr := warmWorkspaceEntry(p.slots[id].Path, record.Parent)
	position := warmWorkspacePosition{atHome: homeExists && home == record.Identity, homeExists: homeExists, homeErr: homeErr}
	if target != "" {
		destination, exists, err := warmWorkspaceEntry(target, warmWorkspaceIdentity{})
		position.atTarget, position.targetExists, position.targetErr = exists && destination == record.Identity, exists, err
	}
	return position
}

func warmWorkspaceEntry(path string, expectedParent warmWorkspaceIdentity) (warmWorkspaceIdentity, bool, error) {
	parent, err := openWarmWorkspaceDir(filepath.Dir(path), false)
	if expectedParent == (warmWorkspaceIdentity{}) && errors.Is(err, os.ErrNotExist) {
		return warmWorkspaceIdentity{}, false, nil
	}
	if err != nil {
		return warmWorkspaceIdentity{}, false, err
	}
	defer parent.Close()
	if expectedParent != (warmWorkspaceIdentity{}) {
		identity, err := warmWorkspaceDirIdentity(parent)
		if err != nil {
			return warmWorkspaceIdentity{}, false, err
		}
		if identity != expectedParent {
			return warmWorkspaceIdentity{}, false, fmt.Errorf("registered parent changed for %s", path)
		}
	}
	return warmWorkspaceChild(parent, filepath.Base(path))
}

func warmWorkspaceChild(parent *os.File, name string) (warmWorkspaceIdentity, bool, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(int(parent.Fd()), name, &stat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, os.ErrNotExist) {
		return warmWorkspaceIdentity{}, false, nil
	}
	if err != nil {
		return warmWorkspaceIdentity{}, false, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return warmWorkspaceIdentity{}, true, nil
	}
	return warmWorkspaceIdentity{Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, true, nil
}

func warmWorkspaceDirIdentity(dir *os.File) (warmWorkspaceIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &stat); err != nil {
		return warmWorkspaceIdentity{}, err
	}
	return warmWorkspaceIdentity{Dev: uint64(stat.Dev), Ino: uint64(stat.Ino)}, nil
}

func openWarmWorkspaceDir(path string, create bool) (*os.File, error) {
	root, err := os.Open(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	relative := strings.TrimPrefix(path, string(filepath.Separator))
	dir, err := walkWarmWorkspaceDirs(root, relative, create, nil)
	if err != nil {
		return nil, &os.PathError{Op: "open directory", Path: path, Err: err}
	}
	return dir, nil
}

// walkWarmWorkspaceDirs takes ownership of parent. It visits parent before
// following any components and returns the open leaf on success.
func walkWarmWorkspaceDirs(parent *os.File, relative string, create bool, visit func(*os.File) error) (*os.File, error) {
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		if visit != nil {
			if err := visit(parent); err != nil {
				parent.Close()
				return nil, err
			}
		}
		fd, err := openWarmWorkspaceChildDir(parent, component, create)
		name := filepath.Join(parent.Name(), component)
		parent.Close()
		if err != nil {
			return nil, err
		}
		parent = os.NewFile(uintptr(fd), name)
	}
	if visit != nil {
		if err := visit(parent); err != nil {
			parent.Close()
			return nil, err
		}
	}
	return parent, nil
}

func openWarmWorkspaceChildDir(parent *os.File, name string, create bool) (int, error) {
	const flags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
	dir := int(parent.Fd())
	fd, err := unix.Openat(dir, name, flags, 0)
	if create && errors.Is(err, os.ErrNotExist) {
		if err := unix.Mkdirat(dir, name, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return -1, err
		}
		fd, err = unix.Openat(dir, name, flags, 0)
	}
	if err == nil && create {
		// A previous attempt might have made the child visible before syncing
		// the parent. Confirm the link even when this call did not create it.
		if err := parent.Sync(); err != nil {
			_ = unix.Close(fd)
			return -1, err
		}
	}
	return fd, err
}

func syncWarmWorkspaceTargetParents(workspaceRoot, target string, sync func(*os.File) error) error {
	parentPath := filepath.Dir(target)
	relative, err := filepath.Rel(workspaceRoot, parentPath)
	if err != nil || !filepath.IsLocal(relative) {
		return fmt.Errorf("warm workspace destination escapes workspace root: %s", target)
	}
	root, err := openWarmWorkspaceDir(workspaceRoot, false)
	if err != nil {
		return err
	}
	// Recovery can see every ancestor created by an interrupted attempt.
	// Sync the entire workspace chain before finalizing the move.
	parent, err := walkWarmWorkspaceDirs(root, relative, false, sync)
	if err != nil {
		return fmt.Errorf("sync warm workspace destination parents: %w", err)
	}
	return parent.Close()
}

func moveWarmWorkspaceDir(home, target string, record warmWorkspaceSlotRecord, returning bool) error {
	homeParent, err := openWarmWorkspaceDir(filepath.Dir(home), false)
	if err != nil {
		return err
	}
	defer homeParent.Close()
	homeParentID, err := warmWorkspaceDirIdentity(homeParent)
	if err != nil {
		return err
	}
	if homeParentID != record.Parent {
		return errors.New("warm workspace parent inode changed")
	}
	targetParent, err := openWarmWorkspaceDir(filepath.Dir(target), false)
	if err != nil {
		return err
	}
	defer targetParent.Close()
	source, destination := homeParent, targetParent
	sourceName, destinationName := filepath.Base(home), filepath.Base(target)
	if returning {
		source, destination = destination, source
		sourceName, destinationName = destinationName, sourceName
	}
	actual, exists, err := warmWorkspaceChild(source, sourceName)
	if err != nil {
		return err
	}
	if !exists || actual != record.Identity {
		return errors.New("warm workspace source inode changed")
	}
	if err := renameWarmWorkspaceDir(int(source.Fd()), sourceName, int(destination.Fd()), destinationName); err != nil {
		return err
	}
	return errors.Join(source.Sync(), destination.Sync())
}

func syncWarmWorkspaceParent(path string) error {
	parent, err := openWarmWorkspaceDir(filepath.Dir(path), false)
	if err != nil {
		return err
	}
	defer parent.Close()
	return parent.Sync()
}
