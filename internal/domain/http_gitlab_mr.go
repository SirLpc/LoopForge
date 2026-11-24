package domain

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"agentflow/internal/gitwork"
)

// MRProjectHTTPGitLab identifies the credential and repository for a private HTTP GitLab MR.
const MRProjectHTTPGitLab = "gitlab-http:"

func parseHTTPGitLabProject(project string) (platform, host, path string, ok bool) {
	authority, path, found := strings.Cut(strings.TrimPrefix(project, MRProjectHTTPGitLab), "/")
	if !found || !validExternalMRRepository(path, false) {
		return "", "", "", false
	}
	scope, err := gitwork.HTTPHostScope(gitwork.PlainHTTPPrefix + authority)
	if err != nil {
		return "", "", "", false
	}
	return GitPlatformGitLab, scope, path, true
}

// HTTPGitLabProject is the external MR project key for a private HTTP clone URL.
func HTTPGitLabProject(remote string) string {
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawPath != "" {
		return ""
	}
	return httpGitLabProjectKey(u.Host, strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git"))
}

func httpGitLabProjectKey(authority, path string) string {
	scope, err := gitwork.HTTPHostScope(gitwork.PlainHTTPPrefix + authority)
	if err != nil || !validExternalMRRepository(path, false) {
		return ""
	}
	return MRProjectHTTPGitLab + strings.TrimPrefix(scope, gitwork.PlainHTTPPrefix) + "/" + path
}

func parseHTTPGitLabMRURL(u *url.URL) (project string, iid int, link string, ok bool) {
	if u.User != nil || u.RawPath != "" {
		return "", 0, "", false
	}
	match := externalMRPath.FindStringSubmatch(u.Path)
	if match == nil {
		return "", 0, "", false
	}
	project = httpGitLabProjectKey(u.Host, match[1])
	iid, err := strconv.Atoi(match[2])
	if project == "" || err != nil || iid <= 0 {
		return "", 0, "", false
	}
	return project, iid, GongfengMRURL(project, iid), true
}

func deliveryRepositoryProject(rc RepositoryContext) string {
	if project := strings.TrimSpace(rc.GongfengProjectID); project != "" {
		return project
	}
	return HTTPGitLabProject(rc.CloneURL)
}

var httpGitLabArtifactMR = regexp.MustCompile(`http://(?:[0-9.]+|\[[0-9a-fA-F:]+\])(?::[0-9]+)?/(?:[A-Za-z0-9_.-]+/)+-/merge_requests/[1-9][0-9]*\b`)

func httpGitLabArtifactMRs(text string) []GongfengMRRef {
	var refs []GongfengMRRef
	for _, link := range httpGitLabArtifactMR.FindAllString(text, -1) {
		if project, iid, canonical, ok := ParseDeliveryMRURL(link); ok {
			refs = append(refs, GongfengMRRef{Project: project, IID: iid, URL: canonical})
		}
	}
	return refs
}
