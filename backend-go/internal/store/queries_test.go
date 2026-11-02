package store

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestDraftChapterQueriesMissingReturnOriginalShape(t *testing.T) {
	_, repository, _ := newFixture(t)
	ctx := context.Background()
	for name, get := range map[string]func(context.Context, int64) (*DraftMeta, error){
		"latest":    repository.GetLatestByChapter,
		"finalized": repository.GetFinalizedByChapter,
	} {
		t.Run(name, func(t *testing.T) {
			meta, err := get(ctx, 123)
			if err != nil || meta != nil {
				t.Fatalf("missing chapter = %+v, %v", meta, err)
			}
			encoded, err := json.Marshal(meta)
			if err != nil || string(encoded) != "null" {
				t.Fatalf("missing chapter JSON = %s, %v", encoded, err)
			}
		})
	}
	if maximum, err := repository.GetMaxFinalizedChapter(ctx); err != nil || maximum != 0 {
		t.Fatalf("empty finalized maximum = %d, %v", maximum, err)
	}
	if next, err := repository.GetNextVersion(ctx, 123); err != nil || next != 1 {
		t.Fatalf("empty next version = %d, %v", next, err)
	}
}

func TestDraftChapterQueriesVersionAndStatusSelection(t *testing.T) {
	db, repository, _ := newFixture(t)
	ctx := context.Background()
	// IDs, timestamps and receipts must not override the descending version.
	seed(t, db, 90, 1, 1, "finalized", "old finalized")
	seed(t, db, 10, 1, 3, "finalized", "new finalized")
	seed(t, db, 20, 1, 4, "superseded", "newer superseded")
	seed(t, db, 30, 1, 5, "draft", "newer working")
	seed(t, db, 40, 1, 6, "unknown-status", "latest unknown")
	seed(t, db, 50, 2, 100, "finalized", "other chapter")
	mustExec(t, db, "UPDATE drafts SET updated_at='9999-12-31' WHERE id=90")
	mustExec(t, db, `INSERT INTO finalization_outbox VALUES
	 ('old-receipt',90,1,'old title',?,?),
	 ('new-receipt',10,1,' new title ',?,?),
	 ('unknown-receipt',40,1,'must stay hidden',?,?)`,
		textHash("old finalized"), "old finalized", textHash("new finalized"), "new finalized",
		textHash("latest unknown"), "latest unknown")
	latest, err := repository.GetLatestByChapter(ctx, 1)
	if err != nil || latest == nil || latest.ID != 40 || latest.Status != "unknown-status" || latest.ChapterTitle != "" {
		t.Fatalf("latest = %+v, %v", latest, err)
	}
	finalized, err := repository.GetFinalizedByChapter(ctx, 1)
	if err != nil || finalized == nil || finalized.ID != 10 || finalized.ChapterTitle != " new title " {
		t.Fatalf("finalized = %+v, %v", finalized, err)
	}

	for index, status := range []string{"draft", "revised", "reviewed", "superseded", "unknown-status", "Finalized", "finalized ", ""} {
		chapter := int64(index + 10)
		seed(t, db, chapter+100, chapter, 7, status, "body")
		latest, err := repository.GetLatestByChapter(ctx, chapter)
		if err != nil || latest == nil || latest.Status != status {
			t.Fatalf("latest status %q = %+v, %v", status, latest, err)
		}
		finalized, err := repository.GetFinalizedByChapter(ctx, chapter)
		if err != nil || finalized != nil {
			t.Fatalf("non-finalized status %q = %+v, %v", status, finalized, err)
		}
	}
	// A superseded draft can be the latest, and retains its receipt title.
	seed(t, db, 200, 20, 1, "superseded", "superseded")
	mustExec(t, db, "INSERT INTO finalization_outbox VALUES('superseded-receipt',200,20,'retained title',?,?)", textHash("superseded"), "superseded")
	latest, err = repository.GetLatestByChapter(ctx, 20)
	if err != nil || latest == nil || latest.ChapterTitle != "retained title" {
		t.Fatalf("superseded latest = %+v, %v", latest, err)
	}
}

