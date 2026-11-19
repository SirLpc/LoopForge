package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/repo"
)

func (h *assistantHandlers) parkWarmGeneration(g domain.AssistantGeneration, req assistantRequest, p *warmConversationHandoff, diagnostics ...domain.AssistantDiagnostics) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ev := domain.AssistantActivityEvent{Kind: "status", ID: "workspace.pool_waiting", Text: "正在等待预热工作区"}
	event, _ := json.Marshal(ev)
	if g.Stream == nil {
		g.Stream = &domain.AssistantStreamSnapshot{}
	}
	g.Stream.Sequence++
	g.Stream.Activity = append(g.Stream.Activity, ev)
	g.Events = append(g.Events, domain.AssistantGenerationEvent{Sequence: g.Stream.Sequence, Data: event})
	return h.repo.ParkWarmGeneration(g, p.run, repo.AssistantWarmWait{RunID: p.run.ID, Previous: p.previous, NativeSessionID: p.nativeSessionID, Request: raw}, diagnostics...)
}

func (h *assistantHandlers) startWarmGenerationRecovery(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			h.reconcileWarmGenerations(ctx)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// A database claim immediately before the next native phase fences competing
// controllers. Only idempotent Runner operations occur while the row is parked.
func (h *assistantHandlers) reconcileWarmGenerations(ctx context.Context) {
	pending, err := h.repo.WaitingWarmGenerations()
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("read parked workspace generations: %v", err)
		}
		return
	}
	for _, p := range pending {
		if generationExecutingHere(p.Generation.ID) {
			continue
		}
		if _, loaded := h.warmAdmissions.LoadOrStore(p.Generation.ID, true); loaded {
			continue
		}
		done := inflightTasks.Add()
		go func() { defer done(); defer h.warmAdmissions.Delete(p.Generation.ID); h.resumeWarmGeneration(ctx, p) }()
	}
}

func (h *assistantHandlers) resumeWarmGeneration(ctx context.Context, p repo.WaitingAssistantGeneration) {
	// Keep the prepared owner pinned until the resumed CLI turn exits. The
	// foreground lease also serializes Runner admission probes across replicas.
	useCtx, cancelUse := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelUse()
	stopShutdown := context.AfterFunc(ctx, cancelUse)
	defer stopShutdown()
	var turn *assistantWorkspaceTurn
	var req assistantRequest
	admissionErr := json.Unmarshal(p.Wait.Request, &req)
	run, ok := h.repo.RunState(p.Wait.RunID)
	if !ok {
		admissionErr = errors.New("预热工作区不存在")
	}
	actor, ok := h.repo.User(p.Generation.ActorID)
	if !ok {
		admissionErr = errors.New("对话发言人已不可用")
	}
	if admissionErr == nil && !p.Generation.CancelRequested {
		_, state, _, err := h.repo.WarmWorkspaceAllocation(run.ID)
		if err != nil {
			return
		}
		if state == domain.WarmWorkspaceReserved || state == domain.WarmWorkspaceReady {
			turn = newAssistantWorkspaceTurn(h.engine, cancelUse)
			defer turn.close(context.WithoutCancel(ctx))
			turn.recordLeases(func(id, token string) { h.recordGenerationWorkspaceLease(p.Generation, id, token) })
			if _, err = turn.hold(useCtx, run.ID); err != nil {
				if errors.Is(err, repo.ErrWorkspaceInUse) && p.Lease.RunID == run.ID {
					_, _ = h.repo.ReleaseAbandonedWorkspaceUse(run.ID, p.Lease.Token, time.Now())
				}
				return
			}
		}
		stopWatch := func() {}
		if turn != nil {
			stopWatch = h.watchWarmAdmission(useCtx, p.Generation, cancelUse)
		}
		ready, err := h.engine.AdvanceConversationWarmWorkspace(useCtx, run, p.Wait.Epoch, actor.ID)
		stopWatch()
		if err == nil && !ready {
			return
		}
		admissionErr = err
	}
	if !stopShutdown() || ctx.Err() != nil {
		return
	}
	g, claimed, err := h.repo.ClaimWarmContinuation(p, admissionErr == nil)
	if errors.Is(err, repo.ErrConversationWorkspaceChanged) {
		admissionErr = err
		g, claimed, err = h.repo.ClaimWarmContinuation(p, false)
	}
	if err != nil {
		log.Printf("claim workspace continuation %s: %v", p.Generation.ID, err)
		return
	}
	if !claimed {
		return
	}
	if admissionErr == nil && !g.CancelRequested && p.Wait.Previous != "" && p.Wait.Previous != p.Wait.RunID {
		if err := h.engine.ReleaseConversationWorkspace(p.Wait.Previous); err != nil {
			log.Printf("release previous conversation workspace %s: %v", p.Wait.Previous, err)
		}
	}
	parent := context.WithValue(useCtx, warmNativeSessionKey{}, p.Wait.NativeSessionID)
	if turn != nil {
		parent = context.WithValue(parent, warmAdmittedTurnKey{}, turn)
	}
	if admissionErr != nil {
		parent = context.WithValue(parent, warmContinuationErrorKey{}, admissionErr)
	}
	h.executeGeneration(parent, req, g, domainUserToAuth(actor))
}

