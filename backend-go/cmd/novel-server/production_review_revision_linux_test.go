//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The fixture is one disposable t.TempDir(): a real admitted project whose
// database carries the review/revision rows the store half already writes, the
// frozen-context materials prepare would read (project core, one chapter
// blueprint, a continuity projection generation), and one generation run frozen
// under a non-live epoch - the strongest run row this lane can hold, because the
// admission permit hashes the file at project:open and the live lease is minted
// after that hash.
const (
	productionReviewLaneProjectName = "review-revision-novel"
	productionReviewLaneProjectID   = "b8d3e0a1-4c67-4f92-9a15-7e0c2b6d8f34"
	productionReviewLaneStaleLease  = "review-revision-stale-lease"
	productionReviewLaneFrozenEpoch = "lease-0000000007-frozen-review"
	productionReviewLaneOperation   = "review-chapter"

	// Draft 1 is the review's frozen source and has no pending revision at all;
	// draft 2 owns the pending revision whose frozen source still matches; draft 3
	// owns the pending revision whose frozen source has moved on.
	productionReviewLaneDraftID       = int64(1)
	productionReviewLanePendingDraft  = int64(2)
	productionReviewLaneMovedDraft    = int64(3)
	productionReviewLaneDraftBody     = "第一章原稿：他推开门。"
	productionReviewLaneChapter2Body  = "第二章原稿：窗外有风。"
	productionReviewLaneChapter3Body  = "第三章新稿：雨停了。"
	productionReviewLaneMovedFrozen   = "第三章旧稿：雨没停。"
	productionReviewLaneReviewBody    = "审稿意见：开篇缺少钩子。"
	productionReviewLaneRevisionBody  = "修稿建议：加一句钩子。"
	productionReviewLaneContinuityGen = 3
)

// productionReviewLaneSchema is the fixture schema: the ledger tables the run
// lives in, the four repository tables the store half writes, and the frozen
// context's materials. continuity_projection_meta keeps the shipped baseline's
// exact columns (internal/admission/baseline_v7_ddl.go:200) so the upsert below
// exercises the real conflict target instead of a test-only shape.
var productionReviewLaneSchema = append([]string{
	"CREATE TABLE generation_roots (root_action_id TEXT PRIMARY KEY, idempotency_key TEXT NOT NULL UNIQUE, action_json TEXT NOT NULL, budget_json TEXT NOT NULL, active_elapsed_ms INTEGER NOT NULL DEFAULT 0, active_since_ms INTEGER, blocked_code TEXT, revision INTEGER NOT NULL DEFAULT 0)",
	"CREATE TABLE generation_runs (run_id TEXT PRIMARY KEY, root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id), binding_json TEXT NOT NULL, status TEXT NOT NULL, created_at_ms INTEGER NOT NULL, open_key TEXT NOT NULL UNIQUE)",
	"CREATE TABLE generation_attempts (attempt_id TEXT PRIMARY KEY, reservation_id TEXT NOT NULL UNIQUE, run_id TEXT NOT NULL REFERENCES generation_runs(run_id), root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id), attempt_json TEXT NOT NULL, usage_receipt_json TEXT, invocation_nonce TEXT NOT NULL, UNIQUE(run_id, invocation_nonce))",
	"CREATE TABLE generation_artifacts (artifact_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL UNIQUE REFERENCES generation_attempts(attempt_id), run_id TEXT NOT NULL REFERENCES generation_runs(run_id), artifact_json TEXT NOT NULL, revision INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'partial')",
	"CREATE TABLE blueprints (chapter_number INTEGER PRIMARY KEY, title TEXT, role TEXT, purpose TEXT, key_events TEXT, characters TEXT, suspense_hook TEXT, user_guidance TEXT, notes TEXT, notes_updated_at TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE continuity_projection_meta (id TEXT PRIMARY KEY, generation INTEGER NOT NULL DEFAULT 0, stale_from_chapter INTEGER DEFAULT NULL)",
	"CREATE TABLE reviews (id INTEGER PRIMARY KEY, base_draft_id INTEGER, review_index INTEGER, content_id INTEGER, created_at TEXT, source_draft_chapter_number INTEGER, source_draft_version INTEGER, source_draft_status TEXT, source_content TEXT)",
	"CREATE TABLE revisions (id INTEGER PRIMARY KEY, base_draft_id INTEGER, revision_index INTEGER, revision_type TEXT, status TEXT DEFAULT 'pending', merged_to_draft_id INTEGER, user_prompt TEXT, review_source_id INTEGER, content_id INTEGER, word_count INTEGER, created_at TEXT, updated_at TEXT, source_draft_chapter_number INTEGER, source_draft_version INTEGER, source_draft_status TEXT, source_content TEXT)",
}, productionDurableSchema...)