func TestDraftChapterQueriesPreserveOriginalVersionOnlyTies(t *testing.T) {
	db, repository, _ := newFixture(t)
	ctx := context.Background()
	seed(t, db, 20, 7, 4, "finalized", "higher ID")
	seed(t, db, 10, 7, 4, "finalized", "lower ID")
	seed(t, db, 30, 7, 3, "finalized", "lower version")
	// This reproduces the original TS selection, which has NO ID tiebreak.
	// The synthetic table scan picks ID 10; authoritative dependency selection
	// separately treats the equal-version ID 20 as newer. Do not conflate them.
	for name, get := range map[string]func(context.Context, int64) (*DraftMeta, error){
		"latest":    repository.GetLatestByChapter,
		"finalized": repository.GetFinalizedByChapter,
	} {
		t.Run(name, func(t *testing.T) {
			filter := ""
			if name == "finalized" {
				filter = " AND drafts.status = 'finalized'"
			}
			var originalID int64
			err := db.QueryRow(`SELECT drafts.id FROM drafts
			 LEFT JOIN finalization_outbox
			 ON drafts.status IN ('finalized','superseded') AND finalization_outbox.draft_id = drafts.id
			 WHERE drafts.chapter_number = ?`+filter+` ORDER BY drafts.version DESC LIMIT 1`, 7).Scan(&originalID)
			if err != nil {
				t.Fatal(err)
			}
			meta, err := get(ctx, 7)
			if err != nil || meta == nil || meta.ID != originalID || meta.ID != 10 {
				t.Fatalf("version-only tie = %+v, %v; original ID = %d", meta, err, originalID)
			}
		})
	}
}

func TestDraftChapterQueriesMetadataAndDependencyParity(t *testing.T) {
	db, repository, _ := newFixture(t)
	ctx := context.Background()
	seed(t, db, 1, 1, 1, "draft", "root")
	seed(t, db, 2, 2, 1, "revised", "candidate")
	seed(t, db, 3, 3, 2, "finalized", "selected content must not be returned")
	deps(t, db, 2, DraftSourceDependency{DraftID: 1, ContentHash: textHash("root")})
	dependency := DraftSourceDependency{DraftID: 2, ContentHash: textHash("candidate"), Kind: "candidate"}
	deps(t, db, 3, dependency)
	mustExec(t, db, "UPDATE drafts SET source='rewrite',word_count=42 WHERE id=3")
	mustExec(t, db, "INSERT INTO finalization_outbox VALUES('receipt',3,3,' title preserved ',?,?)", textHash("selected content must not be returned"), "selected content must not be returned")
	want := DraftMeta{
		ID: 3, ChapterNumber: 3, ChapterTitle: " title preserved ", Version: 2,
		Status: "finalized", Source: "rewrite", ContentID: 3, WordCount: 42,
		SourceDependencies: []DraftSourceDependency{dependency}, DependenciesStale: false,
		CreatedAt: "created-original", UpdatedAt: "updated-original",
	}
	for name, get := range map[string]func(context.Context, int64) (*DraftMeta, error){
		"latest":    repository.GetLatestByChapter,
		"finalized": repository.GetFinalizedByChapter,
	} {
		t.Run(name, func(t *testing.T) {
			meta, err := get(ctx, 3)
			if err != nil || meta == nil || !reflect.DeepEqual(*meta, want) {
				t.Fatalf("metadata = %+v, %v; want %+v", meta, err, want)
			}
			encoded, err := json.Marshal(meta)
			if err != nil {
				t.Fatal(err)
			}
			var shape map[string]any
			if err := json.Unmarshal(encoded, &shape); err != nil {
				t.Fatal(err)
			}
			wantShape := map[string]any{
				"id": float64(3), "chapterNumber": float64(3), "chapterTitle": " title preserved ",
				"version": float64(2), "status": "finalized", "source": "rewrite",
				"contentId": float64(3), "wordCount": float64(42),
				"sourceDependencies": []any{map[string]any{
					"draftId": float64(2), "contentHash": textHash("candidate"), "kind": "candidate",
				}},
				"dependenciesStale": false, "createdAt": "created-original", "updatedAt": "updated-original",
			}
			if !reflect.DeepEqual(shape, wantShape) {
				t.Fatalf("original DraftMeta JSON shape = %s", encoded)
			}
		})
	}
	// These APIs return metadata even when the selected draft's body is absent.
	mustExec(t, db, "DELETE FROM contents WHERE id=3")
	for name, get := range map[string]func(context.Context, int64) (*DraftMeta, error){
		"latest":    repository.GetLatestByChapter,
		"finalized": repository.GetFinalizedByChapter,
	} {
		meta, err := get(ctx, 3)
		if err != nil || meta == nil || !reflect.DeepEqual(*meta, want) {
			t.Fatalf("%s missing selected body changed metadata = %+v, %v", name, meta, err)
		}
	}
	mustExec(t, db, "UPDATE contents SET body='changed root' WHERE id=1")
	for name, get := range map[string]func(context.Context, int64) (*DraftMeta, error){
		"latest":    repository.GetLatestByChapter,
		"finalized": repository.GetFinalizedByChapter,
	} {
		meta, err := get(ctx, 3)
		if err != nil || meta == nil || !meta.DependenciesStale || !reflect.DeepEqual(meta.SourceDependencies, want.SourceDependencies) {
			t.Fatalf("%s transitive stale = %+v, %v", name, meta, err)
		}
	}
	mustExec(t, db, "UPDATE drafts SET source_dependencies='malformed' WHERE id=3")
	mustExec(t, db, "UPDATE finalization_outbox SET chapter_title=? WHERE draft_id=3", "\ufeff \t")
	for name, get := range map[string]func(context.Context, int64) (*DraftMeta, error){
		"latest":    repository.GetLatestByChapter,
		"finalized": repository.GetFinalizedByChapter,
	} {
		meta, err := get(ctx, 3)
		if err != nil || meta == nil || !meta.DependenciesStale || meta.SourceDependencies == nil || len(meta.SourceDependencies) != 0 || meta.ChapterTitle != "" {
			t.Fatalf("%s malformed metadata = %+v, %v", name, meta, err)
		}
		encoded, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		var shape map[string]any
		if err := json.Unmarshal(encoded, &shape); err != nil {
			t.Fatal(err)
		}
		if _, exists := shape["chapterTitle"]; exists {
			t.Fatalf("blank chapter title must be omitted: %s", encoded)
		}
		if !reflect.DeepEqual(shape["sourceDependencies"], []any{}) {
			t.Fatalf("malformed dependencies must serialize as []: %s", encoded)
		}
	}
}

