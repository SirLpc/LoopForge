package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

// Only catalog-matched registrations participate in placement. Keep blocked
// slots in the inventory so the allocator retains the local generation floor.
func (h *runnersHandlers) qualifyWarmSlots(slots []domain.WarmWorkspaceSlot) {
	for i := range slots {
		slot := &slots[i]
		repository, ok := h.repo.Repository(slot.RepositoryID)
		if !ok || repository.ProjectID != slot.ProjectID || repository.CloneURL == "" || domain.URLHasCredentials(repository.CloneURL) || slot.OriginSHA256 != gitwork.RemoteFingerprint(repository.CloneURL) {
			slot.State = "blocked"
			slot.OriginSHA256 = ""
		}
	}
}
