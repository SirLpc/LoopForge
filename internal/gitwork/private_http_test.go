package gitwork

import (
	"context"
	"strings"
	"testing"
)

func TestManagedPrivateHTTPDisablesRepositoryRedirects(t *testing.T) {
	const remote = "http://192.168.50.10:8443/group/repo.git"
	ctx := WithHTTPRemote(WithHTTPCredentials(context.Background(), nil), remote)
	dir := t.TempDir()
	for _, args := range [][]string{{"init"}, {"config", "--local", "http." + remote + ".followRedirects", "true"}} {
		if out, err := command(ctx, dir, nil, args).CombinedOutput(); err != nil {
			t.Fatalf("git setup: %v %s", err, out)
		}
	}
	args := []string{"config", "--get-urlmatch", "http.followRedirects", remote}
	if out, err := command(ctx, dir, nil, args).CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "false" {
		t.Fatalf("redirect check: %v %s", err, out)
	}
}

func TestPrivateHTTPCredentialMatchesOnlyItsEndpoint(t *testing.T) {
	ctx := WithHTTPRemote(WithHTTPCredentials(context.Background(), map[string]HTTPCredential{
		"http://192.168.50.10": {Username: "gitlab-user", Token: "fake-local-pat"},
	}), "http://192.168.50.10/team/repo.git")
	for _, tc := range []struct {
		protocol, authority string
		want                bool
	}{
		{"http", "192.168.50.10", true},
		{"http", "192.168.50.10:80", true},
		{"https", "192.168.50.10", false},
		{"http", "192.168.50.10:8443", false},
		{"http", "192.168.50.11", false},
	} {
		cmd := command(ctx, t.TempDir(), nil, []string{"credential", "fill"})
		cmd.Stdin = strings.NewReader("protocol=" + tc.protocol + "\nhost=" + tc.authority + "\n\n")
		out, err := cmd.CombinedOutput()
		if (err == nil) != tc.want || strings.Contains(string(out), "password=fake-local-pat") != tc.want {
			t.Errorf("%s://%s credential availability: %v, want %v", tc.protocol, tc.authority, err == nil, tc.want)
		}
	}
}
