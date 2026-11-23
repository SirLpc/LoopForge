package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestBwrapWarmConversationCanReadStagedConfig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bubblewrap is Linux-only")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Skip("bubblewrap is unavailable")
	}
	if out, err := exec.CommandContext(t.Context(), bwrap, "--unshare-user", "--ro-bind", "/", "/", "--", "/bin/true").CombinedOutput(); err != nil {
		t.Skipf("bubblewrap namespaces are unavailable: %s", out)
	}
	runnerRoot := t.TempDir()
	conversation := filepath.Join(runnerRoot, ".conversations", "current")
	sibling := filepath.Join(runnerRoot, ".conversations", "sibling")
	platformWorkspace := filepath.Join(conversation, "ws")
	runWorkspace := filepath.Join(runnerRoot, "runs", "current", "workspace")
	for _, dir := range []string{filepath.Join(conversation, "home"), platformWorkspace, runWorkspace, sibling} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	config := filepath.Join(conversation, "current-mcp.json")
	if err := os.WriteFile(config, []byte("staged MCP config"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, workspace := range []string{platformWorkspace, runWorkspace} {
		args, err := bwrapArgsCommand(workspace, SandboxRequirements{
			ConversationRoot: conversation,
			NativeWarm:       &NativeWarmIsolation{WorkspaceRoot: runnerRoot},
		})
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, "/bin/sh", "-ec", `
			test "$(cat "$1")" = "staged MCP config"
			test ! -e "$2"
			! touch "$3/readonly-probe" 2>/dev/null || exit 1
			touch "$3/home/home-probe" "$3/ws/workspace-probe"
		`, "test", config, sibling, conversation)
		if out, err := exec.CommandContext(t.Context(), bwrap, args...).CombinedOutput(); err != nil {
			t.Fatalf("warm conversation in %s: %v: %s", workspace, err, out)
		}
	}
}

func stubBunFSMountPoint(t *testing.T, path string) string {
	t.Helper()
	previous := bunFSMountPoint
	bunFSMountPoint = path
	t.Cleanup(func() { bunFSMountPoint = previous })
	return path
}

func TestBwrapBunFSMountPoint(t *testing.T) {
	workspace := t.TempDir()
	claude := ProviderRequirements("claude-code")

	// Missing but creatable: created once, then mounted.
	created := stubBunFSMountPoint(t, filepath.Join(t.TempDir(), "$bunfs"))
	args, err := bwrapArgsCommand(workspace, claude)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "--tmpfs "+created) {
		t.Fatalf("creatable mount point not mounted: %v", args)
	}
	if info, err := os.Stat(created); err != nil || !info.IsDir() {
		t.Fatalf("mount point not created: %v", err)
	}

	// Cannot be created (parent missing / read-only root): skip the mount
	// instead of failing the whole sandbox.
	missing := stubBunFSMountPoint(t, filepath.Join(t.TempDir(), "absent", "$bunfs"))
	args, err = bwrapArgsCommand(workspace, claude)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(args, " "), missing) {
		t.Fatalf("uncreatable mount point must be skipped: %v", args)
	}

	// CLIs without BunFS never touch it.
	untouched := stubBunFSMountPoint(t, filepath.Join(t.TempDir(), "$bunfs"))
	if _, err := bwrapArgsCommand(workspace, ProviderRequirements("codex")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(untouched); !os.IsNotExist(err) {
		t.Fatalf("codex created the BunFS mount point: %v", err)
	}
}

func TestNativeSandboxResolvesPerOS(t *testing.T) {
	for _, tc := range []struct{ driver, goos, want string }{
		{"native", "darwin", "seatbelt"},
		{"native", "linux", "bwrap"},
		{" native ", "Linux", "bwrap"},
		{"native", "", "native"},
		{"docker", "linux", "docker"},
		{"local", "darwin", "local"},
		{"bwrap", "darwin", "bwrap"},
	} {
		got, err := domain.ResolveSandboxDriver(tc.driver, tc.goos)
		if err != nil || got != tc.want {
			t.Fatalf("ResolveSandboxDriver(%q, %q) = %q, %v; want %q", tc.driver, tc.goos, got, err, tc.want)
		}
	}
	if _, err := domain.ResolveSandboxDriver("native", "windows"); err == nil {
		t.Fatal("native on windows must fail")
	}
}

func TestOpenRemoteSandboxResolvesNative(t *testing.T) {
	// On the test host native must become the host's lightweight sandbox,
	// never reach the driver switch as "native".
	_, err := OpenRemoteSandbox(t.Context(), "native", t.TempDir(), domain.SandboxProfile{ID: "n", Driver: "native"}, SandboxRequirements{}, SandboxResources{})
	if err != nil && strings.Contains(err.Error(), "unsupported runner sandbox driver") {
		t.Fatalf("native leaked to the driver switch: %v", err)
	}
}
