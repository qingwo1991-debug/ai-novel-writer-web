//go:build linux

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// This file proves generation:begin-batch against a disposable admitted project:
// that it opens exactly one batch root and one run, that the run's frozen
// sourceManifest carries the validated intent, that a replayed uiActionNonce opens
// nothing new, that every malformed intent is refused with its own code and
// leaves the author database byte-identical, and - the strongest proof that the
// rows are the right ones - that the batch this operation created is served by the
// already-served generation:read-batch with the same derived progress.
//
// The fixture is the shipped baseline v7 database through productionEditorReadsRoot,
// the same one the editor and batch READ journeys use, with the one story profile
// committed for the generation purpose the run admission requires. The root and
// the run are written by the shipped ledger itself through the operation (not by
// hand), and every claim about them is then re-read over a SEPARATE mode=ro
// connection, so nothing is asserted against a value the serving connection
// happened to cache.
const (
	productionBatchWriteNonce       = "ci-begin-batch-nonce-1"
	productionBatchWriteStart       = int64(3)
	productionBatchWriteEnd         = int64(5)
	productionBatchWriteTargetUnits = int64(1200)
	productionBatchWriteIntentCode  = "GENERATION_BATCH_INTENT_INVALID"
	productionBatchWriteOpenID      = "batch-begin-open-1"
)

// productionBatchWriteLane is one started production lane over one admitted
// project plus the frozen three-key context of its only open session.
type productionBatchWriteLane struct {
	client      *productionDurableClient
	projectPath string
	database    string
	frozen      map[string]any
}

// newProductionBatchWriteLane starts the real production lane over a fresh copy
// of the baseline and opens its only project through HTTP, so every call below
// travels the adapter the renderer talks to.
func newProductionBatchWriteLane(t *testing.T) *productionBatchWriteLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	productionCommitGenerationPurpose(t, root, productionTestModelID)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	client := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	client.decode(t, "project:open", []any{projectPath, productionBatchWriteOpenID, projectPath}, &opened)
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
	return &productionBatchWriteLane{
		client:      client,
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		frozen:      frozen,
	}
}

// productionBatchWriteReadOnly opens the admitted project database over its own
// read-only connection: the ledger rows this operation writes are proved by a
// reader that cannot write them and shares nothing with the serving connection.
func productionBatchWriteReadOnly(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("the read-only ledger connection is unusable: %v", err)
	}
	return db
}

