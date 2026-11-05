//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the name production_draft_commit_linux.go binds,
// generation:commit-draft, one subtest per aspect of the operation.
//
// # Why the proof is split in two
//
// One subtest drives the real HTTP handler over a real admitted project, because
// that is where the binding, the admission fence, the run identity and the
// mutating envelope live. Every other subtest calls store.CommitGenerationDraft
// directly, over a database this test seeded and then reads back through a
// connection of its own, because the served transaction writes a draft from a
// DRAFT-VISIBLE composition receipt and this lane cannot mint one: the recomposer
// internal/composition/visible.go:450 refuses any algorithm but
// visible-append-v1, so generation:compose-visible - the only writer of that key -
// can never store a draft-visible receipt through Go. A commit of real prose is
// therefore unreachable over HTTP today, and pretending otherwise would mean
// asserting a 501 as if it were an answer. The store-level subtests use the same
// ledger writes the composition lane performs
// (production_composition_linux.go:590-612, ledger recordEffect), so the receipt
// they store is the receipt the reference stores, not a shape invented here.
//
// Nothing here dials and nothing here weakens a refusal: every refusal subtest
// compares the database file byte for byte across the refused call, so a refusal
// that wrote anything at all fails.

const (
	// productionDraftCommitEpoch is the frozen epoch of every run this file opens.
	// At store level there is no lease fence to satisfy, so a literal is honest;
	// the HTTP subtest uses a real live run instead.
	productionDraftCommitEpoch = "lease-0000000003-commit"
	// productionDraftCommitBatchTarget is the target units the batch fixture
	// freezes, and the target this file's batch chapter run must agree with.
	productionDraftCommitBatchTarget = "1200"
	// productionDraftCommitSentence is one line of prose the snapshot gate accepts:
	// no punctuation that counts, no whitespace, nothing the sanitiser rewrites.
	productionDraftCommitSentence = "雨落在青石板上。"
	productionDraftCommitNextLine = "风穿过长街尽头的灯笼。"
)

// productionDraftCommitProse is one artifact body: times repetitions of one
// sentence, i.e. seven Han code points per repetition, which is what the unit
// counter charges.
func productionDraftCommitProse(sentence string, times int) string {
	return strings.Repeat(sentence, times)
}

// productionDraftCommitComposed is the composition the served transaction must
// reproduce: the shipped port of the same fold
// (electron/repositories/generation-run-repository.ts:468-512), so the expectation
// is not a second implementation of the rule under test.
func productionDraftCommitComposed(texts []string) string {
	text, _ := generation.ComposeDraftVisibleSources(texts, generation.DraftVisibleTextVersion)
	return text
}

// productionDraftCommitFixture is one disposable admitted project root, one
// writable handle on its author database and the shipped ledger over it.
type productionDraftCommitFixture struct {
	projectPath string
	database    string
	db          *sql.DB
	ledger      *store.GenerationLedger
	reserves    int
}

// productionDraftCommitOpen creates the fixture. It opens the database with the
// same URI flags the production fixtures use, because the drafts INSERT is guarded
// by a foreign key on this schema.
func productionDraftCommitOpen(t *testing.T) *productionDraftCommitFixture {
	t.Helper()
	_, projectPath, _, _, _ := productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return fmt.Sprintf("ci-draft-commit-%03d", ordinal)
	})
	return &productionDraftCommitFixture{projectPath: projectPath, database: database, db: db, ledger: ledger}
}

// bytes reads the author database file, the witness a refusal must leave alone.
func (f *productionDraftCommitFixture) bytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(f.database)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// reader opens a connection of the test's own, read-only, so every assertion
// below is on persisted state rather than on the answer under test.
func (f *productionDraftCommitFixture) reader(t *testing.T) *sql.DB {
	t.Helper()
	return productionStoryStateWriteReadback(t, f.projectPath)
}

// open freezes one run through the shipped ledger and returns its identity.
func (f *productionDraftCommitFixture) open(t *testing.T, request string) (string, string) {
	t.Helper()
	value, err := f.ledger.CallJSON(context.Background(), "open", json.RawMessage("["+request+"]"))
	if err != nil {
		t.Fatalf("open run: %v", err)
	}
	run := syntheticMap(value)
	runID, rootID := syntheticString(run["runId"]), syntheticString(run["rootActionId"])
	if runID == "" || rootID == "" {
		t.Fatalf("open returned no identity: %v", run)
	}
	return runID, rootID
}

