package runtime

import (
	"fmt"
	"strings"

	"agentflow/internal/gitwork"
)

// configurePrivateHTTPGit adds network settings for the HTTP endpoints exposed by the relay.
func configurePrivateHTTPGit(environment map[string]string, hosts string) {
	configCount := 2
	for _, entry := range strings.Split(hosts, ",") {
		host, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(host, gitwork.PlainHTTPPrefix) {
			environment[fmt.Sprintf("GIT_CONFIG_KEY_%d", configCount)] = "http." + host + "/.followRedirects"
			environment[fmt.Sprintf("GIT_CONFIG_VALUE_%d", configCount)] = "false"
			configCount++
			environment[fmt.Sprintf("GIT_CONFIG_KEY_%d", configCount)] = "http." + host + "/.proxy"
			environment[fmt.Sprintf("GIT_CONFIG_VALUE_%d", configCount)] = ""
			configCount++
		}
	}
	environment["GIT_CONFIG_COUNT"] = fmt.Sprint(configCount)
}

// configureGitLabCLI pins the API endpoint that receives the workspace token.
func configureGitLabCLI(set map[string]string, host string) {
	if authority, plain := strings.CutPrefix(host, gitwork.PlainHTTPPrefix); plain {
		set["GITLAB_HOST"] = host
		set["GITLAB_API_HOST"] = authority
		set["GLAB_API_PROTOCOL"] = "http"
		for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
			set[key] = ""
		}
	} else {
		set["GITLAB_HOST"] = "https://" + host
		set["GITLAB_API_HOST"] = host
		set["GLAB_API_PROTOCOL"] = "https"
	}
}
