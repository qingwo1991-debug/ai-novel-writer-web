//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file drives the four chapter:* deletion names that
// production_chapter_deletion_linux.go binds, over the real outbox store: the
// list on a project with nothing to delete, a delete that completes, a delete the
// lane honestly leaves pending, the retry that settles it, the retry that only
// replays a completed one, the delete the legacy guard stops, the confirmation
// that is consumed exactly once, the refusal of every operation without a live
// lease, and the portable-runtime freeze guard itself.
//
// # The fixture
//
// Every subtest opens its own production lane (productionGenerationWriteOpen,
// production_generation_write_linux_test.go:129) and seeds one finalized chapter
// through productionChapterDeletionSeed: a draft of chapter 3, the content it
// points at, the immutable finalization receipt beside it and, when the arm needs
// one, the chapter_finalize post-process run the legacy guard reads. Chapter 3 and
// draft 43 are deliberate: productionEditorReadsRoot seeds draft 1 of chapter 1
// (production_editor_reads_linux_test.go:124), so no assertion here can be
// satisfied by a row a sibling fixture owns.
//
// continuity_projection_meta is seeded with an INSERT and verified by reading the
// generation back, never with a bare UPDATE: the pinned baseline creates the table
// empty, so an UPDATE alone is a silent no-op and the assertion below would then
// be checking nothing at all
// (production_character_state_candidates_linux_test.go:39-42).
//
// # What every subtest proves
//
// A served write is read back over a connection of the test's own
// (productionStoryStateWriteReadback, mode=ro), never from the answer the lane
// just produced, so what is asserted is the committed author database plus the
// project directory. A refusal that writes nothing - a transport refusal, a frozen
// operation, a consumed confirmation, a replay of a completed deletion - is proved
// to leave the database FILE byte for byte identical, not merely to answer a code.
// One arm cannot make that claim and says so where it happens: the legacy-guard
// delete commits the operation row it is about to ask the author about
// (chapter-deletion-service.ts:68-97), so there the invariant proved is that the
// fact, the receipt, the run and the manuscript all survive and the delivered list
// carries the waiting operation.
//
// Nothing here mints an identifier the server produced, weakens a refusal, or
// asserts a code the store does not itself name. The one code this file expects
// that the store never raises is PORTABLE_RUNTIME_FREEZE_INVALID, and it is proved
// against the codec rather than guessed.
const (
	// The one chapter every fixture deletes.
	productionChapterDeletionChapter    = 3
	productionChapterDeletionDraft      = 43
	productionChapterDeletionGeneration = 3
	productionChapterDeletionTitle      = "第3章 门"
	productionChapterDeletionSeedID     = "ci-chapter-deletion-3"
	productionChapterDeletionBody       = "门开了，风从走廊尽头灌进来。"

	// The published manuscript's frozen name: a direct child of the project root,
	// which is the only shape removePublishedManuscript accepts
	// (electron/services/manuscript-publisher.ts:88-94).
	productionChapterDeletionTargetFile = "第3章 门.txt"

	// The managed document an interrupted delete leaves pending: this lane owns no
	// knowledge-base writer, so the operation must stay in the incomplete list.
	productionChapterDeletionKnowledgeID = "kb:ci-chapter-3"
	// productionChapterDeletionRunID is the chapter_finalize run the legacy guard
	// reads. Its steps decide the guard: kb_import raises it, no step clears it
	// (internal/store/finalized_chapter_deletion.go:1223-1270).
	productionChapterDeletionRunID = "ci-chapter-deletion-run-3"
	// productionChapterDeletionPendingID is the id the interrupted delete in the
	// pending fixture minted; the wire never sees it before that seed runs.
	productionChapterDeletionPendingID = "ci-chapter-deletion-pending-3"
)

// productionChapterDeletionFieldNames are the 19 fields of
// ChapterDeletionOperation (src/shared/chapter-deletion.ts:10-30) in the
// reference's own order.
var productionChapterDeletionFieldNames = [...]string{
	"operationId", "draftId", "chapterNumber", "chapterTitle", "finalizationId",
	"targetFileName", "knowledgeDocumentId", "postProcessRunIds", "manuscriptStatus",
	"manuscriptError", "knowledgeStatus", "knowledgeError", "legacyKnowledgeAuthorization",
	"legacyKnowledgeAuthorizedAt", "status", "attemptCount", "createdAt", "updatedAt",
	"completedAt",
}

// TestProductionChapterDeletionsServeTheFinalizedChapterOutbox drives the four
// names. Each subtest opens its own lane, so a failure names one operation and one
// state of the outbox.
func TestProductionChapterDeletionsServeTheFinalizedChapterOutbox(t *testing.T) {
	t.Run("chapter:list-incomplete-deletions answers an empty list for a project with no deletions",
		testProductionChapterDeletionListWithoutDeletions)
	t.Run("chapter:delete-finalized completes a delete and its file is gone",
		testProductionChapterDeletionDeleteCompletes)
	t.Run("chapter:delete-finalized leaves an operation pending while a document is owed",
		testProductionChapterDeletionLeftPendingIsListed)
	t.Run("chapter:retry-deletion settles a pending deletion",
		testProductionChapterDeletionRetrySettles)
	t.Run("chapter:retry-deletion replays a completed deletion without a write",
		testProductionChapterDeletionRetryAnswersCompleted)
	t.Run("chapter:delete-finalized waits for the legacy confirmation",
		testProductionChapterDeletionLegacyGuardBlocks)
	t.Run("chapter:confirm-legacy-knowledge-absent is consumed exactly once",
		testProductionChapterDeletionConfirmConsumesOnce)
	t.Run("every operation refuses without a live lease and validates every argument",
		testProductionChapterDeletionRefusesWithoutLease)
	t.Run("the portable-runtime freeze is honoured",
		testProductionChapterDeletionFreezeGuard)
}

// productionChapterDeletionSeedOptions describes the one finalized chapter a
// fixture starts from. Every arm is a state the outbox really reaches:
//
//	{}                          a finalized chapter nobody has deleted yet
//	target                      a published chapter with no knowledge document
//	target+knowledgeDocumentID   a chapter whose document a cleaner still owes
//	target+pending               an interrupted delete: the fact is gone, the
//	                             operation row is still pending
//	knowledgeRun                 a finalization that projected knowledge but has
//	                             no document id, which raises the legacy guard
//	plainRun                     a finalization whose run carries no kb_import
//	                             step, which clears the guard for a delete
type productionChapterDeletionSeedOptions struct {
	targetFileName      string
	knowledgeDocumentID string
	knowledgeRun        bool
	plainRun            bool
	manuscript          bool
	pending             bool
	operationID         string
}