// settle runs one complete settlement chain per text - reserve, dispatch,
// snapshot, settle - and returns the attempt ids in artifact order. A hand-written
// attempt row would prove only that the reader can read itself.
func (f *productionDraftCommitFixture) settle(t *testing.T, runID string, texts ...string) []string {
	t.Helper()
	return f.settleWith(t, runID, "stop", texts...)
}

// settleWith is settle with an explicit finish reason, so a subtest can seal an
// artifact the way a length-capped generation seals it.
func (f *productionDraftCommitFixture) settleWith(t *testing.T, runID, finishReason string, texts ...string) []string {
	t.Helper()
	ctx := context.Background()
	attemptIDs := make([]string, 0, len(texts))
	for _, text := range texts {
		f.reserves++
		reserved, err := f.ledger.Call(ctx, "reserve", []any{runID,
			fmt.Sprintf("ci-draft-commit-reserve-%d", f.reserves), strings.Repeat("b", 64), 200, 100})
		if err != nil {
			t.Fatalf("reserve: %v", err)
		}
		attemptID := syntheticString(syntheticMap(syntheticMap(reserved)["attempt"])["attemptId"])
		if attemptID == "" {
			t.Fatalf("reserve returned no attempt: %v", reserved)
		}
		for _, step := range []struct {
			method string
			args   []any
		}{
			{"markDispatched", []any{attemptID}},
			{"snapshot", []any{attemptID, 0, text}},
			{"settle", []any{attemptID, map[string]any{"trusted": true, "actualTokens": 120}, finishReason}},
		} {
			if _, err := f.ledger.Call(ctx, step.method, step.args); err != nil {
				t.Fatalf("%s: %v", step.method, err)
			}
		}
		attemptIDs = append(attemptIDs, attemptID)
	}
	return attemptIDs
}

// artifacts reads the run's artifact ids in artifact order through a connection of
// the test's own.
func (f *productionDraftCommitFixture) artifacts(t *testing.T, runID string) []string {
	t.Helper()
	rows, err := f.reader(t).Query(`SELECT a.artifact_id FROM generation_artifacts a
 JOIN generation_attempts t ON t.attempt_id=a.attempt_id WHERE t.run_id=? ORDER BY t.rowid`, runID)
	if err != nil {
		t.Fatalf("read artifacts of %s: %v", runID, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan artifact of %s: %v", runID, err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read artifacts of %s: %v", runID, err)
	}
	return ids
}

// compose stores the visible composition receipt the reference's composeVisible
// stores (:689-693): the algorithm, the hash of the composed text, the artifact
// order and one source projection per artifact. It writes through the ledger's
// recordEffect, the same call production_composition_linux.go:609 makes, so the
// stored shape is the store's own encoder output.
func (f *productionDraftCommitFixture) compose(t *testing.T, runID, attemptID string, artifactIDs []string, texts []string) {
	t.Helper()
	ordered := make([]any, len(artifactIDs))
	sources := make([]any, len(artifactIDs))
	for index := range artifactIDs {
		ordered[index] = artifactIDs[index]
		sources[index] = map[string]any{
			"artifactId": artifactIDs[index],
			"revision":   int64(1),
			"textHash":   generation.TextHash(texts[index]),
		}
	}
	value := map[string]any{
		"algorithm":   generation.DraftVisibleTextVersion,
		"textHash":    generation.TextHash(productionDraftCommitComposed(texts)),
		"artifactIds": ordered,
		"sources":     sources,
	}
	if _, err := f.ledger.Call(context.Background(), "recordEffect",
		[]any{runID, attemptID, "visibleComposition", value}); err != nil {
		t.Fatalf("record composition: %v", err)
	}
}

// request is the store request the served handler would build for this handle.
func (f *productionDraftCommitFixture) request(rootID, runID string, chapter int64, batchID, hash string) store.DraftCommitRequest {
	return store.DraftCommitRequest{
		Handle: store.BatchRunHandle{
			ProjectID:    productionEditorProjectID,
			Epoch:        productionDraftCommitEpoch,
			RootActionID: rootID,
			RunID:        runID,
		},
		ExpectedCompositionHash: hash,
		ChapterNumber:           chapter,
		Source:                  "write",
		BatchID:                 batchID,
		LiveEpoch:               productionDraftCommitEpoch,
	}
}

