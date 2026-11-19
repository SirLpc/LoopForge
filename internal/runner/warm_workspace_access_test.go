package runner

import (
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
	probe := req
	probe.Operation = domain.RunnerWorkspaceOperationInfo
	probe.Sandbox = nil
	if result := d.readRunnerWorkspace(t.Context(), probe); result.Error == "" {
		t.Error("Git status succeeded without qualified sandbox")
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

func TestWarmWorkspaceRestartCannotDisableActivePool(t *testing.T) {
	f := newWarmPoolFixture(t)
	p := f.open(t)
	l := domain.WarmWorkspaceLease{OwnerRunID: "owner", RunnerID: "machine", Epoch: 1, Generation: 1, Slots: map[string]string{"a": "a"}}
	if err := p.reserve(l); err != nil {
		t.Fatal(err)
	}
	if err := p.close(); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(filepath.Dir(f.journal), "daemon")
	if err := os.Rename(f.journal, state+".warm-workspace.json"); err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{BootstrapToken: "test", ServerURL: "http://127.0.0.1:1", StatePath: state, DefaultWorkspaceRoot: "relative"})
	if err == nil {
		if !filepath.IsAbs(d.cfg.DefaultWorkspaceRoot) {
			t.Error("runner root remains relative")
		}
		err = d.Run(t.Context())
	}
	if err == nil || !strings.Contains(err.Error(), "warm workspace") {
		t.Fatalf("missing active journal rejection: %v", err)
	}
}

func TestWarmWorkspaceDockerGitReadAndDelivery(t *testing.T) {
	image := os.Getenv("AGENTFLOW_WARM_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("requires an installed local Docker image")
	}
	t.Setenv("AGENTFLOW_ISOLATED_IMAGE", image)
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", "")
	f := newWarmPoolFixture(t)
	g := newWarmGitFixture(t)
	f.writeSlots(t, g.checkout)
	p := f.open(t)
	d := Daemon{cfg: Config{DefaultWorkspaceRoot: f.workspace}, state: daemonState{RunnerID: "machine"}, warmWorkspacePool: p}
	owner := "docker-git-" + filepath.Base(f.root)
	defer d.closeRunSandboxes(t.Context(), owner)
	l := domain.WarmWorkspaceLease{RunnerID: "machine", OwnerRunID: owner, Epoch: 1, Generation: 2, Slots: map[string]string{"a": "a"}}
	if err := p.reserve(l); err != nil {
		t.Fatal(err)
	}
	if err := p.place(t.Context(), l, map[string]string{"a": "checkout"}); err != nil {
		t.Fatal(err)
	}
	g.repo.RepositoryID = "a"
	g.repo.Dir = "checkout"
	g.repo.WorkBranch = "work/test"
	inv := &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}, GitUserName: "test", GitUserEmail: "test@example.test"}
	if err := p.prepareLeaseCheckout(t.Context(), l, inv); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(f.workspace, "runs", owner, "workspace")
	checkout := filepath.Join(root, "checkout")
	// This inert host-only configuration makes a file invisible to host Git.
	// A status executed inside the sandbox cannot read the host ignore list.
	hostOnly := filepath.Join(t.TempDir(), "host-only-ignore")
	if err := os.WriteFile(hostOnly, []byte("only-visible-inside.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGitT(t, checkout, "config", "core.excludesFile", hostOnly)
	warmGitWrite(t, checkout, "only-visible-inside.txt", "changed")
	if got := runGitT(t, checkout, "status", "--porcelain"); strings.Contains(got, "only-visible-inside.txt") {
		t.Fatal("host-only fixture ineffective", got)
	}
	spec := &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}, Dockerfile: &domain.DockerfileDefinition{Content: runtime.IsolatedDockerfile}}
	req := domain.RunnerWorkspaceRequest{WarmWorkspaceLease: &l, WorkspaceRunID: owner, RunID: owner, WorkspaceLayout: domain.RunnerWorkspaceLayoutShared, Operation: domain.RunnerWorkspaceOperationInfo, Sandbox: spec, Repositories: []domain.RepositoryContext{{WorkspaceDir: "checkout", RepositoryID: "a"}}}
	got := d.readRunnerWorkspace(t.Context(), req)
	if got.Error != "" || !strings.Contains(string(got.Payload), "only-visible-inside.txt") {
		t.Fatalf("isolated status: %+v", got)
	}
	runGitT(t, root, "init", "--bare", "remote.git")
	runGitT(t, checkout, "remote", "set-url", "origin", "/workspace/remote.git")
	inv.Repositories[0].CloneURL = "/workspace/remote.git"
	inv.Repositories[0].SyncWorkBranch = true
	task := domain.RunnerTask{ID: "git-deliver", RunID: owner, Workspace: filepath.Join("runs", owner, "workspace"), WarmWorkspaceLease: &l, Sandbox: spec, Invocation: inv, Action: domain.RunnerTaskActionPushWorkBranch}
	inv.ReportOutputs = true
	inv.OutputFiles = []string{"checkout/only-visible-inside.txt"}
	output := d.withTaskOutputs(t.Context(), task, execResult{})
	if output.errMsg != "" || output.outputs == nil || output.outputs.Files[inv.OutputFiles[0]] != "changed" {
		t.Fatalf("isolated task outputs %+v (%v)", output.outputs, output.errMsg)
	}
	var heads []domain.RunnerWorkBranchHead
	if err := d.executeWarmGitDelivery(t.Context(), task, &heads); err != nil {
		t.Fatal(err)
	}
	head := runGitT(t, filepath.Join(root, "remote.git"), "rev-parse", "refs/heads/work/test")
	if len(heads) != 1 || heads[0].Head != head {
		t.Fatalf("delivered heads: %+v", heads)
	}
	req.Operation = domain.RunnerWorkspaceOperationCheckouts
	got = d.readRunnerWorkspace(t.Context(), req)
	var result struct{ Checkouts []gitwork.Checkout }
	if got.Error != "" || json.Unmarshal(got.Payload, &result) != nil || len(result.Checkouts) != 1 {
		t.Fatalf("isolated checkouts: %+v", got)
	}
}
