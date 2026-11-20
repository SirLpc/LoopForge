package runtime

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Docker Desktop resolves paths in the daemon VM. Only a subpath of the
// runner's qualified volume denotes the same file on both sides.
func (v *WarmDockerVolume) subpath(source string) (string, error) {
	if v.Name == "" || !filepath.IsAbs(v.Backing) || !filepath.IsAbs(v.Root) || v.Root == "/" ||
		filepath.Clean(v.Root) != v.Root || !filepath.IsAbs(source) || filepath.Clean(source) != source {
		return "", errors.New("warm Docker mount requires a qualified volume and canonical absolute paths")
	}
	rel, err := filepath.Rel(v.Root, source)
	if err != nil || !filepath.IsLocal(rel) || rel == "." {
		return "", fmt.Errorf("warm Docker mount %q is outside the dedicated volume or exposes its root", source)
	}
	resolved, err := filepath.EvalSymlinks(source)
	if err != nil {
		return "", fmt.Errorf("resolve warm Docker mount %q: %w", source, err)
	}
	if resolved != source {
		return "", fmt.Errorf("warm Docker mount %q contains a symlink", source)
	}
	return filepath.ToSlash(rel), nil
}

func dockerPathMount(v *WarmDockerVolume, source, destination string, readOnly bool) ([]string, error) {
	// Preserve the original writable workspace bind for ordinary Docker tasks.
	if v == nil && destination == "/workspace" && !readOnly {
		return []string{"-v", source + ":/workspace"}, nil
	}
	fields := []string{"type=bind", "src=" + source, "dst=" + destination}
	if v != nil {
		subpath, err := v.subpath(source)
		if err != nil {
			return nil, err
		}
		fields = []string{"type=volume", "src=" + v.Name, "dst=" + destination, "volume-subpath=" + subpath, "volume-nocopy"}
	}
	if readOnly {
		fields = append(fields, "readonly")
	}
	var mount strings.Builder
	writer := csv.NewWriter(&mount)
	if err := writer.Write(fields); err != nil {
		return nil, err
	}
	writer.Flush()
	return []string{"--mount", strings.TrimSuffix(mount.String(), "\n")}, writer.Error()
}

type warmDockerMountRequest struct {
	Type, Source, Target string
	ReadOnly             bool
	VolumeOptions        *struct {
		Subpath string
		NoCopy  bool
	}
}

func (i warmDockerInspection) validateVolumeRequests(v *WarmDockerVolume, allowed map[string]string, workspaceWritable bool) error {
	if len(i.HostConfig.Binds) != 0 || len(i.HostConfig.Mounts) != len(allowed) {
		return errors.New("unexpected mount requests for the warm Docker volume")
	}
	for dst := range i.HostConfig.Tmpfs {
		if dst != "/tmp" {
			return fmt.Errorf("unexpected warm Docker tmpfs at %s", dst)
		}
	}
	seen := make(map[string]bool, len(allowed))
	for _, m := range i.HostConfig.Mounts {
		source, ok := allowed[m.Target]
		if !ok || seen[m.Target] {
			return fmt.Errorf("unexpected warm Docker mount request at %s", m.Target)
		}
		seen[m.Target] = true
		subpath, err := v.subpath(source)
		if err != nil {
			return err
		}
		wantReadOnly := m.Target != "/workspace" || !workspaceWritable
		if m.Type != "volume" || m.Source != v.Name || m.VolumeOptions == nil ||
			m.VolumeOptions.Subpath != subpath || !m.VolumeOptions.NoCopy || m.ReadOnly != wantReadOnly {
			return fmt.Errorf("incorrect warm Docker volume request at %s", m.Target)
		}
	}
	return nil
}

func (i warmDockerInspection) validateTask(workspace string, requirements SandboxRequirements, cli string) error {
	if requirements.WarmDockerVolume == nil {
		return i.validate(workspace, requirements.GitObjectDirs, cli)
	}
	if err := i.validateHost(); err != nil {
		return err
	}
	return i.validateWarmPaths(requirements.WarmDockerVolume, map[string]string{"/workspace": workspace}, requirements.GitObjectDirs, cli, true)
}

func (i warmDockerInspection) validateWarmPaths(v *WarmDockerVolume, allowed map[string]string, objects []string, cli string, writable bool) error {
	if v == nil {
		return i.validateMounts(allowed, objects, cli, writable)
	}
	for _, path := range objects {
		allowed[path] = path
	}
	if cli != "" {
		allowed[cli] = cli
	}
	return i.validateVolumeMounts(v, allowed, writable)
}

func (i warmDockerInspection) validateVolumeMounts(v *WarmDockerVolume, allowed map[string]string, writable bool) error {
	if err := i.validateVolumeRequests(v, allowed, writable); err != nil {
		return err
	}
	for _, m := range i.Mounts {
		if m.Type == "tmpfs" && m.Destination == "/tmp" {
			continue
		}
		_, ok := allowed[m.Destination]
		private := m.Propagation == "" || m.Propagation == "private" || m.Propagation == "rprivate"
		if !ok || m.Type != "volume" || m.Name != v.Name || m.Source != v.Backing ||
			m.RW != (writable && m.Destination == "/workspace") || !private {
			return fmt.Errorf("unexpected warm Docker volume mount at %s", m.Destination)
		}
		delete(allowed, m.Destination)
	}
	if len(allowed) != 0 {
		return errors.New("warm Docker volume is missing expected mounts")
	}
	return nil
}

func (target warmGitContainer) pathMounts() (string, []string, error) {
	v := target.requirements.WarmDockerVolume
	if v == nil {
		helper, err := linuxWorkspaceHelper()
		if err != nil {
			return "", nil, err
		}
		workspace := target.workspace + ":/workspace"
		if !target.writable {
			workspace += ":ro"
		}
		return helper, []string{"-v", workspace, "-v", helper + ":" + warmGitExecutable + ":ro"}, nil
	}
	workspace, err := dockerPathMount(v, target.workspace, "/workspace", !target.writable)
	if err != nil {
		return "", nil, err
	}
	helper, err := dockerPathMount(v, v.Helper, warmGitExecutable, true)
	if err != nil {
		return "", nil, err
	}
	return v.Helper, append(workspace, helper...), nil
}

func dockerRuntimeCLIMount(v *WarmDockerVolume, dir string) ([]string, error) {
	if v == nil {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("prepare runtime CLI directory: %w", err)
		}
	}
	return dockerPathMount(v, dir, dir, true)
}

func (s *dockerSandbox) canReuseDockerMounts(ctx context.Context, mounts *dockerWorkspaceMounts, recreate bool) (bool, error) {
	if s.requirements.WarmDockerVolume != nil {
		// Check ownership before restarting or replacing any existing container.
		return !recreate, s.verifyWarmDocker(ctx)
	}
	if !dockerBindMatchesWorkspace(mounts.source, s.workspace) {
		return false, fmt.Errorf("docker sandbox %s is bound to %s, not %s", s.name, mounts.source, s.workspace)
	}
	for _, objects := range s.requirements.GitObjectDirs {
		if !dockerBindMatchesWorkspace(mounts.readOnlyObjects[objects], objects) {
			return false, nil
		}
	}
	if recreate || mounts.legacyMasks {
		return false, nil
	}
	return true, s.verifyWarmDocker(ctx)
}
