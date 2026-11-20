package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type desktopMountInfo struct {
	id                       uint64
	device, root, target, fs string
	writable, shared         bool
}
type desktopMountTable []desktopMountInfo

func (t desktopMountTable) at(target string) (desktopMountInfo, bool) {
	var found desktopMountInfo
	for _, m := range t {
		if m.target == target {
			if found.id != 0 {
				return desktopMountInfo{}, false
			}
			found = m
		}
	}
	return found, found.id != 0
}
func readDesktopMountInfo(path string) (desktopMountTable, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseDesktopMountInfo(raw)
}

var desktopProcEscapes = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, "\\")

func parseDesktopMountInfo(raw []byte) (desktopMountTable, error) {
	var table desktopMountTable
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		left, right, ok := strings.Cut(line, " - ")
		f, extra := strings.Fields(left), strings.Fields(right)
		if !ok || len(f) < 6 || len(extra) < 3 {
			return nil, errors.New("invalid Linux mount inventory")
		}
		id, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("invalid Linux mount ID")
		}
		m := desktopMountInfo{id: id, device: f[2], root: desktopProcEscapes.Replace(f[3]), target: desktopProcEscapes.Replace(f[4]), fs: extra[0]}
		for _, x := range strings.Split(f[5], ",") {
			if x == "rw" {
				m.writable = true
			}
		}
		for _, x := range f[6:] {
			if strings.HasPrefix(x, "shared:") {
				m.shared = true
			}
		}
		table = append(table, m)
	}
	return table, nil
}

type desktopProbeRequest struct {
	RunnerPID                                                int
	MountNS, PIDNS, Device, Backing, PhysicalRoot, Workspace string
	desktopInventory
}

var errDesktopBusy = errors.New("a live process or container mount uses this workspace")

// This entry point has no Docker socket or data mount. Its output contains only
// a busy/visibility result. Never return process command lines or file data.
func RunWarmDockerProbeCLI(args []string) bool {
	if len(args) != 1 || args[0] != "warm-docker-process-probe" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	var req desktopProbeRequest
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 8<<20)).Decode(&req); err != nil {
		fmt.Println("invalid request")
		return true
	}
	if err := desktopProcessProbe(ctx, req); err != nil {
		if errors.Is(err, errDesktopBusy) {
			fmt.Println("workspace busy")
		} else {
			fmt.Println("incomplete process or mount inventory:", err)
		}
		return true
	}
	fmt.Println("ok")
	return true
}

func desktopProcessProbe(ctx context.Context, r desktopProbeRequest) error {
	if r.RunnerPID < 1 || r.MountNS == "" || r.PIDNS == "" || r.Device == "" || !filepath.IsAbs(r.PhysicalRoot) || !filepath.IsAbs(r.Backing) {
		return errors.New("missing runner identity")
	}
	if r.Workspace != "" && (r.Workspace == r.PhysicalRoot || !desktopBelow(r.PhysicalRoot, r.Workspace)) {
		return errors.New("invalid physical workspace scope")
	}
	runner := filepath.Join("/proc", strconv.Itoa(r.RunnerPID))
	for name, expected := range map[string]string{"mnt": r.MountNS, "pid": r.PIDNS} {
		ns, err := os.Readlink(filepath.Join(runner, "ns", name))
		if err != nil || ns != expected {
			return errors.New("runner namespace identity mismatch")
		}
	}
	own, err := os.Readlink("/proc/self/ns/pid")
	init, initErr := os.Readlink("/proc/1/ns/pid")
	if err != nil || initErr != nil || own != init || own == r.PIDNS {
		return errors.New("incomplete Docker Desktop host PID namespace")
	}
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	if err := checkWarmProcVisibility(raw); err != nil {
		return err
	}
	scope := desktopMountInfo{root: r.Workspace, device: r.Device}
	if scope.root == "" {
		scope.root = r.PhysicalRoot
	}
	if err := checkDesktopBindSources(ctx, r, scope); err != nil {
		return err
	}
	for _, live := range r.Live {
		if live.PID < 1 || !filepath.IsAbs(live.Target) {
			return errors.New("invalid live Docker mount identity")
		}
		mounts, err := readDesktopMountInfo(filepath.Join("/proc", strconv.Itoa(live.PID), "mountinfo"))
		if err != nil {
			return err
		}
		m, ok := mounts.at(live.Target)
		if !ok {
			// A Docker destination may traverse an image symlink (for
			// example /var/run). Resolve metadata inside this container.
			_, resolved, err := desktopRootStat(filepath.Join("/proc", strconv.Itoa(live.PID), "root"), live.Target)
			if err != nil {
				return errors.New("cannot resolve live Docker mount")
			}
			m, ok = mounts.at(resolved)
			if !ok {
				return errors.New("cannot verify live Docker mount")
			}
		}
		for _, actual := range mounts {
			if desktopBelow(m.target, actual.target) && actual.device == scope.device && desktopOverlap(scope.root, actual.root) {
				return errDesktopBusy
			}
		}
	}
	if r.Workspace == "" {
		return checkWarmLinuxProcesses(ctx, "/proc", func(string) bool { return false })
	}
	return walkWarmLinuxThreads(ctx, "/proc", func(task string) error { return checkDesktopTask(task, scope) })
}

