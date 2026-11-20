package runtime

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

type desktopConversationScope struct {
	workRoot          string
	authorizedObjects func(string) ([]string, bool)
}

// SetConversationScope supplies the runner-local work root and live journal
// lookup before the first Ready call. A container cannot grant itself a lease.
func (d *WarmDockerDesktop) SetConversationScope(workspaceRoot string, authorizedObjects func(string) ([]string, bool)) error {
	if d.conversations != nil || authorizedObjects == nil || workspaceRoot == d.root || !desktopBelow(d.root, workspaceRoot) || filepath.Clean(workspaceRoot) != workspaceRoot {
		return errors.New("invalid Docker Desktop conversation scope")
	}
	d.conversations = &desktopConversationScope{workspaceRoot, authorizedObjects}
	return nil
}

func (d *WarmDockerDesktop) hasVolume(c desktopContainer) bool {
	return slices.ContainsFunc(c.Mounts, func(m desktopMount) bool { return m.Type == "volume" && m.Name == d.volume })
}

func (d *WarmDockerDesktop) leaseObjects(workspace string) ([]string, bool) {
	if d.conversations == nil || filepath.Clean(workspace) != workspace {
		return nil, false
	}
	if filepath.Join(d.conversations.workRoot, "runs", filepath.Base(filepath.Dir(workspace)), "workspace") != workspace {
		return nil, false
	}
	return d.conversations.authorizedObjects(workspace)
}

func (d *WarmDockerDesktop) verifyConversation(c desktopContainer, actual *warmDockerInspection) error {
	if d.conversations == nil || actual == nil || !desktopContainerID.MatchString(c.ID) || actual.ID != c.ID || actual.Name != c.Name {
		return errors.New("unverified Docker Desktop conversation identity")
	}
	root := c.Config.Labels[warmConversationRootLabel]
	workspace := c.Config.Labels["io.loopforge.warm-workspace"]
	if actual.Config.Labels[warmConversationRootLabel] != root || actual.Config.Labels["io.loopforge.warm-workspace"] != workspace {
		return errors.New("Docker Desktop conversation identity changed")
	}
	sessionKey := filepath.Base(root)
	base := filepath.Join(d.conversations.workRoot, ".conversations")
	transient := filepath.Dir(root) == filepath.Join(base, ".transient")
	if transient {
		base = filepath.Join(base, ".transient")
	}
	if !desktopContainerID.MatchString(sessionKey) || root != filepath.Join(base, sessionKey) || c.Name != "/"+dockerContainerName(WarmDockerConversationID(sessionKey, transient)) {
		return errors.New("invalid Docker Desktop session root or container name")
	}
	req := SandboxRequirements{ConversationRoot: root, WarmDockerVolume: &WarmDockerVolume{Name: d.volume, Root: d.root, Backing: d.backing}}
	if workspace != filepath.Join(root, "ws") {
		objects, ok := d.leaseObjects(workspace)
		if transient || !ok || len(objects) == 0 {
			return errors.New("conversation workspace has no placed warm lease")
		}
		req.GitObjectDirs = objects
	}
	return actual.validateTask(workspace, req, RuntimeCLIDir())
}

// The fence is an observer. Labels and mount requests describe scope, but
// only the creating component has the receipt needed to remove a container.
func (d *WarmDockerDesktop) checkVolumeClient(ctx context.Context, c desktopContainer, workspace string) error {
	if workspace != "" {
		overlap, err := d.containerOverlaps(c, workspace)
		if err != nil {
			return err
		}
		if overlap {
			return fmt.Errorf("container %s still has a workspace mount", c.Name)
		}
		return nil
	}
	if _, conversation := c.Config.Labels[warmConversationRootLabel]; conversation && d.hasVolume(c) {
		actual, err := inspectWarmDocker(ctx, c.ID)
		if err != nil || actual == nil {
			return err
		}
		return d.verifyConversation(c, actual)
	}
	return d.checkDedicated(c)
}

// Other volume clients have no access to private conversation state. A
// read-only mount outside /workspace must be a local tool or a leased Git
// object directory, and it must appear at its corresponding destination.
func (d *WarmDockerDesktop) checkCarrier(c desktopContainer) error {
	if c.ID == d.id || !d.hasVolume(c) {
		return nil
	}
	var workspace string
	for _, m := range c.Mounts {
		if m.Destination == "/workspace" && m.Type == "volume" && m.Name == d.volume {
			sub, err := desktopVolumeSubpath(c, m)
			if err != nil {
				return err
			}
			workspace = filepath.Join(d.root, sub)
		}
	}
	objects, ok := d.leaseObjects(workspace)
	if !ok || !warmContainerOwnsWorkspace(c.Name, c.Config.Labels, workspace) {
		return errors.New("warm volume client lacks a placed lease")
	}
	cli := RuntimeCLIDir()
	for _, m := range c.Mounts {
		if m.Type == "tmpfs" && m.Destination == "/tmp" {
			continue
		}
		if m.Type != "volume" || m.Name != d.volume || m.Source != d.backing {
			return errors.New("unverified external mount in warm volume client")
		}
		sub, err := desktopVolumeSubpath(c, m)
		if err != nil {
			return err
		}
		source := filepath.Join(d.root, sub)
		switch {
		case m.Destination == "/workspace" && source == workspace:
		case !m.RW && m.Destination == source && (source == cli || slices.Contains(objects, source)):
		case !m.RW && m.Destination == warmGitExecutable && source == filepath.Join(d.root, "tools", "agentflow-runner") && strings.HasPrefix(c.Config.Labels["io.loopforge.warm-git-parent"], dockerSandboxPrefix):
		default:
			return errors.New("unexpected private volume mount in workspace client")
		}
	}
	return nil
}