// productionChapterDeletionSeed returns the seed hook the lane opener runs before
// it takes its writer lease, so a completed delete really deletes a committed fact.
func productionChapterDeletionSeed(options productionChapterDeletionSeedOptions) func(*testing.T, string) {
	return func(t *testing.T, projectPath string) {
		t.Helper()
		if options.pending && options.operationID == "" {
			t.Fatal("a pending fixture needs the operation id its interrupted delete minted")
		}
		database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
		db := productionFinalizedCharacterOpen(t, database, "mode=rw")
		// The fact: one content, the finalized draft that points at it and the
		// immutable receipt the deletion reads its target from.
		productionFinalizedCharacterExec(t, db, "INSERT INTO contents(id, body) VALUES(?, ?)",
			productionChapterDeletionDraft, productionChapterDeletionBody)
		productionFinalizedCharacterExec(t, db, `INSERT INTO drafts (
  id, chapter_number, version, status, source, content_id, word_count, source_dependencies
 ) VALUES (?, ?, 1, 'finalized', 'write', ?, ?, '[]')`,
			productionChapterDeletionDraft, productionChapterDeletionChapter,
			productionChapterDeletionDraft, len(productionChapterDeletionBody))
		productionFinalizedCharacterExec(t, db, `INSERT INTO finalization_outbox (
  finalization_id, draft_id, chapter_number, chapter_title, content_hash,
  content_revision, content_snapshot, target_file_name, knowledge_document_id
 ) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?)`,
			productionChapterDeletionSeedID, productionChapterDeletionDraft,
			productionChapterDeletionChapter, productionChapterDeletionTitle,
			productionGenerationWriteHash(productionChapterDeletionBody),
			productionChapterDeletionBody, options.targetFileName, options.knowledgeDocumentID)
		// The projection generation the deletion has to invalidate. It is written
		// as an upsert rather than as a bare UPDATE or OR IGNORE: the baseline
		// creates this table empty, so an UPDATE alone would write nothing, and
		// another fixture in this package may already have created the singleton
		// row with generation 0, which OR IGNORE would silently keep. The readback
		// below is what makes that a caught fact instead of an assumption.
		productionFinalizedCharacterExec(t, db,
			"INSERT INTO continuity_projection_meta(id, generation) VALUES('main', ?) "+
				"ON CONFLICT(id) DO UPDATE SET generation=excluded.generation",
			productionChapterDeletionGeneration)
		var generation int64
		productionStoryStateWriteScan(t, db,
			"SELECT generation FROM continuity_projection_meta WHERE id='main'", nil, &generation)
		if generation != productionChapterDeletionGeneration {
			t.Fatalf("continuity_projection_meta.generation = %v, want %v", generation,
				productionChapterDeletionGeneration)
		}
		if options.knowledgeRun || options.plainRun {
			productionFinalizedCharacterExec(t, db, `INSERT INTO post_process_runs (
  id, trigger_source_type, trigger_source_id
 ) VALUES (?, 'chapter_finalize', ?)`, productionChapterDeletionRunID,
				"finalization:"+productionChapterDeletionSeedID)
		}
		if options.knowledgeRun {
			// The run projected a managed document but the receipt carries no
			// document id, so the author has to declare the legacy state absent.
			productionFinalizedCharacterExec(t, db, `INSERT INTO post_process_steps (
  run_id, step_key, label, critical, ok
 ) VALUES (?, 'kb_import', '知识', 1, 1)`, productionChapterDeletionRunID)
		}
		if options.manuscript {
			path := filepath.Join(projectPath, options.targetFileName)
			if err := os.WriteFile(path, []byte(productionChapterDeletionBody), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if options.pending {
			// An interrupted delete: Begin removes the fact in its own transaction
			// (internal/store/finalized_chapter_deletion.go:304), then the process
			// dies before startAttempt, before any projection is cleaned and before
			// settle. The retry below is the only path that can finish this row.
			deletions := store.NewChapterDeletions(db, func() string { return options.operationID })
			required, err := deletions.RequiresLegacyKnowledgeAuthorization(context.Background(),
				productionChapterDeletionDraft, productionChapterDeletionChapter)
			if err != nil {
				t.Fatalf("the pending fixture's legacy guard read: %v", err)
			}
			if required {
				t.Fatal("the pending fixture must not raise the legacy guard")
			}
			if _, err := deletions.Begin(context.Background(), store.ChapterDeletionBegin{
				DraftID:       productionChapterDeletionDraft,
				ChapterNumber: productionChapterDeletionChapter,
			}); err != nil {
				t.Fatalf("stage the interrupted delete: %v", err)
			}
		}
		productionEditorCheckpoint(t, db, database)
	}
}

// productionChapterDeletionWireRequest is the wire request of delete-finalized
// (src/shared/chapter-deletion.ts:5-8). Numbers cross the wire as JSON numbers, so
// the fixture sends float64 and lets the binder project them.
func productionChapterDeletionWireRequest() map[string]any {
	return map[string]any{
		"draftId":       float64(productionChapterDeletionDraft),
		"chapterNumber": float64(productionChapterDeletionChapter),
	}
}

// productionChapterDeletionRecord is the operation object of a mutation answer.
func productionChapterDeletionRecord(t *testing.T, answer map[string]any) map[string]any {
	t.Helper()
	record := syntheticMap(answer["operation"])
	if record == nil || len(record) == 0 {
		t.Fatalf("the answer carries no operation object: %v", answer)
	}
	return record
}

// productionChapterDeletionListed is the operations array of a list answer,
// asserting along the way that the key is present as an array: the renderer does
// result.operations ?? [], so null and absent are both wrong.
func productionChapterDeletionListed(t *testing.T, answer map[string]any) []map[string]any {
	t.Helper()
	raw, present := answer["operations"]
	if !present {
		t.Fatalf("the list answer carries no operations key: %v", answer)
	}
	operations, ok := raw.([]any)
	if !ok {
		t.Fatalf("the list answer's operations is %T, want an array", raw)
	}
	records := make([]map[string]any, 0, len(operations))
	for _, operation := range operations {
		record := syntheticMap(operation)
		if record == nil {
			t.Fatalf("a listed operation is %T, not an object", operation)
		}
		records = append(records, record)
	}
	return records
}

// productionChapterDeletionFieldSet proves the wire record carries exactly the 19
// fields of the reference type, so a rename on either side fails here instead of in
// the renderer.
func productionChapterDeletionFieldSet(t *testing.T, record map[string]any) {
	t.Helper()
	got := make([]string, 0, len(record))
	for name := range record {
		got = append(got, name)
	}
	sort.Strings(got)
	want := make([]string, 0, len(productionChapterDeletionFieldNames))
	want = append(want, productionChapterDeletionFieldNames[:]...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("the deletion record carries %v, want the reference's %v", got, want)
	}
}

// productionChapterDeletionCount reads one count through the test's own connection.
func productionChapterDeletionCount(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	productionStoryStateWriteScan(t, db, query, args, &count)
	return count
}

// productionChapterDeletionText reads one text value the same way.
func productionChapterDeletionText(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	var text string
	productionStoryStateWriteScan(t, db, query, args, &text)
	return text
}

// productionChapterDeletionTransportRefusal is the fence of the four names: the
// operation answers an HTTP error with the given code, and the admitted database
// file is byte for byte what it was.
func productionChapterDeletionTransportRefusal(t *testing.T, lane *productionGenerationWriteLane, label, operation string, args []any, want string) {
	t.Helper()
	before := lane.bytes(t)
	productionFinalizedCharacterRefusal(t, label, lane.author.call(t, operation, args), want)
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatalf("%s: a refused %v changed the admitted database", label, operation)
	}
}

// productionChapterDeletionLeaseRefusal is the same proof for a stale lease, where
// the transport's own code is the one that refuses and this file does not pin which.
func productionChapterDeletionLeaseRefusal(t *testing.T, lane *productionGenerationWriteLane, label, operation string, args []any) {
	t.Helper()
	before := lane.bytes(t)
	productionEditorRefused(t, lane.author, label, operation, args)
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatalf("%s: %v without a live lease changed the admitted database", label, operation)
	}
}

