package runner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func TestWarmDockerDesktopLeaseLifecycle(t *testing.T) {
	volume := os.Getenv("AGENTFLOW_WARM_DESKTOP_TEST_VOLUME")
	root := os.Getenv("AGENTFLOW_WARM_DESKTOP_TEST_ROOT")
	image := os.Getenv("AGENTFLOW_WARM_TEST_DOCKER_IMAGE")
	if volume == "" || root == "" || image == "" {
		t.Skip("requires a disposable dedicated Docker Desktop volume and runner test image")
	}
	t.Setenv("AGENTFLOW_ISOLATED_IMAGE", image)
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", "")
	tmp := filepath.Join(root, "fixtures")
	warmPoolMkdir(t, tmp)
	t.Setenv("TMPDIR", tmp)

	f := newWarmPoolFixture(t)
	g := newWarmGitFixture(t)
	f.writeSlots(t, g.checkout)
	d, err := New(Config{
		ServerURL: "http://127.0.0.1:1", BootstrapToken: "test",
		StatePath: filepath.Join(f.root, "state", "runner.json"), DefaultWorkspaceRoot: f.workspace,
		WarmWorkspacePoolFile: f.config, WarmDockerEndpointPinned: true, WarmDockerVolume: volume, WarmDockerVolumeRoot: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.qualifyWarmDockerVolume(t.Context()); err != nil {
		t.Fatal(err)
	}
	p := f.open(t)
	d.warmWorkspacePool = p
	d.state.RunnerID = "fixture-machine"
	first := domain.WarmWorkspaceLease{RunnerID: d.state.RunnerID, OwnerRunID: "wsl-fixture-" + filepath.Base(f.root), Epoch: 1, Generation: 1, Slots: map[string]string{"a": "a"}}
	g.repo.RepositoryID = "a"
	g.repo.Dir = "checkout"
	inv := &domain.RunnerInvocation{Repositories: []domain.RunnerRepository{g.repo}, GitUserName: "fixture", GitUserEmail: "fixture@example.test"}
	prepare := func(lease domain.WarmWorkspaceLease) {
		t.Helper()
		if err := p.reserve(lease); err != nil {
			t.Fatal(err)
		}
		if err := p.place(t.Context(), lease, map[string]string{"a": "checkout"}); err != nil {
			t.Fatal(err)
		}
		if err := p.prepareLeaseCheckout(t.Context(), lease, inv); err != nil {
			t.Fatal(err)
		}
	}
	prepare(first)
	workspace := filepath.Join(f.workspace, "runs", first.OwnerRunID, "workspace")
	spec := &domain.RunnerSandboxSpec{
		Profile:    domain.SandboxProfile{Driver: domain.SandboxDriverDocker},
		Dockerfile: &domain.DockerfileDefinition{Content: runtime.IsolatedDockerfile},
	}
	req := domain.RunnerWorkspaceRequest{
		RunID: first.OwnerRunID, WorkspaceRunID: first.OwnerRunID, WarmWorkspaceLease: &first,
		WorkspaceLayout: domain.RunnerWorkspaceLayoutShared, Sandbox: spec,
		Repositories: []domain.RepositoryContext{{RepositoryID: "a", WorkspaceDir: "checkout"}}, PreparationInvocation: inv,
	}
	req.Operation = domain.RunnerWorkspaceOperationCheckouts
	if got := d.readRunnerWorkspace(t.Context(), req); got.Error != "" || !strings.Contains(string(got.Payload), "checkout") {
		t.Fatalf("read-only Git helper failed: %+v", got)
	}
	sandbox, done, err := d.acquireTaskSandbox(t.Context(), domain.RunnerTask{
		ID: first.OwnerRunID + "-task", RunID: first.OwnerRunID, Workspace: workspace, Sandbox: spec, WarmWorkspaceLease: &first,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer d.closeRunSandboxes(t.Context(), first.OwnerRunID)
	out, err := sandbox.Exec(t.Context(), runtime.CommandSpec{Argv: []string{"sh", "-ce", "cat checkout/Library/sentinel"}}, nil)
	done()
	if err != nil || strings.TrimSpace(out) != "imported" {
		t.Fatalf("Docker task did not see the warm Unity cache: %q, %v", out, err)
	}
	subpath, err := filepath.Rel(root, workspace)
	if err != nil {
		t.Fatal(err)
	}
	name := first.OwnerRunID + "-foreign"
	create := exec.CommandContext(t.Context(), "docker", "create", "--name", name, "--network", "none",
		"--mount", "type=volume,src="+volume+",dst=/fixture,volume-subpath="+subpath, image, "true")
	if out, err := create.CombinedOutput(); err != nil {
		t.Fatalf("create stopped foreign mount: %v %s", err, out)
	}
	defer exec.Command("docker", "rm", "-f", name).Run()
	req.Operation = domain.RunnerWorkspaceOperationWarmReleaseCheck
	if got := d.warmReturnControl(t.Context(), req); got.Error == "" {
		t.Fatal("overlapping stopped container did not retain the workspace")
	}
	if out, err := exec.CommandContext(t.Context(), "docker", "rm", name).CombinedOutput(); err != nil {
		t.Fatalf("remove fixture: %v %s", err, out)
	}
	proof := d.warmReturnControl(t.Context(), req)
	if proof.Error != "" || json.Unmarshal(proof.Payload, &req.WarmWorkspaceRefs) != nil {
		t.Fatalf("warm return proof failed: %+v", proof)
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmReleaseCommit
	if got := d.warmReturnControl(t.Context(), req); got.Error != "" {
		t.Fatalf("return acknowledgement failed: %+v", got)
	}
	second := first
	second.OwnerRunID += "-second"
	second.Generation++
	prepare(second)
	cache := filepath.Join(f.workspace, "runs", second.OwnerRunID, "workspace", "checkout", "Library", "sentinel")
	if got, err := os.ReadFile(cache); err != nil || string(got) != "imported" {
		t.Fatalf("reacquired cache = %q, %v", got, err)
	}
}
