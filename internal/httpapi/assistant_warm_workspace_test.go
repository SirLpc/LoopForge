package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/engine"
	"agentflow/internal/repo"
	"agentflow/internal/runtime"
)

func warmChatFixture(t *testing.T) (*assistantHandlers, *AgentToolSession, domain.AssistantGeneration, assistantRequest) {
	t.Helper()
	h, u := workbenchFixture(t)
	h.engine = engine.New(h.repo, runtime.NewManager(t.TempDir()))
	p, _ := h.repo.Project("workbench")
	p.WorkspaceMode = domain.WorkspaceModeWarmPool
	p.DefaultRepositoryID = "code"
	if err := h.repo.PutProject(p); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.PutRepository(domain.Repository{ID: "code", ProjectID: p.ID, Name: "code", CloneURL: "https://example.org/team/code.git", DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.PutRunner(domain.Runner{ID: "machine"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := h.repo.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: p.ID, RepositoryID: "code", State: "available"}}); err != nil {
		t.Fatal(err)
	}
	req := assistantRequest{ExecutionTarget: &domain.AssistantExecutionTarget{Pool: "cloud", RunnerID: "machine"}, SessionID: "chat", ProjectID: p.ID, Model: "codebuddy", Messages: []assistantMessage{{Role: "user", Content: "inspect registered code"}}, ApprovalMode: "request"}
	conv := domain.AssistantSession{ID: req.SessionID, ProjectID: p.ID, ExecutionTarget: req.ExecutionTarget, Messages: req.Messages}
	g := domain.AssistantGeneration{ID: "turn", SessionID: conv.ID, ProjectID: p.ID, UserID: u.ID, ActorID: u.ID, Status: "running", MessageCount: 1}
	if err := h.repo.BeginAssistantGeneration(g, conv); err != nil {
		t.Fatal(err)
	}
	s := &AgentToolSession{Mode: AgentToolAssistant, ProjectID: p.ID, User: u, UserID: u.ID, ChatOwnerID: u.ID, ChatSessionID: conv.ID, ExecutionRunnerID: "machine", ctx: context.Background()}
	return h, s, g, req
}

func TestWarmWorkspaceConversationQueuesAndResumes(t *testing.T) {
	h, s, g, req := warmChatFixture(t)
	if err := h.repo.EnqueueWarmWorkspace(domain.WarmWorkspaceDemand{OwnerRunID: "sop", RunnerID: "machine", ProjectID: s.ProjectID, RepositoryIDs: []string{"code"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	sop, ok, err := h.repo.ReserveNextWarmWorkspace("machine", time.Now())
	if err != nil || !ok {
		t.Fatal(sop, err)
	}
	gateway := &agentToolGateway{deps: h.deps}
	result, err := gateway.openConversationWorkspace(t.Context(), s, map[string]any{"repositoryId": "code"})
	if err != nil || !strings.Contains(result, `"handoff":true`) {
		t.Fatalf("repository open did not hand off: %s %v", result, err)
	}
	var out struct{ RunID string }
	if err = json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatal(err)
	}
	if _, _, found, _ := h.repo.WarmWorkspaceAllocation(out.RunID); found {
		t.Fatal("queue acquired before old CLI exited")
	}
	handoffErr := s.modeAction(t.Context())
	var parked *warmConversationHandoff
	if !errors.As(handoffErr, &parked) {
		t.Fatal("old phase lost the requested owner")
	}
	if err := h.parkWarmGeneration(g, req, parked); err != nil {
		t.Fatal(err)
	}
	ageGeneration(t, h, g.SessionID)
	visible, _ := h.currentGeneration(g.UserID, g.SessionID)
	if visible.Status != "running" || len(visible.Events) == 0 || !strings.Contains(string(visible.Events[len(visible.Events)-1].Data), "workspace.pool_waiting") {
		t.Fatalf("pending generation was lost on reconnect: %+v", visible)
	}
	stored, _ := h.repo.AssistantSession(g.UserID, g.SessionID)
	if stored.WorkspaceRunID != "" {
		t.Fatalf("bound before capacity: %s", stored.WorkspaceRunID)
	}
	// A new controller obtains the saved request even after the original session disappears.
	restarted := &assistantHandlers{deps: h.deps}
	called := make(chan string, 1)
	restarted.generate = func(ctx context.Context, prompt string, u domain.User) (string, error) {
		called <- prompt
		return `{"message":"done","drafts":[]}`, nil
	}
	h.engine.WorkspaceCheckpointTransport = func(ctx context.Context, r domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		phase := map[string]string{"warm_prepare": "placed", "warm_checkout": "ready", "warm_verify_access": "authorized"}[q.Operation]
		return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"` + phase + `"}`)}, nil
	}
	if err := h.repo.SettleWarmWorkspace(sop, domain.WarmWorkspaceReleasing); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.SettleWarmWorkspace(sop, domain.WarmWorkspaceReleased); err != nil {
		t.Fatal(err)
	}
	restarted.startWarmGenerationRecovery(t.Context())
	select {
	case prompt := <-called:
		if !strings.Contains(prompt, "inspect registered code") {
			t.Fatal("lost original question")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no continuation")
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		done, _ := h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
		if done.ID == g.ID && done.Status == "completed" {
			for i := 1; i < len(done.Events); i++ {
				if done.Events[i].Sequence <= done.Events[i-1].Sequence {
					t.Fatal("resuming repeated an SSE cursor", done.Events[i].Sequence)
				}
			}
			if done.Stream == nil || !strings.Contains(jsonObject(done.Stream.Activity), "workspace.pool_waiting") {
				t.Fatal("resuming discarded pre-handoff activity")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("recovered generation did not finish")
}

func TestWarmWorkspaceConversationAuthorization(t *testing.T) {
	h, s, _, _ := warmChatFixture(t)
	gw := &agentToolGateway{deps: h.deps}
	if err := h.repo.PutProject(domain.Project{ID: "different", Name: "Different"}); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.PutRepository(domain.Repository{ID: "foreign", ProjectID: "different", Name: "foreign"}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.openConversationWorkspace(t.Context(), s, map[string]any{"repositoryId": "foreign"}); err == nil {
		t.Fatal("cross-project repository opened")
	}
	now := time.Now()
	if err := h.repo.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), []domain.WarmWorkspaceSlot{}); err != nil {
		t.Fatal(err)
	}
	if _, err := gw.openConversationWorkspace(t.Context(), s, map[string]any{"repositoryId": "code"}); err == nil || !strings.Contains(err.Error(), "登记") {
		t.Fatalf("missing registration must direct user to bound machine: %v", err)
	}
	scratch, err := gw.startConversationWorkspace(s, domain.AssistantSession{}, repo.GenericDockerSandboxID)
	if err != nil || scratch.ExecutionContext.WorkspaceMode == domain.WorkspaceModeWarmPool {
		t.Fatalf("scratch entered pool: %v %v", scratch.ID, err)
	}
}

func TestWarmWorkspaceConversationSwitchOneSlot(t *testing.T) {
	for _, scenario := range []string{"clean", "dirty", "terminal", "cancel_queued", "cancel_reserved"} {
		t.Run(scenario, func(t *testing.T) {
			h, s, g, req := warmChatFixture(t)
			gw := &agentToolGateway{deps: h.deps}
			reg, _ := h.repo.Repository("code")
			old, err := h.engine.StartRepositoryConversationWorkspace("old", domain.TelemetryContext{ProjectID: s.ProjectID, ResponsibleUserID: s.UserID, ExecutionPinned: true, ExecutionRunnerID: "machine"}, repo.GenericDockerSandboxID, repositoryRunContext(reg))
			if err != nil {
				t.Fatal(err)
			}
			if err = h.repo.SetAssistantSessionWorkspaceRun(s.ChatOwnerID, s.ChatSessionID, s.ProjectID, old.ID, true); err != nil {
				t.Fatal(err)
			}
			s.workspaceRunID = old.ID
			h.engine.WorkspaceCheckpointTransport = func(ctx context.Context, r domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
				switch q.Operation {
				case "warm_release_check":
					if scenario == "dirty" || scenario == "terminal" {
						return domain.RunnerWorkspaceResponse{Status: 409, Error: scenario, Code: "warm_owner_retained"}, nil
					}
					return domain.RunnerWorkspaceResponse{Payload: []byte(`[{"repositoryId":"code","commit":"0123456789012345678901234567890123456789"}]`)}, nil
				default:
					phase := map[string]string{"warm_prepare": "placed", "warm_checkout": "ready", "warm_verify_access": "authorized", "warm_release_commit": "released"}[q.Operation]
					return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"` + phase + `"}`)}, nil
				}
			}
			if _, err = h.engine.EnsureWarmWorkspace(t.Context(), old, s.UserID); err != nil {
				t.Fatal(err)
			}
			sandbox, _ := h.repo.Sandbox(repo.GenericDockerSandboxID)
			sandbox.ID = "other-env"
			sandbox.Name = "Other"
			sandbox.Shared = false
			sandbox.ProjectID = s.ProjectID
			if err = h.repo.PutSandbox(sandbox); err != nil {
				t.Fatal(err)
			}
			raw, err := gw.openConversationWorkspace(t.Context(), s, map[string]any{"repositoryId": "code", "sandboxProfileId": sandbox.ID})
			if err != nil {
				t.Fatal(err)
			}
			var out struct{ RunID string }
			if err = json.Unmarshal([]byte(raw), &out); err != nil {
				t.Fatal(err)
			}
			if out.RunID == old.ID {
				t.Fatal("switched sandbox reused old owner")
			}
			var parked *warmConversationHandoff
			actionErr := s.modeAction(t.Context())
			conv, _ := h.repo.AssistantSession(g.UserID, g.SessionID)
			if conv.WorkspaceRunID != old.ID {
				t.Fatal("old binding changed before admission")
			}
			if scenario == "dirty" || scenario == "terminal" {
				if actionErr == nil || errors.As(actionErr, &parked) || !strings.Contains(actionErr.Error(), scenario) {
					t.Fatalf("blocked self-holder was hidden: %v", actionErr)
				}
				if state, _ := h.engine.WarmWorkspaceStatus(old.ID); state != domain.WarmWorkspaceReady {
					t.Fatalf("lost dirty owner: %s", state)
				}
				return
			}
			if !errors.As(actionErr, &parked) {
				t.Fatal(actionErr)
			}
			if err = h.parkWarmGeneration(g, req, parked); err != nil {
				t.Fatal(err)
			}
			pending, err := h.repo.WaitingWarmGenerations()
			if err != nil || len(pending) != 1 {
				t.Fatalf("not queued: %v %v", pending, err)
			}
			p := pending[0]
			if scenario != "cancel_queued" {
				if _, ok, err := h.repo.ReserveNextWarmWorkspace("machine", time.Now()); err != nil || !ok {
					t.Fatalf("one-slot wait unable to advance: %v", err)
				}
			}
			if strings.HasPrefix(scenario, "cancel") {
				if err = h.repo.CancelAssistantGeneration(g.UserID, g.SessionID, g.ID); err != nil {
					t.Fatal(err)
				}
				_, state, _, err := h.repo.WarmWorkspaceAllocation(out.RunID)
				want := domain.WarmWorkspaceCanceled
				if scenario == "cancel_reserved" {
					want = domain.WarmWorkspaceReleasing
				}
				if err != nil || state != want {
					t.Fatalf("stop left %s, want %s: %v", state, want, err)
				}
				conv, _ = h.repo.AssistantSession(g.UserID, g.SessionID)
				if conv.WorkspaceRunID != old.ID {
					t.Fatal("stop replaced binding")
				}
				return
			}
			ready, err := h.engine.AdvanceConversationWarmWorkspace(t.Context(), parked.run, p.Wait.Epoch, g.ActorID)
			if err != nil || !ready {
				t.Fatalf("advance %v %v", ready, err)
			}
			_, ok, err := h.repo.ClaimWarmContinuation(p, true)
			if err != nil || !ok {
				t.Fatal(err)
			}
			if _, ok, err := h.repo.ClaimWarmContinuation(p, true); err != nil || ok {
				t.Fatal("duplicate continuation", err)
			}
			conv, _ = h.repo.AssistantSession(g.UserID, g.SessionID)
			if conv.WorkspaceRunID != out.RunID || conv.Mode() != domain.AssistantModeWorkspace {
				t.Fatal("ready workspace not bound")
			}
		})
	}
}

