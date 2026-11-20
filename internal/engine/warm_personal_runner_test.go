package engine

import (
	"testing"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/gitwork"
	"agentflow/internal/repo"
)

func TestWarmSOPSelectsOwnersRegisteredRunner(t *testing.T) {
	e, s := cvEEngine(t)
	e.ConfigureExecution("fixture", "host", time.Minute, false)
	const owner = 42
	project := domain.Project{ID: "warm-project", OwnerID: owner, WorkspaceMode: domain.WorkspaceModeWarmPool}
	if err := s.PutProject(project); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProject(domain.Project{ID: "ordinary-project", OwnerID: owner}); err != nil {
		t.Fatal(err)
	}
	wf := domain.WorkflowDefinition{ID: "warm-routing", Name: "Routing", SandboxProfileID: repo.GenericDockerSandboxID, Nodes: []domain.WorkflowNode{{ID: "check", Type: "command", Command: "true"}}}
	if err := s.PutWorkflow(wf); err != nil {
		t.Fatal(err)
	}
	repos := []domain.RepositoryContext{{RepositoryID: "game", CloneURL: "https://example.test/game.git"}, {RepositoryID: "art", CloneURL: "https://example.test/art.git"}}
	slot := func(i int) domain.WarmWorkspaceSlot {
		return domain.WarmWorkspaceSlot{ID: repos[i].RepositoryID, ProjectID: project.ID, RepositoryID: repos[i].RepositoryID, OriginSHA256: gitwork.RemoteFingerprint(repos[i].CloneURL), State: "reserved"}
	}
	register := func(id string, user int64, slots ...domain.WarmWorkspaceSlot) {
		t.Helper()
		if err := s.PutRunner(domain.Runner{ID: id, OwnerUserID: user, Labels: []string{"warm-cloud"}}); err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		if err := s.RunnerHeartbeat(id, nil, domain.RunnerVersionWarmWorkspace, now, now.Add(time.Hour), slots); err != nil {
			t.Fatal(err)
		}
	}
	register("ordinary", owner)
	register("foreign", 99, slot(0), slot(1))
	register("platform", 0, slot(0), slot(1))
	wrongOrigin := slot(1)
	wrongOrigin.OriginSHA256 = gitwork.RemoteFingerprint("https://example.test/other.git")
	register("wrong-origin", owner, slot(0), wrongOrigin)
	wrongProject := slot(0)
	wrongProject.ProjectID = "another-project"
	register("wrong-project", owner, wrongProject, slot(1))
	register("partial", owner, slot(0))
	register("offline", owner, slot(0), slot(1))
	if err := s.MarkRunnerOffline("offline", time.Now()); err != nil {
		t.Fatal(err)
	}
	register("dedicated", owner, slot(0), slot(1))
	register("another-qualified", owner, slot(0), slot(1))
	for _, tc := range []struct {
		name, project, def, pin, want  string
		required, cloud, missingOrigin bool
	}{
		{name: "ordinary default", project: project.ID, def: "ordinary", want: "dedicated"},
		{name: "no default", project: project.ID, want: "dedicated"},
		{name: "unknown frozen origin", project: project.ID, def: "ordinary", missingOrigin: true},
		{name: "compatible default", project: project.ID, def: "another-qualified", want: "another-qualified"},
		{name: "explicit incompatible pin", project: project.ID, def: "dedicated", pin: "ordinary"},
		{name: "explicit compatible pin", project: project.ID, def: "ordinary", pin: "another-qualified", want: "another-qualified"},
		{name: "ordinary checkout", project: "ordinary-project", def: "ordinary", want: "ordinary"},
		{name: "required cloud", project: project.ID, required: true, cloud: true, want: "platform"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := CloudRunnerPool{}
			if tc.required {
				pool = CloudRunnerPool{Label: "warm-cloud", Projects: []string{project.ID}, Required: true}
			}
			e.ConfigureCloudRunnerPool(pool)
			if err := s.SetDefaultRunner(owner, tc.def, time.Now()); err != nil {
				t.Fatal(err)
			}
			frozen := append([]domain.RepositoryContext(nil), repos...)
			if tc.missingOrigin {
				frozen[1].CloneURL = ""
			}
			run, err := e.StartWithRepositoriesContext(wf.ID, "", frozen, domain.TelemetryContext{ProjectID: tc.project, ResponsibleUserID: owner, ExecutionPinned: tc.pin != "", ExecutionRunnerID: tc.pin})
			if tc.want == "" {
				if err == nil {
					t.Fatalf("unexpected runner selection %q", run.ExecutionContext.ExecutionRunnerID)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := run.ExecutionContext; got.ExecutionRunnerID != tc.want || (got.ExecutionPool == domain.ExecutionModeCloud) != tc.cloud {
				t.Fatalf("frozen runner=%q pool=%q, want runner=%q cloud=%t", got.ExecutionRunnerID, got.ExecutionPool, tc.want, tc.cloud)
			}
			if got := s.DefaultRunner(owner); got != tc.def {
				t.Fatalf("personal default changed to %q", got)
			}
		})
	}
}
