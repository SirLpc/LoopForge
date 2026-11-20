package runner

import (
	"context"
	"path/filepath"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

// Each CLI phase has its own container. Text phases only receive their
// session paths. Workspace phases share the checkout through the lease gate.
func (d *Daemon) warmDockerConversationFactory(request domain.RunnerWorkspaceRequest, req runtime.ConversationProcessRequest, workspace string) (*domain.RunnerSandboxSpec, runtime.ConversationSandboxFactory) {
	id := runtime.WarmDockerConversationID(req.SessionKey, req.Transient)
	if request.RunID == "" {
		spec := &domain.RunnerSandboxSpec{
			Profile:    domain.SandboxProfile{Driver: domain.SandboxDriverDocker},
			Dockerfile: &domain.DockerfileDefinition{Content: runtime.IsolatedDockerfile},
		}
		return spec, func(ctx context.Context, nativeRoot string) (runtime.Sandbox, func(), error) {
			requirements := runtime.SandboxRequirements{WarmDockerVolume: d.warmVolumeMount, ConversationRoot: nativeRoot}
			sb, err := runtime.OpenRemoteSandbox(ctx, id, filepath.Join(nativeRoot, "ws"), spec.Profile, requirements,
				runtime.SandboxResources{Dockerfile: *spec.Dockerfile})
			if err != nil {
				return nil, nil, err
			}
			return sb, closeWarmConversationSandbox(sb), nil
		}
	}
	return request.Sandbox, func(ctx context.Context, nativeRoot string) (runtime.Sandbox, func(), error) {
		gate := d.requestGate(request)
		if err := lockConversationWorkspace(ctx, gate); err != nil {
			return nil, nil, err
		}
		task, err := d.workspaceSandboxTask(workspace, request)
		if err != nil {
			gate.Unlock()
			return nil, nil, err
		}
		requirements, resources := d.localTaskSandboxSpec(task)
		requirements.ConversationRoot = nativeRoot
		sb, err := runtime.OpenRemoteSandbox(ctx, id, workspace, task.Sandbox.Profile, requirements, resources)
		if err != nil {
			gate.Unlock()
			return nil, nil, err
		}
		release := closeWarmConversationSandbox(sb)
		execution, err := d.workspaceExecutionSandbox(ctx, sb, request)
		if err == nil {
			_, err = runtime.EnsureWorkspacePreparation(ctx, execution, *request.Workflow, request.Repositories, nil)
		}
		if err != nil {
			release()
			gate.Unlock()
			return nil, nil, err
		}
		gate.Unlock()
		gate.RLock()
		if err := d.checkWarmOperation(ctx, requestGateKey(request), request.Operation, request.WarmWorkspaceLease); err != nil {
			release()
			gate.RUnlock()
			return nil, nil, err
		}
		return sb, onceRelease(release, gate.RUnlock), nil
	}
}

func closeWarmConversationSandbox(sandbox runtime.Sandbox) func() {
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = sandbox.Close(ctx)
	}
}