// productionChapterDeletionRefusal is the mutation envelope of a state-machine
// refusal: HTTP 200, success false, committed false and the bare symbolic code the
// reference publishes in ChapterDeletionResult.error
// (src/shared/chapter-deletion.ts:32-37).
func productionChapterDeletionRefusal(t *testing.T, client *productionDurableClient, label, operation string, args []any, wantCode string) map[string]any {
	t.Helper()
	answer := productionStoryStateWriteCall(t, client, operation, args)
	if answer["success"] != false {
		t.Fatalf("%s: %s answered success for a refused operation: %v", label, operation, answer)
	}
	if answer["committed"] != false {
		t.Fatalf("%s: %s answered committed for a refused operation: %v", label, operation, answer)
	}
	if code := syntheticString(answer["error"]); code != wantCode {
		t.Fatalf("%s: %s refused with %v instead of %v", label, operation, code, wantCode)
	}
	return answer
}

// testProductionChapterDeletionListWithoutDeletions is the list on a project that
// holds a finalized chapter nobody has deleted: an empty array, and a read that
// writes nothing.
func testProductionChapterDeletionListWithoutDeletions(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionChapterDeletionSeed(
		productionChapterDeletionSeedOptions{}))
	before := lane.bytes(t)
	answer := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionListName,
		[]any{lane.projectPath, lane.frozen})
	if answer["success"] != true {
		t.Fatalf("the list answered %v for a live lease", answer)
	}
	if operations := productionChapterDeletionListed(t, answer); len(operations) != 0 {
		t.Fatalf("a project with no deletions listed %v operations", len(operations))
	}
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("listing incomplete deletions changed the admitted database")
	}
}

