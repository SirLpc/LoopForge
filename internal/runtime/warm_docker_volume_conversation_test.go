package runtime

import (
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestWarmVolumeConversationCannotWidenTaskSandboxInPlace(t *testing.T) {
	dir := t.TempDir()
	conv := filepath.Join(dir, ".conversations", strings.Repeat("a", 64))
	sb := &dockerSandbox{requirements: SandboxRequirements{WarmDockerVolume: &WarmDockerVolume{Root: dir}}}
	err := ConfigureConversationSandbox(t.Context(), sb, conv)
	if err == nil {
		t.Fatal("ordinary warm sandbox can be reconfigured for a conversation")
	}
	if _, err := os.Stat(filepath.Join(conv, "ws")); !os.IsNotExist(err) {
		t.Fatal("in-place conversion touched private conversation state")
	}
}

func TestWarmConversationVolumeGuardProvisioning(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "work/.conversations", strings.Repeat("a", 64))
	workspace := filepath.Join(dir, "work/runs/owner/workspace")
	v := &WarmDockerVolume{Name: "pool", Root: dir, Backing: "/docker/pool"}
	for _, path := range []string{filepath.Join(root, "home"), filepath.Join(root, "ws"), workspace} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	parent := filepath.Join(root, "home/.cursor")
	outside := t.TempDir()
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := warmVolumeConversationArgs(v, root, workspace); err == nil {
		t.Fatal("followed a symlinked guard parent")
	}
	if _, err := os.Stat(filepath.Join(outside, "hooks.json")); !os.IsNotExist(err) {
		t.Fatal("altered an external guard")
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	args, err := warmVolumeConversationArgs(v, root, workspace)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, "\n")
	wantFile := filepath.Join(parent, "hooks.json")
	for _, expected := range []string{
		"dst=" + root + ",volume-subpath=work/.conversations/" + strings.Repeat("a", 64) + ",volume-nocopy,readonly",
		"dst=" + root + "/ws,volume-subpath=work/runs/owner/workspace,volume-nocopy\n",
		"dst=" + wantFile + ",volume-subpath=work/.conversations/" + strings.Repeat("a", 64) + "/home/.cursor/hooks.json,volume-nocopy,readonly",
	} {
		if !strings.Contains(joined+"\n", expected) {
			t.Fatalf("missing expected mount %q: %s", expected, joined)
		}
	}
	if got, err := os.ReadFile(wantFile); err != nil || string(got) != "{}" {
		t.Fatalf("guard = %q, %v", got, err)
	}
	for _, other := range []string{filepath.Dir(root), filepath.Join(dir, "work/runs"), filepath.Join(dir, "work/runs/owner/workspace/subdir")} {
		if _, err := warmConversationMounts(root, other); err == nil {
			t.Fatalf("unapproved workspace layout %q", other)
		}
	}
}

func TestWarmVolumeConversationLifecycle(t *testing.T) {
	image := os.Getenv("AGENTFLOW_WARM_TEST_VOLUME_IMAGE")
	if image == "" {
		t.Skip("set AGENTFLOW_WARM_TEST_VOLUME_IMAGE to a prepared local image containing sh")
	}
	t.Setenv("AGENTFLOW_ISOLATED_IMAGE", image)
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", "")
	t.Setenv("AGENTFLOW_CONVERSATION_RELAY_HOST", "host.docker.internal")
	t.Setenv("AGENTFLOW_CONVERSATION_DOCKER_NETWORK", "none")
	name := "loopforge-test-warm-session-" + strings.ToLower(rand.Text())
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v: %s", args[0], err, out)
		}
		return strings.TrimSpace(string(out))
	}
	base := t.TempDir()
	root := filepath.Join(base, "work/.conversations", strings.Repeat("c", 64))
	text := filepath.Join(root, "ws")
	leased := filepath.Join(base, "work/runs/owner/workspace")
	for _, p := range []string{text, leased} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := conversationGuardPaths(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "settings"), []byte("fixed"), 0666); err != nil {
		t.Fatal(err)
	}
	docker("volume", "create", name)
	t.Cleanup(func() {
		if err := exec.Command("docker", "volume", "rm", name).Run(); err != nil {
			t.Error("remove test volume:", err)
		}
	})
	v := &WarmDockerVolume{Name: name, Root: base, Backing: docker("volume", "inspect", "--format", "{{.Mountpoint}}", name)}
	seed := docker("create", "--pull=never", "--mount", "type=volume,src="+name+",dst=/fixture", "--entrypoint", "true", image)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", seed).Run() })
	docker("cp", base+"/.", seed+":/fixture")
	docker("rm", seed)
	docker("run", "--rm", "--pull=never", "--network=none", "--mount", "type=volume,src="+name+",dst=/fixture", "--entrypoint", "sh", image, "-ec", `
	chmod -R a+rwX /fixture
	echo private >/fixture/work/.conversations/"$1"/ws/source
	echo placed >/fixture/work/runs/owner/workspace/source`, "sh", strings.Repeat("c", 64))
	for _, tc := range []struct{ phase, ws, value string }{{"text", text, "private"}, {"leased", leased, "placed"}} {
		t.Run(tc.phase, func(t *testing.T) {
			sb := &dockerSandbox{name: name, workspace: tc.ws, requirements: SandboxRequirements{ConversationRoot: root, WarmDockerVolume: v}, dockerfile: domain.DockerfileDefinition{Content: IsolatedDockerfile}}
			t.Cleanup(func() {
				if err := sb.Close(context.Background()); err != nil {
					t.Errorf("cleanup: %v", err)
				}
			})
			if provider := os.Getenv("AGENTFLOW_WARM_TEST_VOLUME_CLI"); provider != "" {
				// The first text-phase operation is CLI setup, before any Sandbox.Exec.
				if _, err := ensureConversationCLI(t.Context(), sb, provider, filepath.Join(root, "home")); err != nil {
					t.Fatal(err)
				}
			} else if err := sb.ensureStarted(t.Context()); err != nil {
				t.Fatal(err)
			}
			docker("exec", name, "sh", "-ec", `
				root=$1; test "$(cat "$root/ws/source")" = "$2"
				echo allowed >"$root/home/session-cache"
				echo edited >"$root/ws/edit"
				test "$(cat /workspace/edit)" = edited
				if echo write >"$root/settings"; then exit 1; fi
				if echo write >"$root/home/.cursor/hooks.json"; then exit 1; fi
				if mv "$root/home/.cursor" "$root/home/moved"; then exit 1; fi`, "sh", root, tc.value)
			docker("stop", "-t", "1", name)
			sb.invalidate()
			if err := sb.ensureStarted(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}
