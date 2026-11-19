package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWarmDockerEndpointPin(t *testing.T) {
	dir := t.TempDir()
	// Model context inspect (local metadata only) and a later ordinary command.
	script := `#!/bin/sh
if [ "$1" = context ]; then
 [ "$2" = inspect ] && [ "$3" = --format ] && [ "$#" = 4 ] || exit 2
 cat "$FIXTURE_ENDPOINT"
else
 echo "${DOCKER_HOST:-$(cat "$FIXTURE_ENDPOINT")}"
fi
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	endpointFile := filepath.Join(dir, "endpoint")
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("FIXTURE_ENDPOINT", endpointFile)
	for _, endpoint := range []string{"unix:///var/run/docker.sock", "unix:///run/user/1001/docker.sock", "ssh://other", "tcp://127.0.0.1:2375", "unix://other/path", "unix://"} {
		t.Run(endpoint, func(t *testing.T) {
			t.Setenv("DOCKER_CONTEXT", "fixture")
			t.Setenv("DOCKER_HOST", "")
			if err := os.WriteFile(endpointFile, []byte(endpoint), 0600); err != nil {
				t.Fatal(err)
			}
			err := pinWarmDockerEndpoint(t.Context())
			if !strings.HasPrefix(endpoint, "unix:///") {
				if err == nil || os.Getenv("DOCKER_CONTEXT") != "fixture" || os.Getenv("DOCKER_HOST") != "" {
					t.Fatal("unsupported selection was pinned", endpoint, err)
				}
				return
			}
			if err != nil || os.Getenv("DOCKER_CONTEXT") != "" {
				t.Fatal("local selection was not pinned", err)
			}
			if err := os.WriteFile(endpointFile, []byte("ssh://later"), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := exec.Command("docker", "version").Output()
			if err != nil || strings.TrimSpace(string(got)) != endpoint {
				t.Fatalf("effective endpoint changed: %q, %v", got, err)
			}
		})
	}
}

func TestWarmDockerInstalledContextSelection(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("Docker CLI unavailable")
	}
	t.Setenv("DOCKER_CONFIG", t.TempDir())
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("DOCKER_CONTEXT", "")
	docker := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), "docker", args...).Output()
		if err != nil {
			t.Fatalf("docker %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	// Context commands only manipulate the CLI's private test metadata.
	const socket = "unix:///tmp/example-warm-fixture.sock"
	docker("context", "create", "warm-local", "--docker", "host="+socket)
	docker("context", "create", "other", "--docker", "host=ssh://unused.invalid")
	t.Setenv("DOCKER_CONTEXT", "other")
	if err := pinWarmDockerEndpoint(t.Context()); err == nil || os.Getenv("DOCKER_CONTEXT") != "other" {
		t.Fatalf("remote context accepted: %v", err)
	}
	t.Setenv("DOCKER_CONTEXT", "")
	t.Setenv("DOCKER_HOST", "tcp://unused.invalid:2375")
	if err := pinWarmDockerEndpoint(t.Context()); err == nil || os.Getenv("DOCKER_HOST") != "tcp://unused.invalid:2375" {
		t.Fatalf("remote host accepted: %v", err)
	}
	t.Setenv("DOCKER_HOST", "")
	docker("context", "use", "warm-local")
	if err := pinWarmDockerEndpoint(t.Context()); err != nil {
		t.Fatal(err)
	}
	docker("context", "use", "other")
	if got := docker("context", "inspect", "--format", `{{ (index .Endpoints "docker").Host }}`); got != socket {
		t.Fatalf("context switched away from local endpoint: %q", got)
	}
}