// testProductionChapterDeletionDeleteCompletes is the whole operation end to end:
// the fact is deleted, the published manuscript is removed from the project, the
// projection generation is invalidated, and every one of those is read back over a
// connection of the test's own.
func testProductionChapterDeletionDeleteCompletes(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionChapterDeletionSeed(
		productionChapterDeletionSeedOptions{
			targetFileName: productionChapterDeletionTargetFile,
			plainRun:       true,
			manuscript:     true,
		}))
	manuscript := filepath.Join(lane.projectPath, productionChapterDeletionTargetFile)
	if _, err := os.Lstat(manuscript); err != nil {
		t.Fatalf("the fixture has no published manuscript to delete: %v", err)
	}
	answer := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionDeleteName,
		[]any{productionChapterDeletionWireRequest(), lane.projectPath, lane.frozen})
	if answer["success"] != true {
		t.Fatalf("delete-finalized answered %v for a chapter it can delete", answer)
	}
	if answer["committed"] != true {
		t.Fatalf("delete-finalized answered %v without committing the deletion", answer)
	}
	if code, present := answer["error"]; present {
		t.Fatalf("a completed deletion answered an error %v", code)
	}
	record := productionChapterDeletionRecord(t, answer)
	productionChapterDeletionFieldSet(t, record)
	if id := syntheticString(record["operationId"]); id == "" {
		t.Fatalf("the completed operation carries no id: %v", record)
	}
	if status := syntheticString(record["status"]); status != store.ChapterDeletionCompleted {
		t.Fatalf("the completed operation reports status %v", status)
	}
	if status := syntheticString(record["manuscriptStatus"]); status != store.ChapterDeletionProjectionCompleted {
		t.Fatalf("the removed manuscript reports %v, want completed", status)
	}
	if status := syntheticString(record["knowledgeStatus"]); status != store.ChapterDeletionProjectionNotRequired {
		t.Fatalf("a chapter with no knowledge document reports %v, want not_required", status)
	}
	if attempt := syntheticNumber(record["attemptCount"]); attempt != 1 {
		t.Fatalf("the completed operation reports %v attempts, want 1", attempt)
	}
	if name := syntheticString(record["targetFileName"]); name != productionChapterDeletionTargetFile {
		t.Fatalf("the operation names the target %v", name)
	}
	// The manuscript really is gone from the project directory: an answer that says
	// completed while the author's file survives is the lie this operation exists to
	// prevent.
	if _, err := os.Lstat(manuscript); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the published manuscript survived a completed delete: %v", err)
	}
	readOnly := productionStoryStateWriteReadback(t, lane.projectPath)
	if count := productionChapterDeletionCount(t, readOnly,
		"SELECT COUNT(*) FROM chapter_deletion_operations WHERE status='completed'"); count != 1 {
		t.Fatalf("the committed outbox holds %v completed operations, want 1", count)
	}
	if status := productionChapterDeletionText(t, readOnly,
		"SELECT status FROM chapter_deletion_operations WHERE draft_id=?", productionChapterDeletionDraft); status != store.ChapterDeletionCompleted {
		t.Fatalf("the committed outbox row reports %v, want completed", status)
	}
	// The finalized fact itself: the draft, its content, its receipt and the run
	// that finalization froze are all gone, and the sibling draft of chapter 1 is
	// not.
	for _, query := range []struct {
		label string
		query string
		args  []any
	}{
		{"draft", "SELECT COUNT(*) FROM drafts WHERE id=?", []any{productionChapterDeletionDraft}},
		{"content", "SELECT COUNT(*) FROM contents WHERE id=?", []any{productionChapterDeletionDraft}},
		{"receipt", "SELECT COUNT(*) FROM finalization_outbox WHERE draft_id=?", []any{productionChapterDeletionDraft}},
		{"chapter", "SELECT COUNT(*) FROM drafts WHERE chapter_number=?", []any{productionChapterDeletionChapter}},
	} {
		if count := productionChapterDeletionCount(t, readOnly, query.query, query.args...); count != 0 {
			t.Fatalf("the deleted fact still has a %v row: %v", query.label, count)
		}
	}
	if count := productionChapterDeletionCount(t, readOnly,
		"SELECT COUNT(*) FROM drafts WHERE id=1"); count != 1 {
		t.Fatalf("the delete removed a draft of another chapter: %v chapter-1 drafts left", count)
	}
	// The continuity generation really moved, which is the projection side of the
	// deletion and the reason the seed had to INSERT the row first.
	if generation := productionChapterDeletionCount(t, readOnly,
		"SELECT generation FROM continuity_projection_meta WHERE id='main'"); generation != productionChapterDeletionGeneration+1 {
		t.Fatalf("continuity generation is %v, want the invalidated %v", generation,
			productionChapterDeletionGeneration+1)
	}
	// A completed operation is not incomplete, so it is not listed.
	listed := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionListName,
		[]any{lane.projectPath, lane.frozen})
	if operations := productionChapterDeletionListed(t, listed); len(operations) != 0 {
		t.Fatalf("a completed deletion is still listed: %v operations", len(operations))
	}
}

// testProductionChapterDeletionLeftPendingIsListed is the honest half of the
// cleaner: this lane removes the manuscript, it cannot remove a managed knowledge
// document, so the knowledge projection stays pending and the operation stays in
// the incomplete list for the lane that can finish it.
func testProductionChapterDeletionLeftPendingIsListed(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionChapterDeletionSeed(
		productionChapterDeletionSeedOptions{
			targetFileName:      productionChapterDeletionTargetFile,
			knowledgeDocumentID: productionChapterDeletionKnowledgeID,
			manuscript:          true,
		}))
	manuscript := filepath.Join(lane.projectPath, productionChapterDeletionTargetFile)
	answer := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionDeleteName,
		[]any{productionChapterDeletionWireRequest(), lane.projectPath, lane.frozen})
	if answer["committed"] != true {
		t.Fatalf("delete-finalized answered %v without committing the fact", answer)
	}
	if answer["success"] != false {
		t.Fatalf("a deletion whose document is still owed answered success: %v", answer)
	}
	if code, present := answer["error"]; present {
		// Refusal() names only a failed cleanup and the legacy guard
		// (internal/store/finalized_chapter_deletion.go:223-234). The knowledge
		// projection is not failed - nothing was attempted - so there is no code to
		// publish, and inventing one would be a claim about a cleaner that never ran.
		t.Fatalf("a still-owed knowledge document answered an error %v", code)
	}
	record := productionChapterDeletionRecord(t, answer)
	productionChapterDeletionFieldSet(t, record)
	if status := syntheticString(record["status"]); status != store.ChapterDeletionPending {
		t.Fatalf("the operation reports status %v, want pending", status)
	}
	if status := syntheticString(record["manuscriptStatus"]); status != store.ChapterDeletionProjectionCompleted {
		t.Fatalf("the removed manuscript reports %v, want completed", status)
	}
	if status := syntheticString(record["knowledgeStatus"]); status != store.ChapterDeletionProjectionPending {
		t.Fatalf("the knowledge projection reports %v, want the untouched pending", status)
	}
	if document := syntheticString(record["knowledgeDocumentId"]); document != productionChapterDeletionKnowledgeID {
		t.Fatalf("the operation names the document %v", document)
	}
	// The manuscript half really happened; the document half is the one left.
	if _, err := os.Lstat(manuscript); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the manuscript survived the projection this lane owns: %v", err)
	}
	readOnly := productionStoryStateWriteReadback(t, lane.projectPath)
	if status := productionChapterDeletionText(t, readOnly,
		"SELECT knowledge_status FROM chapter_deletion_operations WHERE draft_id=?",
		productionChapterDeletionDraft); status != store.ChapterDeletionProjectionPending {
		t.Fatalf("the committed row reports knowledge_status %v, want pending", status)
	}
	// And it is listed, with the pending projection intact, so the browser can offer
	// the retry that a lane with a knowledge-base writer will settle.
	listed := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionListName,
		[]any{lane.projectPath, lane.frozen})
	operations := productionChapterDeletionListed(t, listed)
	if len(operations) != 1 {
		t.Fatalf("the incomplete list holds %v operations, want the one left pending", len(operations))
	}
	productionChapterDeletionFieldSet(t, operations[0])
	if id := syntheticString(operations[0]["operationId"]); id != syntheticString(record["operationId"]) {
		t.Fatalf("the list answers operation %v, want the one just left pending", id)
	}
	if status := syntheticString(operations[0]["status"]); status != store.ChapterDeletionPending {
		t.Fatalf("the listed operation reports status %v, want pending", status)
	}
	if status := syntheticString(operations[0]["knowledgeStatus"]); status != store.ChapterDeletionProjectionPending {
		t.Fatalf("the listed operation reports knowledge %v, want pending", status)
	}
}