// accepted runs one commit that must succeed.
func (f *productionDraftCommitFixture) accepted(t *testing.T, label string, request store.DraftCommitRequest) *store.DraftCommitReceipt {
	t.Helper()
	receipt, err := store.CommitGenerationDraft(context.Background(), f.db, request)
	if err != nil {
		t.Fatalf("%s: the commit was refused with %v", label, err)
	}
	if receipt == nil || !receipt.Success {
		t.Fatalf("%s: the commit answered no receipt: %+v", label, receipt)
	}
	return receipt
}

// tally counts the rows a commit could add or change, read over a connection of
// the test's own: drafts, contents and stamped draftCommit receipts.
func (f *productionDraftCommitFixture) tally(t *testing.T) [3]int64 {
	t.Helper()
	var counts [3]int64
	view := f.reader(t)
	productionStoryStateWriteScan(t, view, `SELECT COUNT(*) FROM drafts`, nil, &counts[0])
	productionStoryStateWriteScan(t, view, `SELECT COUNT(*) FROM contents`, nil, &counts[1])
	productionStoryStateWriteScan(t, view,
		`SELECT COUNT(*) FROM generation_attempts
 WHERE json_extract(usage_receipt_json,'$.draftCommit') IS NOT NULL`, nil, &counts[2])
	return counts
}

// refused runs one commit that must be refused with exactly want, and proves the
// refusal wrote nothing: the file is byte-identical, and no draft, content row or
// receipt appeared when the same database is read over another connection.
func (f *productionDraftCommitFixture) refused(t *testing.T, label string, request store.DraftCommitRequest, want error) {
	t.Helper()
	before := f.bytes(t)
	beforeCounts := f.tally(t)
	receipt, err := store.CommitGenerationDraft(context.Background(), f.db, request)
	if err == nil {
		t.Fatalf("%s: the commit was accepted: %+v", label, receipt)
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s: refused with %v instead of %v", label, err, want)
	}
	if after := f.bytes(t); !bytes.Equal(before, after) {
		t.Fatalf("%s: the refusal changed the author database", label)
	}
	if afterCounts := f.tally(t); afterCounts != beforeCounts {
		t.Fatalf("%s: the refusal added rows: %v became %v", label, beforeCounts, afterCounts)
	}
}

// productionDraftCommitRunRequest is the frozen chapter-draft binding one run of
// this file opens. It is the reference's own shape: the operation, the chapter, the
// target units among the author inputs, and the batch it belongs to when the caller
// names one.
func productionDraftCommitRunRequest(nonce string, chapter int64, targetUnits, hash, batchID, parentRootID string) string {
	number := fmt.Sprintf("%d", chapter)
	batch, parent := "", ""
	if batchID != "" {
		batch = `,"batchId":"` + batchID + `"`
	}
	if parentRootID != "" {
		parent = `,"parentRootActionId":"` + parentRootID + `"`
	}
	return `{"projectId":"` + productionEditorProjectID + `","epoch":"` + productionDraftCommitEpoch +
		`","operation":"chapter-draft","uiActionNonce":"` + nonce + `","frozenInputHash":"` + hash +
		`","fingerprint":` + productionBatchFingerprint(hash) + `,"contextSnapshotId":"ci-draft-commit-context-` + number +
		`"` + parent + `,"sourceManifest":{"operation":"chapter-draft"` + batch + `,"chapterNumber":` + number +
		`,"modelId":"` + productionTestModelID + `","outputContract":"visible-text","authorInputs":[{"id":"draft:target-units","text":"` +
		targetUnits + `"}],"selectedDraftIds":[],"selectedFinalizedDraftIds":[]},"sourceRefs":[]` +
		`,"budget":{"maxPhysicalRequests":3,"maxTokenLiability":4000,"maxOutputPerRequest":1000,"maxActiveElapsedMs":600000}}`}
}

// TestProductionDraftCommitServesTheOneTransaction drives generation:commit-draft:
// the binding over HTTP, then the transaction itself at the level where a
// draft-visible composition can exist, one subtest per aspect.
func TestProductionDraftCommitServesTheOneTransaction(t *testing.T) {
	t.Run("generation:commit-draft", testProductionDraftCommitBinding)
	t.Run("store: commit lands contents, drafts and the receipt", testProductionDraftCommitLands)
	t.Run("store: target refusals", testProductionDraftCommitTarget)
	t.Run("store: composition required", testProductionDraftCommitCompositionRequired)
	t.Run("store: incomplete", testProductionDraftCommitIncomplete)
	t.Run("store: receipt invalid", testProductionDraftCommitReceiptInvalid)
	t.Run("store: stale run epoch", testProductionDraftCommitStaleEpoch)
	t.Run("store: batch progress conflict", testProductionDraftCommitBatchConflict)
}

