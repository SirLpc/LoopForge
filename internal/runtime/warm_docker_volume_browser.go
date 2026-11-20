package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// Apply the normal host isolation checks after verifying the browser's single
// exception: its network namespace belongs to the task.
func (i warmDockerInspection) validateBrowserHost(taskNetwork string) error {
	if i.HostConfig.NetworkMode != taskNetwork {
		return errors.New("warm browser does not share its verified task network")
	}
	i.HostConfig.NetworkMode = ""
	return i.validateHost()
}

// Bind the browser to the task's inspected network namespace by immutable ID.
// Inspect newly created and previously stopped sidecars before allowing a start.
func (s *dockerSandbox) ensureWarmVolumeBrowser(ctx context.Context, runID, image, name string) error {
	parent, err := inspectWarmDocker(ctx, s.name)
	if err != nil {
		return err
	}
	if parent == nil || parent.ID == "" || !parent.State.Running {
		return errors.New("warm browser requires a running task sandbox")
	}
	v := s.requirements.WarmDockerVolume
	if err := parent.validateTask(s.workspace, s.requirements, RuntimeCLIDir()); err != nil {
		return err
	}
	network := "container:" + parent.ID
	actual, err := inspectWarmDocker(ctx, name)
	if err != nil {
		return err
	}
	if actual == nil {
		mount, err := dockerPathMount(v, s.workspace, "/workspace", false)
		if err != nil {
			return err
		}
		args := []string{"create", "--pull=never", "--name", name, "--init",
			"--label", browserRunLabel + "=" + runID, "--label", browserOwnerLabel + "=" + s.name, "--label", "io.loopforge.warm-workspace=" + s.workspace,
			"--network", network, "--cap-drop=ALL", "--security-opt", "no-new-privileges", "--shm-size", "512m", "-w", "/workspace",
			"-e", "AGENTFLOW_BROWSER_OUTPUT=/workspace/.agentflow/browser", "-e", "AGENTFLOW_BROWSER_SCALE=" + BrowserDeviceScale()}
		args = append(args, mount...)
		if out, err := exec.CommandContext(ctx, "docker", append(args, image)...).CombinedOutput(); err != nil {
			return fmt.Errorf("create warm browser: %w: %s", err, tailOutput(out))
		}
		actual, err = inspectWarmDocker(ctx, name)
		if err != nil {
			return err
		}
	}
	if actual == nil || actual.ID == "" || actual.Config.Image != image ||
		actual.Config.Labels[browserRunLabel] != runID || actual.Config.Labels[browserOwnerLabel] != s.name ||
		actual.Config.Labels["io.loopforge.warm-workspace"] != s.workspace {
		return errors.New("unexpected warm browser container identity")
	}
	if err := actual.validateBrowserHost(network); err != nil {
		return err
	}
	if err := actual.validateVolumeMounts(v, map[string]string{"/workspace": s.workspace}, true); err != nil {
		return err
	}
	if !actual.State.Running {
		if out, err := exec.CommandContext(ctx, "docker", "start", actual.ID).CombinedOutput(); err != nil {
			return fmt.Errorf("start warm browser: %w: %s", err, tailOutput(out))
		}
	}
	return nil
}
