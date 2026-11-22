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
	Type, Source, Destination, Propagation string
	RW                                     bool
}

type warmDockerInspection struct {
	Mounts     []warmDockerMount
	HostConfig struct {
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
	if len(s.requirements.GitObjectDirs) == 0 {
		return nil
	}
	out, err := exec.CommandContext(ctx, "docker", "inspect", "--type", "container", s.name).Output()
	if err != nil {
		return fmt.Errorf("inspect warm Docker container: %w", err)
	}
	var rows []warmDockerInspection
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
		return errors.New("invalid warm Docker inspection")
	}
	return rows[0].validate(s.workspace, s.requirements.GitObjectDirs, RuntimeCLIDir())
}