// testProductionDraftCommitBinding proves the name is served by this lane: over a
// real admitted project and a real live run, the operation answers through the
// mutating envelope with the codes the reference raises, refuses a lease this
// server never issued, and leaves the database byte-identical every time.
func testProductionDraftCommitBinding(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	handle := lane.begin(t, productionEditorBeginRequest("draft-commit-begin-1"))
	runID, _ := handle["runId"].(string)
	if runID == "" {
		t.Fatalf("generation:begin returned no run id: %v", handle)
	}
	request := func() map[string]any {
		return map[string]any{
			"handle":                  handle,
			"chapterNumber":           1,
			"source":                  "write",
			"expectedCompositionHash": strings.Repeat("a", 64),
		}
	}
	baseline := lane.bytes(t)
	// A live run has artifacts only after generation:execute, and this subtest
	// never executes one, so the composition gate is the first thing a well-formed
	// commit reaches. That is the refusal this lane can prove over HTTP.
	if code := productionLifecycleRefusal(t, "commit without a composition",
		lane.author.call(t, "generation:commit-draft", []any{request(), lane.frozen})); code != "GENERATION_DRAFT_COMPOSITION_REQUIRED" {
		t.Fatalf("generation:commit-draft refused with %q instead of GENERATION_DRAFT_COMPOSITION_REQUIRED", code)
	}
	if !bytes.Equal(baseline, lane.bytes(t)) {
		t.Fatal("a refused commit changed the admitted author database")
	}
	// The target gate precedes the composition gate, so a foreign source is its own
	// code.
	other := request()
	other["source"] = "rewrite"
	if code := productionLifecycleRefusal(t, "commit for a foreign source",
		lane.author.call(t, "generation:commit-draft", []any{other, lane.frozen})); code != "GENERATION_DRAFT_TARGET_INVALID" {
		t.Fatalf("generation:commit-draft refused a foreign source with %q instead of GENERATION_DRAFT_TARGET_INVALID", code)
	}
	stray := request()
	stray["chapterNumber"] = 2
	if code := productionLifecycleRefusal(t, "commit for another chapter",
		lane.author.call(t, "generation:commit-draft", []any{stray, lane.frozen})); code != "GENERATION_DRAFT_TARGET_INVALID" {
		t.Fatalf("generation:commit-draft refused another chapter with %q instead of GENERATION_DRAFT_TARGET_INVALID", code)
	}
	// requireRun: a handle naming no run at all.
	missing := request()
	missing["handle"] = map[string]any{
		"projectId":    handle["projectId"],
		"epoch":        handle["epoch"],
		"rootActionId": handle["rootActionId"],
		"runId":        "ci-missing-run",
	}
	if code := productionLifecycleRefusal(t, "commit for an unknown run",
		lane.author.call(t, "generation:commit-draft", []any{missing, lane.frozen})); code != "GENERATION_RUN_NOT_FOUND" {
		t.Fatalf("generation:commit-draft answered %q for an unknown run instead of GENERATION_RUN_NOT_FOUND", code)
	}
	// A handle that is another run's identity is the reference's mismatch, not an
	// absence.
	foreign := request()
	foreign["handle"] = map[string]any{
		"projectId":    handle["projectId"],
		"epoch":        handle["epoch"],
		"rootActionId": "ci-other-root",
		"runId":        runID,
	}
	if code := productionLifecycleRefusal(t, "commit for another root",
		lane.author.call(t, "generation:commit-draft", []any{foreign, lane.frozen})); code != "GENERATION_RUN_IDENTITY_MISMATCH" {
		t.Fatalf("generation:commit-draft answered %q for a foreign root instead of GENERATION_RUN_IDENTITY_MISMATCH", code)
	}
	if code := productionLifecycleRefusal(t, "stale-lease generation:commit-draft",
		lane.author.call(t, "generation:commit-draft", []any{request(), lane.stale()})); code != "AUTHOR_WRITE_STALE" {
		t.Fatalf("generation:commit-draft answered %q for a lease this server never issued instead of AUTHOR_WRITE_STALE", code)
	}
	if !bytes.Equal(baseline, lane.bytes(t)) {
		t.Fatal("a refused commit changed the admitted author database")
	}
	lane.assertNoDial(t)
}

