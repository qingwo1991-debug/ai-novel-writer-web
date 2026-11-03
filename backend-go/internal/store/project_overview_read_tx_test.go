package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

func TestCurrentProjectOverviewUsesOnlyVerifiedStageFacts(t *testing.T) {
	ctx := context.Background()
	db, _, _ := newFixture(t)
	mustExec(t, db, projectCoreFallbackSchema)
	mustExec(t, db, "CREATE TABLE blueprints(chapter_number INTEGER NOT NULL)")
	mustExec(t, db, "CREATE TABLE reviews(base_draft_id INTEGER NOT NULL)")
	mustExec(t, db, "CREATE TABLE characters(retired INTEGER NOT NULL DEFAULT 0)")
	mustExec(t, db, `INSERT INTO project_core(id,project_name,genre,target_audience,core_outline,total_chapters,words_per_chapter,premise) VALUES('main',' Book 😀 ','fantasy','adult','outline',2,1800,'premise')`)
	mustExec(t, db, "INSERT INTO blueprints(chapter_number) VALUES(1),(2)")
	seed(t, db, 10, 1, 1, "draft", "旧稿")
	seed(t, db, 11, 1, 2, "finalized", "定稿正文😀")
	seed(t, db, 12, 2, 1, "draft", "最新章节🙂")
	mustExec(t, db, "UPDATE drafts SET word_count=100 WHERE id=11")
	mustExec(t, db, "UPDATE drafts SET word_count=200 WHERE id=12")
	mustExec(t, db, "INSERT INTO reviews(base_draft_id) VALUES(10)")
	mustExec(t, db, "INSERT INTO characters(retired) VALUES(0),(0),(1)")
	mustExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,content_hash,content_snapshot) VALUES('f-11',11,1,?,?)`, textHash("定稿正文😀"), "定稿正文😀")
	read := func() *ProjectOverview {
		t.Helper()
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		value, err := ReadCurrentProjectOverviewTx(ctx, tx, "project-a")
		if err != nil || value == nil {
			t.Fatalf("overview = %#v, %v", value, err)
		}
		return value
	}
	value := read()
	if value.ProjectID != "project-a" || value.Name != "Book 😀" || value.TotalWords != 300 || value.Characters != 2 || value.FinalizedChapters != 1 || value.Excerpt != "最新章节🙂" || !value.SetupReady || value.PlannedChapters != 2 || len(value.Revision) != 64 {
		t.Fatalf("overview facts: %#v", value)
	}
	wantStatus := []string{"in-progress", "in-progress", "completed", "completed", "in-progress", "in-progress"}
	wantCount := []int{3, 1, 2, 2, 1, 1}
	for i, stage := range value.Stages {
		if stage.Status != wantStatus[i] || stage.Count != wantCount[i] {
			t.Fatalf("stage %d = %#v", i, stage)
		}
	}
	if next := read(); next.Revision != value.Revision {
		t.Fatal("unchanged facts changed opaque revision")
	}
	mustExec(t, db, "UPDATE finalization_outbox SET content_snapshot='tampered' WHERE draft_id=11")
	tampered := read()
	if tampered.FinalizedChapters != 0 || tampered.Stages[5].Count != 0 || tampered.Revision == value.Revision {
		t.Fatalf("tampered receipt counted or revision stale: %#v", tampered)
	}
	mustExec(t, db, "DELETE FROM project_core")
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	missing, err := ReadCurrentProjectOverviewTx(ctx, tx, "project-a")
	if err != nil || missing != nil {
		t.Fatalf("missing core = %#v, %v", missing, err)
	}
	if strings.Contains(value.Revision, "/") {
		t.Fatal("revision exposed a path")
	}
}
