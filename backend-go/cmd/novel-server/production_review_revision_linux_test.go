//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewrevprep"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves the one review-revision name production_review_revision_linux.go
// binds and the begin-time freeze it supplies:
//
//   - `review-revision:prepare` answers `{contextId, context}` over a real admitted
//     project, the context being exactly what an independent capture of the same
//     request produces, and mints a fresh handle per call;
//   - `generation:begin` carrying that handle freezes the prepared context into the
//     run's source manifest, so the row the ledger really wrote carries the same
//     bytes and the same hash the producer computed;
//   - every refusal - no lease, a path this lane never opened, a source that moved,
//     a handle this process never minted, a handle prepared for another caller, an
//     expired handle, a non-review operation carrying a handle - answers its
//     symbolic code and opens no run.
//
// The prepared context and the frozen manifest are read back through connections
// the test owns (mode=ro), never through the lane's admitted handle, so every
// assertion is on what the producer and the ledger wrote.
const (
	// productionReviewRevisionBeginNonce names the one spend this file makes.
	productionReviewRevisionBeginNonce = "review-revision-begin-1"
	// productionReviewRevisionFocus is the review focus the renderer sends. It is
	// part of the frozen context, so a capture that dropped it could not match.
	productionReviewRevisionFocus = "重点检查开篇那扇门"
	// productionReviewRevisionStaleHash is a well-formed hash no draft has, so a
	// capture that trusted the renderer's contentHash instead of re-hashing the
	// stored body would answer instead of refusing.
	productionReviewRevisionStaleHash = "0000000000000000000000000000000000000000000000000000000000000000"
	// productionReviewRevisionAbsentHandle is an id this process never minted.
	productionReviewRevisionAbsentHandle = "00000000-0000-4000-8000-000000000000"
	// productionReviewRevisionOtherTab is the second tab the wrong-caller arm
	// presents; it holds no lease, exactly like a foreign webContents in the
	// reference.
	productionReviewRevisionOtherTab = "client_0123456789"
)

// TestProductionReviewRevisionPrepareAndFreeze is the family's served half.
func TestProductionReviewRevisionPrepareAndFreeze(t *testing.T) {
	t.Run("review-revision:prepare", testProductionReviewRevisionPrepare)
	t.Run("generation:begin freezes the prepared context", testProductionReviewRevisionBegin)
	t.Run("refusals", testProductionReviewRevisionRefusals)
	t.Run("registry refusals", testProductionReviewRevisionRegistryRefusals)
}

// productionReviewRevisionPrepareArgs is the request shape reviewRevisionRequest
// builds (review-revision-context.ts:23-30): the operation, the selected draft with
// the version the renderer saw, the author's inputs and the UI locale. The hash is
// recomputed by the test from the fixture's own body.
func productionReviewRevisionPrepareArgs(operation, contentHash string) map[string]any {
	return map[string]any{
		"operation": operation,
		"draftId":   int64(1),
		"expectedDraft": map[string]any{
			"chapterNumber": int64(1),
			"version":       int64(1),
			"status":        "draft",
			"contentHash":   contentHash,
		},
		"authorInputs": []any{map[string]any{"id": "review-focus", "text": productionReviewRevisionFocus}},
		"uiLocale":     store.ReviewRevisionLocaleZH,
	}
}

// productionReviewRevisionContentHash is summaryHash over a stored body: the
// producer hashes the draft's text (review_revision_context.go:484), never a
// renderer-supplied value.
func productionReviewRevisionContentHash(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}

// productionReviewRevisionFrozen is one prepared answer as the browser sees it.
type productionReviewRevisionFrozen struct {
	ContextID string
	Context   map[string]any
}

// productionReviewRevisionPrepareOnce prepares one context over the lane and returns
// the answered handle and context.
func productionReviewRevisionPrepareOnce(t *testing.T, lane *productionGenerationWriteLane,
	request map[string]any) productionReviewRevisionFrozen {
	t.Helper()
	raw := productionEditorCall(t, lane.author, productionReviewRevisionPrepare, []any{request, lane.frozen})
	var answered struct {
		ContextID string         `json:"contextId"`
		Context   map[string]any `json:"context"`
	}
	if err := json.Unmarshal(raw, &answered); err != nil {
		t.Fatalf("%s: %v %s", productionReviewRevisionPrepare, err, raw)
	}
	if answered.ContextID == "" {
		t.Fatalf("%s answered no contextId: %s", productionReviewRevisionPrepare, raw)
	}
	if answered.Context == nil {
		t.Fatalf("%s answered no context: %s", productionReviewRevisionPrepare, raw)
	}
	return productionReviewRevisionFrozen{ContextID: answered.ContextID, Context: answered.Context}
}

