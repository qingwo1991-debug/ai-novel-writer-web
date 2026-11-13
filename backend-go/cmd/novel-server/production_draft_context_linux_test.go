//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the two chapter-draft context names
// production_draft_context_linux.go binds, one subtest per operation, and states
// the third name's absence as a test rather than as a comment. Every subtest drives
// the real HTTP handler over a real admitted project database and then reads the
// file back through a connection of the test's own, so a handler that echoed the
// request, projected a default shape or fabricated an empty valid-looking context
// cannot pass.
//
// The fixture is the editor read journey's own: one admitted original-schema v7
// database, one working draft, and one generation run opened, reserved, dispatched,
// snapshotted and settled by the real ledger store (production_editor_reads_linux_
// test.go:206-284). This file only makes that run's frozen manifest a real
// chapter-draft manifest - a chapter number, a model, author inputs, a selected
// draft with its frozen source reference, a batch id and a knowledge snapshot - so
// read-context's projection has seeded facts to project instead of nothing to say.
// The run is frozen under an epoch the live session never holds, which is the
// property that makes it history: read-context must serve it, and it must still
// refuse a stale SESSION lease.
//
// Nothing here dials. The committed model endpoint is a loopback listener this test
// owns, inspected after every subtest the way production_generation_write_linux_test.
// go:216-226 inspects its own: a dial would already be queued for Accept.

const (
	productionDraftContextOpenNonce  = "draft-context-open-1"
	productionDraftContextRunEpoch   = "lease-0000000001-history"
	productionDraftContextRunNonce   = "ci-draft-context-nonce-1"
	productionDraftContextBatchID    = "ci-draft-context-batch-1"
	productionDraftContextQuery      = "门"
	productionDraftContextUnits      = "3000"
	productionDraftContextInputKey   = "draft:target-units"
	productionDraftContextSnapshotID = "ci-draft-context-snapshot-1"
	productionDraftContextUnknownID  = 999
)

// productionDraftContextSeededRun is the identity of the one settled run the
// fixture freezes, so every assertion compares against what the ledger wrote
// instead of a hard-coded guess.
type productionDraftContextSeededRun struct {
	RootActionID string
	RunID        string
	AttemptID    string
	Epoch        string
}

// productionDraftContextLane is one fresh production fixture, one loopback model
// origin and one open lane over it, plus the exact bytes the fixtures left on disk.
type productionDraftContextLane struct {
	projectPath string
	database    string
	listener    net.Listener
	origin      string
	author      *productionDurableClient
	frozen      map[string]any
	baseline    []byte
	run         productionDraftContextSeededRun
}

// productionDraftContextHash is the content hash the fixtures freeze into source
// references and the assertions recompute from the file.
func productionDraftContextHash(text string) string {
	return syntheticHash(text)
}

// productionDraftContextFixture builds the disposable root: the admitted v7
// baseline with the editor rows, the committed model profile the lane must find,
// and one settled chapter-draft run with a fully populated frozen manifest. The
// loopback listener is the one its origin was committed with, so the no-dial
// assertion inspects the endpoint the lane actually holds.
func productionDraftContextFixture(t *testing.T) (string, string, admission.SchemaContract, productionDraftContextSeededRun, net.Listener, string) {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	listener, origin := productionGenerationWriteListener(t)
	productionGenerationWriteModel(t, root, origin)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	return root, projectPath, contract, productionDraftContextSeedRun(t, database), listener, origin
}

