package tracker

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"agentflow/internal/domain"
)

func (s *Service) findHTTPGitLabBranchMRs(project, source, target string, userID int64) ([]domain.DeliveryMR, error) {
	platform, host, path, valid := domain.ExternalMRProject(project)
	if !valid || platform != domain.GitPlatformGitLab {
		return nil, fmt.Errorf("无效的 GitLab MR 项目 %s", project)
	}
	creds, _ := s.projectAuth.(GitHTTPCredentialStore)
	if creds == nil || userID <= 0 {
		return nil, fmt.Errorf("无法读取本轮责任人在 %s 的 GitLab 访问令牌", host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cred, found := creds.GitHTTPCredentialForHost(ctx, userID, host)
	if !found || cred.Token == "" || cred.Platform != platform {
		return nil, fmt.Errorf("请为本轮责任人配置 %s 的 GitLab 访问令牌", host)
	}
	query := url.Values{"source_branch": {source}, "target_branch": {target}, "scope": {"all"}, "order_by": {"created_at"}, "sort": {"desc"}, "per_page": {"100"}}
	var list []struct {
		IID             int    `json:"iid"`
		State           string `json:"state"`
		Source          string `json:"source_branch"`
		Target          string `json:"target_branch"`
		SourceProjectID int64  `json:"source_project_id"`
		TargetProjectID int64  `json:"target_project_id"`
		SHA             string `json:"sha"`
	}
	if err := s.gitHostGet(ctx, cred, "/projects/"+url.PathEscape(path)+"/merge_requests?"+query.Encode(), &list); err != nil {
		return nil, err
	}
	var out []domain.DeliveryMR
	for _, mr := range list {
		if mr.IID <= 0 || mr.Source != source || mr.Target != target || mr.SourceProjectID <= 0 || mr.SourceProjectID != mr.TargetProjectID {
			continue
		}
		out = append(out, domain.DeliveryMR{
			Project: project, IID: mr.IID, URL: domain.GongfengMRURL(project, mr.IID),
			TargetBranch: target, GongfengState: gitLabMRState(mr.State), HeadSHA: mr.SHA, RegisteredBy: userID,
		})
	}
	return out, nil
}

func gitLabMRState(state string) string {
	if state == "locked" {
		return "opened"
	}
	return normalizeMRState(state)
}
