package runner

import (
	"agentflow/internal/domain"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWarmWorkspaceBuiltinVersion(t *testing.T) {
	if !(domain.Runner{RunnerVersion: Version()}).SupportsWarmWorkspace() {
		t.Fatalf("built-in runner %s cannot negotiate warm checkout", Version())
	}
}

func TestWarmWorkspaceInventoryRequiresReadyDriver(t *testing.T) {
	d := Daemon{warmWorkspacePool: newWarmPoolFixture(t).open(t), client: &Client{warmProtocol: true}}
	if got := d.warmWorkspaceInventory(func() bool { return true }); len(got) != 0 {
		t.Fatalf("unqualified host advertised slots: %+v", got)
	}
	d.warmHostVisible.Store(true)
	if got := d.warmWorkspaceInventory(func() bool { return false }); len(got) != 0 {
		t.Fatal(got)
	}
	if got := d.warmWorkspaceInventory(func() bool { return true }); len(got) != 2 {
		t.Fatal(got)
	}
	d.client.warmProtocol = false
	if got := d.warmWorkspaceInventory(func() bool { return true }); len(got) != 0 {
		t.Fatal(got)
	}
}

func TestWarmWorkspacePublicError(t *testing.T) {
	f := newWarmPoolFixture(t)
	d := Daemon{warmWorkspacePool: f.open(t)}
	for _, msg := range []string{"open " + f.homeA + "/Library: permission denied", "rename " + f.workspace + "/runs/owner/workspace/checkout: file exists", "open " + d.warmWorkspacePool.gitRoot + "/object: read-only file system", "open /proc/123/fd: permission denied"} {
		res := d.publicWarmResponse(domain.RunnerWorkspaceResponse{Code: "warm_owner_retained", Status: 409, Error: msg})
		if strings.Contains(res.Error, "/") || res.Code != "warm_owner_retained" || res.Status != 409 || res.Error == "" {
			t.Fatal(res)
		}
	}
	msg := "unarchived file Assets/Scene.unity"
	if got := d.publicWarmResponse(domain.RunnerWorkspaceResponse{Error: msg}); got.Error != msg {
		t.Fatal(got)
	}
}

func TestWarmWorkspaceControlHidesMissingJournalPath(t *testing.T) {
	f := newWarmPoolFixture(t)
	d := Daemon{warmWorkspacePool: f.open(t), state: daemonState{RunnerID: "machine"}}
	dir := filepath.Dir(f.journal)
	if err := os.Rename(dir, dir+"-moved"); err != nil {
		t.Fatal(err)
	}
	got := d.readRunnerWorkspace(t.Context(), domain.RunnerWorkspaceRequest{
		RunID: "owner", Operation: domain.RunnerWorkspaceOperationWarmVerifyAccess, WarmWorkspaceActorID: 1,
		WarmWorkspaceLease:    &domain.WarmWorkspaceLease{OwnerRunID: "owner", RunnerID: "machine", Generation: 1},
		PreparationInvocation: &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{{RepositoryID: "a"}}},
	})
	if got.Status != 404 || !strings.Contains(got.Error, "Runner 日志") || strings.Contains(got.Error, f.root) {
		t.Fatal(got)
	}
}

func TestWarmWorkspaceUnsupportedDockerRetainsJournal(t *testing.T) {
	f := newWarmPoolFixture(t)
	p := f.open(t)
	d, err := New(Config{ServerURL: "https://example.invalid", BootstrapToken: "fixture", WarmWorkspacePoolFile: f.config, DefaultWorkspaceRoot: f.workspace})
	if err != nil {
		t.Fatal(err)
	}
	d.warmWorkspacePool = p
	d.client.warmProtocol = true
	d.warmHostVisible.Store(true)
	d.state.RunnerID = "machine"
	if got := d.warmWorkspaceInventory(func() bool { return true }); len(got) > 0 {
		t.Errorf("remote Docker advertised %+v", got)
	}
	lease := domain.WarmWorkspaceLease{RunnerID: "machine", OwnerRunID: "owner", Epoch: 1, Generation: 1, Slots: map[string]string{"a": "a"}}
	if err := p.reserve(lease); err != nil {
		t.Fatal(err)
	}
	req := domain.RunnerWorkspaceRequest{RunID: "owner", Operation: domain.RunnerWorkspaceOperationWarmStatus, WarmWorkspaceLease: &lease}
	if got := d.readRunnerWorkspace(t.Context(), req); got.Error != "" {
		t.Fatal("journal unavailable", got.Error)
	}
	for _, op := range []string{domain.RunnerWorkspaceOperationWarmPrepare, domain.RunnerWorkspaceOperationWarmReleaseCheck, domain.RunnerWorkspaceOperationWarmReleaseCommit, "browser"} {
		req.Operation = op
		if got := d.readRunnerWorkspace(t.Context(), req); !strings.Contains(got.Error, "Docker") {
			t.Errorf("unqualified %s: %+v", op, got)
		}
	}
	if err := d.checkWarmTask(t.Context(), domain.RunnerTask{RunID: "owner", WarmWorkspaceLease: &lease}); err == nil || !strings.Contains(err.Error(), "Docker") {
		t.Errorf("unqualified task: %v", err)
	}
	if err := d.checkWarmTask(t.Context(), domain.RunnerTask{RunID: "ordinary"}); err != nil {
		t.Errorf("ordinary task: %v", err)
	}
}
