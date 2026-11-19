package domain

import "fmt"

type WorkspaceMode string

const (
	WorkspaceModeCheckout WorkspaceMode = "checkout"
	WorkspaceModeWarmPool WorkspaceMode = "warm_pool"
)

// WarmWorkspaceDemand identifies the repositories a workspace owner needs on a runner.
type WarmWorkspaceDemand struct {
	OwnerRunID    string   `json:"ownerRunId"`
	RunnerID      string   `json:"runnerId"`
	ProjectID     string   `json:"projectId"`
	RepositoryIDs []string `json:"repositoryIds"`
}

// WarmWorkspaceLease assigns runner-local slots to a workspace owner.
type WarmWorkspaceLease struct {
	Epoch      uint64            `json:"epoch,omitempty"`
	OwnerRunID string            `json:"ownerRunId"`
	RunnerID   string            `json:"runnerId"`
	Generation uint64            `json:"generation"`
	Slots      map[string]string `json:"slots"` // repository ID to slot ID
}

// WarmWorkspaceRef records an exact checkout for a returned workspace.
type WarmWorkspaceRef struct {
	RepositoryID string            `json:"repositoryId"`
	Branch       string            `json:"branch,omitempty"`
	Commit       string            `json:"commit"`
	Artifacts    map[string]string `json:"artifacts,omitempty"`
}

type WarmWorkspaceState string

const (
	WarmWorkspaceQueued    WarmWorkspaceState = "queued"
	WarmWorkspaceReserved  WarmWorkspaceState = "reserved"
	WarmWorkspaceReady     WarmWorkspaceState = "ready"
	WarmWorkspaceReleasing WarmWorkspaceState = "releasing"
	WarmWorkspaceReleased  WarmWorkspaceState = "released"
	WarmWorkspaceCanceled  WarmWorkspaceState = "canceled"
	WarmWorkspaceBlocked   WarmWorkspaceState = "blocked"
)

type WarmWorkspaceSlot struct {
	Generation   uint64 `json:"generation,omitempty"`
	ID           string `json:"id"`
	ProjectID    string `json:"projectId"`
	RepositoryID string `json:"repositoryId"`
	State        string `json:"state"`
}

const RunnerVersionWarmWorkspace = "0.12.0"

func (r Runner) SupportsWarmWorkspace() bool {
	return versionAtLeast(r.RunnerVersion, RunnerVersionWarmWorkspace)
}

// Occupied slots count as registered. Their availability is the allocator's job.
func (r Runner) CheckWarmWorkspaceRegistration(projectID string, repositories []RepositoryContext) error {
	if !r.SupportsWarmWorkspace() {
		return fmt.Errorf("当前执行机器需升级至 %s，或在已登记仓库的机器新开对话", RunnerVersionWarmWorkspace)
	}
	registered := map[string]bool{}
	for _, slot := range r.WarmWorkspaceSlots {
		if slot.ProjectID == projectID && slot.State != "blocked" {
			registered[slot.RepositoryID] = true
		}
	}
	for _, repository := range repositories {
		if !registered[repository.RepositoryID] {
			return fmt.Errorf("请在当前执行机器登记仓库 %s 的预热工作区，或在已登记的机器新开对话", repository.RepositoryID)
		}
	}
	if len(repositories) == 0 {
		return fmt.Errorf("请指定已登记的项目仓库")
	}
	return nil
}

const (
	RunnerWorkspaceOperationWarmCheckout      = "warm_checkout"
	RunnerWorkspaceOperationWarmPrepare       = "warm_prepare"
	RunnerWorkspaceOperationWarmVerifyAccess  = "warm_verify_access"
	RunnerWorkspaceOperationWarmStatus        = "warm_status"
	RunnerWorkspaceOperationWarmReleaseCheck  = "warm_release_check"
	RunnerWorkspaceOperationWarmReleaseCommit = "warm_release_commit"
)

func IsWholeWorkspaceOperation(op string) bool {
	return IsWorkspaceCheckpointTransferOperation(op) || op == RunnerWorkspaceOperationRestore || op == RunnerWorkspaceOperationCheckpoint || op == RunnerWorkspaceOperationReclaim
}
func IsLongWarmWorkspaceOperation(op string) bool {
	return op == RunnerWorkspaceOperationWarmVerifyAccess || op == RunnerWorkspaceOperationWarmCheckout || op == RunnerWorkspaceOperationWarmReleaseCheck || op == RunnerWorkspaceOperationWarmReleaseCommit
}

func IsWarmWorkspaceOperation(op string) bool {
	switch op {
	case RunnerWorkspaceOperationWarmVerifyAccess, RunnerWorkspaceOperationWarmCheckout, RunnerWorkspaceOperationWarmPrepare, RunnerWorkspaceOperationWarmStatus, RunnerWorkspaceOperationWarmReleaseCheck, RunnerWorkspaceOperationWarmReleaseCommit:
		return true
	}
	return false
}

// IsWorkspaceGitRead identifies views that invoke Git on worktree metadata.
func IsWorkspaceGitRead(op, ref string) bool {
	switch op {
	case RunnerWorkspaceOperationInfo, RunnerWorkspaceOperationCheckouts, RunnerWorkspaceOperationLog, RunnerWorkspaceOperationCommit, RunnerWorkspaceOperationDiff:
		return true
	case RunnerWorkspaceOperationFile:
		return ref != ""
	}
	return false
}

// Current-process capability for claims and the control socket. An old process
// may reuse the machine session while its predecessor's heartbeat is still live.
const WarmWorkspaceProtocolHeader = "X-LoopForge-Warm-Workspace"
const WarmWorkspaceProtocolV1 = "1"
