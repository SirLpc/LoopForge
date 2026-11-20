package runtime

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWarmDockerCommandsUseVerifiedContainerID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CALLS", filepath.Join(dir, "calls"))
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(`#!/bin/sh
case "$1" in
exec) shift; while [ "$1" = "-e" ]; do shift 2; done; echo "$1" >>"$CALLS";;
inspect) for id in "$@"; do :; done; echo "$id" >>"$CALLS"; echo 'true|generation|now';;
esac
`), 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	sb := &dockerSandbox{id: "demo", name: "mutable-container", workspace: dir, started: true, warmTaskID: id, requirements: SandboxRequirements{WarmDockerVolume: &WarmDockerVolume{}, ConversationRoot: dir}}
	if _, err := sb.Exec(t.Context(), CommandSpec{Argv: []string{"true"}, OwnProcessGroup: true}, nil); err != nil {
		t.Fatal(err)
	}
	cmd, launch, err := sandboxTerminalCommandWithBinding(t.Context(), sb, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSandboxEnvironmentIdentity(t.Context(), sb); err != nil {
		t.Fatal(err)
	}
	sb.warmTaskID = strings.Repeat("b", 64) // next generation started before the old terminal cleaned up
	sandboxTerminalCleanup(sb, launch)
	if !slices.Contains(cmd.Args, id) {
		t.Errorf("terminal missing verified ID: %v", cmd.Args)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(strings.Fields(string(raw)), []string{id, id, id, id}) {
		t.Errorf("exec, process stop, identity or terminal stop used mutable target: %s", raw)
	}
}