// productionReviewRevisionCapture is the independent side of the same request: the
// producer over a connection of the test's own, so the answered context and the
// frozen manifest are compared against what the store computes.
func productionReviewRevisionCapture(t *testing.T, projectPath, projectID string,
	request map[string]any) *store.FrozenReviewRevisionContext {
	t.Helper()
	frozen, err := store.NewReviewRevisionContexts(productionStoryStateWriteReadback(t, projectPath)).
		Capture(context.Background(), projectID, request)
	if err != nil {
		t.Fatalf("the test's own capture was refused: %v", err)
	}
	if frozen == nil || frozen.Text == "" || frozen.ContextHash == "" {
		t.Fatal("the test's own capture answered an empty context")
	}
	return frozen
}

// productionReviewRevisionCanonical re-encodes a JSON value through one encoder, so
// two documents that differ only in object key order compare equal.
func productionReviewRevisionCanonical(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("canonical encoding: %v", err)
	}
	return string(raw)
}

// productionReviewRevisionSelection is the renderer's begin request for a prepared
// context (review-revision-generation.ts:119-138). The author input carries the JSON
// the browser really holds - the answered context re-serialized, whose key order is
// not the frozen order - so the admission's comparison is proved to compare values
// rather than bytes.
func productionReviewRevisionSelection(t *testing.T, handle string, answered productionReviewRevisionFrozen) map[string]any {
	t.Helper()
	selection := map[string]any{}
	selection["operation"] = store.ReviewRevisionOperationReviewChapter
	selection["uiActionNonce"] = productionReviewRevisionBeginNonce
	selection["modelId"] = productionTestModelID
	selection["chapterNumber"] = int64(1)
	selection["selectedDraftIds"] = []any{int64(1)}
	selection["selectedFinalizedDraftIds"] = []any{}
	selection["selectedBlueprintChapterNumbers"] = []any{}
	selection["promptKeys"] = []any{"consistency_check"}
	selection["skillStages"] = []any{"review"}
	selection["authorInputs"] = []any{map[string]any{
		"id":   productionReviewRevisionAuthorInputID,
		"text": productionReviewRevisionCanonical(t, answered.Context),
	}}
	selection["output"] = "structured-data"
	selection["reviewRevisionContextId"] = handle
	return selection
}

// productionReviewRevisionManifest reads one run's frozen source manifest back over
// a connection of the test's own. The values stay raw JSON so a field's bytes can be
// compared, not only its decoded value.
func productionReviewRevisionManifest(t *testing.T, projectPath, runID string) map[string]json.RawMessage {
	t.Helper()
	var binding string
	productionStoryStateWriteScan(t, productionStoryStateWriteReadback(t, projectPath),
		"SELECT binding_json FROM generation_runs WHERE run_id=?", []any{runID}, &binding)
	var stored struct {
		SourceManifest map[string]json.RawMessage `json:"sourceManifest"`
	}
	if err := json.Unmarshal([]byte(binding), &stored); err != nil {
		t.Fatalf("the committed binding is not JSON: %v", err)
	}
	if stored.SourceManifest == nil {
		t.Fatal("the committed run froze no source manifest")
	}
	return stored.SourceManifest
}

// testProductionReviewRevisionPrepare proves the served prepare is the producer's
// own answer: the operation, source, config and author inputs an independent capture
// computes, answered under a fresh handle each time.
func testProductionReviewRevisionPrepare(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	projectID := syntheticString(lane.frozen["projectId"])
	request := productionReviewRevisionPrepareArgs(store.ReviewRevisionOperationReviewChapter,
		productionReviewRevisionContentHash(productionEditorSeed))

	prepared := productionReviewRevisionPrepareOnce(t, lane, request)
	frozen := productionReviewRevisionCapture(t, lane.projectPath, projectID, request)
	// Both sides go through one decoder before they are compared. json.Marshal
	// echoes a json.RawMessage verbatim, so marshalling the raw frozen text would
	// compare the producer's key ORDER against a map's sorted keys and fail for a
	// reason that has nothing to do with the values. The frozen order is compared
	// where it matters - the hash the run freezes below.
	var frozenValue map[string]any
	if err := json.Unmarshal([]byte(frozen.Text), &frozenValue); err != nil {
		t.Fatalf("the producer's frozen text is not an object: %v", err)
	}
	if got, want := productionReviewRevisionCanonical(t, prepared.Context),
		productionReviewRevisionCanonical(t, frozenValue); got != want {
		t.Fatalf("the answered context is not the producer's context:\n got %s\nwant %s", got, want)
	}
	// The fields the workflow reads back (review-revision-command.ts:104-118).
	source := syntheticMap(prepared.Context["source"])
	if source == nil || syntheticNumber(source["id"]) != 1 || syntheticNumber(source["chapterNumber"]) != 1 ||
		syntheticNumber(source["version"]) != 1 || syntheticString(source["status"]) != "draft" {
		t.Fatalf("the answered context identifies another draft: %v", source)
	}
	if syntheticString(prepared.Context["operation"]) != store.ReviewRevisionOperationReviewChapter {
		t.Fatalf("the answered context names another operation: %v", prepared.Context["operation"])
	}
	if syntheticString(prepared.Context["sourceHash"]) != productionReviewRevisionContentHash(productionEditorSeed) {
		t.Fatalf("the answered context carries another source hash: %v", prepared.Context["sourceHash"])
	}
	if syntheticNumber(prepared.Context["version"]) != 1 {
		t.Fatalf("the answered context is not version 1: %v", prepared.Context["version"])
	}
	// A second prepare mints a second handle over the same bytes: a repeated context
	// id would be a cache, and the reference's remember() never reuses one.
	again := productionReviewRevisionPrepareOnce(t, lane, request)
	if again.ContextID == prepared.ContextID {
		t.Fatalf("two prepares answered the same handle %v", again.ContextID)
	}
	if got, want := productionReviewRevisionCanonical(t, again.Context),
		productionReviewRevisionCanonical(t, prepared.Context); got != want {
		t.Fatalf("two captures of one request disagree:\n%s\n%s", got, want)
	}
}