func TestDraftChapterAggregatesPreserveMaxAndNullDefaults(t *testing.T) {
	ctx := context.Background()
	t.Run("finalized maximum", func(t *testing.T) {
		db, repository, _ := newFixture(t)
		for index, status := range []string{"draft", "revised", "reviewed", "superseded", "unknown-status", "Finalized", "finalized "} {
			id := int64(index + 1)
			seed(t, db, id, id+100, 99, status, "not finalized")
		}
		if maximum, err := repository.GetMaxFinalizedChapter(ctx); err != nil || maximum != 0 {
			t.Fatalf("no exact finalized status maximum = %d, %v", maximum, err)
		}
		seed(t, db, 10, 0, 9, "finalized", "chapter zero")
		if maximum, err := repository.GetMaxFinalizedChapter(ctx); err != nil || maximum != 0 {
			t.Fatalf("zero chapter maximum = %d, %v", maximum, err)
		}
		seed(t, db, 11, 3, 20, "finalized", "lower chapter higher version")
		seed(t, db, 12, 8, 1, "finalized", "higher chapter lower version")
		if maximum, err := repository.GetMaxFinalizedChapter(ctx); err != nil || maximum != 8 {
			t.Fatalf("finalized maximum = %d, %v", maximum, err)
		}
	})
	t.Run("negative values are not clamped", func(t *testing.T) {
		db, repository, _ := newFixture(t)
		seed(t, db, 1, -2, -4, "finalized", "synthetic negative")
		if maximum, err := repository.GetMaxFinalizedChapter(ctx); err != nil || maximum != -2 {
			t.Fatalf("negative maximum = %d, %v", maximum, err)
		}
		if next, err := repository.GetNextVersion(ctx, -2); err != nil || next != -3 {
			t.Fatalf("negative next version = %d, %v", next, err)
		}
	})
	t.Run("next version includes all statuses", func(t *testing.T) {
		db, repository, _ := newFixture(t)
		seed(t, db, 1, 1, 1000, "draft", "other chapter")
		seed(t, db, 2, 2, 0, "draft", "zero version")
		if next, err := repository.GetNextVersion(ctx, 2); err != nil || next != 1 {
			t.Fatalf("zero maximum next version = %d, %v", next, err)
		}
		for index, status := range []string{"draft", "revised", "reviewed", "finalized", "superseded", "unknown-status"} {
			version := int64(index + 3)
			seed(t, db, version+10, 2, version, status, "body")
			if next, err := repository.GetNextVersion(ctx, 2); err != nil || next != version+1 {
				t.Fatalf("status %q next version = %d, %v; want %d", status, next, err, version+1)
			}
		}
		if next, err := repository.GetNextVersion(ctx, 999); err != nil || next != 1 {
			t.Fatalf("absent chapter next version = %d, %v", next, err)
		}
	})
}

func TestDraftChapterQueriesDoNotHideSchemaErrors(t *testing.T) {
	db, repository, _ := newFixture(t)
	mustExec(t, db, "DROP TABLE drafts")
	ctx := context.Background()
	if _, err := repository.GetLatestByChapter(ctx, 1); err == nil {
		t.Fatal("latest query hid missing schema")
	}
	if _, err := repository.GetFinalizedByChapter(ctx, 1); err == nil {
		t.Fatal("finalized query hid missing schema")
	}
	if _, err := repository.GetMaxFinalizedChapter(ctx); err == nil {
		t.Fatal("finalized maximum hid missing schema")
	}
	if _, err := repository.GetNextVersion(ctx, 1); err == nil {
		t.Fatal("next version hid missing schema")
	}
}
