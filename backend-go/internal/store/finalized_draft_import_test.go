package store

// These tests provision a real SQLite database from the pinned asset in
// internal/admission (WriteBaselineV7, baseline_v7_ddl.go:744) and drive the two
// writes of finalized_draft_import.go against the real schema: the authority
// read over drafts/contents/finalization_outbox, the three INSERTs of one
// imported chapter (contents, drafts, finalization_outbox) and the operation row
// in finalized_draft_import_operations.
//
// Every effect is proved twice over: once by a separate mode=ro connection that
// could not have performed the write, and - for every refusal - once by the
// file's own bytes, which a refused call must leave untouched. Each refusal runs
// on its own fresh database, so one refusal cannot hide behind another's state.
//
// continuity_projection_meta is seeded by an UPSERT (fdiSeedContinuity) and never
// by a bare UPDATE: a freshly provisioned v7 database has no row in that table
// (baseline_v7_ddl.go:200-204 is DDL only, nothing seeds it), so an UPDATE would
// be a silent no-op and INSERT OR IGNORE alone would keep the stale 0 that
// surfaces later as a misleading "the projection is current" reading. The row is
// then asserted unchanged after every import, because the reference invalidates
// no continuity projection here (see the implementation file's header).
//
// Word counts come from draftalg.CountDraftUnits, the shared visible-prose
// counter (draft-units.ts:49-54) the payload is validated against.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
)

const (
	fdiChapterOneBody    = "山雨欲来，风满楼。"
	fdiChapterTwoBody    = "夜色沉沉，江上无人。"
	fdiChapterThreeBody  = "归途已远，灯火将尽。"
	fdiSeededChapterBody = "这是作者自己写的定稿。"
)

// fdiChapter is one importable chapter whose word count is the shared counter's,
// which is what normalizeChapters (:79-81) demands.
func fdiChapter(number int64, title, content string) FinalizedDraftImportChapter {
	count := int64(draftalg.CountDraftUnits(content))
	return FinalizedDraftImportChapter{ChapterNumber: number, Title: title, Content: content,
		WordCount: &count}
}

// fdiWordCount is a supplied count for the malformed-payload cases.
func fdiWordCount(value int64) *int64 { return &value }

// fdiFixture provisions the pinned v7 schema in a fresh file, opens it the way a
// project database is opened (ExistingDSN: mode=rw, foreign keys on), and seats
// the continuity projection row a real project carries.
func fdiFixture(t *testing.T) (*sql.DB, *Drafts, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "finalized import #? 中文.db")
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
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	fdiSeedContinuity(t, db, 7)
	return db, NewDrafts(db), path
}

// fdiSeedContinuity writes the continuity projection generation with an upsert:
// the table starts empty, so the INSERT is the arm that matters and the conflict
// arm is what a re-seed would take.
func fdiSeedContinuity(t *testing.T, db *sql.DB, generation int64) {
	t.Helper()
	fdiExec(t, db, `INSERT INTO continuity_projection_meta (id, generation, stale_from_chapter)
 VALUES ('main', ?, NULL)
 ON CONFLICT(id) DO UPDATE SET generation = excluded.generation,
  stale_from_chapter = excluded.stale_from_chapter`, generation)
}

// fdiReadOnly opens the same file a second time, read-only, so a write is proved
// by a connection that could not have performed it.
func fdiReadOnly(t *testing.T, path string) *sql.DB {
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

func fdiExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec failed: %v", err)
	}
}

func fdiQueryRows(t *testing.T, db *sql.DB, query string, args ...any) []map[string]any {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query failed: %v", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		entry := make(map[string]any, len(columns))
		for index, column := range columns {
			if raw, ok := values[index].([]byte); ok {
				entry[column] = string(raw)
				continue
			}
			entry[column] = values[index]
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func fdiQueryOne(t *testing.T, db *sql.DB, query string, args ...any) map[string]any {
	t.Helper()
	rows := fdiQueryRows(t, db, query, args...)
	if len(rows) != 1 {
		t.Fatalf("query returned %d rows, want 1", len(rows))
	}
	return rows[0]
}

func fdiCount(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count failed: %v", err)
	}
	return count
}

func fdiText(t *testing.T, row map[string]any, column string) string {
	t.Helper()
	text, ok := row[column].(string)
	if !ok {
		t.Fatalf("%s is %T, want text", column, row[column])
	}
	return text
}

func fdiInt(t *testing.T, row map[string]any, column string) int64 {
	t.Helper()
	number, ok := row[column].(int64)
	if !ok {
		t.Fatalf("%s is %T, want an integer", column, row[column])
	}
	return number
}

// fdiDigest is the whole file's bytes, so a refusal that wrote anything at all
// fails even if it rolled the logical rows back.
// fdiDerefInt64 renders an optional preview count without printing a pointer:
// %v on a *int64 prints the address, which is how a failed assertion in this file
// once reported "0xff42b9fb440" instead of the value it was comparing.
func fdiDerefInt64(value *int64) any {
	if value == nil {
		return "omitted"
	}
	return *value
}

func fdiDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// fdiRefusal runs one refused call and proves both the code and that the
// database is byte-identical afterwards. The code is compared verbatim, so a
// wrapper cannot slip a different refusal through.
func fdiRefusal(t *testing.T, path, code string, call func() error) {
	t.Helper()
	before := fdiDigest(t, path)
	err := call()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got none", code)
	}
	if err.Error() != code {
		t.Fatalf("%s: got %v", code, err.Error())
	}
	if after := fdiDigest(t, path); after != before {
		t.Fatalf("%s changed the database", code)
	}
}

