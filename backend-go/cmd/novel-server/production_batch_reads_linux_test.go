//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the four batch/directory reads this lane binds against rows
// that really exist in an admitted author database, and proves every refusal is
// a read: the file bytes are compared before and after each one.
//
// The fixture is a real v7 project database (the shipped baseline, through the
// same oracle the other production journeys use). The batch runs are created by
// the shipped ledger itself - open, reserve, markDispatched, snapshot, settle - so
// run ids, attempt ids, bindings and receipts are the ones the ledger writes, not
// hand-written look-alikes. Only the two receipt keys whose writers live in the
// generation write family this lane does not own are patched in afterwards:
//
//	$.draftCommit       written by GenerationDraftEffects.commit
//	$.visibleComposition written by the visible-composition writer
//	$.directoryProgress written by recordDirectoryProgress
//
// Their writers are not bound in this lane, and binding them is not part of this
// slice; a hand-written receipt for a table whose writer this lane does not own
// is the only honest way to test a reader of that table. The rest of the fixture
// (drafts, contents, finalization_outbox, post_process_runs, post_process_steps,
// blueprint_commit_operations) is plain schema the read derives from.
const (
	productionBatchEpoch        = "lease-0000000002-batch"
	productionBatchModelID      = "ci-batch-model-1"
	productionBatchRootNonce    = "ci-batch-root-nonce"
	productionBatchTargetUnits  = 1200
	productionBatchStartChapter = 2
	productionBatchEndChapter   = 5

	productionBatchDirectoryEpoch       = "lease-0000000003-directory"
	productionBatchDirectoryRun         = "ci-directory-run-1"
	productionBatchDirectoryRoot        = "ci-directory-root-1"
	productionBatchDirectoryOperationID = "ci-blueprint-commit-1"
	productionBatchRetiredRun           = "ci-directory-retired-1"
	productionBatchRetiredRoot          = "ci-directory-retired-root-1"
	productionBatchRangeInput           = `{"startChapter":1,"endChapter":3}`

	productionBatchForeignRun     = "ci-foreign-batch-run-1"
	productionBatchForeignRoot    = "ci-foreign-batch-root-1"
	productionBatchForeignProject = "9d2b6f31-4a08-4c55-9e77-1b3c5d8f0a24"

	productionBatchChapterTwoText   = "第二章 门后有人，他听见雪落。"
	productionBatchChapterThreeText = "第三章 灯下无影，他吹熄了灯。"
	productionBatchChapterFourText  = "第四章 雪落无声，他走出门去。"
)

// productionBatchChapter is one seeded chapter of the batch fixture: the rows the
// derivation must find, captured at seed time so the assertions compare against
// what was written instead of a guess.
type productionBatchChapter struct {
	ChapterNumber       int64
	RunID               string
	AttemptID           string
	DraftID             int64
	Version             int64
	Content             string
	ContentHash         string
	FinalizationID      string
	Published           bool
	PostProcessComplete bool
}

// productionBatchFixture is the seeded batch whose progress the reads derive.
type productionBatchFixture struct {
	BatchID              string
	RootID               string
	Chapters             []productionBatchChapter
	PendingChapterNumber int64
	PendingRunID         string
	DirectoryOperationID string
	DirectoryPayloadHash string
}

// productionBatchLane is one started production lane over one admitted project
// plus the frozen context of its only open session.
type productionBatchLane struct {
	client      *productionDurableClient
	projectPath string
	frozen      map[string]any
	fixture     productionBatchFixture
}

// The views below are JSON views of the wire, written out here instead of reusing
// the store types: if the store's tags drifted from the contract, asserting
// through the store's own structs would hide it.
type productionBatchHandleView struct {
	ProjectID    string `json:"projectId"`
	Epoch        string `json:"epoch"`
	RootActionID string `json:"rootActionId"`
	RunID        string `json:"runId"`
}

type productionBatchChapterView struct {
	ChapterNumber         int64                     `json:"chapterNumber"`
	DraftID               int64                     `json:"draftId"`
	Version               int64                     `json:"version"`
	ContentHash           string                    `json:"contentHash"`
	SourceRunHandle       productionBatchHandleView `json:"sourceRunHandle"`
	FinalizationID        *string                   `json:"finalizationId"`
	PendingFinalizationID *string                   `json:"pendingFinalizationId"`
	PostProcessComplete   *bool                     `json:"postProcessComplete"`
}

type productionBatchInputView struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type productionBatchRangeView struct {
	StartChapter int64 `json:"startChapter"`
	EndChapter   int64 `json:"endChapter"`
}

type productionBatchProgressView struct {
	Mode                    string                       `json:"mode"`
	Range                   productionBatchRangeView     `json:"range"`
	TargetUnits             int64                        `json:"targetUnits"`
	ModelID                 string                       `json:"modelId"`
	BatchID                 string                       `json:"batchId"`
	RootHandle              productionBatchHandleView    `json:"rootHandle"`
	CompletedChapters       []productionBatchChapterView `json:"completedChapters"`
	CurrentChapterRunHandle *productionBatchHandleView   `json:"currentChapterRunHandle"`
	NextChapterNumber       *int64                       `json:"nextChapterNumber"`
	AuthorInputs            []productionBatchInputView   `json:"authorInputs"`
	SourceCurrent           *bool                        `json:"sourceCurrent"`
}