// productionReviewLaneDatabase is the author database path inside one admitted
// project, spelled once so no subtest builds it differently.
func productionReviewLaneDatabase(projectPath string) string {
	return filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
}

// productionReviewLaneRoot builds the disposable data root and seeds it through a
// raw write connection, before the lane opens the file: the permit hashes the
// database at project:open, so rows written afterwards would move the bytes the
// refusal assertions compare.
func productionReviewLaneRoot(t *testing.T) (root, projectPath, database string, contract admission.SchemaContract) {
	t.Helper()
	root = syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionReviewLaneProjectName, productionReviewLaneProjectID,
		admission.CurrentSchemaVersion, productionReviewLaneSchema)
	projectPath = filepath.Join(library, productionReviewLaneProjectName)
	database = productionReviewLaneDatabase(projectPath)
	productionReviewLaneSeedRows(t, projectPath, database)
	contract = productionGraphReadsContract(t, productionReviewLaneSchema)
	return root, projectPath, database, contract
}

// productionReviewLaneSeedRows writes the author rows and reads the continuity
// generation back. The meta row is upserted, never a bare UPDATE: the baseline
// creates continuity_projection_meta empty (internal/admission/baseline_v7_ddl.go:200),
// so an UPDATE would silently seed nothing and an OR IGNORE would silently keep
// whatever a later migration left there.
func productionReviewLaneSeedRows(t *testing.T, projectPath, database string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, created_at, updated_at)
 VALUES ('main', '审稿修稿小说', 'zh-CN', 'fantasy', '', '', 12, 3000, 'auto', 3,
 '', '', '', '门的世界', '', '', '', '', '', '{}', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO blueprints (chapter_number, title, role, purpose, key_events, characters,
 suspense_hook, user_guidance, notes, notes_updated_at, created_at, updated_at)
 VALUES (1, '第一章 门', 'setup', '开篇', '["他推开门"]', '[]', '风声', '', '', '',
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		// The upsert the brief asks for: one row, correct on an empty table and on
		// a table a migration already touched.
		{`INSERT INTO continuity_projection_meta (id, generation, stale_from_chapter)
 VALUES ('main', ?, NULL)
 ON CONFLICT(id) DO UPDATE SET generation = excluded.generation,
 stale_from_chapter = excluded.stale_from_chapter`, []any{productionReviewLaneContinuityGen}},
		{`INSERT INTO contents (id, body) VALUES (1, ?)`, []any{productionReviewLaneDraftBody}},
		{`INSERT INTO contents (id, body) VALUES (2, ?)`, []any{productionReviewLaneReviewBody}},
		{`INSERT INTO contents (id, body) VALUES (3, ?)`, []any{productionReviewLaneChapter2Body}},
		{`INSERT INTO contents (id, body) VALUES (4, ?)`, []any{productionReviewLaneChapter3Body}},
		{`INSERT INTO contents (id, body) VALUES (5, ?)`, []any{productionReviewLaneRevisionBody}},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id, word_count,
 source_dependencies, created_at, updated_at)
 VALUES (1, 1, 1, 'draft', 'write', 1, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id, word_count,
 source_dependencies, created_at, updated_at)
 VALUES (2, 2, 1, 'draft', 'write', 3, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id, word_count,
 source_dependencies, created_at, updated_at)
 VALUES (3, 3, 1, 'draft', 'write', 4, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		// One saved review of draft 1, frozen on draft 1's own source.
		{`INSERT INTO reviews (id, base_draft_id, review_index, content_id, created_at,
 source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (1, 1, 1, 2, '2024-05-06 07:08:09', 1, 1, 'draft', ?)`, []any{productionReviewLaneDraftBody}},
		// Draft 2's pending revision still matches its frozen source.
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at, updated_at,
 source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (1, 2, 1, 'refine', 'pending', NULL, '', NULL, 5, 11, '2024-05-06 07:08:09', '2024-05-06 07:08:09',
 2, 1, 'draft', ?)`, []any{productionReviewLaneChapter2Body}},
		// Draft 3's pending revision was frozen on the prose draft 3 no longer holds.
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at, updated_at,
 source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (2, 3, 1, 'review-fix', 'pending', NULL, '补钩子', NULL, 5, 11, '2024-05-06 07:08:09', '2024-05-06 07:08:09',
 3, 1, 'draft', ?)`, []any{productionReviewLaneMovedFrozen}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			_ = db.Close()
			t.Fatalf("seed %s: %v", statement.query, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Read the meta row back over a separate read-only connection: the seed is only
	// real if the file says so.
	generation := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
		"SELECT generation FROM continuity_projection_meta WHERE id = 'main'"), "generation")
	if generation != productionReviewLaneContinuityGen {
		t.Fatalf("continuity_projection_meta.generation = %d; want the upserted %d", generation, productionReviewLaneContinuityGen)
	}
}

