package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/runtime"
)

type warmGitCommand struct {
	WindowsGit             bool
	Outputs                *domain.RunnerInvocation
	SkipPatchInventory     bool `json:",omitempty"`
	Read                   *domain.RunnerWorkspaceRequest
	UnityCheckout          *unityCheckoutRequest
	Action, Message, Label string
	Invocation             *domain.RunnerInvocation
	KnownHosts             []byte
}

// The helper's worktree and available commands are fixed independently of
// its JSON input. Only its dedicated Docker container receives delivery keys.
func RunWarmGitHelperCLI(args []string) bool {
	root := "/workspace"
	switch {
	case len(args) == 1 && args[0] == "warm-git-helper":
	case len(args) == 2 && args[0] == "warm-git-helper-native" && filepath.IsAbs(args[1]) && filepath.Clean(args[1]) == args[1]:
		root = args[1]
	default:
		return false
	}
	var op warmGitCommand
	resp := domain.RunnerWorkspaceResponse{}
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, warmGitMessageLimit))
	if len(args) == 2 {
		decoder = json.NewDecoder(io.LimitReader(os.Stdin, maxWarmGitSessionBytes))
	}
	encoder := json.NewEncoder(os.Stdout)
	if err := decoder.Decode(&op); err != nil {
		resp = runnerWorkspaceError(err)
	} else {
		ctx := context.Background()
		if op.WindowsGit && args[0] == "warm-git-helper-native" {
			ctx = warmWindowsHelperContext(ctx, decoder, encoder)
		}
		resp = op.executeAt(ctx, root, args[0] == "warm-git-helper-native")
	}
	if op.WindowsGit {
		_ = encoder.Encode(warmWindowsHelperMessage{Result: &resp})
	} else {
		_ = encoder.Encode(resp)
	}
	return true
}

func (op warmGitCommand) executeAt(ctx context.Context, root string, native bool) domain.RunnerWorkspaceResponse {
	if req := op.UnityCheckout; req != nil {
		if !native || !validWarmWorkspaceRelativePath(req.RepositoryDir) {
			return runnerWorkspaceError(errors.New("invalid Unity checkout request"))
		}
		info, err := readUnityCheckout(ctx, filepath.Join(root, req.RepositoryDir), req.CommitOnly)
		if err != nil {
			return runnerWorkspaceError(err)
		}
		return runnerWorkspacePayload(info)
	}
	if op.Outputs != nil {
		if op.SkipPatchInventory {
			return runnerWorkspacePayload(collectTaskOutputFiles(root, op.Outputs))
		}
		if native {
			// Recheck after selecting the Git backend, in case a writer started
			// after the Runner's preflight.
			lock, err := taskOutputIndexLock(root, patchInventoryDirs(op.Outputs))
			if err == nil && lock != "" {
				err = outputIndexLocked(lock)
			}
			if err != nil {
				reply := runnerWorkspacePayload(collectTaskOutputFiles(root, op.Outputs))
				reply.Error = err.Error()
				return reply
			}
		}
		out, err := collectTaskOutputsContext(ctx, root, op.Outputs)
		reply := runnerWorkspacePayload(out)
		if native && err != nil {
			reply.Error = err.Error()
		}
		return reply
	}
	if op.Read != nil {
		if !domain.IsWorkspaceGitRead(op.Read.Operation, op.Read.Ref) {
			return runnerWorkspaceError(errors.New("unsupported warm Git read"))
		}
		return readWorkspaceContent(ctx, root, *op.Read)
	}
	if native {
		if op.Invocation == nil {
			return runnerWorkspaceError(errors.New("missing fixed warm checkout repositories"))
		}
		for _, repo := range invocationRepositories(op.Invocation) {
			if !validWarmWorkspaceRelativePath(repo.Dir) {
				return runnerWorkspaceError(errors.New("invalid fixed warm checkout path"))
			}
			if err := verifyNativeWarmConfig(ctx, filepath.Join(root, repo.Dir), "/tmp"); err != nil {
				return runnerWorkspaceError(err)
			}
		}
	}
	if len(op.KnownHosts) > 0 {
		file, err := os.CreateTemp("", "warm-git-hosts-")
		if err != nil {
			return runnerWorkspaceError(err)
		}
		defer os.Remove(file.Name())
		_, err = file.Write(op.KnownHosts)
		err = errors.Join(err, file.Close())
		if err != nil {
			return runnerWorkspaceError(err)
		}
		if err = os.Setenv(gitwork.SSHKnownHostsEnv, file.Name()); err != nil {
			return runnerWorkspaceError(err)
		}
	}
	var value any
	var err error
	switch op.Action {
	case domain.RunnerTaskActionIntegrate:
		value, err = integrateTaskRepositories(ctx, root, op.Invocation, op.Label)
	case domain.RunnerTaskActionPullWorkBranch, domain.RunnerTaskActionPushWorkBranch:
		value, err = syncTaskWorkBranches(ctx, root, op.Invocation, op.Action == domain.RunnerTaskActionPushWorkBranch, op.Message)
	default:
		err = errors.New("unsupported warm Git delivery")
	}
	if err != nil {
		return runnerWorkspaceError(err)
	}
	return runnerWorkspacePayload(value)
}

