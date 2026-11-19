package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"

	"agentflow/internal/domain"
)

type warmPoolFixture struct {
	root, config, workspace, journal, homeA, homeB string
}

func newWarmPoolFixture(t *testing.T) warmPoolFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := warmPoolFixture{
		root: root, config: filepath.Join(root, "pool.json"),
		workspace: filepath.Join(root, "work"),
		journal:   filepath.Join(root, "state", "pool.json"),
		homeA:     filepath.Join(root, "slots", "one"),
		homeB:     filepath.Join(root, "slots", "two"),
	}
	for _, dir := range []string{f.workspace, f.homeA, f.homeB} {
		warmPoolMkdir(t, dir)
	}
	f.writeSlots(t, f.homeA, f.homeB)
	return f
}

func (f warmPoolFixture) writeSlots(t *testing.T, paths ...string) {
	t.Helper()
	ids := []string{"a", "b"}
	var slots []map[string]string
	for i, path := range paths {
		slots = append(slots, map[string]string{
			"id": ids[i], "projectId": "project", "repositoryId": ids[i], "path": path, "unityProjectPath": ".",
		})
	}
	data, err := json.Marshal(map[string]any{"slots": slots})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f warmPoolFixture) open(t *testing.T) *warmWorkspacePool {
	t.Helper()
	pool, err := openWarmWorkspacePool(f.config, f.workspace, f.journal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.close() })
	return pool
}

func warmPoolMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func warmPoolStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info
}

func warmPoolRequireSame(t *testing.T, before os.FileInfo, after string) {
	t.Helper()
	if !os.SameFile(before, warmPoolStat(t, after)) {
		t.Fatalf("directory %s changed inode during workspace transfer", after)
	}
}

func TestWarmWorkspacePoolPaths(t *testing.T) {
	cases := map[string]func(*testing.T, warmPoolFixture){
		"duplicate paths": func(t *testing.T, f warmPoolFixture) {
			f.writeSlots(t, f.homeA, f.homeA)
		},
		"nested paths": func(t *testing.T, f warmPoolFixture) {
			child := filepath.Join(f.homeA, "child")
			warmPoolMkdir(t, child)
			f.writeSlots(t, f.homeA, child)
		},
		"symlink": func(t *testing.T, f warmPoolFixture) {
			link := filepath.Join(f.root, "link")
			if err := os.Symlink(f.homeA, link); err != nil {
				t.Fatal(err)
			}
			f.writeSlots(t, link)
		},
		"symlink ancestor": func(t *testing.T, f warmPoolFixture) {
			link := filepath.Join(f.root, "link")
			if err := os.Symlink(filepath.Dir(f.homeA), link); err != nil {
				t.Fatal(err)
			}
			f.writeSlots(t, filepath.Join(link, filepath.Base(f.homeA)))
		},
		"workspace overlap": func(t *testing.T, f warmPoolFixture) {
			child := filepath.Join(f.workspace, "child")
			warmPoolMkdir(t, child)
			f.writeSlots(t, child)
		},
		"state overlap": func(t *testing.T, f warmPoolFixture) {
			child := filepath.Join(filepath.Dir(f.journal), "child")
			warmPoolMkdir(t, child)
			f.writeSlots(t, child)
		},
		"different filesystems": func(t *testing.T, f warmPoolFixture) {
			slot, err := filepath.EvalSymlinks("/dev/shm")
			if err != nil || !warmPoolDifferentDevice(t, f.workspace, slot) {
				t.Skip("separate mounted filesystem unavailable")
			}
			f.writeSlots(t, slot)
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			f := newWarmPoolFixture(t)
			arrange(t, f)
			pool, err := openWarmWorkspacePool(f.config, f.workspace, f.journal)
			if err == nil {
				_ = pool.close()
				t.Fatal("unsafe pool configuration accepted")
			}
		})
	}
}

