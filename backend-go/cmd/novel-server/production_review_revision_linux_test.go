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

// TestProductionReviewRevisionCommitReview is the family's write half, and the
// only test in this repo that calls `review-revision:commit-review` at all. It
// drives the whole workflow the renderer drives - prepare, begin, execute, commit -
// over the real HTTP handler and reads every row back through a connection of the
// test's own (mode=ro), never through the lane's admitted handle:
//
//   - the review row, its content row, its cycle row and every finding land in one
//     transaction, and the receipt the browser receives is a projection of those
//     rows (the store re-reads them before answering, review_cycles.go:290-298);
//   - the attempt that produced the artifact carries the stamped review effect, so
//     the run can never commit a second review for another artifact;
//   - every refusal happens before the first write: the whole database file stays
//     byte for byte what it was and no row is added (review_cycles.go:186-241, and
//     the first INSERT is at :259).
//
// The replay arm is not a refusal and this test says so: the reference answers the
// saved receipt for an identical request (review-revision-generation.ts:192-201 -
// `saved()` at :195 runs before `assertPrepared` at :201), so the run that has
// already committed refuses only a DIFFERENT artifact.
func TestProductionReviewRevisionCommitReview(t *testing.T) {
	t.Run("review-revision:commit-review", testProductionReviewRevisionCommitReview)
}

const (
	// productionReviewRevisionCommitReport is the settled artifact this test
	// dispatches: a version 2 review report in the model's own output shape, whose
	// one finding quotes the fixture's chapter body so the finding can be anchored.
	productionReviewRevisionCommitReport = `{"summary":"审稿通过","items":[{"category":"风格","severity":"warning","description":"开篇推门一句可以更紧凑","quote":"他推开门"}]}`
	// productionReviewRevisionCommitNonce names the one dispatch this test makes.
	productionReviewRevisionCommitNonce = "review-revision-commit-1"
	// productionReviewRevisionCommitForeignProject is the project id a body that
	// names another database presents. The borrow is the lease's, so this can never
	// reach a second project's rows.
	productionReviewRevisionCommitForeignProject = "proj-0000-review-commit-foreign"
)

// productionReviewRevisionCommitArtifact reads one run's settled artifact back over
// a connection of the test's own. It returns the identity the browser presents and
// the text, and it proves the stored textHash is the sha256 of the stored text, so
// the commit below is asserted against what the ledger really wrote.
func productionReviewRevisionCommitArtifact(t *testing.T, projectPath, runID string) (string, int64, string) {
	t.Helper()
	readback := productionStoryStateWriteReadback(t, projectPath)
	var artifactID string
	productionStoryStateWriteScan(t, readback,
		"SELECT artifact_id FROM generation_artifacts WHERE run_id=? ORDER BY rowid LIMIT 1",
		[]any{runID}, &artifactID)
	if artifactID == "" {
		t.Fatalf("the executed run wrote no artifact row")
	}
	var stored string
	var revision int64
	productionStoryStateWriteScan(t, readback,
		"SELECT artifact_json,revision FROM generation_artifacts WHERE artifact_id=?",
		[]any{artifactID}, &stored, &revision)
	var artifact struct {
		Text     string `json:"text"`
		TextHash string `json:"textHash"`
	}
	if err := json.Unmarshal([]byte(stored), &artifact); err != nil {
		t.Fatalf("the settled artifact is not JSON: %v", err)
	}
	if got := productionReviewRevisionContentHash(artifact.Text); artifact.TextHash != got {
		t.Fatalf("the settled artifact claims hash %v; its own text hashes %v", artifact.TextHash, got)
	}
	if artifact.Text != productionReviewRevisionCommitReport {
		t.Fatalf("the settled artifact is not the report the seam dispatched: %v", artifact.Text)
	}
	return artifactID, revision, artifact.TextHash
}

// productionReviewRevisionCommitCounts is a read-only count of the three tables a
// commit writes, so a refusal can be proven to have written nothing even when the
// transaction it never reached would have rolled back.
func productionReviewRevisionCommitCounts(t *testing.T, projectPath string) [3]int64 {
	t.Helper()
	readback := productionStoryStateWriteReadback(t, projectPath)
	var counts [3]int64
	for index, query := range []string{
		"SELECT COUNT(*) FROM reviews",
		"SELECT COUNT(*) FROM review_cycles",
		"SELECT COUNT(*) FROM review_findings",
	} {
		productionStoryStateWriteScan(t, readback, query, nil, &counts[index])
	}
	return counts
}

