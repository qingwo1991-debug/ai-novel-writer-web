//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func testOwner() projectsession.Owner {
	return projectsession.Owner{SessionID: "synthetic-session", ClientID: "synthetic-tab"}
}

func durableLifecycle(t *testing.T) (*ReadLifecycle, string, projectsession.Context, string, string) {
	t.Helper()
	catalog, key, config, databasePath, _ := readFixture(t)
	// The shared oracle is a schema baseline with no project_core main row;
	// seed this disposable copy so the durable open's core read succeeds.
	seed, err := sql.Open("sqlite3", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec("INSERT INTO project_core(id, project_name, writing_language, genre, sub_genre, target_audience, total_chapters, words_per_chapter, creative_strategy, narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline, world_setting, golden_finger, protagonist_profile, global_guidance, writing_style, reference_works, character_states, created_at, updated_at) VALUES('main', 'Synthetic novel', 'zh-CN', 'fantasy', 'x', 'adult', 120, 2000, 'consistency-first', 4, 'three_act', 'first_person', 'outline', 'world', 'gold', 'lead', 'guidance', 'plain', 'reference', '{}', '2026-01-01 00:00:00', '2026-01-02 01:02:03')"); err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	// The lifecycle resolves the exact project per open from the catalog key;
	// its baseline config must carry no ProjectPath.
	lifecycleConfig := config
	lifecycleConfig.ProjectPath = ""
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle, err := NewReadLifecycleWithHeldLock(catalog, lifecycleConfig, held)
	if err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lifecycle.Shutdown() })
	frozen := projectsession.Context{ProjectID: fixtureProjectID(), ProjectPath: config.ProjectPath, LeaseID: "placeholder"}
	return lifecycle, key, frozen, config.DataRoot, config.ProjectPath
}

func TestDurableOpenSaveCloseReopenPersists(t *testing.T) {
	lifecycle, key, _, dataRoot, projectPath := durableLifecycle(t)
	owner := testOwner()
	first, err := lifecycle.OpenProjectDurable(context.Background(), owner, key, "token-1")
	if err != nil {
		t.Fatal(err)
	}
	if !first.Success || first.Project == nil || first.RequestToken != "token-1" {
		t.Fatalf("open DTO malformed: %+v", first)
	}
	current, present, err := lifecycle.CurrentProjectDurable(context.Background(), owner)
	if err != nil || !present {
		t.Fatalf("current durable context: %+v %v", current, err)
	}
	if current.ProjectID != fixtureProjectID() || current.ProjectPath != projectPath {
		t.Fatalf("unexpected frozen context: %+v", current)
	}
	// The single-writer lock must remain held for the whole lifetime.
	if _, err := writerlock.AcquireLegacy(dataRoot); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("durable lifetime released gateway lock: %v", err)
	}
	// A wrong frozen context must be rejected before any write.
	stale := projectsession.Context{ProjectID: "other", LeaseID: current.LeaseID, ProjectPath: current.ProjectPath}
	if _, err := lifecycle.SaveCore(context.Background(), owner, stale, map[string]any{"projectName": "骗写"}); err == nil {
		t.Fatal("stale project context admitted a write")
	}
	updated, err := lifecycle.SaveCore(context.Background(), owner, current, map[string]any{"projectName": "持久化新书名"})
	if err != nil {
		t.Fatal(err)
	}
	name, _ := updated["projectName"].(string)
	if name != "持久化新书名" {
		t.Fatalf("save result missing update: %q", name)
	}
	// The write must be committed to the author's own database immediately.
	verify, err := sqlOpenReadOnly(projectPath + "/.ai-novel/project.db")
	if err != nil {
		t.Fatal(err)
	}
	var persisted string
	if err := verify.QueryRow("SELECT project_name FROM project_core WHERE id='main'").Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if err := verify.Close(); err != nil {
		t.Fatal(err)
	}
	if persisted != "持久化新书名" {
		t.Fatalf("author database not persisted: %q", persisted)
	}
	// Close, then reopen through the same lifecycle and read back the state.
	if err := lifecycle.CloseProjectDurable(context.Background(), owner, current); err != nil {
		t.Fatal(err)
	}
	second, err := lifecycle.OpenProjectDurable(context.Background(), owner, key, "token-2")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Success || second.Project == nil {
		t.Fatalf("reopen failed: %+v", second)
	}
	reopened, _ := second.Project["projectName"].(string)
	if reopened != "持久化新书名" {
		t.Fatalf("reopen lost update: %q", reopened)
	}
}

func TestDurableSaveRejectsClosedHandle(t *testing.T) {
	lifecycle, key, frozen, _, _ := durableLifecycle(t)
	owner := testOwner()
	if _, err := lifecycle.OpenProjectDurable(context.Background(), owner, key, "token"); err != nil {
		t.Fatal(err)
	}
	current, present, err := lifecycle.CurrentProjectDurable(context.Background(), owner)
	if err != nil || !present {
		t.Fatal("missing current context", err)
	}
	if err := lifecycle.CloseProjectDurable(context.Background(), owner, current); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.CloseProjectDurable(context.Background(), owner, current); err == nil {
		t.Fatal("double close accepted")
	}
	if _, err := lifecycle.SaveCore(context.Background(), owner, frozen, map[string]any{"projectName": "closed"}); err == nil {
		t.Fatal("save accepted after close")
	}
}

func TestDurableRequiresCatalogAndPermission(t *testing.T) {
	lifecycle, _, _, _, _ := durableLifecycle(t)
	if result, _ := lifecycle.OpenProjectDurable(context.Background(), testOwner(), "no-such-catalog-key", "bad-token"); result.Success || result.Error == "" {
		t.Fatalf("unregistered key accepted: %+v", result)
	}
	if result, _ := lifecycle.OpenProjectDurable(context.Background(), projectsession.Owner{}, "no-such-catalog-key", "token"); result.Success || result.Error == "" {
		t.Fatalf("invalid owner accepted: %+v", result)
	}
}

func TestDurableLockReleasesAfterShutdown(t *testing.T) {
	lifecycle, key, _, dataRoot, _ := durableLifecycle(t)
	owner := testOwner()
	if _, err := lifecycle.OpenProjectDurable(context.Background(), owner, key, "token"); err != nil {
		t.Fatal(err)
	}
	if _, err := writerlock.AcquireLegacy(dataRoot); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("lock not held while lifecycle active: %v", err)
	}
	if err := lifecycle.Shutdown(); err != nil {
		t.Fatal(err)
	}
	reacquired, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatalf("lifecycle shutdown did not release the lock: %v", err)
	}
	if err := reacquired.Close(); err != nil {
		t.Fatal(err)
	}
}

func sqlOpenReadOnly(database string) (*sql.DB, error) {
	return sql.Open("sqlite3", "file:"+database+"?mode=ro")
}
