package httpapi

import (
	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"bytes"
	"context"
	"github.com/gin-gonic/gin"
	"sync/atomic"
	"testing"
	"time"
)

func TestReadyWarmWorkspaceUsesOpeningActor(t *testing.T) {
	a, owner, member, _, _ := sharedWorkbenchFixture(t)
	a.engine = engine.New(a.repo, nil)
	run, _ := a.repo.RunState("shared-run")
	run.Kind = domain.RunKindWorkspace
	run.ExecutionContext.WorkspaceMode = domain.WorkspaceModeWarmPool
	run.ExecutionContext.ExecutionMode = domain.ExecutionModeRunner
	run.ExecutionContext.ExecutionRunnerID = "machine"
	run.ExecutionContext.ResponsibleUserID = owner.ID
	run.Repository = domain.RepositoryContext{RepositoryID: "code", WorkspaceDir: "code", CloneURL: "https://example.org/code"}
	run.WorkflowSnapshot.SandboxSnapshot = &domain.RunnerSandboxSpec{Profile: domain.SandboxProfile{Driver: "docker"}}
	if err := a.repo.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if err := a.repo.PutRunner(domain.Runner{ID: "machine"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := a.repo.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "workbench", RepositoryID: "code", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.repo.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: run.ID, RunnerID: "machine", ProjectID: "workbench", RepositoryIDs: []string{"code"}}, now); err != nil {
		t.Fatal(err)
	}
	l, ok, err := a.repo.ReserveNextWarmWorkspace("machine", now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	if err := a.repo.SettleWarmWorkspace(l, domain.WarmWorkspaceReady); err != nil {
		t.Fatal(err)
	}
	hub := quietRunnerHub()
	var dispatched atomic.Int32
	fakeRunnerConnection(t, hub, "machine", func(q domain.RunnerWorkspaceRequest) domain.RunnerWorkspaceResponse {
		dispatched.Add(1)
		return domain.RunnerWorkspaceResponse{Payload: []byte(`{"content":"fixture"}`)}
	})
	hub.authorizeWarm = a.engine.AuthorizeWarmWorkspaceControl
	hub.conns["machine"].warmProtocol = true
	h := runsHandlers{deps: a.deps, runnerControls: hub}
	b := browserHandlers{deps: a.deps, runnerControls: hub}
	router := gin.New()
	router.GET("/api/runs/:id/workspace/file", h.runWorkspace)
	router.PUT("/api/runs/:id/workspace/file", h.workspaceWrite)
	router.POST("/api/runs/:id/workspace/terminal", h.workspaceTerminal)
	router.POST("/api/runs/:id/browser", b.browser)
	probes := 0
	allow := true
	a.engine.WorkspaceCheckpointTransport = func(_ context.Context, r domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		if q.Operation != domain.RunnerWorkspaceOperationWarmVerifyAccess || q.WarmWorkspaceActorID != member.ID || r.ID != run.ID {
			t.Fatalf("probe for wrong actor or owner: %s %d %s", q.Operation, q.WarmWorkspaceActorID, r.ID)
		}
		probes++
		if !allow {
			return domain.RunnerWorkspaceResponse{Status: 403, Error: "fixture access denied"}, nil
		}
		return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"authorized"}`)}, nil
	}
	call := func(method, path, body string) int {
		c, w := workbenchContext(member, method, "/api/runs/"+run.ID+path, bytes.NewBufferString(body))
		router.ServeHTTP(w, c.Request)
		return w.Code
	}
	if status := call("GET", "/workspace/file?path=README.md", ""); status != 200 || probes != 1 || dispatched.Load() != 1 {
		t.Fatalf("authorized read: status=%d probes=%d reads=%d", status, probes, dispatched.Load())
	}
	if actor, err := a.repo.WarmWorkspaceActor(l); err != nil || actor != member.ID {
		t.Fatalf("authorized repository actor for return: %d, %v", actor, err)
	}
	allow = false
	for _, tc := range [][3]string{{"GET", "/workspace/file?path=README.md", ""}, {"PUT", "/workspace/file", `{"path":"README.md","content":"fixture"}`}, {"POST", "/workspace/terminal", `{"operation":"read","id":"fixture"}`}, {"POST", "/browser", `{"operation":"status"}`}} {
		before := probes
		if status := call(tc[0], tc[1], tc[2]); status != 403 || probes != before+1 || dispatched.Load() != 1 {
			t.Errorf("denied %s: status=%d probes=%d reads=%d", tc[1], status, probes, dispatched.Load())
		}
	}
	before := probes
	if status := call("POST", "/workspace/terminal", `{"operation":"close","id":"fixture"}`); status != 200 || probes != before || dispatched.Load() != 2 {
		t.Fatalf("close after remote denial: %d probes=%d dispatch=%d", status, probes, dispatched.Load())
	}
	project, _ := a.repo.Project("workbench")
	project.Members = []domain.ProjectMember{}
	if err := a.repo.PutProject(project); err != nil {
		t.Fatal(err)
	}
	if status := call("GET", "/workspace/file?path=README.md", ""); status != 403 || probes != before || dispatched.Load() != 2 {
		t.Fatalf("removed member: %d probes=%d dispatch=%d", status, probes, dispatched.Load())
	}

}
