package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"agentflow/internal/domain"
)

type warmGitContainer struct {
	parent, workspace, image, user, network string
	requirements                            SandboxRequirements
	writable                                bool
}

// RunWarmGitOffline inspects a locally prepared frozen image by immutable ID.
// It cannot build the image, prepare the environment, or start a workflow container.
func RunWarmGitOffline(ctx context.Context, id, workspace string, requirements SandboxRequirements, dockerfile domain.DockerfileDefinition, input []byte, recoverOutputs bool) ([]byte, error) {
	if id == "" || len(requirements.GitObjectDirs) == 0 || strings.TrimSpace(dockerfile.Content) == "" {
		return nil, errors.New("warm Git requires a prepared Docker image and an allocated workspace")
	}
	image := isolatedRuntimeImage(dockerfile)
	if image == "" {
		image = cachedDockerImageName(dockerfile)
	}
	raw, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{json .}}", image).Output()
	if err != nil {
		return nil, fmt.Errorf("warm Git view requires the frozen Docker image to be prepared on this runner: %w", err)
	}
	var prepared struct {
		ID     string
		Config struct{ User string }
	}
	if err := json.Unmarshal(raw, &prepared); err != nil || prepared.ID == "" {
		return nil, errors.New("cannot identify prepared warm Git image")
	}
	return (warmGitContainer{parent: dockerContainerName(id), workspace: workspace, image: prepared.ID, user: dockerWorkspaceUser(prepared.Config.User), network: "none", requirements: requirements, writable: recoverOutputs}).run(ctx, input)
}

// Match the workspace owner's identity when the image defaults to root and
// Docker drops DAC capabilities. Explicit non-root image users are retained.
func dockerWorkspaceUser(configured string) string {
	user, _, _ := strings.Cut(strings.TrimSpace(configured), ":")
	if os.Geteuid() != 0 && (user == "" || user == "root" || user == "0") {
		return fmt.Sprintf("%d:%d", os.Geteuid(), os.Getegid())
	}
	return strings.TrimSpace(configured)
}

const warmGitExecutable = "/opt/loopforge/warm-git-helper"

// RunWarmGitHelper uses the qualified workspace image in a single-operation
// container. Killing the container also stops helpers and hooks after a timeout.
// Inspection is performed before stdin containing delivery credentials is sent.
func RunWarmGitHelper(ctx context.Context, sandbox Sandbox, input []byte) (reply []byte, resultErr error) {
	s, ok := sandbox.(*dockerSandbox)
	if !ok || len(s.requirements.GitObjectDirs) == 0 {
		return nil, errors.New("warm Git requires an isolated Docker workspace")
	}
	if err := s.ensureStarted(ctx); err != nil {
		return nil, err
	}
	s.execGate.RLock()
	defer s.execGate.RUnlock()
	if err := s.verifyWarmDocker(ctx); err != nil {
		return nil, err
	}
	var parent struct {
		Image      string
		Config     struct{ User string }
		HostConfig struct{ NetworkMode string }
	}
	inspect, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", "--format", "{{json .}}", s.name).Output()
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(inspect, &parent); err != nil || parent.Image == "" {
		return nil, errors.New("cannot identify warm Git sandbox image")
	}
	return (warmGitContainer{parent: s.name, workspace: s.workspace, image: parent.Image, user: parent.Config.User, network: parent.HostConfig.NetworkMode, requirements: s.requirements, writable: true}).run(ctx, input)
}

func (target warmGitContainer) run(ctx context.Context, input []byte) (reply []byte, resultErr error) {
	helper, mounts, err := target.pathMounts()
	if err != nil {
		return nil, err
	}
	name := target.parent + "-git-" + rand.Text()
	args := []string{"create", "--name", name, "--label", "io.loopforge.warm-git-parent=" + target.parent, "--pull=never", "--init", "--interactive", "--cap-drop=ALL", "--security-opt", "no-new-privileges", "--network", target.network, "-w", "/workspace"}
	args = append(args, mounts...)
	if target.user != "" {
		args = append(args, "--user", target.user)
	}
	binds, err := dockerGitObjectMounts(target.requirements)
	if err != nil {
		return nil, err
	}
	args = append(args, binds...)
	args = append(args, "--entrypoint", warmGitExecutable, target.image, "warm-git-helper")
	// Volume helpers require the create receipt and matching inspected mount
	// contract. The release fence handles an unacknowledged helper.
	cleanupTarget := name
	volume := target.requirements.WarmDockerVolume
	if volume != nil {
		cleanupTarget = ""
	}
	defer func() {
		if cleanupTarget == "" {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		resultErr = errors.Join(resultErr, removeDockerContainer(cleanup, cleanupTarget))
	}()
	created, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("create warm Git helper: %w", err)
	}
	startTarget := name
	if volume != nil {
		startTarget = strings.TrimSpace(string(created))
		if !dockerImmutableID(startTarget) {
			return nil, errors.New("invalid warm Git helper create acknowledgement")
		}
	}
	raw, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", "--format", "{{json .}}", startTarget).Output()
	if err != nil {
		return nil, err
	}
	var actual warmDockerInspection
	if err = json.Unmarshal(raw, &actual); err != nil {
		return nil, err
	}
	if actual.HostConfig.NetworkMode != target.network {
		return nil, errors.New("unexpected warm Git network")
	}
	if err = actual.validateHost(); err != nil {
		return nil, err
	}
	if err = actual.validateWarmPaths(target.requirements.WarmDockerVolume, map[string]string{"/workspace": target.workspace, warmGitExecutable: helper}, target.requirements.GitObjectDirs, "", target.writable); err != nil {
		return nil, err
	}
	if volume != nil {
		if actual.ID != startTarget || actual.Name != "/"+name || actual.Config.Labels["io.loopforge.warm-git-parent"] != target.parent || actual.Config.Image != target.image {
			return nil, errors.New("unexpected warm Git helper identity")
		}
		cleanupTarget = startTarget
	}
	cmd := exec.CommandContext(ctx, "docker", "start", "-ai", startTarget)
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	const max = 20 << 20
	response, readErr := io.ReadAll(io.LimitReader(output, max+1))
	output.Close()
	if len(response) > max || readErr != nil {
		_ = cmd.Process.Kill()
	}
	if err = cmd.Wait(); err != nil {
		return nil, fmt.Errorf("run isolated Git operation: %w", err)
	}
	if readErr != nil || len(response) > max || len(strings.TrimSpace(string(response))) == 0 {
		return nil, errors.New("invalid isolated Git response")
	}
	return response, nil
}
