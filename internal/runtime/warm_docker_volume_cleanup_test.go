package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWarmVolumeCloseKeepsForeignNames(t *testing.T) {
	dir := t.TempDir()
	workspace := filepath.Join(dir, "workspace")
	if err := os.Mkdir(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FIXTURES", dir)
	// The expected task name exists but belongs to a different owner. A
	// destructive name lookup would also remove its unrelated companions.
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(`#!/bin/sh
case "$1" in
inspect) echo '[{"ID":"foreign","Name":"/agentflow-task","Config":{"Image":"other"}}]';;
rm) echo "$3" >>"$FIXTURES/removed";;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	sb := dockerSandbox{name: "agentflow-task", workspace: workspace, requirements: SandboxRequirements{WarmDockerVolume: &WarmDockerVolume{Name: "test-volume", Root: dir, Backing: "/var/lib/docker/volumes/test/_data"}}}
	err := sb.Close(t.Context())
	removed, _ := os.ReadFile(filepath.Join(dir, "removed"))
	if err == nil || len(strings.TrimSpace(string(removed))) != 0 {
		t.Fatalf("close error=%v removed=%q", err, removed)
	}
}
