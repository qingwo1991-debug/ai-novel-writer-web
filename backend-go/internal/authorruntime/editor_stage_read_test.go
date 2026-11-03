//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestReadLifecycleEditorStageKeepsPrivateSourceAndLease(t *testing.T) {
	catalog, key, config, source, _ := readFixture(t)
	seed, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	// The original baseline schema intentionally has no project_core/main row.
	// Seed one here so this test exercises the non-null read DTO; a missing row
	// remains a valid null result in the repository and transaction helper.
	if _, err := seed.Exec("INSERT OR IGNORE INTO project_core(id, project_name) VALUES('main', 'Stage fixture')"); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("UPDATE project_core SET project_name='Stage fixture', premise='Read-only premise' WHERE id='main'"); err != nil {
		t.Fatal(err)
	}
	var chapter int64
	if err := seed.QueryRow("SELECT COALESCE(MAX(chapter_number),0)+1 FROM blueprints").Scan(&chapter); err != nil {
		t.Fatal(err)
	}
	if err := store.NewBlueprints(seed).Upsert(context.Background(), map[string]any{
		"chapterNumber": chapter, "title": "同租约蓝图🙂", "role": "转折",
		"purpose": "目的", "keyEvents": "事件", "characters": []any{"甲"},
		"suspenseHook": "悬念", "userGuidance": "", "notes": "", "notesUpdatedAt": "",
	}); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "stage-cookie", ClientID: "stage-tab"}
	identity, err := lifecycle.Open(context.Background(), owner, key, "stage-open")
	if err != nil {
		t.Fatal(err)
	}
	core, err := lifecycle.ReadEditorStageJSON(context.Background(), owner, identity, "db:project-core-get")
	if err != nil || !json.Valid(core) || bytes.Equal(core, []byte("null")) {
		t.Fatalf("project core = %s, %v", core, err)
	}
	var coreDTO struct {
		Premise string `json:"premise"`
	}
	if err := json.Unmarshal(core, &coreDTO); err != nil || coreDTO.Premise != "Read-only premise" {
		t.Fatalf("project core premise = %s, %v", core, err)
	}
	blueprints, err := lifecycle.ReadEditorStageJSON(context.Background(), owner, identity, "db:blueprint-get-all")
	if err != nil || !bytes.Contains(blueprints, []byte("同租约蓝图🙂")) {
		t.Fatalf("blueprints = %s, %v", blueprints, err)
	}
	if _, err := lifecycle.ReadEditorStageJSON(context.Background(), projectsession.Owner{SessionID: owner.SessionID, ClientID: "foreign-tab"}, identity, "db:project-core-get"); err == nil {
		t.Fatal("foreign tab admitted")
	}
	stale := identity
	stale.LeaseID = "stale-lease"
	if _, err := lifecycle.ReadEditorStageJSON(context.Background(), owner, stale, "db:blueprint-get-all"); err == nil {
		t.Fatal("stale lease admitted")
	}
	if _, err := lifecycle.ReadEditorStageJSON(context.Background(), owner, identity, "db:project-core-update"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("write operation admitted: %v", err)
	}
	if err := lifecycle.Close(context.Background(), owner, identity); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("stage reads changed source bytes: %v", err)
	}
}

func TestEditorStageBufferWire(t *testing.T) {
	wire := editorStageWire(map[string]any{"blob": []byte{0, 255}, "rows": []map[string]any{{"blob": []byte{1}}}})
	encoded, err := json.Marshal(wire)
	if err != nil || !bytes.Contains(encoded, []byte(`"type":"Buffer"`)) || !bytes.Contains(encoded, []byte(`"data":[0,255]`)) {
		t.Fatalf("buffer wire = %s, %v", encoded, err)
	}
}
