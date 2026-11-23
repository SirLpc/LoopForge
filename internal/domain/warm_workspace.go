package domain

import "slices"

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
	OriginSHA256 string `json:"originSHA256,omitempty"`
	Generation   uint64 `json:"generation,omitempty"`
	ID           string `json:"id"`
	ProjectID    string `json:"projectId"`
	RepositoryID string `json:"repositoryId"`
	State        string `json:"state"`
}

const WorkspaceErrorWarmCheckoutRetryable = "warm_checkout_retryable"
const WorkspaceErrorWarmReturnStale = "warm_return_stale"

const RunnerVersionWarmWorkspace = "0.12.0"

func (r Runner) SupportsWarmWorkspace() bool {
	return versionAtLeast(r.RunnerVersion, RunnerVersionWarmWorkspace)
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

// WarmWorkspaceClaimPath is the dedicated Runner claim endpoint. A separate
// path prevents an older server from silently leasing checkout tasks.
const WarmWorkspaceClaimPath = "/runners/warm-tasks/claim"

// RunnerLabelNativeWarmWorkspace marks a dedicated native pool. Unlabelled
// runners continue to serve Docker warm leases.
const RunnerLabelNativeWarmWorkspace = "loopforge-warm-native"

func WarmWorkspaceSandboxDriver(driver string) bool {
	return driver == SandboxDriverDocker || driver == SandboxDriverNative
}

func (r Runner) SupportsWarmWorkspaceDriver(driver string) bool {
	native := slices.Contains(r.Labels, RunnerLabelNativeWarmWorkspace)
	return (native && driver == SandboxDriverNative) || (!native && driver == SandboxDriverDocker)
}
