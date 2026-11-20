package runtime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestWarmDockerVolumeChecksRequestAndResolvedMount(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "runs", "one")
	if err := os.MkdirAll(workspace, 0700); err != nil {
		t.Fatal(err)
	}
	v := WarmDockerVolume{Name: "test-pool", Root: root, Backing: "/var/lib/docker/volumes/test-pool/_data"}
	const fixture = `{"HostConfig":{"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges"],"Mounts":[{"Type":"volume","Source":"test-pool","Target":"/workspace","ReadOnly":false,"VolumeOptions":{"Subpath":"runs/one","NoCopy":true}}]},"Mounts":[{"Type":"volume","Name":"test-pool","Source":"/var/lib/docker/volumes/test-pool/_data","Destination":"/workspace","RW":true}]}`
	for _, tc := range []struct{ name, from, to string }{
		{name: "verified"},
		{"sibling", "runs/one", "runs/two"},
		{"volume root", "runs/one", ""},
		{"different request", `"Source":"test-pool"`, `"Source":"other"`},
		{"different resolved name", `"Name":"test-pool"`, `"Name":"other"`},
		{"different backing", `test-pool/_data`, `other/_data`},
		{"other target", `"Target":"/workspace"`, `"Target":"/other"`},
		{"other destination", `"Destination":"/workspace"`, `"Destination":"/other"`},
		{"wrong request access", `"ReadOnly":false`, `"ReadOnly":true`},
		{"wrong effective access", `"RW":true`, `"RW":false`},
		{"unrestricted capabilities", `["ALL"]`, `[]`},
		{"missing request", `"Mounts":[{"Type":"volume","Source"`, `"Removed":[{"Type":"volume","Source"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := strings.Replace(fixture, tc.from, tc.to, 1)
			var actual warmDockerInspection
			if err := json.Unmarshal([]byte(fixture), &actual); err != nil {
				t.Fatal(err)
			}
			err := actual.validateTask(workspace, SandboxRequirements{WarmDockerVolume: &v}, "")
			if (err == nil) != (tc.name == "verified") {
				t.Fatalf("validation = %v", err)
			}
		})
	}
}

func TestWarmDockerVolumeRefusesUnqualifiedSource(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "slot")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	v := &WarmDockerVolume{Name: "test-pool", Root: root, Backing: "/var/lib/docker/volumes/test-pool/_data"}
	for _, src := range []string{root, outside, link, root + "/../" + filepath.Base(root) + "/x"} {
		if _, err := dockerGitObjectMounts(SandboxRequirements{WarmDockerVolume: v, GitObjectDirs: []string{src}}); err == nil {
			t.Fatalf("accepted %q", src)
		}
	}
}

func TestWarmVolumeBrowserRequiresVerifiedParentNetwork(t *testing.T) {
	actual := warmDockerInspection{}
	actual.HostConfig.CapDrop = []string{"ALL"}
	actual.HostConfig.SecurityOpt = []string{"no-new-privileges"}
	for _, network := range []string{"host", "container:another", "bridge", "container:parent"} {
		actual.HostConfig.NetworkMode = network
		err := actual.validateBrowserHost("container:parent")
		if (err == nil) != (network == "container:parent") {
			t.Errorf("network %s accepted=%v", network, err == nil)
		}
	}
}

// A local mirror provides the runner's path view. All daemon-side test files
// live exclusively in the disposable named volume.
func TestWarmDockerVolumeLifecycle(t *testing.T) {
	image := os.Getenv("AGENTFLOW_WARM_TEST_VOLUME_IMAGE")
	if image == "" {
		t.Skip("set AGENTFLOW_WARM_TEST_VOLUME_IMAGE to a prepared local image containing sh")
	}
	t.Setenv("AGENTFLOW_ISOLATED_IMAGE", image)
	t.Setenv("AGENTFLOW_DOCKER_NETWORK", "none")
	name := "agentflow-rt-test-warm-volume-" + strings.ToLower(rand.Text())
	cli := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "docker", args...).CombinedOutput()
		if err != nil {
			t.Fatalf("docker %s: %v: %s", args[0], err, out)
		}
		return strings.TrimSpace(string(out))
	}
	root := t.TempDir()
	workspace, sibling := filepath.Join(root, "runs/one"), filepath.Join(root, "runs/two")
	objects, tools := filepath.Join(root, "cache/objects"), filepath.Join(root, "tools")
	for _, path := range []string{workspace, sibling, objects, tools} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", tools)
	helper := filepath.Join(tools, "helper")
	if err := os.WriteFile(helper, nil, 0700); err != nil {
		t.Fatal(err)
	}
	cli("volume", "create", name)
	t.Cleanup(func() {
		if out, err := exec.Command("docker", "volume", "rm", name).CombinedOutput(); err != nil {
			t.Errorf("remove test volume: %v: %s", err, out)
		}
	})
	v := &WarmDockerVolume{Name: name, Root: root, Helper: helper, Backing: cli("volume", "inspect", "--format", "{{.Mountpoint}}", name)}
	cli("run", "--rm", "--pull=never", "--network=none", "--mount", "type=volume,src="+name+",dst=/fixture", "--entrypoint", "sh", image, "-ec", `
	mkdir -p /fixture/runs/one /fixture/runs/two /fixture/cache/objects /fixture/tools
	chmod 0777 /fixture/runs/one /fixture/cache/objects /fixture/tools
	echo first >/fixture/runs/one/marker
	echo second >/fixture/runs/two/marker
	echo cached >/fixture/cache/objects/marker
	cat >/fixture/tools/helper <<'PROBE'
#!/bin/sh
set -eu
read expected
test "$(cat /workspace/marker)" = first
if test "$expected" = ro; then
	if touch /workspace/test-write; then exit 1; fi
else
	touch /workspace/test-write
fi
printf 'verified-%s\n' "$expected"
PROBE
	chmod 0755 /fixture/tools/helper
	`)
	req := SandboxRequirements{WarmDockerVolume: v, GitObjectDirs: []string{objects}}
	sb := &dockerSandbox{id: name, name: name, requirements: req, workspace: workspace, profile: domain.SandboxProfile{ReadOnlyRootFS: true}, dockerfile: domain.DockerfileDefinition{Content: IsolatedDockerfile}}
	t.Cleanup(func() {
		if err := removeDockerWorkspaceContainers(context.Background(), name); err != nil {
			t.Errorf("remove test containers: %v", err)
		}
	})
	if err := sb.ensureStarted(t.Context()); err != nil {
		t.Fatal(err)
	}
	cli("exec", name, "sh", "-ec", `test "$(cat /workspace/marker)" = first && test "$(cat "$1/marker")" = cached && test -x "$2/helper" && touch /workspace/test && ! touch "$1/wrong" && ! touch "$2/wrong"`, "sh", objects, tools)
	for _, access := range []string{"ro", "rw"} {
		out, err := (warmGitContainer{parent: name, workspace: workspace, image: image, network: "none", requirements: req, writable: access == "rw"}).run(t.Context(), []byte(access+"\n"))
		if err != nil || strings.TrimSpace(string(out)) != "verified-"+access {
			t.Fatalf("file subpath helper (%s): %s %v", access, out, err)
		}
	}
	cli("stop", "-t", "1", name)
	sb.invalidate()
	sb.workspace = sibling
	if err := sb.ensureStarted(t.Context()); err == nil {
		t.Fatal("restarted a container assigned to a different volume subpath")
	}
	if got := cli("inspect", "--format", "{{.State.Running}}", name); got != "false" {
		t.Fatal("mismatched sandbox started")
	}
	sb.workspace = workspace
	if err := sb.ensureStarted(t.Context()); err != nil {
		t.Fatal(err)
	}
	cli("exec", name, "sh", "-ec", "test -f /workspace/test")

	// A local image whose default entrypoint sleeps lets the real sidecar
	// creation path run offline without Chromium or an external image pull.
	fixture := cli("create", "--pull=never", "--label", "io.loopforge.test="+name, "--entrypoint", "sleep", image, "60")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", fixture).Run() })
	browserImage := cli("commit", fixture)
	browser := dockerBrowserContainerName(name)
	t.Cleanup(func() {
		_ = exec.Command("docker", "rm", "-f", browser).Run()
		if out, err := exec.Command("docker", "image", "rm", browserImage).CombinedOutput(); err != nil {
			t.Errorf("remove browser fixture: %v: %s", err, out)
		}
	})
	if err := sb.ensureWarmVolumeBrowser(t.Context(), name, browserImage, browser); err != nil {
		t.Fatal(err)
	}
	cli("exec", browser, "sh", "-ec", "test -f /workspace/test")
	cli("stop", "-t", "1", browser)
	if err := sb.ensureWarmVolumeBrowser(t.Context(), name, browserImage, browser); err != nil {
		t.Fatal(err)
	}
	if got := cli("inspect", "--format", "{{.State.Running}}", browser); got != "true" {
		t.Fatal("verified browser did not restart")
	}
	t.Setenv("AGENTFLOW_BROWSER_IMAGE", browserImage)
	reqFile := domain.BrowserRequest{Operation: "file", File: "/workspace/marker"}
	if exec.CommandContext(t.Context(), "docker", "exec", browser, "sh", "-ec", "command -v node").Run() == nil {
		state, err := HandleWarmDockerVolumeBrowser(t.Context(), name, workspace, req, reqFile)
		if err != nil || state.HTTP == nil || string(state.HTTP.Body) != "first\n" {
			t.Fatalf("verified browser file: %+v %v", state, err)
		}
		t.Log("served fixture file via verified Docker browser ID")
	}
	foreign := cli("run", "--rm", "-d", "--pull=never", "--network=none", "--label", browserRunLabel+"="+name, "--entrypoint", "sleep", image, "60")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", foreign).Run() })
	if _, err := HandleWarmDockerVolumeBrowser(t.Context(), name, workspace, req, reqFile); err == nil {
		t.Fatal("duplicate live browser identity accepted")
	}

	// Retiring a workspace may remove its verified task and browser only. A
	// stopped foreign container with an expected name remains visible to Fence.
	makeCollision := func(container string) string {
		return cli("create", "--pull=never", "--name", container, "--network=none", "--mount", "type=volume,src="+name+",dst=/foreign,volume-subpath=runs/one,volume-nocopy", "--entrypoint", "sleep", image, "60")
	}
	exists := func(id string) bool { return cli("ps", "-aq", "--no-trunc", "--filter", "id="+id) == id }
	scriptsID := makeCollision(name + legacyDockerScriptSuffix)
	taskID, browserID := cli("inspect", "--format", "{{.Id}}", name), cli("inspect", "--format", "{{.Id}}", browser)
	if err := sb.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if exists(taskID) || exists(browserID) || !exists(scriptsID) {
		t.Fatal("close failed to distinguish verified task and browser from foreign legacy companion")
	}
	foreignTask := makeCollision(name)
	if err := sb.Close(t.Context()); err == nil || !exists(foreignTask) {
		t.Fatal("close removed a foreign stopped task")
	}
	if err := sb.Recreate(t.Context()); err == nil || !exists(foreignTask) {
		t.Fatal("recreate replaced a foreign stopped task")
	}
	cli("rm", foreignTask)
	if err := sb.ensureStarted(t.Context()); err != nil {
		t.Fatal(err)
	}
	foreignBrowser := makeCollision(browser)
	if err := sb.Close(t.Context()); err == nil || !exists(foreignBrowser) {
		t.Fatal("close removed a foreign stopped browser")
	}
	if err := sb.Recreate(t.Context()); err == nil || !exists(foreignBrowser) {
		t.Fatal("recreate removed a foreign stopped browser")
	}

}