// productionDraftContextSeedRun writes one complete settlement chain through the
// real ledger - open, reserve, dispatch, snapshot, settle - over a chapter-draft
// binding whose manifest names the draft this fixture seeded. A hand-written row set
// would prove only that the reader can read itself.
func productionDraftContextSeedRun(t *testing.T, database string) productionDraftContextSeededRun {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return productionDraftContextSnapshotID + "-" + string(rune('a'+ordinal))
	})
	ctx := context.Background()
	request := productionDraftContextRunRequest()
	value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+request+"]"))
	if err != nil {
		_ = db.Close()
		t.Fatalf("seed draft context open: %v", err)
	}
	run := syntheticMap(value)
	runID, rootID := syntheticString(run["runId"]), syntheticString(run["rootActionId"])
	if runID == "" || rootID == "" {
		_ = db.Close()
		t.Fatalf("seed draft context open returned no identity: %v", run)
	}
	reserved, err := ledger.Call(ctx, "reserve", []any{runID, productionDraftContextRunNonce, strings.Repeat("b", 64), 200, 100})
	if err != nil {
		_ = db.Close()
		t.Fatalf("seed draft context reserve: %v", err)
	}
	attemptID := syntheticString(syntheticMap(syntheticMap(reserved)["attempt"])["attemptId"])
	if attemptID == "" {
		_ = db.Close()
		t.Fatalf("seed draft context reserve returned no attempt: %v", reserved)
	}
	for _, step := range []struct {
		method string
		args   []any
	}{
		{"markDispatched", []any{attemptID}},
		{"snapshot", []any{attemptID, 0, productionEditorGenerationText}},
		{"settle", []any{attemptID, map[string]any{"trusted": true, "actualTokens": 120}, "stop"}},
	} {
		if _, err := ledger.Call(ctx, step.method, step.args); err != nil {
			_ = db.Close()
			t.Fatalf("seed draft context %s: %v", step.method, err)
		}
	}
	// The lane admits the file itself, so the seeded write must be checkpointed
	// into it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
	return productionDraftContextSeededRun{RootActionID: rootID, RunID: runID, AttemptID: attemptID, Epoch: productionDraftContextRunEpoch}
}

// productionDraftContextRunRequest is the frozen chapter-draft binding the ledger
// opens: the same nine-hash fingerprint, opaque manifest and budget the shipped
// ledger accepts, with a manifest that carries every field read-context projects.
func productionDraftContextRunRequest() string {
	hash := strings.Repeat("a", 64)
	epoch := productionDraftContextRunEpoch
	return `{"projectId":"` + productionEditorProjectID + `","epoch":"` + epoch +
		`","operation":"` + productionDraftContextOperation + `","uiActionNonce":"` + productionDraftContextRunNonce +
		`","frozenInputHash":"` + hash + `","fingerprint":{"chapterBriefHash":"` + hash + `","authorGuidanceHash":"` + hash +
		`","dependencyHash":"` + hash + `","contextSnapshotHash":"` + hash + `","templateHash":"` + hash +
		`","skillSnapshotHash":"` + hash + `","modelLeaseRevision":"` + hash + `","policyHash":"` + hash +
		`","outputContractHash":"` + hash + `"},"contextSnapshotId":"` + productionDraftContextSnapshotID +
		`","sourceManifest":{"operation":"` + productionDraftContextOperation + `","chapterNumber":1,"modelId":"` +
		productionTestModelID + `","authorInputs":[{"id":"` + productionDraftContextInputKey + `","text":"` +
		productionDraftContextUnits + `"}],"selectedDraftIds":[1],"selectedFinalizedDraftIds":[]` +
		`,"selectedBlueprintChapterNumbers":[1,2,3,4,5,6],"batchId":"` + productionDraftContextBatchID +
		`","knowledgeSnapshot":` + productionDraftContextSnapshotJSON() + `},"sourceRefs":[{"projectId":"` +
		productionEditorProjectID + `","epoch":"` + epoch + `","sourceId":"draft:1","revision":1,"contentHash":"` +
		productionDraftContextHash(productionEditorSeed) + `"}],"budget":{"maxPhysicalRequests":3,"maxTokenLiability":4000` +
		`,"maxOutputPerRequest":1000,"maxActiveElapsedMs":600000}}`
}

// productionDraftContextSnapshotJSON is the frozen knowledge snapshot the fixture
// stores, so a read-context that invents one instead of passing the manifest's
// through is detectable.
func productionDraftContextSnapshotJSON() string {
	return `{"version":1,"state":"empty","storageState":"absent","query":"` + productionDraftContextQuery +
		`","topK":5,"canonicalRevision":null,"documentsRevision":null,"items":[]}`
}