func desktopMountPath(m desktopMountInfo, name string) (string, error) {
	name = strings.TrimSuffix(name, " (deleted)")
	if !desktopBelow(m.target, name) {
		return "", errors.New("cannot resolve a path on the workspace filesystem")
	}
	suffix, _ := filepath.Rel(m.target, name)
	return filepath.Join(m.root, suffix), nil
}
func desktopHandlePath(scope desktopMountInfo, mounts desktopMountTable, id uint64, name string) (string, error) {
	for _, m := range mounts {
		if m.id == id && m.device == scope.device {
			return desktopMountPath(m, name)
		}
	}
	return "", errors.New("cannot identify a mount on the workspace filesystem")
}
func checkDesktopHandle(scope desktopMountInfo, mounts desktopMountTable, id uint64, name string) error {
	path, err := desktopHandlePath(scope, mounts, id, name)
	if err != nil {
		return err
	}
	if desktopBelow(scope.root, path) {
		return errDesktopBusy
	}
	return nil
}

func desktopStat(path string) (unix.Statx_t, error) {
	var s unix.Statx_t
	err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_INO|unix.STATX_MNT_ID, &s)
	if err == nil && s.Mask&(unix.STATX_INO|unix.STATX_MNT_ID) != unix.STATX_INO|unix.STATX_MNT_ID {
		err = errors.New("kernel mount identity unavailable")
	}
	return s, err
}
func desktopDevice(stat unix.Statx_t) string {
	return fmt.Sprintf("%d:%d", stat.Dev_major, stat.Dev_minor)
}

