package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func TestWarmWorkspaceControlReconnect(t *testing.T) {
	s, err := repo.OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	const id = "warm-control"
	if err := s.PutRunner(domain.Runner{ID: id, RunnerVersion: domain.RunnerVersionWarmWorkspace}); err != nil {
		t.Fatal(err)
	}
	if err := s.RunnerHeartbeat(id, nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "a", ProjectID: "p", RepositoryID: "repo", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{RunnerID: id, OwnerRunID: "owner", ProjectID: "p", RepositoryIDs: []string{"repo"}}, now); err != nil {
		t.Fatal(err)
	}
	l, ok, err := s.ReserveNextWarmWorkspace(id, now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	hub := quietRunnerHub()
	run := domain.Run{ID: "owner", Status: "running", ExecutionContext: domain.TelemetryContext{ProjectID: "p", ExecutionMode: domain.ExecutionModeRunner, ExecutionRunnerID: id, WorkspaceMode: domain.WorkspaceModeWarmPool}, Repositories: []domain.RepositoryContext{{RepositoryID: "repo", WorkspaceDir: "trusted", BaseBranch: "main"}}}
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	hub.authorizeWarm = engine.New(s, nil).AuthorizeWarmWorkspaceControl
	req := domain.RunnerWorkspaceRequest{RunID: "owner", Operation: domain.RunnerWorkspaceOperationWarmStatus, WarmWorkspaceLease: &l}
	if _, err := hub.requestWorkspace(t.Context(), id, req); err == nil {
		t.Fatal("offline request succeeded")
	}
	conn := testRunnerConn(id)
	registerTestConn(hub, conn)
	legacy := requestAsync(hub, id, req)
	select {
	case msg := <-conn.send:
		t.Fatalf("legacy socket received warm workspace request %v", msg.Type)
	case res := <-legacy:
		if res.err == nil {
			t.Fatal("legacy socket accepted warm work")
		}
	case <-time.After(time.Second):
		t.Fatal("legacy socket did not reject warm work")
	}
	conn = testRunnerConn(id)
	conn.warmProtocol = true
	// Explicitly opt this simulated replacement process into the current protocol.
	registerTestConn(hub, conn)
	done := requestAsync(hub, id, req)
	got := answerOnce(t, hub, conn, domain.RunnerWorkspaceResponse{})
	if result := <-done; result.err != nil || got.WarmWorkspaceLease.Generation != l.Generation {
		t.Fatalf("reconnected %+v %v", got, result.err)
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmPrepare
	req.Repositories = []domain.RepositoryContext{{RepositoryID: "repo", WorkspaceDir: "spoof", BaseBranch: "spoof"}}
	doneRelay := make(chan error, 1)
	go func() { _, err := hub.requestRelayed(t.Context(), id, req); doneRelay <- err }()
	got = answerOnce(t, hub, conn, domain.RunnerWorkspaceResponse{})
	if err := <-doneRelay; err != nil || len(got.Repositories) != 1 || got.Repositories[0] != run.Repositories[0] {
		t.Fatalf("untrusted relay fields %+v %v", got, err)
	}
	// A second run freezes the same repository ID in another project. Its
	// malformed demand reserved project p; the mismatch must never hit the socket.
	if err := s.CancelWarmWorkspace("owner", now); err != nil {
		t.Fatal(err)
	}
	if err := s.SettleWarmWorkspace(l, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	run.ID = "other-owner"
	run.ExecutionContext.ProjectID = "other-project"
	if err := s.PutRun(run); err != nil {
		t.Fatal(err)
	}
	if err := s.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{RunnerID: id, OwnerRunID: run.ID, ProjectID: "p", RepositoryIDs: []string{"repo"}}, now); err != nil {
		t.Fatal(err)
	}
	other, ok, err := s.ReserveNextWarmWorkspace(id, now)
	if !ok || err != nil {
		t.Fatal(err)
	}
	otherReq := req
	otherReq.RunID = other.OwnerRunID
	otherReq.WarmWorkspaceLease = &other
	if _, err := hub.requestRelayed(t.Context(), id, otherReq); err == nil || len(conn.send) != 0 {
		t.Fatalf("cross-project relay reached runner: %v", err)
	}
	req.Operation = domain.RunnerWorkspaceOperationWarmStatus
	l.Generation++
	if _, err := hub.requestRelayed(t.Context(), id, req); err == nil {
		t.Fatal("relay accepted stale lease")
	}
}

func TestWarmWorkspaceClaimRequiresCurrentProcessCapability(t *testing.T) {
	f := cvBNewHTTPFixture(t)
	id, session := registerTestRunner(t, f.h)
	r, _ := f.s.Runner(id)
	r.RunnerVersion = domain.RunnerVersionWarmWorkspace
	if err := f.s.PutRunner(r); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, task := range []domain.RunnerTask{{ID: "warm-first", WarmWorkspaceLease: &domain.WarmWorkspaceLease{Generation: 1}, CreatedAt: now.Add(-time.Minute)}, {ID: "ordinary", CreatedAt: now}} {
		task.RunnerID = id
		task.Provider = "mock"
		if err := f.s.PutRunnerTask(task); err != nil {
			t.Fatal(err)
		}
	}
	headers := map[string]string{"Authorization": "Bearer " + session}
	claim := func(preflight bool) string {
		resp := requestWithHeaders(t, f.h, http.MethodPost, "/api/runners/tasks/claim", map[string]any{"availableRuntimes": []string{"mock"}, "noWait": true, "preflight": preflight}, headers)
		return resp.Body.String()
	}
	if got := claim(false); !strings.Contains(got, "ordinary") {
		t.Fatalf("legacy process claimed %s", got)
	}
	if got := claim(true); strings.Contains(got, "true") {
		t.Fatalf("legacy preflight signalled warm work: %s", got)
	}
	headers[domain.WarmWorkspaceProtocolHeader] = domain.WarmWorkspaceProtocolV1
	if got := claim(false); !strings.Contains(got, "warm-first") {
		t.Fatalf("capable process missed warm work: %s", got)
	}
}

func TestWarmWorkspaceSocketCapabilityBelongsToConnection(t *testing.T) {
	f := cvBNewHTTPFixture(t)
	id, session := registerTestRunner(t, f.h)
	hub := newRunnerControlHub()
	handler := &runnersHandlers{deps: &deps{repo: f.s, engine: f.e}, runnerControls: hub}
	router := gin.New()
	router.GET("/connect", handler.runnerControlConnect)
	server := httptest.NewServer(router)
	defer server.Close()
	for _, capable := range []bool{true, false} {
		headers := http.Header{"Authorization": []string{"Bearer " + session}}
		if capable {
			headers.Set(domain.WarmWorkspaceProtocolHeader, domain.WarmWorkspaceProtocolV1)
		}
		hub.mu.Lock()
		changed := hub.registered
		hub.mu.Unlock()
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/connect", headers)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		select {
		case <-changed:
		case <-time.After(time.Second):
			t.Fatal("socket registration timed out")
		}
		hub.mu.Lock()
		current := hub.conns[id]
		hub.mu.Unlock()
		if current == nil || current.warmProtocol != capable {
			t.Fatalf("socket capability did not follow reconnect: %v", capable)
		}
		if !capable {
			_, sent, err := hub.send(t.Context(), current, domain.RunnerWorkspaceRequest{WarmWorkspaceLease: &domain.WarmWorkspaceLease{Generation: 1}})
			if sent || err == nil {
				t.Fatal("downgraded connection received typed workspace request")
			}
		}
	}
}