// testProductionChapterDeletionRetrySettles finishes the deletion the pending
// fixture interrupted: one attempt, the manuscript cleaned, the settle that turns
// the row completed, and a list that no longer answers it.
func testProductionChapterDeletionRetrySettles(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionChapterDeletionSeed(
		productionChapterDeletionSeedOptions{
			targetFileName: productionChapterDeletionTargetFile,
			plainRun:       true,
			manuscript:     true,
			pending:        true,
			operationID:    productionChapterDeletionPendingID,
		}))
	manuscript := filepath.Join(lane.projectPath, productionChapterDeletionTargetFile)
	readOnly := productionStoryStateWriteReadback(t, lane.projectPath)
	if status := productionChapterDeletionText(t, readOnly,
		"SELECT status FROM chapter_deletion_operations WHERE operation_id=?",
		productionChapterDeletionPendingID); status != store.ChapterDeletionPending {
		t.Fatalf("the interrupted fixture reports %v, want pending", status)
	}
	if count := productionChapterDeletionCount(t, readOnly,
		"SELECT COUNT(*) FROM drafts WHERE id=?", productionChapterDeletionDraft); count != 0 {
		t.Fatalf("the interrupted delete did not remove its fact: %v drafts left", count)
	}
	answer := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionRetryName,
		[]any{productionChapterDeletionPendingID, lane.projectPath, lane.frozen})
	if answer["success"] != true {
		t.Fatalf("the retry answered %v for a pending deletion it can settle", answer)
	}
	if answer["committed"] != true {
		t.Fatalf("the retry answered %v without committing", answer)
	}
	record := productionChapterDeletionRecord(t, answer)
	productionChapterDeletionFieldSet(t, record)
	if id := syntheticString(record["operationId"]); id != productionChapterDeletionPendingID {
		t.Fatalf("the retry answered operation %v, want the pending one", id)
	}
	if status := syntheticString(record["status"]); status != store.ChapterDeletionCompleted {
		t.Fatalf("the retried operation reports status %v, want completed", status)
	}
	if attempt := syntheticNumber(record["attemptCount"]); attempt != 1 {
		t.Fatalf("the retried operation reports %v attempts, want the one attempt", attempt)
	}
	if status := syntheticString(record["manuscriptStatus"]); status != store.ChapterDeletionProjectionCompleted {
		t.Fatalf("the retried manuscript reports %v, want completed", status)
	}
	if _, err := os.Lstat(manuscript); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the retry left the manuscript on disk: %v", err)
	}
	if status := productionChapterDeletionText(t, readOnly,
		"SELECT status FROM chapter_deletion_operations WHERE operation_id=?",
		productionChapterDeletionPendingID); status != store.ChapterDeletionCompleted {
		t.Fatalf("the committed row reports %v after the retry", status)
	}
	if attempt := productionChapterDeletionCount(t, readOnly,
		"SELECT attempt_count FROM chapter_deletion_operations WHERE operation_id=?",
		productionChapterDeletionPendingID); attempt != 1 {
		t.Fatalf("the committed row reports %v attempts after one retry", attempt)
	}
	listed := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionListName,
		[]any{lane.projectPath, lane.frozen})
	if operations := productionChapterDeletionListed(t, listed); len(operations) != 0 {
		t.Fatalf("the settled deletion is still incomplete: %v operations", len(operations))
	}
}

// testProductionChapterDeletionRetryAnswersCompleted is the replay: a retry of a
// completed deletion answers the row and writes nothing at all - no attempt, no
// clean, no settle. That is asserted twice over: the persisted attempt count stays
// the completed delete's, and not one byte of the admitted database file changes.
// The bytes alone would not settle it - a write could sit in a sidecar - which is
// why the count is read over a connection of the test's own before and after.
func testProductionChapterDeletionRetryAnswersCompleted(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionChapterDeletionSeed(
		productionChapterDeletionSeedOptions{
			targetFileName: productionChapterDeletionTargetFile,
			plainRun:       true,
			manuscript:     true,
		}))
	deleted := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionDeleteName,
		[]any{productionChapterDeletionWireRequest(), lane.projectPath, lane.frozen})
	if deleted["success"] != true {
		t.Fatalf("the fixture delete answered %v", deleted)
	}
	operationID := syntheticString(productionChapterDeletionRecord(t, deleted)["operationId"])
	readOnly := productionStoryStateWriteReadback(t, lane.projectPath)
	if attempt := productionChapterDeletionCount(t, readOnly,
		"SELECT attempt_count FROM chapter_deletion_operations WHERE operation_id=?",
		operationID); attempt != 1 {
		t.Fatalf("the completed delete reports %v attempts, want 1", attempt)
	}
	before := lane.bytes(t)
	first := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionRetryName,
		[]any{operationID, lane.projectPath, lane.frozen})
	if first["success"] != true || first["committed"] != true {
		t.Fatalf("a retry of a completed deletion answered %v", first)
	}
	firstRecord := productionChapterDeletionRecord(t, first)
	if status := syntheticString(firstRecord["status"]); status != store.ChapterDeletionCompleted {
		t.Fatalf("the replayed operation reports status %v", status)
	}
	second := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionRetryName,
		[]any{operationID, lane.projectPath, lane.frozen})
	secondRecord := productionChapterDeletionRecord(t, second)
	if id := syntheticString(secondRecord["operationId"]); id != operationID {
		t.Fatalf("the second retry answered operation %v", id)
	}
	if attempt := syntheticNumber(secondRecord["attemptCount"]); attempt != syntheticNumber(firstRecord["attemptCount"]) {
		t.Fatalf("a replay bumped the attempt count from %v to %v",
			syntheticNumber(firstRecord["attemptCount"]), attempt)
	}
	if attempt := syntheticNumber(secondRecord["attemptCount"]); attempt != 1 {
		t.Fatalf("the replayed operation reports %v attempts, want the completed one", attempt)
	}
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a retry of a completed deletion wrote to the admitted database")
	}
	if attempt := productionChapterDeletionCount(t, readOnly,
		"SELECT attempt_count FROM chapter_deletion_operations WHERE operation_id=?",
		operationID); attempt != 1 {
		t.Fatalf("the committed row reports %v attempts after two retries", attempt)
	}
}

