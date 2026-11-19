package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestWarmWorkspaceReleaseRetainsOnHiddenProcfs(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "999"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := checkWarmLinuxProcesses(t.Context(), dir, func(string) bool { return false }); err == nil {
		t.Fatal("incomplete inventory qualified for release")
	}
}
func TestWarmWorkspaceReleaseDetectsNativeWorkspaceProcess(t *testing.T) {
	root := t.TempDir()
	cleanErr := CheckWarmWorkspaceProcesses(t.Context(), root)
	// A host without system-wide visibility must retain even a clean checkout.
	if cleanErr != nil && !strings.Contains(cleanErr.Error(), "workspace retained") {
		t.Fatal(cleanErr)
	}
	cmd := exec.Command("sleep", "30")
	cmd.Dir = root
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if runtime.GOOS == "linux" {
		dir := t.TempDir()
		if err := os.Symlink(filepath.Join("/proc", strconv.Itoa(cmd.Process.Pid)), filepath.Join(dir, strconv.Itoa(cmd.Process.Pid))); err != nil {
			t.Fatal(err)
		}
		if err := checkWarmLinuxProcesses(t.Context(), dir, func(p string) bool { return p == root }); err == nil || !strings.Contains(err.Error(), "native process") {
			t.Fatalf("missed open cwd: %v", err)
		}
		if err := checkWarmLinuxProcesses(t.Context(), dir, func(string) bool { return false }); err != nil {
			t.Fatalf("fully visible inventory blocked: %v", err)
		}
	}
	if err := CheckWarmWorkspaceProcesses(t.Context(), root); err == nil || cleanErr == nil && !strings.Contains(err.Error(), "native process") {
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