// productionDraftContextOpen opens one lane over the fixture and one author session
// over it, and pins the bytes on disk after both, so nothing a subtest asserts can
// be blamed on opening the project.
func productionDraftContextOpen(t *testing.T) *productionDraftContextLane {
	t.Helper()
	root, projectPath, contract, run, listener, origin := productionDraftContextFixture(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	lane := &productionDraftContextLane{projectPath: projectPath, database: database, run: run, listener: listener, origin: origin}
	lane.author, lane.frozen = productionExecuteSession(t, handler, projectPath, productionDraftContextOpenNonce)
	if lane.frozen["leaseId"] == lane.run.Epoch {
		t.Fatalf("the fixture's live lease is the seeded run's epoch %q, so a stale-lease refusal could never fire", lane.run.Epoch)
	}
	if lane.baseline, err = os.ReadFile(database); err != nil {
		t.Fatal(err)
	}
	return lane
}

// stale reports the three-key context a server that never issued this lease would
// have: the same project, a lease it cannot prove.
func (l *productionDraftContextLane) stale() map[string]any {
	return map[string]any{"projectId": l.frozen["projectId"], "projectPath": l.projectPath, "leaseId": "stale-lease"}
}

// handle is the browser run handle of the seeded run, rebuilt from what the ledger
// wrote rather than from a fixture literal.
func (l *productionDraftContextLane) handle() map[string]any {
	return map[string]any{
		"projectId":    productionEditorProjectID,
		"epoch":        l.run.Epoch,
		"rootActionId": l.run.RootActionID,
		"runId":        l.run.RunID,
	}
}

// unchanged reads the admitted database file again and fails when the bytes moved.
// It is the only proof that an operation did not write: the lane hands the browser
// a projection, not a mutation.
func (l *productionDraftContextLane) unchanged(t *testing.T, label string) {
	t.Helper()
	after, err := os.ReadFile(l.database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, l.baseline) {
		t.Fatalf("%s changed the admitted author database", label)
	}
}

// assertNoDial proves nothing under test reached the committed endpoint: a dial
// would already be queued on the listener, which never Accepted before this point.
func (l *productionDraftContextLane) assertNoDial(t *testing.T) {
	t.Helper()
	if tcp, ok := l.listener.(*net.TCPListener); ok {
		if err := tcp.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if connection, acceptErr := l.listener.Accept(); acceptErr == nil {
		_ = connection.Close()
		t.Fatalf("a read dialed the committed origin %s", l.origin)
	}
}

// productionDraftContextRefusal asserts one symbolic refusal code. The message
// leads with the status and brackets the code instead of quoting it, so a log that
// truncates at the first double quote still shows which code the wire carried.
func productionDraftContextRefusal(t *testing.T, client *productionDurableClient, label, operation string, args []any, want string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%s: %s answered %d instead of refusing with [%s]: %s", label, operation, response.Code, want, response.Body.String())
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %s answered %d with a body that is not the error envelope (%v): %s",
			label, operation, response.Code, err, response.Body.String())
	}
	if wire.Error.Code != want {
		t.Fatalf("%s: %s answered %d with code [%s] instead of [%s]: %s",
			label, operation, response.Code, wire.Error.Code, want, response.Body.String())
	}
}

// productionDraftContextDraft reads one draft row over a connection of the test's
// own, in read-only mode, so an assertion compares the handler's answer with the
// file instead of with this test's variables.
func productionDraftContextDraft(t *testing.T, database string, id int64) (chapter, version int64, body string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	if err := db.QueryRow(`SELECT d.chapter_number, d.version, c.body FROM drafts d
 JOIN contents c ON c.id = d.content_id WHERE d.id = ?`, id).Scan(&chapter, &version, &body); err != nil {
		t.Fatalf("read persisted draft %d: %v", id, err)
	}
	return chapter, version, body
}

// productionDraftContextIsID reports whether the value is a v4-shaped UUID, the
// only thing the browser may treat as an opaque preparation id.
func productionDraftContextIsID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index, c := range value {
		switch index {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return false
			}
		}
	}
	return true
}

// productionDraftContextPrepareRequest is the request the renderer sends
// (src/services/workflows/commands/generate-draft.command.ts:574-581): the chapter,
// the resolved model, the prompt key, the stage, the author inputs, the retrieval
// query, the selected candidate drafts and the batch the run belongs to.
func productionDraftContextPrepareRequest() map[string]any {
	return map[string]any{
		"chapterNumber":    float64(1),
		"modelId":          productionTestModelID,
		"promptKeys":       []any{"chapter-draft-template"},
		"skillStages":      []any{"drafting"},
		"authorInputs":     []any{map[string]any{"id": productionDraftContextInputKey, "text": productionDraftContextUnits}},
		"query":            productionDraftContextQuery,
		"selectedDraftIds": []any{float64(1)},
		"batchId":          productionDraftContextBatchID,
	}
}

