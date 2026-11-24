package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
)

const unityTestPrompt = `当前节点可以运行登记工作区的 Windows Unity 测试。需要验证时，在 shell 执行：
  "$AGENTFLOW_UNITY_TEST" --mode EditMode
  "$AGENTFLOW_UNITY_TEST" --mode PlayMode
可加 --test-filter 选择用例。有多个 Unity 仓库时用 --repository 指定仓库 ID。首次导入可能较慢，保持命令运行并等待结果。返回 JSON 包含测试状态、计数及工作区相对报告路径。请依据本次报告说明结果。

`

const unityTestTimeout = 30 * time.Minute

func validUnityEditorPath(path string) bool {
	if len(path) < 4 || path[1:3] != `:\` || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ", rune(path[0])) {
		return false
	}
	parts := strings.Split(path[3:], `\`)
	for _, part := range parts {
		if part == "" || strings.TrimRight(part, " .") != part || strings.ContainsAny(part, "<>:\"/|?*\r\n\x00") {
			return false
		}
	}
	return strings.EqualFold(parts[len(parts)-1], "Unity.exe")
}

func (d *Daemon) unityTestSlots(task domain.RunnerTask) map[string]warmWorkspaceSlot {
	p, lease := d.warmWorkspacePool, task.WarmWorkspaceLease
	if p == nil || !d.cfg.WarmNative || lease == nil || !d.matchesWarmSandbox(task.Sandbox) {
		return nil
	}
	var slots map[string]warmWorkspaceSlot
	for repo, id := range lease.Slots {
		if slot := p.slots[id]; slot.UnityEditorPath != "" && slot.RepositoryID == repo {
			if slots == nil {
				slots = make(map[string]warmWorkspaceSlot)
			}
			slots[repo] = slot
		}
	}
	return slots
}

func (d *Daemon) hasUnityTestSlot(task domain.RunnerTask) bool {
	return len(d.unityTestSlots(task)) != 0
}

func (d *Daemon) unityTestInvocation(task domain.RunnerTask, driver string) (string, string) {
	if !d.hasUnityTestSlot(task) {
		return task.Prompt, driver
	}
	// Select the existing provider behavior for the outer bwrap sandbox,
	// which owns this task's socket mount.
	return unityTestPrompt + task.Prompt, domain.SandboxDriverBwrap
}

func (d *Daemon) bindUnityTest(ctx context.Context, task domain.RunnerTask, command runtime.CommandSpec) (runtime.CommandSpec, func(), error) {
	if !d.hasUnityTestSlot(task) {
		return command, func() {}, nil
	}
	binary, err := os.Executable()
	if err != nil {
		return command, nil, err
	}
	dir, closeBridge, err := startUnityTestBridge(ctx, func(ctx context.Context, req unityTestRequest) unityTestResult {
		return d.runUnityTestForTask(ctx, task, req)
	})
	if err != nil {
		return command, nil, err
	}
	client := filepath.Join(dir, unityTestClientName)
	if err := os.WriteFile(client, nil, 0600); err != nil {
		closeBridge()
		return command, nil, err
	}
	return runtime.WithNativeWarmCommandTool(command, binary, client, "AGENTFLOW_UNITY_TEST"), closeBridge, nil
}

func (d *Daemon) runUnityTestForTask(ctx context.Context, task domain.RunnerTask, req unityTestRequest) (result unityTestResult) {
	result = unityTestResult{Status: "infra_error", Mode: req.Mode, Repository: req.Repository}
	slots := d.unityTestSlots(task)
	if req.Repository == "" && len(slots) == 1 {
		for repo := range slots {
			req.Repository = repo
		}
	}
	slot, ok := slots[req.Repository]
	if !ok {
		ids := make([]string, 0, len(slots))
		for id := range slots {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		result.Message = "select a Unity-enabled leased repository with --repository: " + strings.Join(ids, ", ")
		return result
	}
	result.Repository = slot.RepositoryID
	p, lease := d.warmWorkspacePool, task.WarmWorkspaceLease
	if _, busy := p.unityTests.LoadOrStore(slot.ID, struct{}{}); busy {
		result.Status, result.Message = "busy", "the leased Unity project already has a test in progress"
		return result
	}
	defer p.unityTests.Delete(slot.ID)
	ctx, cancel := context.WithTimeout(ctx, unityTestTimeout)
	defer cancel()
	defer func() {
		switch ctx.Err() {
		case context.DeadlineExceeded:
			result.Status, result.Message = "timeout", "Unity test exceeded the Runner deadline"
		case context.Canceled:
			result.Status, result.Message = "canceled", "Unity test was canceled"
		}
	}()

	owner, workspace := d.taskGateKey(task), d.taskWorkspace(task)
	leaseIsCurrent := func() error {
		if owner != lease.OwnerRunID || workspace != filepath.Join(p.workspaceRoot, "runs", owner, "workspace") {
			return errors.New("Unity tests require the matching shared warm workspace")
		}
		return d.checkWarmOperation(ctx, owner, domain.RunnerWorkspaceOperationInfo, lease)
	}
	if err := leaseIsCurrent(); err != nil {
		result.Message = d.publicWarmError(err.Error())
		return result
	}
	if nativeWarmEditorBusy(slot) {
		result.Status, result.Message = "busy", "the leased Unity project is open in an editor"
		return result
	}
	p.mu.Lock()
	checkout := p.state.Leases[owner].Targets[slot.ID]
	p.mu.Unlock()
	rel, err := filepath.Rel(workspace, checkout)
	if err != nil || !validWarmWorkspaceRelativePath(filepath.ToSlash(rel)) {
		result.Message = "the leased Unity checkout has an invalid workspace path"
		return result
	}
	reqGit := unityCheckoutRequest{RepositoryDir: filepath.ToSlash(rel)}
	ref, err := d.unityCheckout(ctx, task, reqGit)
	if err != nil {
		result.Message = "cannot determine the current Unity checkout commit"
		return result
	}
	result.Commit, result.Dirty = ref.Commit, ref.Dirty
	project, err := warmWindowsPath(ctx, filepath.Join(slot.Path, slot.UnityProjectPath))
	if err != nil || len(project) < 3 || project[1:3] != `:\` {
		result.Message = "the leased Unity project must be on a Windows drive visible to WSL"
		return result
	}
	output, err := runWindowsUnityTest(ctx, windowsUnityTestRequest{
		Editor: slot.UnityEditorPath, Project: project, Mode: req.Mode, Filter: req.TestFilter,
	}, func() error {
		if err := leaseIsCurrent(); err != nil {
			return err
		}
		if nativeWarmEditorBusy(slot) {
			return errUnityEditorBusy
		}
		reqGit.CommitOnly = true
		current, err := d.unityCheckout(ctx, task, reqGit)
		if err != nil || current.Commit != ref.Commit {
			return errors.New("Unity checkout commit changed before test launch")
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, errUnityEditorBusy) {
			result.Status = "busy"
		}
		result.Message = d.publicWarmError(err.Error())
		return result
	}
	result.Status, result.Message, result.ExitCode = output.Status, output.Message, output.ExitCode
	result.LogTruncated = output.LogTruncated
	if output.ExitCode != nil && result.Status == "completed" {
		result.Status, result.Counts = unityReportStatus(output.XML, *output.ExitCode)
		if result.Status == "infra_error" {
			result.Message = unityTestDiagnostic(output.Log)
		}
	}
	if err := saveUnityTestReports(workspace, output, &result); err != nil {
		result.Status, result.Message = "infra_error", "could not save Unity test reports in the leased workspace"
	}
	return result
}

var errUnityEditorBusy = errors.New("the leased Unity project is open in an editor")

type unityCheckoutRequest struct {
	RepositoryDir string
	CommitOnly    bool
}

type unityCheckoutInfo struct {
	Commit string
	Dirty  *bool
}

func (d *Daemon) unityCheckout(ctx context.Context, task domain.RunnerTask, req unityCheckoutRequest) (unityCheckoutInfo, error) {
	var info unityCheckoutInfo
	reply := d.runWarmGit(ctx, task, warmGitCommand{UnityCheckout: &req})
	if reply.Error != "" {
		return info, errors.New(reply.Error)
	}
	return info, json.Unmarshal(reply.Payload, &info)
}

// This runs in the existing offline, read-only Git helper. The checkout still
// belongs to the task and can change its Git metadata while testing.
func readUnityCheckout(ctx context.Context, dir string, commitOnly bool) (unityCheckoutInfo, error) {
	git := func(args ...string) (string, error) {
		return gitwork.CStdoutRaw(ctx, dir, nil, nativeWarmGitArgs(args...)...)
	}
	commit, err := git("rev-parse", "--verify", "HEAD^{commit}")
	info := unityCheckoutInfo{Commit: strings.TrimSpace(commit)}
	if err != nil || commitOnly {
		return info, err
	}
	if verifyNativeWarmConfig(ctx, dir, "/tmp") != nil {
		return info, nil
	}
	err = nativeWarmStatus(git)
	dirty := errors.Is(err, errNativeWarmDirty)
	if err == nil || dirty {
		info.Dirty = &dirty
	}
	return info, nil
}

func unityTestDiagnostic(log []byte) string {
	message := strings.ToLower(string(log))
	for _, phrase := range []string{"no valid unity editor license", "failed to activate", "failed to connect to the licensing client", "no valid license"} {
		if strings.Contains(message, phrase) {
			return "Unity licensing failed under the Runner's Windows user"
		}
	}
	return "Unity did not exit normally with a complete NUnit test report"
}

func saveUnityTestReports(workspace string, output windowsUnityTestOutput, result *unityTestResult) error {
	if len(output.XML) == 0 && len(output.Log) == 0 {
		return nil
	}
	var id [16]byte
	rand.Read(id[:])
	dir, err := runtime.EnsureControlDirectory(workspace, "runtime", "unity", hex.EncodeToString(id[:]))
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer root.Close()
	rel, err := filepath.Rel(workspace, dir)
	if err != nil {
		return err
	}
	for _, report := range []struct {
		name string
		data []byte
		path *string
	}{{"results.xml", output.XML, &result.XMLPath}, {"Editor.log", output.Log, &result.LogPath}} {
		if len(report.data) == 0 {
			continue
		}
		path := filepath.Join(rel, report.name)
		file, err := root.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = file.Write(report.data)
		if err := errors.Join(err, file.Close()); err != nil {
			return err
		}
		*report.path = filepath.ToSlash(path)
	}
	return nil
}
