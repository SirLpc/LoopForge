package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestUnityResultRequiresFreshExecutedTests(t *testing.T) {
	for _, tc := range []struct {
		name, xml, status string
		exit              int
	}{
		{"passed", `<test-run result="Passed" total="3" passed="2" failed="0" skipped="1"/>`, "passed", 0},
		{"failed", `<test-run result="Failed" total="1" passed="0" failed="1"/>`, "failed", 2},
		{"all skipped", `<test-run result="Skipped" total="1" passed="0" failed="0" skipped="1"/>`, "no_tests", 0},
		{"missing", ``, "infra_error", 0},
		{"truncated", `<test-run result="Passed" total="1" passed="1">`, "infra_error", 0},
		{"abnormal exit", `<test-run result="Passed" total="1" passed="1"/>`, "infra_error", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, _ := unityReportStatus([]byte(tc.xml), tc.exit); status != tc.status {
				t.Fatalf("status = %q, want %q", status, tc.status)
			}
		})
	}
}

func TestUnityTaskCommandUsesPrivateBridge(t *testing.T) {
	bridge, closeBridge, err := startUnityTestBridge(t.Context(), func(_ context.Context, request unityTestRequest) unityTestResult {
		return unityTestResult{Status: "passed", Mode: request.Mode, Repository: request.Repository,
			Message: request.TestFilter, XMLPath: ".agentflow/runtime/unity/run/results.xml"}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBridge()
	var stdout, stderr bytes.Buffer
	code := runUnityTestClient(bridge, []string{"--mode", "PlayMode", "--repository", "pet", "--test-filter", "A.B|C"}, &stdout, &stderr)
	var result unityTestResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v (%s)", err, stderr.String())
	}
	if code != 0 || result.Status != "passed" || result.Mode != "PlayMode" || result.Repository != "pet" || result.Message != "A.B|C" || result.XMLPath == "" {
		t.Fatalf("code = %d, result = %+v", code, result)
	}
	stdout.Reset()
	if code := runUnityTestClient(bridge, []string{"--mode", "PlayMode", "--projectPath", "/other"}, &stdout, &stderr); code == 0 {
		t.Fatal("client accepted a project path override")
	}
	closeBridge()
	if code := runUnityTestClient(bridge, []string{"--mode", "EditMode"}, &stdout, &stderr); code == 0 || !strings.Contains(stderr.String(), "Unity") {
		t.Fatalf("closed task bridge returned %d: %s", code, stderr.String())
	}
}

func TestUnityBridgeRejectsConcurrentCallAndCancelsOnDisconnect(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	dir, closeBridge, err := startUnityTestBridge(t.Context(), func(ctx context.Context, req unityTestRequest) unityTestResult {
		close(started)
		<-ctx.Done()
		close(stopped)
		return unityTestResult{Status: "canceled", Mode: req.Mode}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBridge()
	conn, err := net.Dial("unix", filepath.Join(dir, unityTestSocket))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("{\"mode\":\"EditMode\"}\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("test call did not start")
	}
	var stdout, stderr bytes.Buffer
	if code := runUnityTestClient(dir, []string{"--mode", "PlayMode"}, &stdout, &stderr); code != 1 || !strings.Contains(stdout.String(), `"status":"busy"`) {
		t.Fatalf("concurrent call returned %d: %s %s", code, stdout.String(), stderr.String())
	}
	_ = conn.Close()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect did not stop the in-flight call")
	}
}

func TestUnityRequiresActiveMatchingLease(t *testing.T) {
	fixture := newWarmPoolFixture(t)
	pool := fixture.open(t)
	slot := pool.slots["a"]
	slot.UnityEditorPath = `C:\Unity\Editor\Unity.exe`
	pool.slots[slot.ID] = slot
	runner := &Daemon{cfg: Config{WarmNative: true, DefaultWorkspaceRoot: fixture.workspace}, warmWorkspacePool: pool}
	task := domain.RunnerTask{Workspace: "runs/old/workspace", RunID: "old", WarmWorkspaceLease: &domain.WarmWorkspaceLease{
		OwnerRunID: "old", Slots: map[string]string{"a": "a"},
	}, Sandbox: &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: domain.SandboxDriverNative}}}
	result := runner.runUnityTestForTask(t.Context(), task, unityTestRequest{Mode: "EditMode"})
	if result.Status != "infra_error" || !strings.Contains(result.Message, "lease no longer active") || result.ExitCode != nil {
		t.Fatalf("accepted an inactive lease: %+v", result)
	}
}