// testProductionReviewRevisionBegin proves generation:begin freezes the prepared
// context: the run the ledger wrote carries the producer's exact bytes under
// reviewRevisionContext, their sha256 under reviewRevisionContextHash, the captured
// operation, and exactly one author input - the fields the review-cycle verifier
// reads before it admits an effect (reviewcycle_effect.go:166-207).
func testProductionReviewRevisionBegin(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	projectID := syntheticString(lane.frozen["projectId"])
	request := productionReviewRevisionPrepareArgs(store.ReviewRevisionOperationReviewChapter,
		productionReviewRevisionContentHash(productionEditorSeed))
	prepared := productionReviewRevisionPrepareOnce(t, lane, request)
	frozen := productionReviewRevisionCapture(t, lane.projectPath, projectID, request)

	handle := lane.begin(t, productionReviewRevisionSelection(t, prepared.ContextID, prepared))
	runID, _ := handle["runId"].(string)
	if runID == "" {
		t.Fatalf("the admitted run carries no id: %v", handle)
	}
	manifest := productionReviewRevisionManifest(t, lane.projectPath, runID)
	// The bytes: the context the ledger holds is the producer's Text.
	contextBytes := string(manifest["reviewRevisionContext"])
	if contextBytes != frozen.Text {
		t.Fatalf("the frozen manifest holds another context:\n got %s\nwant %s", contextBytes, frozen.Text)
	}
	// The hash: what the manifest claims is the sha256 of the bytes it stored, and it
	// equals the hash the producer computed.
	if got := productionReviewRevisionContentHash(contextBytes); got != frozen.ContextHash {
		t.Fatalf("the frozen context hashes to %s; the producer computed %s", got, frozen.ContextHash)
	}
	var frozenHash string
	if err := json.Unmarshal(manifest["reviewRevisionContextHash"], &frozenHash); err != nil {
		t.Fatalf("reviewRevisionContextHash is not text: %v", err)
	}
	if frozenHash != frozen.ContextHash {
		t.Fatalf("the manifest froze %s; the producer computed %s", frozenHash, frozen.ContextHash)
	}
	// The operation the context was captured for, not the begin's own field.
	var operation string
	if err := json.Unmarshal(manifest["operation"], &operation); err != nil || operation != store.ReviewRevisionOperationReviewChapter {
		t.Fatalf("the frozen manifest names operation %v (%v)", operation, err)
	}
	// Exactly one author input, carrying the frozen bytes under the id the verifier
	// requires.
	var inputs []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(manifest["authorInputs"], &inputs); err != nil {
		t.Fatalf("authorInputs is not a list of entries: %v", err)
	}
	if len(inputs) != 1 || inputs[0].ID != productionReviewRevisionAuthorInputID || inputs[0].Text != frozen.Text {
		t.Fatalf("the frozen manifest holds %d author inputs; want the one frozen context: %v", len(inputs), inputs)
	}
}

// productionReviewRevisionRefused asserts one prepare refusal: a symbolic code and
// no 200. The transport publishes a named refusal's code in the error body, exactly
// like every other binder in this lane (transport/public.go:18-32).
func productionReviewRevisionRefused(t *testing.T, client *productionDurableClient, label string,
	args []any, want ...string) {
	t.Helper()
	response := client.call(t, productionReviewRevisionPrepare, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s: %s is still the lane's fail-closed 501: %s", label, productionReviewRevisionPrepare, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%s: %s answered 200 for a refused prepare: %s", label, productionReviewRevisionPrepare, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	for _, code := range want {
		if body.Error.Code == code {
			return
		}
	}
	t.Fatalf("%s: refused with %v; want one of %v: %s", label, body.Error.Code, want, response.Body.String())
}