// testProductionChapterDeletionLegacyGuardBlocks is the delete the legacy guard
// stops: the author is asked, the fact and the manuscript survive intact, and the
// waiting operation is delivered so the confirmation can name it.
//
// This is the one arm that writes on its way to a non-success answer: the
// operation row IS the request for confirmation (chapter-deletion-service.ts:68-97),
// so byte-equality would be the wrong invariant here and the invariant proved
// instead is that nothing the author owns was removed.
func testProductionChapterDeletionLegacyGuardBlocks(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionChapterDeletionSeed(
		productionChapterDeletionSeedOptions{
			targetFileName: productionChapterDeletionTargetFile,
			knowledgeRun:   true,
			manuscript:     true,
		}))
	manuscript := filepath.Join(lane.projectPath, productionChapterDeletionTargetFile)
	answer := productionChapterDeletionRefusal(t, lane.author, "delete-finalized", productionChapterDeletionDeleteName,
		[]any{productionChapterDeletionWireRequest(), lane.projectPath, lane.frozen},
		store.ErrChapterDeletionLegacyKnowledgeCheckRequired.Error())
	record := productionChapterDeletionRecord(t, answer)
	productionChapterDeletionFieldSet(t, record)
	if authorization := syntheticString(record["legacyKnowledgeAuthorization"]); authorization != store.ChapterDeletionLegacyKnowledgeRequired {
		t.Fatalf("the blocked operation reports authorization %v, want required", authorization)
	}
	if status := syntheticString(record["status"]); status != store.ChapterDeletionAuthorizationRequired {
		t.Fatalf("the blocked operation reports status %v, want authorization_required", status)
	}
	if status := syntheticString(record["manuscriptStatus"]); status != store.ChapterDeletionProjectionPending {
		t.Fatalf("a blocked delete reports manuscript %v, want untouched pending", status)
	}
	// Nothing was deleted and nothing was cleaned.
	readOnly := productionStoryStateWriteReadback(t, lane.projectPath)
	for _, query := range []struct {
		label string
		query string
		args  []any
	}{
		{"draft", "SELECT COUNT(*) FROM drafts WHERE id=?", []any{productionChapterDeletionDraft}},
		{"content", "SELECT COUNT(*) FROM contents WHERE id=?", []any{productionChapterDeletionDraft}},
		{"receipt", "SELECT COUNT(*) FROM finalization_outbox WHERE draft_id=?", []any{productionChapterDeletionDraft}},
		{"run", "SELECT COUNT(*) FROM post_process_runs WHERE id=?", []any{productionChapterDeletionRunID}},
	} {
		if count := productionChapterDeletionCount(t, readOnly, query.query, query.args...); count != 1 {
			t.Fatalf("a blocked delete removed a %v row: %v left", query.label, count)
		}
	}
	if _, err := os.Lstat(manuscript); err != nil {
		t.Fatalf("a blocked delete removed the manuscript: %v", err)
	}
	// The author has to be able to see and confirm it, so it is listed.
	listed := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionListName,
		[]any{lane.projectPath, lane.frozen})
	operations := productionChapterDeletionListed(t, listed)
	if len(operations) != 1 {
		t.Fatalf("the blocked operation is not listed: %v operations", len(operations))
	}
	if authorization := syntheticString(operations[0]["legacyKnowledgeAuthorization"]); authorization != store.ChapterDeletionLegacyKnowledgeRequired {
		t.Fatalf("the listed operation reports authorization %v, want required", authorization)
	}
}

// testProductionChapterDeletionConfirmConsumesOnce is the confirmation: it consumes
// the authorization, finishes the deletion the guard was holding, and a second
// confirmation of the same row is refused with the store's own code while leaving
// the database file byte for byte identical.
func testProductionChapterDeletionConfirmConsumesOnce(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionChapterDeletionSeed(
		productionChapterDeletionSeedOptions{
			targetFileName: productionChapterDeletionTargetFile,
			knowledgeRun:   true,
			manuscript:     true,
		}))
	manuscript := filepath.Join(lane.projectPath, productionChapterDeletionTargetFile)
	blocked := productionChapterDeletionRefusal(t, lane.author, "delete-finalized", productionChapterDeletionDeleteName,
		[]any{productionChapterDeletionWireRequest(), lane.projectPath, lane.frozen},
		store.ErrChapterDeletionLegacyKnowledgeCheckRequired.Error())
	operationID := syntheticString(productionChapterDeletionRecord(t, blocked)["operationId"])
	if operationID == "" {
		t.Fatal("the blocked delete answered no operation to confirm")
	}
	confirmed := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionConfirmName,
		[]any{operationID, lane.projectPath, lane.frozen})
	if confirmed["success"] != true {
		t.Fatalf("the confirmation answered %v for the operation it just confirmed", confirmed)
	}
	if confirmed["committed"] != true {
		t.Fatalf("the confirmation answered %v without committing", confirmed)
	}
	record := productionChapterDeletionRecord(t, confirmed)
	productionChapterDeletionFieldSet(t, record)
	if authorization := syntheticString(record["legacyKnowledgeAuthorization"]); authorization != store.ChapterDeletionLegacyKnowledgeConsumed {
		t.Fatalf("the confirmed operation reports authorization %v, want consumed", authorization)
	}
	if status := syntheticString(record["status"]); status != store.ChapterDeletionCompleted {
		t.Fatalf("the confirmed operation reports status %v, want completed", status)
	}
	if status := syntheticString(record["knowledgeStatus"]); status != store.ChapterDeletionProjectionNotRequired {
		t.Fatalf("the confirmed knowledge projection reports %v, want not_required", status)
	}
	if authorized := syntheticString(record["legacyKnowledgeAuthorizedAt"]); authorized == "" {
		t.Fatal("the confirmed operation carries no confirmation timestamp")
	}
	if _, err := os.Lstat(manuscript); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the confirmed delete left the manuscript on disk: %v", err)
	}
	readOnly := productionStoryStateWriteReadback(t, lane.projectPath)
	if count := productionChapterDeletionCount(t, readOnly,
		"SELECT COUNT(*) FROM drafts WHERE id=?", productionChapterDeletionDraft); count != 0 {
		t.Fatalf("the confirmed delete left its fact behind: %v drafts", count)
	}
	// The confirmation is spent. The second call is refused, and because the store's
	// refusal rolls its transaction back, the file is untouched.
	before := lane.bytes(t)
	productionChapterDeletionRefusal(t, lane.author, "confirm-legacy-knowledge-absent",
		productionChapterDeletionConfirmName, []any{operationID, lane.projectPath, lane.frozen},
		store.ErrChapterDeletionConfirmationConsumed.Error())
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused second confirmation wrote to the admitted database")
	}
	if authorization := productionChapterDeletionText(t, readOnly,
		"SELECT legacy_knowledge_authorization FROM chapter_deletion_operations WHERE operation_id=?",
		operationID); authorization != store.ChapterDeletionLegacyKnowledgeConsumed {
		t.Fatalf("the committed row reports authorization %v after two confirmations", authorization)
	}
	if count := productionChapterDeletionCount(t, readOnly,
		"SELECT COUNT(*) FROM chapter_deletion_operations"); count != 1 {
		t.Fatalf("the outbox holds %v operations, want the one confirmed", count)
	}
}

