package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
)

func TestWarmVolumePanelVerifiesEndpointBeforeServingFiles(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "runs/owner")
	if err := os.MkdirAll(ws, 0700); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENTFLOW_BROWSER_IMAGE", "browser-test")
	t.Setenv("AGENTFLOW_RUNTIME_CLI_DIR", "")
	t.Setenv("FIXTURES", dir)
	const parentName = "agentflow-rt-run-owner-test"
	parentID, browserID := strings.Repeat("1", 64), strings.Repeat("2", 64)
	backing := "/var/lib/docker/volumes/test-pool/_data"
	requires := SandboxRequirements{WarmDockerVolume: &WarmDockerVolume{Root: root, Name: "test-pool", Backing: backing}}
	inspection := func(id, name, network, image, labels string) string {
		return fmt.Sprintf(`[{
			"Id":%q,"Name":%q,"State":{"Running":true},"Config":{"Image":%q,"Labels":%s},
			"HostConfig":{"NetworkMode":%q,"CapDrop":["ALL"],"SecurityOpt":["no-new-privileges"],
				"Mounts":[{"Type":"volume","Source":"test-pool","Target":"/workspace","VolumeOptions":{"NoCopy":true,"Subpath":"runs/owner"}}]},
			"Mounts":[{"Type":"volume","Name":"test-pool","Source":%q,"Destination":"/workspace","RW":true}]
		}]`, id, "/"+name, image, labels, network, backing)
	}
	parent := inspection(parentID, parentName, "none", "task-test", `{"io.loopforge.warm-workspace":"`+ws+`"}`)
	sidecar := inspection(browserID, parentName+"-browser", "container:"+parentID, "browser-test", `{"io.loopforge.warm-workspace":"`+ws+`","agentflow.browser.run":"owner","agentflow.browser.owner":"`+parentName+`"}`)
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("docker", `#!/bin/sh
case "$1" in
ps) cat "$FIXTURES/list";;
inspect) for id in "$@"; do :; done; cat "$FIXTURES/$id";;
exec) echo "$3" >"$FIXTURES/cdp-container"; exit 1;;
*) exit 1;;
esac
`)
	write(parentID, parent)
	originalExec, originalLookup := workspaceFileExec, devServerLookup
	t.Cleanup(func() { workspaceFileExec, devServerLookup = originalExec, originalLookup })
	devServerLookup = func(context.Context, string) (browserEndpoint, bool) {
		return browserEndpoint{Kind: "docker", Container: "foreign"}, true
	}
	var served string
	workspaceFileExec = func(_ context.Context, id, _ string) ([]byte, int, error) {
		served = id
		return []byte("ok"), workspaceFileOK, nil
	}
	for _, tc := range []struct {
		name, ids, sidecar, parent string
		wantFile                   bool
	}{
		{"exact", browserID, sidecar, parent, true},
		{"duplicate", browserID + "\n" + strings.Repeat("3", 64), sidecar, parent, false},
		{"foreign workspace", browserID, strings.Replace(sidecar, "runs/owner", "runs/other", 1), parent, false},
		{"wrong subpath", browserID, strings.Replace(sidecar, `"Subpath":"runs/owner"`, `"Subpath":"runs/other"`, 1), parent, false},
		{"foreign parent", browserID, sidecar, strings.Replace(parent, `"Subpath":"runs/owner"`, `"Subpath":"runs/other"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			served = ""
			write("list", tc.ids)
			write(parentID, tc.parent)
			write(browserID, tc.sidecar)
			state, err := HandleWarmDockerVolumeBrowser(t.Context(), "owner", ws, requires, domain.BrowserRequest{Operation: "file", File: "/workspace/x"})
			if tc.wantFile {
				if err != nil || state.HTTP == nil || served != browserID {
					t.Fatalf("served=%q state=%+v err=%v", served, state, err)
				}
			} else if err == nil || served != "" {
				t.Fatalf("accepted unverified endpoint: served=%q state=%+v err=%v", served, state, err)
			}
		})
	}
	t.Run("CDP attaches by immutable ID", func(t *testing.T) {
		write("list", browserID)
		write(parentID, parent)
		write(browserID, sidecar)
		_, _ = HandleWarmDockerVolumeBrowser(t.Context(), "owner", ws, requires, domain.BrowserRequest{Operation: "status"})
		raw, err := os.ReadFile(filepath.Join(dir, "cdp-container"))
		if err != nil || strings.TrimSpace(string(raw)) != browserID {
			t.Fatalf("CDP target=%q err=%v", raw, err)
		}
	})
	t.Run("dev proxy ignores foreign cached endpoint", func(t *testing.T) {
		write("list", browserID)
		write(parentID, parent)
		write(browserID, sidecar)
		cacheKey := "owner" + "\x00" + browserID
		devServerClients.Lock()
		devServerClients.items[cacheKey] = &devServerClient{endpoint: browserEndpoint{Kind: "docker", Container: "foreign"}, client: &http.Client{}, checked: time.Now(), used: time.Now()}
		devServerClients.Unlock()
		originalDial := devServerDial
		t.Cleanup(func() {
			devServerDial = originalDial
			devServerClients.Lock()
			defer devServerClients.Unlock()
			delete(devServerClients.items, cacheKey)
		})
		var dialed string
		devServerDial = func(_ context.Context, ep browserEndpoint, _ int) (net.Conn, error) {
			dialed = ep.Container
			return nil, errors.New("fixture refused")
		}
		_, err := HandleWarmDockerVolumeBrowser(t.Context(), "owner", ws, requires, domain.BrowserRequest{Operation: "http", HTTP: &domain.BrowserHTTPRequest{Port: 8080, Path: "/"}})
		if err != nil || dialed != browserID {
			t.Fatalf("dev proxy used %q, err=%v", dialed, err)
		}
	})
}
