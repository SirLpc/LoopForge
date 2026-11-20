package runtime

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"agentflow/internal/domain"
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
	parent, err := s.inspectVolumeTask(ctx, s.name, "")
	if err != nil {
		return err
	}
	if parent == nil || parent.ID == "" || !parent.State.Running {
		return errors.New("warm browser requires a running task sandbox")
	}
	v := s.requirements.WarmDockerVolume
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
		created, err := exec.CommandContext(ctx, "docker", append(args, image)...).Output()
		if err != nil {
			return fmt.Errorf("create warm browser: %w", err)
		}
		id := strings.TrimSpace(string(created))
		if !dockerImmutableID(id) {
			return errors.New("invalid warm browser create acknowledgement")
		}
		actual, err = inspectWarmDocker(ctx, id)
		if err != nil {
			return err
		}
		if actual == nil || actual.ID != id {
			return errors.New("warm browser identity changed after creation")
		}
	} else if s.warmBrowserID != "" && actual.ID != s.warmBrowserID {
		return errors.New("warm browser identity changed")
	}
	if err := actual.validateWarmBrowser(runID, image, s.name, s.workspace, network, v); err != nil {
		return err
	}
	s.warmBrowserID, s.warmBrowserRunID = actual.ID, runID
	if !actual.State.Running {
		if out, err := exec.CommandContext(ctx, "docker", "start", actual.ID).CombinedOutput(); err != nil {
			return fmt.Errorf("start warm browser: %w: %s", err, tailOutput(out))
		}
	}
	return nil
}

func (actual *warmDockerInspection) validateWarmBrowser(runID, image, owner, workspace, network string, v *WarmDockerVolume) error {
	if actual == nil || !dockerImmutableID(actual.ID) || actual.Name != "/"+dockerBrowserContainerName(owner) || actual.Config.Image != image ||
		actual.Config.Labels[browserRunLabel] != runID || actual.Config.Labels[browserOwnerLabel] != owner ||
		actual.Config.Labels["io.loopforge.warm-workspace"] != workspace {
		return errors.New("unexpected warm browser container identity")
	}
	if err := actual.validateBrowserHost(network); err != nil {
		return err
	}
	return actual.validateVolumeMounts(v, map[string]string{"/workspace": workspace}, true)
}

// HandleWarmDockerVolumeBrowser pins every panel operation, including file and
// dev-server requests, to the inspected sidecar. Missing or ambiguous ownership
// cannot fall back to a host browser or an earlier run-label lookup.
func HandleWarmDockerVolumeBrowser(ctx context.Context, runID, workspace string, requirements SandboxRequirements, req domain.BrowserRequest) (domain.BrowserState, error) {
	v := requirements.WarmDockerVolume
	if v == nil || runID == "" {
		return domain.BrowserState{}, errors.New("a qualified warm Docker browser is required")
	}
	raw, err := exec.CommandContext(ctx, "docker", "ps", "--no-trunc", "--filter", "label="+browserRunLabel+"="+runID, "--format", "{{.ID}}").Output()
	if err != nil {
		return domain.BrowserState{}, fmt.Errorf("list warm browser containers: %w", err)
	}
	ids := strings.Fields(string(raw))
	if len(ids) == 0 {
		return domain.BrowserState{Reason: "预热工作区浏览器尚未启动，请先启动任务浏览器"}, nil
	}
	if len(ids) != 1 {
		return domain.BrowserState{}, errors.New("multiple browser containers claim this run")
	}
	browser, err := inspectWarmDocker(ctx, ids[0])
	if err != nil {
		return domain.BrowserState{}, err
	}
	if browser == nil || browser.ID != ids[0] || !browser.State.Running {
		return domain.BrowserState{}, errors.New("warm browser identity changed")
	}
	owner := browser.Config.Labels[browserOwnerLabel]
	parentID, shared := strings.CutPrefix(browser.HostConfig.NetworkMode, "container:")
	if !shared || parentID == "" || !strings.HasPrefix(owner, dockerSandboxPrefix) || browser.Name != "/"+dockerBrowserContainerName(owner) {
		return domain.BrowserState{}, errors.New("unexpected warm browser owner")
	}
	parent, err := inspectWarmDocker(ctx, parentID)
	if err != nil {
		return domain.BrowserState{}, err
	}
	if parent == nil || parent.ID != parentID || parent.Name != "/"+owner || !parent.State.Running || parent.Config.Labels["io.loopforge.warm-workspace"] != workspace {
		return domain.BrowserState{}, errors.New("unexpected warm browser task identity")
	}
	if err := parent.validateTask(workspace, requirements, RuntimeCLIDir()); err != nil {
		return domain.BrowserState{}, err
	}
	image, err := browserImageName()
	if err != nil {
		return domain.BrowserState{}, err
	}
	if err := browser.validateWarmBrowser(runID, image, owner, workspace, "container:"+parent.ID, v); err != nil {
		return domain.BrowserState{}, err
	}

	endpoint := browserEndpoint{Kind: "docker", Container: browser.ID}
	ctx = context.WithValue(ctx, verifiedBrowserEndpointKey{}, endpoint)
	return HandleBrowser(ctx, runID, req)
}

type verifiedBrowserEndpointKey struct{}

func requestBrowserEndpoint(ctx context.Context, runID string, lookup func(context.Context, string) (browserEndpoint, bool)) (browserEndpoint, bool) {
	if pinned, ok := ctx.Value(verifiedBrowserEndpointKey{}).(browserEndpoint); ok {
		return pinned, true
	}
	return lookup(ctx, runID)
}

func browserEndpointMatchesRequest(ctx context.Context, cached browserEndpoint) bool {
	pinned, ok := ctx.Value(verifiedBrowserEndpointKey{}).(browserEndpoint)
	return !ok || cached == pinned
}

// Keep old CDP sessions and pooled HTTP connections tied to their container.
// A NUL cannot occur in an ordinary runner workspace path or Docker argument.
func browserRequestCacheKey(ctx context.Context, runID string) string {
	if ep, ok := ctx.Value(verifiedBrowserEndpointKey{}).(browserEndpoint); ok {
		return runID + "\x00" + ep.Container
	}
	return runID
}