func checkDesktopTask(task string, scope desktopMountInfo) error {
	var mounts desktopMountTable
	loadMounts := func() error {
		if mounts != nil {
			return nil
		}
		var err error
		mounts, err = readDesktopMountInfo(filepath.Join(task, "mountinfo"))
		return err
	}
	handle := func(link string) error {
		// Pin the object while identifying it. A live process can close
		// and reuse a descriptor number between readlink and statx.
		fd, err := unix.Open(link, unix.O_PATH|unix.O_CLOEXEC, 0)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		pinned := "/proc/self/fd/" + strconv.Itoa(fd)
		s, err := desktopStat(pinned)
		if err != nil {
			return err
		}
		if desktopDevice(s) != scope.device {
			return nil
		}
		path, err := os.Readlink(pinned)
		if err != nil {
			return err
		}
		if err := loadMounts(); err != nil {
			return err
		}
		return checkDesktopHandle(scope, mounts, s.Mnt_id, path)
	}
	for _, name := range []string{"cwd", "root", "exe"} {
		if err := handle(filepath.Join(task, name)); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Join(task, "fd"))
	if err != nil {
		return err
	}
	for _, f := range entries {
		if err := handle(filepath.Join(task, "fd", f.Name())); err != nil {
			return err
		}
	}
	maps, err := os.Open(filepath.Join(task, "maps"))
	if err != nil {
		return err
	}
	defer maps.Close()
	scanner := bufio.NewScanner(maps)
	for scanner.Scan() {
		line := scanner.Text()
		var f [5]string
		for i := range f {
			line = strings.TrimLeft(line, " ")
			f[i], line, _ = strings.Cut(line, " ")
		}
		major, minor, ok := strings.Cut(f[3], ":")
		a, ae := strconv.ParseUint(major, 16, 32)
		b, be := strconv.ParseUint(minor, 16, 32)
		if !ok || ae != nil || be != nil {
			return errors.New("invalid mapped file device")
		}
		if fmt.Sprintf("%d:%d", a, b) != scope.device {
			continue
		}
		name := strings.TrimLeft(line, " ")
		// A mapping can outlive its descriptor or mount. Verify the live name's
		// inode as well as its mount ID before using it to exclude a mapped file.
		ino, err := strconv.ParseUint(f[4], 10, 64)
		if err != nil || !filepath.IsAbs(name) {
			return errors.New("unverified mapping on workspace filesystem")
		}
		if err := loadMounts(); err != nil {
			return err
		}
		stat, path, err := desktopRootStat(filepath.Join(task, "root"), name)
		if err != nil || stat.Ino != ino || desktopDevice(stat) != scope.device {
			return errors.New("unresolved mapping on workspace filesystem")
		}
		if err := checkDesktopHandle(scope, mounts, stat.Mnt_id, path); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// Resolve only metadata, inside the selected process root. RESOLVE_IN_ROOT
// handles absolute symlinks inside that namespace and NO_MAGICLINKS excludes
// procfs file descriptor indirection. O_PATH cannot read file or socket data.
func desktopRootStat(root, path string) (unix.Statx_t, string, error) {
	var zero unix.Statx_t
	dir, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return zero, "", err
	}
	defer unix.Close(dir)
	fd, err := unix.Openat2(dir, path, &unix.OpenHow{Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_IN_ROOT | unix.RESOLVE_NO_MAGICLINKS})
	if err != nil {
		return zero, "", err
	}
	defer unix.Close(fd)
	link := "/proc/self/fd/" + strconv.Itoa(fd)
	s, err := desktopStat(link)
	if err != nil {
		return zero, "", err
	}
	resolved, err := os.Readlink(link)
	return s, resolved, err
}

func checkDesktopBindSources(ctx context.Context, r desktopProbeRequest, scope desktopMountInfo) error {
	if len(r.BindSources) == 0 {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	var daemon string
	for _, p := range entries {
		if _, err := strconv.Atoi(p.Name()); err != nil {
			continue
		}
		base := filepath.Join("/proc", p.Name())
		comm, err := os.ReadFile(filepath.Join(base, "comm"))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if strings.TrimSpace(string(comm)) != "dockerd" {
			continue
		}
		// Match the daemon that actually sees the qualified volume backing.
		mounts, err := readDesktopMountInfo(filepath.Join(base, "mountinfo"))
		if err != nil {
			return err
		}
		stat, path, err := desktopRootStat(filepath.Join(base, "root"), r.Backing)
		if err != nil || desktopDevice(stat) != r.Device {
			continue
		}
		physical, err := desktopHandlePath(scope, mounts, stat.Mnt_id, path)
		if err != nil || physical != r.PhysicalRoot {
			continue
		}
		if daemon != "" {
			return errors.New("ambiguous Docker daemon process")
		}
		daemon = base
	}
	if daemon == "" {
		return errors.New("cannot identify Docker daemon mount namespace")
	}
	mounts, err := readDesktopMountInfo(filepath.Join(daemon, "mountinfo"))
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, source := range r.BindSources {
		if err := ctx.Err(); err != nil {
			return err
		}
		if seen[source] {
			continue
		}
		seen[source] = true
		if !filepath.IsAbs(source) {
			return errors.New("unknown Docker bind source")
		}
		stat, path, err := desktopRootStat(filepath.Join(daemon, "root"), source)
		if err != nil {
			return errors.New("cannot resolve Docker bind source")
		}
		if desktopDevice(stat) == scope.device {
			physical, err := desktopHandlePath(scope, mounts, stat.Mnt_id, path)
			if err != nil {
				return err
			}
			if desktopOverlap(scope.root, physical) {
				return errDesktopBusy
			}
		}
		for _, m := range mounts {
			if m.device == scope.device && desktopBelow(path, m.target) && desktopOverlap(scope.root, m.root) {
				return errDesktopBusy
			}
		}
	}
	return nil
}
