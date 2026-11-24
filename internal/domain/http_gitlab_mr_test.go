package domain

import "testing"

func TestResolveRunDeliveryMRsAssociatesHTTPGitLabArtifactWithBoundRepository(t *testing.T) {
	const link = "http://192.168.50.10:8443/group/repo/-/merge_requests/2"
	run := Run{
		ExecutionContext: TelemetryContext{ResponsibleUserID: 42},
		Repository:       RepositoryContext{RepositoryID: "local", CloneURL: "http://192.168.50.10:8443/group/repo.git"},
		Artifacts:        []Artifact{{Name: "mr-body", Content: "[MR](" + link + ") from [another repository](http://192.168.50.10:8443/group/other/-/merge_requests/3)"}},
	}
	mrs := ResolveRunDeliveryMRs(run, nil)
	if len(mrs) != 1 || mrs[0].URL != link || mrs[0].RepositoryID != "local" || mrs[0].RegisteredBy != 42 {
		t.Fatalf("unexpected delivery MRs: %+v", mrs)
	}
}

func TestPrivateHTTPGitLabMRURL(t *testing.T) {
	const link = "http://192.168.50.10:8443/g/r/-/merge_requests/1"
	const key = "gitlab-http:192.168.50.10:8443/g/r"
	project, iid, canonical, ok := ParseDeliveryMRURL(link)
	if !ok || project != key || iid != 1 || canonical != link || GongfengMRURL(project, iid) != link {
		t.Fatalf("unexpected canonical MR: %q %d %q %v", project, iid, canonical, ok)
	}
	for _, raw := range []string{"http://gitlab.example.org/g/r/-/merge_requests/1", "http://169.254.169.254/g/r/-/merge_requests/1"} {
		if _, _, _, ok := ParseDeliveryMRURL(raw); ok {
			t.Errorf("accepted unsupported MR address %q", raw)
		}
	}
}

func TestUnboundPrivateHTTPArtifactKeepsHostedTelemetryDelivery(t *testing.T) {
	const github = "https://github.com/org/repo/pull/7"
	run := Run{
		Artifacts: []Artifact{{Name: "mr-body", Content: "http://192.168.50.10:8443/group/repo/-/merge_requests/2"}},
		Telemetry: []TelemetryEvent{{Attributes: map[string]any{"url": github}}},
	}
	mrs := ResolveRunDeliveryMRs(run, nil)
	if len(mrs) != 1 || mrs[0].URL != github {
		t.Fatalf("expected the hosted delivery from telemetry, got %+v", mrs)
	}
}
