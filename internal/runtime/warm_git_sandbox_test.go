package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestWarmGitSandboxObjectsReadOnly(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	cache := filepath.Join(root, "cache", "objects")
	for _, dir := range []string{workspace, cache} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	req := SandboxRequirements{GitObjectDirs: []string{cache}}
	args, err := dockerGitObjectMounts(req)
	if err != nil || !slices.Contains(args, "type=bind,src="+cache+",dst="+cache+",readonly") {
		t.Fatalf("docker args = %v, %v", args, err)
	}
	args, err = bwrapArgsCommand(workspace, req)
	if err != nil || !strings.Contains(strings.Join(args, " "), "--ro-bind "+cache+" "+cache) {
		t.Fatalf("bwrap args = %v, %v", args, err)
	}
	profile, err := seatbeltProfileCommand(workspace, req)
	if err != nil || !strings.Contains(profile, "(deny file-write* (subpath "+strconv.Quote(filepath.Dir(cache))+"))") {
		t.Fatalf("seatbelt cache policy missing: %s %v", profile, err)
	}
}
func TestWarmGitSandboxNativeExecution(t *testing.T) {
	var driver, binary string
	switch runtime.GOOS {
	case "linux":
		driver, binary = "bwrap", "bwrap"
		if out, err := exec.Command(binary, "--ro-bind", "/", "/", "--", "true").CombinedOutput(); err != nil {
			t.Skipf("bwrap unavailable: %s %v", out, err)
		}
	case "darwin":
		driver, binary = "seatbelt", "sandbox-exec"
	default:
		t.Skip("unsupported native sandbox")
	}
	ws, objects, oid := warmGitSandboxFixture(t)
	sb := processSandbox{id: "warm", workspace: ws, driver: driver, binary: binary, profile: domain.SandboxProfile{}, requirements: SandboxRequirements{GitCacheRoot: filepath.Dir(filepath.Dir(objects)), GitObjectDirs: []string{objects}}}
	// bwrap puts a private tmpfs over /tmp; use the per-process workspace bind
	// to keep this temporary checkout visible to the actual command.
	cmd, err := sb.command(t.Context(), CommandSpec{Argv: []string{"sh", "-c", `test "$(git cat-file blob "$1")" = "trusted object" && ! touch "$2/new"`, "sh", oid, objects}, privateBinds: []string{ws}}, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %s %v", driver, out, err)
	}
}

func warmGitSandboxFixture(t *testing.T) (ws, objects, oid string) {
	t.Helper()
	root := t.TempDir()
	ws = filepath.Join(root, "workspace")
	cache := filepath.Join(root, "private", "db.git")
	if err := os.MkdirAll(filepath.Dir(cache), 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--bare", cache}, {"init", ws}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v", out, err)
		}
	}
	cmd := exec.Command("git", "--git-dir="+cache, "hash-object", "-w", "--stdin")
	cmd.Stdin = strings.NewReader("trusted object")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	objects = filepath.Join(cache, "objects")
	if err := os.WriteFile(filepath.Join(ws, ".git/objects/info/alternates"), []byte(objects+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return ws, objects, strings.TrimSpace(string(out))
}
func TestWarmGitSandboxDockerExecution(t *testing.T) {
	image := os.Getenv("AGENTFLOW_WARM_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("set AGENTFLOW_WARM_TEST_DOCKER_IMAGE to a local image containing git")
	}
	ws, objects, oid := warmGitSandboxFixture(t)
	mounts, err := dockerGitObjectMounts(SandboxRequirements{GitObjectDirs: []string{objects}})
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"run", "--pull=never", "--rm", "--network=none", "--cap-drop=ALL", "--security-opt", "no-new-privileges", "--user", strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid()), "-v", ws + ":/workspace", "-w", "/workspace"}
	args = append(args, mounts...)
	args = append(args, "--entrypoint", "sh", image, "-c", `test "$(git cat-file blob "$1")" = "trusted object" && ! touch "$2/new"`, "sh", oid, objects)
	if out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("read-only Git object bind: %s %v", out, err)
	}
}

