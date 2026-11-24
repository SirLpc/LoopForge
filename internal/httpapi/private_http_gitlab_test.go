package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestPrivateHTTPGitLabRepositoryAndCredential(t *testing.T) {
	t.Setenv("AGENTFLOW_GIT_EGRESS_ALLOW_CIDRS", "192.168.50.10/32")
	f := cvBNewHTTPFixture(t)
	const host = "http://192.168.50.10:8443"
	for _, address := range []string{host, "http://192.168.50.11:8443"} {
		want := http.StatusOK
		if address != host {
			want = http.StatusBadRequest
		}
		remote := address + "/team/code.git"
		resp := cvBRequest(t, f, http.MethodPost, "/api/repositories", map[string]string{"projectId": "platform", "repoUrl": address + "/team/code", "cloneUrl": remote}, true)
		if resp.Code != want {
			t.Errorf("repository %s returned HTTP %d, want %d: %s", address, resp.Code, want, resp.Body.String())
		} else if want == http.StatusOK {
			var repo domain.Repository
			if json.Unmarshal(resp.Body.Bytes(), &repo) != nil || repo.CloneURL != remote {
				t.Errorf("unexpected repository: %s", resp.Body.String())
			}
		}
		endpoint := fmt.Sprintf("/api/users/%d/git-http-credentials", f.user.ID)
		resp = cvBRequest(t, f, http.MethodPut, endpoint, map[string]string{"host": address, "name": "Local GitLab", "platform": "gitlab", "token": "fake-local-pat"}, true)
		if resp.Code != want || strings.Contains(resp.Body.String(), "fake-local-pat") {
			t.Errorf("credential %s returned HTTP %d, want %d: %s", address, resp.Code, want, resp.Body.String())
		}
	}
	cred, ok := f.s.GitHTTPCredentialForHost(context.Background(), f.user.ID, host)
	if !ok || cred.APIBase() != host+"/api/v4" || cred.Token != "fake-local-pat" {
		t.Fatal("saved token unavailable at the configured HTTP GitLab API")
	}
	t.Setenv("AGENTFLOW_GIT_EGRESS_ALLOW_CIDRS", "")
	if _, ok := f.s.GitHTTPCredentialForHost(context.Background(), f.user.ID, host); ok {
		t.Fatal("credential still available after removing the IP from the allowlist")
	}
}
