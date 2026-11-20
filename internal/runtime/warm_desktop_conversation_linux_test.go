package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDesktopConversationOwnership(t *testing.T) {
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", "")
	root := t.TempDir()
	work := filepath.Join(root, "work")
	key := strings.Repeat("a", 64)
	owner := filepath.Join(work, "runs", "owner", "workspace")
	object := filepath.Join(root, "cache", "object-dir", "objects")
	for _, path := range []string{owner, object} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	d := &WarmDockerDesktop{root: root, backing: "/var/lib/docker/volumes/pool/_data", volume: "pool"}
	if err := d.SetConversationScope(work, func(ws string) ([]string, bool) { return []string{object}, ws == owner }); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name              string
		transient, leased bool
	}{{"text", false, false}, {"transient", true, false}, {"leased", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			session := filepath.Join(work, ".conversations", key)
			prefix := "/agentflow-wsl-conversation-"
			if tc.transient {
				session = filepath.Join(work, ".conversations", ".transient", key)
				prefix += "transient-"
			}
			home := filepath.Join(session, "home")
			ws := filepath.Join(session, "ws")
			if err := os.MkdirAll(ws, 0700); err != nil {
				t.Fatal(err)
			}
			parents, files, err := conversationGuardPaths(session)
			if err != nil {
				t.Fatal(err)
			}
			source := ws
			if tc.leased {
				source = owner
			}
			c := desktopContainer{Name: prefix + key, ID: strings.Repeat("b", 64)}
			c.Config.Labels = map[string]string{"io.loopforge.warm-conversation-root": session, "io.loopforge.warm-workspace": source}
			i := warmDockerInspection{Name: c.Name, ID: c.ID}
			i.Config.Labels = c.Config.Labels
			i.HostConfig.CapDrop = []string{"ALL"}
			i.HostConfig.SecurityOpt = []string{"no-new-privileges"}
			add := func(s, dst string, rw bool) {
				sub, err := filepath.Rel(root, s)
				if err != nil {
					t.Fatal(err)
				}
				i.Mounts = append(i.Mounts, warmDockerMount{Type: "volume", Name: "pool", Source: d.backing, Destination: dst, RW: rw})
				i.HostConfig.Mounts = append(i.HostConfig.Mounts, warmDockerMountRequest{Type: "volume", Source: "pool", Target: dst, ReadOnly: !rw, VolumeOptions: &struct {
					Subpath string
					NoCopy  bool
				}{sub, true}})
			}
			add(source, "/workspace", true)
			add(session, session, false)
			add(home, home, true)
			add(source, ws, true)
			for _, p := range parents {
				add(p, p, true)
			}
			for _, p := range files {
				add(p, p, false)
			}
			if tc.leased {
				add(object, object, false)
			}
			if err := d.verifyConversation(c, &i); err != nil {
				t.Fatalf("authorized %s: %v", tc.name, err)
			}
			original := c.Name
			c.Name, i.Name = "/foreign", "/foreign"
			if err := d.verifyConversation(c, &i); err == nil {
				t.Fatal("foreign container name accepted")
			}
			c.Name, i.Name = original, original
			for index, m := range i.Mounts {
				c.Mounts = append(c.Mounts, desktopMount{Type: m.Type, Name: m.Name, Source: m.Source, Destination: m.Destination, RW: m.RW})
				r := i.HostConfig.Mounts[index]
				spec := desktopMountSpec{Type: r.Type, Source: r.Source, Target: r.Target}
				spec.VolumeOptions.Subpath = r.VolumeOptions.Subpath
				c.HostConfig.Mounts = append(c.HostConfig.Mounts, spec)
			}
			unrelated := filepath.Join(work, "runs", "another", "workspace")
			if err := d.checkVolumeClient(t.Context(), c, unrelated); err != nil {
				t.Fatalf("unrelated lease return affected this session: %v", err)
			}
			c.Config.Labels["io.loopforge.warm-workspace"] = filepath.Join(work, "runs", "unleased", "workspace")
			if err := d.verifyConversation(c, &i); err == nil {
				t.Fatal("unleased workspace accepted")
			}
			c.Config.Labels["io.loopforge.warm-workspace"] = source
			i.Mounts[1].RW = true
			if err := d.verifyConversation(c, &i); err == nil {
				t.Fatal("writable session root accepted")
			}
			i.Mounts[1].RW = false
			add(filepath.Join(root, "tools"), "/unexpected", true)
			if err := d.verifyConversation(c, &i); err == nil {
				t.Fatal("extra writable tools mount accepted")
			}
		})
	}
}

func TestDesktopGenericCarrierCannotMountConversationState(t *testing.T) {
	d := &WarmDockerDesktop{root: "/pool", volume: "pool", backing: "/docker/volumes/pool/_data"}
	work := "/pool/work"
	workspace := "/pool/work/runs/owner/workspace"
	err := d.SetConversationScope(work, func(ws string) ([]string, bool) { return nil, ws == workspace })
	if err != nil {
		t.Fatal(err)
	}
	c := desktopContainer{ID: "other", Name: "/agentflow-fixture"}
	c.Config.Labels = map[string]string{"io.loopforge.warm-workspace": workspace}
	for _, r := range []struct {
		sub, dst string
		rw       bool
	}{{"work/runs/owner/workspace", "/workspace", true}, {"work/.conversations/" + strings.Repeat("a", 64) + "/home", "/data", false}} {
		c.Mounts = append(c.Mounts, desktopMount{Type: "volume", Name: "pool", Source: d.backing, Destination: r.dst, RW: r.rw})
		m := desktopMountSpec{Type: "volume", Source: "pool", Target: r.dst}
		m.VolumeOptions.Subpath = r.sub
		c.HostConfig.Mounts = append(c.HostConfig.Mounts, m)
	}
	if err := d.checkDedicated(c); err == nil {
		t.Fatal("generic carrier gained access to private conversation HOME")
	}
}
