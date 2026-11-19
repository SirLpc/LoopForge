package engine

import (
	"fmt"
	"strings"

	"agentflow/internal/domain"
)

// StartConversationWorkspace allocates an idle workspace, never an SOP scheduler
// or a group of role tasks. Docker definitions are frozen by the normal run builder.
func (e *Engine) StartConversationWorkspace(input string, actor domain.TelemetryContext, sandboxID string) (domain.Run, error) {
	return e.startConversationWorkspace(input, actor, sandboxID, nil)
}

func (e *Engine) StartRepositoryConversationWorkspace(input string, actor domain.TelemetryContext, sandboxID string, repository domain.RepositoryContext) (domain.Run, error) {
	registered, ok := e.repo.Repository(repository.RepositoryID)
	if !ok || actor.ProjectID == "" || registered.ProjectID != actor.ProjectID {
		return domain.Run{}, fmt.Errorf("仓库未登记在当前项目")
	}
	return e.startConversationWorkspace(input, actor, sandboxID, []domain.RepositoryContext{repository})
}

func (e *Engine) startConversationWorkspace(input string, actor domain.TelemetryContext, sandboxID string, repositories []domain.RepositoryContext) (domain.Run, error) {
	if strings.TrimSpace(input) == "" {
		return domain.Run{}, fmt.Errorf("请填写工作区目标")
	}
	actor.ProjectID = strings.TrimSpace(actor.ProjectID)
	if actor.ProjectID == "" {
		if actor.ResponsibleUserID <= 0 {
			return domain.Run{}, fmt.Errorf("个人工作区需要登录用户")
		}
	} else if _, ok := e.repo.Project(actor.ProjectID); !ok {
		return domain.Run{}, fmt.Errorf("项目不存在")
	}
	if _, err := e.repo.ConversationSandbox(actor.ProjectID, sandboxID); err != nil {
		return domain.Run{}, err
	}
	wf := domain.WorkflowDefinition{ID: domain.ConversationWorkspaceWorkflowID, Name: "对话工作区", Version: 1, ProjectID: actor.ProjectID, SandboxProfileID: sandboxID, Nodes: []domain.WorkflowNode{}, Edges: []domain.WorkflowEdge{}}
	return e.startWorkflowDefinition(wf, input, repositories, actor, true)
}
