package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestWarmDesktopFencePhysicalScope(t *testing.T) {
	d := &WarmDockerDesktop{volume: "pool", root: "/pool", backing: "/var/lib/docker/volumes/pool/_data", mount: desktopMountInfo{root: "/data/docker/volumes/pool/_data", device: "8:64"}, id: strings.Repeat("a", 64)}
	const ws = "/pool/runs/one/workspace"
	for _, tt := range []struct {
		name, typ, volume, source, sub, target, id string
		held                                       bool
	}{
		{"stopped exact", "volume", "pool", d.backing, "runs/one/workspace", "/other", "foreign", true},
		{"ancestor", "volume", "pool", d.backing, "runs/one", "/other", "foreign", true},
		{"sibling", "volume", "pool", d.backing, "runs/one/workspace-old", "/other", "foreign", false},
		{"unknown subpath", "volume", "pool", d.backing, "../elsewhere", "/other", "foreign", true},
		{"wrong backing", "volume", "pool", "/other", "sibling", "/other", "foreign", true},
		{"physical bind", "bind", "", d.mount.root + "/runs/one/workspace/sub", "", "/other", "foreign", true},
		{"daemon bind", "bind", "", d.backing + "/runs/one", "", "/other", "foreign", true},
		{"self root", "volume", "pool", d.backing, "", "/pool", d.id, false},
		{"spoofed self", "volume", "pool", d.backing, "", "/pool", "spoof", true},
		{"self extra mount", "volume", "pool", d.backing, "", "/other", d.id, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := desktopContainer{ID: tt.id}
			c.Mounts = []desktopMount{{Type: tt.typ, Name: tt.volume, Source: tt.source, Destination: tt.target, RW: true}}
			if tt.typ == "volume" {
				spec := desktopMountSpec{Type: "volume", Source: tt.volume, Target: tt.target}
				spec.VolumeOptions.Subpath = tt.sub
				c.HostConfig.Mounts = []desktopMountSpec{spec}
			}
			held, _, err := d.containerScope(c, ws)
			if (held || err != nil) != tt.held {
				t.Fatalf("held=%v err=%v", held, err)
			}
		})
	}
}

func TestWarmDesktopForeignRunnerMount(t *testing.T) {
	d := &WarmDockerDesktop{volume: "pool", root: "/pool", backing: "/var/lib/docker/volumes/pool/_data"}
	c := desktopContainer{}
	c.Mounts = []desktopMount{{Type: "volume", Name: "other", Source: d.backing, Destination: "/pool", RW: true}}
	if err := d.checkSelfMount(c); err == nil {
		t.Fatal("foreign volume accepted")
	}
}

func TestWarmDesktopUnavailableInventory(t *testing.T) {
	if err := desktopProcessProbe(t.Context(), desktopProbeRequest{RunnerPID: 123}); err == nil {
		t.Fatal("unverified runner identity accepted")
	}
}

func TestWarmDesktopMapsVolumeAlias(t *testing.T) {
	scope := desktopMountInfo{device: "8:64", root: "/data/docker/volumes/pool/_data/runs/one/workspace"}
	mounts, err := parseDesktopMountInfo([]byte("36 1 8:64 /data/docker/volumes/pool/_data /private rw - ext4 /dev/sde rw\n37 1 8:64 /data/docker/volumes/pool/_data/runs/one/workspace /workspace rw - ext4 /dev/sde rw\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		path string
		id   uint64
		busy bool
	}{
		{"/private/runs/one/workspace/owner (deleted)", 36, true}, {"/workspace/path with spaces", 37, true}, {"/private/runs/one/workspace-old/owner", 36, false}, {"/unknown", 99, true},
	} {
		err := checkDesktopHandle(scope, mounts, tt.id, tt.path)
		if tt.busy != (err != nil) {
			t.Fatalf("handle %s: %v", tt.path, err)
		}
	}
	if err := checkDesktopHandle(scope, mounts, 37, "/workspace/file"); !errors.Is(err, errDesktopBusy) {
		t.Fatalf("wrong busy result: %v", err)
	}
}

func TestWarmDesktopDedicatedVolumeExcludesForeignContainers(t *testing.T) {
	d := &WarmDockerDesktop{volume: "pool", root: "/pool", backing: "/var/lib/docker/volumes/pool/_data", id: strings.Repeat("a", 64)}
	c := desktopContainer{ID: "other", Name: "/unrelated"}
	mount := desktopMount{Type: "volume", Name: "pool", Source: d.backing, Destination: "/data", RW: true}
	c.Mounts = []desktopMount{mount}
	if err := d.checkDedicated(c); err == nil {
		t.Fatal("second whole-volume client accepted")
	}
	c.ID = d.id
	c.Mounts[0].Destination = d.root
	if err := d.checkDedicated(c); err != nil {
		t.Fatal(err)
	}
	c.ID = "other"
	c.Mounts[0].Destination = "/workspace"
	spec := desktopMountSpec{Type: "volume", Source: "pool", Target: "/workspace"}
	spec.VolumeOptions.Subpath = "runs/one/workspace"
	c.HostConfig.Mounts = []desktopMountSpec{spec}
	if err := d.checkDedicated(c); err == nil {
		t.Fatal("unknown scoped client accepted")
	}
	c.Config.Labels = map[string]string{"io.loopforge.warm-workspace": "/pool/runs/one/workspace"}
	if err := d.checkDedicated(c); err != nil {
		t.Fatal(err)
	}
}

func TestWarmDesktopFenceRejectsAmbiguousVolumeAlias(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "work"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("work", filepath.Join(root, "sibling")); err != nil {
		t.Fatal(err)
	}
	d := &WarmDockerDesktop{root: root}
	if err := d.checkLocalSubpath("sibling"); err == nil {
		t.Fatal("volume alias bypassed physical scope")
	}
}

func TestWarmDesktopStoppedForeignBindVolume(t *testing.T) {
	v := desktopVolume{Driver: "local", Options: map[string]string{"device": "/aliased-slot", "type": "none", "o": "bind"}}
	paths, err := v.sources("/docker/volumes/other/_data")
	if err != nil || !slices.Contains(paths, "/aliased-slot") {
		t.Fatal("stopped volume device omitted", paths, err)
	}
	v = desktopVolume{Driver: "external"}
	if _, err := v.sources("/volume"); err == nil {
		t.Fatal("opaque volume driver qualified")
	}
}

func TestWarmDesktopThreadHandlesAndMappings(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "file with spaces"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(4096); err != nil {
		t.Fatal(err)
	}
	st, err := desktopStat(dir)
	if err != nil {
		t.Fatal(err)
	}
	table, err := readDesktopMountInfo("/proc/thread-self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	scope := desktopMountInfo{device: desktopDevice(st)}
	scope.root, err = desktopHandlePath(scope, table, st.Mnt_id, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkDesktopTask("/proc/thread-self", scope); !errors.Is(err, errDesktopBusy) {
		t.Fatalf("file descriptor: %v", err)
	}
	mapping, err := unix.Mmap(int(f.Fd()), 0, 4096, unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Munmap(mapping)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := checkDesktopTask("/proc/thread-self", scope); !errors.Is(err, errDesktopBusy) {
		t.Fatalf("closed descriptor with live mapping: %v", err)
	}
}

func TestWarmDesktopFenceRequiresCanonicalWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.Symlink("/tmp", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	d := &WarmDockerDesktop{root: root}
	if err := d.Fence(t.Context(), filepath.Join(root, "alias")); err == nil || !strings.Contains(err.Error(), "canonical workspace") {
		t.Fatalf("unqualified alias reached Docker: %v", err)
	}
}