// testProductionDraftCommitLands proves the write half: one commit of a composed
// two-artifact chapter, read back over a connection of the test's own, and then the
// duplicate answer the reference gives without touching a row.
func testProductionDraftCommitLands(t *testing.T) {
	fixture := productionDraftCommitOpen(t)
	hash := strings.Repeat("c", 64)
	runID, rootID := fixture.open(t, productionDraftCommitRunRequest("ci-draft-commit-lands", 1, "3000", hash, "", ""))
	first := productionDraftCommitProse(productionDraftCommitSentence, 200)
	second := productionDraftCommitProse(productionDraftCommitNextLine, 150)
	attempts := fixture.settle(t, runID, first, second)
	artifacts := fixture.artifacts(t, runID)
	if len(artifacts) != 2 {
		t.Fatalf("the seeded run holds %d artifacts, not 2: %v", len(artifacts), artifacts)
	}
	composed := productionDraftCommitComposed([]string{first, second})
	composedHash := generation.TextHash(composed)
	targetRange, err := generation.DraftTargetUnitRange(3000)
	if err != nil {
		t.Fatalf("the fixture's target of 3000 units has no range: %v", err)
	}
	if units := int64(generation.CountDraftUnits(composed)); units < targetRange.Minimum {
		t.Fatalf("the fixture composed %d units, below the %d the target requires", units, targetRange.Minimum)
	}
	fixture.compose(t, runID, attempts[1], artifacts, []string{first, second})
	receipt := fixture.accepted(t, "commit",
		fixture.request(rootID, runID, 1, "", composedHash))
	if receipt.Content != composed || receipt.ContentHash != composedHash {
		t.Fatalf("the receipt does not carry the composed text: %+v", receipt)
	}
	// Read back: the contents row, the drafts row and the stamped receipt, all
	// agree with each other and with the receipt the commit answered.
	var body, storedHash, storedContent string
	var storedID, storedVersion, storedChapter, wordCount, chapterNumber, version int64
	var source, status string
	readback := fixture.reader(t)
	productionStoryStateWriteScan(t, readback,
		`SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=?`, []any{receipt.ID}, &body)
	productionStoryStateWriteScan(t, readback,
		`SELECT chapter_number, version, source, status, word_count FROM drafts WHERE id=?`, []any{receipt.ID},
		&chapterNumber, &version, &source, &status, &wordCount)
	productionStoryStateWriteScan(t, readback,
		`SELECT json_extract(usage_receipt_json,'$.draftCommit.id'),
 json_extract(usage_receipt_json,'$.draftCommit.version'),
 json_extract(usage_receipt_json,'$.draftCommit.chapterNumber'),
 json_extract(usage_receipt_json,'$.draftCommit.contentHash'),
 json_extract(usage_receipt_json,'$.draftCommit.content')
 FROM generation_attempts WHERE attempt_id=?`, []any{attempts[1]},
		&storedID, &storedVersion, &storedChapter, &storedHash, &storedContent)
	if body != composed || body != storedContent {
		t.Fatalf("the contents row does not hold the composed text: %d vs %d code points",
			len([]rune(body)), len([]rune(composed)))
	}
	if chapterNumber != 1 || source != "write" || status != "draft" {
		t.Fatalf("the drafts row is not a working chapter-1 write: %d %q %q", chapterNumber, source, status)
	}
	if wordCount != int64(generation.CountDraftUnits(composed)) {
		t.Fatalf("the drafts row stores %d units, not %d", wordCount, generation.CountDraftUnits(composed))
	}
	if version != receipt.Version || chapterNumber != storedChapter || storedID != receipt.ID ||
		storedVersion != receipt.Version || storedHash != receipt.ContentHash {
		t.Fatalf("the receipt and the rows disagree: %d/%d/%d/%d vs %+v",
			storedID, storedVersion, storedChapter, version, receipt)
	}
	// The duplicate: the same answer, and not one byte written.
	baseline := fixture.bytes(t)
	duplicate := fixture.accepted(t, "duplicate commit",
		fixture.request(rootID, runID, 1, "", composedHash))
	if duplicate.ID != receipt.ID || duplicate.Version != receipt.Version ||
		duplicate.Content != composed || duplicate.ContentHash != composedHash {
		t.Fatalf("the duplicate answered a different receipt: %+v vs %+v", duplicate, receipt)
	}
	if !bytes.Equal(baseline, fixture.bytes(t)) {
		t.Fatal("a duplicate commit wrote to the author database")
	}
	var drafts int64
	productionStoryStateWriteScan(t, readback, `SELECT COUNT(*) FROM drafts`, nil, &drafts)
	if drafts != 2 {
		t.Fatalf("the duplicate commit left %d drafts, not the fixture's 1 plus 1", drafts)
	}
	// A different composition hash is the reference's own conflict.
	fixture.refused(t, "conflicting duplicate",
		fixture.request(rootID, runID, 1, "", generation.TextHash("另一份正文")), store.ErrDraftCommitConflict)
}

