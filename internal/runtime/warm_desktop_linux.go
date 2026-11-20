//go:build linux

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// WarmDockerDesktop is bound to one running container, its immutable image,
// and its private, plain local volume on one pinned Desktop Linux engine.
type WarmDockerDesktop struct {
	conversations                                          *desktopConversationScope
	volume, root, backing, id, image, executable, endpoint string
	mount                                                  desktopMountInfo
	pid                                                    int
	mountNS, pidNS                                         string
}

type desktopMount struct {
	Type, Name, Source, Destination, Propagation string
	RW                                           bool
}
type desktopMountSpec struct {
	Type, Source, Target string
	VolumeOptions        struct{ Subpath string }
}
type desktopContainer struct {
	ID, Name, Image string
	State           struct {
		Running bool
		Pid     int
	}
	Config     struct{ Labels map[string]string }
	HostConfig struct{ Mounts []desktopMountSpec }
	Mounts     []desktopMount
}

var desktopContainerID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var desktopVolumeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func desktopDockerJSON(ctx context.Context, target any, args ...string) error {
	raw, err := exec.CommandContext(ctx, "docker", args...).Output()
	if err != nil {
		return fmt.Errorf("Docker Desktop %s: %w", args[0], err)
	}
	return json.Unmarshal(raw, target)
}

