package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

func TestProjectWorkspaceModeAPI(t *testing.T) {
	t.Setenv("AGENTFLOW_AUTH_REQUIRED", "true")
	t.Setenv("AGENTFLOW_SCHEDULER_ENABLED", "false")
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	h := New(s, engine.New(s, runtime.NewManager(t.TempDir())), nil, "", "")

	call := func(cookie *http.Cookie, method, path, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Origin", "http://localhost")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatalf("%s %s = %d, want %d, body = %s", method, path, w.Code, want, w.Body)
		}
		return w
	}
	session := func(login string) *http.Cookie {
		t.Helper()
		w := call(nil, http.MethodPost, "/api/auth/register", `{"login":"`+login+`","password":"secret1"}`, http.StatusOK)
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("register %s returned %d cookies", login, len(cookies))
		}
		return cookies[0]
	}
	assertMode := func(w *httptest.ResponseRecorder, want domain.WorkspaceMode) {
		t.Helper()
		var project domain.Project
		if err := json.Unmarshal(w.Body.Bytes(), &project); err != nil || project.WorkspaceMode != want {
			t.Fatalf("project workspace mode = %q, want %q, decode err = %v, body = %s", project.WorkspaceMode, want, err, w.Body)
		}
	}

	admin := session("admin")
	owner := session("owner")
	member := session("member")
	memberUser, ok := s.UserByLoginName("member")
	if !ok {
		t.Fatal("registered project member missing")
	}
	legacy := call(owner, http.MethodPost, "/api/projects", `{"id":"workspace","name":"Default"}`, http.StatusOK)
	assertMode(legacy, domain.WorkspaceModeCheckout)

	project, ok := s.Project("workspace")
	if !ok {
		t.Fatal("created project missing")
	}
	project.Members = []domain.ProjectMember{{UserID: memberUser.ID, Role: "maintainer"}}
	if err := s.PutProject(project); err != nil {
		t.Fatal(err)
	}

	enable := `{"id":"workspace","name":"Warm","workspaceMode":"warm_pool"}`
	assertMode(call(owner, http.MethodPut, "/api/projects", enable, http.StatusOK), domain.WorkspaceModeWarmPool)
	assertMode(call(owner, http.MethodPut, "/api/projects", `{"id":"workspace","name":"Renamed"}`, http.StatusOK), domain.WorkspaceModeWarmPool)

	for _, invalid := range []string{`"unknown"`, `""`, `null`, `42`} {
		call(owner, http.MethodPut, "/api/projects", `{"id":"workspace","name":"Invalid","workspaceMode":`+invalid+`}`, http.StatusBadRequest)
	}
	disable := `{"id":"workspace","name":"Checkout","workspaceMode":"checkout"}`
	denied := call(member, http.MethodPut, "/api/projects", disable, http.StatusForbidden)
	var response map[string]string
	if err := json.Unmarshal(denied.Body.Bytes(), &response); err != nil || response["error"] != "只有项目 Owner 或平台管理员可以修改项目设置" {
		t.Fatalf("project write denial = %s, decode err = %v", denied.Body, err)
	}
	assertMode(call(owner, http.MethodGet, "/api/projects/workspace", "", http.StatusOK), domain.WorkspaceModeWarmPool)
	assertMode(call(admin, http.MethodPut, "/api/projects", disable, http.StatusOK), domain.WorkspaceModeCheckout)
}
