package engine

import (
	"log"

	"agentflow/internal/domain"
)

func (e *Engine) findHTTPGitLabDelivery(run domain.Run, rc domain.RepositoryContext, project string) (domain.DeliveryMR, bool) {
	if e.FindBranchMRsInto == nil {
		return domain.DeliveryMR{}, false
	}
	mrs, err := e.FindBranchMRsInto(domain.BranchMRQuery{
		ProjectID: run.ExecutionContext.ProjectID, RemoteProject: project, SourceBranch: rc.WorkBranch,
		TargetBranch: repositoryBaseBranch(rc), ResponsibleUserID: run.ExecutionContext.ResponsibleUserID,
	})
	if err != nil {
		log.Printf("resolve GitLab delivery MR run=%s project=%s branch=%s: %v", run.ID, project, rc.WorkBranch, err)
	}
	for _, state := range []string{"opened", "merged", "closed"} {
		for _, mr := range mrs {
			if deliveryMRState(mr) == state {
				return mr, true
			}
		}
	}
	return domain.DeliveryMR{}, false
}

func httpGitLabDeliveryHint(run domain.Run) string {
	for _, repo := range run.RepositoryContexts() {
		if domain.HTTPGitLabProject(repo.CloneURL) != "" {
			return "内网 HTTP GitLab 可直接使用 glab，也可通过 Git credential helper 请求 HTTP 令牌，再调用仓库所在主机的 GitLab API。"
		}
	}
	return ""
}
