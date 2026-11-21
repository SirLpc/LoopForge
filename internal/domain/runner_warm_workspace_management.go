package domain

import "time"

// RunnerWarmWorkspacePool combines the runner's latest inventory with the
// platform's active leases and queued requests.
type RunnerWarmWorkspacePool struct {
	RunnerStatus          string                    `json:"runnerStatus"`
	LastSeenAt            *time.Time                `json:"lastSeenAt,omitempty"`
	SupportsWarmWorkspace bool                      `json:"supportsWarmWorkspace"`
	Slots                 []RunnerWarmWorkspaceSlot `json:"slots"`
	Queue                 []RunnerWarmWorkspaceWait `json:"queue"`
}

type RunnerWarmWorkspaceSlot struct {
	ID             string             `json:"id"`
	ProjectID      string             `json:"projectId,omitempty"`
	RepositoryID   string             `json:"repositoryId,omitempty"`
	InventoryState string             `json:"inventoryState,omitempty"`
	LeaseState     WarmWorkspaceState `json:"leaseState,omitempty"`
	Paused         bool               `json:"paused"`
}

type RunnerWarmWorkspaceWait struct {
	ProjectID     string    `json:"projectId"`
	RepositoryIDs []string  `json:"repositoryIds"`
	QueuedAt      time.Time `json:"queuedAt"`
}
