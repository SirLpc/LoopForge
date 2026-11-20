package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type warmVolumePath struct {
	Source   string
	Writable bool
}

// Derive the independently checkable mount contract without creating session
// state. Each writable anchor prevents renaming a parent of a guarded file.
func warmConversationMounts(root, workspace string) (map[string]warmVolumePath, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
		return nil, errors.New("invalid warm conversation paths")
	}
	key := filepath.Base(root)
	sessions := filepath.Dir(root)
	if filepath.Base(sessions) == ".transient" {
		sessions = filepath.Dir(sessions)
	}
	if filepath.Base(sessions) != ".conversations" || !dockerImmutableID(key) || key != strings.ToLower(key) {
		return nil, errors.New("invalid warm conversation session root")
	}
	ws := filepath.Join(root, "ws")
	if workspace != ws {
		runs := filepath.Join(filepath.Dir(sessions), "runs")
		if filepath.Dir(filepath.Dir(workspace)) != runs || filepath.Base(workspace) != "workspace" {
			return nil, errors.New("invalid warm conversation checkout")
		}
	}
	home := filepath.Join(root, "home")
	mounts := map[string]warmVolumePath{
		"/workspace": {workspace, true},
		root:         {root, false},
		home:         {home, true},
		ws:           {workspace, true},
	}
	for _, rel := range conversationGuardFiles {
		file := filepath.Join(root, filepath.FromSlash(rel))
		mounts[file] = warmVolumePath{file, false}
		for parent := filepath.Dir(file); parent != home; parent = filepath.Dir(parent) {
			mounts[parent] = warmVolumePath{parent, true}
		}
	}
	return mounts, nil
}

func warmVolumeConversationArgs(v *WarmDockerVolume, root, workspace string) ([]string, error) {
	expected, err := warmConversationMounts(root, workspace)
	if err != nil {
		return nil, err
	}
	if err := v.checkConversationAlias(root); err != nil {
		return nil, err
	}
	if _, err := v.subpath(workspace); err != nil {
		return nil, err
	}
	if err := provisionWarmConversationGuards(root); err != nil {
		return nil, err
	}

	args := []string{"--label", warmConversationRootLabel + "=" + root}
	// The task already supplies /workspace. Emit the nested mounts in path
	// order, starting with the read-only session root.
	delete(expected, "/workspace")
	for _, dst := range slices.Sorted(maps.Keys(expected)) {
		m := expected[dst]
		bind, err := dockerPathMount(v, m.Source, dst, !m.Writable)
		if err != nil {
			return nil, err
		}
		args = append(args, bind...)
	}
	return args, nil
}

func provisionWarmConversationGuards(path string) error {
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, rel := range conversationGuardFiles {
		file := filepath.FromSlash(rel)
		if err := rejectWorkspaceSymlinks(root, file); err != nil {
			return err
		}
		if err := root.MkdirAll(filepath.Dir(file), 0700); err != nil {
			return err
		}
		f, err := root.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if os.IsExist(err) {
			info, err := root.Lstat(file)
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("invalid conversation guard %q", rel)
			}
			continue
		}
		if err != nil {
			return err
		}
		_, writeErr := f.WriteString("{}")
		if err := errors.Join(writeErr, f.Close()); err != nil {
			return err
		}
	}
	return nil
}

// The destination of the checkout alias exists inside the session-root mount.
// A borrowed checkout has a different source, so validate this destination too.
func (v *WarmDockerVolume) checkConversationAlias(root string) error {
	dst := filepath.Join(root, "ws")
	if _, err := v.subpath(dst); err != nil {
		return err
	}
	info, err := os.Stat(dst)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("invalid warm conversation workspace alias")
	}
	return nil
}

// CLI preparation can be the first operation on a text-only sandbox. Pin all
// three docker exec calls, especially the installer, to its verified ID.
func (s *dockerSandbox) conversationCLIContainer(ctx context.Context, home string) (string, error) {
	if s.requirements.WarmDockerVolume == nil {
		return s.name, nil
	}
	if s.requirements.ConversationRoot == "" || home != filepath.Join(s.requirements.ConversationRoot, "home") {
		return "", errors.New("warm CLI setup requires the private conversation HOME")
	}
	if err := s.ensureStarted(ctx); err != nil {
		return "", err
	}
	id, err := s.executionTarget()
	if err != nil {
		return "", err
	}
	actual, err := s.inspectVolumeTask(ctx, id, id)
	if err != nil {
		return "", err
	}
	if actual == nil || !actual.State.Running {
		return "", errors.New("warm conversation container stopped before CLI setup")
	}
	return id, nil
}
