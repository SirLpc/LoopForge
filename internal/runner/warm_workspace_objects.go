package runner

import (
	"path/filepath"
	"sort"
)

func (p *warmWorkspacePool) workspaceObjectDirectories(workspace string) ([]string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for owner, allocation := range p.state.Leases {
		if allocation.Phase != warmWorkspacePlaced || workspace != filepath.Join(p.workspaceRoot, "runs", owner, "workspace") {
			continue
		}
		dirs := make([]string, 0, len(allocation.Lease.Slots))
		for _, slot := range allocation.Lease.Slots {
			dirs = append(dirs, filepath.Join(p.gitCache(slot), "objects"))
		}
		sort.Strings(dirs)
		return dirs, true
	}
	return nil, false
}
