package runner

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"agentflow/internal/domain"
	"golang.org/x/sys/unix"
)

type warmWorkspacePhase string

const (
	warmWorkspaceReserved  warmWorkspacePhase = "reserved"
	warmWorkspacePlacing   warmWorkspacePhase = "placing"
	warmWorkspacePlaced    warmWorkspacePhase = "placed"
	warmWorkspaceReturning warmWorkspacePhase = "returning"
	warmWorkspaceReleased  warmWorkspacePhase = "released"
)

type warmWorkspaceIdentity struct {
	Dev uint64 `json:"device"`
	Ino uint64 `json:"inode"`
}

type warmWorkspaceSlotRecord struct {
	OriginSHA256    string                   `json:"originSHA256,omitempty"`
	ReturnedRef     *domain.WarmWorkspaceRef `json:"returnedRef,omitempty"`
	Path            string                   `json:"path"`
	ProjectID       string                   `json:"projectId"`
	RepositoryID    string                   `json:"repositoryId"`
	Identity        warmWorkspaceIdentity    `json:"identity"`
	Parent          warmWorkspaceIdentity    `json:"parent"`
	Generation      uint64                   `json:"generation"`
	NeedsInspection bool                     `json:"needsInspection,omitempty"`
}

type warmWorkspaceAllocation struct {
	PreparedRefs     map[string]domain.WarmWorkspaceRef `json:"preparedRefs,omitempty"`
	PreparingGit     bool                               `json:"preparingGit,omitempty"`
	CheckoutComplete bool                               `json:"checkoutComplete,omitempty"`
	ReturnProven     bool                               `json:"returnProven,omitempty"`
	ReturnRefs       []domain.WarmWorkspaceRef          `json:"returnRefs,omitempty"`
	ReturnRemove     []string                           `json:"returnRemove,omitempty"`
	GitReady         bool                               `json:"gitReady,omitempty"`

	Lease   domain.WarmWorkspaceLease `json:"lease"`
	Phase   warmWorkspacePhase        `json:"phase"`
	Targets map[string]string         `json:"targets,omitempty"` // slot ID to absolute destination
}

type warmWorkspaceJournal struct {
	Native        bool                               `json:"native,omitempty"`
	Version       int                                `json:"version"`
	WorkspaceRoot string                             `json:"workspaceRoot"`
	Workspace     warmWorkspaceIdentity              `json:"workspaceIdentity"`
	Slots         map[string]warmWorkspaceSlotRecord `json:"slots"`
	Leases        map[string]warmWorkspaceAllocation `json:"leases"`
}

func (j warmWorkspaceJournal) clone() warmWorkspaceJournal {
	j.Slots = maps.Clone(j.Slots)
	j.Leases = maps.Clone(j.Leases)
	return j
}

func (j warmWorkspaceJournal) activeSlots() map[string]bool {
	active := make(map[string]bool)
	for _, allocation := range j.Leases {
		if allocation.Phase != warmWorkspaceReleased {
			for _, id := range allocation.Lease.Slots {
				active[id] = true
			}
		}
	}
	return active
}

