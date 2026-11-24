package runtime

import (
	"context"
	"slices"
	"strings"
	"testing"

	"agentflow/internal/domain"
)

func TestPrivateHTTPGitLabCLI(t *testing.T) {
	const host = "http://192.168.50.10:8443"
	for _, args := range [][]string{nil, {"mr", "list", "--repo", host + "/group/app"}} {
		if got := cliShimHost("glab", args, func(string) string { return "" }, func() string { return host + "/group/app.git" }); got != host {
			t.Errorf("glab %v selected %q", args, got)
		}
	}
	if got := cliShimHost("glab", []string{"mr", "list", "--repo", "http://other.example.org/group/app"}, func(string) string { return "" }, func() string { return host + "/group/app.git" }); got != "" {
		t.Errorf("glab picked the origin credential for an invalid override: %q", got)
	}
	base := []string{"HTTP_PROXY=http://proxy.example.org", "GLAB_API_PROTOCOL=http"}
	env := cliShimEnv("glab", host, "tok", base)
	for _, want := range []string{"GITLAB_TOKEN=tok", "GITLAB_HOST=" + host, "GITLAB_API_HOST=192.168.50.10:8443", "GLAB_API_PROTOCOL=http", "HTTP_PROXY="} {
		if !slices.Contains(env, want) {
			t.Errorf("missing glab configuration %q", want)
		}
	}
	env = cliShimEnv("glab", "gitlab.example.org", "tok", base)
	if !slices.Contains(env, "GLAB_API_PROTOCOL=https") {
		t.Error("inherited configuration downgraded the HTTPS token")
	}
}

func TestPrivateHTTPRunCredentialRelay(t *testing.T) {
	const host = "http://192.168.50.10:8443"
	credentials := RunCredentials{GitHTTPCredentials: map[string]domain.GitHTTPExecutionCredential{
		host: {GitHTTPCredential: domain.GitHTTPCredential{Host: host, Platform: domain.GitPlatformGitLab}, Token: "local-sop-secret"},
	}}
	script := `import subprocess
def fill(protocol, host):
    return subprocess.run(['git', 'credential', 'fill'], input=f'protocol={protocol}\nhost={host}\n\n', text=True, capture_output=True).stdout
assert 'password=local-sop-secret' in fill('http', '192.168.50.10:8443')
assert 'password=' not in fill('https', '192.168.50.10:8443')
assert 'password=' not in fill('http', '192.168.50.10')
assert subprocess.check_output(['git', 'config', '--get-urlmatch', 'http.followRedirects', 'http://192.168.50.10:8443/group/repo.git'], text=True).strip() == 'false'
print('BOUND_OK')
`
	command := CommandSpec{Argv: []string{"python3", "-c", script}, Provider: "script", CredentialBindings: []CredentialBinding{BindGit}}
	if out, err := ExecuteLocalCommand(context.Background(), t.TempDir(), command, credentials, nil); err != nil || !strings.Contains(out, "BOUND_OK") {
		t.Fatalf("HTTP SOP credential binding: %v %s", err, out)
	}
}
