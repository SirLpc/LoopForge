package gitwork

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPatchInventoryDoesNotRefreshSharedIndex(t *testing.T) {
	workspace := t.TempDir()
	workBranchGit(t, workspace, "init", "-q")
	file := filepath.Join(workspace, "tracked.txt")
	if err := os.WriteFile(file, []byte("tracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	staged := time.Unix(1262304000, 0)
	if err := os.Chtimes(file, staged, staged); err != nil {
		t.Fatal(err)
	}
	workBranchGit(t, workspace, "add", "tracked.txt")
	workBranchGit(t, workspace, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "fixture")
	if err := RecordBaseline(workspace, ""); err != nil {
		t.Fatal(err)
	}
	touched := staged.Add(time.Hour)
	if err := os.Chtimes(file, touched, touched); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(workspace, ".git", "index")
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	diff, err := PatchInventoryContext(t.Context(), workspace)
	if err != nil || len(diff) != 0 {
		t.Fatalf("unchanged diff = %q, err = %v", diff, err)
	}
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("diff refreshed the shared index")
	}
	workBranchGit(t, workspace, "config", "core.trustctime", "false")
	workBranchGit(t, workspace, "config", "core.checkStat", "minimal")
	if err := os.WriteFile(file, []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, index} {
		if err := os.Chtimes(path, staged, staged); err != nil {
			t.Fatal(err)
		}
	}
	diff, err = PatchInventoryContext(t.Context(), workspace)
	if err != nil || !strings.Contains(string(diff), "M\ttracked.txt\t+1/-1") {
		t.Fatalf("content change inventory = %q, err = %v", diff, err)
	}
}
