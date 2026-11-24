package engine

import (
	"testing"

	"agentflow/internal/domain"
)

func TestPrivateHTTPGitLabRunChecksAndRegistersCurrentMR(t *testing.T) {
	const project = "gitlab-http:192.168.50.10:8443/group/repo"
	run := domain.Run{
		ExecutionContext: domain.TelemetryContext{ResponsibleUserID: 42},
		Repository: domain.RepositoryContext{
			RepositoryID: "local", CloneURL: "http://192.168.50.10:8443/group/repo.git", WorkBranch: "feature", BaseBranch: "main",
		},
	}
	e := &Engine{FindBranchMRsInto: func(q domain.BranchMRQuery) ([]domain.DeliveryMR, error) {
		if q.RemoteProject != project || q.ResponsibleUserID != 42 || q.SourceBranch != "feature" || q.TargetBranch != "main" {
			t.Fatalf("unexpected query: %+v", q)
		}
		return []domain.DeliveryMR{
			{IID: 1, State: "merged", HeadSHA: "old"},
			{IID: 2, State: "opened", HeadSHA: "current"},
		}, nil
	}}
	if err := e.verifyRunMRs(run, "", func(domain.RepositoryContext) (pushedWork, error) {
		return pushedWork{Tip: "current", Ahead: 1}, nil
	}); err != nil {
		t.Fatal(err)
	}
	mrs := e.deliveryMRsFromRun(run)
	if len(mrs) != 1 || mrs[0].IID != 2 || mrs[0].RegisteredBy != 42 || mrs[0].URL != "http://192.168.50.10:8443/group/repo/-/merge_requests/2" {
		t.Fatalf("unexpected delivery MRs: %+v", mrs)
	}
}