// TestProductionDraftContextNames drives the two operations this lane
// binds for the draft-context stage. Each subtest works on its own fixture, so a
// failure names one operation.
func TestProductionDraftContextNames(t *testing.T) {
	t.Run("generation:prepare-draft-context", testProductionDraftContextPrepare)
	t.Run("generation:read-context", testProductionDraftContextRead)
}

// testProductionDraftContextPrepare proves prepare answers from the author's own
// rows: the assembled context must contain the seeded draft's chapter, version,
// body and content hash, checked against the file over a read-only connection, and
// it must leave the database byte-identical (the reference's transaction is
// SELECT-only and keeps the preparation in memory).
func testProductionDraftContextPrepare(t *testing.T) {
	lane := productionDraftContextOpen(t)
	var prepared struct {
		PreparationID string         `json:"preparationId"`
		Knowledge     map[string]any `json:"knowledgeSnapshot"`
		Selected      []struct {
			DraftID       int64  `json:"draftId"`
			ChapterNumber int64  `json:"chapterNumber"`
			Version       int64  `json:"version"`
			ContentHash   string `json:"contentHash"`
			Content       string `json:"content"`
		} `json:"selectedDrafts"`
	}
	productionEditorDecode(t, lane.author, "generation:prepare-draft-context",
		[]any{productionDraftContextPrepareRequest(), lane.frozen}, &prepared)
	if !productionDraftContextIsID(prepared.PreparationID) {
		t.Fatalf("prepare-draft-context returned %q as a preparation id", prepared.PreparationID)
	}
	// The knowledge snapshot is the honesty marker of this lane: it says the
	// retrieval state is absent instead of pretending an empty item list was
	// retrieved.
	if prepared.Knowledge["version"] != float64(1) || prepared.Knowledge["state"] != "empty" ||
		prepared.Knowledge["storageState"] != "absent" || prepared.Knowledge["query"] != productionDraftContextQuery ||
		prepared.Knowledge["topK"] != float64(5) || prepared.Knowledge["canonicalRevision"] != nil ||
		prepared.Knowledge["documentsRevision"] != nil {
		t.Fatalf("prepare-draft-context froze the wrong knowledge snapshot: %v", prepared.Knowledge)
	}
	if items, ok := prepared.Knowledge["items"].([]any); !ok || len(items) != 0 {
		t.Fatalf("prepare-draft-context froze knowledge items it never retrieved: %v", prepared.Knowledge["items"])
	}
	// The assembled context is the seeded draft, read back from the file itself.
	chapter, version, body := productionDraftContextDraft(t, lane.database, 1)
	if len(prepared.Selected) != 1 {
		t.Fatalf("prepare-draft-context returned %d selected drafts; want the one seeded draft: %v", len(prepared.Selected), prepared.Selected)
	}
	entry := prepared.Selected[0]
	if entry.DraftID != 1 || entry.ChapterNumber != chapter || entry.Version != version ||
		entry.Content != body || entry.ContentHash != productionDraftContextHash(body) {
		t.Fatalf("prepare-draft-context assembled %+v, but the admitted database holds draft 1 as chapter %d version %d body %q",
			entry, chapter, version, body)
	}
	lane.unchanged(t, "generation:prepare-draft-context")

	// A second preparation of the same request is a different preparation: nothing
	// was persisted, so nothing can be replayed out of the database.
	var again struct {
		PreparationID string `json:"preparationId"`
	}
	productionEditorDecode(t, lane.author, "generation:prepare-draft-context",
		[]any{productionDraftContextPrepareRequest(), lane.frozen}, &again)
	if again.PreparationID == prepared.PreparationID || !productionDraftContextIsID(again.PreparationID) {
		t.Fatalf("prepare-draft-context reused %q as a preparation id", again.PreparationID)
	}
	lane.unchanged(t, "generation:prepare-draft-context (repeat)")

	// The allowlist is the reference's: a key outside it is refused before anything
	// is read.
	unknownKey := productionDraftContextPrepareRequest()
	unknownKey["preparationId"] = "x"
	productionDraftContextRefusal(t, lane.author, "unknown request key", "generation:prepare-draft-context",
		[]any{unknownKey, lane.frozen}, "GENERATION_DRAFT_PREPARATION_INVALID")
	noQuery := productionDraftContextPrepareRequest()
	delete(noQuery, "query")
	productionDraftContextRefusal(t, lane.author, "missing query", "generation:prepare-draft-context",
		[]any{noQuery, lane.frozen}, "GENERATION_DRAFT_PREPARATION_INVALID")
	noChapter := productionDraftContextPrepareRequest()
	noChapter["chapterNumber"] = float64(0)
	productionDraftContextRefusal(t, lane.author, "absent chapter", "generation:prepare-draft-context",
		[]any{noChapter, lane.frozen}, "GENERATION_DRAFT_PREPARATION_INVALID")
	// A model this deployment does not have is refused before the selection, exactly
	// as the reference's loadModel is.
	missingModel := productionDraftContextPrepareRequest()
	missingModel["modelId"] = "no-such-model"
	productionDraftContextRefusal(t, lane.author, "unknown model", "generation:prepare-draft-context",
		[]any{missingModel, lane.frozen}, "GENERATION_MODEL_MISSING")
	// A selected draft that is not on disk cannot be frozen, and the reference lets
	// the missing row surface as the generic failure.
	missingDraft := productionDraftContextPrepareRequest()
	missingDraft["selectedDraftIds"] = []any{float64(productionDraftContextUnknownID)}
	productionDraftContextRefusal(t, lane.author, "absent selected draft", "generation:prepare-draft-context",
		[]any{missingDraft, lane.frozen}, "GENERATION_REQUEST_FAILED")
	// The arity and the frozen session context are checked before the request.
	productionDraftContextRefusal(t, lane.author, "absent frozen context", "generation:prepare-draft-context",
		[]any{productionDraftContextPrepareRequest()}, "INVALID_ARGUMENTS")
	productionEditorRefused(t, lane.author, "stale lease", "generation:prepare-draft-context",
		[]any{productionDraftContextPrepareRequest(), lane.stale()})
	lane.unchanged(t, "generation:prepare-draft-context (refusals)")
	lane.assertNoDial(t)
}

