package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"

	"golang.org/x/sys/unix"
)

func nativeWarmEditorBusy(slot warmWorkspaceSlot) bool {
	_, err := os.Lstat(filepath.Join(slot.Path, slot.UnityProjectPath, "Temp", "UnityLockfile"))
	return !errors.Is(err, os.ErrNotExist)
}

func (p *warmWorkspacePool) bindNativeAllocation(ctx context.Context, a warmWorkspaceAllocation) error {
	returning := a.Phase == warmWorkspaceReturning
	for _, id := range slices.Sorted(maps.Keys(a.Targets)) {
		if err := ctx.Err(); err != nil {
			return err
		}
		target := a.Targets[id]
		position := p.inspectSlot(id, target)
		if !position.atHome || position.targetErr != nil {
			return fmt.Errorf("cannot verify native warm slot %q: %w", id, errors.Join(position.homeErr, position.targetErr))
		}
		if returning {
			if position.atTarget {
				if err := unmountNativeWarmDir(target); err != nil {
					return err
				}
			}
			continue
		}
		if position.atTarget {
			continue
		}
		dir, err := openWarmWorkspaceDir(target, true)
		if err != nil {
			return err
		}
		children, err := dir.Readdirnames(1)
		dir.Close()
		if len(children) > 0 || (err != nil && !errors.Is(err, io.EOF)) {
			return fmt.Errorf("native warm workspace mount point is occupied: %s", target)
		}
		if err := bindNativeWarmDir(p.slots[id].Path, target); err != nil {
			return err
		}
		if !p.inspectSlot(id, target).atTarget {
			_ = unmountNativeWarmDir(target)
			return fmt.Errorf("native warm mount does not expose the registered directory for slot %q", id)
		}
	}
	if a.Phase == warmWorkspacePlaced {
		return nil
	}
	return p.finishMove(a)
}

// Lock the original inode so independent Runner journals cannot lease it.
func (p *warmWorkspacePool) lockNativeWarmSlots() error {
	for _, id := range slices.Sorted(maps.Keys(p.slots)) {
		dir, err := openWarmWorkspaceDir(p.slots[id].Path, false)
		if err != nil {
			return err
		}
		if err := unix.Flock(int(dir.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			dir.Close()
			return fmt.Errorf("native warm slot %q cannot be locked: %w", id, err)
		}
		p.nativeSlots = append(p.nativeSlots, dir)
	}
	return nil
}

func (p *warmWorkspacePool) closeNativeWarmSlots() error {
	var err error
	for _, dir := range p.nativeSlots {
		err = errors.Join(err, dir.Close())
	}
	p.nativeSlots = nil
	return err
}