func (p *warmWorkspacePool) openJournal(dir string, workspaceID warmWorkspaceIdentity) error {
	var err error
	p.journalDir, err = openWarmWorkspaceDir(dir, true)
	if err != nil {
		return fmt.Errorf("open warm workspace state directory: %w", err)
	}
	p.lock, err = p.openJournalFile(p.journalName+".lock", unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return fmt.Errorf("open warm workspace journal lock: %w", err)
	}
	if err := unix.Flock(int(p.lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("lock warm workspace journal: %w", err)
	}

	journal, err := p.openJournalFile(p.journalName, unix.O_RDONLY)
	fresh := errors.Is(err, os.ErrNotExist)
	switch {
	case fresh:
		p.state = warmWorkspaceJournal{
			Version: 1, Native: p.native, WorkspaceRoot: p.workspaceRoot, Workspace: workspaceID,
			Slots:  make(map[string]warmWorkspaceSlotRecord),
			Leases: make(map[string]warmWorkspaceAllocation),
		}
	case err != nil:
		return fmt.Errorf("open warm workspace journal: %w", err)
	default:
		defer journal.Close()
		if err := decodeWarmWorkspaceJSON(journal, &p.state); err != nil {
			return fmt.Errorf("read warm workspace journal: %w", err)
		}
		if err := p.validateJournal(workspaceID); err != nil {
			return err
		}
	}
	if err := p.registerSlots(dir); err != nil {
		return err
	}
	return p.recover()
}

func (p *warmWorkspacePool) openJournalFile(name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(int(p.journalDir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("warm workspace journal entry %s is not a regular file", name)
	}
	return file, nil
}

func decodeWarmWorkspaceJSON(file *os.File, value any) error {
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected data after JSON document")
	}
	return nil
}

func (p *warmWorkspacePool) validateJournal(workspaceID warmWorkspaceIdentity) error {
	if p.state.Version != 1 || p.state.Native != p.native || p.state.WorkspaceRoot != p.workspaceRoot ||
		p.state.Workspace != workspaceID || p.state.Slots == nil || p.state.Leases == nil {
		return errors.New("warm workspace journal does not match the workspace root or format")
	}
	identities := make(map[warmWorkspaceIdentity]string)
	for id, record := range p.state.Slots {
		slot, exists := p.slots[id]
		if !exists || slot.Path != record.Path || slot.ProjectID != record.ProjectID || slot.RepositoryID != record.RepositoryID {
			return fmt.Errorf("warm workspace registration changed for journaled slot %q", id)
		}
		if record.Identity.Ino == 0 || record.Parent.Ino == 0 ||
			!p.native && (record.Identity.Dev != workspaceID.Dev || record.Parent.Dev != workspaceID.Dev) {
			return fmt.Errorf("warm workspace journal contains an invalid inode for slot %q", id)
		}
		if other, exists := identities[record.Identity]; exists {
			return fmt.Errorf("warm workspace slots %q and %q share an inode", other, id)
		}
		identities[record.Identity] = id
	}
	assigned := make(map[string]bool)
	for owner, allocation := range p.state.Leases {
		lease := allocation.Lease
		if owner != lease.OwnerRunID {
			return errors.New("warm workspace journal contains an inconsistent owner")
		}
		if err := p.validateLease(lease); err != nil {
			return err
		}
		switch allocation.Phase {
		case warmWorkspaceReserved, warmWorkspaceReleased:
			if len(allocation.Targets) != 0 {
				return errors.New("warm workspace journal contains unexpected destinations")
			}
		case warmWorkspacePlacing, warmWorkspacePlaced, warmWorkspaceReturning:
			if allocation.Phase == warmWorkspaceReturning && len(allocation.Targets) == 0 {
				break // Return can start before the lease has ever been placed.
			}
			dirs := make(map[string]string, len(lease.Slots))
			for repo, id := range lease.Slots {
				dirs[repo] = allocation.Targets[id]
			}
			targets, err := p.ownerTargets(lease, dirs)
			if err != nil || !maps.Equal(targets, allocation.Targets) {
				return errors.New("warm workspace journal contains an invalid destination")
			}
		default:
			return fmt.Errorf("warm workspace journal contains an invalid phase %q", allocation.Phase)
		}
		for _, id := range lease.Slots {
			if assigned[id] || p.state.Slots[id].Generation != lease.Generation {
				return fmt.Errorf("warm workspace journal contains conflicting assignments for slot %q", id)
			}
			assigned[id] = true
		}
	}
	return nil
}

func (p *warmWorkspacePool) registerSlots(stateDir string) error {
	next := p.state.clone()
	for id, slot := range p.slots {
		if _, exists := next.Slots[id]; exists {
			continue
		}
		record, err := p.registerSlot(slot)
		if err != nil {
			return fmt.Errorf("register warm workspace slot %q: %w", id, err)
		}
		next.Slots[id] = record
	}
	if err := validateWarmWorkspaceTopology(next.Slots, p.workspaceRoot, stateDir); err != nil {
		return err
	}
	if len(next.Slots) == len(p.state.Slots) {
		return nil
	}
	return p.save(next)
}

func validateWarmWorkspaceTopology(records map[string]warmWorkspaceSlotRecord, protectedPaths ...string) error {
	slotRoots := make(map[warmWorkspaceIdentity]string, len(records))
	for id, record := range records {
		if other, exists := slotRoots[record.Identity]; exists {
			return fmt.Errorf("warm workspace slots %q and %q share a directory inode", other, id)
		}
		slotRoots[record.Identity] = id
	}
	protectedRoots := make(map[warmWorkspaceIdentity]string, len(protectedPaths))
	for _, path := range protectedPaths {
		lineage, err := warmWorkspaceDirLineage(path)
		if err != nil {
			return err
		}
		for _, ancestor := range lineage {
			if slotID, exists := slotRoots[ancestor]; exists {
				return fmt.Errorf("warm workspace slot %q overlaps protected directory %s", slotID, path)
			}
		}
		protectedRoots[lineage[len(lineage)-1]] = path
	}
	// Compare identities along each directory path. Case aliases on a
	// case-insensitive volume can evade lexical comparisons.
	for id, record := range records {
		// Old allocations can leave their home empty or inaccessible. Inspect
		// the reachable prefix here and let recovery check the leased inode.
		lineage, _ := warmWorkspaceDirLineage(filepath.Dir(record.Path))
		for _, ancestor := range lineage {
			if other, exists := slotRoots[ancestor]; exists {
				return fmt.Errorf("overlapping paths for warm workspace slots %q and %q", other, id)
			}
			if path, exists := protectedRoots[ancestor]; exists {
				return fmt.Errorf("warm workspace slot %q overlaps protected directory %s", id, path)
			}
		}
	}
	return nil
}

func warmWorkspaceDirLineage(path string) ([]warmWorkspaceIdentity, error) {
	root, err := os.Open(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	var lineage []warmWorkspaceIdentity
	dir, err := walkWarmWorkspaceDirs(root, strings.TrimPrefix(path, string(filepath.Separator)), false, func(dir *os.File) error {
		identity, err := warmWorkspaceDirIdentity(dir)
		if err != nil {
			return err
		}
		lineage = append(lineage, identity)
		return nil
	})
	if dir != nil {
		dir.Close()
	}
	return lineage, err
}

func (p *warmWorkspacePool) registerSlot(slot warmWorkspaceSlot) (warmWorkspaceSlotRecord, error) {
	var record warmWorkspaceSlotRecord
	parent, err := openWarmWorkspaceDir(filepath.Dir(slot.Path), false)
	if err != nil {
		return record, err
	}
	defer parent.Close()
	parentID, err := warmWorkspaceDirIdentity(parent)
	if err != nil {
		return record, err
	}
	identity, exists, err := warmWorkspaceChild(parent, filepath.Base(slot.Path))
	if err != nil {
		return record, err
	}
	if !exists || identity.Ino == 0 {
		return record, errors.New("slot path must be a real directory")
	}
	if (!p.native && identity.Dev != p.state.Workspace.Dev) || parentID.Dev != identity.Dev {
		return record, errors.New("slot and workspace root must be on the same filesystem")
	}
	if err := p.checkUnityProject(slot, slot.Path); err != nil {
		return record, err
	}
	return warmWorkspaceSlotRecord{
		Path: slot.Path, ProjectID: slot.ProjectID, RepositoryID: slot.RepositoryID,
		Identity: identity, Parent: parentID,
	}, nil
}

func (p *warmWorkspacePool) checkUnityProject(slot warmWorkspaceSlot, checkout string) error {
	if slot.UnityProjectPath == "." {
		return nil
	}
	project, err := openWarmWorkspaceDir(filepath.Join(checkout, slot.UnityProjectPath), false)
	if err != nil {
		return fmt.Errorf("open Unity project directory: %w", err)
	}
	defer project.Close()
	identity, err := warmWorkspaceDirIdentity(project)
	if err != nil {
		return err
	}
	checkoutID, ok, err := warmWorkspaceEntry(checkout, warmWorkspaceIdentity{})
	if err != nil || !ok || identity.Dev != checkoutID.Dev {
		return errors.New("Unity project is on another filesystem")
	}
	return nil
}

func (p *warmWorkspacePool) recover() error {
	active := p.state.activeSlots()
	next := p.state.clone()
	for id, record := range p.state.Slots {
		if !active[id] && !record.NeedsInspection &&
			(!p.inspectSlot(id, "").atHome || p.checkUnityProject(p.slots[id], record.Path) != nil) {
			record.NeedsInspection = true
			next.Slots[id] = record
		}
	}
	if !maps.Equal(next.Slots, p.state.Slots) {
		if err := p.save(next); err != nil {
			return err
		}
	}
	for _, allocation := range p.state.Leases {
		if p.native && len(allocation.Targets) > 0 {
			if err := p.bindNativeAllocation(context.Background(), allocation); err != nil {
				return err
			}
			continue
		}
		if allocation.Phase == warmWorkspaceReleased {
			continue
		}
		positions, err := p.inspectLease(allocation)
		if err != nil {
			if positions == nil {
				return err // The inspection result could not be committed.
			}
			continue
		}
		if (allocation.Phase == warmWorkspacePlacing || allocation.Phase == warmWorkspaceReturning) &&
			warmWorkspaceMoveComplete(allocation, positions) {
			if err := p.finishMove(allocation); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *warmWorkspacePool) saveAllocation(allocation warmWorkspaceAllocation) error {
	next := p.state.clone()
	next.Leases[allocation.Lease.OwnerRunID] = allocation
	return p.save(next)
}

func (p *warmWorkspacePool) save(next warmWorkspaceJournal) error {
	tmp, err := p.openJournalFile(".warm-workspace-"+rand.Text(), unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL)
	if err != nil {
		return err
	}
	dir := int(p.journalDir.Fd())
	defer unix.Unlinkat(dir, tmp.Name(), 0)
	if err := json.NewEncoder(tmp).Encode(next); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(dir, tmp.Name(), dir, p.journalName); err != nil {
		return err
	}
	p.state = next
	// Following a failed directory sync, block new operations until the
	// current durable snapshot can be confirmed.
	p.journalSyncPending = true
	return p.syncJournal()
}

func (p *warmWorkspacePool) syncJournal() error {
	if err := p.journalDir.Sync(); err != nil {
		return err
	}
	p.journalSyncPending = false
	return nil
}

func (p *warmWorkspacePool) ready() error {
	if p.lock == nil {
		return errors.New("warm workspace journal is closed")
	}
	if p.journalSyncPending {
		if err := p.syncJournal(); err != nil {
			return err
		}
	}
	workspace, err := openWarmWorkspaceDir(p.workspaceRoot, false)
	if err != nil {
		return err
	}
	defer workspace.Close()
	identity, err := warmWorkspaceDirIdentity(workspace)
	if err != nil {
		return err
	}
	if identity != p.state.Workspace {
		return errors.New("warm workspace root inode changed")
	}
	return nil
}

func (p *warmWorkspacePool) close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	var lockErr, dirErr error
	if p.lock != nil {
		lockErr = p.lock.Close()
		p.lock = nil
	}
	if p.journalDir != nil {
		dirErr = p.journalDir.Close()
		p.journalDir = nil
	}
	return errors.Join(lockErr, dirErr, p.closeNativeWarmSlots())
}

func checkDisabledWarmJournal(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	path = absolute
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	dir, err := openWarmWorkspaceDir(filepath.Dir(path), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	p := warmWorkspacePool{journalDir: dir}
	file, err := p.openJournalFile(filepath.Base(path), unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect disabled warm workspace pool: %w", err)
	}
	defer file.Close()
	var journal warmWorkspaceJournal
	if err := decodeWarmWorkspaceJSON(file, &journal); err != nil {
		return fmt.Errorf("invalid warm workspace journal: %w", err)
	}
	if journal.Version != 1 || journal.Leases == nil || journal.Slots == nil {
		return errors.New("invalid disabled warm workspace pool journal")
	}
	for _, a := range journal.Leases {
		if a.Phase != warmWorkspaceReleased {
			return errors.New("active warm workspace leases require the registered pool configuration")
		}
	}
	return nil
}