func TestWarmGitSandboxRejectsLocalDriver(t *testing.T) {
	ws := t.TempDir()
	sb, err := OpenRemoteSandbox(t.Context(), "warm", ws, domain.SandboxProfile{Driver: "local"}, SandboxRequirements{GitObjectDirs: []string{"/private/cache/objects"}}, SandboxResources{})
	if err == nil {
		sb.Close(t.Context())
		t.Fatal("local driver accepted a warm object cache")
	}
}
func TestWarmGitSandboxConcealsIdleCaches(t *testing.T) {
	root := t.TempDir()
	ws := t.TempDir()
	req := SandboxRequirements{GitCacheRoot: root}
	args, err := bwrapArgsCommand(ws, req)
	if err != nil || !strings.Contains(strings.Join(args, " "), "--tmpfs "+root+" --remount-ro "+root) {
		t.Fatalf("unassigned native cache root not masked: %v %v", args, err)
	}
	profile, err := seatbeltProfileCommand(ws, req)
	if err != nil || !strings.Contains(profile, "(deny file-read* file-write* (subpath "+strconv.Quote(root)+"))") {
		t.Fatalf("unassigned seatbelt cache accessible: %v %v", profile, err)
	}
}

func TestWarmDockerSandboxChecksActualHostIsolation(t *testing.T) {
	ws := t.TempDir()
	objects := filepath.Join(t.TempDir(), "objects")
	if err := os.Mkdir(objects, 0700); err != nil {
		t.Fatal(err)
	}
	good := warmDockerInspection{}
	good.HostConfig.CapDrop = []string{"ALL"}
	good.HostConfig.SecurityOpt = []string{"no-new-privileges"}
	good.Mounts = []warmDockerMount{{Type: "bind", Source: ws, Destination: "/workspace", RW: true}, {Type: "bind", Source: objects, Destination: objects}}
	dirs := []string{objects}
	if err := good.validate(ws, dirs, ""); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.HostConfig.Privileged = true
	if bad.validate(ws, dirs, "") == nil {
		t.Fatal("privileged warm runtime accepted")
	}
	for _, mount := range []warmDockerMount{{Type: "bind", Source: "/var/run", Destination: "/var/run"}, {Type: "bind", Source: "/", Destination: "/host", RW: true}, {Type: "bind", Source: "/var/run/docker.sock", Destination: "/runtime"}} {
		bad = good
		bad.Mounts = append(append([]warmDockerMount(nil), good.Mounts...), mount)
		if bad.validate(ws, dirs, "") == nil {
			t.Fatalf("unexpected mount accepted: %v", mount)
		}
	}
}

func TestWarmDockerWorkspaceContainerLifecycle(t *testing.T) {
	image := os.Getenv("AGENTFLOW_WARM_TEST_DOCKER_IMAGE")
	if image == "" {
		t.Skip("requires a local Docker image containing Git")
	}
	ws, objects, oid := warmGitSandboxFixture(t)
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", "")
	name := "loopforge-test-warm-" + filepath.Base(t.TempDir()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	command := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %s %v", args[0], out, err)
		}
		return strings.TrimSpace(string(out))
	}
	requires := SandboxRequirements{GitObjectDirs: []string{objects}}
	sb := dockerSandbox{id: name, name: name, builtImage: image, workspace: ws, requirements: requires}
	args := []string{"create", "--pull=never", "--name", name, "--init", "--network=none", "--cap-drop=ALL", "--security-opt", "no-new-privileges", "--user", strconv.Itoa(os.Geteuid()) + ":" + strconv.Itoa(os.Getegid()), "-v", ws + ":/workspace"}
	binds, err := dockerGitObjectMounts(requires)
	if err != nil {
		t.Fatal(err)
	}
	args = append(args, binds...)
	command(append(args, "--entrypoint", "sh", image, "-c", "sleep 600 & wait")...)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	if err := sb.verifyWarmDocker(t.Context()); err != nil {
		t.Fatal(err)
	}
	command("start", name)
	command("exec", name, "sh", "-c", `cd /workspace && test "$(git cat-file blob "$1")" = "trusted object" && ! touch "$2/new"`, "sh", oid, objects)
	if err := sb.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := command("ps", "-aq", "--filter", "name=^/"+name+"$"); got != "" {
		t.Fatal("container survived teardown", got)
	}
}
