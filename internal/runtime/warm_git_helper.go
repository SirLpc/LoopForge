package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type WarmGitAccess struct{ WriteWorkspace, Network bool }

const warmGitExecutable = "/opt/loopforge/warm-git-helper"

// RunWarmGitHelper uses the qualified workspace image in a single-operation
// container. Killing the container also stops helpers and hooks after a timeout.
// Inspection is performed before stdin containing delivery credentials is sent.
func RunWarmGitHelper(ctx context.Context, sandbox Sandbox, input []byte, access WarmGitAccess) (reply []byte, resultErr error) {
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
	helper, err := linuxWorkspaceHelper()
	if err != nil {
		return nil, err
	}
	name := s.name + "-git-" + rand.Text()
	network := "none"
	workspaceMount := s.workspace + ":/workspace:ro"
	if access.Network {
		network = parent.HostConfig.NetworkMode
	}
	if access.WriteWorkspace {
		workspaceMount = s.workspace + ":/workspace"
	}
	args := []string{"create", "--name", name, "--label", "io.loopforge.warm-git-parent=" + s.name, "--pull=never", "--init", "--interactive", "--cap-drop=ALL", "--security-opt", "no-new-privileges", "--network", network, "-w", "/workspace", "-v", workspaceMount, "-v", helper + ":" + warmGitExecutable + ":ro"}
	if parent.Config.User != "" {
		args = append(args, "--user", parent.Config.User)
	}
	binds, err := dockerGitObjectMounts(s.requirements)
	if err != nil {
		return nil, err
	}
	args = append(args, binds...)
	args = append(args, "--entrypoint", warmGitExecutable, parent.Image, "warm-git-helper")
	// Even if the CLI loses its create acknowledgement, remove the unique name.
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		resultErr = errors.Join(resultErr, removeDockerContainer(cleanup, name))
	}()
	if _, err = exec.CommandContext(ctx, "docker", args...).Output(); err != nil {
		return nil, fmt.Errorf("create warm Git helper: %w", err)
	}
	raw, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", "--format", "{{json .}}", name).Output()
	if err != nil {
		return nil, err
	}
	var actual warmDockerInspection
	if err = json.Unmarshal(raw, &actual); err != nil {
		return nil, err
	}
	if err = actual.validateHost(); err != nil {
		return nil, err
	}
	if err = actual.validateMounts(map[string]string{"/workspace": s.workspace, warmGitExecutable: helper}, s.requirements.GitObjectDirs, "", access.WriteWorkspace); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "docker", "start", "-ai", name)
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