// productionReviewLaneSeedRun writes one review-chapter run through the shipped
// ledger store, frozen under a non-live epoch and carrying the only manifest this
// lane can write: an operation and nothing else. The reference context
// (sourceManifest.reviewRevisionContext, reviewRevisionContextHash) is
// deliberately absent, because no writer in this module produces it.
func productionReviewLaneSeedRun(t *testing.T, database string) (rootID, runID string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ordinal := 0
	ledger := store.NewGenerationLedger(db,
		func() int64 { return 1700000000000 },
		func() string {
			ordinal++
			return "review-revision-ledger-" + strconv.Itoa(ordinal)
		})
	manifest := map[string]any{}
	manifest["operation"] = productionReviewLaneOperation
	run := productionGraphReadsSeedRun(t, context.Background(), ledger, productionGraphReadsSeedRequest(t,
		productionReviewLaneOperation, productionReviewLaneProjectID, productionReviewLaneFrozenEpoch, manifest))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return syntheticString(run["rootActionId"]), syntheticString(run["runId"])
}

// productionReviewLaneLine is one line of a source file, so a seam note that has
// drifted off its declaration fails the test instead of pointing at nothing.
func productionReviewLaneLine(t *testing.T, path string, line int) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(raw), "\n")
	if line < 1 || line > len(lines) {
		t.Fatalf("%s holds %d lines; the seam names line %d", path, len(lines), line)
	}
	return lines[line-1]
}

// productionReviewLaneSeamProof re-reads every declaration one refused name's
// seam points at.
func productionReviewLaneSeamProof(t *testing.T, operation string) {
	t.Helper()
	seam, found := productionReviewRevisionSeamOf(operation)
	if !found || len(seam.Required) == 0 {
		t.Fatalf("%s has no documented missing piece; a refused name may not be undocumented", operation)
	}
	for _, missing := range seam.Required {
		if !strings.Contains(productionReviewLaneLine(t, missing.Path, missing.Line), missing.Marker) {
			t.Fatalf("the seam for %s names %s:%d, which no longer holds %s", operation, missing.Path, missing.Line, missing.Marker)
		}
	}
}

