package domain

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
	RepositoryID string `json:"repositoryId"`
	Branch       string `json:"branch,omitempty"`
	Commit       string `json:"commit"`
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

const (
	RunnerWorkspaceOperationWarmCheckout      = "warm_checkout"
	RunnerWorkspaceOperationWarmPrepare       = "warm_prepare"
	RunnerWorkspaceOperationWarmStatus        = "warm_status"
	RunnerWorkspaceOperationWarmReleaseCheck  = "warm_release_check"
	RunnerWorkspaceOperationWarmReleaseCommit = "warm_release_commit"
)

func IsWarmWorkspaceOperation(op string) bool {
	switch op {
	case RunnerWorkspaceOperationWarmCheckout, RunnerWorkspaceOperationWarmPrepare, RunnerWorkspaceOperationWarmStatus, RunnerWorkspaceOperationWarmReleaseCheck, RunnerWorkspaceOperationWarmReleaseCommit:
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
