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
	OwnerRunID string            `json:"ownerRunId"`
	RunnerID   string            `json:"runnerId"`
	Generation uint64            `json:"generation"`
	Slots      map[string]string `json:"slots"` // repository ID to slot ID
}
