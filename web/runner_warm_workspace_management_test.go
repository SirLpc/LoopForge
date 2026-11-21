package web

import (
	"os/exec"
	"testing"
)

func TestRunnerWarmWorkspaceManagementUI(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	if output, err := exec.Command(node, "runner_warm_workspace_management_test.js").CombinedOutput(); err != nil {
		t.Fatalf("runner warm workspace UI: %v\n%s", err, output)
	}
}
