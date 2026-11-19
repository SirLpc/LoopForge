package runner

import (
	"context"
	"log"
	"regexp"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
)

// Run on startup and the normal runtime rescan, outside the heartbeat loop.
// The control protocol remains available for owners already in the journal.
func (d *Daemon) refreshWarmHostVisibility(ctx context.Context) {
	if d.warmWorkspacePool == nil {
		return
	}
	probe, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	err := runtime.CheckWarmHostVisibility(probe)
	d.warmHostVisible.Store(err == nil)
	if err != nil {
		log.Printf("warm slots withheld: %v", err)
	}
}

// Only a configured, protocol-compatible process with a reachable Docker
// daemon can advertise physical capacity. Admission still checks the sandbox.
func (d *Daemon) warmWorkspaceInventory(dockerReady func() bool) []domain.WarmWorkspaceSlot {
	if d.warmWorkspacePool == nil || !d.client.warmProtocol || !d.warmHostVisible.Load() || !dockerReady() {
		return nil
	}
	return d.warmWorkspacePool.inventory()
}

var warmAbsolutePath = regexp.MustCompile(`(^|[[:space:]="'(:])/[^/]`)

// Git and the filesystem can include runner paths in diagnostics. Keep those
// in runner logs while preserving user-actionable relative file blockers.
func (d *Daemon) publicWarmResponse(res domain.RunnerWorkspaceResponse) domain.RunnerWorkspaceResponse {
	if res.Error == "" {
		return res
	}
	containsPath := func(path string) bool { return path != "" && strings.Contains(res.Error, path) }
	private := warmAbsolutePath.MatchString(res.Error) || containsPath(d.cfg.WarmWorkspacePoolFile)
	if p := d.warmWorkspacePool; p != nil {
		private = private || containsPath(p.workspaceRoot) || containsPath(p.gitRoot) || (p.journalDir != nil && containsPath(p.journalDir.Name()))
		for _, slot := range p.slots {
			if containsPath(slot.Path) {
				private = true
				break
			}
		}
	}
	if private {
		log.Printf("warm workspace: %s", gitwork.Redact(res.Error))
		res.Error = "预热工作区检查失败，请联系执行机器管理员查看 Runner 日志"
	}
	return res
}