func TestWarmWorkspaceConversationBoundSpeaker(t *testing.T) {
	h, s, g, req := warmChatFixture(t)
	reg, _ := h.repo.Repository("code")
	owner, err := h.engine.StartRepositoryConversationWorkspace("old", domain.TelemetryContext{ProjectID: s.ProjectID, ResponsibleUserID: s.UserID, ExecutionPinned: true, ExecutionRunnerID: "machine"}, repo.GenericDockerSandboxID, repositoryRunContext(reg))
	if err != nil {
		t.Fatal(err)
	}
	if err = h.repo.SetAssistantSessionWorkspaceRun(s.ChatOwnerID, s.ChatSessionID, s.ProjectID, owner.ID, true); err != nil {
		t.Fatal(err)
	}
	if err = h.repo.SetAssistantRuntimeMode(s.ChatOwnerID, s.ChatSessionID, s.ProjectID, domain.AssistantModeWorkspace, g.ID); err != nil {
		t.Fatal(err)
	}
	if err = h.repo.PutUser(domain.User{LoginName: "peer", CodeBuddyAPIKey: "test-only"}); err != nil {
		t.Fatal(err)
	}
	peer, _ := h.repo.UserByLoginName("peer")
	p, _ := h.repo.Project(s.ProjectID)
	p.Members = []domain.ProjectMember{{UserID: peer.ID, Role: "viewer"}}
	if err = h.repo.PutProject(p); err != nil {
		t.Fatal(err)
	}
	checked := int64(0)
	h.engine.WorkspaceCheckpointTransport = func(ctx context.Context, r domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		switch q.Operation {
		case "warm_verify_access":
			checked = q.WarmWorkspaceActorID
			if checked == peer.ID {
				return domain.RunnerWorkspaceResponse{Error: "git denied", Status: 403}, nil
			}
		case "warm_release_check":
			return domain.RunnerWorkspaceResponse{Payload: []byte(`[{"repositoryId":"code","commit":"0123456789012345678901234567890123456789"}]`)}, nil
		}
		phase := map[string]string{"warm_verify_access": "authorized", "warm_prepare": "placed", "warm_checkout": "ready", "warm_release_commit": "released"}[q.Operation]
		return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"` + phase + `"}`)}, nil
	}
	if _, err = h.engine.EnsureWarmWorkspace(t.Context(), owner, s.UserID); err != nil {
		t.Fatal(err)
	}
	s.User = peer
	s.UserID = peer.ID
	s.workspaceRunID = owner.ID
	s.RuntimeMode = domain.AssistantModeWorkspace
	_, err = h.conversationExecutor(s, t.TempDir())(t.Context(), "", runtime.CommandSpec{}, runtime.RunCredentials{}, nil)
	if err == nil || checked != peer.ID {
		t.Fatalf("bound turn opened with wrong Git actor: checked=%d %v", checked, err)
	}
	if err = h.engine.ReleaseWarmWorkspace(t.Context(), owner, g.ActorID); err != nil {
		t.Fatal(err)
	}
	// A new turn in the saved workspace mode must park before starting a native CLI.
	called := false
	h.generate = func(context.Context, string, domain.User) (string, error) {
		called = true
		return `{"message":"unexpected","drafts":[]}`, nil
	}
	h.executeGeneration(t.Context(), req, g, domainUserToAuth(s.User))
	if called {
		t.Fatal("started CLI before readmitting returned workspace")
	}
}

func TestWarmWorkspaceConversationRecoveryRetriesTransientAccess(t *testing.T) {
	h, s, g, req := warmChatFixture(t)
	gw := &agentToolGateway{deps: h.deps}
	if _, err := gw.openConversationWorkspace(t.Context(), s, map[string]any{"repositoryId": "code"}); err != nil {
		t.Fatal(err)
	}
	var hand *warmConversationHandoff
	if err := s.modeAction(t.Context()); !errors.As(err, &hand) {
		t.Fatal(err)
	}
	if err := h.parkWarmGeneration(g, req, hand); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.repo.ReserveNextWarmWorkspace("machine", time.Now()); err != nil || !ok {
		t.Fatal(err)
	}
	h.engine.WorkspaceCheckpointTransport = func(ctx context.Context, r domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		if q.Operation == "warm_verify_access" {
			return domain.RunnerWorkspaceResponse{Status: 503, Error: "runner reconnecting"}, nil
		}
		phase := map[string]string{"warm_prepare": "placed", "warm_checkout": "ready"}[q.Operation]
		return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"` + phase + `"}`)}, nil
	}
	pending, err := h.repo.WaitingWarmGenerations()
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	h.resumeWarmGeneration(t.Context(), pending[0])
	pending, err = h.repo.WaitingWarmGenerations()
	if err != nil || len(pending) != 1 {
		t.Fatalf("transient verification consumed pending request: %v %v", pending, err)
	}
}

