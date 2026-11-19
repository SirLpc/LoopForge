package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWarmWorkspaceReleaseDetectsNativeWorkspaceProcess(t *testing.T) {
	root := t.TempDir()
	if err := CheckWarmWorkspaceProcesses(t.Context(), root); err != nil {
		t.Fatal("clean fixture blocked", err)
	}
	cmd := exec.Command("sleep", "30")
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := CheckWarmWorkspaceProcesses(t.Context(), root); err == nil || !strings.Contains(err.Error(), "native process") {
		t.Fatalf("expected live owner, got %v", err)
	}
}
func TestWarmWorkspaceReleaseFencesOrphanGitHelper(t *testing.T) {
	image := os.Getenv("AGENTFLOW_WARM_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("requires a local Docker image")
	}
	root := t.TempDir()
	parent := fmt.Sprintf("agentflow-rt-run-absent-%d", os.Getpid())
	name := parent + "-git-orphan"
	args := []string{"run", "--pull=never", "-d", "--name", name, "--label", "io.loopforge.warm-git-parent=" + parent, "--network=none", "-v", root + ":/workspace:ro", "--entrypoint", "sleep", image, "300"}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	if err := FenceWarmWorkspaceContainers(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("docker", "container", "inspect", name).Run(); err == nil {
		t.Fatal("surviving read helper")
	}
	other := name + "-other"
	if err := os.Mkdir(filepath.Join(root, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	args = []string{"run", "--pull=never", "-d", "--name", other, "--network=none", "-v", filepath.Join(root, "nested") + ":/data", "--entrypoint", "sleep", image, "300"}
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", other).Run() })
	if err := FenceWarmWorkspaceContainers(t.Context(), root); err == nil {
		t.Fatal("missed another process mount")
	}
}