// OpenWarmDockerDesktop verifies the local engine and its view of this runner.
// Ready also verifies the daemon's PID namespace before any capacity is offered.
func OpenWarmDockerDesktop(ctx context.Context, volume, root string) (*WarmDockerDesktop, error) {
	host := os.Getenv("DOCKER_HOST")
	socket, ok := strings.CutPrefix(host, "unix://")
	if !ok || !filepath.IsAbs(socket) || filepath.Clean(socket) != socket || os.Getenv("DOCKER_CONTEXT") != "" {
		return nil, errors.New("Docker Desktop requires a pinned local Unix socket")
	}
	fi, err := os.Stat(socket)
	if err != nil || fi.Mode()&os.ModeSocket == 0 {
		return nil, errors.New("Docker Desktop socket unavailable")
	}
	if !desktopVolumeName.MatchString(volume) || !filepath.IsAbs(root) || root == "/" || filepath.Clean(root) != root {
		return nil, errors.New("invalid Docker Desktop warm volume or mount root")
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil || root != realRoot {
		return nil, errors.New("Docker Desktop warm mount root must be canonical")
	}
	info := struct{ OperatingSystem, OSType, KernelVersion, DockerRootDir string }{}
	if err := desktopDockerJSON(ctx, &info, "info", "--format", "{{json .}}"); err != nil {
		return nil, err
	}
	kernel, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil || strings.TrimSpace(string(kernel)) != info.KernelVersion || !strings.Contains(strings.ToLower(info.KernelVersion), "microsoft-standard-wsl2") || info.OperatingSystem != "Docker Desktop" || info.OSType != "linux" || !filepath.IsAbs(info.DockerRootDir) {
		return nil, errors.New("a local Docker Desktop WSL2 engine is required")
	}
	var vol desktopVolume
	if err := desktopDockerJSON(ctx, &vol, "volume", "inspect", "--format", "{{json .}}", volume); err != nil {
		return nil, err
	}
	if vol.Name != volume || vol.Scope != "local" || vol.Driver != "local" || len(vol.Options) != 0 || vol.Mountpoint != filepath.Join(info.DockerRootDir, "volumes", volume, "_data") {
		return nil, errors.New("a plain private Docker Desktop local volume is required")
	}
	mounts, err := readDesktopMountInfo("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	hostname, ok := mounts.at("/etc/hostname")
	if !ok || filepath.Base(hostname.root) != "hostname" {
		return nil, errors.New("cannot identify the runner container")
	}
	containerDir := filepath.Dir(hostname.root)
	id := filepath.Base(containerDir)
	if !desktopContainerID.MatchString(id) || filepath.Base(filepath.Dir(containerDir)) != "containers" {
		return nil, errors.New("cannot identify the runner container mount")
	}
	mount, ok := mounts.at(root)
	physicalRoot := filepath.Join(filepath.Dir(filepath.Dir(containerDir)), "volumes", volume, "_data")
	if !ok || mount.root != physicalRoot || mount.device != hostname.device || mount.fs != "ext4" || !mount.writable || mount.shared {
		return nil, errors.New("private native Docker Desktop volume mount required")
	}
	for _, m := range mounts {
		if m.target != root && desktopBelow(root, m.target) {
			return nil, errors.New("nested mount obscures warm volume")
		}
	}
	d := &WarmDockerDesktop{volume: volume, root: root, backing: vol.Mountpoint, id: id, endpoint: host, mount: mount}
	c, err := desktopInspect(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.ID != id || !c.State.Running || c.State.Pid < 1 || !strings.HasPrefix(c.Image, "sha256:") || !desktopContainerID.MatchString(strings.TrimPrefix(c.Image, "sha256:")) {
		return nil, errors.New("Docker Desktop cannot identify the running container or image")
	}
	if err := d.checkSelfMount(c); err != nil {
		return nil, err
	}
	d.image, d.pid = c.Image, c.State.Pid
	d.executable, err = os.Executable()
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(d.executable) || filepath.Clean(d.executable) != d.executable {
		return nil, errors.New("invalid runner image executable")
	}
	for _, m := range c.Mounts {
		if desktopBelow(m.Destination, d.executable) {
			return nil, errors.New("the probe executable must come from the runner image")
		}
	}
	var volumes map[string]json.RawMessage
	if err := desktopDockerJSON(ctx, &volumes, "image", "inspect", "--format", "{{json .Config.Volumes}}", d.image); err != nil {
		return nil, err
	}
	if len(volumes) != 0 {
		return nil, errors.New("the probe image must not declare volumes")
	}
	d.mountNS, err = os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		return nil, err
	}
	d.pidNS, err = os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return nil, err
	}
	return d, nil
}

func desktopInspect(ctx context.Context, id string) (desktopContainer, error) {
	var c desktopContainer
	// Avoid retrieving environment variables or command lines from other containers.
	const format = `{"Id":{{json .Id}},"Name":{{json .Name}},"Image":{{json .Image}},"State":{"Running":{{json .State.Running}},"Pid":{{json .State.Pid}}},"Config":{"Labels":{{json .Config.Labels}}},"Mounts":{{json .Mounts}},"HostConfig":{"Mounts":{{json (index .HostConfig "Mounts")}}}}`
	err := desktopDockerJSON(ctx, &c, "inspect", "--type", "container", "--format", format, id)
	return c, err
}

func (d *WarmDockerDesktop) checkSelfMount(c desktopContainer) error {
	found := false
	for _, m := range c.Mounts {
		if m.Destination == d.root {
			sub, err := desktopVolumeSubpath(c, m)
			if found || err != nil || sub != "" || m.Type != "volume" || m.Name != d.volume || m.Source != d.backing || !m.RW || (m.Propagation != "" && m.Propagation != "rprivate" && m.Propagation != "private") {
				return errors.New("runner must mount the entire private warm volume read-write")
			}
			found = true
		} else if desktopBelow(d.root, m.Destination) {
			return errors.New("runner warm volume contains a nested mount")
		}
	}
	if !found {
		return errors.New("runner warm volume missing")
	}
	return nil
}

func desktopVolumeSubpath(c desktopContainer, m desktopMount) (string, error) {
	sub, found := "", false
	for _, s := range c.HostConfig.Mounts {
		if s.Target != m.Destination {
			continue
		}
		if found || s.Type != "volume" || s.Source != m.Name {
			return "", errors.New("ambiguous volume mount definition")
		}
		sub, found = s.VolumeOptions.Subpath, true
		if sub != "" && (!filepath.IsLocal(sub) || filepath.Clean(sub) != sub || sub == ".") {
			return "", errors.New("invalid Docker volume subpath")
		}
	}
	// Legacy -v and inherited volumes have no explicit subpath. Treating them
	// as the whole backing is conservative when the container is external.
	return sub, nil
}

func desktopBelow(parent, child string) bool {
	if !filepath.IsAbs(parent) || !filepath.IsAbs(child) {
		return false
	}
	rel, err := filepath.Rel(parent, child)
	return err == nil && filepath.IsLocal(rel)
}
func desktopOverlap(a, b string) bool { return desktopBelow(a, b) || desktopBelow(b, a) }

func (d *WarmDockerDesktop) Backing() string { return d.backing }

func (d *WarmDockerDesktop) Ready(ctx context.Context) error {
	if err := d.verifyCurrent(ctx); err != nil {
		return err
	}
	mounts, err := d.inventory(ctx, "", false)
	if err != nil {
		return err
	}
	return d.probe(ctx, "", mounts)
}
func (d *WarmDockerDesktop) verifyCurrent(ctx context.Context) error {
	next, err := OpenWarmDockerDesktop(ctx, d.volume, d.root)
	if err != nil {
		return err
	}
	next.conversations = d.conversations
	if *d != *next {
		return errors.New("Docker Desktop runner identity or volume changed")
	}
	return nil
}
func (d *WarmDockerDesktop) Fence(ctx context.Context, workspace string) error {
	if workspace == d.root || filepath.Clean(workspace) != workspace || !desktopBelow(d.root, workspace) {
		return errors.New("workspace outside verified Docker volume")
	}
	if real, err := filepath.EvalSymlinks(workspace); err != nil || real != workspace {
		return errors.New("Docker Desktop return requires a canonical workspace")
	}
	if err := d.verifyCurrent(ctx); err != nil {
		return err
	}
	// Verify the daemon PID identity before removing an owned helper.
	if err := d.probe(ctx, "", desktopInventory{}); err != nil {
		return err
	}
	if _, err := d.inventory(ctx, workspace, true); err != nil {
		return err
	}
	mounts, err := d.inventory(ctx, workspace, false)
	if err != nil {
		return err
	}
	return d.probe(ctx, workspace, mounts)
}

type desktopVolume struct {
	Name, Scope, Driver, Mountpoint string
	Options                         map[string]string
}

// An inactive local bind volume can expose an empty mountpoint until restart.
// Include its underlying device path in the daemon-side physical scope check.
func (v desktopVolume) sources(source string) ([]string, error) {
	if v.Driver != "local" {
		return nil, errors.New("unknown Docker volume driver during workspace fence")
	}
	if len(v.Options) == 0 {
		return []string{source}, nil
	}
	device := v.Options["device"]
	if filepath.IsAbs(device) && (v.Options["type"] == "none" || v.Options["type"] == "") {
		for _, o := range strings.Split(v.Options["o"], ",") {
			if o == "bind" || o == "rbind" {
				return []string{source, device}, nil
			}
		}
	}
	return nil, errors.New("unknown backing for external Docker volume")
}
func (d *WarmDockerDesktop) checkLocalSubpath(sub string) error {
	if sub == "" {
		return nil
	}
	p := filepath.Join(d.root, sub)
	real, err := filepath.EvalSymlinks(p)
	if err != nil || p != real {
		return errors.New("cannot verify Docker volume subpath on the mounted filesystem")
	}
	return nil
}

// A dedicated volume may serve the verified runner and its scoped children.
// The writable workspace must be positively identified even for a read helper.
func (d *WarmDockerDesktop) checkDedicated(c desktopContainer) error {
	if d.conversations != nil && c.ID != d.id {
		return d.checkCarrier(c)
	}
	scoped, owner := false, false
	for _, m := range c.Mounts {
		if m.Type != "volume" || m.Name != d.volume {
			continue
		}
		sub, err := desktopVolumeSubpath(c, m)
		if err != nil || m.Source != d.backing {
			return errors.New("unverified Docker warm volume identity")
		}
		if c.ID == d.id && m.Destination == d.root && sub == "" && m.RW {
			continue
		}
		scoped = true
		if sub == "" || (m.RW && m.Destination != "/workspace") {
			return errors.New("warm volume shared with an unqualified container")
		}
		if m.Destination == "/workspace" && filepath.Base(sub) == "workspace" {
			owner = warmContainerOwnsWorkspace(c.Name, c.Config.Labels, filepath.Join(d.root, sub))
		}
	}
	if scoped && !owner {
		return errors.New("warm volume shared with an unidentified container")
	}
	return nil
}

type desktopLiveMount struct {
	PID    int
	Target string
}
type desktopInventory struct {
	BindSources []string
	Live        []desktopLiveMount
}

func (d *WarmDockerDesktop) inventory(ctx context.Context, workspace string, cleanup bool) (result desktopInventory, err error) {
	raw, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--no-trunc").Output()
	if err != nil {
		return result, fmt.Errorf("cannot enumerate Docker Desktop containers: %w", err)
	}
	foreign := map[string]desktopVolume{}
	for _, id := range strings.Fields(string(raw)) {
		c, err := desktopInspect(ctx, id)
		if err != nil {
			var failure *exec.ExitError
			if errors.As(err, &failure) && dockerObjectGone(string(failure.Stderr), err) {
				continue
			}
			return result, err
		}
		if c.ID != id {
			return result, errors.New("container identity changed during workspace fence")
		}
		remove, err := d.classifyVolumeClient(ctx, c, workspace)
		if err != nil {
			return result, err
		}
		if remove {
			if !cleanup {
				return result, fmt.Errorf("container %s still has a workspace mount", c.Name)
			}
			if err := removeDockerContainer(ctx, c.ID); err != nil {
				return result, err
			}
			continue
		}
		for _, m := range c.Mounts {
			switch m.Type {
			case "bind":
				result.BindSources = append(result.BindSources, m.Source)
			case "volume":
				if m.Name == d.volume {
					sub, err := desktopVolumeSubpath(c, m)
					if err != nil {
						return result, err
					}
					if err := d.checkLocalSubpath(sub); err != nil {
						return result, err
					}
					if workspace == "" || c.ID == d.id && m.Destination == d.root {
						continue
					}
				} else {
					v, ok := foreign[m.Name]
					if !ok {
						if err := desktopDockerJSON(ctx, &v, "volume", "inspect", "--format", "{{json .}}", m.Name); err != nil {
							return result, err
						}
						foreign[m.Name] = v
					}
					if v.Name != m.Name || v.Mountpoint != m.Source {
						return result, errors.New("foreign Docker volume backing changed")
					}
					sources, err := v.sources(m.Source)
					if err != nil {
						return result, err
					}
					result.BindSources = append(result.BindSources, sources...)
				}
			case "tmpfs":
				continue
			default:
				return result, errors.New("unknown Docker mount type during workspace fence")
			}
			// A running bind is pinned to its original mount, even if the daemon's
			// source path has since been renamed or points to another filesystem.
			if c.State.Running {
				result.Live = append(result.Live, desktopLiveMount{c.State.Pid, m.Destination})
			}
		}
	}
	return result, nil
}

func (d *WarmDockerDesktop) containerScope(c desktopContainer, workspace string) (mounted, owns bool, err error) {
	sub, _ := filepath.Rel(d.root, workspace)
	daemonPath, physicalPath := filepath.Join(d.backing, sub), filepath.Join(d.mount.root, sub)
	for _, m := range c.Mounts {
		switch m.Type {
		case "volume":
			if m.Name != d.volume {
				if desktopOverlap(m.Source, daemonPath) || desktopOverlap(m.Source, physicalPath) {
					return true, false, errors.New("unexpected volume identity at workspace backing")
				}
				continue
			}
			if m.Source != d.backing {
				return true, false, errors.New("Docker volume backing changed")
			}
			s, err := desktopVolumeSubpath(c, m)
			if err != nil {
				return true, false, err
			}
			if c.ID == d.id && m.Destination == d.root && s == "" && m.RW {
				continue
			}
			source := filepath.Join(d.backing, s)
			if !desktopOverlap(source, daemonPath) {
				continue
			}
			mounted = true
			if source != daemonPath || m.Destination != "/workspace" {
				return true, false, nil
			}
			owns = warmContainerOwnsWorkspace(c.Name, c.Config.Labels, workspace)
		case "bind":
			if !filepath.IsAbs(m.Source) || desktopOverlap(m.Source, daemonPath) || desktopOverlap(m.Source, physicalPath) {
				return true, false, nil
			}
		}
	}
	return mounted, owns, nil
}

func (d *WarmDockerDesktop) probe(ctx context.Context, workspace string, inventory desktopInventory) error {
	req := desktopProbeRequest{RunnerPID: d.pid, MountNS: d.mountNS, PIDNS: d.pidNS, Device: d.mount.device, Backing: d.backing, PhysicalRoot: d.mount.root, desktopInventory: inventory}
	if workspace != "" {
		rel, _ := filepath.Rel(d.root, workspace)
		req.Workspace = filepath.Join(d.mount.root, rel)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "run", "--rm", "--pull=never", "--pid=host", "--network=none", "--read-only", "--cap-drop=ALL", "--cap-add=SYS_PTRACE", "--cap-add=DAC_READ_SEARCH", "--security-opt=no-new-privileges", "--no-healthcheck", "--user=0:0", "--interactive", "--entrypoint", d.executable, d.image, "warm-docker-process-probe")
	cmd.Stdin = bytes.NewReader(raw)
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("workspace retained: Docker Desktop process inventory unavailable: %w", err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return fmt.Errorf("workspace retained: Docker Desktop process inventory: %s", strings.TrimSpace(string(out)))
	}
	return nil
}