func TestWarmWorkspaceConversationCloudPrefersDefaultRepo(t *testing.T) {
	h, s, _, _ := warmChatFixture(t)
	h.engine.ConfigureCloudRunnerPool(engine.CloudRunnerPool{Label: "cloud", Projects: []string{s.ProjectID}})
	for _, machine := range []struct {
		id, version string
		slots       []domain.WarmWorkspaceSlot
	}{
		{"legacy", "0.11.0", nil},
		{"warm", domain.RunnerVersionWarmWorkspace, []domain.WarmWorkspaceSlot{{ID: "slot2", RepositoryID: "code", ProjectID: s.ProjectID, State: "reserved"}}},
	} {
		if err := h.repo.PutRunner(domain.Runner{ID: machine.id, Labels: []string{"cloud"}}); err != nil {
			t.Fatal(err)
		}
		if err := h.repo.RunnerHeartbeat(machine.id, nil, machine.version, time.Now(), time.Now().Add(time.Hour), machine.slots); err != nil {
			t.Fatal(err)
		}
	}
	target, err := h.newExecutionTarget(s.UserID, s.ProjectID, nil)
	if err != nil || target.RunnerID != "warm" {
		t.Fatalf("new target ignored compatible machine: %+v %v", target, err)
	}
	if err = h.repo.RunnerHeartbeat("machine", nil, "0.11.0", time.Now(), time.Now().Add(time.Hour), []domain.WarmWorkspaceSlot{{ID: "slot", RepositoryID: "code", ProjectID: s.ProjectID, State: "available"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = (&agentToolGateway{deps: h.deps}).openConversationWorkspace(t.Context(), s, map[string]any{"repositoryId": "code"}); err == nil {
		t.Fatal("older bound runner opened pooled checkout")
	}
}

func TestWarmWorkspaceConversationStopDuringAdmission(t *testing.T) {
	h, s, g, req := warmChatFixture(t)
	gw := &agentToolGateway{deps: h.deps}
	if _, err := gw.openConversationWorkspace(t.Context(), s, map[string]any{"repositoryId": "code"}); err != nil {
		t.Fatal(err)
	}
	var hand *warmConversationHandoff
	if err := s.modeAction(t.Context()); !errors.As(err, &hand) {
		t.Fatal(err)
	}
	if err := h.parkWarmGeneration(g, req, hand); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h.repo.ReserveNextWarmWorkspace("machine", time.Now()); err != nil || !ok {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	left := make(chan struct{})
	h.engine.WorkspaceCheckpointTransport = func(ctx context.Context, r domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		close(entered)
		defer close(left)
		<-ctx.Done()
		return domain.RunnerWorkspaceResponse{}, ctx.Err()
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	h.startWarmGenerationRecovery(ctx)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("admission never started")
	}
	if err := h.repo.CancelAssistantGeneration(g.UserID, g.SessionID, g.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-left:
	case <-time.After(3 * time.Second):
		t.Fatal("active checkout ignored explicit stop")
	}
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		job, _ := h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
		if job.Status == "cancelled" {
			return
		}
		time.Sleep(time.Millisecond * 15)
	}
	t.Fatal("parked user stop did not finish")
}

func TestWarmWorkspaceConversationOpenStreamFollowsParkedGeneration(t *testing.T) {
	h, s, g, _ := warmChatFixture(t)
	rec := newAssistantGenerationRecorder(g)
	entered := make(chan struct{})
	read := rec.live.read
	var first sync.Once
	rec.live.read = func() domain.AssistantGeneration { first.Do(func() { close(entered) }); return read() }
	c, w := workbenchContext(s.User, "GET", "/stream", bytes.NewBuffer(nil))
	ctx, cancel := context.WithCancel(t.Context())
	c.Request = c.Request.WithContext(ctx)
	done := make(chan struct{})
	defer func() { cancel(); <-done }()
	go func() {
		defer close(done)
		serveAssistantGenerationStream(c, g, func() (domain.AssistantGeneration, bool) {
			return h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
		})
	}()
	<-entered
	parked := g
	parked.Status = "completed"
	parked.Stream = &domain.AssistantStreamSnapshot{Sequence: 1, Activity: []domain.AssistantActivityEvent{{Kind: "status", ID: "workspace.pool_waiting"}}}
	if err := h.repo.CheckpointAssistantGeneration(parked); err != nil {
		t.Fatal(err)
	}
	rec.close()
	select {
	case <-done:
		if !strings.Contains(w.Body.String(), "workspace.pool_waiting") {
			t.Fatal("missing parked progress")
		}
	case <-time.After(1500 * time.Millisecond):
		t.Fatal("open SSE stayed on the departed worker")
	}
}
