package repo

import (
	"fmt"
	"os"
	"testing"
	"time"

	"agentflow/internal/domain"

	"github.com/go-sql-driver/mysql"
	"gorm.io/gorm"
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

// The omitted write pauses before its first INSERT. On the old path, it
// had already observed a missing row under READ COMMITTED at that point.
func TestMySQLProjectWorkspaceModeConcurrentCreate(t *testing.T) {
	dsn := os.Getenv("AGENTFLOW_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("AGENTFLOW_TEST_MYSQL_DSN is not set")
	}
	config, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal("invalid test MySQL DSN")
	}
	if config.Params == nil {
		config.Params = make(map[string]string)
	}
	config.Params["transaction_isolation"] = "'READ-COMMITTED'"

	open := func() *Store {
		t.Helper()
		store, err := Open(config.FormatDSN())
		if err != nil {
			t.Fatalf("open test MySQL store failed (%T); DSN omitted", err)
		}
		t.Cleanup(store.Close)
		return store
	}
	omittedStore, explicitStore := open(), open()
	id := fmt.Sprintf("workspace-mode-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		if err := explicitStore.DB.Where("id = ?", id).Delete(&projectRow{}).Error; err != nil {
			t.Errorf("delete test project: %v", err)
		}
	})

	beforeInsert := make(chan struct{})
	continueInsert := make(chan struct{})
	defer close(continueInsert)
	if err := omittedStore.DB.Callback().Create().Before("gorm:create").Register("pause_project_insert", func(tx *gorm.DB) {
		if tx.Statement.Table == "projects" {
			close(beforeInsert)
			<-continueInsert
		}
	}); err != nil {
		t.Fatal(err)
	}
	omittedDone := make(chan error, 1)
	go func() {
		omittedDone <- omittedStore.PutProject(domain.Project{ID: id, Name: "Omitted mode"})
	}()
	select {
	case <-beforeInsert:
	case err := <-omittedDone:
		t.Fatalf("omitted write completed before INSERT: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("omitted write did not reach INSERT")
	}
	if err := explicitStore.PutProject(domain.Project{
		ID: id, Name: "Explicit mode", WorkspaceMode: domain.WorkspaceModeWarmPool,
	}); err != nil {
		t.Fatal(err)
	}
	continueInsert <- struct{}{}
	select {
	case err := <-omittedDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("omitted write did not complete")
	}
	stored, ok := explicitStore.Project(id)
	if !ok || stored.Name != "Omitted mode" || stored.WorkspaceMode != domain.WorkspaceModeWarmPool {
		t.Fatalf("project after competing creates = %+v, found = %v", stored, ok)
	}
}
