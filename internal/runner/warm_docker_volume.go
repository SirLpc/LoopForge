package runner

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

var errUnleasedDedicatedVolume = errors.New("the dedicated warm Docker Runner requires an active workspace lease")

func (d *Daemon) checkDedicatedVolumeRequest(request domain.RunnerWorkspaceRequest) error {
	if d.cfg.WarmDockerVolume == "" {
		return nil
	}
	if request.WarmWorkspaceLease != nil {
		if request.WorkspaceLayout != domain.RunnerWorkspaceLayoutShared {
			return errors.New("the dedicated warm Docker Runner requires a shared workspace layout")
		}
		return nil
	}
	if request.Operation == "conversation_process" && request.RunID == "" && request.WorkspaceRunID == "" && d.warmVolumeMount != nil {
		return nil
	}
	return errUnleasedDedicatedVolume
}

func validateWarmDockerVolumeCLIDirectory(root, cliDir string) error {
	tools := filepath.Join(root, "tools")
	if cliDir != "" && cliDir != tools && !warmVolumeChild(tools, cliDir) {
		return errors.New("warm Docker runtime CLI directory must be in the dedicated tools directory")
	}
	return nil
}

func validateWarmDockerVolumeConfig(cfg Config) error {
	root := cfg.WarmDockerVolumeRoot
	if cfg.WarmDockerVolume == "" {
		if root != "" {
			return errors.New("warm Docker volume root requires a volume name")
		}
		return nil
	}
	if cfg.WarmWorkspacePoolFile == "" {
		return errors.New("warm Docker volume requires a warm workspace pool config")
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) {
		return errors.New("warm Docker volume root must be a clean absolute mount point")
	}
	for _, entry := range []struct {
		path  string
		label string
	}{
		{cfg.DefaultWorkspaceRoot, "workspace root"},
		{cfg.StatePath, "state file"},
		{cfg.WarmWorkspacePoolFile, "pool config"},
	} {
		if !warmVolumeChild(root, entry.path) || warmWorkspacePathsOverlap(filepath.Join(root, "tools"), entry.path) {
			return fmt.Errorf("warm Docker volume %s must be a clean absolute path inside the dedicated volume, outside its tools directory", entry.label)
		}
	}
	return nil
}

func warmVolumeChild(root, path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && filepath.IsLocal(rel)
}

func validateWarmDockerVolumeSlots(root string, slots map[string]warmWorkspaceSlot) error {
	for id, slot := range slots {
		if !warmVolumeChild(root, slot.Path) || warmWorkspacePathsOverlap(filepath.Join(root, "tools"), slot.Path) {
			return fmt.Errorf("warm Docker volume slot %q must be inside the dedicated volume, outside its tools directory", id)
		}
	}
	return nil
}

func (d *Daemon) openWarmDockerVolume(ctx context.Context) error {
	if d.cfg.WarmDockerVolume == "" {
		return nil
	}
	if d.warmDockerUnavailable() {
		return errWarmDockerUnavailable
	}
	root := d.cfg.WarmDockerVolumeRoot
	desktop, err := runtime.OpenWarmDockerDesktop(ctx, d.cfg.WarmDockerVolume, root)
	if err != nil {
		return fmt.Errorf("qualify dedicated warm Docker volume: %w", err)
	}
	if err := prepareWarmDockerVolumeCLIDirectory(root, runtime.RuntimeCLIDir()); err != nil {
		return err
	}
	helper := filepath.Join(root, "tools", "agentflow-runner")
	if err := validateWarmDockerVolumeHelper(helper); err != nil {
		return err
	}
	d.warmDockerDesktop = desktop
	d.warmVolumeMount = &runtime.WarmDockerVolume{Name: d.cfg.WarmDockerVolume, Root: root, Backing: desktop.Backing(), Helper: helper}
	return nil
}

func (d *Daemon) qualifyWarmDockerVolume(ctx context.Context) error {
	if d.warmDockerDesktop == nil {
		return nil
	}
	if err := d.warmDockerDesktop.SetConversationScope(d.cfg.DefaultWorkspaceRoot, d.warmWorkspacePool.workspaceObjectDirectories); err != nil {
		return err
	}
	probe, stop := context.WithTimeout(ctx, 15*time.Second)
	defer stop()
	if err := d.warmDockerDesktop.Ready(probe); err != nil {
		return fmt.Errorf("qualify warm Docker Desktop process inspection: %w", err)
	}
	d.warmHostVisible.Store(true)
	return nil
}

func prepareWarmDockerVolumeCLIDirectory(root, cliDir string) error {
	if err := validateWarmDockerVolumeCLIDirectory(root, cliDir); err != nil || cliDir == "" {
		return err
	}
	dir, err := openWarmWorkspaceDir(cliDir, true)
	if err != nil {
		return fmt.Errorf("open warm Docker runtime CLI directory: %w", err)
	}
	return dir.Close()
}

func validateWarmDockerVolumeHelper(helper string) error {
	dir, err := openWarmWorkspaceDir(filepath.Dir(helper), false)
	if err != nil {
		return fmt.Errorf("open warm Docker tools directory: %w", err)
	}
	defer dir.Close()
	info, err := os.Lstat(helper)
	if err != nil {
		return fmt.Errorf("locate the warm Docker helper: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&022 != 0 || info.Mode().Perm()&0111 == 0 {
		return errors.New("warm Docker helper must be an executable regular file protected from other writers")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	actual, err := warmVolumeExecutableDigest(helper)
	if err != nil {
		return err
	}
	expected, err := warmVolumeExecutableDigest(executable)
	if err != nil {
		return err
	}
	if actual != expected {
		return errors.New("warm Docker helper must match the running Runner binary")
	}
	return nil
}

func warmVolumeExecutableDigest(path string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	file, err := os.Open(path)
	if err != nil {
		return out, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return out, err
	}
	hash.Sum(out[:0])
	return out, nil
}

func (d *Daemon) fenceWarmWorkspaceReturn(ctx context.Context, root string, checkProcesses func(context.Context, string) error) error {
	if d.warmDockerDesktop != nil {
		return d.warmDockerDesktop.Fence(ctx, root)
	}
	if err := runtime.FenceWarmWorkspaceContainers(ctx, root); err != nil {
		return err
	}
	return checkProcesses(ctx, root)
}

func (d *Daemon) handleWorkspaceBrowserEndpoint(ctx context.Context, request domain.RunnerWorkspaceRequest) (domain.BrowserState, error) {
	if d.warmVolumeMount == nil || request.WarmWorkspaceLease == nil {
		return runtime.HandleBrowser(ctx, request.RunID, *request.Browser)
	}
	root, err := d.runnerWorkspaceRoot(request)
	if err != nil {
		return domain.BrowserState{}, err
	}
	task, err := d.workspaceSandboxTask(root, request)
	if err != nil {
		return domain.BrowserState{}, err
	}
	requirements, _ := d.localTaskSandboxSpec(task)
	return runtime.HandleWarmDockerVolumeBrowser(ctx, request.RunID, root, requirements, *request.Browser)
}
