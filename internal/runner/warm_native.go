package runner

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"slices"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

var errWarmSandboxDriver = errors.New("sandbox driver does not match the warm Runner")

func configureNativeWarmRunner(cfg *Config) error {
	cfg.Labels = slices.DeleteFunc(slices.Clone(cfg.Labels), func(label string) bool {
		return label == domain.RunnerLabelNativeWarmWorkspace
	})
	if !cfg.WarmNative {
		return nil
	}
	if cfg.WarmWorkspacePoolFile == "" {
		return errors.New("native warm Runner needs a pool file")
	}
	state, err := filepath.Abs(cfg.StatePath)
	if err != nil {
		return err
	}
	stateDir := filepath.Dir(state)
	if home, _ := os.UserHomeDir(); stateDir == home || warmWorkspacePathsOverlap(cfg.DefaultWorkspaceRoot, stateDir) {
		return errors.New("native warm Runner needs a dedicated state directory outside the workspace root")
	}
	cfg.StatePath = state
	cfg.Labels = append(cfg.Labels, domain.RunnerLabelNativeWarmWorkspace)
	return nil
}

func (d *Daemon) matchesWarmSandbox(sandbox *domain.RunnerSandboxSpec) bool {
	if sandbox == nil {
		return false
	}
	if d.cfg.WarmNative {
		// The task claim API can resolve native to bwrap before dispatch.
		return sandbox.Profile.Driver == domain.SandboxDriverNative ||
			goruntime.GOOS == "linux" && sandbox.Profile.Driver == domain.SandboxDriverBwrap
	}
	return sandbox.Profile.Driver == domain.SandboxDriverDocker
}

func (d *Daemon) nativeWarmReady(ctx context.Context) error {
	if _, err := exec.LookPath("git"); err != nil {
		return err
	}
	isolation := d.nativeWarmIsolation()
	workspace, err := os.MkdirTemp(isolation.WorkspaceRoot, ".native-warm-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	sandbox, err := runtime.OpenRemoteSandbox(ctx, "native-warm-probe", workspace,
		domain.SandboxProfile{Driver: domain.SandboxDriverNative},
		runtime.SandboxRequirements{NativeWarm: isolation}, runtime.SandboxResources{})
	if err != nil {
		return err
	}
	defer sandbox.Close(ctx)
	_, err = sandbox.Exec(ctx, runtime.CommandSpec{Argv: []string{"/bin/true"}}, nil)
	return err
}

func (d *Daemon) fenceNativeWarmReturn(ctx context.Context, root string) error {
	p := d.warmWorkspacePool
	p.mu.Lock()
	defer p.mu.Unlock()
	owner := filepath.Base(filepath.Dir(root))
	if err := p.checkNativeWarmEditors(p.state.Leases[owner]); err != nil {
		return err
	}
	return ctx.Err()
}

// The caller holds the pool mutex while checking the assigned slots.
func (p *warmWorkspacePool) checkNativeWarmEditors(a warmWorkspaceAllocation) error {
	for slot := range a.Targets {
		if nativeWarmEditorBusy(p.slots[slot]) {
			return fmt.Errorf("close Unity before writing or releasing warm slot %q", slot)
		}
	}
	return nil
}

func nativeWarmWorkspaceMayWrite(operation string) bool {
	switch operation {
	case domain.RunnerWorkspaceOperationInfo, domain.RunnerWorkspaceOperationTree,
		domain.RunnerWorkspaceOperationFile, domain.RunnerWorkspaceOperationDiff,
		domain.RunnerWorkspaceOperationSearch, domain.RunnerWorkspaceOperationLog,
		domain.RunnerWorkspaceOperationCommit, domain.RunnerWorkspaceOperationCheckouts:
		return false
	default:
		return true
	}
}

func (d *Daemon) runNativeWarmGit(ctx context.Context, task domain.RunnerTask, command warmGitCommand, input []byte) ([]byte, error) {
	workspace := d.taskWorkspace(task)
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	requirements, _ := d.localTaskSandboxSpec(task)
	writable := command.Action != "" || command.Outputs != nil && command.Outputs.RecoverOutputs
	if targets := d.warmWindowsGitTargets(ctx, task, workspace); len(targets) > 0 {
		if writable {
			unlock := lockInvocationWorkspace(workspace)
			defer unlock()
		}
		ctx = context.WithValue(ctx, warmGitRunIDKey{}, task.RunID)
		command.WindowsGit = true
		return runtime.RunNativeWarmGitSession(ctx, workspace, executable, writable, command.Action != "", requirements.NativeWarm, warmWindowsGitExchange(ctx, command, targets))
	}
	return runtime.RunNativeWarmGit(ctx, workspace, executable, input, writable, command.Action != "", requirements.NativeWarm)
}

var errUnleasedNativeWarmRunner = errors.New("the dedicated native warm Runner requires an active workspace lease")

func (d *Daemon) checkNativeWarmRequest(request domain.RunnerWorkspaceRequest) error {
	if !d.cfg.WarmNative {
		return nil
	}
	if request.WarmWorkspaceLease != nil {
		if request.WorkspaceLayout != domain.RunnerWorkspaceLayoutShared {
			return errors.New("the dedicated native warm Runner requires a shared workspace layout")
		}
		return nil
	}
	if request.Operation == "conversation_process" && request.RunID == "" && request.WorkspaceRunID == "" {
		return nil
	}
	return errUnleasedNativeWarmRunner
}

func (d *Daemon) nativeWarmIsolation() *runtime.NativeWarmIsolation {
	p := d.warmWorkspacePool
	// State and journal files are atomically replaced inside this directory.
	// Masking filenames alone would expose both new inodes and temporary files.
	protected := []string{p.journalDir.Name(), d.cfg.WarmWorkspacePoolFile, p.gitCacheRoot(),
		"/run/WSL", "/var/run/docker.sock",
	}
	if cwd, err := os.Getwd(); err == nil {
		protected = append(protected, filepath.Join(cwd, ".env"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{".ssh", ".aws", ".docker", ".kube", ".netrc", ".git-credentials"} {
			protected = append(protected, filepath.Join(home, name))
		}
	}
	for _, slot := range p.slots {
		protected = append(protected, slot.Path)
	}
	return &runtime.NativeWarmIsolation{WorkspaceRoot: p.workspaceRoot, Protected: protected}
}

func (d *Daemon) openNativeWarmConversation(ctx context.Context, root string, release func()) (runtime.Sandbox, func(), error) {
	sb, err := runtime.OpenRemoteSandbox(ctx, "native-warm-conversation", filepath.Join(root, "ws"),
		domain.SandboxProfile{Driver: domain.SandboxDriverNative},
		runtime.SandboxRequirements{NativeWarm: d.nativeWarmIsolation(), ConversationRoot: root},
		runtime.SandboxResources{})
	if err != nil {
		release()
		return nil, nil, err
	}
	return sb, func() {
		_ = sb.Close(context.Background())
		release()
	}, nil
}