// productionReviewLaneRefusal asserts the fail-closed answer for one name: the
// transport's own 501 GO_OPERATION_NOT_MIGRATED, no filesystem path in the body,
// and an author database the permit hashed and this call did not touch.
func productionReviewLaneRefusal(t *testing.T, client *productionDurableClient, database string, before []byte,
	projectPath, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("%s answered %d; want the fail-closed 501: %s", operation, response.Code, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if body.Error.Code != "GO_OPERATION_NOT_MIGRATED" {
		t.Fatalf("%s refused with %s; want GO_OPERATION_NOT_MIGRATED: %s", operation, body.Error.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), projectPath) {
		t.Fatalf("%s leaked a filesystem path in its refusal: %s", operation, response.Body.String())
	}
	productionGraphReadsUnchanged(t, database, before, operation)
}

// TestProductionLaneRefusesReviewRevision pins the four review-revision names
// where this lane leaves them: unbound, answering the fail-closed 501. One
// subtest per operation, so a failure names the channel that moved.
//
// The brief for this file expected a commit-review that creates a review. That is
// not what this lane can honestly do, and the subtests say so instead of
// pretending: the commit request is the reference's own shape, the fixture carries
// the draft, the saved review and the pending revisions the store half would need,
// and the answer is still the 501 because the frozen context and the effect seam
// do not exist (production_review_revision_linux.go, the four seams). The rows are
// proved over a second, read-only connection, so an acknowledgement is never taken
// as evidence - and the file stays byte-identical, which is what makes the 501 a
// real boundary rather than a stub that answers before the write.
func TestProductionLaneRefusesReviewRevision(t *testing.T) {
	root, projectPath, database, contract := productionReviewLaneRoot(t)
	rootID, runID := productionReviewLaneSeedRun(t, database)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "review-revision-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: success %v dbReady %v error %s",
			opened.Success, opened.DBReady, opened.Error)
	}
	projectID := syntheticString(opened.Project["id"])
	leaseID := syntheticString(opened.Project["sessionLease"])
	if projectID != productionReviewLaneProjectID || leaseID == "" {
		t.Fatalf("project:open returned %s with lease %s; want the seeded project under a live lease", projectID, leaseID)
	}
	frozen := map[string]any{"projectId": projectID, "projectPath": projectPath, "leaseId": leaseID}
	stale := map[string]any{"projectId": projectID, "projectPath": projectPath,
		"leaseId": productionReviewLaneStaleLease}
	unleased := map[string]any{"projectId": projectID, "projectPath": projectPath}
	// The handle the reference itself would accept for the seeded run: the frozen
	// epoch is the run's own binding epoch, not this session's lease.
	handle := map[string]any{"projectId": projectID, "epoch": productionReviewLaneFrozenEpoch,
		"rootActionId": rootID, "runId": runID}
	artifact := map[string]any{"artifactId": "review-revision-artifact-1", "revision": float64(1),
		"textHash": strings.Repeat("a", 64)}
	before := productionGraphReadsBytes(t, database)
	reviewRequest := map[string]any{"contextId": "review-revision-context-1", "handle": handle, "artifact": artifact}
	revisionRequest := map[string]any{"contextId": "review-revision-context-1", "handle": handle,
		"expectedCompositionHash": strings.Repeat("b", 64)}
	prepareRequest := map[string]any{"operation": productionReviewLaneOperation, "draftId": float64(productionReviewLaneDraftID),
		"expectedDraft": map[string]any{"chapterNumber": float64(1), "version": float64(1), "status": "draft",
			"contentHash": strings.Repeat("c", 64)},
		"authorInputs": []any{map[string]any{"id": "review-focus", "text": "看开篇"}}, "uiLocale": "zh-CN"}

	t.Run("the store half is a one-to-one port of the two repositories", func(t *testing.T) {
		// The claim this batch was handed, checked declaration by declaration: the
		// store methods exist exactly where the port says they do, and not one of
		// them is a review-revision operation.
		for _, declared := range []struct {
			line   int
			marker string
		}{
			{266, "func (r *ReviewRevisions) CreateReview("},
			{297, "func (r *ReviewRevisions) CreateRevision("},
			{301, "func (r *ReviewRevisions) ReplacePending("},
			{383, "func (r *ReviewRevisions) MergeRevision("},
			{476, "func (r *ReviewRevisions) MarkDiscarded("},
			{480, "func (r *ReviewRevisions) MarkMerged("},
			{148, "func (r *ReviewRevisions) ListReviews("},
			{159, "func (r *ReviewRevisions) FullReview("},
			{165, "func (r *ReviewRevisions) ListRevisions("},
			{172, "func (r *ReviewRevisions) FullRevision("},
			{178, "func (r *ReviewRevisions) NextReviewIndex("},
			{185, "func (r *ReviewRevisions) NextRevisionIndex("},
			{230, "func rrAssertSource("},
			{250, "func rrUnbound("},
		} {
			line := productionReviewLaneLine(t, "../../internal/store/reviewrevision.go", declared.line)
			if !strings.Contains(line, declared.marker) {
				t.Fatalf("internal/store/reviewrevision.go:%d no longer holds %s", declared.line, declared.marker)
			}
		}
	})

	t.Run("prepare on a project with a draft", func(t *testing.T) {
		productionReviewLaneSeamProof(t, productionReviewRevisionPrepare)
		// The fixture really is a project with a draft and the freeze's materials:
		// the refusal is not an empty-database artifact.
		body := productionFixtureText(t, productionFixtureSingle(t, projectPath,
			"SELECT c.body AS body FROM drafts d JOIN contents c ON c.id = d.content_id WHERE d.id = 1"), "body")
		if body != productionReviewLaneDraftBody {
			t.Fatalf("draft 1 holds %s; want the seeded chapter 1 prose", body)
		}
		blueprint := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			"SELECT chapter_number FROM blueprints WHERE chapter_number = 1"), "chapter_number")
		if blueprint != 1 {
			t.Fatalf("the chapter 1 blueprint reads %d; want the seeded chapter", blueprint)
		}
		productionReviewLaneRefusal(t, author, database, before, projectPath, productionReviewRevisionPrepare,
			[]any{prepareRequest, projectPath, frozen})
	})

	t.Run("commit-review on a project with a draft", func(t *testing.T) {
		productionReviewLaneSeamProof(t, productionReviewRevisionCommitReview)
		// No review is created, proved over a separate mode=ro connection: one row
		// before, one row after, and the seeded body still where the store put it.
		total := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			"SELECT COUNT(*) AS total FROM reviews WHERE base_draft_id = 1"), "total")
		if total != 1 {
			t.Fatalf("draft 1 carries %d reviews before the refusal; want the one the seed committed", total)
		}
		productionReviewLaneRefusal(t, author, database, before, projectPath, productionReviewRevisionCommitReview,
			[]any{reviewRequest, projectPath, frozen})
		after := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			"SELECT COUNT(*) AS total FROM reviews WHERE base_draft_id = 1"), "total")
		if after != 1 {
			t.Fatalf("a refused commit-review created a review: %d rows for draft 1, want 1", after)
		}
		saved := productionFixtureText(t, productionFixtureSingle(t, projectPath,
			"SELECT c.body AS body FROM reviews r JOIN contents c ON c.id = r.content_id WHERE r.id = 1"), "body")
		if saved != productionReviewLaneReviewBody {
			t.Fatalf("the seeded review body reads %s; want the seeded report", saved)
		}
	})

	t.Run("commit-revision on a pending revision", func(t *testing.T) {
		productionReviewLaneSeamProof(t, productionReviewRevisionCommitRevision)
		status := productionFixtureText(t, productionFixtureSingle(t, projectPath,
			"SELECT status FROM revisions WHERE id = 1"), "status")
		if status != "pending" {
			t.Fatalf("revision 1 reads status %s before the refusal; want pending", status)
		}
		productionReviewLaneRefusal(t, author, database, before, projectPath, productionReviewRevisionCommitRevision,
			[]any{revisionRequest, projectPath, frozen})
		status = productionFixtureText(t, productionFixtureSingle(t, projectPath,
			"SELECT status FROM revisions WHERE id = 1"), "status")
		if status != "pending" {
			t.Fatalf("a refused commit-revision moved revision 1 to %s; want it still pending", status)
		}
	})

	t.Run("commit whose expected source has moved", func(t *testing.T) {
		// Draft 3's pending revision was frozen on prose draft 3 no longer holds, so
		// the reference's assertSources would refuse this commit even with a frozen
		// context. The fixture proves the move over the read-only connection first.
		row := productionFixtureSingle(t, projectPath,
			`SELECT v.source_content AS source_content, c.body AS body
 FROM revisions v JOIN drafts d ON d.id = v.base_draft_id JOIN contents c ON c.id = d.content_id
 WHERE v.id = 2`)
		frozenSource := productionFixtureText(t, row, "source_content")
		current := productionFixtureText(t, row, "body")
		if frozenSource != productionReviewLaneMovedFrozen || current != productionReviewLaneChapter3Body || frozenSource == current {
			t.Fatalf("the moved-source fixture reads frozen %s and current %s; want the two different seeded bodies",
				frozenSource, current)
		}
		productionReviewLaneRefusal(t, author, database, before, projectPath, productionReviewRevisionCommitReview,
			[]any{reviewRequest, projectPath, frozen})
		productionReviewLaneRefusal(t, author, database, before, projectPath, productionReviewRevisionCommitRevision,
			[]any{revisionRequest, projectPath, frozen})
	})

	t.Run("read-recovery with no pending revision", func(t *testing.T) {
		productionReviewLaneSeamProof(t, productionReviewRevisionReadRecovery)
		pending := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			"SELECT COUNT(*) AS total FROM revisions WHERE base_draft_id = 1 AND status = 'pending'"), "total")
		if pending != 0 {
			t.Fatalf("draft 1 carries %d pending revisions; this scenario needs none", pending)
		}
		productionReviewLaneRefusal(t, author, database, before, projectPath, productionReviewRevisionReadRecovery,
			[]any{map[string]any{"handle": handle}, projectPath, frozen})
	})

	t.Run("read-recovery with a pending revision", func(t *testing.T) {
		pending := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			"SELECT COUNT(*) AS total FROM revisions WHERE base_draft_id = 2 AND status = 'pending'"), "total")
		if pending != 1 {
			t.Fatalf("draft 2 carries %d pending revisions; this scenario needs exactly one", pending)
		}
		productionReviewLaneRefusal(t, author, database, before, projectPath, productionReviewRevisionReadRecovery,
			[]any{map[string]any{"handle": handle}, projectPath, frozen})
	})

	t.Run("read-recovery after the source moved", func(t *testing.T) {
		row := productionFixtureSingle(t, projectPath,
			`SELECT v.source_content AS source_content, c.body AS body
 FROM revisions v JOIN drafts d ON d.id = v.base_draft_id JOIN contents c ON c.id = d.content_id
 WHERE v.id = 2`)
		if productionFixtureText(t, row, "source_content") == productionFixtureText(t, row, "body") {
			t.Fatal("draft 3's frozen revision source still matches the draft; this scenario needs a moved source")
		}
		productionReviewLaneRefusal(t, author, database, before, projectPath, productionReviewRevisionReadRecovery,
			[]any{map[string]any{"handle": handle}, projectPath, frozen})
	})

	t.Run("every name without a live lease", func(t *testing.T) {
		for _, refused := range []struct {
			name    string
			request any
			context map[string]any
		}{
			{productionReviewRevisionPrepare, prepareRequest, stale},
			{productionReviewRevisionCommitReview, reviewRequest, stale},
			{productionReviewRevisionCommitRevision, revisionRequest, stale},
			{productionReviewRevisionReadRecovery, map[string]any{"handle": handle}, stale},
			{productionReviewRevisionPrepare, prepareRequest, unleased},
			{productionReviewRevisionCommitReview, reviewRequest, unleased},
			{productionReviewRevisionCommitRevision, revisionRequest, unleased},
			{productionReviewRevisionReadRecovery, map[string]any{"handle": handle}, unleased},
		} {
			productionReviewLaneRefusal(t, author, database, before, projectPath, refused.name,
				[]any{refused.request, projectPath, refused.context})
		}
		// The write names stay writes even while refused: nothing was added to the
		// tables a commit would reach.
		reviews := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			"SELECT COUNT(*) AS total FROM reviews"), "total")
		revisions := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			"SELECT COUNT(*) AS total FROM revisions"), "total")
		contents := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			"SELECT COUNT(*) AS total FROM contents"), "total")
		if reviews != 1 || revisions != 2 || contents != 5 {
			t.Fatalf("the refused writes moved rows: %d reviews, %d revisions, %d contents; want 1/2/5",
				reviews, revisions, contents)
		}
	})

	t.Run("the binder installs nothing", func(t *testing.T) {
		if err := bindProductionReviewRevision(nil); err == nil {
			t.Fatal("bindProductionReviewRevision accepted a nil server")
		}
		if err := bindProductionReviewRevision(productionReviewLaneServer(t, handler)); err != nil {
			t.Fatalf("bindProductionReviewRevision refused a live server: %v", err)
		}
		served := productionLaneBoundOperations(t, handler)
		for _, operation := range productionReviewRevisionOperations() {
			if _, found := migrationRegistry(t).Operations[operation]; !found {
				t.Fatalf("%s is not an inventoried operation, so the 501 is a 404 waiting to happen", operation)
			}
			for _, bound := range served {
				if bound == operation {
					t.Fatalf("%s is served by this lane, so productionServedOperations has to move off 158", operation)
				}
			}
		}
	})
}

// productionReviewLaneServer is the operation server the built lane really mounted
// at /api/, so the binder's nil-server guard is proved against the lane's own
// wiring instead of a second server built here.
func productionReviewLaneServer(t *testing.T, handler http.Handler) *transport.Server {
	t.Helper()
	mux, ok := handler.(*http.ServeMux)
	if !ok {
		t.Fatalf("the production lane returned %T, not the route table it builds", handler)
	}
	mounted, pattern := mux.Handler(&http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/api/"}})
	server, ok := mounted.(*transport.Server)
	if !ok {
		t.Fatalf("the production route table mounts %T at %s, not the operation server", mounted, pattern)
	}
	return server
}