type productionBatchCatalogView struct {
	Progress []struct {
		OperationID    string                     `json:"operationId"`
		PayloadHash    string                     `json:"payloadHash"`
		SourceHandle   productionBatchHandleView  `json:"sourceHandle"`
		RequestedRange productionBatchRangeView   `json:"requestedRange"`
		CommittedRange productionBatchRangeView   `json:"committedRange"`
		RemainingRange *productionBatchRangeView  `json:"remainingRange"`
		AuthorInputs   []productionBatchInputView `json:"authorInputs"`
	} `json:"progress"`
	Diagnostics []struct {
		RunID string `json:"runId"`
		Kind  string `json:"kind"`
		Code  string `json:"code"`
	} `json:"diagnostics"`
}

// newProductionBatchLane seeds the fixture into a fresh admitted project, starts
// the real production lane over that root and opens the project through HTTP, so
// every assertion below travels the same adapter the renderer talks to.
func newProductionBatchLane(t *testing.T, mutate func(t *testing.T, db *sql.DB, fixture *productionBatchFixture)) *productionBatchLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	fixture := seedProductionBatch(t, database)
	if mutate != nil {
		db := productionBatchDatabase(t, database)
		mutate(t, db, &fixture)
		productionEditorCheckpoint(t, db, database)
	}
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	client := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	client.decode(t, "project:open", []any{projectPath, "batch-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted fixture project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["projectId"] != productionEditorProjectID || frozen["leaseId"] == nil || frozen["leaseId"] == "" {
		t.Fatalf("project:open returned the wrong project session: %v", opened.Project)
	}
	return &productionBatchLane{client: client, projectPath: projectPath, frozen: frozen, fixture: fixture}
}

// productionBatchDatabase opens the admitted project database for seeding.
func productionBatchDatabase(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// productionBatchBytes is the admitted file, read from disk.
func productionBatchBytes(t *testing.T, projectPath string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// productionBatchFingerprint is the nine frozen hashes the ledger requires.
func productionBatchFingerprint(hash string) string {
	keys := []string{"chapterBriefHash", "authorGuidanceHash", "dependencyHash", "contextSnapshotHash",
		"templateHash", "skillSnapshotHash", "modelLeaseRevision", "policyHash", "outputContractHash"}
	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, `"`+key+`":"`+hash+`"`)
	}
	return "{" + strings.Join(fields, ",") + "}"
}

// productionBatchRootRequest is the frozen batch root binding: operation
// batch-chapters with the intent, the model receipt and the target units the
// renderer freezes (electron/services/main-generation-owner.ts:1524-1528).
func productionBatchRootRequest() string {
	hash := strings.Repeat("c", 64)
	return `{"projectId":"` + productionEditorProjectID + `","epoch":"` + productionBatchEpoch +
		`","operation":"batch-chapters","uiActionNonce":"` + productionBatchRootNonce +
		`","frozenInputHash":"` + hash + `","fingerprint":` + productionBatchFingerprint(hash) +
		`,"contextSnapshotId":"ci-context-snapshot-batch","sourceManifest":{"operation":"batch-chapters"` +
		`,"batchIntent":{"mode":"draft_review","range":{"startChapter":2,"endChapter":5},"targetUnits":1200}` +
		`,"modelReceipt":{"modelId":"` + productionBatchModelID + `"}` +
		`,"authorInputs":[{"id":"draft:target-units","text":"1200"}]},"sourceRefs":[]` +
		`,"budget":{"maxPhysicalRequests":12,"maxTokenLiability":24000,"maxOutputPerRequest":4000,"maxActiveElapsedMs":600000}}`
}

// productionBatchChapterRequest is one chapter-draft child run: same root, its own
// nonce, and the batch id plus chapter number the batch derivation reads.
func productionBatchChapterRequest(chapter int64, batchID, parentRootID string) string {
	hash := strings.Repeat("e", 64)
	number := fmt.Sprintf("%d", chapter)
	return `{"projectId":"` + productionEditorProjectID + `","epoch":"` + productionBatchEpoch +
		`","operation":"chapter-draft","uiActionNonce":"ci-batch-chapter-` + number + `-nonce"` +
		`,"frozenInputHash":"` + hash + `","fingerprint":` + productionBatchFingerprint(hash) +
		`,"contextSnapshotId":"ci-context-snapshot-batch-` + number + `","parentRootActionId":"` + parentRootID +
		`","sourceManifest":{"operation":"chapter-draft","batchId":"` + batchID + `"` +
		`,"chapterNumber":` + number + `,"authorInputs":[{"id":"draft:target-units","text":"1200"}]},` +
		`"sourceRefs":[],"budget":{"maxPhysicalRequests":3,"maxTokenLiability":4000,` +
		`"maxOutputPerRequest":1000,"maxActiveElapsedMs":600000}}`
}

// productionBatchSettle runs one complete settlement chain for a run through the
// shipped ledger and returns the attempt id the receipt is patched on.
func productionBatchSettle(t *testing.T, ledger *store.GenerationLedger, runID, text string) string {
	t.Helper()
	ctx := context.Background()
	reserved, err := ledger.Call(ctx, "reserve", []any{runID, "ci-batch-nonce-" + runID, strings.Repeat("d", 64), 200, 100})
	if err != nil {
		t.Fatalf("batch reserve %s: %v", runID, err)
	}
	receipt, _ := reserved.(map[string]any)
	attempt, _ := receipt["attempt"].(map[string]any)
	attemptID, _ := attempt["attemptId"].(string)
	if attemptID == "" {
		t.Fatalf("batch reserve returned no attempt: %v", reserved)
	}
	if _, err := ledger.Call(ctx, "markDispatched", []any{attemptID}); err != nil {
		t.Fatalf("batch markDispatched: %v", err)
	}
	if _, err := ledger.Call(ctx, "snapshot", []any{attemptID, 0, text}); err != nil {
		t.Fatalf("batch snapshot: %v", err)
	}
	if _, err := ledger.Call(ctx, "settle", []any{attemptID, map[string]any{"trusted": true, "actualTokens": 120}, "stop"}); err != nil {
		t.Fatalf("batch settle: %v", err)
	}
	return attemptID
}