// A conversation may still be in workspace mode after its previous checkout
// was safely returned. Park the turn before dispatching any new CLI writer.
func (h *assistantHandlers) boundWarmHandoff(ctx context.Context, s *AgentToolSession) (*warmConversationHandoff, error) {
	if s.RuntimeMode != domain.AssistantModeWorkspace || s.RunID != "" {
		return nil, nil
	}
	id := s.workspaceRun()
	run, ok := h.repo.RunState(id)
	if !ok || !run.IsWorkspace() || run.ExecutionContext.WorkspaceMode != domain.WorkspaceModeWarmPool {
		return nil, nil
	}
	l, state, _, err := h.repo.WarmWorkspaceAllocation(id)
	if err != nil || state == domain.WarmWorkspaceReady {
		return nil, err
	}
	if state == domain.WarmWorkspaceReleased && l.Generation != 0 {
		if err := h.engine.AuthorizeConversationWarmWorkspace(ctx, run, s.UserID); err != nil {
			return nil, err
		}
	}
	return &warmConversationHandoff{run: run, previous: id}, nil
}

// Continue the same diagnostic timeline on whichever API instance won admission.
func (h *assistantHandlers) restoreWarmDiagnostics(rec *assistantGenerationRecorder, g domain.AssistantGeneration) {
	report, ok := h.repo.AssistantDiagnostics(g.UserID, g.SessionID, g.ID)
	if !ok {
		return
	}
	d := rec.diagnostics
	d.mu.Lock()
	defer d.mu.Unlock()
	d.report = report
	d.sequence = len(report.Attempts) + report.Dropped["attempts"]
	for _, ev := range report.Timeline {
		d.timelineSequence = max(d.timelineSequence, ev.Sequence)
	}
	for _, tool := range report.Tools {
		var seq int
		if _, err := fmt.Sscanf(tool.ID, "gateway/%d", &seq); err == nil {
			d.toolSequence = max(d.toolSequence, seq)
		}
	}
	d.appendTimelineLocked(domain.AssistantDiagnosticEvent{Source: "platform", Kind: "workspace.pool_continued"})
}

// Git checkout may take minutes. Stop its work on any controller as soon as
// the durable generation is cancelled. Controller shutdown only stops the RPC.
func (h *assistantHandlers) watchWarmAdmission(ctx context.Context, g domain.AssistantGeneration, cancel context.CancelFunc) func() {
	watch, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-watch.Done():
				return
			case <-tick.C:
				current, ok := h.repo.PeekAssistantGeneration(g.UserID, g.SessionID)
				if ok && (current.ID != g.ID || current.CancelRequested || current.Status != "running") {
					cancel()
					return
				}
			}
		}
	}()
	return func() { stop(); <-done }
}