// testProductionDraftCommitTarget proves the target gate: the source has to be this
// write, the chapter has to be this run's own, and the batch has to be the frozen
// one - each refused by its own name, and each leaving the file alone.
func testProductionDraftCommitTarget(t *testing.T) {
	fixture := productionDraftCommitOpen(t)
	hash := strings.Repeat("c", 64)
	runID, rootID := fixture.open(t, productionDraftCommitRunRequest("ci-draft-commit-target", 1, "3000", hash, "", ""))
	texts := []string{productionDraftCommitProse(productionDraftCommitSentence, 200),
		productionDraftCommitProse(productionDraftCommitNextLine, 150)}
	attempts := fixture.settle(t, runID, texts...)
	fixture.compose(t, runID, attempts[1], fixture.artifacts(t, runID), texts)
	composedHash := generation.TextHash(productionDraftCommitComposed(texts))
	foreign := fixture.request(rootID, runID, 1, "", composedHash)
	foreign.Source = "finalize"
	fixture.refused(t, "commit as a finalization", foreign, store.ErrDraftCommitTargetInvalid)
	otherChapter := fixture.request(rootID, runID, 2, "", composedHash)
	fixture.refused(t, "commit for chapter 2", otherChapter, store.ErrDraftCommitTargetInvalid)
	otherBatch := fixture.request(rootID, runID, 1, "ci-some-other-batch", composedHash)
	fixture.refused(t, "commit under another batch", otherBatch, store.ErrDraftCommitTargetInvalid)
}

// testProductionDraftCommitCompositionRequired proves the composition gate: no
// stored receipt, and a stored receipt whose hash is not the one the caller froze,
// are the same refusal. The point is that the server never trusts text off the wire:
// it recomposes from the artifacts and only writes what it recomposed.
func testProductionDraftCommitCompositionRequired(t *testing.T) {
	fixture := productionDraftCommitOpen(t)
	hash := strings.Repeat("c", 64)
	runID, rootID := fixture.open(t, productionDraftCommitRunRequest("ci-draft-commit-composition", 1, "3000", hash, "", ""))
	texts := []string{productionDraftCommitProse(productionDraftCommitSentence, 200),
		productionDraftCommitProse(productionDraftCommitNextLine, 150)}
	attempts := fixture.settle(t, runID, texts...)
	artifacts := fixture.artifacts(t, runID)
	composedHash := generation.TextHash(productionDraftCommitComposed(texts))
	fixture.refused(t, "commit without a stored composition",
		fixture.request(rootID, runID, 1, "", composedHash), store.ErrDraftCommitCompositionRequired)
	fixture.compose(t, runID, attempts[1], artifacts, texts)
	staleHash := fixture.request(rootID, runID, 1, "", generation.TextHash("另一份正文"))
	fixture.refused(t, "commit with a foreign composition hash", staleHash, store.ErrDraftCommitCompositionRequired)
	fixture.accepted(t, "commit with the composed hash", fixture.request(rootID, runID, 1, "", composedHash))
}