// productionReviewRevisionCommitRefused asserts one commit refusal: a symbolic code
// and neither a 200 nor the lane's fail-closed 501, with the whole database file
// unchanged and the three committed tables exactly as they were.
func productionReviewRevisionCommitRefused(t *testing.T, client *productionDurableClient, label string,
	args []any, lane *productionGenerationWriteLane, counts [3]int64, want ...string) {
	t.Helper()
	before := lane.bytes(t)
	response := client.call(t, productionReviewRevisionCommitReview, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s: %s is still the lane's fail-closed 501: %s", label,
			productionReviewRevisionCommitReview, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%s: a refused commit answered 200: %s", label, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	matched := false
	for _, code := range want {
		if body.Error.Code == code {
			matched = true
		}
	}
	if !matched {
		t.Fatalf("%s: refused with %v; want one of %v: %s", label, body.Error.Code, want, response.Body.String())
	}
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatalf("%s: a refused commit changed the committed database file", label)
	}
	if after := productionReviewRevisionCommitCounts(t, lane.projectPath); after != counts {
		t.Fatalf("%s: a refused commit wrote rows: %v -> %v", label, counts, after)
	}
}

// testProductionReviewRevisionCommitReview is the served path and the refusals that
// fence it, over one lane and one settled run.
func testProductionReviewRevisionCommitReview(t *testing.T) {
	seam := &productionGenerationSeam{text: productionReviewRevisionCommitReport}
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)
	prepare := productionReviewRevisionPrepareArgs(store.ReviewRevisionOperationReviewChapter,
		productionReviewRevisionContentHash(productionEditorSeed))
	prepared := productionReviewRevisionPrepareOnce(t, lane, prepare)
	handle := lane.begin(t, productionReviewRevisionSelection(t, prepared.ContextID, prepared))
	runID := syntheticString(handle["runId"])
	projectID := syntheticString(handle["projectId"])
	if runID == "" || projectID == "" {
		t.Fatalf("the admitted review run carries no identity: %v", handle)
	}
	// One dispatch settles the report above as the run's own artifact. Nothing here
	// dials: the seam stands in for the model, exactly as the execute tests do.
	executed := productionExecuteAnswer(t, lane.author,
		productionExecuteRequest(handle, productionReviewRevisionCommitNonce), lane.frozen)
	if calls := seam.dispatched(); calls != 1 {
		t.Fatalf("generation:execute dispatched %v times; want one", calls)
	}
	outcome := syntheticMap(executed["outcome"])
	if syntheticString(outcome["finishReason"]) != "stop" ||
		syntheticString(outcome["content"]) != productionReviewRevisionCommitReport {
		t.Fatalf("the run did not settle the report it dispatched: %v", outcome)
	}
	artifactID, revision, textHash := productionReviewRevisionCommitArtifact(t, lane.projectPath, runID)

	// The wire request the renderer builds (review-chapter.command.ts:173-176): the
	// prepared context id, the main run handle, and the artifact the recovery read
	// handed back - three keys, nothing else.
	wire := map[string]any{
		"contextId": prepared.ContextID,
		"handle": map[string]any{
			"projectId":    projectID,
			"epoch":        handle["epoch"],
			"rootActionId": handle["rootActionId"],
			"runId":        runID,
		},
		"artifact": map[string]any{"artifactId": artifactID, "revision": revision, "textHash": textHash},
	}

	counts := productionReviewRevisionCommitCounts(t, lane.projectPath)
	// A context id this process never minted: the handle proof runs before the first
	// write, and the reference's own answer is "prepare again" (:120).
	unminted := productionReviewRevisionCommitCopy(wire)
	unminted["contextId"] = productionReviewRevisionAbsentHandle
	productionReviewRevisionCommitRefused(t, lane.author, "an unminted handle",
		[]any{unminted, lane.frozen}, lane, counts, "GENERATION_REVIEW_CONTEXT_REQUIRED")
	// A handle and artifact that belong to another project: the body's project id is
	// the lease's own, and the borrowed database is that project's, so a body naming
	// another project is a request about a database this handler never opened
	// (production_review_revision_linux.go:556-562, the family's own invalid code).
	foreign := productionReviewRevisionCommitCopy(wire)
	foreign["handle"].(map[string]any)["projectId"] = productionReviewRevisionCommitForeignProject
	productionReviewRevisionCommitRefused(t, lane.author, "a foreign project",
		[]any{foreign, lane.frozen}, lane, counts, "GENERATION_REVIEW_CONTEXT_INVALID")
	// A lease this server never issued: the borrow refuses before the request is read.
	productionReviewRevisionCommitRefused(t, lane.author, "a stale lease",
		[]any{productionReviewRevisionCommitCopy(wire), lane.stale()}, lane, counts,
		"PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_ROOT_MISMATCH",
		"PROJECT_CONTEXT_CHANGED", "PROJECT_NOT_OPEN")
	// The binder table's arity: one request plus the frozen context, and the renderer
	// sends exactly that (src/shared/review-revision-generation.ts:175 is arity 1,
	// the project session is the trailing wire argument).
	productionReviewRevisionCommitRefused(t, lane.author, "a short wire call",
		[]any{productionReviewRevisionCommitCopy(wire)}, lane, counts, "INVALID_ARGUMENTS")

	// The commit itself, over the same handler the renderer posts to.
	raw := productionEditorCall(t, lane.author, productionReviewRevisionCommitReview,
		[]any{productionReviewRevisionCommitCopy(wire), lane.frozen})
	var receipt struct {
		Success     bool   `json:"success"`
		Kind        string `json:"kind"`
		ID          int64  `json:"id"`
		Index       int64  `json:"index"`
		Content     string `json:"content"`
		ContentHash string `json:"contentHash"`
		Source      struct {
			ID            int64  `json:"id"`
			ChapterNumber int64  `json:"chapterNumber"`
			Version       int64  `json:"version"`
			Status        string `json:"status"`
			Content       string `json:"content"`
		} `json:"source"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatalf("%s: %v %s", productionReviewRevisionCommitReview, err, raw)
	}
	if !receipt.Success || receipt.Kind != "review" || receipt.ID < 1 || receipt.Index < 1 {
		t.Fatalf("the commit answered %v %v %v %v; want a saved review receipt", receipt.Success,
			receipt.Kind, receipt.ID, receipt.Index)
	}
	if receipt.ContentHash != productionReviewRevisionContentHash(receipt.Content) {
		t.Fatalf("the receipt claims hash %v; its own content hashes %v", receipt.ContentHash,
			productionReviewRevisionContentHash(receipt.Content))
	}
	if receipt.Source.ID != 1 || receipt.Source.ChapterNumber != 1 || receipt.Source.Version != 1 ||
		receipt.Source.Status != "draft" || receipt.Source.Content != productionEditorSeed {
		t.Fatalf("the receipt names another source draft: %+v", receipt.Source)
	}
	if !strings.Contains(receipt.Content, "审稿") {
		t.Fatalf("the review body is not the built report: %v", receipt.Content)
	}

	// The rows, read back over a connection of the test's own.
	after := productionReviewRevisionCommitCounts(t, lane.projectPath)
	if after[0] != counts[0]+1 || after[1] != counts[1]+1 || after[2] <= counts[2] {
		t.Fatalf("the commit wrote %v rows (reviews, cycles, findings); it started from %v", after, counts)
	}
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var storedIndex, storedContentID, storedBaseDraft, storedChapter, storedVersion int64
	var storedStatus, storedSource, storedBody string
	productionStoryStateWriteScan(t, readback,
		`SELECT r.review_index,r.base_draft_id,r.source_draft_chapter_number,r.source_draft_version,
 r.source_draft_status,r.source_content,c.body,r.content_id
 FROM reviews r JOIN contents c ON c.id=r.content_id WHERE r.id=?`, []any{receipt.ID},
		&storedIndex, &storedBaseDraft, &storedChapter, &storedVersion, &storedStatus,
		&storedSource, &storedBody, &storedContentID)
	if storedIndex != receipt.Index || storedBaseDraft != 1 || storedChapter != 1 ||
		storedVersion != 1 || storedStatus != "draft" || storedSource != productionEditorSeed {
		t.Fatalf("the committed review row reads back as index %v draft %v chapter %v version %v status %v",
			storedIndex, storedBaseDraft, storedChapter, storedVersion, storedStatus)
	}
	// The row the browser was handed is the row that was written: the receipt's
	// content is the stored body, byte for byte.
	if storedBody != receipt.Content || productionReviewRevisionContentHash(storedBody) != receipt.ContentHash {
		t.Fatalf("the committed review body is not the receipt's content")
	}
	var cycleReview, cycleVersion, cycleRecheck int64
	var cycleHash, cycleSourceHash, cycleRevisionStatus string
	productionStoryStateWriteScan(t, readback,
		`SELECT review_id,review_content_hash,source_hash,comparison_version,revision_status,recheck_count
 FROM review_cycles WHERE review_id=?`, []any{receipt.ID},
		&cycleReview, &cycleHash, &cycleSourceHash, &cycleVersion, &cycleRevisionStatus, &cycleRecheck)
	if cycleReview != receipt.ID || cycleHash != receipt.ContentHash ||
		cycleSourceHash != productionReviewRevisionContentHash(productionEditorSeed) ||
		cycleVersion != 1 || cycleRevisionStatus != "not-generated" || cycleRecheck != 0 {
		t.Fatalf("the committed cycle row reads back as review %v hash %v source %v version %v status %v recheck %v",
			cycleReview, cycleHash, cycleSourceHash, cycleVersion, cycleRevisionStatus, cycleRecheck)
	}
	// Every finding belongs to that cycle, and the count is the report's own items
	// plus the goal projections the builder adds for the frozen blueprint goals.
	var cycleID string
	productionStoryStateWriteScan(t, readback, "SELECT cycle_id FROM review_cycles WHERE review_id=?",
		[]any{receipt.ID}, &cycleID)
	var findings, misfiled int64
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM review_findings WHERE cycle_id=?",
		[]any{cycleID}, &findings)
	productionStoryStateWriteScan(t, readback,
		"SELECT COUNT(*) FROM review_findings WHERE cycle_id<>? OR cycle_id IS NULL", []any{cycleID}, &misfiled)
	if findings < 1 || misfiled != 0 {
		t.Fatalf("the cycle holds %v findings and %v belong to another cycle", findings, misfiled)
	}
	// The effect that makes this run's review spent: the attempt carries it, so no
	// second artifact can be committed for the same run.
	var effectKind string
	productionStoryStateWriteScan(t, readback,
		"SELECT json_extract(usage_receipt_json,'$.reviewRevisionEffect.kind') FROM generation_attempts WHERE run_id=?",
		[]any{runID}, &effectKind)
	if effectKind != "review" {
		t.Fatalf("the settled attempt carries effect %v; want review", effectKind)
	}

	// The replay: the reference answers the saved receipt instead of writing twice
	// (review_cycles.go:206-217), so the file and every count stay as they are.
	committed := lane.bytes(t)
	replayed := productionEditorCall(t, lane.author, productionReviewRevisionCommitReview,
		[]any{productionReviewRevisionCommitCopy(wire), lane.frozen})
	var again struct {
		Success     bool   `json:"success"`
		ID          int64  `json:"id"`
		Index       int64  `json:"index"`
		ContentHash string `json:"contentHash"`
	}
	if err := json.Unmarshal(replayed, &again); err != nil {
		t.Fatalf("the replayed commit is not a receipt: %v", err)
	}
	if !again.Success || again.ID != receipt.ID || again.Index != receipt.Index ||
		again.ContentHash != receipt.ContentHash {
		t.Fatalf("the replay answered another receipt: %v against %v", again, receipt.ID)
	}
	if now := productionReviewRevisionCommitCounts(t, lane.projectPath); now != after {
		t.Fatalf("the replay wrote rows: %v -> %v", after, now)
	}
	if now := lane.bytes(t); !bytes.Equal(committed, now) {
		t.Fatalf("the replay changed the committed database file")
	}

	// The spent arm: the same run, already spent on one artifact, presented for
	// another one. This is the conflict the reference raises, and it is raised
	// before the handle is even consulted.
	spent := productionReviewRevisionCommitCopy(wire)
	spent["artifact"].(map[string]any)["revision"] = revision + 1
	productionReviewRevisionCommitRefused(t, lane.author, "a spent artifact",
		[]any{spent, lane.frozen}, lane, after, "GENERATION_REVIEW_COMMIT_CONFLICT")
}
