package runner

import (
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func TestWarmDockerVolumeConfig(t *testing.T) {
	for _, tc := range []struct {
		name, volume, root, pool, workspace, want string
	}{
		{name: "ordinary", workspace: "/home/runner/work"},
		{name: "native pool", pool: "/home/runner/pool.json", workspace: "/home/runner/work"},
		{name: "volume root without volume", root: "/pool", workspace: "/pool/work", want: "volume name"},
		{name: "volume without pool", volume: "unity", root: "/pool", workspace: "/pool/work", want: "pool config"},
		{name: "unclean root", volume: "unity", pool: "/pool/config/pool.json", root: "/pool/.", workspace: "/pool/work", want: "volume root"},
		{name: "outside volume", volume: "unity", pool: "/pool/config/pool.json", root: "/pool", workspace: "/pool-other/work", want: "workspace root"},
		{name: "overlapping volume", volume: "unity", pool: "/pool/config/pool.json", root: "/pool", workspace: "/pool", want: "workspace root"},
		{name: "dedicated volume", volume: "unity", pool: "/pool/config/pool.json", root: "/pool", workspace: "/pool/work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := New(Config{
				ServerURL: "http://localhost:8090", BootstrapToken: "test",
				DefaultWorkspaceRoot: tc.workspace, WarmWorkspacePoolFile: tc.pool, StatePath: "/pool/config/state.json",
				WarmDockerVolume: tc.volume, WarmDockerVolumeRoot: tc.root,
			})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if dedicated := d.client.taskClaimPath() == domain.WarmWorkspaceClaimPath; dedicated != (tc.volume != "") {
					t.Fatalf("unexpected claim endpoint %q", d.client.taskClaimPath())
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestWarmDockerVolumeSlotsRejectPathsOutsideVolume(t *testing.T) {
	slots := map[string]warmWorkspaceSlot{"unity": {ID: "unity", Path: "/pool-mirror/unity"}}
	if err := validateWarmDockerVolumeSlots("/pool", slots); err == nil {
		t.Fatal("slot outside the dedicated volume was accepted")
	}
	slots["unity"] = warmWorkspaceSlot{ID: "unity", Path: "/pool/slots/unity"}
	if err := validateWarmDockerVolumeSlots("/pool", slots); err != nil {
		t.Fatalf("a slot in the dedicated volume was rejected: %v", err)
	}
}

func TestWarmDockerVolumeCLIDirectory(t *testing.T) {
	for _, path := range []string{"", "/pool/tools", "/pool/tools/cli"} {
		if err := validateWarmDockerVolumeCLIDirectory("/pool", path); err != nil {
			t.Errorf("allowed tool directory %q rejected: %v", path, err)
		}
	}
	for _, path := range []string{"/pool", "/pool/config", "/pool/tools-mirror", "/pool/tools/../config", "tools"} {
		if err := validateWarmDockerVolumeCLIDirectory("/pool", path); err == nil {
			t.Errorf("unsafe tool directory %q accepted", path)
		}
	}
}

func TestWarmDockerVolumeRejectsUnleasedWork(t *testing.T) {
	d := &Daemon{cfg: Config{WarmDockerVolume: "unity"}, warmVolumeMount: &runtime.WarmDockerVolume{}}
	if err := d.checkWarmTask(t.Context(), domain.RunnerTask{ID: "plain", Command: "true"}); err == nil {
		t.Error("unleased command accepted by the dedicated runner")
	}
	for _, req := range []domain.RunnerWorkspaceRequest{
		{Operation: "conversation_process", RunID: "ordinary"},
		{Operation: domain.RunnerWorkspaceOperationInfo, RunID: "ordinary"},
		{Operation: domain.RunnerWorkspaceOperationTerminal, RunID: "ordinary"},
		{Operation: domain.RunnerWorkspaceOperationInfo, RunID: "warm", WarmWorkspaceLease: &domain.WarmWorkspaceLease{}},
	} {
		if err := d.checkDedicatedVolumeRequest(req); err == nil {
			t.Errorf("unleased or non-shared %s accepted", req.Operation)
		}
	}
	for _, req := range []domain.RunnerWorkspaceRequest{
		{Operation: "conversation_process"},
		{Operation: domain.RunnerWorkspaceOperationInfo, WorkspaceLayout: domain.RunnerWorkspaceLayoutShared, WarmWorkspaceLease: &domain.WarmWorkspaceLease{}},
	} {
		if err := d.checkDedicatedVolumeRequest(req); err != nil {
			t.Errorf("isolated conversation or leased request rejected: %v", err)
		}
	}
}