// testProductionDraftCommitIncomplete proves the three INCOMPLETE gates in the
// order the reference checks them: the last artifact has to have finished, the run
// has to freeze a usable target, and the composed text has to reach the bottom of
// that target's range.
func testProductionDraftCommitIncomplete(t *testing.T) {
	fixture := productionDraftCommitOpen(t)
	text := productionDraftCommitProse(productionDraftCommitSentence, 200)
	hash := strings.Repeat("c", 64)
	// One artifact that stopped on the length cap instead of finishing.
	capped, cappedRoot := fixture.open(t,
		productionDraftCommitRunRequest("ci-draft-commit-capped", 1, "3000", hash, "", ""))
	cappedAttempts := fixture.settleWith(t, capped, "length", text)
	fixture.compose(t, capped, cappedAttempts[0], fixture.artifacts(t, capped), []string{text})
	fixture.refused(t, "commit over a length-capped artifact",
		fixture.request(cappedRoot, capped, 1, "", generation.TextHash(productionDraftCommitComposed([]string{text}))),
		store.ErrDraftCommitIncomplete)
	// A run whose manifest freezes no usable target.
	untargeted, untargetedRoot := fixture.open(t,
		productionDraftCommitRunRequest("ci-draft-commit-untargeted", 3, "0", hash, "", ""))
	untargetedAttempts := fixture.settle(t, untargeted, text)
	fixture.compose(t, untargeted, untargetedAttempts[0], fixture.artifacts(t, untargeted), []string{text})
	fixture.refused(t, "commit without a usable target",
		fixture.request(untargetedRoot, untargeted, 3, "", generation.TextHash(productionDraftCommitComposed([]string{text}))),
		store.ErrDraftCommitIncomplete)
	// A run whose composed text is below the bottom of its own range.
	short, shortRoot := fixture.open(t,
		productionDraftCommitRunRequest("ci-draft-commit-short", 4, "3000", hash, "", ""))
	shortText := productionDraftCommitProse(productionDraftCommitSentence, 10)
	shortAttempts := fixture.settle(t, short, shortText)
	fixture.compose(t, short, shortAttempts[0], fixture.artifacts(t, short), []string{shortText})
	fixture.refused(t, "commit below the target range",
		fixture.request(shortRoot, short, 4, "", generation.TextHash(productionDraftCommitComposed([]string{shortText}))),
		store.ErrDraftCommitIncomplete)
}

// testProductionDraftCommitReceiptInvalid proves the duplicate path's own guard: a
// receipt the artifacts no longer support, and a draft body that moved under a saved
// receipt, are both refused instead of being replayed.
func testProductionDraftCommitReceiptInvalid(t *testing.T) {
	fixture := productionDraftCommitOpen(t)
	runID, rootID := fixture.open(t,
		productionDraftCommitRunRequest("ci-draft-commit-receipt", 5, "3000", strings.Repeat("c", 64), "", ""))
	texts := []string{productionDraftCommitProse(productionDraftCommitSentence, 200),
		productionDraftCommitProse(productionDraftCommitNextLine, 150)}
	attempts := fixture.settle(t, runID, texts...)
	fixture.compose(t, runID, attempts[1], fixture.artifacts(t, runID), texts)
	composedHash := generation.TextHash(productionDraftCommitComposed(texts))
	receipt := fixture.accepted(t, "commit", fixture.request(rootID, runID, 5, "", composedHash))
	// The draft body moved out from under the saved receipt.
	if _, err := fixture.db.Exec("UPDATE contents SET body=? WHERE id=(SELECT content_id FROM drafts WHERE id=?)",
		"另一份正文", receipt.ID); err != nil {
		t.Fatalf("rewrite the draft body: %v", err)
	}
	fixture.refused(t, "commit over a changed draft body",
		fixture.request(rootID, runID, 5, "", composedHash), store.ErrDraftCommitReceiptInvalid)
	// A receipt whose stored hash no longer covers its own content.
	if _, err := fixture.db.Exec("UPDATE contents SET body=? WHERE id=(SELECT content_id FROM drafts WHERE id=?)",
		productionDraftCommitComposed(texts), receipt.ID); err != nil {
		t.Fatalf("restore the draft body: %v", err)
	}
	if _, err := fixture.db.Exec(
		"UPDATE generation_attempts SET usage_receipt_json=json_set(usage_receipt_json,'$.draftCommit.contentHash',?) WHERE attempt_id=?",
		strings.Repeat("d", 64), attempts[1]); err != nil {
		t.Fatalf("tamper the receipt: %v", err)
	}
	fixture.refused(t, "commit with a tampered receipt",
		fixture.request(rootID, runID, 5, "", composedHash), store.ErrDraftCommitReceiptInvalid)
}

