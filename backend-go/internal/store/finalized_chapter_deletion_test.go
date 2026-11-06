package store

// These tests provision a real SQLite database from the pinned asset in
// internal/admission (WriteBaselineV7, baseline_v7_ddl.go:744) and drive the
// outbox state machine of finalized_chapter_deletion.go against the real
// chapter_deletion_operations schema (baseline_v7_ddl.go:74-94), the finalized
// facts it deletes (drafts, contents, finalization_outbox, post_process_runs and
// their cascading steps) and the continuity projection it invalidates.
//
// Every deletion is proved by a separate mode=ro connection that could not have
// performed it: the chapter row, its contents, its receipt and its frozen runs
// are counted there, because the writer might otherwise be answering from its
// own uncommitted state. Every refusal is proved by the file's own bytes, which
// a refused call must leave untouched.
//
// The two halves the reference splits across the service are kept apart on
// purpose: this layer decides and records (Begin, StartAttempt, MarkProjection,
// Settle, ConfirmLegacyKnowledgeAbsent), while the manuscript file and the
// knowledge-base document are the adapter's cleaner - so a projection here is
// marked the way the cleaner would report it
// (chapter-deletion-service.ts:161-190).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

// fcdFixture provisions the pinned v7 schema in a fresh file, seeds the
// continuity row the invalidation writes to, then opens the file the way a
// project database is opened (ExistingDSN: mode=rw, foreign keys on). The
// operation id the state machine mints is the caller's, so an assertion can name
// it.
func fcdFixture(t *testing.T, operationID string) (*sql.DB, *ChapterDeletions, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "chapter deletion #? 中文.db")
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=rwc&_busy_timeout=5000&_foreign_keys=on"
	seed, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.WriteBaselineV7(path, seed); err != nil {
		_ = seed.Close()
		t.Fatalf("provision baseline v7: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	dsn, err := ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Four connections: each write reserves one while the assertions below read
	// through the pool.
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	// The baseline creates continuity_projection_meta empty; the deletion's
	// first statement is an UPDATE of the 'main' row, so it has to exist.
	fcdExec(t, db, "INSERT INTO continuity_projection_meta(id, generation, stale_from_chapter) VALUES('main', 3, NULL)")
	return db, NewChapterDeletions(db, func() string { return operationID }), path
}

// fcdReadOnly opens the same file a second time, read-only, so a write is proved
// by a connection that could not have performed it.
func fcdReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=ro&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("read-only connection: %v", err)
	}
	return db
}

func fcdExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func fcdCount(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

func fcdInt(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var value int64
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return value
}

// fcdDigest is the whole file's bytes, so a refusal that wrote anything at all
// fails even if it rolled the logical rows back.
func fcdDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// fcdRefusal runs one refused call and proves both its exact code and that the
// database is byte-identical afterwards. The error is printed with %v: a CI
// annotation is truncated at the first quote, so %q on an error would hide the
// half that matters.
func fcdRefusal(t *testing.T, path string, want error, call func() error) {
	t.Helper()
	before := fcdDigest(t, path)
	err := call()
	if err == nil {
		t.Fatalf("%v: expected a refusal, got none", want)
	}
	if !errors.Is(err, want) || err.Error() != want.Error() {
		t.Fatalf("%v: got %v", want, err)
	}
	if after := fcdDigest(t, path); after != before {
		t.Fatalf("%v: the refusal changed the database", want)
	}
}

// fcdSeedFinalized writes one finalized chapter exactly as the reference's
// fixture does: a content row, a finalized draft, its finalization receipt, and
// the two post-process runs listPostProcessRunIds looks for - one keyed by the
// chapter number and one keyed by the receipt (chapter-deletion-repository.ts:
// 88-93) - each carrying a kb_import step.
func fcdSeedFinalized(t *testing.T, db *sql.DB, draftID, chapterNumber int64, knowledgeDocumentID string) string {
	t.Helper()
	contentID := draftID * 10
	finalizationID := fmt.Sprintf("finalization-%d", draftID)
	body := fmt.Sprintf("第%d章的正文。", chapterNumber)
	fcdExec(t, db, "INSERT INTO contents(id, body) VALUES(?, ?)", contentID, body)
	fcdExec(t, db, `INSERT INTO drafts(id, chapter_number, version, status, source, content_id, word_count)
 VALUES(?, ?, 1, 'finalized', 'write', ?, 8)`, draftID, chapterNumber, contentID)
	fcdExec(t, db, `INSERT INTO finalization_outbox(
  finalization_id, draft_id, chapter_number, chapter_title, content_hash, content_revision,
  content_snapshot, target_file_name, knowledge_document_id)
 VALUES(?, ?, ?, ?, ?, 1, ?, ?, ?)`,
		finalizationID, draftID, chapterNumber, fmt.Sprintf("第%d章", chapterNumber),
		fmt.Sprintf("hash-%d", draftID), body, fmt.Sprintf("第%d章 标题.txt", chapterNumber), knowledgeDocumentID)
	for _, sourceID := range []string{strconv.FormatInt(chapterNumber, 10), "finalization:" + finalizationID} {
		runID := "run-" + sourceID
		fcdExec(t, db, "INSERT INTO post_process_runs(id, trigger_source_type, trigger_source_id) VALUES(?, 'chapter_finalize', ?)", runID, sourceID)
		fcdExec(t, db, "INSERT INTO post_process_steps(run_id, step_key, label, critical) VALUES(?, 'kb_import', '知识', 1)", runID)
	}
	return finalizationID
}

// fcdBegin starts one deletion the way delete-finalized does: the service's
// guard first (chapter-deletion-service.ts:84-90), then begin.
func fcdBegin(t *testing.T, deletions *ChapterDeletions, draftID, chapterNumber int64) *ChapterDeletionOperation {
	t.Helper()
	ctx := context.Background()
	required, err := deletions.RequiresLegacyKnowledgeAuthorization(ctx, draftID, chapterNumber)
	if err != nil {
		t.Fatalf("legacy guard read: %v", err)
	}
	operation, err := deletions.Begin(ctx, ChapterDeletionBegin{DraftID: draftID, ChapterNumber: chapterNumber,
		LegacyKnowledgeAuthorizationRequired: required})
	if err != nil {
		t.Fatalf("begin deletion: %v", err)
	}
	return operation
}

// fcdClean is the adapter's cleaner half (chapter-deletion-service.ts:146-151)
// for the projections a caller names: the attempt, one verdict per projection,
// and the settle that aggregates them.
func fcdClean(t *testing.T, deletions *ChapterDeletions, operationID string, projections ...ChapterDeletionProjection) *ChapterDeletionOperation {
	t.Helper()
	ctx := context.Background()
	operation, started, err := deletions.StartAttempt(ctx, operationID)
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	if !started {
		t.Fatalf("start attempt did not start %v", operationID)
	}
	for _, projection := range projections {
		if _, err := deletions.MarkProjection(ctx, operationID, projection, ChapterDeletionProjectionCompleted, ""); err != nil {
			t.Fatalf("mark %v: %v", projection, err)
		}
	}
	operation, err = deletions.Settle(ctx, operationID)
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	return operation
}

// TestChapterDeletionListOnEmptyProject is list-incomplete-deletions before any
// deletion exists: the reference answers [] (:380-387), never null, so the
// result is an empty non-nil slice and JSON encodes it as [].
func TestChapterDeletionListOnEmptyProject(t *testing.T) {
	db, deletions, _ := fcdFixture(t, "delete-empty")
	if operations, err := deletions.ListIncomplete(context.Background()); err != nil {
		t.Fatalf("list incomplete: %v", err)
	} else if operations == nil {
		t.Fatal("list incomplete answered nil, want an empty list")
	} else if len(operations) != 0 {
		t.Fatalf("list incomplete answered %d operations, want 0", len(operations))
	}
	if count := fcdCount(t, db, "SELECT COUNT(*) FROM chapter_deletion_operations"); count != 0 {
		t.Fatalf("chapter_deletion_operations holds %d rows, want 0", count)
	}
}

// TestChapterDeletionRemovesFinalizedChapter is one delete-finalized that runs
// to completion: the receipt is inserted, the finalized fact and its frozen runs
// are deleted in the same transaction, every projection is cleaned, and the
// settled operation is 'completed'.
func TestChapterDeletionRemovesFinalizedChapter(t *testing.T) {
	db, deletions, path := fcdFixture(t, "delete-1")
	finalizationID := fcdSeedFinalized(t, db, 1, 1, "kb-1")
	// A revision of the deleted draft owns a second content row; deleteDraftFacts
	// collects it through revisions.base_draft_id (:182-188).
	fcdExec(t, db, "INSERT INTO contents(id, body) VALUES(11, '第一章的修订稿。')")
	fcdExec(t, db, `INSERT INTO revisions(base_draft_id, revision_index, revision_type, content_id, word_count)
 VALUES(1, 1, 'refine', 11, 6)`)
	// Chapter 3 and an unrelated receipt must survive every statement below.
	fcdSeedFinalized(t, db, 7, 3, "kb-3")
	fcdExec(t, db, "INSERT INTO post_process_runs(id, trigger_source_type, trigger_source_id) VALUES('run-finalization:other', 'chapter_finalize', 'finalization:other')")
	ctx := context.Background()

	created := fcdBegin(t, deletions, 1, 1)
	if created.OperationID != "delete-1" {
		t.Fatalf("operation id is %v, want delete-1", created.OperationID)
	}
	if created.DraftID != 1 || created.ChapterNumber != 1 {
		t.Fatalf("operation names draft %d chapter %d, want 1/1", created.DraftID, created.ChapterNumber)
	}
	if created.Status != ChapterDeletionPending {
		t.Fatalf("status is %v, want pending", created.Status)
	}
	if created.LegacyKnowledgeAuthorization != ChapterDeletionLegacyKnowledgeNotRequired {
		t.Fatalf("legacy authorization is %v, want not_required", created.LegacyKnowledgeAuthorization)
	}
	if created.ManuscriptStatus != ChapterDeletionProjectionPending || created.KnowledgeStatus != ChapterDeletionProjectionPending {
		t.Fatalf("projections are %v/%v, want pending/pending", created.ManuscriptStatus, created.KnowledgeStatus)
	}
	if created.FinalizationID != finalizationID {
		t.Fatalf("finalization id is %v, want %v", created.FinalizationID, finalizationID)
	}
	if created.KnowledgeDocumentID != "kb-1" || created.TargetFileName != "第1章 标题.txt" {
		t.Fatalf("projection names are %v/%v", created.KnowledgeDocumentID, created.TargetFileName)
	}
	if len(created.PostProcessRunIDs) != 2 || created.PostProcessRunIDs[0] != "run-1" ||
		created.PostProcessRunIDs[1] != "run-finalization:"+finalizationID {
		t.Fatalf("frozen run ids are %v", created.PostProcessRunIDs)
	}
	if created.AttemptCount != 0 {
		t.Fatalf("attempt count is %d, want 0", created.AttemptCount)
	}

	// The fact is already gone: begin deletes it in its own transaction (:304).
	// The read-only connection could not have written it.
	readOnly := fcdReadOnly(t, path)
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM drafts WHERE id = 1"); count != 0 {
		t.Fatalf("the chapter row survived the deletion: %d rows, want 0", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM finalization_outbox WHERE finalization_id = ?", finalizationID); count != 0 {
		t.Fatalf("the finalization receipt survived the deletion: %d rows, want 0", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM post_process_runs WHERE id IN ('run-1', ?)", "run-finalization:"+finalizationID); count != 0 {
		t.Fatalf("the frozen runs survived the deletion: %d rows, want 0", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM contents WHERE id IN (10, 11)"); count != 0 {
		t.Fatalf("the draft and revision contents survived the deletion: %d rows, want 0", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM drafts WHERE id = 7"); count != 1 {
		t.Fatalf("chapter 3 draft count is %d, want 1", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM post_process_runs WHERE id = ?", "run-finalization:finalization-7"); count != 1 {
		t.Fatalf("chapter 3 runs were removed: %d left, want 1", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM post_process_runs WHERE id = 'run-finalization:other'"); count != 1 {
		t.Fatalf("unrelated run count is %d, want 1", count)
	}
	if generation := fcdInt(t, readOnly, "SELECT generation FROM continuity_projection_meta WHERE id = 'main'"); generation != 4 {
		t.Fatalf("continuity generation is %d, want 4", generation)
	}
	if stale := fcdInt(t, readOnly, "SELECT stale_from_chapter FROM continuity_projection_meta WHERE id = 'main'"); stale != 1 {
		t.Fatalf("stale_from_chapter is %d, want 1", stale)
	}

	// The cleaner removes the manuscript, then the knowledge document; each
	// verdict lands in the same transaction as the aggregate refresh.
	operation, started, err := deletions.StartAttempt(ctx, created.OperationID)
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	if !started || operation.AttemptCount != 1 || operation.Status != ChapterDeletionPending {
		t.Fatalf("attempt is %v/%d/%v, want started/1/pending", started, operation.AttemptCount, operation.Status)
	}
	if operation, err = deletions.MarkProjection(ctx, created.OperationID, ChapterDeletionManuscript, ChapterDeletionProjectionCompleted, ""); err != nil {
		t.Fatalf("mark manuscript: %v", err)
	}
	if operation.Status != ChapterDeletionPending {
		t.Fatalf("status after the manuscript is %v, want pending", operation.Status)
	}
	if operation, err = deletions.MarkProjection(ctx, created.OperationID, ChapterDeletionKnowledge, ChapterDeletionProjectionCompleted, ""); err != nil {
		t.Fatalf("mark knowledge: %v", err)
	}
	if operation.Status != ChapterDeletionCompleted || operation.CompletedAt == "" {
		t.Fatalf("status after both projections is %v (%v), want completed", operation.Status, operation.CompletedAt)
	}
	if settled, err := deletions.Settle(ctx, created.OperationID); err != nil {
		t.Fatalf("settle: %v", err)
	} else if settled.Status != ChapterDeletionCompleted || settled.AttemptCount != 1 {
		t.Fatalf("settled operation is %v/%d, want completed/1", settled.Status, settled.AttemptCount)
	}
	if refusal := operation.Refusal(); refusal != nil {
		t.Fatalf("a completed deletion refuses with %v", refusal)
	}
	if operations, err := deletions.ListIncomplete(ctx); err != nil {
		t.Fatalf("list incomplete: %v", err)
	} else if len(operations) != 0 {
		t.Fatalf("incomplete list holds %d operations, want 0", len(operations))
	}
}

// TestChapterDeletionLeavesFailedAttemptListed is a delete whose knowledge
// document could not be removed: the finalized fact is already gone, the
// operation stays 'failed' with the cleaner's message, and
// list-incomplete-deletions answers it.
func TestChapterDeletionLeavesFailedAttemptListed(t *testing.T) {
	db, deletions, _ := fcdFixture(t, "delete-failed")
	fcdSeedFinalized(t, db, 2, 2, "kb-2")
	ctx := context.Background()

	created := fcdBegin(t, deletions, 2, 2)
	if _, started, err := deletions.StartAttempt(ctx, created.OperationID); err != nil {
		t.Fatalf("start attempt: %v", err)
	} else if !started {
		t.Fatal("start attempt did not start the deletion")
	}
	if _, err := deletions.MarkProjection(ctx, created.OperationID, ChapterDeletionManuscript, ChapterDeletionProjectionCompleted, ""); err != nil {
		t.Fatalf("mark manuscript: %v", err)
	}
	operation, err := deletions.MarkProjection(ctx, created.OperationID, ChapterDeletionKnowledge,
		ChapterDeletionProjectionFailed, "KB_LOCKED")
	if err != nil {
		t.Fatalf("mark knowledge: %v", err)
	}
	if operation.Status != ChapterDeletionFailed {
		t.Fatalf("status is %v, want failed", operation.Status)
	}
	if operation.KnowledgeStatus != ChapterDeletionProjectionFailed || operation.KnowledgeError != "KB_LOCKED" {
		t.Fatalf("knowledge projection is %v (%v), want failed/KB_LOCKED", operation.KnowledgeStatus, operation.KnowledgeError)
	}
	listed, err := deletions.ListIncomplete(ctx)
	if err != nil {
		t.Fatalf("list incomplete: %v", err)
	}
	if len(listed) != 1 || listed[0].OperationID != "delete-failed" || listed[0].Status != ChapterDeletionFailed {
		t.Fatalf("incomplete list is %v, want the failed deletion", listed)
	}
	if refusal := listed[0].Refusal(); !errors.Is(refusal, ErrChapterDeletionCleanupIncomplete) {
		t.Fatalf("failed deletion refuses with %v", refusal)
	}
	// begin is the write that removed the chapter, so the fact is gone even
	// though the cleanup is not finished.
	if count := fcdCount(t, db, "SELECT COUNT(*) FROM drafts WHERE id = 2"); count != 0 {
		t.Fatalf("draft count is %d, want 0", count)
	}
}

// TestChapterDeletionRetryCompletesFailedAttempt is the retry half: startAttempt
// re-pends the failed row, counts the second attempt, the two projections are
// cleaned again, and the settle completes it.
func TestChapterDeletionRetryCompletesFailedAttempt(t *testing.T) {
	db, deletions, _ := fcdFixture(t, "delete-retry")
	fcdSeedFinalized(t, db, 3, 3, "kb-3")
	ctx := context.Background()

	created := fcdBegin(t, deletions, 3, 3)
	if _, started, err := deletions.StartAttempt(ctx, created.OperationID); err != nil {
		t.Fatalf("start attempt: %v", err)
	} else if !started {
		t.Fatal("first attempt did not start")
	}
	if _, err := deletions.MarkProjection(ctx, created.OperationID, ChapterDeletionManuscript,
		ChapterDeletionProjectionFailed, "simulated manuscript cleanup outage"); err != nil {
		t.Fatalf("mark manuscript: %v", err)
	}
	if count := len(fcdMustList(t, deletions)); count != 1 {
		t.Fatalf("incomplete list holds %d operations, want 1", count)
	}

	// retry-deletion (:400-409 for the write, :161-190 for the cleaning).
	retried := fcdClean(t, deletions, created.OperationID, ChapterDeletionManuscript, ChapterDeletionKnowledge)
	if retried.Status != ChapterDeletionCompleted {
		t.Fatalf("retried status is %v, want completed", retried.Status)
	}
	if retried.AttemptCount != 2 {
		t.Fatalf("attempt count is %d, want 2", retried.AttemptCount)
	}
	if retried.ManuscriptError != "" {
		t.Fatalf("the stale manuscript error survived the retry: %v", retried.ManuscriptError)
	}
	if count := len(fcdMustList(t, deletions)); count != 0 {
		t.Fatalf("incomplete list holds %d operations after the retry, want 0", count)
	}
}

// TestChapterDeletionRetryOfCompletedWritesNothing is a retry of a deletion that
// already completed. The service answers a completed receipt without writing
// (chapter-deletion-service.ts:142-144) and never reaches startAttempt, so this
// transaction answers the same row, leaves attempt_count alone, and leaves the
// file byte-identical - a finished deletion cannot be re-opened by a retry.
func TestChapterDeletionRetryOfCompletedWritesNothing(t *testing.T) {
	db, deletions, path := fcdFixture(t, "delete-done")
	fcdSeedFinalized(t, db, 4, 4, "kb-4")
	ctx := context.Background()

	created := fcdBegin(t, deletions, 4, 4)
	completed := fcdClean(t, deletions, created.OperationID, ChapterDeletionManuscript, ChapterDeletionKnowledge)
	if completed.Status != ChapterDeletionCompleted || completed.AttemptCount != 1 {
		t.Fatalf("completed operation is %v/%d, want completed/1", completed.Status, completed.AttemptCount)
	}
	before := fcdDigest(t, path)

	started := true
	var operation *ChapterDeletionOperation
	err := func() error {
		var attemptErr error
		operation, started, attemptErr = deletions.StartAttempt(ctx, created.OperationID)
		return attemptErr
	}()
	if err != nil {
		t.Fatalf("retry of a completed deletion: %v", err)
	}
	if started {
		t.Fatal("a completed deletion was re-opened by a retry")
	}
	if operation.Status != ChapterDeletionCompleted || operation.AttemptCount != 1 {
		t.Fatalf("retried operation is %v/%d, want completed/1", operation.Status, operation.AttemptCount)
	}
	if after := fcdDigest(t, path); after != before {
		t.Fatal("the retry of a completed deletion changed the database")
	}
	if refusal := operation.Refusal(); refusal != nil {
		t.Fatalf("a completed deletion refuses with %v", refusal)
	}
	if count := fcdCount(t, db, "SELECT COUNT(*) FROM chapter_deletion_operations"); count != 1 {
		t.Fatalf("chapter_deletion_operations holds %d rows, want 1", count)
	}
}

// TestChapterDeletionLegacyKnowledgeGuard is the legacy guard end to end: a
// finalization with no knowledge document whose run did project knowledge
// requires the author's confirmation, begin stops at 'authorization_required'
// and deletes nothing, an attempt is refused, and only
// confirm-legacy-knowledge-absent consumes the authorization and deletes the
// fact - once.
func TestChapterDeletionLegacyKnowledgeGuard(t *testing.T) {
	db, deletions, path := fcdFixture(t, "delete-legacy")
	// An empty knowledge_document_id with a kb_import step is the legacy shape
	// chapter-deletion-service.ts:97-104 detects.
	fcdSeedFinalized(t, db, 5, 5, "")
	// A finalization that already names its document needs no authorization.
	fcdSeedFinalized(t, db, 6, 6, "kb-6")
	ctx := context.Background()

	required, err := deletions.RequiresLegacyKnowledgeAuthorization(ctx, 5, 5)
	if err != nil {
		t.Fatalf("legacy guard read: %v", err)
	}
	if !required {
		t.Fatal("the legacy guard did not fire for a chapter with no knowledge document")
	}
	if required, err = deletions.RequiresLegacyKnowledgeAuthorization(ctx, 6, 6); err != nil {
		t.Fatalf("legacy guard read: %v", err)
	} else if required {
		t.Fatal("the legacy guard fired for a chapter that names its knowledge document")
	}

	blocked := fcdBegin(t, deletions, 5, 5)
	if blocked.Status != ChapterDeletionAuthorizationRequired {
		t.Fatalf("status is %v, want authorization_required", blocked.Status)
	}
	if blocked.LegacyKnowledgeAuthorization != ChapterDeletionLegacyKnowledgeRequired {
		t.Fatalf("legacy authorization is %v, want required", blocked.LegacyKnowledgeAuthorization)
	}
	if blocked.KnowledgeStatus != ChapterDeletionProjectionPending {
		t.Fatalf("knowledge projection is %v, want pending", blocked.KnowledgeStatus)
	}
	if refusal := blocked.Refusal(); !errors.Is(refusal, ErrChapterDeletionLegacyKnowledgeCheckRequired) {
		t.Fatalf("blocked deletion refuses with %v", refusal)
	}
	// Nothing was deleted while the authorization is outstanding.
	readOnly := fcdReadOnly(t, path)
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM drafts WHERE id = 5"); count != 1 {
		t.Fatalf("the blocked deletion removed its chapter: %d rows left, want 1", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM finalization_outbox WHERE draft_id = 5"); count != 1 {
		t.Fatalf("the blocked deletion removed its receipt: %d rows left, want 1", count)
	}
	if operations := fcdMustList(t, deletions); len(operations) != 1 ||
		operations[0].Status != ChapterDeletionAuthorizationRequired {
		t.Fatalf("incomplete list is %v, want the blocked deletion", operations)
	}
	// startAttempt's guard is the reference's (:404-405): a row waiting for the
	// author is not an attempt anyone can start.
	fcdRefusal(t, path, ErrChapterDeletionOperationNotFound, func() error {
		_, _, err := deletions.StartAttempt(ctx, blocked.OperationID)
		return err
	})

	confirmed, err := deletions.ConfirmLegacyKnowledgeAbsent(ctx, blocked.OperationID)
	if err != nil {
		t.Fatalf("confirm legacy knowledge absent: %v", err)
	}
	if confirmed.LegacyKnowledgeAuthorization != ChapterDeletionLegacyKnowledgeConsumed {
		t.Fatalf("legacy authorization is %v, want consumed", confirmed.LegacyKnowledgeAuthorization)
	}
	if confirmed.LegacyKnowledgeAuthorizedAt == "" {
		t.Fatal("the confirmation stamped no legacy_knowledge_authorized_at")
	}
	if confirmed.KnowledgeStatus != ChapterDeletionProjectionNotRequired {
		t.Fatalf("knowledge projection is %v, want not_required", confirmed.KnowledgeStatus)
	}
	if confirmed.Status != ChapterDeletionPending {
		t.Fatalf("status is %v, want pending", confirmed.Status)
	}
	// The confirmation is what deleted the fact (:358).
	readOnly = fcdReadOnly(t, path)
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM drafts WHERE id = 5"); count != 0 {
		t.Fatalf("the confirmed deletion left %d chapter rows, want 0", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM finalization_outbox WHERE draft_id = 5"); count != 0 {
		t.Fatalf("the confirmed deletion left %d receipts, want 0", count)
	}
	// Chapter 6 was untouched by the guard's transaction.
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM drafts WHERE id = 6"); count != 1 {
		t.Fatalf("chapter 6 draft count is %d, want 1", count)
	}
	// A second confirmation has nothing left to consume (:321-326).
	fcdRefusal(t, path, ErrChapterDeletionConfirmationConsumed, func() error {
		_, err := deletions.ConfirmLegacyKnowledgeAbsent(ctx, blocked.OperationID)
		return err
	})
}

// TestChapterDeletionWithoutLiveLease proves this layer needs no lease. The
// reference's lease is the project session the controller asserts before any of
// these calls (chapter-lifecycle-controller.ts:26-30, PROJECT_SESSION_STALE), and
// no session, lease or owner row exists in the schema; the store therefore
// performs a delete and a retry on a database that holds none, instead of
// fabricating a lease requirement of its own.
func TestChapterDeletionWithoutLiveLease(t *testing.T) {
	db, deletions, path := fcdFixture(t, "delete-no-lease")
	fcdExec(t, db, "INSERT INTO post_process_runs(id, trigger_source_type, trigger_source_id) VALUES('run-finalization:other', 'chapter_finalize', 'finalization:other')")
	fcdSeedFinalized(t, db, 8, 8, "kb-8")
	ctx := context.Background()

	created := fcdBegin(t, deletions, 8, 8)
	if created.Status != ChapterDeletionPending {
		t.Fatalf("status is %v, want pending", created.Status)
	}
	if _, started, err := deletions.StartAttempt(ctx, created.OperationID); err != nil {
		t.Fatalf("start attempt: %v", err)
	} else if !started {
		t.Fatal("the attempt did not start without a lease")
	}
	if _, err := deletions.MarkProjection(ctx, created.OperationID, ChapterDeletionManuscript,
		ChapterDeletionProjectionFailed, "simulated outage"); err != nil {
		t.Fatalf("mark manuscript: %v", err)
	}
	retried := fcdClean(t, deletions, created.OperationID, ChapterDeletionManuscript, ChapterDeletionKnowledge)
	if retried.Status != ChapterDeletionCompleted || retried.AttemptCount != 2 {
		t.Fatalf("retried operation is %v/%d, want completed/2", retried.Status, retried.AttemptCount)
	}
	// The deletion did happen: the chapter and its receipt are gone from a
	// connection that could not have removed them.
	readOnly := fcdReadOnly(t, path)
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM drafts WHERE id = 8"); count != 0 {
		t.Fatalf("draft count is %d, want 0", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM finalization_outbox WHERE draft_id = 8"); count != 0 {
		t.Fatalf("receipt count is %d, want 0", count)
	}
	if count := fcdCount(t, readOnly, "SELECT COUNT(*) FROM post_process_runs WHERE id = 'run-finalization:other'"); count != 1 {
		t.Fatalf("unrelated run count is %d, want 1", count)
	}
}

// fcdMustList is list-incomplete-deletions as the sidebar calls it, with the
// error promoted so an assertion stays one expression.
func fcdMustList(t *testing.T, deletions *ChapterDeletions) []ChapterDeletionOperation {
	t.Helper()
	operations, err := deletions.ListIncomplete(context.Background())
	if err != nil {
		t.Fatalf("list incomplete: %v", err)
	}
	return operations
}
