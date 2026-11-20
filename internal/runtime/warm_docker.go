package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

type warmDockerMount struct {
	Type, Name, Source, Destination, Propagation string
	RW                                           bool
}

type warmDockerInspection struct {
	ID, Name, Image string
	Config          struct {
		Image, User string
		Labels      map[string]string
	}
	State      struct{ Running bool }
	Mounts     []warmDockerMount
	HostConfig struct {
		Binds                                                        []string
		Mounts                                                       []warmDockerMountRequest
		Tmpfs                                                        map[string]string
		Privileged                                                   bool
		CapAdd, CapDrop, SecurityOpt, VolumesFrom, DeviceCgroupRules []string
		Devices, DeviceRequests                                      []json.RawMessage
		PidMode, IpcMode, NetworkMode, UsernsMode                    string
	}
}

// Check the daemon's effective container definition before starting or
// attaching. Docker image volumes and inherited containers can add mounts
// that are absent from the arguments assembled by this process.
func (i warmDockerInspection) validate(workspace string, objects []string, cli string) error {
	if err := i.validateHost(); err != nil {
		return err
	}

	allowed := map[string]string{"/workspace": workspace}
	return i.validateMounts(allowed, objects, cli, true)
}

func (i warmDockerInspection) validateMounts(allowed map[string]string, objects []string, cli string, writable bool) error {
	for _, dir := range objects {
		allowed[dir] = dir
	}
	if cli != "" {
		allowed[cli] = cli
	}
	for _, m := range i.Mounts {
		if m.Type == "tmpfs" && m.Destination == "/tmp" {
			continue
		}
		expected, ok := allowed[m.Destination]
		private := m.Propagation == "" || m.Propagation == "rprivate" || m.Propagation == "private"
		if !ok || m.Type != "bind" || m.Source != expected || m.RW != (writable && m.Destination == "/workspace") || !private {
			return fmt.Errorf("unexpected warm Docker mount at %s", m.Destination)
		}
		delete(allowed, m.Destination)
	}
	if len(allowed) != 0 {
		return errors.New("warm Docker sandbox is missing expected mounts")
	}
	return nil
}

func (i warmDockerInspection) validateHost() error {
	h := i.HostConfig
	noNewPrivileges := slices.Contains(h.SecurityOpt, "no-new-privileges") || slices.Contains(h.SecurityOpt, "no-new-privileges:true")
	if h.Privileged || !slices.Contains(h.CapDrop, "ALL") || !noNewPrivileges || len(h.CapAdd) > 0 {
		return errors.New("warm Docker sandbox requires restricted capabilities and no new privileges")
	}
	if len(h.Devices) > 0 || len(h.DeviceRequests) > 0 || len(h.DeviceCgroupRules) > 0 || len(h.VolumesFrom) > 0 {
		return errors.New("warm Docker sandbox has host devices or inherited volumes")
	}
	if h.PidMode != "" && h.PidMode != "private" || h.UsernsMode == "host" || h.IpcMode == "host" || strings.HasPrefix(h.IpcMode, "container:") || h.NetworkMode == "host" || strings.HasPrefix(h.NetworkMode, "container:") {
		return errors.New("warm Docker sandbox shares an unsafe host namespace")
	}

	return nil
}

func (s *dockerSandbox) verifyWarmDocker(ctx context.Context) error {
	if len(s.requirements.GitObjectDirs) == 0 && s.requirements.WarmDockerVolume == nil {
		return nil
	}
	_, err := s.inspectedWarmTask(ctx)
	return err
}

func (s *dockerSandbox) inspectedWarmTask(ctx context.Context) (*warmDockerInspection, error) {
	target, err := s.executionTarget()
	if err != nil {
		return nil, err
	}
	var actual *warmDockerInspection
	if s.requirements.WarmDockerVolume != nil {
		actual, err = s.inspectVolumeTask(ctx, target, target)
	} else {
		actual, err = inspectWarmDocker(ctx, target)
		if err == nil && actual != nil {
			err = actual.validateTask(s.workspace, s.requirements, RuntimeCLIDir())
		}
	}
	if err != nil {
		return nil, err
	}
	if actual == nil {
		return nil, errors.New("warm Docker sandbox disappeared")
	}
	return actual, nil
}

func inspectWarmDocker(ctx context.Context, name string) (*warmDockerInspection, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", name).CombinedOutput()
	if dockerObjectGone(string(out), err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect warm Docker container: %w", err)
	}
	var rows []warmDockerInspection
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
		return nil, errors.New("invalid warm Docker inspection")
	}
	return &rows[0], nil
}
