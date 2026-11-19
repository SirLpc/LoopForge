package runner

import (
	"os"
	"path/filepath"
	"testing"

	"agentflow/internal/domain"
)

func TestWarmWorkspaceExactReadyGeneration(t *testing.T) {
	f := newWarmPoolFixture(t)
	g := newWarmGitFixture(t)
	f.writeSlots(t, g.checkout)
	p := f.open(t)
	d := Daemon{cfg: Config{DefaultWorkspaceRoot: f.workspace}, state: daemonState{RunnerID: "machine"}, warmWorkspacePool: p}
	l := domain.WarmWorkspaceLease{RunnerID: "machine", OwnerRunID: "owner", Epoch: 1, Generation: 5, Slots: map[string]string{"a": "a"}}
	req := domain.RunnerWorkspaceRequest{RunID: "owner", WorkspaceLayout: domain.RunnerWorkspaceLayoutShared, WarmWorkspaceLease: &l, Operation: domain.RunnerWorkspaceOperationWarmPrepare, Repositories: []domain.RepositoryContext{{RepositoryID: "a", WorkspaceDir: "checkout"}}}
	if got := d.readRunnerWorkspace(t.Context(), req); got.Error != "" {
		t.Fatal(got)
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmCheckout
	g.repo.RepositoryID = "a"
	g.repo.Dir = "checkout"
	req.Sandbox = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
	req.PreparationInvocation = &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}}
	if got := d.readRunnerWorkspace(t.Context(), req); got.Error != "" {
		t.Fatal(got)
	}
	checkout := filepath.Join(f.workspace, "runs", "owner", "workspace", "checkout")
	warmGitWrite(t, checkout, "README.md", "live edits")
	if got := d.readRunnerWorkspace(t.Context(), req); got.Error != "" {
		t.Fatal(got)
	}
	if data, err := os.ReadFile(filepath.Join(checkout, "README.md")); err != nil || string(data) != "live edits" {
		t.Fatal("retry modified live checkout", err)
	}
	if _, err := os.Stat(filepath.Join(p.gitCache("a"), "objects")); err != nil {
		t.Fatal("Git cache was not prepared", err)
	}
	req.Operation = "file"
	req.Path = "checkout/README.md"
	req.WorkspaceRunID = "owner"
	req.RunID = "child"
	if got := d.readRunnerWorkspace(t.Context(), req); got.Error != "" {
		t.Fatal(got)
	}
	stale := l
	stale.Generation--
	for _, token := range []*domain.WarmWorkspaceLease{nil, &stale} {
		req.WarmWorkspaceLease = token
		if got := d.readRunnerWorkspace(t.Context(), req); got.Error == "" {
			t.Fatal("stale read", got)
		}
	}

	task := domain.RunnerTask{RunID: "child", RunnerID: "machine", Workspace: "runs/owner/workspace", Command: "true", Sandbox: req.Sandbox, WarmWorkspaceLease: &l}
	if err := d.checkWarmTask(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	task.WarmWorkspaceLease = &stale
	if err := d.checkWarmTask(t.Context(), task); err == nil {
		t.Fatal("stale task used current generation")
	}
	task.WarmWorkspaceLease = nil
	if err := d.checkWarmTask(t.Context(), task); err == nil {
		t.Fatal("untyped task used pooled directory")
	}
	req.WarmWorkspaceLease = &l
	for _, op := range []string{domain.RunnerWorkspaceOperationRestore, domain.RunnerWorkspaceOperationCheckpointExport, domain.RunnerWorkspaceOperationReclaim} {
		req.Operation = op
		if got := d.readRunnerWorkspace(t.Context(), req); got.Error == "" {
			t.Fatalf("%s reached an active pool", op)
		}
	}
}
