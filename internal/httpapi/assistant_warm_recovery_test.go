package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

func warmRecoveryFixture(t *testing.T) (*assistantHandlers, *AgentToolSession, domain.AssistantGeneration, assistantRequest, *warmConversationHandoff) {
	t.Helper()
	t.Setenv("AGENTFLOW_CONVERSATION_DIR", t.TempDir())
	h, s, g, req := warmChatFixture(t)
	h.sourceRoot = t.TempDir()
	h.runnerControls = quietRunnerHub()
	h.runnerControls.authorizeWarm = h.engine.AuthorizeWarmWorkspaceControl
	h.engine.WorkspaceCheckpointTransport = func(ctx context.Context, run domain.Run, q domain.RunnerWorkspaceRequest) (domain.RunnerWorkspaceResponse, error) {
		if q.Operation == "warm_release_check" {
			return domain.RunnerWorkspaceResponse{Payload: []byte(`[{"repositoryId":"code","commit":"0123456789012345678901234567890123456789"}]`)}, nil
		}
		phase := map[string]string{"warm_prepare": "placed", "warm_checkout": "ready", "warm_verify_access": "authorized", "warm_release_commit": "released"}[q.Operation]
		return domain.RunnerWorkspaceResponse{Payload: []byte(`{"phase":"` + phase + `"}`)}, nil
	}
	gw := &agentToolGateway{deps: h.deps}
	if _, err := gw.openConversationWorkspace(t.Context(), s, map[string]any{"repositoryId": "code"}); err != nil {
		t.Fatal(err)
	}
	var parked *warmConversationHandoff
	if err := s.modeAction(t.Context()); !errors.As(err, &parked) {
		t.Fatal(err)
	}
	return h, s, g, req, parked
}