func (d *Daemon) runWarmGit(ctx context.Context, task domain.RunnerTask, command warmGitCommand) domain.RunnerWorkspaceResponse {
	if !d.matchesWarmSandbox(task.Sandbox) {
		return runnerWorkspaceError(errWarmSandboxDriver)
	}
	var indexErr error
	if d.cfg.WarmNative && command.Outputs != nil {
		indexErr = waitForTaskOutputIndex(ctx, d.taskWorkspace(task), patchInventoryDirs(command.Outputs))
		command.SkipPatchInventory = indexErr != nil
	}
	data, err := json.Marshal(command)
	if err != nil {
		return runnerWorkspaceError(err)
	}
	var out []byte
	if d.cfg.WarmNative {
		out, err = d.runNativeWarmGit(ctx, task, command, data)
	} else if command.Action == "" {
		requirements, resources := d.localTaskSandboxSpec(task)
		_, id, _ := runSandboxKey(task, requirements, resources)
		out, err = runtime.RunWarmGitOffline(ctx, id, d.taskWorkspace(task), requirements, resources.Dockerfile, data, command.Outputs != nil && command.Outputs.RecoverOutputs)
	} else {
		sandbox, release, openErr := d.acquireTaskSandbox(ctx, task)
		if openErr != nil {
			return runnerWorkspaceError(openErr)
		}
		defer release()
		out, err = runtime.RunWarmGitHelper(ctx, sandbox, data)
	}
	if err != nil {
		return runnerWorkspaceError(err)
	}
	var reply domain.RunnerWorkspaceResponse
	if err = json.Unmarshal(out, &reply); err != nil {
		return runnerWorkspaceError(err)
	}
	if indexErr != nil && reply.Error == "" {
		reply.Error = indexErr.Error()
	}
	return reply
}

func (d *Daemon) readWarmWorkspaceGit(ctx context.Context, root string, req domain.RunnerWorkspaceRequest) domain.RunnerWorkspaceResponse {
	rel, err := filepath.Rel(d.cfg.DefaultWorkspaceRoot, root)
	if err != nil {
		return runnerWorkspaceError(err)
	}
	task := domain.RunnerTask{ID: "workspace-" + req.RunID, RunID: req.RunID, WarmWorkspaceLease: req.WarmWorkspaceLease, Workspace: filepath.ToSlash(rel), Sandbox: req.Sandbox, Provider: "terminal"}
	read := domain.RunnerWorkspaceRequest{Operation: req.Operation, Path: req.Path, Ref: req.Ref, Repositories: req.Repositories, RunStatus: req.RunStatus}
	return d.runWarmGit(ctx, task, warmGitCommand{Read: &read})
}

func (d *Daemon) executeWarmGitDelivery(ctx context.Context, task domain.RunnerTask, target any) error {
	if task.Invocation == nil {
		return errors.New("missing warm delivery repositories")
	}
	inv := task.Invocation
	scoped := &domain.RunnerInvocation{Repositories: invocationRepositories(inv), RunID: inv.RunID, NodeID: inv.NodeID, GitUserName: inv.GitUserName, GitUserEmail: inv.GitUserEmail, GitSSHKeys: make(map[string]string), GitHTTPCredentials: make(map[string]domain.GitHTTPExecutionCredential)}
	for _, repo := range scoped.Repositories {
		if repo.UseSSH {
			if key := inv.GitSSHKeys[repo.CloneURL]; key != "" {
				scoped.GitSSHKeys[repo.CloneURL] = key
			}
			continue
		}
		if host, ok := gitwork.HTTPSRemoteHost(repo.CloneURL); ok {
			if cred, ok := inv.GitHTTPCredentials[host]; ok {
				scoped.GitHTTPCredentials[host] = cred
			}
		}
	}
	cmd := warmGitCommand{Action: task.Action, Message: task.WorkBranchMessage, Label: task.IntegrationLabel, Invocation: scoped}
	if path := os.Getenv(gitwork.SSHKnownHostsEnv); path != "" && len(scoped.GitSSHRemotes()) > 0 {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		cmd.KnownHosts = data
	}
	reply := d.runWarmGit(ctx, task, cmd)
	if reply.Error != "" {
		return errors.New(reply.Error)
	}
	return json.Unmarshal(reply.Payload, target)
}

func (d *Daemon) withTaskOutputs(ctx context.Context, task domain.RunnerTask, result execResult) execResult {
	inv := task.Invocation
	if inv == nil || !inv.ReportOutputs {
		return result
	}
	if task.WarmWorkspaceLease == nil {
		result.outputs = collectTaskOutputs(d.taskWorkspace(task), inv)
		return result
	}
	// Output reporting includes Git patch inventory. Keep all its Git commands
	// and artifact paths in the qualified sandbox, even when the node failed.
	report := &domain.RunnerInvocation{ReportOutputs: true, RecoverOutputs: inv.RecoverOutputs, OutputFiles: inv.OutputFiles}
	for _, repo := range invocationRepositories(inv) {
		report.Repositories = append(report.Repositories, domain.RunnerRepository{Dir: repo.Dir})
	}
	response := d.runWarmGit(ctx, task, warmGitCommand{Outputs: report})
	var err error
	if response.Error == "" || len(response.Payload) != 0 {
		err = json.Unmarshal(response.Payload, &result.outputs)
	}
	if response.Error != "" {
		err = errors.Join(errors.New(response.Error), err)
	}
	if err != nil {
		if result.status != domain.RunnerTaskCanceled {
			result.status = domain.RunnerTaskFailed
		}
		msg := fmt.Sprintf("warm task output collection failed: %v", err)
		if result.errMsg != "" {
			msg = result.errMsg + "\n" + msg
		}
		result.errMsg = msg
	}
	return result
}
