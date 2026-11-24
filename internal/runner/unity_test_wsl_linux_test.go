package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func TestUnityWSLRunnerExecutesBothModes(t *testing.T) {
	editor := os.Getenv("LOOPFORGE_TEST_UNITY_EDITOR")
	parent := os.Getenv("LOOPFORGE_TEST_WINDOWS_WARM_ROOT")
	if editor == "" || parent == "" {
		t.Skip("set LOOPFORGE_TEST_UNITY_EDITOR and LOOPFORGE_TEST_WINDOWS_WARM_ROOT, then run in a private mount namespace")
	}
	fixture := newWarmPoolFixture(t)
	slot, err := os.MkdirTemp(parent, "loopforge unity test ")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(slot) })
	warmGitWrite(t, slot, "ProjectSettings/ProjectVersion.txt", "m_EditorVersion: 2022.3.62f3\n")
	warmGitWrite(t, slot, "Packages/manifest.json", `{"dependencies":{"com.unity.test-framework":"1.1.33"}}`)
	warmGitWrite(t, slot, ".gitignore", "/Library/\n/Logs/\n/Temp/\n/UserSettings/\n")
	warmGitWrite(t, slot, "Assets/Edit/EditTests.asmdef", `{"name":"EditTests","optionalUnityReferences":["TestAssemblies"],"includePlatforms":["Editor"]}`)
	warmGitWrite(t, slot, "Assets/Edit/EditTests.cs", `using System.Collections; using NUnit.Framework; using UnityEngine.TestTools;
public class EditTests {
    [Test] public void EditPasses() { Assert.Pass(); }
    [UnityTest] public IEnumerator CancelOnly() { while (true) yield return null; }
}`)
	warmGitWrite(t, slot, "Assets/Play/PlayTests.asmdef", `{"name":"PlayTests","optionalUnityReferences":["TestAssemblies"]}`)
	warmGitWrite(t, slot, "Assets/Play/PlayTests.cs", `using System.Collections; using NUnit.Framework; using UnityEngine.TestTools; public class PlayTests { [UnityTest] public IEnumerator PlayPasses() { yield return null; Assert.Pass(); } }`)
	runGitT(t, slot, "init", "-q")
	runGitT(t, slot, "config", "core.filemode", "false")
	runGitT(t, slot, "add", "--all")
	runGitT(t, slot, "-c", "user.name=Unity fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "Unity fixture")
	commit := strings.TrimSpace(runGitT(t, slot, "rev-parse", "HEAD"))
	config, err := json.Marshal(map[string]any{"slots": []warmWorkspaceSlot{{
		ID: "unity", ProjectID: "fixture", RepositoryID: "repo", Path: slot,
		UnityProjectPath: ".", UnityEditorPath: editor,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fixture.config, config, 0600); err != nil {
		t.Fatal(err)
	}
	pool, err := openWarmWorkspacePoolWithinMode(fixture.config, fixture.workspace, fixture.journal, true)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.close()
	lease := domain.WarmWorkspaceLease{OwnerRunID: "run-unity", RunnerID: "runner", Generation: 1, Slots: map[string]string{"repo": "unity"}}
	if err := pool.reserve(lease); err != nil {
		t.Fatal(err)
	}
	if err := pool.place(t.Context(), lease, map[string]string{"repo": "repo"}); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(fixture.workspace, "runs", lease.OwnerRunID, "workspace")
	defer unmountNativeWarmDir(filepath.Join(workspace, "repo"))
	allocation := pool.state.Leases[lease.OwnerRunID]
	allocation.GitReady = true
	if err := pool.saveAllocation(allocation); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	d := &Daemon{
		cfg:    Config{WarmNative: true, DefaultWorkspaceRoot: fixture.workspace, DisableRunSandboxReuse: true},
		client: NewClient(server.URL), executor: runtime.NewExecutorRunner(), warmWorkspacePool: pool,
		state: daemonState{RunnerID: lease.RunnerID},
	}
	task := domain.RunnerTask{ID: "unity-script", RunID: lease.OwnerRunID, Workspace: "runs/run-unity/workspace", WarmWorkspaceLease: &lease,
		Sandbox: &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: domain.SandboxDriverNative}}}
	check := func(t *testing.T, result execResult, mode string) {
		t.Helper()
		t.Logf("%s: %s", mode, result.result)
		if result.status != domain.RunnerTaskSuccess {
			t.Fatalf("%s task failed: %s", mode, result.errMsg)
		}
		var report unityTestResult
		if err := json.Unmarshal([]byte(result.result), &report); err != nil {
			t.Fatal(err)
		}
		if report.Status != "passed" || report.Mode != mode || report.Commit != commit || report.Counts == nil || report.Counts.Passed != 1 {
			t.Fatalf("unexpected %s report: %+v", mode, report)
		}
		for _, path := range []string{report.XMLPath, report.LogPath} {
			if stat, err := os.Stat(filepath.Join(workspace, path)); err != nil || !stat.Mode().IsRegular() || stat.Size() == 0 {
				t.Fatalf("%s report %q missing or empty: %v", mode, path, err)
			}
		}
	}
	t.Run("script EditMode", func(t *testing.T) {
		task.Command = `set -e
			test -z "${WSL_INTEROP-}"
			test -S "${AGENTFLOW_UNITY_TEST%/*}/test.sock"
			if /mnt/c/Windows/System32/cmd.exe /d /c ver >/dev/null 2>&1; then exit 42; fi
			"$AGENTFLOW_UNITY_TEST" --mode EditMode --test-filter EditPasses`
		check(t, d.executeTerminal(t.Context(), task), "EditMode")
	})
	t.Run("agent PlayMode", func(t *testing.T) {
		task.ID, task.Command, task.Prompt = "unity-agent", "", "Run the PlayMode test."
		binary := filepath.Join(workspace, "fixture-codex")
		// Exercise the real Codex command preparation. Simulate its core
		// environment policy without requiring a model call for this test.
		if err := os.WriteFile(binary, []byte(`#!/bin/sh
set -eu
for arg do
  case "$arg" in
    shell_environment_policy.set.AGENTFLOW_UNITY_TEST=*) tool="${arg#*=}" ;;
    danger-full-access) outer_bwrap=1 ;;
    *当前节点*AGENTFLOW_UNITY_TEST*) prompt=1 ;;
  esac
done
test "${outer_bwrap-}" = 1
test "${prompt-}" = 1
tool="${tool#\"}"
tool="${tool%\"}"
env -i PATH=/usr/bin:/bin "$tool" --mode PlayMode --test-filter PlayPasses
`), 0700); err != nil {
			t.Fatal(err)
		}
		task.Invocation = &domain.RunnerInvocation{
			RunID: task.RunID, NodeID: "agent", InvocationID: "unity-agent", Agent: domain.AgentDefinition{ID: "agent", Name: "Agent"},
			Executor: domain.ExecutorDefinition{Kind: "codex", Binary: binary, Args: []string{"exec", "--json", "--sandbox", "workspace-write"}},
		}
		result, _ := d.executeInvocation(t.Context(), task)
		check(t, result, "PlayMode")
	})
	t.Run("cancel releases the project", func(t *testing.T) {
		task.ID, task.Command, task.Invocation = "unity-cancel", `"$AGENTFLOW_UNITY_TEST" --mode EditMode --test-filter CancelOnly`, nil
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan execResult, 1)
		go func() { done <- d.executeTerminal(ctx, task) }()
		wait := time.NewTimer(time.Minute)
		defer wait.Stop()
		poll := time.NewTicker(100 * time.Millisecond)
		defer poll.Stop()
		for !nativeWarmEditorBusy(pool.slots["unity"]) {
			select {
			case result := <-done:
				t.Fatalf("Unity exited before cancellation: %+v", result)
			case <-wait.C:
				cancel()
				<-done
				t.Fatal("Unity never took the project lock")
			case <-poll.C:
			}
		}
		cancel()
		select {
		case result := <-done:
			if result.status != domain.RunnerTaskCanceled {
				t.Fatalf("canceled task = %+v", result)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Runner did not stop the Windows launcher after cancellation")
		}
		if nativeWarmEditorBusy(pool.slots["unity"]) {
			t.Fatal("a canceled test left the Unity project locked")
		}
		task.ID, task.Command = "unity-after-cancel", `"$AGENTFLOW_UNITY_TEST" --mode EditMode --test-filter EditPasses`
		check(t, d.executeTerminal(t.Context(), task), "EditMode")
	})
}