func recoverWarmTurn(t *testing.T, h *assistantHandlers) {
	t.Helper()
	p, err := h.repo.WaitingWarmGenerations()
	if err != nil || len(p) != 1 {
		t.Fatalf("expected one parked generation: %v %v", p, err)
	}
	if _, ok, err := h.repo.ReserveNextWarmWorkspace("machine", time.Now()); err != nil || !ok {
		t.Fatal("reserve", ok, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h.resumeWarmGeneration(ctx, p[0])
}

func warmNativeRunner(t *testing.T, h *assistantHandlers, handle func(runtime.ConversationProcessRequest) string) {
	t.Helper()
	fakeRunnerConnection(t, h.runnerControls, "machine", func(q domain.RunnerWorkspaceRequest) domain.RunnerWorkspaceResponse {
		var r runtime.ConversationProcessRequest
		if err := json.Unmarshal(q.Conversation, &r); err != nil {
			return domain.RunnerWorkspaceResponse{Status: 500, Error: err.Error()}
		}
		if msg := handle(r); msg != "" {
			return domain.RunnerWorkspaceResponse{Status: 503, Error: msg}
		}
		res := runtime.ConversationProcessResult{ID: r.ID, Done: r.Operation == "stop"}
		if r.Operation == "poll" {
			res.Done = true
			res.Sequence = 1
			res.Events = []domain.TelemetryEvent{{Name: "process.stdout", Message: `{"type":"result","result":"inspection complete"}`}}
		}
		payload, _ := json.Marshal(res)
		return domain.RunnerWorkspaceResponse{Status: 200, Payload: payload}
	})
	h.runnerControls.mu.Lock()
	h.runnerControls.conns["machine"].warmProtocol = true
	h.runnerControls.mu.Unlock()
}

func TestWarmReopenSendsNewMessageToNativeCLI(t *testing.T) {
	h, s, g, req, hand := warmRecoveryFixture(t)
	ctx := t.Context()
	if _, err := h.engine.EnsureWarmWorkspace(ctx, hand.run, s.UserID); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.SetAssistantSessionWorkspaceRun(g.UserID, g.SessionID, g.ProjectID, hand.run.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.SetAssistantRuntimeMode(g.UserID, g.SessionID, g.ProjectID, domain.AssistantModeWorkspace, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.engine.ReleaseWarmWorkspace(ctx, hand.run, s.UserID); err != nil {
		t.Fatal(err)
	}
	saved, _ := h.repo.AssistantSession(g.UserID, g.SessionID)
	native, err := openAssistantNativeSession(req, saved, s.User, h.resolvedSourceRoot())
	if err != nil {
		t.Fatal(err)
	}
	native.checkpoint.Unstarted = false
	if err = native.commit(nil); err != nil {
		t.Fatal(err)
	}
	native.Close()
	var prompts []string
	warmNativeRunner(t, h, func(r runtime.ConversationProcessRequest) string {
		if r.Operation == "start" {
			prompts = append(prompts, strings.Join(r.Command.Argv, " "))
		}
		return ""
	})
	h.executeGeneration(ctx, req, g, domainUserToAuth(s.User))
	recoverWarmTurn(t, h)
	done, _ := h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
	if done.Status != "completed" || len(prompts) != 1 {
		t.Fatalf("native starts=%d generation=%s %s", len(prompts), done.Status, done.Error)
	}
	if !strings.Contains(prompts[0], req.Messages[0].Content) {
		t.Fatal("reopened native CLI did not receive the queued user message")
	}
}

func TestWarmContinuationSteerSendsNewMessageToNativeCLI(t *testing.T) {
	h, s, g, req, hand := warmRecoveryFixture(t)
	hand.nativeSessionID = newAssistantSessionID()
	if err := h.parkWarmGeneration(g, req, hand); err != nil {
		t.Fatal(err)
	}
	const correction = "inspect the material importer instead"
	var prompts []string
	warmNativeRunner(t, h, func(r runtime.ConversationProcessRequest) string {
		if r.Operation == "start" {
			prompts = append(prompts, strings.Join(r.Command.Argv, " "))
			if len(prompts) == 1 {
				if err := h.repo.SteerAssistantGeneration(g.UserID, g.SessionID, g.ID, domain.AssistantSteer{Content: correction, ActorID: s.UserID, Model: req.Model, TriggerID: "redirect"}); err != nil {
					return err.Error()
				}
			}
		}
		return ""
	})
	recoverWarmTurn(t, h)
	done, _ := h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
	if done.ID == g.ID || done.Status != "completed" || len(prompts) != 2 {
		t.Fatalf("native starts=%d next=%s %s %s", len(prompts), done.ID, done.Status, done.Error)
	}
	if !strings.Contains(prompts[1], correction) {
		t.Fatal("steered native CLI did not receive the new message")
	}
}

func TestWarmHandoffRequiresConfirmedNativeExit(t *testing.T) {
	h, s, g, req := warmChatFixture(t)
	t.Setenv("AGENTFLOW_CONVERSATION_DIR", t.TempDir())
	h.sourceRoot = t.TempDir()
	h.runnerControls = quietRunnerHub()
	old := conversationProcessStopTimeout
	conversationProcessStopTimeout = 150 * time.Millisecond
	defer func() { conversationProcessStopTimeout = old }()
	var opens, stops int
	warmNativeRunner(t, h, func(r runtime.ConversationProcessRequest) string {
		switch r.Operation {
		case "start":
			session, ok := agentToolSessions.get(r.Token)
			if !ok {
				return "missing native tool session"
			}
			gw := &agentToolGateway{deps: h.deps}
			if _, err := gw.openConversationWorkspace(t.Context(), session, map[string]any{"repositoryId": "code"}); err != nil {
				return err.Error()
			}
			opens++
		case "poll":
			return "poll reply lost"
		case "stop":
			stops++
			return "stop unconfirmed"
		}
		return ""
	})
	h.executeGeneration(t.Context(), req, g, domainUserToAuth(s.User))
	if opens != 1 || stops == 0 {
		t.Fatalf("runner fixture failed to exercise handoff and stop, opens=%d stops=%d", opens, stops)
	}
	p, err := h.repo.WaitingWarmGenerations()
	if err != nil {
		t.Fatal(err)
	}
	if len(p) != 0 {
		t.Fatal("handoff parked a new workspace while old native writer exit was unconfirmed")
	}
	done, _ := h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
	if done.Status != "failed" || done.Failure == nil || done.Failure.Code != "CLI_EXIT_UNCONFIRMED" {
		t.Fatalf("missing unconfirmed-writer diagnostic: %s %+v", done.Status, done.Failure)
	}
}

func TestWarmRetryValidatesRememberedRunner(t *testing.T) {
	for _, tc := range []struct {
		name, version, want string
		slots               []domain.WarmWorkspaceSlot
	}{
		{"unregistered", domain.RunnerVersionWarmWorkspace, "登记", []domain.WarmWorkspaceSlot{}},
		{"downgraded", "0.11.0", "升级", []domain.WarmWorkspaceSlot{{ID: "slot", ProjectID: "workbench", RepositoryID: "code", State: "available"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, s, g, req, hand := warmRecoveryFixture(t)
			if err := h.parkWarmGeneration(g, req, hand); err != nil {
				t.Fatal(err)
			}
			if err := h.repo.CancelAssistantGeneration(g.UserID, g.SessionID, g.ID); err != nil {
				t.Fatal(err)
			}
			before, _, _, _ := h.repo.WarmWorkspaceAllocation(hand.run.ID)
			if err := h.repo.RunnerHeartbeat("machine", nil, tc.version, time.Now(), time.Now().Add(time.Hour), tc.slots); err != nil {
				t.Fatal(err)
			}
			retry := &AgentToolSession{Mode: s.Mode, ProjectID: s.ProjectID, User: s.User, UserID: s.UserID, ChatOwnerID: s.ChatOwnerID, ChatSessionID: s.ChatSessionID, ExecutionRunnerID: s.ExecutionRunnerID}
			_, err := (&agentToolGateway{deps: h.deps}).openConversationWorkspace(t.Context(), retry, map[string]any{"repositoryId": "code"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("remembered %s Runner accepted retry without %q diagnostic: %v", tc.name, tc.want, err)
			}
			after, state, _, _ := h.repo.WarmWorkspaceAllocation(hand.run.ID)
			if after.Epoch != before.Epoch || state != domain.WarmWorkspaceCanceled {
				t.Fatal("rejected retry replaced its epoch", state)
			}
		})
	}
}

func TestWarmQueueWaitsForOfflineRunnerAndDiagnosesLostRegistration(t *testing.T) {
	h, _, g, req, hand := warmRecoveryFixture(t)
	if err := h.parkWarmGeneration(g, req, hand); err != nil {
		t.Fatal(err)
	}
	p, _ := h.repo.WaitingWarmGenerations()
	r, _ := h.repo.Runner("machine")
	r.Status = domain.RunnerStatusOffline
	if err := h.repo.PutRunner(r); err != nil {
		t.Fatal(err)
	}
	h.resumeWarmGeneration(t.Context(), p[0])
	wait, err := h.repo.WaitingWarmGenerations()
	if err != nil || len(wait) != 1 || wait[0].Wait.Epoch != p[0].Wait.Epoch {
		t.Fatalf("offline wait changed: %+v %v", wait, err)
	}
	if err := h.repo.RunnerHeartbeat("machine", nil, domain.RunnerVersionWarmWorkspace, time.Now(), time.Now().Add(time.Hour), []domain.WarmWorkspaceSlot{}); err != nil {
		t.Fatal(err)
	}
	h.resumeWarmGeneration(t.Context(), p[0])
	done, _ := h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
	if done.Status != "failed" || !strings.Contains(done.Error, "登记") {
		t.Fatalf("removed registration left generation %s without an actionable error: %s", done.Status, done.Error)
	}
	_, state, _, _ := h.repo.WarmWorkspaceAllocation(hand.run.ID)
	if state != domain.WarmWorkspaceCanceled {
		t.Fatal("orphaned invalid demand", state)
	}
}

func TestWarmContinuationProposalTimerStartsFreshGeneration(t *testing.T) {
	h, _, g, req, hand := warmRecoveryFixture(t)
	previous := assistantProposalTimeout
	assistantProposalTimeout = 80 * time.Millisecond
	defer func() { assistantProposalTimeout = previous }()
	hand.nativeSessionID = newAssistantSessionID()
	if err := h.parkWarmGeneration(g, req, hand); err != nil {
		t.Fatal(err)
	}
	var starts int
	prompt := make(chan string, 1)
	warmNativeRunner(t, h, func(r runtime.ConversationProcessRequest) string {
		if r.Operation != "start" {
			return ""
		}
		starts++
		if starts == 1 {
			s, ok := agentToolSessions.get(r.Token)
			if !ok {
				return "missing native tool session"
			}
			if err := s.commitFinal(AgentTerminalResult{Kind: AgentTerminalAssistant, Assistant: &assistantReply{Message: "建议走 SOP", Drafts: []assistantDraft{sopDraft("issue", "start")}}}); err != nil {
				return err.Error()
			}
		} else {
			prompt <- strings.Join(r.Command.Argv, " ")
		}
		return ""
	})
	recoverWarmTurn(t, h)
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		done, _ := h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
		if done.ID != g.ID && done.Status != "running" {
			if done.Status != "completed" {
				t.Fatalf("timed follow-up finished %s: %s", done.Status, done.Error)
			}
			select {
			case p := <-prompt:
				if !strings.Contains(p, assistantProposalExpiredText) {
					t.Fatal("timer follow-up lost the new prompt")
				}
			default:
				t.Fatal("timed follow-up never reached native CLI")
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	done, _ := h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
	t.Fatalf("no timed follow-up after recovered generation: %s %s %s", done.ID, done.Status, done.Error)
}

func TestWarmActiveOwnerSurvivesChangedRegistration(t *testing.T) {
	h, s, g, _, hand := warmRecoveryFixture(t)
	if _, err := h.engine.EnsureWarmWorkspace(t.Context(), hand.run, s.UserID); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.SetAssistantSessionWorkspaceRun(g.UserID, g.SessionID, g.ProjectID, hand.run.ID, true); err != nil {
		t.Fatal(err)
	}
	before, _, _, err := h.repo.WarmWorkspaceAllocation(hand.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.repo.RunnerHeartbeat("machine", nil, "0.11.0", time.Now(), time.Now().Add(time.Hour), []domain.WarmWorkspaceSlot{}); err != nil {
		t.Fatal(err)
	}
	retry := &AgentToolSession{Mode: s.Mode, ProjectID: s.ProjectID, User: s.User, UserID: s.UserID, ChatOwnerID: s.ChatOwnerID, ChatSessionID: s.ChatSessionID, ExecutionRunnerID: s.ExecutionRunnerID}
	if _, err = (&agentToolGateway{deps: h.deps}).openConversationWorkspace(t.Context(), retry, map[string]any{"repositoryId": "code"}); err != nil {
		t.Fatal("existing placement lost its entry", err)
	}
	if err = retry.modeAction(t.Context()); err != nil {
		t.Fatal("existing placement requeued", err)
	}
	after, state, _, err := h.repo.WarmWorkspaceAllocation(hand.run.ID)
	if err != nil || state != domain.WarmWorkspaceReady || after.Epoch != before.Epoch || after.Generation != before.Generation {
		t.Fatalf("existing placement changed: %+v %s %v", after, state, err)
	}
}