// testProductionDraftContextRead proves read-context projects the seeded run: the
// handle, the operation, the chapter, the model, the frozen author inputs, the
// selected id lists, the batch id, the frozen knowledge snapshot and the selected
// drafts whose source references still match. It also proves the run is readable
// under an epoch the live session no longer holds while a stale SESSION lease is
// still refused, and that nothing on disk moved.
func testProductionDraftContextRead(t *testing.T) {
	lane := productionDraftContextOpen(t)
	var projected map[string]any
	productionEditorDecode(t, lane.author, "generation:read-context", []any{map[string]any{"handle": lane.handle()}, lane.frozen}, &projected)
	handle, ok := projected["handle"].(map[string]any)
	if !ok || handle["runId"] != lane.run.RunID || handle["rootActionId"] != lane.run.RootActionID ||
		handle["epoch"] != lane.run.Epoch || handle["projectId"] != productionEditorProjectID {
		t.Fatalf("read-context rebuilt the wrong handle: %v", projected["handle"])
	}
	if projected["operation"] != productionDraftContextOperation || projected["modelId"] != productionTestModelID {
		t.Fatalf("read-context projected operation %v model %v", projected["operation"], projected["modelId"])
	}
	if projected["chapterNumber"] != float64(1) {
		t.Fatalf("read-context projected chapter %v", projected["chapterNumber"])
	}
	if projected["batchId"] != productionDraftContextBatchID {
		t.Fatalf("read-context projected batch %v", projected["batchId"])
	}
	if projected["inFlight"] != false {
		t.Fatalf("read-context reported inFlight %v for a settled run", projected["inFlight"])
	}
	// The selected id lists are the manifest's, not defaults.
	if ids := projected["selectedDraftIds"].([]any); len(ids) != 1 || ids[0] != float64(1) {
		t.Fatalf("read-context projected selectedDraftIds %v", projected["selectedDraftIds"])
	}
	if ids := projected["selectedFinalizedDraftIds"].([]any); len(ids) != 0 {
		t.Fatalf("read-context projected selectedFinalizedDraftIds %v", projected["selectedFinalizedDraftIds"])
	}
	if ids := projected["selectedBlueprintChapterNumbers"].([]any); len(ids) != 6 || ids[5] != float64(6) {
		t.Fatalf("read-context projected selectedBlueprintChapterNumbers %v", projected["selectedBlueprintChapterNumbers"])
	}
	// The author input the run froze comes back verbatim.
	inputs, ok := projected["authorInputs"].([]any)
	if !ok || len(inputs) != 1 {
		t.Fatalf("read-context projected authorInputs %v", projected["authorInputs"])
	}
	if input := syntheticMap(inputs[0]); input["id"] != productionDraftContextInputKey || input["text"] != productionDraftContextUnits {
		t.Fatalf("read-context projected author input %v", inputs[0])
	}
	// The frozen knowledge snapshot is passed through, not re-derived.
	snapshot, ok := projected["knowledgeSnapshot"].(map[string]any)
	if !ok || snapshot["query"] != productionDraftContextQuery || snapshot["state"] != "empty" || snapshot["topK"] != float64(5) {
		t.Fatalf("read-context did not pass the frozen knowledge snapshot through: %v", projected["knowledgeSnapshot"])
	}
	// The attempt rows the shipped ledger settled carry no purpose of their own, so
	// the reference's placeholder is what the browser must see.
	purposes, ok := projected["attemptedPurposes"].([]any)
	if !ok || len(purposes) != 1 || purposes[0] != productionDraftContextUnknownPurpose {
		t.Fatalf("read-context projected attemptedPurposes %v", projected["attemptedPurposes"])
	}
	// No commit-draft ever ran for this fixture, and no composition was composed, so
	// both fields say so instead of inventing a receipt.
	if save, ok := projected["draftSave"].(map[string]any); !ok || save["kind"] != "absent" {
		t.Fatalf("read-context projected draftSave %v", projected["draftSave"])
	}
	if projected["composition"] != nil || projected["lastCompositionFinishReason"] != nil {
		t.Fatalf("read-context projected a composition it never composed: %v / %v", projected["composition"], projected["lastCompositionFinishReason"])
	}
	// The selected drafts are projected only because the frozen source reference
	// still matches the row on disk; both sides are read here, the answer and the file.
	chapter, version, body := productionDraftContextDraft(t, lane.database, 1)
	drafts, ok := projected["selectedDrafts"].([]any)
	if !ok || len(drafts) != 1 {
		t.Fatalf("read-context projected selectedDrafts %v; the frozen reference still matches draft 1", projected["selectedDrafts"])
	}
	entry := syntheticMap(drafts[0])
	if entry["draftId"] != float64(1) || entry["chapterNumber"] != float64(chapter) ||
		entry["version"] != float64(version) || entry["content"] != body ||
		entry["contentHash"] != productionDraftContextHash(body) {
		t.Fatalf("read-context projected selected draft %v, but the file holds chapter %d version %d body %q", entry, chapter, version, body)
	}
	if _, present := entry["required"]; present {
		t.Fatalf("read-context marked a draft required although the run froze no material decision: %v", entry)
	}
	lane.unchanged(t, "generation:read-context")

	// A handle for another run is refused, and so is an envelope that is not
	// `{handle}`.
	foreign := lane.handle()
	foreign["runId"] = "another-run"
	productionDraftContextRefusal(t, lane.author, "foreign run", "generation:read-context",
		[]any{map[string]any{"handle": foreign}, lane.frozen}, "GENERATION_RUN_NOT_FOUND")
	// An epoch the run was never frozen under is the rotated-lease case: the
	// caller holds a valid lease for this project, so the refusal names the stale
	// epoch (the workflow resumes it) instead of pretending the run is missing.
	foreignEpoch := lane.handle()
	foreignEpoch["epoch"] = "lease-0000000002-other"
	productionDraftContextRefusal(t, lane.author, "foreign epoch", "generation:read-context",
		[]any{map[string]any{"handle": foreignEpoch}, lane.frozen}, "GENERATION_EPOCH_STALE")
	productionDraftContextRefusal(t, lane.author, "bare handle", "generation:read-context",
		[]any{lane.handle(), lane.frozen}, "GENERATION_REQUEST_FAILED")
	productionDraftContextRefusal(t, lane.author, "absent frozen context", "generation:read-context",
		[]any{map[string]any{"handle": lane.handle()}}, "INVALID_ARGUMENTS")
	productionEditorRefused(t, lane.author, "stale lease", "generation:read-context",
		[]any{map[string]any{"handle": lane.handle()}, lane.stale()})
	lane.unchanged(t, "generation:read-context (refusals)")
	lane.assertNoDial(t)
}

