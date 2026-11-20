package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// A helper can outlive both its creating process and its parent container.
// Inspect all container mounts, including stopped containers that could restart.
func FenceWarmWorkspaceContainers(ctx context.Context, workspace string) error {
	for pass := 0; pass < 2; pass++ {
		raw, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--no-trunc").Output()
		if err != nil {
			return fmt.Errorf("cannot verify workspace containers: %w", err)
		}
		for _, id := range strings.Fields(string(raw)) {
			out, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", "--format", `{"Name":{{json .Name}},"Config":{"Labels":{{json .Config.Labels}}},"Mounts":{{json .Mounts}}}`, id).CombinedOutput()
			if dockerObjectGone(string(out), err) {
				continue
			}
			if err != nil {
				return err
			}
			var info struct {
				Name   string
				Config struct{ Labels map[string]string }
				Mounts []warmDockerMount
			}
			if err = json.Unmarshal(out, &info); err != nil {
				return err
			}
			mounted, owns := false, false
			for _, m := range info.Mounts {
				if m.Type != "bind" {
					continue
				}
				below, err := filepath.Rel(workspace, m.Source)
				above, e := filepath.Rel(m.Source, workspace)
				if err != nil || e != nil {
					return errors.New("cannot identify workspace mount")
				}
				if filepath.IsLocal(below) || filepath.IsLocal(above) {
					mounted = true
				}
				if m.Source == workspace && m.Destination == "/workspace" {
					owns = warmContainerOwnsWorkspace(info.Name, info.Config.Labels, workspace)
				}
			}
			if !mounted {
				continue
			}
			if !owns || pass != 0 {
				return fmt.Errorf("container %s still has a workspace mount", info.Name)
			}
			if err := removeDockerContainer(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func warmContainerOwnsWorkspace(name string, labels map[string]string, workspace string) bool {
	parent := labels["io.loopforge.warm-git-parent"]
	return labels["io.loopforge.warm-workspace"] == workspace || strings.HasPrefix(parent, dockerSandboxPrefix) && strings.HasPrefix(strings.TrimPrefix(name, "/"), parent+"-git-")
}
