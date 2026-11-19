package repo

import (
	"testing"

	"agentflow/internal/domain"
)

func TestProjectWorkspaceModeRoundTrip(t *testing.T) {
	s, err := OpenTest()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	const id = "legacy-workspace"
	if err := s.DB.Create(&projectRow{
		ID: id, ProjectKey: "LEGACY", Name: "Legacy",
		Payload: []byte(`{"id":"legacy-workspace","key":"LEGACY","name":"Legacy"}`),
	}).Error; err != nil {
		t.Fatal(err)
	}

	legacy, ok := s.Project(id)
	if !ok || legacy.WorkspaceMode != domain.WorkspaceModeCheckout {
		t.Fatalf("legacy project workspace mode = %q, found = %v", legacy.WorkspaceMode, ok)
	}
	listed := false
	for _, p := range s.Projects() {
		if p.ID == id {
			listed = true
			if p.WorkspaceMode != domain.WorkspaceModeCheckout {
				t.Fatalf("listed legacy project workspace mode = %q", p.WorkspaceMode)
			}
		}
	}
	if !listed {
		t.Fatal("legacy project missing from project list")
	}

	legacy.WorkspaceMode = domain.WorkspaceModeWarmPool
	if err := s.PutProject(legacy); err != nil {
		t.Fatal(err)
	}
	stored, ok := s.Project(id)
	if !ok || stored.WorkspaceMode != domain.WorkspaceModeWarmPool {
		t.Fatalf("saved project workspace mode = %q, found = %v", stored.WorkspaceMode, ok)
	}

	if err := s.PutProject(domain.Project{ID: id, Key: "LEGACY", Name: "Renamed"}); err != nil {
		t.Fatal(err)
	}
	stored, ok = s.Project(id)
	if !ok || stored.WorkspaceMode != domain.WorkspaceModeWarmPool || stored.Name != "Renamed" {
		t.Fatalf("project after update without workspace mode = %+v, found = %v", stored, ok)
	}
}