// testProductionReviewRevisionRefusals is the family's refusal half. Each arm is a
// state the reference also refuses, and none of them may open a run or write a byte.
func testProductionReviewRevisionRefusals(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	projectID := syntheticString(lane.frozen["projectId"])
	hash := productionReviewRevisionContentHash(productionEditorSeed)
	request := productionReviewRevisionPrepareArgs(store.ReviewRevisionOperationReviewChapter, hash)
	prepared := productionReviewRevisionPrepareOnce(t, lane, request)

	baseline := lane.bytes(t)
	// No lease: the frozen context is the transport envelope, so a body without one
	// cannot name a database. This is the reference's absent session
	// (generation-controller.ts:184).
	noLease := map[string]any{"projectId": projectID, "projectPath": lane.projectPath, "leaseId": ""}
	productionReviewRevisionRefused(t, lane.author, "absent lease", []any{request, noLease}, "PROJECT_SESSION_REQUIRED")
	// A tab that never opened this project is not an owner.
	foreign := newProductionDurableClient(t, lane.author.handler, productionReviewRevisionOtherTab)
	productionReviewRevisionRefused(t, foreign, "foreign tab", []any{request, lane.frozen},
		"PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE", "PROJECT_NOT_OPEN")
	// A path this lane never admitted: the borrow refuses before any handle is
	// resolved.
	stalePath := map[string]any{"projectId": projectID, "projectPath": lane.projectPath + "-never-opened", "leaseId": lane.frozen["leaseId"]}
	productionReviewRevisionRefused(t, lane.author, "unopened path", []any{request, stalePath},
		"PROJECT_NOT_OPEN", "PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED")
	// The author moved the draft after the renderer read it: the expected hash and
	// the expected version both disagree with the stored row (:58-60).
	productionReviewRevisionRefused(t, lane.author, "moved content",
		[]any{productionReviewRevisionPrepareArgs(store.ReviewRevisionOperationReviewChapter, productionReviewRevisionStaleHash), lane.frozen},
		"GENERATION_REVIEW_SOURCE_CHANGED")
	movedVersion := productionReviewRevisionPrepareArgs(store.ReviewRevisionOperationReviewChapter, hash)
	movedVersion["expectedDraft"].(map[string]any)["version"] = int64(2)
	productionReviewRevisionRefused(t, lane.author, "moved version", []any{movedVersion, lane.frozen},
		"GENERATION_REVIEW_SOURCE_CHANGED")
	// The one lineage arm this lane refuses instead of under-freezing: a recheck needs
	// ReviewCycleRepository.planRecheck and a human-confirmed-review snapshot codec,
	// and neither is ported.
	productionReviewRevisionRefused(t, lane.author, "refine from review",
		[]any{productionReviewRevisionPrepareArgs(store.ReviewRevisionOperationRefineFromReview, hash), lane.frozen},
		"REVIEW_REVISION_CONTEXT_CONFIRMATION_UNAVAILABLE")
	// An operation outside REVIEW_REVISION_OPERATIONS has no frozen context at all.
	productionReviewRevisionRefused(t, lane.author, "unknown operation",
		[]any{productionReviewRevisionPrepareArgs(productionEditorGenerationOperation, hash), lane.frozen},
		"GENERATION_REVIEW_CONTEXT_INVALID")
	// A malformed request is the same code, not a database read.
	productionReviewRevisionRefused(t, lane.author, "non-object request", []any{"review", lane.frozen},
		"GENERATION_REVIEW_CONTEXT_INVALID")

	// A handle this process never minted is "prepare again", the reference's own
	// answer when its map has no entry (:120).
	absent := productionReviewRevisionSelection(t, productionReviewRevisionAbsentHandle, prepared)
	if code := productionLifecycleRefusal(t, "absent handle",
		lane.author.call(t, "generation:begin", []any{absent, lane.frozen})); code != "GENERATION_REVIEW_CONTEXT_REQUIRED" {
		t.Fatalf("a begin carrying an unminted handle refused with %v; want GENERATION_REVIEW_CONTEXT_REQUIRED", code)
	}
	// A handle that does not belong to the operation the begin asks for is a misuse
	// of a handle the caller does hold: the registry's purpose does not match.
	refine := productionReviewRevisionSelection(t, prepared.ContextID, prepared)
	refine["operation"] = store.ReviewRevisionOperationRefineDraft
	refine["promptKeys"] = []any{"refine_chapter"}
	refine["skillStages"] = []any{"refinement"}
	refine["output"] = "visible-text"
	if code := productionLifecycleRefusal(t, "handle for another operation",
		lane.author.call(t, "generation:begin", []any{refine, lane.frozen})); code != "GENERATION_REVIEW_CONTEXT_INVALID" {
		t.Fatalf("a cross-operation handle refused with %v; want GENERATION_REVIEW_CONTEXT_INVALID", code)
	}
	// A selection that does not match the context it carries - here a prompt key the
	// frozen operation never uses - is refused rather than frozen.
	wrongPrompt := productionReviewRevisionSelection(t, prepared.ContextID, prepared)
	wrongPrompt["promptKeys"] = []any{"opening"}
	if code := productionLifecycleRefusal(t, "mismatched selection",
		lane.author.call(t, "generation:begin", []any{wrongPrompt, lane.frozen})); code != "GENERATION_REVIEW_CONTEXT_MISMATCH" {
		t.Fatalf("a mismatched selection refused with %v; want GENERATION_REVIEW_CONTEXT_MISMATCH", code)
	}
	// An author input that is not the frozen context is a different context.
	wrongInput := productionReviewRevisionSelection(t, prepared.ContextID, prepared)
	wrongInput["authorInputs"] = []any{map[string]any{"id": productionReviewRevisionAuthorInputID, "text": `{"version":1}`}}
	if code := productionLifecycleRefusal(t, "other author input",
		lane.author.call(t, "generation:begin", []any{wrongInput, lane.frozen})); code != "GENERATION_REVIEW_CONTEXT_MISMATCH" {
		t.Fatalf("another author input refused with %v; want GENERATION_REVIEW_CONTEXT_MISMATCH", code)
	}
	// A non-review operation carrying a handle cannot use it: the reference throws
	// GENERATION_REVIEW_CONTEXT_INVALID for exactly this (:118).
	nonReview := productionEditorBeginRequest(productionReviewRevisionBeginNonce)
	nonReview["reviewRevisionContextId"] = prepared.ContextID
	if code := productionLifecycleRefusal(t, "handle on a non-review begin",
		lane.author.call(t, "generation:begin", []any{nonReview, lane.frozen})); code != "GENERATION_REVIEW_CONTEXT_INVALID" {
		t.Fatalf("a handle on a non-review begin refused with %v; want GENERATION_REVIEW_CONTEXT_INVALID", code)
	}
	// A stale lease is the reused admission's own refusal, taken before any handle.
	stale := productionReviewRevisionSelection(t, prepared.ContextID, prepared)
	if code := productionLifecycleRefusal(t, "stale lease",
		lane.author.call(t, "generation:begin", []any{stale, lane.stale()})); code == "" {
		t.Fatal("a stale-lease begin was refused without a symbolic code")
	}
	// No refusal opened a run, and the author's bytes are the bytes the fixture wrote.
	var runs int64
	productionStoryStateWriteScan(t, productionStoryStateWriteReadback(t, lane.projectPath),
		"SELECT COUNT(*) FROM generation_runs WHERE binding_json LIKE ?", []any{"%reviewRevisionContext%"}, &runs)
	if runs != 0 {
		t.Fatalf("a refused begin opened %d review-revision runs", runs)
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused review-revision call changed the committed author database")
	}
}