func TestWarmWorkspacePoolMoveAndRestart(t *testing.T) {
	f := newWarmPoolFixture(t)
	pool := f.open(t)
	lease := domain.WarmWorkspaceLease{
		OwnerRunID: "original", RunnerID: "runner", Generation: 7,
		Slots: map[string]string{"a": "a", "b": "b"},
	}
	originalA, originalB := warmPoolStat(t, f.homeA), warmPoolStat(t, f.homeB)
	root := filepath.Join(f.workspace, "runs", lease.OwnerRunID, "workspace")
	targetA, targetB := filepath.Join(root, "first"), filepath.Join(root, "second")
	targets := map[string]string{"a": targetA, "b": targetB}
	if err := pool.reserve(lease); err != nil {
		t.Fatal(err)
	}
	warmPoolMkdir(t, targetB)
	obstacle := warmPoolStat(t, targetB)
	if err := pool.place(t.Context(), lease, targets); err == nil {
		t.Fatal("placement overwrote an existing directory")
	}
	warmPoolRequireSame(t, obstacle, targetB)
	if err := os.Remove(targetB); err != nil {
		t.Fatal(err)
	}
	// Move the first repository after the intent was written, as if the
	// process exited before it could record completion.
	if _, err := os.Stat(targetA); os.IsNotExist(err) {
		if err := os.Rename(f.homeA, targetA); err != nil {
			t.Fatal(err)
		}
	}
	warmPoolRequireSame(t, originalA, targetA)
	warmPoolRequireSame(t, originalB, f.homeB)
	if err := pool.close(); err != nil {
		t.Fatal(err)
	}
	pool = f.open(t)

	older := lease
	older.Generation--
	if err := pool.place(t.Context(), older, targets); err == nil {
		t.Fatal("stale lease placed repositories")
	}
	if err := pool.returnSlots(t.Context(), older); err == nil {
		t.Fatal("stale lease returned repositories")
	}
	partial := lease
	partial.Slots = map[string]string{"a": "a"}
	if err := pool.returnSlots(t.Context(), partial); err == nil {
		t.Fatal("partial lease returned repositories")
	}
	next := lease
	next.OwnerRunID = "next"
	next.Generation++
	if err := pool.reserve(next); err == nil {
		t.Fatal("new owner acquired an active slot")
	}
	if err := pool.place(t.Context(), lease, targets); err != nil {
		t.Fatal(err)
	}
	warmPoolRequireSame(t, originalA, targetA)
	warmPoolRequireSame(t, originalB, targetB)

	warmPoolMkdir(t, f.homeB)
	if err := pool.returnSlots(t.Context(), lease); err == nil {
		t.Fatal("return overwrote an existing directory")
	}
	if err := os.Remove(f.homeB); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.homeA); os.IsNotExist(err) {
		if err := os.Rename(targetA, f.homeA); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.close(); err != nil {
		t.Fatal(err)
	}
	pool = f.open(t)

	if err := pool.reserve(next); err == nil {
		t.Fatal("new owner acquired slots before the partial return completed")
	}
	if err := pool.returnSlots(t.Context(), lease); err != nil {
		t.Fatal(err)
	}
	warmPoolRequireSame(t, originalA, f.homeA)
	warmPoolRequireSame(t, originalB, f.homeB)
	if err := pool.returnSlots(t.Context(), lease); err != nil {
		t.Fatalf("retry completed return: %v", err)
	}
	if err := pool.reserve(lease); err == nil {
		t.Fatal("completed generation reserved a slot again")
	}
	if err := pool.reserve(next); err != nil {
		t.Fatalf("reserve safely returned slots: %v", err)
	}
	if err := pool.place(t.Context(), lease, targets); err == nil {
		t.Fatal("old owner placed a newly allocated slot")
	}
	if err := pool.returnSlots(t.Context(), lease); err == nil {
		t.Fatal("old owner returned a newly allocated slot")
	}
}

func TestWarmWorkspacePoolLocksJournal(t *testing.T) {
	f := newWarmPoolFixture(t)
	f.open(t)
	pool, err := openWarmWorkspacePool(f.config, f.workspace, f.journal)
	if err == nil {
		_ = pool.close()
		t.Fatal("second pool opened a locked journal")
	}
}

func TestWarmWorkspacePoolQuarantinesMissingInode(t *testing.T) {
	f := newWarmPoolFixture(t)
	pool := f.open(t)
	lease := domain.WarmWorkspaceLease{
		OwnerRunID: "owner", RunnerID: "runner", Generation: 1, Slots: map[string]string{"a": "a"},
	}
	if err := pool.reserve(lease); err != nil {
		t.Fatal(err)
	}
	if err := pool.close(); err != nil {
		t.Fatal(err)
	}
	detour := filepath.Join(f.root, "detour")
	if err := os.Rename(f.homeA, detour); err != nil {
		t.Fatal(err)
	}
	warmPoolMkdir(t, f.homeA)
	pool = f.open(t)
	if err := pool.place(t.Context(), lease, map[string]string{"a": "project"}); err == nil {
		t.Fatal("a replacement inode was placed as the original slot")
	}
	if err := pool.returnSlots(t.Context(), lease); err == nil {
		t.Fatal("a replacement inode was returned as the original slot")
	}
	lease.OwnerRunID, lease.Generation = "other", 2
	if err := pool.reserve(lease); err == nil {
		t.Fatal("a quarantined slot became available")
	}
	lease.Slots = map[string]string{"b": "b"}
	if err := pool.reserve(lease); err != nil {
		t.Fatalf("unrelated slot did not remain available: %v", err)
	}
}

func warmPoolDifferentDevice(t *testing.T, first, second string) bool {
	t.Helper()
	var a, b unix.Stat_t
	if err := unix.Stat(first, &a); err != nil {
		t.Fatal(err)
	}
	if err := unix.Stat(second, &b); err != nil {
		t.Fatal(err)
	}
	return a.Dev != b.Dev
}

func TestWarmWorkspacePoolRechecksUnityPathBeforeReservation(t *testing.T) {
	f := newWarmPoolFixture(t)
	data, err := json.Marshal(map[string]any{"slots": []map[string]string{{
		"id": "a", "projectId": "project", "repositoryId": "a", "path": f.homeA, "unityProjectPath": "Unity",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	projectPath := filepath.Join(f.homeA, "Unity")
	warmPoolMkdir(t, projectPath)
	pool := f.open(t)
	if err := pool.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(projectPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.homeB, projectPath); err != nil {
		t.Fatal(err)
	}
	pool = f.open(t)
	lease := domain.WarmWorkspaceLease{
		OwnerRunID: "owner", RunnerID: "runner", Generation: 1, Slots: map[string]string{"a": "a"},
	}
	if err := pool.reserve(lease); err == nil {
		t.Fatal("a slot with a symlinked Unity project became available after restart")
	}
}
