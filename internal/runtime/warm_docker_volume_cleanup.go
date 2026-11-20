package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

func dockerImmutableID(id string) bool {
	if len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// Validate the task's witnessed identity as well as every effective mount.
// A process restarted without its creation receipt leaves the holder retained.
func (s *dockerSandbox) inspectVolumeTask(ctx context.Context, ref, knownID string) (*warmDockerInspection, error) {
	actual, err := inspectWarmDocker(ctx, ref)
	if err != nil || actual == nil {
		return actual, err
	}
	image := isolatedRuntimeImage(s.dockerfile)
	if image == "" {
		image = cachedDockerImageName(s.dockerfile)
	}
	if !dockerImmutableID(knownID) || actual.ID != knownID || actual.Name != "/"+s.name ||
		actual.Config.Image != image || actual.Config.Labels["io.loopforge.warm-workspace"] != s.workspace {
		return nil, errors.New("warm Docker task identity changed")
	}
	if err := actual.validateTask(s.workspace, s.requirements, RuntimeCLIDir()); err != nil {
		return nil, err
	}
	return actual, nil
}

// Called with s.mu held. The daemon's immutable ID is used for both starting
// the task and removing it after a failed start or readiness check.
func (s *dockerSandbox) createdStartTarget(ctx context.Context, created []byte) (string, error) {
	if s.requirements.WarmDockerVolume == nil {
		return s.name, s.verifyWarmDocker(ctx)
	}
	id := strings.TrimSpace(string(created))
	if !dockerImmutableID(id) {
		return "", errors.New("invalid warm Docker create acknowledgement")
	}
	return s.volumeStartTarget(ctx, id, id)
}

func (s *dockerSandbox) volumeStartTarget(ctx context.Context, ref, known string) (string, error) {
	actual, err := s.inspectVolumeTask(ctx, ref, known)
	if err != nil {
		return "", err
	}
	if actual == nil {
		return "", errors.New("warm Docker task disappeared")
	}
	s.warmTaskID = actual.ID
	return actual.ID, nil
}

func (s *dockerSandbox) removeTaskContainers(ctx context.Context) error {
	if s.requirements.WarmDockerVolume == nil {
		return removeDockerWorkspaceContainers(ctx, s.name)
	}
	ctx, cancel := context.WithTimeout(ctx, dockerTeardownTimeout)
	defer cancel()
	s.browserMu.Lock()
	defer s.browserMu.Unlock()

	// Volume-mode runs never create a legacy "-scripts" companion. Leave all
	// unexpected objects for the independent all-container release fence.
	task, err := s.inspectVolumeTask(ctx, s.name, s.warmTaskID)
	if err != nil {
		return err
	}
	browser, err := inspectWarmDocker(ctx, dockerBrowserContainerName(s.name))
	if err != nil {
		return err
	}
	if browser != nil {
		if task == nil || browser.ID != s.warmBrowserID || !dockerImmutableID(browser.ID) || browser.Name != "/"+dockerBrowserContainerName(s.name) || s.warmBrowserRunID == "" {
			return errors.New("warm Docker browser ownership is unverified")
		}
		image, err := browserImageName()
		if err != nil {
			return err
		}
		if err := browser.validateWarmBrowser(s.warmBrowserRunID, image, s.name, s.workspace, "container:"+task.ID, s.requirements.WarmDockerVolume); err != nil {
			return err
		}
		if err := removeDockerContainer(ctx, browser.ID); err != nil {
			return fmt.Errorf("remove verified warm browser: %w", err)
		}
	}
	if task != nil {
		if err := removeDockerContainer(ctx, task.ID); err != nil {
			return err
		}
	}
	s.requirements.WarmDockerVolume.browserParents.Delete(s.warmBrowserID)
	s.warmTaskID, s.warmBrowserID, s.warmBrowserRunID = "", "", ""
	return nil
}

// executionTarget returns the container generation already verified at start.
// Native Docker keeps its ordinary name-based lifecycle.
func (s *dockerSandbox) executionTarget() (string, error) {
	if s.requirements.WarmDockerVolume == nil {
		return s.name, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !dockerImmutableID(s.warmTaskID) {
		return "", errors.New("warm Docker container has no verified execution target")
	}
	return s.warmTaskID, nil
}