// testProductionChapterDeletionRefusesWithoutLease asserts both halves of the
// fence for all four names: each argument is validated before anything is borrowed,
// and a caller without the live writer lease is refused by the transport. Every
// refusal leaves the admitted database file byte for byte unchanged.
func testProductionChapterDeletionRefusesWithoutLease(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionChapterDeletionSeed(
		productionChapterDeletionSeedOptions{
			targetFileName: productionChapterDeletionTargetFile,
			plainRun:       true,
			manuscript:     true,
			pending:        true,
			operationID:    productionChapterDeletionPendingID,
		}))
	request := productionChapterDeletionWireRequest()
	// The arity of every name is its wire arguments plus the frozen context
	// (src/shared/ipc-channels.ts:1274-1290).
	for _, refusal := range []struct {
		label     string
		operation string
		args      []any
		code      string
	}{
		{"delete without the frozen context", productionChapterDeletionDeleteName,
			[]any{request, lane.projectPath}, "INVALID_ARGUMENTS"},
		{"delete with a truncated session", productionChapterDeletionDeleteName,
			[]any{request, lane.projectPath, map[string]any{"projectId": lane.frozen["projectId"]}},
			"PROJECT_SESSION_REQUIRED"},
		{"delete with no expected path", productionChapterDeletionDeleteName,
			[]any{request, "", lane.frozen}, "PROJECT_CONTEXT_REQUIRED"},
		{"delete for another project", productionChapterDeletionDeleteName,
			[]any{request, "/tmp/another-project", lane.frozen}, "PROJECT_CONTEXT_CHANGED"},
		{"delete with a fractional draft id", productionChapterDeletionDeleteName,
			[]any{map[string]any{"draftId": 1.5, "chapterNumber": float64(productionChapterDeletionChapter)},
				lane.projectPath, lane.frozen}, "INVALID_ARGUMENTS"},
		{"delete with a string chapter", productionChapterDeletionDeleteName,
			[]any{map[string]any{"draftId": float64(productionChapterDeletionDraft), "chapterNumber": "3"},
				lane.projectPath, lane.frozen}, "INVALID_ARGUMENTS"},
		{"delete without a request", productionChapterDeletionDeleteName,
			[]any{"not-an-object", lane.projectPath, lane.frozen}, "INVALID_ARGUMENTS"},
		{"retry without the frozen context", productionChapterDeletionRetryName,
			[]any{productionChapterDeletionPendingID, lane.projectPath}, "INVALID_ARGUMENTS"},
		{"retry with a numeric operation id", productionChapterDeletionRetryName,
			[]any{float64(1), lane.projectPath, lane.frozen}, "INVALID_ARGUMENTS"},
		{"retry for another project", productionChapterDeletionRetryName,
			[]any{productionChapterDeletionPendingID, "/tmp/another-project", lane.frozen},
			"PROJECT_CONTEXT_CHANGED"},
		{"confirm without the frozen context", productionChapterDeletionConfirmName,
			[]any{productionChapterDeletionPendingID}, "INVALID_ARGUMENTS"},
		{"confirm with a boolean operation id", productionChapterDeletionConfirmName,
			[]any{true, lane.projectPath, lane.frozen}, "INVALID_ARGUMENTS"},
		{"list without the frozen context", productionChapterDeletionListName,
			[]any{lane.projectPath}, "INVALID_ARGUMENTS"},
		{"list with a path instead of a session", productionChapterDeletionListName,
			[]any{lane.projectPath, "not-a-session"}, "PROJECT_SESSION_REQUIRED"},
		{"list for another project", productionChapterDeletionListName,
			[]any{"/tmp/another-project", lane.frozen}, "PROJECT_CONTEXT_CHANGED"},
	} {
		productionChapterDeletionTransportRefusal(t, lane, refusal.label, refusal.operation,
			refusal.args, refusal.code)
	}
	// The request is read but never trusted for scope: an empty operation id is not
	// refused at the fence at all, it is the store that answers it, which keeps the
	// reference's own not-found answer.
	answer := productionChapterDeletionRefusal(t, lane.author, "retry-deletion", productionChapterDeletionRetryName,
		[]any{"", lane.projectPath, lane.frozen}, store.ErrChapterDeletionOperationNotFound.Error())
	if _, present := answer["operation"]; present {
		t.Fatalf("a missing operation answered an operation object: %v", answer)
	}
	// None of those refusals reached the outbox: the interrupted operation is still
	// exactly one pending row, not a second one and not a settled one.
	if count := productionChapterDeletionCount(t,
		productionStoryStateWriteReadback(t, lane.projectPath),
		"SELECT COUNT(*) FROM chapter_deletion_operations WHERE operation_id=?",
		productionChapterDeletionPendingID); count != 1 {
		t.Fatalf("the interrupted fixture lost its pending operation: %v", count)
	}
	// Without the live lease, nothing is answered and nothing is written.
	for _, refusal := range []struct {
		label     string
		operation string
		args      []any
	}{
		{"delete-finalized", productionChapterDeletionDeleteName,
			[]any{request, lane.projectPath, lane.stale()}},
		{"retry-deletion", productionChapterDeletionRetryName,
			[]any{productionChapterDeletionPendingID, lane.projectPath, lane.stale()}},
		{"confirm-legacy-knowledge-absent", productionChapterDeletionConfirmName,
			[]any{productionChapterDeletionPendingID, lane.projectPath, lane.stale()}},
		{"list-incomplete-deletions", productionChapterDeletionListName,
			[]any{lane.projectPath, lane.stale()}},
	} {
		productionChapterDeletionLeaseRefusal(t, lane, refusal.label, refusal.operation, refusal.args)
	}
	// The lease really was live before: the same retry with the real session settles
	// the pending row, so the refusals above were about the lease and not about a
	// fixture that could never be written.
	settled := productionStoryStateWriteCall(t, lane.author, productionChapterDeletionRetryName,
		[]any{productionChapterDeletionPendingID, lane.projectPath, lane.frozen})
	if settled["success"] != true {
		t.Fatalf("the live lease could not retry the pending deletion: %v", settled)
	}
}

