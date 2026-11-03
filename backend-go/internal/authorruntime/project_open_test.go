//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

func TestProjectOpenCoreProjectsRealRowAndLease(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.Exec(`CREATE TABLE project_core (
		id TEXT PRIMARY KEY, project_name TEXT, writing_language TEXT, genre TEXT,
		sub_genre TEXT, target_audience TEXT, total_chapters INTEGER, words_per_chapter INTEGER,
		creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT,
		narrative_pov TEXT, core_outline TEXT, world_setting TEXT, golden_finger TEXT,
		protagonist_profile TEXT, global_guidance TEXT, writing_style TEXT, reference_works TEXT,
		character_states TEXT, created_at TEXT, updated_at TEXT);
		INSERT INTO project_core VALUES ('main','Novel','en-US','fantasy','x','adult',120,2000,
		'consistency-first',4,'three_act','first_person','outline','world','gold','lead',
		'guidance','plain','reference','{}','2026-01-01 00:00:00','2026-01-02 01:02:03')`)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	project, err := readProjectOpenCore(context.Background(), tx, projectsession.Context{
		ProjectID: "project-id", ProjectPath: "/trusted/project", LeaseID: "lease-id",
	})
	if err != nil {
		t.Fatal(err)
	}
	if project["id"] != "project-id" || project["path"] != "/trusted/project" || project["sessionLease"] != "lease-id" {
		t.Fatalf("identity not projected: %#v", project)
	}
	if project["createdAt"] != "2026-01-01T00:00:00.000Z" || project["updatedAt"] != "2026-01-02T01:02:03.000Z" {
		t.Fatalf("database timestamps not projected: %#v", project)
	}
	config := project["novelConfig"].(map[string]any)
	if config["narrativePOV"] != "first_person" || config["writingLanguage"] != "en-US" || config["totalChapters"] != int64(120) {
		t.Fatalf("core fields not projected: %#v", config)
	}
}

func TestProjectOpenReadOnlyFailedBCannotReplaceA(t *testing.T) {
	catalog, key, config, source, original := readFixture(t)
	// The shared oracle is a schema baseline; seed only this disposable copy.
	seed, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("INSERT INTO project_core(id, project_name) VALUES('main', 'Synthetic novel')"); err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "cookie", ClientID: "tab"}
	opened, err := lifecycle.OpenProjectReadOnly(context.Background(), owner, key, "open-a")
	if err != nil {
		t.Fatal(err)
	}
	if !opened.Success || !opened.DBReady || !opened.DatabaseRestored || opened.Project == nil || opened.RequestToken != "open-a" {
		t.Fatalf("real open not ready: %+v", opened)
	}
	if opened.Project["path"] != filepath.Dir(filepath.Dir(source)) || opened.Project["sessionLease"] == "" {
		t.Fatalf("untrusted project identity: %#v", opened.Project)
	}
	failed, err := lifecycle.OpenProjectReadOnly(context.Background(), owner, "unknown-key", "open-b")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Success || failed.Project != nil || !failed.DBReady || failed.ActiveProjectPath == nil || *failed.ActiveProjectPath != opened.Project["path"] {
		t.Fatalf("failed B did not preserve A: %+v", failed)
	}
	// B has an admitted schema but no project_core main row. Its metadata read
	// must fail before the coordinator commits a replacement lease.
	keyB := secondProjectKey(t, catalog, config, original)
	failed, err = lifecycle.OpenProjectReadOnly(context.Background(), owner, keyB, "empty-b")
	if err != nil {
		t.Fatal(err)
	}
	if failed.Success || failed.Project != nil || !failed.DBReady || failed.ActiveProjectPath == nil || *failed.ActiveProjectPath != opened.Project["path"] {
		t.Fatalf("missing core switched away from A: %+v", failed)
	}
	refreshed, err := lifecycle.OpenProjectReadOnly(context.Background(), owner, key, "refresh-a")
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed.Success || refreshed.Project["sessionLease"] != opened.Project["sessionLease"] || refreshed.RequestToken != "refresh-a" {
		t.Fatalf("refresh rotated lease: %+v", refreshed)
	}
}
