package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
	"agentflow/internal/tracker"
)

func TestRunnerWarmWorkspaceManagement(t *testing.T) {
	h, owner := workbenchFixture(t)
	st := h.repo
	const runnerID = "warm-pool-manager"
	now := time.Now().UTC().Truncate(time.Second)
	for _, machine := range []domain.Runner{{ID: runnerID, OwnerUserID: owner.ID}, {ID: "other-machine", OwnerUserID: owner.ID}, {ID: "shared-machine"}} {
		if err := st.PutRunner(machine); err != nil {
			t.Fatal(err)
		}
	}
	const remote = "https://example.test/warm-code.git"
	if err := st.PutRepository(domain.Repository{ID: "warm-code", ProjectID: "workbench", CloneURL: remote}); err != nil {
		t.Fatal(err)
	}
	slots := []domain.WarmWorkspaceSlot{
		{ID: "warm/a", ProjectID: "workbench", RepositoryID: "warm-code", State: "available", OriginSHA256: gitwork.RemoteFingerprint(remote)},
		{ID: "warm-b", ProjectID: "workbench", RepositoryID: "other-code", State: "available"},
		{ID: "needs-check", ProjectID: "workbench", RepositoryID: "broken-code", State: "blocked"},
	}
	heartbeat := func(inventory []domain.WarmWorkspaceSlot) {
		t.Helper()
		if err := st.RunnerHeartbeat(runnerID, nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), inventory); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat(slots)

	srv := New(st, engine.New(st, runtime.NewManager(filepath.Join(t.TempDir(), "workspaces"))), tracker.NewService(), "", "")
	peer := domain.User{ID: owner.ID + 1, LoginName: "another-user"}
	do := func(method, machine, body string, user *domain.User, want int) *httptest.ResponseRecorder {
		t.Helper()
		c, rec := cvAContext(t, method, "/api/runners/"+machine+"/warm-workspaces", body, user)
		srv.ServeHTTP(rec, c.Request)
		if rec.Code != want {
			t.Fatalf("%s %s = %d, want %d: %s", method, machine, rec.Code, want, rec.Body)
		}
		return rec
	}
	recordDemand := func(id, machine string, at time.Time, repositories ...string) {
		t.Helper()
		if err := st.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: id, RunnerID: machine, ProjectID: "workbench", RepositoryIDs: repositories}, at); err != nil {
			t.Fatal(err)
		}
	}
	reserve := func(want string) domain.WarmWorkspaceLease {
		t.Helper()
		lease, ok, err := st.ReserveNextWarmWorkspace(runnerID, now)
		if err != nil || ok != (want != "") || lease.OwnerRunID != want {
			t.Fatalf("reservation = %+v, %v, %v; want %q", lease, ok, err, want)
		}
		return lease
	}
	settle := func(lease domain.WarmWorkspaceLease, state domain.WarmWorkspaceState) {
		t.Helper()
		if err := st.SettleWarmWorkspace(lease, state); err != nil {
			t.Fatal(err)
		}
	}

	type slotStatus struct {
		ID, RepositoryID, InventoryState, LeaseState string
		Paused                                       bool
	}
	type poolStatus struct {
		RunnerStatus string
		LastSeenAt   time.Time
		Slots        []slotStatus
		Queue        []struct {
			ProjectID     string
			RepositoryIDs []string
			QueuedAt      time.Time
		}
	}
	view := func() poolStatus {
		t.Helper()
		var out poolStatus
		rec := do("GET", runnerID, "", &owner, http.StatusOK)
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		for _, hidden := range []string{"ownerRunId", "originSHA256", gitwork.RemoteFingerprint(remote), remote, "sessionToken"} {
			if strings.Contains(rec.Body.String(), hidden) {
				t.Fatalf("pool exposes %q: %s", hidden, rec.Body)
			}
		}
		return out
	}
	checkSlot := func(pool poolStatus, want slotStatus) {
		t.Helper()
		i := slices.IndexFunc(pool.Slots, func(s slotStatus) bool { return s.ID == want.ID })
		if i < 0 || pool.Slots[i] != want {
			t.Fatalf("slots = %+v; want %+v", pool.Slots, want)
		}
	}
	const pause = `{"slotId":"warm/a","paused":true}`
	const resume = `{"slotId":"warm/a","paused":false}`
	do("GET", runnerID, "", nil, http.StatusUnauthorized)
	do("GET", runnerID, "", &peer, http.StatusForbidden)
	do("PATCH", runnerID, pause, &peer, http.StatusForbidden)
	do("GET", "shared-machine", "", &owner, http.StatusForbidden)
	do("GET", "missing-machine", "", &owner, http.StatusNotFound)
	do("PATCH", runnerID, `{"slotId":"warm/a"}`, &owner, http.StatusBadRequest)
	do("PATCH", runnerID, `{"slotId":"missing","paused":true}`, &owner, http.StatusNotFound)

	recordDemand("active", runnerID, now, "warm-code")
	active := reserve("active")
	recordDemand("other", "other-machine", now, "warm-code")
	recordDemand("wait-a", runnerID, now.Add(time.Second), "warm-code")
	recordDemand("wait-b", runnerID, now.Add(2*time.Second), "other-code")
	pool := view()
	checkSlot(pool, slotStatus{ID: "warm/a", RepositoryID: "warm-code", InventoryState: "available", LeaseState: "reserved"})
	checkSlot(pool, slotStatus{ID: "needs-check", RepositoryID: "broken-code", InventoryState: "blocked"})
	if pool.RunnerStatus != "online" || !pool.LastSeenAt.Equal(now) {
		t.Fatalf("runner heartbeat = %q at %v", pool.RunnerStatus, pool.LastSeenAt)
	}
	if len(pool.Queue) != 2 || !slices.Equal(pool.Queue[0].RepositoryIDs, []string{"warm-code"}) ||
		!pool.Queue[0].QueuedAt.Equal(now.Add(time.Second)) || !slices.Equal(pool.Queue[1].RepositoryIDs, []string{"other-code"}) {
		t.Fatalf("queue = %+v", pool.Queue)
	}

	do("PATCH", runnerID, pause, &owner, http.StatusNoContent)
	do("PATCH", runnerID, pause, &owner, http.StatusNoContent)
	settle(active, domain.WarmWorkspaceReady)
	checkSlot(view(), slotStatus{ID: "warm/a", RepositoryID: "warm-code", InventoryState: "available", LeaseState: "ready", Paused: true})
	reserve("wait-b")

	// A disconnected or unhealthy runner can stop advertising inventory while
	// the durable lease still holds the slot.
	heartbeat(nil)
	if err := st.MarkRunnerOffline(runnerID, now); err != nil {
		t.Fatal(err)
	}
	settle(active, domain.WarmWorkspaceReleasing)
	pool = view()
	if pool.RunnerStatus != "offline" || len(pool.Queue) != 1 {
		t.Fatalf("offline pool = %+v", pool)
	}
	checkSlot(pool, slotStatus{ID: "warm/a", RepositoryID: "warm-code", LeaseState: "releasing", Paused: true})
	do("PATCH", runnerID, resume, &owner, http.StatusNoContent)
	do("PATCH", runnerID, pause, &owner, http.StatusNoContent)
	settle(active, domain.WarmWorkspaceReleased)
	heartbeat(slots)
	checkSlot(view(), slotStatus{ID: "warm/a", RepositoryID: "warm-code", InventoryState: "available", Paused: true})
	reserve("")
	do("PATCH", runnerID, resume, &owner, http.StatusNoContent)
	if l := reserve("wait-a"); l.Slots["warm-code"] != "warm/a" {
		t.Fatalf("resumed lease = %+v", l)
	}
}