// productionReviewRevisionFailureCode is the code the browser would receive for a
// refusal, through the production mapping layer.
func productionReviewRevisionFailureCode(err error) string {
	mapped := productionReviewRevisionFailure(err)
	if mapped == nil {
		return ""
	}
	return mapped.Error()
}

// testProductionReviewRevisionRegistryRefusals drives the production freeze function
// directly, over a registry whose clock and caller the test owns, for the two arms
// one process cannot reach over HTTP: a handle prepared for another caller, and a
// handle whose TTL has run out. Both are pure refusals: the function never writes.
func testProductionReviewRevisionRegistryRefusals(t *testing.T) {
	_, projectPath, _, _, _ := productionEditorReadsRoot(t)
	request := productionReviewRevisionPrepareArgs(store.ReviewRevisionOperationReviewChapter,
		productionReviewRevisionContentHash(productionEditorSeed))
	frozen := productionReviewRevisionCapture(t, projectPath, productionEditorProjectID, request)

	now := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	key := bytes.Repeat([]byte{0x5a}, productionReviewRevisionServerKeyBytes)
	registry, err := reviewrevprep.NewStore(key, reviewrevprep.StoreOptions{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("the test's registry was refused: %v", err)
	}
	owner := transport.Caller{Principal: "production-author", SessionID: "session-1", ClientID: productionEditorTab}
	preparedContext := reviewrevprep.PreparedContext{}
	preparedContext.ProjectID = productionEditorProjectID
	preparedContext.Operation = frozen.Operation
	preparedContext.DraftID = frozen.DraftID
	preparedContext.ChapterNumber = frozen.ChapterNumber
	preparedContext.Version = frozen.Version
	preparedContext.Status = frozen.Status
	preparedContext.SourceHash = frozen.SourceHash
	preparedContext.Text = frozen.Text
	preparedContext.ContextHash = frozen.ContextHash
	handle, err := registry.Prepare(productionReviewRevisionBinding(owner, "epoch-1", frozen.Operation), preparedContext)
	if err != nil {
		t.Fatalf("the test's registry refused a real context: %v", err)
	}
	db := productionStoryStateWriteReadback(t, projectPath)
	scope := generationowner.Scope{Caller: "production-author", Session: "session-1", Tab: productionEditorTab, Project: productionEditorProjectID, Epoch: "epoch-1"}
	selection := &syntheticSelection{}
	selection.Operation = frozen.Operation
	selection.HasChapterNumber = true
	selection.ChapterNumber = frozen.ChapterNumber
	selection.SelectedDraftIDs = []int64{frozen.DraftID}
	selection.PromptKeys = []string{"consistency_check"}
	selection.SkillStages = []any{"review"}
	selection.Output = "structured-data"
	selection.AuthorInputs = []map[string]any{{"id": productionReviewRevisionAuthorInputID, "text": frozen.Text}}
	selection.ReviewRevisionContextID = handle

	// The admitted arm first, so the refusals below are proven against a function
	// that does work: the frozen fields are the producer's own.
	extra, err := productionReviewRevisionFreezeWith(registry, context.Background(), owner, db, scope, selection, handle)
	if err != nil {
		t.Fatalf("the production freeze refused a handle it minted: %v", err)
	}
	if got := syntheticString(extra["reviewRevisionContextHash"]); got != frozen.ContextHash {
		t.Fatalf("the freeze answered %v; the producer computed %v", got, frozen.ContextHash)
	}
	raw, ok := extra["reviewRevisionContext"].(json.RawMessage)
	if !ok || string(raw) != frozen.Text {
		t.Fatalf("the freeze answered another context: %v", extra["reviewRevisionContext"])
	}
	if len(selection.AuthorInputs) != 1 || syntheticString(selection.AuthorInputs[0]["text"]) != frozen.Text ||
		syntheticString(selection.AuthorInputs[0]["id"]) != productionReviewRevisionAuthorInputID {
		t.Fatalf("the freeze did not rewrite the selection's author input: %v", selection.AuthorInputs)
	}
	// Another caller, another process in effect: the same handle is not theirs.
	other := transport.Caller{Principal: "production-author", SessionID: "session-1", ClientID: productionReviewRevisionOtherTab}
	if _, err := productionReviewRevisionFreezeWith(registry, context.Background(), other, db, scope, selection, handle); productionReviewRevisionFailureCode(err) != "GENERATION_REVIEW_CONTEXT_INVALID" {
		t.Fatalf("a handle presented by another caller refused with %v", err)
	}
	// The registry's TTL is IMPORT_INSPECTION_TTL_MS, ten minutes; one minute past it
	// the handle is collected and the answer is "prepare again".
	now = now.Add(11 * time.Minute)
	if _, err := productionReviewRevisionFreezeWith(registry, context.Background(), owner, db, scope, selection, handle); productionReviewRevisionFailureCode(err) != "GENERATION_REVIEW_CONTEXT_REQUIRED" {
		t.Fatalf("an expired handle refused with %v", err)
	}
	// An id this registry never minted, before the clock matters at all.
	if _, err := productionReviewRevisionFreezeWith(registry, context.Background(), owner, db, scope, selection,
		productionReviewRevisionAbsentHandle); productionReviewRevisionFailureCode(err) != "GENERATION_REVIEW_CONTEXT_REQUIRED" {
		t.Fatalf("an unminted handle refused with %v", err)
	}
}

// productionReviewRevisionCommitCopy is the wire request plus its two nested objects,
// copied so one table arm cannot edit the next one.
func productionReviewRevisionCommitCopy(source map[string]any) map[string]any {
	copied := make(map[string]any, len(source))
	for key, value := range source {
		copied[key] = value
	}
	for _, key := range []string{"handle", "artifact"} {
		inner, ok := source[key].(map[string]any)
		if !ok {
			continue
		}
		clone := make(map[string]any, len(inner))
		for innerKey, innerValue := range inner {
			clone[innerKey] = innerValue
		}
		copied[key] = clone
	}
	return copied
}

// TestProductionReviewRevisionCommitRequestReadsTheWireShape pins the commit request
// to its exact shape: three request keys, four handle keys, three artifact keys, a
// whole non-negative revision, a lowercase digest, and the lease the lane admits. A
// request this file does not read is refused before the store sees it, so a wrong
// shape can never reach the transaction.
func TestProductionReviewRevisionCommitRequestReadsTheWireShape(t *testing.T) {
	contextValue := projectsession.Context{ProjectID: "proj-commit", LeaseID: "lease-commit",
		ProjectPath: "/tmp/project"}
	digest := productionReviewRevisionContentHash("an answer")
	valid := map[string]any{
		"contextId": "00000000-0000-4000-8000-000000000001",
		"handle": map[string]any{
			"projectId":    "proj-commit",
			"epoch":        "lease-commit",
			"rootActionId": "root-1",
			"runId":        "run-1",
		},
		"artifact": map[string]any{"artifactId": "artifact-1", "revision": int64(0), "textHash": digest},
	}
	parsed, err := productionReviewRevisionCommitRequest(valid, contextValue)
	if err != nil {
		t.Fatalf("the production parser refused a well-formed commit: %v", err)
	}
	if parsed.ContextID != "00000000-0000-4000-8000-000000000001" ||
		parsed.Request.ProjectID != "proj-commit" || parsed.Request.LiveEpoch != "lease-commit" ||
		parsed.Request.Handle.ProjectID != "proj-commit" || parsed.Request.Handle.Epoch != "lease-commit" ||
		parsed.Request.Handle.RootActionID != "root-1" || parsed.Request.Handle.RunID != "run-1" ||
		parsed.Request.Artifact.ArtifactID != "artifact-1" || parsed.Request.Artifact.Revision != 0 ||
		parsed.Request.Artifact.TextHash != digest || parsed.Request.AssertPrepared != nil {
		t.Fatalf("the parser read the request as %+v", parsed)
	}
	for _, arm := range []struct {
		label  string
		mutate func(map[string]any)
	}{
		{"an extra request key", func(request map[string]any) { request["extra"] = true }},
		{"a missing context id", func(request map[string]any) { delete(request, "contextId") }},
		{"a blank context id", func(request map[string]any) { request["contextId"] = "   " }},
		{"a handle that is not an object", func(request map[string]any) { request["handle"] = "root-1" }},
		{"an extra handle key", func(request map[string]any) {
			request["handle"].(map[string]any)["extra"] = true
		}},
		{"a missing handle key", func(request map[string]any) {
			delete(request["handle"].(map[string]any), "runId")
		}},
		{"a blank run id", func(request map[string]any) {
			request["handle"].(map[string]any)["runId"] = ""
		}},
		{"an artifact that is not an object", func(request map[string]any) { request["artifact"] = digest }},
		{"a missing artifact key", func(request map[string]any) {
			delete(request["artifact"].(map[string]any), "textHash")
		}},
		{"an uppercase digest", func(request map[string]any) {
			request["artifact"].(map[string]any)["textHash"] = strings.ToUpper(digest)
		}},
		{"a short digest", func(request map[string]any) {
			request["artifact"].(map[string]any)["textHash"] = digest[:63]
		}},
		{"a fractional revision", func(request map[string]any) {
			request["artifact"].(map[string]any)["revision"] = 1.5
		}},
		{"a negative revision", func(request map[string]any) {
			request["artifact"].(map[string]any)["revision"] = int64(-1)
		}},
		{"a revision that is a string", func(request map[string]any) {
			request["artifact"].(map[string]any)["revision"] = "0"
		}},
		{"another project's handle", func(request map[string]any) {
			request["handle"].(map[string]any)["projectId"] = "proj-other"
		}},
	} {
		request := productionReviewRevisionCommitCopy(valid)
		arm.mutate(request)
		if _, err := productionReviewRevisionCommitRequest(request, contextValue); err == nil {
			t.Fatalf("the production parser accepted %s", arm.label)
		}
	}
}

// TestProductionReviewRevisionCommitRefusalCodes proves the commit half's codes are
// reachable at the boundary: each sentinel resolves to the reference's own string
// through the one mapping table, and wrapping a refusal never widens it into a
// neighbour's code.
func TestProductionReviewRevisionCommitRefusalCodes(t *testing.T) {
	for _, arm := range []struct {
		refusal error
		code    string
	}{
		{store.ErrReviewCommitInvalid, "GENERATION_REVIEW_COMMIT_INVALID"},
		{store.ErrReviewCommitConflict, "GENERATION_REVIEW_COMMIT_CONFLICT"},
		{store.ErrReviewCommitArtifact, "GENERATION_REVIEW_ARTIFACT_INVALID"},
		{store.ErrReviewCommitIdentity, "GENERATION_RUN_IDENTITY_MISMATCH"},
		{store.ErrReviewCommitCancelled, "GENERATION_ACTION_CANCELLED"},
		{store.ErrReviewCommitReceipt, "GENERATION_REVIEW_RECEIPT_INVALID"},
		{store.ErrReviewCommitContextRequired, "GENERATION_REVIEW_CONTEXT_REQUIRED"},
		{store.ErrReviewCommitContextUnproven, "GENERATION_REVIEW_CONTEXT_INVALID"},
		{store.ErrReviewCommitContextChanged, "GENERATION_REVIEW_CONTEXT_CHANGED"},
		{store.ErrReviewCycleConflict, "REVIEW_CYCLE_CONFLICT"},
		{store.ErrSourceDraftChanged, "GENERATION_SOURCE_CHANGED"},
		{reviewrevprep.ErrHandleUnknown, "GENERATION_REVIEW_CONTEXT_REQUIRED"},
		{reviewrevprep.ErrHandleExpired, "GENERATION_REVIEW_CONTEXT_REQUIRED"},
		{reviewrevprep.ErrHandleRevoked, "GENERATION_REVIEW_CONTEXT_REQUIRED"},
		{reviewrevprep.ErrCallerMismatch, "GENERATION_REVIEW_CONTEXT_INVALID"},
		{reviewrevprep.ErrPurposeMismatch, "GENERATION_REVIEW_CONTEXT_INVALID"},
	} {
		if got := productionReviewRevisionCode(arm.refusal); got != arm.code {
			t.Fatalf("a named refusal resolves to %v instead of its own code", got)
		}
		if got := productionReviewRevisionFailureCode(arm.refusal); got != arm.code {
			t.Fatalf("the browser would receive %v instead of %v", got, arm.code)
		}
	}
	wrapped := fmt.Errorf("commit: %w", store.ErrReviewCommitConflict)
	if got := productionReviewRevisionFailureCode(wrapped); got != "GENERATION_REVIEW_COMMIT_CONFLICT" {
		t.Fatalf("a wrapped conflict answered %v", got)
	}
	// A failure this lane does not name is the family's invalid code, never a SQLite
	// message and never a draft body.
	if got := productionReviewRevisionFailureCode(errors.New("sqlite3: disk I/O error")); got !=
		errProductionReviewRevisionInvalid.Error() {
		t.Fatalf("an unnamed failure answered %v", got)
	}
}

// TestProductionReviewRevisionCommitPrepared proves the closure the binder hands the
// writer: a handle this process minted answers for its own operation and its own
// bytes, and every other presentation is the boundary's published answer rather than
// a write. Only the bytes arm reaches the store's own sentinel; the rest are the
// registry's identity refusals, which the mapping table publishes.
func TestProductionReviewRevisionCommitPrepared(t *testing.T) {
	handle := "00000000-0000-4000-8000-000000000002"
	caller := transport.Caller{Principal: "production-author", SessionID: "session-1", ClientID: "client_commit"}
	epoch := "lease-commit"
	operation := store.ReviewRevisionOperationReviewChapter
	frozen := "  灯影摇晃  "
	key := make([]byte, productionReviewRevisionServerKeyBytes)
	for index := range key {
		key[index] = byte(index)
	}
	registry, err := reviewrevprep.NewStore(key, reviewrevprep.StoreOptions{
		ID: func() (string, error) { return handle, nil },
	})
	if err != nil {
		t.Fatalf("a fresh registry: %v", err)
	}
	minted, err := registry.Prepare(productionReviewRevisionBinding(caller, epoch, operation),
		reviewrevprep.PreparedContext{ProjectID: "proj-commit", Operation: operation, Status: "revised",
			DraftID: 1, ChapterNumber: 1, Version: 1, SourceHash: productionReviewRevisionContentHash("灯影摇晃"),
			Text: frozen, ContextHash: productionReviewRevisionContentHash(frozen)})
	if err != nil {
		t.Fatalf("minting a handle: %v", err)
	}
	if minted != handle {
		t.Fatalf("the registry minted a handle this test did not name")
	}
	assert := productionReviewRevisionCommitPrepared(registry, caller, epoch, "proj-commit", handle)
	if err := assert(operation, frozen); err != nil {
		t.Fatalf("a minted handle refused its own operation and bytes: %v", err)
	}
	for _, arm := range []struct {
		label  string
		assert func(string, string) error
		code   string
	}{
		{"another operation", assert, "GENERATION_REVIEW_CONTEXT_INVALID"},
		{"other bytes", assert, "GENERATION_REVIEW_CONTEXT_REQUIRED"},
		{"another caller", productionReviewRevisionCommitPrepared(registry,
			transport.Caller{Principal: "production-author", SessionID: "session-1",
				ClientID: productionReviewRevisionOtherTab}, epoch, "proj-commit", handle),
			"GENERATION_REVIEW_CONTEXT_INVALID"},
		{"another project", productionReviewRevisionCommitPrepared(registry, caller, epoch,
			"proj-other", handle), "GENERATION_REVIEW_CONTEXT_REQUIRED"},
		{"an unminted handle", productionReviewRevisionCommitPrepared(registry, caller, epoch,
			"proj-commit", productionReviewRevisionAbsentHandle), "GENERATION_REVIEW_CONTEXT_REQUIRED"},
	} {
		// The two arms that share the minted closure differ only in what they present.
		presented, text := operation, frozen
		switch arm.label {
		case "another operation":
			presented = store.ReviewRevisionOperationRefineDraft
		case "other bytes":
			text = frozen + " "
		}
		err := arm.assert(presented, text)
		if got := productionReviewRevisionFailureCode(err); got != arm.code {
			t.Fatalf("%s answered %v instead of %v", arm.label, got, arm.code)
		}
	}
}