// testProductionDraftCommitStaleEpoch proves assertSources' epoch gate: a run whose
// frozen epoch is not the calling lease's is refused, and the refusal writes
// nothing. This is the one refusal the reference raises inside the transaction and
// only on the first commit, after the duplicate replay.
func testProductionDraftCommitStaleEpoch(t *testing.T) {
	fixture := productionDraftCommitOpen(t)
	runID, rootID := fixture.open(t,
		productionDraftCommitRunRequest("ci-draft-commit-epoch", 6, "3000", strings.Repeat("c", 64), "", ""))
	texts := []string{productionDraftCommitProse(productionDraftCommitSentence, 200),
		productionDraftCommitProse(productionDraftCommitNextLine, 150)}
	attempts := fixture.settle(t, runID, texts...)
	fixture.compose(t, runID, attempts[1], fixture.artifacts(t, runID), texts)
	composedHash := generation.TextHash(productionDraftCommitComposed(texts))
	stale := fixture.request(rootID, runID, 6, "", composedHash)
	stale.LiveEpoch = "lease-0000000004-other"
	fixture.refused(t, "commit under a rotated lease", stale, store.ErrDraftCommitEpochStale)
	// The same run commits once the epoch is the frozen one, so the refusal above is
	// the epoch and not something else.
	fixture.accepted(t, "commit under the frozen epoch", fixture.request(rootID, runID, 6, "", composedHash))
}

// testProductionDraftCommitBatchConflict proves the batch guard: a batch whose first
// chapter is already saved refuses a second save of that chapter, and the refusal
// happens inside the transaction that would have written it.
//
// Two rows are rewritten before the commit, and only rows the fixtures own: the
// shipped ledger mints a fresh root for every open, while the reference gives a
// batch and its chapter runs one root action, so the batch root and the saved
// chapter run are pointed at the committing run's root. The saved chapter's receipt
// itself is written the way production_batch_reads_linux_test.go:295 writes the
// receipts the family writers would have written, because this lane owns no batch
// writer either.
func testProductionDraftCommitBatchConflict(t *testing.T) {
	fixture := productionDraftCommitOpen(t)
	batchID, batchRootID := fixture.open(t, productionBatchRootRequest())
	if _, err := fixture.db.Exec(`INSERT INTO contents (id, body) VALUES (2, ?)`, productionBatchChapterTwoText); err != nil {
		t.Fatalf("seed the saved chapter's content: %v", err)
	}
	if _, err := fixture.db.Exec(`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (2, 2, 1, 'draft', 'write', 2, 0, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`); err != nil {
		t.Fatalf("seed the saved chapter's draft: %v", err)
	}
	savedRunID, _ := fixture.open(t, productionBatchChapterRequest(2, batchID, batchRootID))
	// The run under test: a chapter-2 run of the same batch, under its own root.
	runID, rootID := fixture.open(t, productionDraftCommitRunRequest("ci-draft-commit-batch", 2,
		productionDraftCommitBatchTarget, strings.Repeat("c", 64), batchID, batchRootID))
	// Every ledger write happens before the two rows are repointed, so no settlement
	// runs against a root that is no longer the run's own.
	savedAttempt := productionBatchSettle(t, fixture.ledger, savedRunID, productionBatchChapterTwoText)
	texts := []string{productionDraftCommitProse(productionDraftCommitSentence, 200)}
	attempts := fixture.settle(t, runID, texts...)
	fixture.compose(t, runID, attempts[0], fixture.artifacts(t, runID), texts)
	for _, update := range []struct {
		runID string
		label string
	}{{batchID, "the batch root"}, {savedRunID, "the saved chapter run"}} {
		if _, err := fixture.db.Exec("UPDATE generation_runs SET root_action_id=? WHERE run_id=?", rootID, update.runID); err != nil {
			t.Fatalf("share the root with %s: %v", update.label, err)
		}
	}
	// The already-saved chapter 2, receipted the way the batch fixtures receipt one.
	productionBatchPatchReceipt(t, fixture.db, savedAttempt, map[string]any{
		"artifactIdentity": map[string]any{"epoch": productionBatchEpoch},
		"visibleComposition": map[string]any{
			"algorithm":   generation.DraftVisibleTextVersion,
			"text":        productionBatchChapterTwoText,
			"textHash":    sha256Hex(productionBatchChapterTwoText),
			"artifactIds": []any{},
			"sources":     []any{},
		},
		"draftCommit": map[string]any{
			"success":       true,
			"id":            int64(2),
			"version":       int64(1),
			"chapterNumber": int64(2),
			"content":       productionBatchChapterTwoText,
			"contentHash":   sha256Hex(productionBatchChapterTwoText),
			"batchId":       batchID,
			"handle": map[string]any{
				"projectId":    productionEditorProjectID,
				"epoch":        productionBatchEpoch,
				"rootActionId": rootID,
				"runId":        savedRunID,
			},
		},
	})
	fixture.refused(t, "commit over a saved chapter",
		fixture.request(rootID, runID, 2, batchID, generation.TextHash(productionDraftCommitComposed(texts))),
		store.ErrDraftCommitBatchConflict)
}
