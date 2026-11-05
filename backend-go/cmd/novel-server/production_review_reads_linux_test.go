//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

const (
	// The draft the editor fixture already seeded (production_editor_reads_linux_test.go):
	// chapter 1, version 1, working status and that stored body. The review and
	// revision rows below freeze exactly this source, which is what the full-row
	// reads project back as their sourceDraft.
	productionReviewDraftID      = 1
	productionReviewDraftChapter = 1
	productionReviewDraftVersion = 1
	productionReviewDraftStatus  = "draft"
	productionReviewWordCount    = 7
	productionReviewBody         = "审稿意见：开篇缺少钩子。"
	productionReviewPendingBody  = "修稿建议：加一句钩子。"
	productionReviewDroppedBody  = "修稿建议：已放弃的版本。"
)

// productionReviewRevisionSeed is the identity of the rows the fixture committed
// through the shipped review/revision repository, so every assertion compares
// against what that store actually wrote instead of a hard-coded guess.
type productionReviewRevisionSeed struct {
	ReviewID            int64
	PendingRevisionID   int64
	DiscardedRevisionID int64
}

// seedProductionReviewRevisionRows commits one review, one pending revision and
// one discarded revision on the draft the editor fixture seeded, and returns
// their identities. The fixture is written through store.NewReviewRevisions -
// CreateReview, CreateRevision twice and MarkDiscarded - instead of hand-written
// INSERTs, so the rows the reads below project carry the original schema's own
// review index, revision index and frozen source snapshot. It runs before the lane
// is opened: the admitted bytes the test compares against are read afterwards.
func seedProductionReviewRevisionRows(t *testing.T, projectPath string) productionReviewRevisionSeed {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	// The baseline holds no review rows; clearing the derived tables first keeps
	// the row counts below deterministic instead of depending on that.
	for _, statement := range []string{
		"DELETE FROM review_findings",
		"DELETE FROM review_cycles",
		"DELETE FROM revisions",
		"DELETE FROM reviews",
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	ctx := context.Background()
	repository := store.NewReviewRevisions(db)
	source := map[string]any{
		"id":            int64(productionReviewDraftID),
		"chapterNumber": int64(productionReviewDraftChapter),
		"version":       int64(productionReviewDraftVersion),
		"status":        productionReviewDraftStatus,
		"content":       productionEditorSeed,
	}
	review, err := repository.CreateReview(ctx, map[string]any{
		"baseDraftId":    int64(productionReviewDraftID),
		"content":        productionReviewBody,
		"expectedSource": source,
	})
	if err != nil {
		_ = db.Close()
		t.Fatalf("seed review: %v", err)
	}
	reviewID, ok := review["id"].(int64)
	if !ok || reviewID < 1 {
		_ = db.Close()
		t.Fatalf("seed review returned %v as its identity", review)
	}
	pending, err := repository.CreateRevision(ctx, map[string]any{
		"baseDraftId":    int64(productionReviewDraftID),
		"content":        productionReviewPendingBody,
		"revisionType":   "refine",
		"wordCount":      int64(productionReviewWordCount),
		"expectedSource": source,
	})
	if err != nil {
		_ = db.Close()
		t.Fatalf("seed pending revision: %v", err)
	}
	pendingID, ok := pending["id"].(int64)
	if !ok || pendingID < 1 {
		_ = db.Close()
		t.Fatalf("seed pending revision returned %v as its identity", pending)
	}
	dropped, err := repository.CreateRevision(ctx, map[string]any{
		"baseDraftId":    int64(productionReviewDraftID),
		"content":        productionReviewDroppedBody,
		"revisionType":   "review-fix",
		"wordCount":      int64(productionReviewWordCount),
		"expectedSource": source,
	})
	if err != nil {
		_ = db.Close()
		t.Fatalf("seed discarded revision: %v", err)
	}
	droppedID, ok := dropped["id"].(int64)
	if !ok || droppedID < 1 {
		_ = db.Close()
		t.Fatalf("seed discarded revision returned %v as its identity", dropped)
	}
	if err := repository.MarkDiscarded(ctx, droppedID); err != nil {
		_ = db.Close()
		t.Fatalf("seed discarded revision status: %v", err)
	}
	// The lane admits the file itself, so the seeded rows must be checkpointed
	// into it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
	return productionReviewRevisionSeed{
		ReviewID:            reviewID,
		PendingRevisionID:   pendingID,
		DiscardedRevisionID: droppedID,
	}
}

// TestProductionReviewReadsServeAdmittedAuthorDatabase is the review/revision
// read half of this lane. Each of the six bound operations is asserted by its own
// subtest - never in a loop over the names - so a failure names the operation
// that broke and the annotation is not an empty one. Every read is compared with
// the rows the author database really holds, every read refuses a lease this
// server never issued under the same fence as every other editor read, and none
// of them may touch the admitted bytes.
func TestProductionReviewReadsServeAdmittedAuthorDatabase(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	seed := seedProductionReviewRevisionRows(t, projectPath)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	expected, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	// unchanged re-reads the admitted database: a served read and a refused lease
	// must both leave the author's bytes exactly as they were seeded.
	unchanged := func(t *testing.T, label string) {
		t.Helper()
		after, err := os.ReadFile(database)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, expected) {
			t.Fatalf("%s changed the admitted author database", label)
		}
	}

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "review-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}

	// db:review-list: the seeded review with its own index, content identity and
	// creation time - keys of the metadata projection, never the body.
	t.Run("db:review-list", func(t *testing.T) {
		args := []any{float64(productionReviewDraftID), projectPath, frozen}
		var reviews []map[string]any
		productionEditorDecode(t, author, "db:review-list", args, &reviews)
		if len(reviews) != 1 {
			t.Fatalf("db:review-list returned %d reviews; want the one seeded review: %v", len(reviews), reviews)
		}
		review := reviews[0]
		if review["id"] != float64(seed.ReviewID) ||
			review["baseDraftId"] != float64(productionReviewDraftID) ||
			review["reviewIndex"] != float64(1) {
			t.Fatalf("db:review-list did not read the seeded review: %v", review)
		}
		if contentID, ok := review["contentId"].(float64); !ok || contentID < 1 {
			t.Fatalf("db:review-list lost the review content identity: %v", review)
		}
		if createdAt, ok := review["createdAt"].(string); !ok || createdAt == "" {
			t.Fatalf("db:review-list lost the seeded creation time: %v", review)
		}
		productionEditorRefused(t, author, "stale lease", "db:review-list",
			[]any{float64(productionReviewDraftID), projectPath, stale})
		unchanged(t, "db:review-list")
	})

	// db:revision-list: both revisions of the seeded draft, in revision order,
	// with the status each one really holds.
	t.Run("db:revision-list", func(t *testing.T) {
		args := []any{float64(productionReviewDraftID), projectPath, frozen}
		var revisions []map[string]any
		productionEditorDecode(t, author, "db:revision-list", args, &revisions)
		if len(revisions) != 2 {
			t.Fatalf("db:revision-list returned %d revisions; want the pending and the discarded one: %v",
				len(revisions), revisions)
		}
		pending := revisions[0]
		if pending["id"] != float64(seed.PendingRevisionID) ||
			pending["baseDraftId"] != float64(productionReviewDraftID) ||
			pending["revisionIndex"] != float64(1) ||
			pending["revisionType"] != "refine" ||
			pending["status"] != "pending" {
			t.Fatalf("db:revision-list did not read the pending revision first: %v", pending)
		}
		if pending["wordCount"] != float64(productionReviewWordCount) {
			t.Fatalf("db:revision-list lost the seeded word count: %v", pending)
		}
		dropped := revisions[1]
		if dropped["id"] != float64(seed.DiscardedRevisionID) ||
			dropped["revisionIndex"] != float64(2) ||
			dropped["revisionType"] != "review-fix" ||
			dropped["status"] != "discarded" {
			t.Fatalf("db:revision-list did not read the discarded revision: %v", dropped)
		}
		productionEditorRefused(t, author, "stale lease", "db:revision-list",
			[]any{float64(productionReviewDraftID), projectPath, stale})
		unchanged(t, "db:revision-list")
	})

	// db:revision-get-pending: only the still-pending revision of that draft, so a
	// reader that ignores the pending filter returns the discarded row too.
	t.Run("db:revision-get-pending", func(t *testing.T) {
		args := []any{float64(productionReviewDraftID), projectPath, frozen}
		var pending []map[string]any
		productionEditorDecode(t, author, "db:revision-get-pending", args, &pending)
		if len(pending) != 1 {
			t.Fatalf("db:revision-get-pending returned %d revisions; want only the pending one: %v",
				len(pending), pending)
		}
		revision := pending[0]
		if revision["id"] != float64(seed.PendingRevisionID) || revision["status"] != "pending" ||
			revision["revisionType"] != "refine" {
			t.Fatalf("db:revision-get-pending read the wrong revision: %v", revision)
		}
		// Neither relation was seeded: the projection must keep them absent instead
		// of inventing a merge target or a review binding.
		if revision["mergedToDraftId"] != nil || revision["reviewSourceId"] != nil {
			t.Fatalf("db:revision-get-pending invented a merge or review binding: %v", revision)
		}
		productionEditorRefused(t, author, "stale lease", "db:revision-get-pending",
			[]any{float64(productionReviewDraftID), projectPath, stale})
		unchanged(t, "db:revision-get-pending")
	})

	// db:review-get-full: the seeded review plus the frozen source draft it was
	// written against, joined to its stored body.
	t.Run("db:review-get-full", func(t *testing.T) {
		args := []any{float64(seed.ReviewID), projectPath, frozen}
		var full map[string]any
		productionEditorDecode(t, author, "db:review-get-full", args, &full)
		if full["id"] != float64(seed.ReviewID) ||
			full["baseDraftId"] != float64(productionReviewDraftID) ||
			full["reviewIndex"] != float64(1) ||
			full["content"] != productionReviewBody {
			t.Fatalf("db:review-get-full did not read the seeded review: %v", full)
		}
		source, ok := full["sourceDraft"].(map[string]any)
		if !ok || source == nil {
			t.Fatalf("db:review-get-full lost the frozen source draft: %v", full)
		}
		if source["id"] != float64(productionReviewDraftID) ||
			source["chapterNumber"] != float64(productionReviewDraftChapter) ||
			source["version"] != float64(productionReviewDraftVersion) ||
			source["status"] != productionReviewDraftStatus ||
			source["content"] != productionEditorSeed {
			t.Fatalf("db:review-get-full read the wrong frozen source: %v", source)
		}
		// The raw source columns are folded into sourceDraft, exactly like the
		// original DTO; leaking them would change the browser contract shape.
		for _, key := range []string{"sourceChapter", "sourceVersion", "sourceStatus", "sourceContent"} {
			if _, present := full[key]; present {
				t.Fatalf("db:review-get-full leaked the raw %s column: %v", key, full)
			}
		}
		productionEditorRefused(t, author, "stale lease", "db:review-get-full",
			[]any{float64(seed.ReviewID), projectPath, stale})
		unchanged(t, "db:review-get-full")
	})

	// db:revision-get-full: the pending revision plus the same frozen source.
	t.Run("db:revision-get-full", func(t *testing.T) {
		args := []any{float64(seed.PendingRevisionID), projectPath, frozen}
		var full map[string]any
		productionEditorDecode(t, author, "db:revision-get-full", args, &full)
		if full["id"] != float64(seed.PendingRevisionID) ||
			full["baseDraftId"] != float64(productionReviewDraftID) ||
			full["revisionType"] != "refine" ||
			full["status"] != "pending" ||
			full["content"] != productionReviewPendingBody {
			t.Fatalf("db:revision-get-full did not read the seeded revision: %v", full)
		}
		if full["wordCount"] != float64(productionReviewWordCount) {
			t.Fatalf("db:revision-get-full lost the seeded word count: %v", full)
		}
		source, ok := full["sourceDraft"].(map[string]any)
		if !ok || source == nil {
			t.Fatalf("db:revision-get-full lost the frozen source draft: %v", full)
		}
		if source["id"] != float64(productionReviewDraftID) ||
			source["version"] != float64(productionReviewDraftVersion) ||
			source["content"] != productionEditorSeed {
			t.Fatalf("db:revision-get-full read the wrong frozen source: %v", source)
		}
		productionEditorRefused(t, author, "stale lease", "db:revision-get-full",
			[]any{float64(seed.PendingRevisionID), projectPath, stale})
		unchanged(t, "db:revision-get-full")
	})

	// db:review-cycle-get: this lane serves the original's nullable result for a
	// review that has no saved cycle. The fixture deliberately commits no
	// review_cycles row, so the exact answer is null - and it is asserted with
	// productionEditorCall, which fails on 501, because a silently unbound name
	// would otherwise look like a successful refusal.
	t.Run("db:review-cycle-get", func(t *testing.T) {
		raw := productionEditorCall(t, author, "db:review-cycle-get",
			[]any{float64(seed.ReviewID), projectPath, frozen})
		if string(raw) != "null" {
			t.Fatalf("db:review-cycle-get returned %s; want the exact nullable null for a review with no saved cycle", raw)
		}
		productionEditorRefused(t, author, "stale lease", "db:review-cycle-get",
			[]any{float64(seed.ReviewID), projectPath, stale})
		unchanged(t, "db:review-cycle-get")
	})
}