// productionBatchWriteCount is one COUNT over the read-only connection.
func productionBatchWriteCount(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

// productionBatchWriteRoots counts the batch roots this nonce owns and the runs
// under them. A batch IS that root plus that run, so both must be exactly one.
func productionBatchWriteRoots(t *testing.T, db *sql.DB, nonce string) (int64, int64) {
	t.Helper()
	roots := productionBatchWriteCount(t, db,
		`SELECT COUNT(*) FROM generation_roots WHERE json_extract(action_json,'$.operation')=? AND json_extract(action_json,'$.uiActionNonce')=?`,
		"batch-chapters", nonce)
	runs := productionBatchWriteCount(t, db,
		`SELECT COUNT(*) FROM generation_runs r JOIN generation_roots g ON g.root_action_id=r.root_action_id
 WHERE json_extract(g.action_json,'$.uiActionNonce')=?`, nonce)
	return roots, runs
}

// productionBatchWriteBinding reads the one run of this nonce over the read-only
// connection and returns its id plus the parsed binding_json the ledger froze.
func productionBatchWriteBinding(t *testing.T, db *sql.DB, nonce string) (string, map[string]any) {
	t.Helper()
	var runID, bindingJSON string
	if err := db.QueryRow(
		`SELECT r.run_id, r.binding_json FROM generation_runs r JOIN generation_roots g ON g.root_action_id=r.root_action_id
 WHERE json_extract(g.action_json,'$.uiActionNonce')=?`, nonce).Scan(&runID, &bindingJSON); err != nil {
		t.Fatalf("read the batch run back over the read-only connection: %v", err)
	}
	var binding map[string]any
	if err := json.Unmarshal([]byte(bindingJSON), &binding); err != nil {
		t.Fatalf("the frozen binding is not JSON: %v", err)
	}
	return runID, binding
}

// productionBatchWriteBeginRequest is the renderer's BeginGenerationBatchRequest
// (src/shared/generation-owner-contract.ts:230-236) as the workflow sends it
// (src/services/workflows/batch-chapter-workflow.ts:180-188), carrying no batchId:
// a batch root's identity is created by the operation, never supplied by a caller.
func productionBatchWriteBeginRequest() map[string]any {
	return map[string]any{
		"uiActionNonce": productionBatchWriteNonce,
		"modelId":       productionTestModelID,
		"mode":          "draft_review",
		"range": map[string]any{
			"startChapter": productionBatchWriteStart,
			"endChapter":   productionBatchWriteEnd,
		},
		"targetUnits": productionBatchWriteTargetUnits,
		"promptKeys":  []any{"first_chapter_draft", "next_chapter_draft"},
		"skillStages": []any{"drafting"},
		"authorInputs": []any{
			map[string]any{"id": "draft:target-units", "text": "1200"},
		},
	}
}

// productionBatchWriteAnswer decodes one mutating generation answer: the derived
// batch progress on success, or the business DTO's symbolic code
// ({"success":false,"error":"Error: CODE"}) on refusal, which is the envelope
// generation:begin answers with (internal/transport/generation.go:306-319).
func productionBatchWriteAnswer(t *testing.T, label string, response *httptest.ResponseRecorder) (json.RawMessage, string) {
	t.Helper()
	if response.Code == http.StatusNotImplemented {
		// A 501 would mean the binder installed nothing, which no assertion below
		// may mistake for a refusal of this request.
		t.Fatalf("generation:begin-batch is still GO_OPERATION_NOT_MIGRATED in the production lane, so this file's "+
			"binder is not installed: production_open_linux.go's binder needs the entry newProductionBatchWrite(owner).Bind(server) "+
			"inside the owner branch (production_open_linux.go:576-579, where newProductionComposition is bound): %s",
			response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", label, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	var dto struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(wire.Result, &dto); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	if dto.Success != nil && !*dto.Success {
		if !strings.HasPrefix(dto.Error, "Error: ") {
			t.Fatalf("%s: the refusal is not the family's business DTO: %s", label, response.Body.String())
		}
		return nil, strings.TrimPrefix(dto.Error, "Error: ")
	}
	return wire.Result, ""
}

// productionBatchWriteRead decodes one read-family answer, naming the unbound
// case explicitly so a missing binder can never pass as a served read.
func productionBatchWriteRead(t *testing.T, lane *productionBatchWriteLane, operation string, args []any, into any) {
	t.Helper()
	response := lane.client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented {
		t.Fatalf("%s is GO_OPERATION_NOT_MIGRATED in this lane: %s", operation, response.Body.String())
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

// TestProductionBatchBeginOpensOneRootAndRun is the whole write surface of
// generation:begin-batch. The subtests share one admitted project because the
// last two are about the batch the first one opened.
func TestProductionBatchBeginOpensOneRootAndRun(t *testing.T) {
	lane := newProductionBatchWriteLane(t)
	readOnly := productionBatchWriteReadOnly(t, lane.database)
	var batchID, rootID string
	var progress productionBatchProgressView

	t.Run("opens one root and one run carrying the intent", func(t *testing.T) {
		rootsBefore := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_roots")
		runsBefore := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_runs")
		attemptsBefore := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_attempts")
		raw, code := productionBatchWriteAnswer(t, "begin-batch",
			lane.client.call(t, "generation:begin-batch", []any{productionBatchWriteBeginRequest(), lane.frozen}))
		if code != "" {
			t.Fatalf("begin-batch refused an admitted intent with %q", code)
		}
		if err := json.Unmarshal(raw, &progress); err != nil {
			t.Fatalf("begin-batch did not return the batch progress: %v %s", err, raw)
		}
		batchID, rootID = progress.BatchID, progress.RootHandle.RootActionID
		if batchID == "" || rootID == "" {
			t.Fatalf("begin-batch returned no batch identity: %+v", progress)
		}
		if progress.Mode != "draft_review" || progress.Range.StartChapter != productionBatchWriteStart ||
			progress.Range.EndChapter != productionBatchWriteEnd || progress.TargetUnits != productionBatchWriteTargetUnits ||
			progress.ModelID != productionTestModelID {
			t.Fatalf("begin-batch derived the wrong intent: %+v", progress)
		}
		if progress.RootHandle != (productionBatchHandleView{
			ProjectID:    productionEditorProjectID,
			Epoch:        lane.frozen["leaseId"].(string),
			RootActionID: rootID,
			RunID:        batchID,
		}) {
			t.Fatalf("begin-batch derived the wrong root handle: %+v", progress.RootHandle)
		}
		// A fresh batch has drafted nothing: the cursor is on the first chapter of
		// the range and there is no pending child run yet.
		if len(progress.CompletedChapters) != 0 {
			t.Fatalf("a freshly opened batch already reports chapters: %+v", progress.CompletedChapters)
		}
		if progress.NextChapterNumber == nil || *progress.NextChapterNumber != productionBatchWriteStart {
			t.Fatalf("begin-batch derived nextChapterNumber %v; the batch starts at %d", progress.NextChapterNumber, productionBatchWriteStart)
		}
		if progress.CurrentChapterRunHandle != nil {
			t.Fatalf("a freshly opened batch reports a current chapter run: %+v", progress.CurrentChapterRunHandle)
		}
		if len(progress.AuthorInputs) != 1 || progress.AuthorInputs[0] != (productionBatchInputView{ID: "draft:target-units", Text: "1200"}) {
			t.Fatalf("begin-batch derived the wrong frozen author inputs: %+v", progress.AuthorInputs)
		}

		// The ledger rows, read back over the separate read-only connection.
		if roots := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_roots"); roots != rootsBefore+1 {
			t.Fatalf("begin-batch left %d roots; the admitted project had %d and a batch adds exactly one", roots, rootsBefore)
		}
		if runs := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_runs"); runs != runsBefore+1 {
			t.Fatalf("begin-batch left %d runs; the admitted project had %d and a batch adds exactly one", runs, runsBefore)
		}
		if attempts := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_attempts"); attempts != attemptsBefore {
			t.Fatalf("begin-batch created %d attempts; opening a batch dispatches nothing", attempts-attemptsBefore)
		}
		roots, runs := productionBatchWriteRoots(t, readOnly, productionBatchWriteNonce)
		if roots != 1 || runs != 1 {
			t.Fatalf("the batch nonce owns %d roots and %d runs; a batch is exactly one of each", roots, runs)
		}
		runID, binding := productionBatchWriteBinding(t, readOnly, productionBatchWriteNonce)
		if runID != batchID {
			t.Fatalf("the ledger run %q is not the batch id begin-batch returned (%q)", runID, batchID)
		}
		if binding["projectId"] != productionEditorProjectID || binding["epoch"] != lane.frozen["leaseId"] {
			t.Fatalf("the run is frozen outside this project session: %+v", binding)
		}
		manifest, ok := binding["sourceManifest"].(map[string]any)
		if !ok {
			t.Fatalf("the run carries no frozen source manifest: %+v", binding)
		}
		if manifest["operation"] != "batch-chapters" {
			t.Fatalf("the run's operation is %v; a batch root is batch-chapters", manifest["operation"])
		}
		intent, ok := manifest["batchIntent"].(map[string]any)
		if !ok {
			t.Fatalf("the run's sourceManifest carries no batchIntent: %+v", manifest)
		}
		chapterRange, ok := intent["range"].(map[string]any)
		if !ok {
			t.Fatalf("the frozen intent carries no range: %+v", intent)
		}
		if intent["mode"] != "draft_review" || chapterRange["startChapter"] != float64(productionBatchWriteStart) ||
			chapterRange["endChapter"] != float64(productionBatchWriteEnd) ||
			intent["targetUnits"] != float64(productionBatchWriteTargetUnits) {
			t.Fatalf("the frozen intent is not the request's: %+v", intent)
		}
		// A batch ROOT carries the intent; only its chapter children carry the id.
		if _, present := manifest["batchId"]; present {
			t.Fatalf("a batch root froze a batchId: %+v", manifest)
		}
		// generation:read-batch derives modelId from modelReceipt (batch_progress.go:466-471),
		// so a root without one is a batch no served read could project.
		receipt, ok := manifest["modelReceipt"].(map[string]any)
		if !ok || receipt["modelId"] != productionTestModelID {
			t.Fatalf("the frozen model receipt does not name the leased model: %+v", manifest["modelReceipt"])
		}
		if modelLease := manifest["modelLeaseId"]; modelLease == nil || modelLease == "" {
			t.Fatalf("the frozen manifest lost the model lease handle: %+v", manifest)
		}
	})

	t.Run("a replayed nonce opens nothing new", func(t *testing.T) {
		if batchID == "" {
			t.Fatal("the first subtest opened no batch to replay")
		}
		rootsBefore := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_roots")
		runsBefore := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_runs")
		raw, code := productionBatchWriteAnswer(t, "replayed nonce",
			lane.client.call(t, "generation:begin-batch", []any{productionBatchWriteBeginRequest(), lane.frozen}))
		switch code {
		case "":
			// The ledger returns the run it already holds for this idempotency key,
			// so a replay answers the same batch.
			var replay productionBatchProgressView
			if err := json.Unmarshal(raw, &replay); err != nil {
				t.Fatalf("the replayed nonce returned no progress: %v %s", err, raw)
			}
			if replay.BatchID != batchID {
				t.Fatalf("a replayed nonce opened batch %q; the first begin opened %q", replay.BatchID, batchID)
			}
		case "GENERATION_NONCE_CONFLICT":
			// The other honest answer: the same key frozen with a different model
			// lease id is the ledger's nonce conflict, refused before any INSERT.
		default:
			t.Fatalf("a replayed nonce was answered %q, which is neither the same batch nor the ledger's nonce conflict", code)
		}
		if roots := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_roots"); roots != rootsBefore {
			t.Fatalf("a replayed nonce left %d roots; the replay added %d", roots, roots-rootsBefore)
		}
		if runs := productionBatchWriteCount(t, readOnly, "SELECT COUNT(*) FROM generation_runs"); runs != runsBefore {
			t.Fatalf("a replayed nonce left %d runs; the replay added %d", runs, runs-runsBefore)
		}
		roots, runs := productionBatchWriteRoots(t, readOnly, productionBatchWriteNonce)
		if roots != 1 || runs != 1 {
			t.Fatalf("after the replay the nonce owns %d roots and %d runs; a batch is exactly one of each", roots, runs)
		}
	})

	t.Run("generation:read-batch derives the batch this operation opened", func(t *testing.T) {
		if batchID == "" {
			t.Fatal("the first subtest opened no batch to read")
		}
		var derived productionBatchProgressView
		productionBatchWriteRead(t, lane, "generation:read-batch",
			[]any{map[string]any{"batchId": batchID}, lane.frozen}, &derived)
		if !reflect.DeepEqual(derived, progress) {
			t.Fatalf("read-batch derived\n%+v\nbut begin-batch returned\n%+v", derived, progress)
		}
		// The served read and the read-only connection agree about which rows the
		// batch is: one run, whose id is the batch id.
		runID, _ := productionBatchWriteBinding(t, readOnly, productionBatchWriteNonce)
		if derived.BatchID != runID || derived.RootHandle.RunID != runID {
			t.Fatalf("read-batch named batch %q / run %q; the ledger holds %q", derived.BatchID, derived.RootHandle.RunID, runID)
		}
	})

	// Every malformed intent is refused by the intent gate alone: the code is the
	// reference's GENERATION_BATCH_INTENT_INVALID and the author database is
	// byte-identical afterwards, so nothing was opened, leased or written.
	for _, malformed := range []struct {
		label  string
		mutate func(map[string]any)
	}{
		{"refuses a mode that is not a batch mode", func(request map[string]any) {
			request["mode"] = "draft-then-publish"
		}},
		{"refuses an empty range", func(request map[string]any) {
			request["range"] = map[string]any{}
		}},
		{"refuses a range wider than the cap", func(request map[string]any) {
			request["range"] = map[string]any{"startChapter": 1, "endChapter": 11}
		}},
		{"refuses non-positive target units", func(request map[string]any) {
			request["targetUnits"] = 0
		}},
	} {
		malformed := malformed
		t.Run(malformed.label, func(t *testing.T) {
			request := productionBatchWriteBeginRequest()
			malformed.mutate(request)
			before := productionBatchBytes(t, lane.projectPath)
			_, code := productionBatchWriteAnswer(t, malformed.label,
				lane.client.call(t, "generation:begin-batch", []any{request, lane.frozen}))
			if code != productionBatchWriteIntentCode {
				t.Fatalf("%s was refused with %q instead of %s", malformed.label, code, productionBatchWriteIntentCode)
			}
			if after := productionBatchBytes(t, lane.projectPath); !bytes.Equal(before, after) {
				t.Fatalf("%s changed the admitted author database", malformed.label)
			}
		})
	}
}
