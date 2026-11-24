package tracker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

func TestPrivateHTTPGitLabMRLookupAndSync(t *testing.T) {
	t.Setenv("AGENTFLOW_GIT_EGRESS_ALLOW_CIDRS", "192.168.50.10/32")
	st, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.PutUser(domain.User{LoginName: "gitlab-http-user"}); err != nil {
		t.Fatal(err)
	}
	user, _ := st.UserByLoginName("gitlab-http-user")
	const host = "http://192.168.50.10:8443"
	const project = "gitlab-http:192.168.50.10:8443/group/repo"
	const link = host + "/group/repo/-/merge_requests/2"
	if err := st.SaveGitHTTPCredential(user.ID, "", domain.GitHTTPCredentialSecret{
		GitHTTPCredential: domain.GitHTTPCredential{Name: "GitLab", Platform: domain.GitPlatformGitLab, Host: host},
		Token:             "local-token",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureMRDeliveryState(project, 2, link, "ISSUE-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMRDeliveryRegisteredBy(project, 2, user.ID); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "local-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.EscapedPath() {
		case "/projects/group%2Frepo/merge_requests/2":
			_, _ = w.Write([]byte(`{"iid":2,"state":"opened","target_branch":"main"}`))
		case "/projects/group%2Frepo/merge_requests":
			query := r.URL.Query()
			if query.Get("source_branch") != "feature" || query.Get("target_branch") != "main" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_, _ = w.Write([]byte(`[{"iid":2,"state":"opened","source_branch":"feature","target_branch":"main","source_project_id":1,"target_project_id":1,"sha":"matching-tip"},{"iid":3,"state":"opened","source_branch":"feature","target_branch":"main","source_project_id":2,"target_project_id":1,"sha":"fork-tip"}]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	s := NewService()
	s.ConfigureWithStore(domain.SyncConfig{}, st)
	s.gitHostAPIBase = srv.URL
	s.SyncMRDeliveryStatesOnce(st)
	key := domain.MRDeliveryKey{Project: project, IID: 2}
	if state := st.MRDeliveryStatesMap([]domain.MRDeliveryKey{key})[key]; state.State != "opened" || state.CheckedAt.IsZero() {
		t.Errorf("private GitLab delivery was not refreshed: %+v", state)
	}
	mrs, err := s.FindBranchMRsInto(domain.BranchMRQuery{RemoteProject: project, SourceBranch: "feature", TargetBranch: "main", ResponsibleUserID: user.ID})
	if err != nil || len(mrs) != 1 || mrs[0].URL != link || mrs[0].HeadSHA != "matching-tip" {
		t.Errorf("private GitLab branch MRs = %+v, %v", mrs, err)
	}
}
