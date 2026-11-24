package runner

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
)

func TestNativeWarmOutputWaitsForIndexLockRelease(t *testing.T) {
	workspace := t.TempDir()
	meta := filepath.Join(workspace, "repo", ".git")
	if err := os.MkdirAll(meta, 0o700); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(meta, "index.lock")
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- waitForTaskOutputIndex(t.Context(), workspace, []string{"repo"}) }()
	select {
	case err := <-done:
		t.Fatalf("returned while the lock was still held: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not resume after the index lock was released")
	}
}

func TestNativeWarmOutputHelperPreservesArtifactsWhenIndexIsLocked(t *testing.T) {
	workspace := t.TempDir()
	checkout := filepath.Join(workspace, "repo")
	runGitT(t, workspace, "init", checkout)
	lock := filepath.Join(checkout, ".git", "index.lock")
	if err := os.WriteFile(lock, []byte("held"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "requirement.md"), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := warmGitCommand{Outputs: &domain.RunnerInvocation{
		ReportOutputs: true,
		OutputFiles:   []string{"requirement.md"},
		Repositories:  []domain.RunnerRepository{{Dir: "repo"}},
	}}
	reply := command.executeAt(t.Context(), workspace, true)
	if !strings.Contains(reply.Error, "Git 索引锁未释放") {
		t.Fatalf("expected actionable lock error, got %q", reply.Error)
	}
	var outputs domain.RunnerTaskOutputs
	if err := json.Unmarshal(reply.Payload, &outputs); err != nil || outputs.Files["requirement.md"] != "ready" {
		t.Fatalf("available artifacts were lost: %+v, %v", outputs, err)
	}
	if len(outputs.Patches) != 0 {
		t.Fatalf("unexpected patch inventory: %v", outputs.Patches)
	}
	if content, err := os.ReadFile(lock); err != nil || string(content) != "held" {
		t.Fatalf("index lock was changed: %q, %v", content, err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if retry := command.executeAt(t.Context(), workspace, true); retry.Error != "" {
		t.Fatalf("retry after releasing lock failed: %s", retry.Error)
	}

	runGitT(t, checkout, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "fixture")
	if err := gitwork.RecordBaseline(checkout, ""); err != nil {
		t.Fatal(err)
	}
	lateLock := gitwork.WithLocalGit(t.Context(), func(_ context.Context, dir string, _ io.Reader, args []string) ([]byte, error, bool) {
		if dir == checkout && len(args) != 0 && args[0] == "ls-files" {
			if err := os.WriteFile(lock, []byte("held"), 0o600); err != nil {
				t.Fatal(err)
			}
			return []byte("new.txt\x00"), nil, true
		}
		return nil, nil, false
	})
	if err := os.WriteFile(filepath.Join(checkout, "new.txt"), []byte("untracked"), 0o600); err != nil {
		t.Fatal(err)
	}
	reply = command.executeAt(lateLock, workspace, true)
	if !strings.Contains(reply.Error, "Git 索引锁未释放") {
		t.Fatalf("lock created during inventory was ignored: %q", reply.Error)
	}
	outputs = domain.RunnerTaskOutputs{}
	if err := json.Unmarshal(reply.Payload, &outputs); err != nil || outputs.Files["requirement.md"] != "ready" {
		t.Fatalf("available artifacts were lost after late lock: %+v, %v", outputs, err)
	}
}

func TestPrepareInvocationWorkspaceWritesInputsAndClearsGateResult(t *testing.T) {
	workspace := t.TempDir()
	gate := ".agentflow/gate-results/review/attempt-1.json"
	if err := os.MkdirAll(filepath.Join(workspace, filepath.Dir(gate)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, gate), []byte(`{"decision":"pass"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	inv := &domain.RunnerInvocation{
		GateResultPath: gate,
		Inputs:         []domain.RunnerInputFile{{Path: ".agentflow/inputs/plan/plan.md", Content: "v1"}},
	}
	if err := prepareInvocationWorkspace(workspace, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, gate)); !os.IsNotExist(err) {
		t.Fatalf("stale gate result survived: %v", err)
	}
	inv.Inputs[0].Content = "v2"
	if err := prepareInvocationWorkspace(workspace, inv); err != nil {
		t.Fatalf("rewriting a read-only input: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(workspace, ".agentflow/inputs/plan/plan.md"))
	if string(got) != "v2" {
		t.Fatalf("input = %q", got)
	}
	for _, bad := range []string{"../escape.md", ".agentflow/other/x.md", "/etc/passwd"} {
		err := prepareInvocationWorkspace(workspace, &domain.RunnerInvocation{Inputs: []domain.RunnerInputFile{{Path: bad}}})
		if err == nil {
			t.Fatalf("input path %q accepted", bad)
		}
	}
}

func TestCollectTaskOutputsReportsOnlyRegularFilesInsideWorkspace(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "link.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "report.md"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, maxArtifactUploadBytes+1)
	if err := os.WriteFile(filepath.Join(workspace, "big.md"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(workspace, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "nested", "deep", "misplaced.md"), []byte("found"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := collectTaskOutputs(workspace, &domain.RunnerInvocation{
		ReportOutputs:  true,
		RecoverOutputs: true,
		OutputFiles:    []string{"report.md", "link.md", "big.md", "missing.md", "misplaced.md", "../outside.md"},
	})
	if out == nil || out.Files["report.md"] != "ok" {
		t.Fatalf("outputs = %+v", out)
	}
	if _, ok := out.Files["link.md"]; ok {
		t.Fatal("symlink target was uploaded")
	}
	if len(out.TooLarge) != 1 || out.TooLarge[0] != "big.md" {
		t.Fatalf("tooLarge = %v", out.TooLarge)
	}
	if out.Files["misplaced.md"] != "found" || len(out.Recovered) != 1 || !strings.Contains(out.Recovered[0], "nested/deep/misplaced.md") {
		t.Fatalf("recovery = %v, files = %v", out.Recovered, out.Files)
	}
	if out.Facts == nil {
		t.Fatal("workspace facts missing")
	}
	if collectTaskOutputs(workspace, &domain.RunnerInvocation{}) != nil {
		t.Fatal("outputs reported without ReportOutputs")
	}
}