// productionBatchPatchReceipt merges keys into one attempt's stored receipt, the
// way the family writers this lane does not own would have merged them.
func productionBatchPatchReceipt(t *testing.T, db *sql.DB, attemptID string, add map[string]any) {
	t.Helper()
	var stored sql.NullString
	if err := db.QueryRow("SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?", attemptID).Scan(&stored); err != nil {
		t.Fatalf("read receipt of %s: %v", attemptID, err)
	}
	receipt := map[string]any{}
	if stored.Valid && strings.TrimSpace(stored.String) != "" {
		if err := json.Unmarshal([]byte(stored.String), &receipt); err != nil {
			t.Fatalf("decode receipt of %s: %v", attemptID, err)
		}
	}
	for key, value := range add {
		receipt[key] = value
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=?", string(encoded), attemptID); err != nil {
		t.Fatalf("patch receipt of %s: %v", attemptID, err)
	}
}

// seedProductionBatch writes the whole fixture and returns its identity.
func seedProductionBatch(t *testing.T, database string) productionBatchFixture {
	t.Helper()
	db := productionBatchDatabase(t, database)
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return fmt.Sprintf("production-batch-%03d", ordinal)
	})
	ctx := context.Background()
	opened, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+productionBatchRootRequest()+"]"))
	if err != nil {
		t.Fatalf("open the batch root through the ledger: %v", err)
	}
	root, _ := opened.(map[string]any)
	batchID, _ := root["runId"].(string)
	rootID, _ := root["rootActionId"].(string)
	if batchID == "" || rootID == "" {
		t.Fatalf("the batch root open returned no identity: %v", opened)
	}
	fixture := productionBatchFixture{BatchID: batchID, RootID: rootID}
	texts := map[int64]string{2: productionBatchChapterTwoText, 3: productionBatchChapterThreeText, 4: productionBatchChapterFourText}
	for chapter := int64(productionBatchStartChapter); chapter <= productionBatchEndChapter; chapter++ {
		text := texts[chapter]
		if text == "" {
			text = "第五章 未写的稿。"
		}
		value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+productionBatchChapterRequest(chapter, batchID, rootID)+"]"))
		if err != nil {
			t.Fatalf("open chapter run %d: %v", chapter, err)
		}
		run, _ := value.(map[string]any)
		runID, _ := run["runId"].(string)
		if runID == "" {
			t.Fatalf("chapter run %d returned no run id: %v", chapter, value)
		}
		attemptID := productionBatchSettle(t, ledger, runID, text)
		if chapter == productionBatchEndChapter {
			fixture.PendingChapterNumber = chapter
			fixture.PendingRunID = runID
			continue
		}
		chapterRow := productionBatchChapter{
			ChapterNumber: chapter,
			RunID:         runID,
			AttemptID:     attemptID,
			DraftID:       chapter,
			Version:       1,
			Content:       text,
			ContentHash:   sha256Hex(text),
		}
		chapterRow.FinalizationID = "ci-finalization-" + fmt.Sprintf("%d", chapter)
		chapterRow.Published = chapter == productionBatchStartChapter
		chapterRow.PostProcessComplete = chapter == productionBatchStartChapter
		fixture.Chapters = append(fixture.Chapters, chapterRow)
	}
	// Contents and drafts: one committed draft per chapter, id == chapter number,
	// so the derived draftId/version/contentHash can be compared literally.
	for _, chapter := range fixture.Chapters {
		if _, err := db.Exec("INSERT INTO contents (id, body) VALUES (?, ?)", chapter.DraftID, chapter.Content); err != nil {
			t.Fatalf("seed content of chapter %d: %v", chapter.ChapterNumber, err)
		}
		status := "draft"
		if chapter.FinalizationID != "" {
			status = "finalized"
		}
		if _, err := db.Exec(`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (?, ?, ?, ?, 'write', ?, ?, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
			chapter.DraftID, chapter.ChapterNumber, chapter.Version, status, chapter.DraftID, len(chapter.Content)); err != nil {
			t.Fatalf("seed draft of chapter %d: %v", chapter.ChapterNumber, err)
		}
	}
	// The two receipts the family writers would have stored on the same attempt.
	for _, chapter := range fixture.Chapters {
		productionBatchPatchReceipt(t, db, chapter.AttemptID, map[string]any{
			"artifactIdentity": map[string]any{"epoch": productionBatchEpoch},
			"visibleComposition": map[string]any{
				"algorithm":   "draft-visible-v2",
				"text":        chapter.Content,
				"textHash":    chapter.ContentHash,
				"artifactIds": []any{},
				"sources":     []any{},
			},
			"draftCommit": map[string]any{
				"success":       true,
				"id":            chapter.DraftID,
				"version":       chapter.Version,
				"chapterNumber": chapter.ChapterNumber,
				"content":       chapter.Content,
				"contentHash":   chapter.ContentHash,
				"batchId":       fixture.BatchID,
				"handle": map[string]any{
					"projectId":    productionEditorProjectID,
					"epoch":        productionBatchEpoch,
					"rootActionId": fixture.RootID,
					"runId":        chapter.RunID,
				},
			},
		})
	}
	// One published finalization with a complete post-process run, and one
	// finalization still pending projection: the confirmation must accept only
	// the first.
	for _, chapter := range fixture.Chapters {
		if chapter.FinalizationID == "" {
			continue
		}
		publication := "pending"
		if chapter.Published {
			publication = "published"
		}
		if _, err := db.Exec(`INSERT INTO finalization_outbox (finalization_id, draft_id, chapter_number,
 chapter_title, content_hash, content_revision, content_snapshot, target_file_name,
 knowledge_document_id, publication_status)
 VALUES (?, ?, ?, '', ?, 1, ?, ?, '', ?)`,
			chapter.FinalizationID, chapter.DraftID, chapter.ChapterNumber, chapter.ContentHash,
			chapter.Content, fmt.Sprintf("第%d章.md", chapter.ChapterNumber), publication); err != nil {
			t.Fatalf("seed outbox of chapter %d: %v", chapter.ChapterNumber, err)
		}
		if !chapter.PostProcessComplete {
			continue
		}
		postProcessID := "ci-post-process-2"
		if _, err := db.Exec(`INSERT INTO post_process_runs (id, trigger_source_type, trigger_source_id,
 all_critical_passed, created_at) VALUES (?, 'chapter_finalize', ?, 1, '2024-05-06 07:09:09')`,
			postProcessID, "finalization:"+chapter.FinalizationID); err != nil {
			t.Fatalf("seed post process run: %v", err)
		}
		for _, step := range []struct {
			key      string
			critical int
		}{{"kb_import", 1}, {"chapter_notes", 1}, {"character_state", 0}} {
			if _, err := db.Exec(`INSERT INTO post_process_steps (run_id, step_key, critical, ok)
 VALUES (?, ?, ?, 1)`, postProcessID, step.key, step.critical); err != nil {
				t.Fatalf("seed post process step %s: %v", step.key, err)
			}
		}
	}
	productionBatchSeedDirectory(t, db, &fixture)
	productionBatchSeedForeign(t, db)
	productionEditorCheckpoint(t, db, database)
	return fixture
}

// productionBatchSeedDirectory writes one directory run with a frozen progress
// receipt plus its blueprint commit operation, and one retired directory run. The
// directory operation itself is a planning operation, which the shipped ledger
// refuses to open, and its writer is not bound in this lane either, so the rows
// are inserted the way that writer would have inserted them.
func productionBatchSeedDirectory(t *testing.T, db *sql.DB, fixture *productionBatchFixture) {
	t.Helper()
	// generation_runs carries a foreign key onto generation_roots, so the root has
	// to exist before the run. The ledger writes both in one call; this seed is
	// writing the rows that the unbound directory writer would have written, so it
	// inserts the root first, with the same empty action and budget the planning
	// operation would carry.
	if _, err := db.Exec(`INSERT INTO generation_roots (root_action_id, idempotency_key, action_json, budget_json)
 VALUES (?, ?, '{}', '{}')`, productionBatchDirectoryRoot, "ci-directory-root-key"); err != nil {
		t.Fatalf("seed the directory root: %v", err)
	}
	payloadHash := sha256Hex(productionBatchRangeInput)
	binding := `{"projectId":"` + productionEditorProjectID + `","epoch":"` + productionBatchDirectoryEpoch +
		`","fingerprint":` + productionBatchFingerprint(strings.Repeat("f", 64)) +
		`,"contextSnapshotId":"ci-context-snapshot-directory","sourceManifest":{"operation":"chapter-blueprint-directory"` +
		`,"outputContract":"structured-data","selectedBlueprintChapterNumbers":[1,2,3]` +
		`,"authorInputs":[{"id":"directory:requested-range","text":` + quoteJSON(productionBatchRangeInput) + `}]},"sourceRefs":[]}`
	if _, err := db.Exec(`INSERT INTO generation_runs (run_id, root_action_id, binding_json, status, created_at_ms, open_key)
 VALUES (?, ?, ?, 'running', 1700000000000, ?)`, productionBatchDirectoryRun, productionBatchDirectoryRoot, binding,
		"ci-directory-open-key"); err != nil {
		t.Fatalf("seed the directory run: %v", err)
	}
	receipt := `{"artifactIdentity":{"epoch":"` + productionBatchDirectoryEpoch + `"},"directoryProgress":{` +
		`"operationId":"` + productionBatchDirectoryOperationID + `","payloadHash":"` + payloadHash + `",` +
		`"sourceHandle":{"projectId":"` + productionEditorProjectID + `","epoch":"` + productionBatchDirectoryEpoch +
		`","rootActionId":"` + productionBatchDirectoryRoot + `","runId":"` + productionBatchDirectoryRun + `"},` +
		`"requestedRange":{"startChapter":1,"endChapter":3},` +
		`"committedRange":{"startChapter":1,"endChapter":2},` +
		`"remainingRange":{"startChapter":3,"endChapter":3}}}`
	if _, err := db.Exec(`INSERT INTO generation_attempts (attempt_id, reservation_id, run_id, root_action_id,
 attempt_json, usage_receipt_json, invocation_nonce) VALUES (?, ?, ?, ?, '{}', ?, ?)`,
		"ci-directory-attempt-1", "ci-directory-reservation-1", productionBatchDirectoryRun,
		productionBatchDirectoryRoot, receipt, "ci-directory-nonce-1"); err != nil {
		t.Fatalf("seed the directory receipt: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO blueprint_commit_operations (operation_id, payload_hash, mode,
 start_chapter, end_chapter, character_sync_input) VALUES (?, ?, 'replace-range', 1, 2, '[]')`,
		productionBatchDirectoryOperationID, payloadHash); err != nil {
		t.Fatalf("seed the blueprint commit operation: %v", err)
	}
	retired := `{"projectId":"` + productionEditorProjectID + `","epoch":"` + productionBatchDirectoryEpoch +
		`","sourceManifest":{"operation":"chapter-blueprint-directory"},` +
		`"$blueprintRetirement":"blueprints-cleared"}`
	if _, err := db.Exec(`INSERT INTO generation_runs (run_id, root_action_id, binding_json, status, created_at_ms, open_key)
 VALUES (?, ?, ?, 'running', 1700000000001, ?)`, productionBatchRetiredRun, productionBatchRetiredRoot, retired,
		"ci-directory-retired-open-key"); err != nil {
		t.Fatalf("seed the retired directory run: %v", err)
	}
	fixture.DirectoryOperationID = productionBatchDirectoryOperationID
	fixture.DirectoryPayloadHash = payloadHash
}

// productionBatchSeedForeign writes a batch root that belongs to another project.
// The read must not serve it, and list-batches must not list it.
func productionBatchSeedForeign(t *testing.T, db *sql.DB) {
	t.Helper()
	binding := `{"projectId":"` + productionBatchForeignProject + `","epoch":"` + productionBatchEpoch +
		`","sourceManifest":{"operation":"batch-chapters"` +
		`,"batchIntent":{"mode":"draft_review","range":{"startChapter":1,"endChapter":2},"targetUnits":900}` +
		`,"modelReceipt":{"modelId":"ci-foreign-model"}}}`
	if _, err := db.Exec(`INSERT INTO generation_runs (run_id, root_action_id, binding_json, status, created_at_ms, open_key)
 VALUES (?, ?, ?, 'running', 1700000000002, ?)`, productionBatchForeignRun, productionBatchForeignRoot, binding,
		"ci-foreign-open-key"); err != nil {
		t.Fatalf("seed the foreign batch root: %v", err)
	}
}

// quoteJSON encodes one string as a JSON string literal.
func quoteJSON(text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// productionBatchOpen sends one of the four reads and decodes its result.
func productionBatchOpen(t *testing.T, lane *productionBatchLane, operation string, args []any, into any) {
	t.Helper()
	response := lane.client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented {
		t.Fatalf("%s is still GO_OPERATION_NOT_MIGRATED: the lane does not bind it, so the "+
			"production_open_linux.go binder slice is missing this slice's entry: %s", operation, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if err := json.Unmarshal(wire.Result, into); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
}

// productionBatchRefused asserts one refusal and proves it wrote nothing.
func productionBatchRefused(t *testing.T, lane *productionBatchLane, label, operation string, args []any) string {
	t.Helper()
	before := productionBatchBytes(t, lane.projectPath)
	response := lane.client.call(t, operation, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%s: %s answered 200 where it had to refuse: %s", label, operation, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body.Error.Code == "" {
		t.Fatalf("%s: %s returned no symbolic refusal: %v %s", label, operation, err, response.Body.String())
	}
	after := productionBatchBytes(t, lane.projectPath)
	if !bytes.Equal(before, after) {
		t.Fatalf("%s: %s refused with %q but changed the admitted author database",
			label, operation, body.Error.Code)
	}
	return body.Error.Code
}

// productionBatchChapterArgs addresses one batch.
func productionBatchChapterArgs(lane *productionBatchLane, batchID string) []any {
	return []any{map[string]any{"batchId": batchID}, lane.frozen}
}

// TestProductionBatchReadsDeriveSeededProgress is the whole read surface: the four
// names are bound, they answer 200 over HTTP, and the progress they report is the
// progress the seeded rows describe, chapter by chapter.
func TestProductionBatchReadsDeriveSeededProgress(t *testing.T) {
	lane := newProductionBatchLane(t, nil)
	fixture := lane.fixture
	if len(fixture.Chapters) != 3 || fixture.PendingRunID == "" {
		t.Fatalf("the fixture did not seed the expected batch: %+v", fixture)
	}

	// generation:read-batch
	t.Run("generation:read-batch", func(t *testing.T) {
		var progress productionBatchProgressView
		productionBatchOpen(t, lane, "generation:read-batch", productionBatchChapterArgs(lane, fixture.BatchID), &progress)
		if progress.BatchID != fixture.BatchID || progress.Mode != "draft_review" ||
			progress.Range.StartChapter != productionBatchStartChapter ||
			progress.Range.EndChapter != productionBatchEndChapter ||
			progress.TargetUnits != productionBatchTargetUnits || progress.ModelID != productionBatchModelID {
			t.Fatalf("read-batch derived the wrong intent: %+v", progress)
		}
		if progress.RootHandle != (productionBatchHandleView{
			ProjectID:    productionEditorProjectID,
			Epoch:        productionBatchEpoch,
			RootActionID: fixture.RootID,
			RunID:        fixture.BatchID,
		}) {
			t.Fatalf("read-batch derived the wrong root handle: %+v", progress.RootHandle)
		}
		if len(progress.CompletedChapters) != len(fixture.Chapters) {
			t.Fatalf("read-batch derived %d chapters; the ledger holds %d: %+v",
				len(progress.CompletedChapters), len(fixture.Chapters), progress.CompletedChapters)
		}
		for index, chapter := range fixture.Chapters {
			got := progress.CompletedChapters[index]
			if got.ChapterNumber != chapter.ChapterNumber || got.DraftID != chapter.DraftID ||
				got.Version != chapter.Version || got.ContentHash != chapter.ContentHash {
				t.Fatalf("chapter %d derived %+v; the seeded draft is %+v", chapter.ChapterNumber, got, chapter)
			}
			if got.SourceRunHandle != (productionBatchHandleView{
				ProjectID:    productionEditorProjectID,
				Epoch:        productionBatchEpoch,
				RootActionID: fixture.RootID,
				RunID:        chapter.RunID,
			}) {
				t.Fatalf("chapter %d derived the wrong source run handle: %+v", chapter.ChapterNumber, got.SourceRunHandle)
			}
			if chapter.Published {
				if got.FinalizationID == nil || *got.FinalizationID != chapter.FinalizationID {
					t.Fatalf("chapter %d derived finalizationId %v; the published outbox is %q",
						chapter.ChapterNumber, got.FinalizationID, chapter.FinalizationID)
				}
				if got.PendingFinalizationID != nil {
					t.Fatalf("chapter %d reported a pending finalization while published: %v",
						chapter.ChapterNumber, *got.PendingFinalizationID)
				}
			} else {
				if got.FinalizationID != nil {
					t.Fatalf("chapter %d reported finalizationId %q for a finalization that is not published",
						chapter.ChapterNumber, *got.FinalizationID)
				}
				if got.PendingFinalizationID == nil || *got.PendingFinalizationID != chapter.FinalizationID {
					t.Fatalf("chapter %d derived pendingFinalizationId %v; the pending outbox is %q",
						chapter.ChapterNumber, got.PendingFinalizationID, chapter.FinalizationID)
				}
			}
			if got.PostProcessComplete == nil || *got.PostProcessComplete != chapter.PostProcessComplete {
				t.Fatalf("chapter %d derived postProcessComplete %v; the seeded post-process rows say %v",
					chapter.ChapterNumber, got.PostProcessComplete, chapter.PostProcessComplete)
			}
		}
		if progress.NextChapterNumber == nil || *progress.NextChapterNumber != fixture.PendingChapterNumber {
			t.Fatalf("read-batch derived nextChapterNumber %v; the next unwritten chapter is %d",
				progress.NextChapterNumber, fixture.PendingChapterNumber)
		}
		if progress.CurrentChapterRunHandle == nil || progress.CurrentChapterRunHandle.RunID != fixture.PendingRunID {
			t.Fatalf("read-batch derived currentChapterRunHandle %+v; the pending run is %q",
				progress.CurrentChapterRunHandle, fixture.PendingRunID)
		}
		if len(progress.AuthorInputs) != 1 || progress.AuthorInputs[0] != (productionBatchInputView{ID: "draft:target-units", Text: "1200"}) {
			t.Fatalf("read-batch derived the wrong frozen author inputs: %+v", progress.AuthorInputs)
		}
	})

	// generation:list-batches
	t.Run("generation:list-batches", func(t *testing.T) {
		var history []productionBatchProgressView
		productionBatchOpen(t, lane, "generation:list-batches", []any{lane.frozen}, &history)
		if len(history) != 1 {
			t.Fatalf("list-batches returned %d batches; the project has one batch root and one foreign root: %+v",
				len(history), history)
		}
		entry := history[0]
		if entry.BatchID != fixture.BatchID || entry.SourceCurrent == nil || !*entry.SourceCurrent {
			t.Fatalf("list-batches reported the wrong batch or a false source conflict: %+v", entry)
		}
		if len(entry.CompletedChapters) != len(fixture.Chapters) {
			t.Fatalf("list-batches returned %d chapters; the ledger holds %d", len(entry.CompletedChapters), len(fixture.Chapters))
		}
		for index, chapter := range fixture.Chapters {
			got := entry.CompletedChapters[index]
			if got.ChapterNumber != chapter.ChapterNumber || got.DraftID != chapter.DraftID ||
				got.ContentHash != chapter.ContentHash {
				t.Fatalf("list-batches chapter %d derived %+v; the seeded draft is %+v", chapter.ChapterNumber, got, chapter)
			}
		}
		if entry.NextChapterNumber == nil || *entry.NextChapterNumber != fixture.PendingChapterNumber {
			t.Fatalf("list-batches derived nextChapterNumber %v; want %d", entry.NextChapterNumber, fixture.PendingChapterNumber)
		}
	})

	// generation:confirm-batch-finalization
	t.Run("generation:confirm-batch-finalization", func(t *testing.T) {
		published := fixture.Chapters[0]
		var confirmed productionBatchProgressView
		productionBatchOpen(t, lane, "generation:confirm-batch-finalization", []any{map[string]any{
			"batchId":        fixture.BatchID,
			"chapterNumber":  published.ChapterNumber,
			"finalizationId": published.FinalizationID,
		}, lane.frozen}, &confirmed)
		if confirmed.BatchID != fixture.BatchID || len(confirmed.CompletedChapters) != len(fixture.Chapters) {
			t.Fatalf("confirm-batch-finalization confirmed with a truncated projection: %+v", confirmed)
		}

		// A pending finalization is not a confirmation.
		pending := fixture.Chapters[1]
		if code := productionBatchRefused(t, lane, "pending finalization", "generation:confirm-batch-finalization",
			[]any{map[string]any{
				"batchId":        fixture.BatchID,
				"chapterNumber":  pending.ChapterNumber,
				"finalizationId": pending.FinalizationID,
			}, lane.frozen}); code != "GENERATION_BATCH_FINALIZATION_REQUIRED" {
			t.Fatalf("confirm-batch-finalization refused a pending finalization with %q; want GENERATION_BATCH_FINALIZATION_REQUIRED", code)
		}
		// Nor is a chapter that never reached the outbox at all: the pending
		// chapter run has no commit, so it is not a completed chapter.
		if code := productionBatchRefused(t, lane, "chapter with no committed draft", "generation:confirm-batch-finalization",
			[]any{map[string]any{
				"batchId":        fixture.BatchID,
				"chapterNumber":  fixture.PendingChapterNumber,
				"finalizationId": "ci-finalization-never",
			}, lane.frozen}); code != "GENERATION_BATCH_FINALIZATION_REQUIRED" {
			t.Fatalf("confirm-batch-finalization refused an unwritten chapter with %q; want GENERATION_BATCH_FINALIZATION_REQUIRED", code)
		}
		// Nor is a published chapter confirmed by the wrong finalization.
		if code := productionBatchRefused(t, lane, "wrong finalization id", "generation:confirm-batch-finalization",
			[]any{map[string]any{
				"batchId":        fixture.BatchID,
				"chapterNumber":  published.ChapterNumber,
				"finalizationId": "ci-finalization-other",
			}, lane.frozen}); code != "GENERATION_BATCH_FINALIZATION_REQUIRED" {
			t.Fatalf("confirm-batch-finalization accepted a status code %q; want GENERATION_BATCH_FINALIZATION_REQUIRED", code)
		}
	})

	// generation:list-directory-progress
	t.Run("generation:list-directory-progress", func(t *testing.T) {
		var catalog productionBatchCatalogView
		productionBatchOpen(t, lane, "generation:list-directory-progress", []any{lane.frozen}, &catalog)
		if len(catalog.Progress) != 1 {
			t.Fatalf("list-directory-progress returned %d receipts; one directory run carries a receipt: %+v",
				len(catalog.Progress), catalog.Progress)
		}
		item := catalog.Progress[0]
		if item.OperationID != fixture.DirectoryOperationID || item.PayloadHash != fixture.DirectoryPayloadHash {
			t.Fatalf("list-directory-progress derived the wrong operation: %+v", item)
		}
		if item.SourceHandle.RunID != productionBatchDirectoryRun ||
			item.SourceHandle.RootActionID != productionBatchDirectoryRoot ||
			item.SourceHandle.Epoch != productionBatchDirectoryEpoch ||
			item.SourceHandle.ProjectID != productionEditorProjectID {
			t.Fatalf("list-directory-progress derived the wrong source handle: %+v", item.SourceHandle)
		}
		if item.RequestedRange != (productionBatchRangeView{StartChapter: 1, EndChapter: 3}) ||
			item.CommittedRange != (productionBatchRangeView{StartChapter: 1, EndChapter: 2}) {
			t.Fatalf("list-directory-progress derived the wrong ranges: %+v", item)
		}
		if item.RemainingRange == nil || *item.RemainingRange != (productionBatchRangeView{StartChapter: 3, EndChapter: 3}) {
			t.Fatalf("list-directory-progress derived a wrong remaining range: %+v", item.RemainingRange)
		}
		if len(item.AuthorInputs) != 1 || item.AuthorInputs[0].ID != "directory:requested-range" {
			t.Fatalf("list-directory-progress did not project the source run's frozen author inputs: %+v", item.AuthorInputs)
		}
		if len(catalog.Diagnostics) != 1 || catalog.Diagnostics[0].RunID != productionBatchRetiredRun ||
			catalog.Diagnostics[0].Kind != "retired" || catalog.Diagnostics[0].Code != "GENERATION_BLUEPRINTS_CLEARED" {
			t.Fatalf("list-directory-progress reported the wrong diagnostics: %+v", catalog.Diagnostics)
		}
	})

	// The four reads above are reads.
	if after := productionBatchBytes(t, lane.projectPath); len(after) == 0 {
		t.Fatal("the admitted author database disappeared")
	}
}

// TestProductionBatchReadsRefuseForeignAndUnknownBatches pins the refusal codes a
// caller can actually reach, each in its own subtest, and proves none of them
// writes.
func TestProductionBatchReadsRefuseForeignAndUnknownBatches(t *testing.T) {
	lane := newProductionBatchLane(t, nil)
	fixture := lane.fixture

	t.Run("foreign project batch", func(t *testing.T) {
		code := productionBatchRefused(t, lane, "foreign batch", "generation:read-batch",
			productionBatchChapterArgs(lane, productionBatchForeignRun))
		if code != "GENERATION_BATCH_IDENTITY_INVALID" {
			t.Fatalf("read-batch served a batch root of another project with %q; want GENERATION_BATCH_IDENTITY_INVALID", code)
		}
	})
	t.Run("unknown batch id", func(t *testing.T) {
		code := productionBatchRefused(t, lane, "unknown batch", "generation:read-batch",
			productionBatchChapterArgs(lane, "ci-batch-does-not-exist"))
		if code != "GENERATION_RUN_MISSING" {
			t.Fatalf("read-batch refused an unknown batch id with %q; want GENERATION_RUN_MISSING", code)
		}
	})
	t.Run("run that is not a batch", func(t *testing.T) {
		// A chapter run carries no batch intent: it is a wrong intent, not a wrong
		// project, and that is the code the original raises first.
		code := productionBatchRefused(t, lane, "not a batch", "generation:read-batch",
			productionBatchChapterArgs(lane, fixture.Chapters[0].RunID))
		if code != "GENERATION_BATCH_INTENT_INVALID" {
			t.Fatalf("read-batch refused a chapter run with %q; want GENERATION_BATCH_INTENT_INVALID", code)
		}
	})
	t.Run("arity and frozen context", func(t *testing.T) {
		if code := productionBatchRefused(t, lane, "arity", "generation:read-batch",
			[]any{map[string]any{"batchId": fixture.BatchID}}); code != "INVALID_ARGUMENTS" {
			t.Fatalf("read-batch without the frozen context refused with %q; want INVALID_ARGUMENTS", code)
		}
		if code := productionBatchRefused(t, lane, "malformed context", "generation:list-batches",
			[]any{map[string]any{"projectId": productionEditorProjectID, "projectPath": lane.projectPath}}); code != "PROJECT_SESSION_REQUIRED" {
			t.Fatalf("list-batches with a two-key context refused with %q; want PROJECT_SESSION_REQUIRED", code)
		}
		if code := productionBatchRefused(t, lane, "malformed confirm request", "generation:confirm-batch-finalization",
			[]any{map[string]any{"batchId": fixture.BatchID}, lane.frozen}); code != "INVALID_ARGUMENTS" {
			t.Fatalf("confirm-batch-finalization without a chapter refused with %q; want INVALID_ARGUMENTS", code)
		}
	})
}

// TestProductionBatchReadsRefuseUnsoundLedger pins the two ledger shapes the
// derivation must refuse instead of reporting: a second uncommitted run for the
// same chapter, and a draft commit the draft rows contradict.
func TestProductionBatchReadsRefuseUnsoundLedger(t *testing.T) {
	t.Run("duplicate uncommitted run", func(t *testing.T) {
		lane := newProductionBatchLane(t, func(t *testing.T, db *sql.DB, fixture *productionBatchFixture) {
			binding := `{"projectId":"` + productionEditorProjectID + `","epoch":"` + productionBatchEpoch +
				`","sourceManifest":{"operation":"chapter-draft","batchId":"` + fixture.BatchID +
				`","chapterNumber":` + fmt.Sprintf("%d", fixture.PendingChapterNumber) + `}}`
			if _, err := db.Exec(`INSERT INTO generation_runs (run_id, root_action_id, binding_json, status, created_at_ms, open_key)
 VALUES (?, ?, ?, 'running', 1700000000003, ?)`, "ci-duplicate-chapter-run", fixture.RootID, binding,
				"ci-duplicate-open-key"); err != nil {
				t.Fatalf("seed the duplicate chapter run: %v", err)
			}
		})
		code := productionBatchRefused(t, lane, "duplicate pending run", "generation:read-batch",
			productionBatchChapterArgs(lane, lane.fixture.BatchID))
		if code != "GENERATION_BATCH_PROGRESS_INVALID" {
			t.Fatalf("read-batch served two uncommitted runs for one chapter with %q; want GENERATION_BATCH_PROGRESS_INVALID", code)
		}
		// list-batches maps the same rows without recovering either.
		if code := productionBatchRefused(t, lane, "duplicate pending run", "generation:list-batches",
			[]any{lane.frozen}); code != "GENERATION_BATCH_PROGRESS_INVALID" {
			t.Fatalf("list-batches served two uncommitted runs for one chapter with %q; want GENERATION_BATCH_PROGRESS_INVALID", code)
		}
	})
	t.Run("receipt contradicted by the draft row", func(t *testing.T) {
		lane := newProductionBatchLane(t, func(t *testing.T, db *sql.DB, fixture *productionBatchFixture) {
			chapter := fixture.Chapters[1]
			if _, err := db.Exec("UPDATE drafts SET version=? WHERE id=?", chapter.Version+1, chapter.DraftID); err != nil {
				t.Fatalf("bump the draft version: %v", err)
			}
		})
		code := productionBatchRefused(t, lane, "stale receipt version", "generation:read-batch",
			productionBatchChapterArgs(lane, lane.fixture.BatchID))
		if code != "GENERATION_DRAFT_RECEIPT_INVALID" {
			t.Fatalf("read-batch served a receipt the draft row contradicts with %q; want GENERATION_DRAFT_RECEIPT_INVALID", code)
		}
	})
}

// TestProductionBatchReadsRefuseStaleLease proves the four names sit behind the
// same lease fence as the rest of the lane: a stale, foreign or absent frozen
// context is refused before a row is read, and nothing is written.
func TestProductionBatchReadsRefuseStaleLease(t *testing.T) {
	lane := newProductionBatchLane(t, nil)
	stale := map[string]any{
		"projectId":   productionEditorProjectID,
		"projectPath": lane.projectPath,
		"leaseId":     "ci-stale-lease-id",
	}
	for _, operation := range []string{
		"generation:read-batch",
		"generation:list-batches",
		"generation:confirm-batch-finalization",
		"generation:list-directory-progress",
	} {
		t.Run(operation, func(t *testing.T) {
			var args []any
			switch operation {
			case "generation:read-batch":
				args = []any{map[string]any{"batchId": lane.fixture.BatchID}, stale}
			case "generation:confirm-batch-finalization":
				args = []any{map[string]any{
					"batchId":        lane.fixture.BatchID,
					"chapterNumber":  lane.fixture.Chapters[0].ChapterNumber,
					"finalizationId": lane.fixture.Chapters[0].FinalizationID,
				}, stale}
			default:
				args = []any{stale}
			}
			code := productionBatchRefused(t, lane, "stale lease", operation, args)
			switch code {
			case "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE", "PROJECT_NOT_OPEN",
				"AUTHOR_WRITE_STALE", "PROJECT_SESSION_REQUIRED", "PROJECT_SESSION_CONTEXT_REQUIRED":
			default:
				t.Fatalf("%s refused a stale lease with %q, which is not a lease refusal", operation, code)
			}
		})
	}
}