// productionChapterDeletionFreezeDocument is one runtime-freeze sidecar
// (electron/services/portable-runtime-freeze.ts:65-106) naming every given
// chapter_deletion_operations row. The shape is the codec's, not this file's:
// archiveformat.ParseRuntimeFreeze rejects a document that deviates from it.
func productionChapterDeletionFreezeDocument(operationIDs ...string) string {
	records := make([]string, 0, len(operationIDs))
	for index, operationID := range operationIDs {
		records = append(records, `{"projectionId":"projection-`+strconv.Itoa(index)+
			`","table":"`+productionChapterDeletionTable+`","recordId":"`+operationID+
			`","terminalState":"completed","projection":{"kind":"ci"},"projectionHash":"`+
			strings.Repeat("a1", 32)+`","excludedFields":[],"nonReplayable":true,"originalReceiptVerified":false}`)
	}
	return `{"version":1,"originProjectId":"11111111-2222-4333-8444-555555555555",` +
		`"snapshotGeneration":"portable-ci-3","nonReplayable":true,"requiresRuntimeFreezeGuard":true,` +
		`"records":[` + strings.Join(records, ",") + `],"avatarReferenceProjections":[]}`
}

// testProductionChapterDeletionFreezeGuard proves the freeze read itself: a project
// with no sidecar is not frozen, a document naming an operation freezes exactly that
// operation, and every shape the codec refuses is PORTABLE_RUNTIME_FREEZE_INVALID
// rather than an empty guard.
//
// The wire cannot reach this: admission refuses any project carrying the marker
// before the request is served (internal/authorruntime/read_linux.go:138,
// internal/projectcatalog/catalog_linux.go:228), so the frozen answer and the list
// filter are defended here, and honoured at runtime rather than assumed away.
func testProductionChapterDeletionFreezeGuard(t *testing.T) {
	root := t.TempDir()
	storage := filepath.Join(root, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0o700); err != nil {
		t.Fatal(err)
	}
	freezePath := filepath.Join(storage, productionChapterDeletionFreezeName)
	authorityPath := filepath.Join(storage, productionChapterDeletionTransferName)
	opened, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = opened.Close() }()
	// No sidecar at all: an ordinary project, so no operation is frozen.
	freeze, err := productionChapterDeletionReadFreeze(opened)
	if err != nil {
		t.Fatalf("a project with no freeze sidecar was refused: %v", err)
	}
	if freeze.isFrozen(productionChapterDeletionPendingID) {
		t.Fatal("a project with no freeze sidecar froze an operation")
	}
	// One document naming one operation: exactly that row is frozen.
	if err := os.WriteFile(freezePath,
		[]byte(productionChapterDeletionFreezeDocument(productionChapterDeletionPendingID)), 0o600); err != nil {
		t.Fatal(err)
	}
	freeze, err = productionChapterDeletionReadFreeze(opened)
	if err != nil {
		t.Fatalf("a valid freeze document was refused: %v", err)
	}
	if !freeze.isFrozen(productionChapterDeletionPendingID) {
		t.Fatal("the freeze document did not freeze the operation it names")
	}
	if freeze.isFrozen("another-operation") {
		t.Fatal("the freeze document froze an operation it does not name")
	}
	// A document the codec refuses is a refusal, never an empty guard: an author's
	// row must not become deletable because its sidecar is corrupt.
	if err := os.WriteFile(freezePath, []byte(`{"version":1,"records":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := productionChapterDeletionReadFreeze(opened); err == nil {
		t.Fatal("a malformed freeze document was read as no freeze at all")
	} else if code := productionChapterDeletionCode(err); code != productionChapterDeletionCodeFreezeInvalid {
		t.Fatalf("a malformed freeze document answered %v", code)
	}
	// A transfer authority with no freeze document is a half-installed portable
	// project, not a normal one (portable-runtime-freeze.ts:54-55).
	if err := os.Remove(freezePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorityPath, []byte(`{"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := productionChapterDeletionReadFreeze(opened); err == nil {
		t.Fatal("a transfer authority without a freeze document was read as no freeze")
	} else if code := productionChapterDeletionCode(err); code != productionChapterDeletionCodeFreezeInvalid {
		t.Fatalf("an authority without a freeze document answered %v", code)
	}
	// A marker that is not a regular file is uncertainty, never absence.
	if err := os.Remove(authorityPath); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(freezePath, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := productionChapterDeletionReadFreeze(opened); err == nil {
		t.Fatal("a directory at the freeze name was read as no freeze")
	} else if code := productionChapterDeletionCode(err); code != productionChapterDeletionCodeFreezeInvalid {
		t.Fatalf("a directory at the freeze name answered %v", code)
	}
	// The frozen answer itself: no operation object, because the frozen row is not
	// this lane's to report on (chapter-deletion-service.ts:54-57).
	frozen := productionChapterDeletionFrozenAnswer()
	if frozen.Success || frozen.Committed || frozen.Operation != nil {
		t.Fatalf("the frozen answer carries state it must not: %v", frozen)
	}
	if frozen.Error != productionChapterDeletionCodeFrozen {
		t.Fatalf("the frozen answer carries the code %v", frozen.Error)
	}
}