// fdiSeedFinalized writes one already-finalized chapter with its outbox row, the
// state an existing chapter number is in.
func fdiSeedFinalized(t *testing.T, db *sql.DB, chapterNumber int64, title, content, finalizationID string) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO contents (body) VALUES (?)`, content)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = db.Exec(`INSERT INTO drafts
 (chapter_number, version, status, source, content_id, word_count)
 VALUES (?, 1, 'finalized', 'write', ?, ?)`,
		chapterNumber, contentID, int64(draftalg.CountDraftUnits(content)))
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	fdiSeedOutbox(t, db, draftID, chapterNumber, title, content, finalizationID)
	return draftID
}

// fdiSeedOutbox freezes one outbox row for an existing draft, the shape every
// imported chapter gets.
func fdiSeedOutbox(t *testing.T, db *sql.DB, draftID, chapterNumber int64, title, content, finalizationID string) {
	t.Helper()
	fdiExec(t, db, `INSERT INTO finalization_outbox
 (finalization_id, draft_id, chapter_number, chapter_title, content_hash,
  content_revision, content_snapshot, target_file_name, publication_status, last_error)
 VALUES (?, ?, ?, ?, ?, 1, ?, ?, 'pending', '')`,
		finalizationID, draftID, chapterNumber, title, draftBodyHash(content), content,
		fmt.Sprintf("第%d章 %s.txt", chapterNumber, title))
}

// fdiSeedWorkingDraft writes a non-finalized draft, so the import's MAX(version)+1
// probe has to skip it rather than reuse its version.
func fdiSeedWorkingDraft(t *testing.T, db *sql.DB, chapterNumber, version int64, content string) int64 {
	t.Helper()
	result, err := db.Exec(`INSERT INTO contents (body) VALUES (?)`, content)
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	result, err = db.Exec(`INSERT INTO drafts
 (chapter_number, version, status, source, content_id, word_count)
 VALUES (?, ?, 'draft', 'write', ?, ?)`,
		chapterNumber, version, contentID, int64(draftalg.CountDraftUnits(content)))
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return draftID
}

// fdiAssertContinuity proves the import neither bumped nor damaged the projection
// meta the fixture seated.
func fdiAssertContinuity(t *testing.T, db *sql.DB) {
	t.Helper()
	row := fdiQueryOne(t, db, `SELECT generation, stale_from_chapter FROM continuity_projection_meta WHERE id = 'main'`)
	if got := fdiInt(t, row, "generation"); got != 7 {
		t.Fatalf("continuity generation is %d, want the seeded 7", got)
	}
	if row["stale_from_chapter"] != nil {
		t.Fatalf("continuity stale_from_chapter is %v, want NULL", row["stale_from_chapter"])
	}
}

// TestFinalizedImportPreviewEmptyProject is the preview over a project with no
// chapters at all: everything appends, so the batch is 'ready' from an 'empty'
// authority, and a preview writes nothing.
func TestFinalizedImportPreviewEmptyProject(t *testing.T) {
	db, drafts, path := fdiFixture(t)
	ctx := context.Background()
	before := fdiDigest(t, path)
	preview, err := drafts.FinalizedImportPreview(ctx, []FinalizedDraftImportChapter{
		fdiChapter(1, "山雨", fdiChapterOneBody),
		fdiChapter(2, "夜渡", fdiChapterTwoBody),
	})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if preview.Classification != "ready" || preview.AuthorityInvalid || preview.TargetStatus != "finalized" {
		t.Fatalf("preview is %s/%v/%s, want ready/false/finalized", preview.Classification,
			preview.AuthorityInvalid, preview.TargetStatus)
	}
	if preview.ChapterCount != 2 {
		t.Fatalf("chapterCount is %d, want 2", preview.ChapterCount)
	}
	// :167-175 - the empty authority is the fingerprint of the JSON array "[]".
	if preview.AuthorityFingerprint != draftBodyHash("[]") {
		t.Fatalf("authority fingerprint is %s, want the empty array digest", preview.AuthorityFingerprint)
	}
	wantManifest := fmt.Sprintf(
		`[{"chapterNumber":1,"title":"山雨","contentHash":%s,"wordCount":%d},`+
			`{"chapterNumber":2,"title":"夜渡","contentHash":%s,"wordCount":%d}]`,
		fdiJSON(draftBodyHash(fdiChapterOneBody)), int64(draftalg.CountDraftUnits(fdiChapterOneBody)),
		fdiJSON(draftBodyHash(fdiChapterTwoBody)), int64(draftalg.CountDraftUnits(fdiChapterTwoBody)))
	if preview.ManifestFingerprint != draftBodyHash(wantManifest) {
		t.Fatalf("manifest fingerprint is %s, want the digest of %s", preview.ManifestFingerprint,
			wantManifest)
	}
	if len(preview.NewChapterNumbers) != 2 || preview.NewChapterNumbers[0] != 1 ||
		preview.NewChapterNumbers[1] != 2 {
		t.Fatalf("newChapterNumbers is %v, want [1 2]", preview.NewChapterNumbers)
	}
	if len(preview.DuplicateChapterNumbers) != 0 || len(preview.ConflictChapterNumbers) != 0 {
		t.Fatalf("duplicates/conflicts are %v/%v, want empty", preview.DuplicateChapterNumbers,
			preview.ConflictChapterNumbers)
	}
	if len(preview.Chapters) != 2 || preview.Chapters[0].Disposition != "new" ||
		preview.Chapters[1].WordCount != int64(draftalg.CountDraftUnits(fdiChapterTwoBody)) {
		t.Fatalf("preview chapters are %v, want two new ones with the shared word counts", preview.Chapters)
	}
	// :370-372 - lastChapterNumber (0 on an empty project) plus this manifest's
	// fresh count plus one. Two fresh chapters therefore preview 3, not 1: the
	// 1 at :171 belongs to the draft import, which numbers from an empty ledger.
	if preview.NextChapterNumber == nil || *preview.NextChapterNumber != 3 {
		t.Fatalf("nextChapterNumber is %v, want 3",
			fdiDerefInt64(preview.NextChapterNumber))
	}
	if preview.FirstGapChapterNumber != nil {
		t.Fatalf("firstGapChapterNumber is %v, want omitted",
			fdiDerefInt64(preview.FirstGapChapterNumber))
	}
	if after := fdiDigest(t, path); after != before {
		t.Fatal("the preview changed the database")
	}
	fdiAssertContinuity(t, db)
}

// fdiJSON renders one JSON string value, quoting a hex digest without escaping.
func fdiJSON(value string) string {
	encoded, err := jsonMarshalCompact(value)
	if err != nil {
		panic(err.Error())
	}
	return encoded
}

// TestFinalizedImportCommitWritesBatch imports three chapters at once and proves
// the result over a separate read-only connection: chapter numbering, the version
// the MAX(version) probe skipped past, word counts, bodies, frozen outbox rows,
// the operation receipt, and the idempotent replay that writes nothing.
func TestFinalizedImportCommitWritesBatch(t *testing.T) {
	db, drafts, path := fdiFixture(t)
	ctx := context.Background()

	// A working draft on chapter 2 sits on version 2, so the import must land on
	// version 3 there and 1 elsewhere (:442-444).
	seeded := fdiSeedWorkingDraft(t, db, 2, 2, "作者正在写的草稿。")

	request := FinalizedDraftImportRequest{OperationID: "  author-import:run-1  ",
		Chapters: []FinalizedDraftImportChapter{
			fdiChapter(3, "归途", fdiChapterThreeBody),
			fdiChapter(1, "山雨", fdiChapterOneBody),
			fdiChapter(2, "夜渡", fdiChapterTwoBody),
		}}
	receipt, err := drafts.FinalizedImportCommit(ctx, request)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if receipt.Idempotent {
		t.Fatal("a first commit is not idempotent")
	}
	if receipt.OperationID != "author-import:run-1" {
		t.Fatalf("operationId is %s, want the trimmed id", receipt.OperationID)
	}
	if len(receipt.ChapterNumbers) != 3 || receipt.ChapterNumbers[0] != 1 ||
		receipt.ChapterNumbers[1] != 2 || receipt.ChapterNumbers[2] != 3 {
		t.Fatalf("chapterNumbers is %v, want ascending [1 2 3]", receipt.ChapterNumbers)
	}
	if len(receipt.Drafts) != 3 || len(receipt.Drafts[0].FinalizationID) != len("import-")+32 ||
		!strings.HasPrefix(receipt.Drafts[0].FinalizationID, "import-") {
		t.Fatalf("draft receipts are %v, want one import- identity per chapter", receipt.Drafts)
	}
	wantFinalization := "import-" + draftBodyHash("author-import:run-1:1")[:32]
	if receipt.Drafts[0].FinalizationID != wantFinalization {
		t.Fatalf("finalizationId is %s, want %s", receipt.Drafts[0].FinalizationID, wantFinalization)
	}
	if receipt.Drafts[0].ContentHash != draftBodyHash(fdiChapterOneBody) ||
		receipt.Drafts[0].TargetFileName != "第1章 山雨.txt" ||
		receipt.Drafts[0].Status != "finalized" || receipt.Drafts[0].PublicationStatus != "pending" {
		t.Fatalf("first draft receipt is %v, want a frozen pending finalized chapter", receipt.Drafts[0])
	}

	// Every effect, read by a connection that could not have written it.
	ro := fdiReadOnly(t, path)
	draftRows := fdiQueryRows(t, ro, `SELECT chapter_number, version, status, source, word_count
 FROM drafts ORDER BY chapter_number ASC, version ASC`)
	if len(draftRows) != 4 {
		t.Fatalf("drafts has %d rows, want the seeded draft plus three imported ones", len(draftRows))
	}
	wantDrafts := []struct {
		chapter, version, words int64
		status                  string
	}{
		{1, 1, int64(draftalg.CountDraftUnits(fdiChapterOneBody)), "finalized"},
		{2, 2, int64(draftalg.CountDraftUnits("作者正在写的草稿。")), "draft"},
		{2, 3, int64(draftalg.CountDraftUnits(fdiChapterTwoBody)), "finalized"},
		{3, 1, int64(draftalg.CountDraftUnits(fdiChapterThreeBody)), "finalized"},
	}
	for index, want := range wantDrafts {
		row := draftRows[index]
		if fdiInt(t, row, "chapter_number") != want.chapter || fdiInt(t, row, "version") != want.version ||
			fdiInt(t, row, "word_count") != want.words || fdiText(t, row, "status") != want.status {
			t.Fatalf("draft %d is %v, want chapter %d version %d words %d %s", index, row,
				want.chapter, want.version, want.words, want.status)
		}
		if want.status == "finalized" && fdiText(t, row, "source") != "write" {
			t.Fatalf("imported draft source is %s, want write", fdiText(t, row, "source"))
		}
	}
	if got := fdiCount(t, ro, `SELECT COUNT(*) FROM finalization_outbox`); got != 3 {
		t.Fatalf("outbox has %d rows, want 3", got)
	}
	if got := fdiCount(t, ro, `SELECT COUNT(*) FROM finalization_outbox WHERE publication_status = 'pending'
 AND last_error = '' AND content_revision = 1 AND knowledge_document_id = ''`); got != 3 {
		t.Fatalf("%d outbox rows are pending, unrevisioned and unretried, want 3", got)
	}
	bodyRows := fdiQueryRows(t, ro, `SELECT drafts.chapter_number, contents.body, finalization_outbox.content_snapshot,
 finalization_outbox.chapter_title, finalization_outbox.target_file_name,
 finalization_outbox.finalization_id, finalization_outbox.content_hash, drafts.word_count
 FROM drafts
 JOIN contents ON contents.id = drafts.content_id
 JOIN finalization_outbox ON finalization_outbox.draft_id = drafts.id
 ORDER BY drafts.chapter_number ASC`)
	if len(bodyRows) != 3 {
		t.Fatalf("the imported chapters' frozen rows are %d, want 3", len(bodyRows))
	}
	bodies := []string{fdiChapterOneBody, fdiChapterTwoBody, fdiChapterThreeBody}
	titles := []string{"山雨", "夜渡", "归途"}
	for index, number := range []int64{1, 2, 3} {
		row := bodyRows[index]
		if fdiInt(t, row, "chapter_number") != number || fdiText(t, row, "body") != bodies[index] ||
			fdiText(t, row, "content_snapshot") != bodies[index] ||
			fdiText(t, row, "chapter_title") != titles[index] ||
			fdiText(t, row, "content_hash") != draftBodyHash(bodies[index]) ||
			fdiText(t, row, "target_file_name") != fmt.Sprintf("第%d章 %s.txt", number, titles[index]) ||
			fdiText(t, row, "finalization_id") != receipt.Drafts[index].FinalizationID ||
			fdiInt(t, row, "word_count") != int64(draftalg.CountDraftUnits(bodies[index])) {
			t.Fatalf("imported chapter %d is %v, want its own body, title, hash and target name", number, row)
		}
	}
	operation := fdiQueryOne(t, ro, `SELECT operation_id, payload_hash, receipt_json
 FROM finalized_draft_import_operations`)
	encoded, err := jsonMarshalCompact(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if fdiText(t, operation, "operation_id") != "author-import:run-1" ||
		fdiText(t, operation, "payload_hash") != receipt.PayloadHash ||
		fdiText(t, operation, "receipt_json") != encoded {
		t.Fatalf("stored operation is %v, want the returned receipt frozen verbatim", operation)
	}
	if len(receipt.PayloadHash) != 64 {
		t.Fatalf("payload hash is %d characters, want a sha256 digest", len(receipt.PayloadHash))
	}
	fdiAssertContinuity(t, db)

	// The replay answers the same receipt and writes nothing at all (:406-413).
	before := fdiDigest(t, path)
	replay, err := drafts.FinalizedImportCommit(ctx, request)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replay.Idempotent {
		t.Fatal("the replay is not marked idempotent")
	}
	if replay.PayloadHash != receipt.PayloadHash || len(replay.Drafts) != 3 ||
		replay.Drafts[0].DraftID != receipt.Drafts[0].DraftID ||
		replay.Drafts[2].DraftID != receipt.Drafts[2].DraftID {
		t.Fatalf("replay receipt is %v, want the committed one", replay)
	}
	if after := fdiDigest(t, path); after != before {
		t.Fatal("the replay changed the database")
	}
	if seeded == 0 {
		t.Fatal("the seeded working draft has no id")
	}
}

// TestFinalizedImportCommitRefusesFinalizedChapter is the collision guard: a
// chapter number that already has a finalized draft is refused (:433-438), and
// because the batch is one transaction nothing else is written either.
func TestFinalizedImportCommitRefusesFinalizedChapter(t *testing.T) {
	db, drafts, path := fdiFixture(t)
	ctx := context.Background()
	fdiSeedFinalized(t, db, 2, "旧稿", fdiSeededChapterBody, "existing-finalization-2")
	fdiRefusal(t, path, "FINALIZED_IMPORT_CHAPTER_ALREADY_FINALIZED", func() error {
		_, err := drafts.FinalizedImportCommit(ctx, FinalizedDraftImportRequest{
			OperationID: "import-collision",
			Chapters: []FinalizedDraftImportChapter{
				fdiChapter(1, "山雨", fdiChapterOneBody),
				fdiChapter(2, "夜渡", fdiChapterTwoBody),
				fdiChapter(3, "归途", fdiChapterThreeBody),
			}})
		return err
	})
	// The all-or-nothing proof: the chapters before the colliding one were not
	// written either.
	if got := fdiCount(t, db, `SELECT COUNT(*) FROM drafts`); got != 1 {
		t.Fatalf("drafts has %d rows, want only the pre-existing finalized one", got)
	}
	if got := fdiCount(t, db, `SELECT COUNT(*) FROM contents`); got != 1 {
		t.Fatalf("contents has %d rows, want only the pre-existing body", got)
	}
	if got := fdiCount(t, db, `SELECT COUNT(*) FROM finalized_draft_import_operations`); got != 0 {
		t.Fatalf("%d operation rows survived a refused batch, want 0", got)
	}
	if got := fdiCount(t, db, `SELECT COUNT(*) FROM finalization_outbox`); got != 1 {
		t.Fatalf("the outbox has %d rows, want only the pre-existing one", got)
	}
}

// TestFinalizedImportCommitRollsBackWholeBatch forces a failure after the first
// chapter has already been inserted: a chapter 2 outbox identity is pre-seeded on
// another draft, so finalization_outbox's PRIMARY KEY (baseline_v7_ddl.go:218-234)
// rejects chapter 2 and the whole batch must roll back - chapter 1's contents,
// draft and outbox rows with it. This is the one arm where the transaction
// boundary is observable: every other refusal happens before the first write.
func TestFinalizedImportCommitRollsBackWholeBatch(t *testing.T) {
	db, drafts, path := fdiFixture(t)
	ctx := context.Background()
	otherBody := "别的草稿，不属于本批次。"
	draftID := fdiSeedWorkingDraft(t, db, 9, 1, otherBody)
	fdiSeedOutbox(t, db, draftID, 9, "别章", otherBody,
		"import-"+draftBodyHash("import-rollback:2")[:32])
	draftsBefore := fdiCount(t, db, `SELECT COUNT(*) FROM drafts`)
	contentsBefore := fdiCount(t, db, `SELECT COUNT(*) FROM contents`)
	_, err := drafts.FinalizedImportCommit(ctx, FinalizedDraftImportRequest{
		OperationID: "import-rollback",
		Chapters: []FinalizedDraftImportChapter{
			fdiChapter(1, "山雨", fdiChapterOneBody),
			fdiChapter(2, "夜渡", fdiChapterTwoBody),
		}})
	if err == nil {
		t.Fatal("a duplicate finalization identity must fail the batch")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "constraint") {
		t.Fatalf("the batch failed on something other than the seeded constraint: %v", err.Error())
	}
	// Chapter 1 was written before chapter 2 failed, so these counts are the proof
	// that the rollback reached it.
	if got := fdiCount(t, db, `SELECT COUNT(*) FROM drafts`); got != draftsBefore {
		t.Fatalf("drafts has %d rows after the rollback, want the %d it had before", got, draftsBefore)
	}
	if got := fdiCount(t, db, `SELECT COUNT(*) FROM contents`); got != contentsBefore {
		t.Fatalf("contents has %d rows after the rollback, want the %d it had before", got,
			contentsBefore)
	}
	if got := fdiCount(t, db, `SELECT COUNT(*) FROM drafts WHERE chapter_number IN (1, 2)`); got != 0 {
		t.Fatalf("%d rows of the refused batch survived the rollback, want 0", got)
	}
	if got := fdiCount(t, db, `SELECT COUNT(*) FROM finalization_outbox`); got != 1 {
		t.Fatalf("the outbox has %d rows after the rollback, want only the seeded one", got)
	}
	if got := fdiCount(t, db, `SELECT COUNT(*) FROM finalized_draft_import_operations`); got != 0 {
		t.Fatalf("%d operation rows survived a rolled-back batch, want 0", got)
	}
	// A second connection agrees, so the rollback is not just this pool's view.
	ro := fdiReadOnly(t, path)
	if got := fdiCount(t, ro, `SELECT COUNT(*) FROM drafts`); got != draftsBefore {
		t.Fatalf("a read-only connection sees %d drafts after the rollback, want %d", got,
			draftsBefore)
	}
	if got := fdiCount(t, ro, `SELECT COUNT(*) FROM finalization_outbox`); got != 1 {
		t.Fatalf("a read-only connection sees %d outbox rows after the rollback, want 1", got)
	}
	fdiAssertContinuity(t, db)
}

// TestFinalizedImportCommitRefusesMalformedPayload covers normalizeChapters and
// requireNonEmptyOperationId. All of it refuses before the transaction opens.
func TestFinalizedImportCommitRefusesMalformedPayload(t *testing.T) {
	valid := fdiChapter(1, "山雨", fdiChapterOneBody)
	cases := []struct {
		name        string
		code        string
		operationID string
		chapters    []FinalizedDraftImportChapter
	}{
		{"no chapters", "FINALIZED_IMPORT_CHAPTERS_INVALID", "import-empty", nil},
		{"chapter number below one", "FINALIZED_IMPORT_CHAPTERS_INVALID", "import-zero",
			[]FinalizedDraftImportChapter{fdiChapter(0, "山雨", fdiChapterOneBody)}},
		{"duplicate chapter numbers", "FINALIZED_IMPORT_CHAPTERS_INVALID", "import-duplicate",
			[]FinalizedDraftImportChapter{valid, valid}},
		{"blank content", "FINALIZED_IMPORT_CHAPTERS_INVALID", "import-blank",
			[]FinalizedDraftImportChapter{{ChapterNumber: 1, Title: "山雨", Content: "   ",
				WordCount: fdiWordCount(0)}}},
		{"missing word count", "FINALIZED_IMPORT_CHAPTERS_INVALID", "import-missing",
			[]FinalizedDraftImportChapter{{ChapterNumber: 1, Title: "山雨", Content: fdiChapterOneBody}}},
		{"wrong word count", "FINALIZED_IMPORT_CHAPTERS_INVALID", "import-wrong",
			[]FinalizedDraftImportChapter{{ChapterNumber: 1, Title: "山雨", Content: fdiChapterOneBody,
				WordCount: fdiWordCount(99)}}},
		{"blank operation id", "FINALIZED_IMPORT_OPERATION_INVALID", "   ",
			[]FinalizedDraftImportChapter{valid}},
		{"over-long operation id", "FINALIZED_IMPORT_OPERATION_INVALID", strings.Repeat("a", 201),
			[]FinalizedDraftImportChapter{valid}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, drafts, path := fdiFixture(t)
			fdiRefusal(t, path, testCase.code, func() error {
				_, err := drafts.FinalizedImportCommit(context.Background(),
					FinalizedDraftImportRequest{OperationID: testCase.operationID,
						Chapters: testCase.chapters})
				return err
			})
		})
	}
}

// TestFinalizedImportRefusesWithoutProjectDatabase is the absence refusal. The
// store borrows an admitted project database and never a lease or a session: the
// reference's only absence guard is PROJECT_NOT_OPEN (:144, :239, :297, :393), and
// a run's execution lease belongs to the import-run executor that calls in here.
func TestFinalizedImportRefusesWithoutProjectDatabase(t *testing.T) {
	_, _, path := fdiFixture(t)
	chapters := []FinalizedDraftImportChapter{fdiChapter(1, "山雨", fdiChapterOneBody)}
	request := FinalizedDraftImportRequest{OperationID: "import-no-project", Chapters: chapters}
	var absent *Drafts
	calls := []struct {
		name string
		call func() error
	}{
		{"commit on a nil repository", func() error {
			_, err := absent.FinalizedImportCommit(context.Background(), request)
			return err
		}},
		{"commit without a database", func() error {
			_, err := NewDrafts(nil).FinalizedImportCommit(context.Background(), request)
			return err
		}},
		{"preview on a nil repository", func() error {
			_, err := absent.FinalizedImportPreview(context.Background(), chapters)
			return err
		}},
		{"preview without a database", func() error {
			_, err := NewDrafts(nil).FinalizedImportPreview(context.Background(), chapters)
			return err
		}},
		{"committed operation without a database", func() error {
			_, err := NewDrafts(nil).FinalizedImportCommittedOperation(context.Background(),
				"import-no-project", chapters)
			return err
		}},
	}
	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			fdiRefusal(t, path, "PROJECT_NOT_OPEN", call.call)
		})
	}
}

// TestFinalizedImportCommitRefusesOperationConflict binds an operation id to one
// payload only (:407-409).
func TestFinalizedImportCommitRefusesOperationConflict(t *testing.T) {
	_, drafts, path := fdiFixture(t)
	ctx := context.Background()
	first := FinalizedDraftImportRequest{OperationID: "import-conflict",
		Chapters: []FinalizedDraftImportChapter{fdiChapter(1, "山雨", fdiChapterOneBody)}}
	if _, err := drafts.FinalizedImportCommit(ctx, first); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	fdiRefusal(t, path, "FINALIZED_IMPORT_OPERATION_CONFLICT", func() error {
		_, err := drafts.FinalizedImportCommit(ctx, FinalizedDraftImportRequest{
			OperationID: "import-conflict",
			Chapters:    []FinalizedDraftImportChapter{fdiChapter(1, "山雨", fdiChapterTwoBody)}})
		return err
	})
}

// TestFinalizedImportCommitRefusesStalePreview covers both stale arms: the commit
// manifest (:417-421) and the authority (:422-426). The refusals happen inside
// the transaction, so the byte proof is the rollback proving itself.
func TestFinalizedImportCommitRefusesStalePreview(t *testing.T) {
	chapters := []FinalizedDraftImportChapter{fdiChapter(1, "山雨", fdiChapterOneBody)}
	t.Run("stale commit manifest", func(t *testing.T) {
		_, drafts, path := fdiFixture(t)
		stale := strings.Repeat("a", 64)
		fdiRefusal(t, path, "FINALIZED_IMPORT_PREVIEW_STALE", func() error {
			_, err := drafts.FinalizedImportCommit(context.Background(), FinalizedDraftImportRequest{
				OperationID: "import-stale-manifest", Chapters: chapters,
				ExpectedCommitManifestFingerprint: &stale})
			return err
		})
	})
	t.Run("stale legacy manifest", func(t *testing.T) {
		_, drafts, path := fdiFixture(t)
		stale := strings.Repeat("b", 64)
		fdiRefusal(t, path, "FINALIZED_IMPORT_PREVIEW_STALE", func() error {
			_, err := drafts.FinalizedImportCommit(context.Background(), FinalizedDraftImportRequest{
				OperationID: "import-stale-legacy", Chapters: chapters,
				ExpectedManifestFingerprint: &stale})
			return err
		})
	})
	t.Run("stale authority", func(t *testing.T) {
		_, drafts, path := fdiFixture(t)
		preview, err := drafts.FinalizedImportPreview(context.Background(), chapters)
		if err != nil {
			t.Fatalf("preview: %v", err)
		}
		stale := strings.Repeat("c", 64)
		fdiRefusal(t, path, "FINALIZED_IMPORT_PREVIEW_STALE", func() error {
			_, err := drafts.FinalizedImportCommit(context.Background(), FinalizedDraftImportRequest{
				OperationID: "import-stale-authority", Chapters: chapters,
				ExpectedCommitManifestFingerprint: &preview.ManifestFingerprint,
				ExpectedAuthorityFingerprint:      &stale})
			return err
		})
	})
}

// TestFinalizedImportCommitRefusesAuthorityConflict is the arm where both
// fingerprints match but the batch would still not append cleanly: the authority
// is empty, so chapter 2 leaves a hole at 1, and preview classifies it 'conflict'
// (:427-430).
func TestFinalizedImportCommitRefusesAuthorityConflict(t *testing.T) {
	_, drafts, path := fdiFixture(t)
	ctx := context.Background()
	empty, err := drafts.FinalizedImportPreview(ctx,
		[]FinalizedDraftImportChapter{fdiChapter(1, "山雨", fdiChapterOneBody)})
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	gapped := []FinalizedDraftImportChapter{fdiChapter(2, "夜渡", fdiChapterTwoBody)}
	gappedPreview, err := drafts.FinalizedImportPreview(ctx, gapped)
	if err != nil {
		t.Fatalf("gapped preview: %v", err)
	}
	if gappedPreview.Classification != "conflict" || gappedPreview.FirstGapChapterNumber == nil ||
		*gappedPreview.FirstGapChapterNumber != 1 {
		t.Fatalf("gapped preview is %s (gap %v), want conflict at chapter 1",
			gappedPreview.Classification, gappedPreview.FirstGapChapterNumber)
	}
	fdiRefusal(t, path, "FINALIZED_IMPORT_AUTHORITY_CONFLICT", func() error {
		_, err := drafts.FinalizedImportCommit(ctx, FinalizedDraftImportRequest{
			OperationID:                       "import-gap",
			Chapters:                          gapped,
			ExpectedCommitManifestFingerprint: &gappedPreview.ManifestFingerprint,
			ExpectedAuthorityFingerprint:      &empty.AuthorityFingerprint})
		return err
	})
}

// TestFinalizedImportCommitRefusesChangedFacts is the replay guard: once a frozen
// fact moved, the operation must not answer its receipt (:267-279).
func TestFinalizedImportCommitRefusesChangedFacts(t *testing.T) {
	db, drafts, path := fdiFixture(t)
	ctx := context.Background()
	request := FinalizedDraftImportRequest{OperationID: "import-changed",
		Chapters: []FinalizedDraftImportChapter{fdiChapter(1, "山雨", fdiChapterOneBody),
			fdiChapter(2, "夜渡", fdiChapterTwoBody)}}
	if _, err := drafts.FinalizedImportCommit(ctx, request); err != nil {
		t.Fatalf("commit: %v", err)
	}
	fdiExec(t, db, `UPDATE contents SET body = body || '篡改'
 WHERE id = (SELECT content_id FROM drafts WHERE chapter_number = 1 AND status = 'finalized')`)
	fdiRefusal(t, path, "FINALIZED_IMPORT_FACTS_CHANGED", func() error {
		_, err := drafts.FinalizedImportCommit(ctx, request)
		return err
	})
}

// TestFinalizedImportCommittedOperation is getCommittedOperation (:292-309): the
// replay probe answers the receipt, answers nil for an unknown operation, and
// refuses a receipt that no longer describes itself.
func TestFinalizedImportCommittedOperation(t *testing.T) {
	db, drafts, path := fdiFixture(t)
	ctx := context.Background()
	chapters := []FinalizedDraftImportChapter{fdiChapter(1, "山雨", fdiChapterOneBody)}
	request := FinalizedDraftImportRequest{OperationID: "import-committed", Chapters: chapters}
	committed, err := drafts.FinalizedImportCommit(ctx, request)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	found, err := drafts.FinalizedImportCommittedOperation(ctx, "import-committed", chapters)
	if err != nil {
		t.Fatalf("committed operation: %v", err)
	}
	if found == nil || found.OperationID != committed.OperationID ||
		found.PayloadHash != committed.PayloadHash || len(found.Drafts) != 1 ||
		found.Drafts[0].DraftID != committed.Drafts[0].DraftID {
		t.Fatalf("committed operation is %v, want the stored receipt", found)
	}
	absent, err := drafts.FinalizedImportCommittedOperation(ctx, "import-unknown", chapters)
	if err != nil {
		t.Fatalf("unknown operation: %v", err)
	}
	if absent != nil {
		t.Fatalf("unknown operation is %v, want nil", absent)
	}
	// :211-232 - a receipt that is not a JSON object is corrupt, never empty.
	fdiExec(t, db, `UPDATE finalized_draft_import_operations SET receipt_json = 'not json'
 WHERE operation_id = 'import-committed'`)
	fdiRefusal(t, path, "FINALIZED_IMPORT_RECEIPT_CORRUPT", func() error {
		_, err := drafts.FinalizedImportCommittedOperation(ctx, "import-committed", chapters)
		return err
	})
	fdiRefusal(t, path, "FINALIZED_IMPORT_RECEIPT_CORRUPT", func() error {
		_, err := drafts.FinalizedImportCommit(ctx, request)
		return err
	})
}
