//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's chapter-draft CONTEXT surface: the two
// operations a browser needs around one chapter draft, and only those two.
//
//	generation:prepare-draft-context   bound, READ,  wire arity 2
//	generation:read-context            bound, READ,  wire arity 2
//	generation:commit-draft            NOT BOUND (see "What is missing")
//
// The reference handlers are
// electron/controllers/generation-controller.ts:193 (prepare) and :275
// (read-context); the owner bodies they call are main-generation-owner.ts:396-408
// (prepareDraftContext) and :912-960 (readContext); the wire types are
// src/shared/generation-owner-contract.ts:216-231 (PreparedDraftContext) and
// :139-169 (GenerationRecoveryContext), with the channel table at :307-309.
//
// # Why these are reads, and why prepare writes nothing
//
// prepare-draft-context assembles and freezes the context of one chapter: the
// validated request, the selected drafts as they are on disk right now, and a
// knowledge snapshot. In the reference that whole body runs inside
// `deps.database.transaction`, and the transaction contains exactly one SELECT
// (main-generation-owner.ts:401-405): the drafts JOIN contents read. There is no
// INSERT anywhere in it, and no `preparations` table exists in the schema - the
// receipt is kept in a process-memory `Map` (:406-408) with a 64 entry bound, so
// it dies with the process exactly as it does in the desktop app. This lane is
// therefore not allowed to improve on it: a preparation row would be a write the
// reference never performs, and admission carries no such object
// (`grep -rn preparation internal/admission` is empty). The registry below is the
// same in-memory map, kept for the same reason and with the same bound; its only
// observable product is the preparationId the browser hands back to
// generation:begin.
//
// read-context is a pure projection: `requireRun` (main-generation-owner.ts:
// 156-164) reads the run binding, `draftEffects.readRecovery` (:69-73) reads the
// attempt rows, and the returned object is assembled from receipts the shipped
// ledger already wrote. Nothing is created, updated or deleted, which is why the
// tests assert the author database file is byte-identical afterwards.
//
// commit-draft is the write of this family and is NOT bound here, because this
// lane cannot produce the text it has to persist (see "What is missing").
//
// # The admission, and the one place it differs from the mutation gate
//
// Both operations admit through the same owner entry generation:begin and
// generation:read already use (synthetic_generation_linux.go:318-336): the frozen
// three-key session, then the live writer lease, then the ledger. A stale, foreign
// or closed lease is refused there, before a single row is read, which is what the
// tests assert per operation.
//
// They deliberately do NOT take production_generation_write_linux.go:243's
// `mutation()` gate, which additionally requires the committed model-egress policy
// and the executor built from it. Neither operation dials - prepare only looks the
// model up in the committed model list, exactly like the reference's
// `deps.loadModel` (main-generation-owner.ts:387), and read-context does not touch
// a model at all - so this file joins the read slice
// (production_generation_reads_linux.go), which binds ledger reads over the durable
// admission alone.
//
// The run lookup in read-context is the epoch-independent one
// (synthetic_generation_linux.go:1292-1326, ReadCommitted): a committed run
// outlives the lease it was frozen under, so the request's handle.epoch must equal
// the binding's epoch and the run must belong to the frozen project, but the
// calling lease is not required to still be that epoch. Refusing there would make
// every saved chapter unreadable after the next save.
//
// # Every refusal is checked before the frozen context is read
//
// The family adapter below validates `len(args) == arity+1` and the three-key
// session context at `args[arity]` before either body runs, so an arity or session
// error can never depend on, or leak through, what the frozen context names.
//
// # What is missing, and why generation:commit-draft stays unbound
//
// commit-draft must persist the COMPOSED text of a run
// (generation-draft-effects.ts:146-201): it recomposes the visible continuation
// from the run's artifacts, refuses `GENERATION_DRAFT_INCOMPLETE` when the
// recomposed text does not cover the target unit range (:169-173), writes one
// drafts row, and then stamps a `draftCommit` receipt into the attempt's
// usage_receipt_json. This lane cannot do any of that honestly:
//
//   - the composed text. The reference never stores it: composeVisible persists
//     `{algorithm, artifactIds, sources, chapters}` WITHOUT `text`
//     (generation-run-repository.ts:693) and recomposes on every read via
//     `visibleComposition` (:460-505), which needs `sanitizeDraftText` /
//     `composeDraftVisibleContinuation` (src/shared/draft-visible-text.ts:33,53)
//     and `composeVisibleContinuation` (src/shared/visible-continuation.ts).
//     internal/generation/service.go:4 states the same gap on the Go side:
//     "composition ... unsupported". There is no Go producer of that text, so a
//     commit here could only persist text the browser sent, which the reference's
//     own request type does not even carry (GenerationDraftCommitRequest, :131-137,
//     has no content field).
//   - the incomplete check. `GENERATION_DRAFT_INCOMPLETE` (generation-draft-effects.
//     ts:169-173) is `countDraftUnits(composition.text)` against
//     `draftTargetUnitRange(manifest.chapterNumber)`; both live in
//     src/shared/draft-units.ts and have no Go port.
//   - the transaction. The write is one IMMEDIATE transaction over `contents`,
//     `drafts` and `generation_attempts` with an in-transaction next-chapter check
//     (:146-201). store.Drafts.Create (internal/store/draft_author.go:59,96) opens
//     its own transaction, so it cannot be joined to the receipt stamp without an
//     in-transaction variant.
//
// The names stay unbound rather than being installed as handlers that could only
// refuse, so a browser that reaches them still fails closed at the registry's
// GO_OPERATION_NOT_MIGRATED instead of at a fabricated success.
//
// # What read-context can and cannot project
//
// The composition is read from the reference's own key, newest attempt row first
// (`json_extract(usage_receipt_json,'$.visibleComposition')`, ... ORDER BY rowid
// DESC, generation-run-repository.ts:664-673). The reference recomposes and then
// compares algorithm/textHash/sources/chapters against what was stored, refusing
// GENERATION_COMPOSITION_INTEGRITY_FAILED on a mismatch. This lane has no
// recomposer, so it performs the checks it can perform - the stored text hashes to
// the stored textHash, the artifact list is non-empty and unique, an algorithm is
// named, sources are present - and refuses with the lane-local
// GENERATION_CONTEXT_UNSUPPORTED when the stored row cannot be verified that way
// (that is the shape composeVisible writes today, which stores no text at all).
// The same code refuses a run whose context the reference would build from a
// receipt this lane cannot produce: a non chapter-draft manifest operation, a
// `materialDecision.shortOutlinePromptHash` (draftShortOutline,
// main-generation-owner.ts:230-240) or an attempt purpose of
// `chapter-draft-reconcile` (draftReconciliation, :948-960). Omitting those fields
// silently would hand the browser a context that looks complete and is not.
//
// If a sibling slice lands `generation:compose-visible` and persists
// `visibleComposition` with a `text`/`textHash` pair in the reference's own key,
// this projection starts returning it with no change here: the checks above are
// the only gate, and the newest-row-first lookup is already the reference's.

const (
	// productionDraftContextPreparations is the reference's registry bound
	// (main-generation-owner.ts:406-408): at 64 live preparations the oldest is
	// dropped before the new one is stored.
	productionDraftContextPreparations = 64
	// productionDraftContextTopK is the retrieval budget frozen into an empty
	// knowledge snapshot: the reference captures with the configured topK, whose
	// committed default is 5 (generation-knowledge-source.ts:104-154).
	productionDraftContextTopK = 5
	// productionDraftContextOperation is the only manifest operation whose context
	// this lane serves; draftPreparationBinding hardcodes it
	// (main-generation-owner.ts:390).
	productionDraftContextOperation = "chapter-draft"
	// productionDraftContextReconcilePurpose is DRAFT_RECONCILE_PURPOSE
	// (src/shared/draft-reconciliation.ts:12).
	productionDraftContextReconcilePurpose = "chapter-draft-reconcile"
	// productionDraftContextUnknownPurpose is the reference's placeholder for an
	// attempt row without a recorded purpose (main-generation-owner.ts:956).
	productionDraftContextUnknownPurpose = "unknown"
)

// productionDraftContextDraftAlgorithm is the closed set of the two draft
// composition versions (src/shared/draft-visible-text.ts:3-7,
// isDraftVisibleTextVersion). It is a data constant, not a rule: the versions are
// the only two strings that ever name a draft composition.
func productionDraftContextDraftAlgorithm(value string) bool {
	return value == "draft-visible-v1" || value == "draft-visible-v2"
}

// productionDraftContextRequest is one validated prepare-draft-context request.
// The key set is the reference's allowlist (main-generation-owner.ts:385):
// chapterNumber, modelId, promptKeys, skillStages, authorInputs, query,
// selectedDraftIds, batchId. A key outside it is
// GENERATION_DRAFT_PREPARATION_INVALID, exactly as the reference's
// `Object.keys(request).some(...)` refuses it.
//
// Three fields are validated more strictly here than the reference validates them
// at this layer, because the reference hands them to buildGenerationSourceBinding,
// which this lane has no port of (`grep -rn buildGenerationSourceBinding
// internal/` is empty; internal/generationowner/binding.go:44-48 states BindSources
// is not the same function). Without that builder the only honest options are to
// validate the shapes that builder would consume or to accept anything; so the
// chapter number must be a positive integer (the binding is a chapter binding),
// promptKeys must be strings and authorInputs must be `{id, text}` pairs. A request
// that cannot name a chapter cannot be frozen as one.
type productionDraftContextRequest struct {
	ChapterNumber    int64
	ModelID          string
	PromptKeys       []string
	SkillStages      []any
	AuthorInputs     []any
	Query            string
	SelectedDraftIDs []int64
	BatchID          string
}

// productionDraftContextPreparation is one frozen context held in memory, the
// counterpart of the reference's `preparations` map value
// (main-generation-owner.ts:406-408). It carries the validated request and the
// knowledge snapshot that was frozen with it, so a later reader can prove the two
// halves belong together even though nothing consumes it yet: generation:begin
// accepts `preparationId` and ignores it
// (synthetic_generation_linux.go:400), which is why
// GENERATION_DRAFT_PREPARATION_REQUIRED and _CHANGED have no consumer in this lane.
type productionDraftContextPreparation struct {
	request       productionDraftContextRequest
	knowledge     map[string]any
	selectedCount int
	projectID     string
	leaseID       string
}

// productionDraftContext is the file's one service value: the owner it projects
// through plus the in-memory preparation registry.
type productionDraftContext struct {
	owner *productionGeneration
	mu    sync.Mutex
	order []string
	held  map[string]productionDraftContextPreparation
}

// newProductionDraftContext builds the service over the same owner generation:begin
// already uses. A nil owner is not an error here: Bind then installs nothing, and
// the two names keep failing closed at GO_OPERATION_NOT_MIGRATED.
func newProductionDraftContext(owner *productionGeneration) *productionDraftContext {
	return &productionDraftContext{owner: owner, held: make(map[string]productionDraftContextPreparation, productionDraftContextPreparations)}
}

// Bind installs exactly the two names this file owns, one server.Bind each
// (internal/transport/server.go:73-77), the same way production_review_write_linux.
// go:51-56 installs its slice. BindGeneration cannot be used: it installs thirteen
// names atomically (internal/transport/generation.go:37), including the five this
// lane already binds through BindGenerationBegin/Execute/Lifecycle and
// generation:commit-draft, which this file must not publish. Bind refuses an
// uninventoried name (OPERATION_NOT_EXPOSED) and a double bind
// (OPERATION_ALREADY_BOUND), so a collision is a startup error rather than silent
// dead code.
func (d *productionDraftContext) Bind(server *transport.Server) error {
	if d == nil || d.owner == nil || server == nil {
		return nil
	}
	if err := server.Bind("generation:prepare-draft-context", productionDraftContextRead(1, d.Prepare)); err != nil {
		return err
	}
	return server.Bind("generation:read-context", productionDraftContextRead(1, d.Read))
}

// productionDraftContextRead is the family adapter of internal/transport/
// generation.go:260-302 with `mutating` false: the wire arity is the request plus
// the frozen session context at args[arity], that context must hold exactly the
// three frozen keys, and an error surfaces as a security.HTTPError carrying the
// symbolic code (generationFailure, :304-315) instead of the mutating DTO. The
// arity and session checks run first, so a malformed envelope is refused before
// the frozen context is read.
func productionDraftContextRead(arity int, call func(context.Context, transport.Caller, map[string]any, any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		fail := func(err error) (any, error) {
			var public *security.HTTPError
			if errors.As(err, &public) && public != nil {
				return nil, security.NewHTTPError(public.Status, public.Code)
			}
			return nil, security.NewHTTPError(http.StatusInternalServerError, productionDraftContextCode(err))
		}
		if len(args) != arity+1 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		raw, ok := args[arity].(map[string]any)
		if !ok || len(raw) != 3 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
		}
		expected := make(map[string]any, 3)
		for _, key := range []string{"projectId", "leaseId", "projectPath"} {
			text, ok := raw[key].(string)
			if !ok || text == "" || !utf8.ValidString(text) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
			}
			expected[key] = text
		}
		if ctx == nil {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		value, err := call(ctx, caller, expected, args[0])
		if err != nil {
			return fail(err)
		}
		return value, nil
	}
}

// productionDraftContextCode is internal/transport/generation.go:317's
// generationSymbolicCode: an all-caps symbolic code, or REQUEST_FAILED for anything
// else, so no SQLite diagnostic, path or provider message can reach the browser.
func productionDraftContextCode(err error) string {
	if err == nil {
		return "REQUEST_FAILED"
	}
	code := err.Error()
	var public *security.HTTPError
	if errors.As(err, &public) {
		code = public.Code
	}
	if len(code) < 2 {
		return "REQUEST_FAILED"
	}
	for index, c := range code {
		if !(c >= 'A' && c <= 'Z') && !(index > 0 && (c >= '0' && c <= '9' || c == '_')) {
			return "REQUEST_FAILED"
		}
	}
	return code
}

// Prepare is generation:prepare-draft-context. It validates the request against
// the reference's allowlist, resolves the model in the committed model list,
// reads the selected drafts on the author's own database as they are now, freezes
// an empty knowledge snapshot, and registers the result in memory. It writes
// nothing (see the file header).
func (d *productionDraftContext) Prepare(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if d == nil || d.owner == nil || d.owner.inner == nil || d.owner.inner.loadModel == nil {
		return nil, errProductionGenerationUnavailable
	}
	request, err := productionDraftContextParsePrepare(arg)
	if err != nil {
		return nil, err
	}
	// The model must exist before anything is read: the reference's
	// `deps.loadModel(request.modelId)` (main-generation-owner.ts:387) mints the
	// read-only evidence receipt the frozen binding is built with, so a request for
	// a model this deployment does not have never reaches the selection.
	if _, found, err := d.owner.inner.loadModel(ctx, request.ModelID); err != nil || !found {
		return nil, errors.New("GENERATION_MODEL_MISSING")
	}
	_, scope, _, db, done, err := d.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	if db == nil {
		return nil, errProductionGenerationUnavailable
	}
	selected, err := productionDraftContextSelected(ctx, db, request.SelectedDraftIDs)
	if err != nil {
		return nil, err
	}
	entries := make([]any, 0, len(selected))
	for index, row := range selected {
		if row == nil {
			// The reference dereferences the missing row and the failure surfaces as
			// a plain TypeError, which the controller reports as the generic code.
			return nil, errors.New("GENERATION_REQUEST_FAILED")
		}
		entries = append(entries, map[string]any{
			"draftId":       request.SelectedDraftIDs[index],
			"chapterNumber": row.ChapterNumber,
			"version":       row.Version,
			"contentHash":   syntheticHash(row.Content),
			"content":       row.Content,
		})
	}
	knowledge := productionDraftContextKnowledge(request.Query)
	id, err := productionDraftContextID()
	if err != nil {
		return nil, err
	}
	d.remember(id, productionDraftContextPreparation{
		request:       request,
		knowledge:     knowledge,
		selectedCount: len(entries),
		projectID:     scope.Project,
		leaseID:       scope.Epoch,
	})
	return map[string]any{
		"preparationId":     id,
		"knowledgeSnapshot": knowledge,
		"selectedDrafts":    entries,
	}, nil
}

// Read is generation:read-context. It projects the run's frozen binding and the
// receipts the shipped ledger wrote into GenerationRecoveryContext
// (src/shared/generation-owner-contract.ts:139-169).
func (d *productionDraftContext) Read(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if d == nil || d.owner == nil || d.owner.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	inner, err := productionFamilyRequestHandle(arg, "GENERATION_REQUEST_FAILED")
	if err != nil {
		return nil, err
	}
	// syntheticParseHandle refuses a handle that is not four non-empty strings with
	// GENERATION_RUN_NOT_FOUND, the reference's requireRun code
	// (main-generation-owner.ts:156-164).
	handle, err := syntheticParseHandle(inner)
	if err != nil {
		return nil, err
	}
	_, scope, ledger, db, done, err := d.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	if ledger == nil || db == nil {
		return nil, errProductionGenerationUnavailable
	}
	binding, manifest, err := productionDraftContextRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	attempts, err := productionDraftContextAttempts(ctx, db, handle.RunID)
	if err != nil {
		return nil, err
	}
	if err := productionDraftContextSupported(manifest, attempts); err != nil {
		return nil, err
	}
	composition, err := productionDraftContextComposition(attempts)
	if err != nil {
		return nil, err
	}
	draftSave, err := productionDraftContextDraftSave(ctx, db, handle, manifest, attempts, composition)
	if err != nil {
		return nil, err
	}
	inFlight, err := syntheticCall(ctx, ledger, "hasInFlight", handle.RunID)
	if err != nil {
		return nil, err
	}
	finishReason, err := productionDraftContextFinishReason(ctx, db, attempts, composition)
	if err != nil {
		return nil, err
	}
	result := map[string]any{
		"modelId":                         syntheticString(manifest["modelId"]),
		"handle":                          productionDraftContextHandleOf(binding, handle),
		"inFlight":                        syntheticBool(inFlight),
		"operation":                       syntheticString(manifest["operation"]),
		"authorInputs":                    productionDraftContextList(manifest["authorInputs"]),
		"selectedDraftIds":                productionDraftContextNumbers(manifest["selectedDraftIds"]),
		"selectedFinalizedDraftIds":       productionDraftContextNumbers(manifest["selectedFinalizedDraftIds"]),
		"selectedBlueprintChapterNumbers": productionDraftContextNumbers(manifest["selectedBlueprintChapterNumbers"]),
		"composition":                     composition,
		"lastCompositionFinishReason":     finishReason,
		"attemptedPurposes":               productionDraftContextPurposes(attempts),
		"draftSave":                       draftSave,
	}
	if chapter, ok := productionDraftContextInt(manifest["chapterNumber"]); ok {
		result["chapterNumber"] = chapter
	}
	if snapshot := syntheticMap(manifest["knowledgeSnapshot"]); snapshot != nil {
		result["knowledgeSnapshot"] = snapshot
	}
	if batch := syntheticString(manifest["batchId"]); batch != "" {
		result["batchId"] = batch
	}
	selected, complete, err := productionDraftContextSelectedMatches(ctx, db, manifest, binding)
	if err != nil {
		return nil, err
	}
	if complete {
		result["selectedDrafts"] = selected
	}
	return result, nil
}

// remember stores one preparation under the reference's bound, dropping the oldest
// first (main-generation-owner.ts:406-408).
func (d *productionDraftContext) remember(id string, preparation productionDraftContextPreparation) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.held == nil {
		d.held = make(map[string]productionDraftContextPreparation, productionDraftContextPreparations)
	}
	if len(d.order) >= productionDraftContextPreparations {
		oldest := d.order[0]
		d.order = d.order[1:]
		delete(d.held, oldest)
	}
	d.order = append(d.order, id)
	d.held[id] = preparation
}

// productionDraftContextID mints the preparationId. The reference uses
// randomUUID(); the shape is the same v4 UUID so the browser's opaque id needs no
// special case.
func productionDraftContextID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("GENERATION_DRAFT_PREPARATION_INVALID")
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// productionDraftContextKnowledge freezes the empty knowledge snapshot. The
// reference captures the author's knowledge base through an embedding dispatch
// (generation-knowledge-source.ts:104-154); this lane binds no embedding path
// (internal/transport/knowledge_search.go:30-31 installs only the bounded FTS
// fallback and no binder installs it in cmd/novel-server), and the FTS result
// type carries no chunk identity (internal/knowledgequery/search.go:38-42), so no
// honest `items` entry can be minted. The snapshot says so instead of pretending
// retrieval happened: state 'empty', storageState 'absent', no revision, no items.
func productionDraftContextKnowledge(query string) map[string]any {
	return map[string]any{
		"version":           float64(1),
		"state":             "empty",
		"storageState":      "absent",
		"query":             query,
		"topK":              float64(productionDraftContextTopK),
		"canonicalRevision": nil,
		"documentsRevision": nil,
		"items":             []any{},
	}
}

// productionDraftContextParsePrepare validates one prepare request against
// main-generation-owner.ts:383-392 without the missing binding builder (see
// productionDraftContextRequest).
func productionDraftContextParsePrepare(value any) (productionDraftContextRequest, error) {
	invalid := errors.New("GENERATION_DRAFT_PREPARATION_INVALID")
	object := syntheticMap(value)
	if object == nil {
		return productionDraftContextRequest{}, invalid
	}
	for key := range object {
		switch key {
		case "chapterNumber", "modelId", "promptKeys", "skillStages", "authorInputs", "query", "selectedDraftIds", "batchId":
		default:
			return productionDraftContextRequest{}, invalid
		}
	}
	query, ok := object["query"].(string)
	if !ok || !utf8.ValidString(query) {
		return productionDraftContextRequest{}, invalid
	}
	request := productionDraftContextRequest{Query: query}
	if model, present := object["modelId"]; present && model != nil {
		text, ok := model.(string)
		if !ok || !utf8.ValidString(text) {
			return productionDraftContextRequest{}, invalid
		}
		request.ModelID = text
	}
	chapter, ok := productionDraftContextInt(object["chapterNumber"])
	if !ok || chapter < 1 {
		return productionDraftContextRequest{}, invalid
	}
	request.ChapterNumber = chapter
	if raw, present := object["promptKeys"]; present && raw != nil {
		values := syntheticSlice(raw)
		if values == nil {
			return productionDraftContextRequest{}, invalid
		}
		request.PromptKeys = make([]string, 0, len(values))
		for _, item := range values {
			text, ok := item.(string)
			if !ok || text == "" || !utf8.ValidString(text) {
				return productionDraftContextRequest{}, invalid
			}
			request.PromptKeys = append(request.PromptKeys, text)
		}
	}
	if raw, present := object["skillStages"]; present && raw != nil {
		if request.SkillStages = syntheticSlice(raw); request.SkillStages == nil {
			return productionDraftContextRequest{}, invalid
		}
	}
	if raw, present := object["authorInputs"]; present && raw != nil {
		values := syntheticSlice(raw)
		if values == nil {
			return productionDraftContextRequest{}, invalid
		}
		for _, item := range values {
			entry := syntheticMap(item)
			if entry == nil || !utf8.ValidString(syntheticString(entry["id"])) || !utf8.ValidString(syntheticString(entry["text"])) {
				return productionDraftContextRequest{}, invalid
			}
		}
		request.AuthorInputs = values
	}
	if raw, present := object["selectedDraftIds"]; present && raw != nil {
		values := syntheticSlice(raw)
		if values == nil {
			return productionDraftContextRequest{}, invalid
		}
		request.SelectedDraftIDs = make([]int64, 0, len(values))
		for _, item := range values {
			id, ok := productionDraftContextInt(item)
			if !ok || id < 1 {
				return productionDraftContextRequest{}, invalid
			}
			request.SelectedDraftIDs = append(request.SelectedDraftIDs, id)
		}
	}
	if raw, present := object["batchId"]; present && raw != nil {
		text, ok := raw.(string)
		if !ok || text == "" || !utf8.ValidString(text) {
			return productionDraftContextRequest{}, invalid
		}
		request.BatchID = text
	}
	return request, nil
}

// productionDraftContextSelected reads each requested draft through the drafts
// store on the admitted author database, exactly the row the reference's SELECT
// returns (main-generation-owner.ts:402): chapter number, version and body. A
// missing row keeps its position as nil so the caller decides whether that is
// fatal (prepare) or a mismatch (read-context's `selectedDrafts`).
func productionDraftContextSelected(ctx context.Context, db *sql.DB, ids []int64) ([]*store.DraftFull, error) {
	rows := make([]*store.DraftFull, 0, len(ids))
	if len(ids) == 0 {
		return rows, nil
	}
	drafts := store.NewDrafts(db)
	for _, id := range ids {
		full, err := drafts.GetFull(ctx, id)
		if err != nil {
			return nil, err
		}
		rows = append(rows, full)
	}
	return rows, nil
}

// productionDraftContextRun is synthetic_generation_linux.go:1292-1326 applied to
// this file's handle: the run must exist, its root and run id must be the ones the
// request names, its binding must belong to the frozen project, and the request's
// epoch must be the binding's epoch. Only the equality between the binding epoch
// and the calling lease is dropped, because a lease rotates on every save and a
// committed chapter must stay readable after it does.
func productionDraftContextRun(ctx context.Context, ledger *store.GenerationLedger, scope generationowner.Scope, handle syntheticHandle) (map[string]any, map[string]any, error) {
	args, err := syntheticArgs(handle.RunID)
	if err != nil {
		return nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	value, err := ledger.CallJSON(ctx, "get", args)
	if err != nil {
		return nil, nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	manifest := syntheticMap(binding["sourceManifest"])
	if run["rootActionId"] != handle.RootActionID || run["runId"] != handle.RunID ||
		binding["projectId"] != scope.Project || binding["epoch"] != handle.Epoch || manifest == nil {
		return nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	return binding, manifest, nil
}

// productionDraftContextAttempt is one attempt row of the run: its id and the
// usage receipt the shipped ledger wrote for it.
type productionDraftContextAttempt struct {
	ID    string
	Usage map[string]any
}

// productionDraftContextAttempts reads the run's attempt rows newest-last, the
// order the reference's `ORDER BY rowid` produces
// (main-generation-owner.ts:940-942). A row whose receipt is not JSON cannot be
// verified, so it is refused rather than skipped: every projection below is derived
// from a receipt, and a silently skipped row could hide a commit.
func productionDraftContextAttempts(ctx context.Context, db *sql.DB, runID string) ([]productionDraftContextAttempt, error) {
	rows, err := db.QueryContext(ctx, "SELECT attempt_id,usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid", runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []productionDraftContextAttempt
	for rows.Next() {
		var id string
		var raw sql.NullString
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		usage := map[string]any{}
		if raw.Valid && strings.TrimSpace(raw.String) != "" {
			if err := json.Unmarshal([]byte(raw.String), &usage); err != nil {
				return nil, errors.New("GENERATION_DRAFT_RECEIPT_INVALID")
			}
		}
		out = append(out, productionDraftContextAttempt{ID: id, Usage: usage})
	}
	return out, rows.Err()
}

// productionDraftContextSupported refuses a run whose context the reference builds
// from a receipt this lane cannot produce (see the file header). Each marker is a
// manifest field or an attempt purpose, so the check never guesses.
func productionDraftContextSupported(manifest map[string]any, attempts []productionDraftContextAttempt) error {
	if syntheticString(manifest["operation"]) != productionDraftContextOperation {
		return errors.New("GENERATION_CONTEXT_UNSUPPORTED")
	}
	if decision := syntheticMap(manifest["materialDecision"]); decision != nil && syntheticString(decision["shortOutlinePromptHash"]) != "" {
		return errors.New("GENERATION_CONTEXT_UNSUPPORTED")
	}
	for _, attempt := range attempts {
		if syntheticString(attempt.Usage["purpose"]) == productionDraftContextReconcilePurpose {
			return errors.New("GENERATION_CONTEXT_UNSUPPORTED")
		}
	}
	return nil
}

// productionDraftContextComposition finds the newest stored composition and
// validates it. The reference walks the same rows newest first
// (generation-run-repository.ts:664-673) and skips empty ones.
func productionDraftContextComposition(attempts []productionDraftContextAttempt) (map[string]any, error) {
	for index := len(attempts) - 1; index >= 0; index-- {
		stored := syntheticMap(attempts[index].Usage["visibleComposition"])
		if stored == nil {
			continue
		}
		return productionDraftContextCompose(stored)
	}
	return nil, nil
}

// productionDraftContextCompose is the verification this lane can perform without
// a recomposer: the text it returns must be the text that was hashed, the artifact
// list must be a real list of distinct artifacts, an algorithm must be named and
// the sources must be present. A row that only names artifacts and a hash - which
// is what the reference's own composeVisible persists - is refused instead of being
// expanded into text this lane would have to invent.
func productionDraftContextCompose(stored map[string]any) (map[string]any, error) {
	unsupported := errors.New("GENERATION_CONTEXT_UNSUPPORTED")
	algorithm := syntheticString(stored["algorithm"])
	if algorithm == "" {
		return nil, unsupported
	}
	values := syntheticSlice(stored["artifactIds"])
	if len(values) == 0 {
		return nil, unsupported
	}
	seen := make(map[string]bool, len(values))
	ids := make([]any, 0, len(values))
	for _, item := range values {
		id, ok := item.(string)
		if !ok || id == "" || seen[id] {
			return nil, unsupported
		}
		seen[id] = true
		ids = append(ids, id)
	}
	text, ok := stored["text"].(string)
	if !ok || !utf8.ValidString(text) {
		return nil, unsupported
	}
	hash := syntheticString(stored["textHash"])
	if hash == "" || hash != syntheticHash(text) {
		return nil, unsupported
	}
	sources := syntheticSlice(stored["sources"])
	if len(sources) == 0 {
		return nil, unsupported
	}
	composition := map[string]any{
		"algorithm":   algorithm,
		"text":        text,
		"textHash":    hash,
		"artifactIds": ids,
		"sources":     sources,
	}
	if chapters := syntheticSlice(stored["chapters"]); len(chapters) != 0 {
		composition["chapters"] = chapters
	}
	return composition, nil
}

// productionDraftContextDraftSave is draftEffects.readRecovery
// (generation-draft-effects.ts:59-79) over this lane's rows: absent when no attempt
// carries a draftCommit, `changed` when the committed text is no longer the body on
// disk, and `current` when the run's own receipt still describes the row. A
// receipt that cannot be verified is refused
// (GENERATION_DRAFT_RECEIPT_INVALID), never reported as absent.
func productionDraftContextDraftSave(ctx context.Context, db *sql.DB, handle syntheticHandle, manifest map[string]any, attempts []productionDraftContextAttempt, composition map[string]any) (map[string]any, error) {
	invalid := errors.New("GENERATION_DRAFT_RECEIPT_INVALID")
	var committed []productionDraftContextAttempt
	for _, attempt := range attempts {
		if syntheticMap(attempt.Usage["draftCommit"]) != nil {
			committed = append(committed, attempt)
		}
	}
	if len(committed) == 0 {
		return map[string]any{"kind": "absent"}, nil
	}
	if len(committed) > 1 {
		return nil, invalid
	}
	if composition == nil {
		return nil, invalid
	}
	stored := syntheticMap(committed[0].Usage["draftCommit"])
	algorithm := syntheticString(composition["algorithm"])
	if !productionDraftContextDraftAlgorithm(algorithm) {
		return nil, invalid
	}
	if success, ok := stored["success"].(bool); !ok || !success {
		return nil, invalid
	}
	chapter, ok := productionDraftContextInt(stored["chapterNumber"])
	if !ok {
		return nil, invalid
	}
	manifestChapter, ok := productionDraftContextInt(manifest["chapterNumber"])
	if !ok || chapter != manifestChapter {
		return nil, invalid
	}
	content, ok := stored["content"].(string)
	if !ok || !utf8.ValidString(content) {
		return nil, invalid
	}
	contentHash := syntheticString(stored["contentHash"])
	if contentHash == "" || contentHash != syntheticHash(content) {
		return nil, invalid
	}
	if contentHash != syntheticString(composition["textHash"]) || content != syntheticString(composition["text"]) {
		return nil, invalid
	}
	manifestBatch := syntheticString(manifest["batchId"])
	if manifestBatch != syntheticString(stored["batchId"]) {
		return nil, invalid
	}
	identity := syntheticMap(committed[0].Usage["artifactIdentity"])
	saved := syntheticMap(stored["handle"])
	if saved == nil || len(saved) != 4 {
		return nil, invalid
	}
	for key, want := range map[string]string{
		"projectId":    handle.ProjectID,
		"epoch":        syntheticString(identity["epoch"]),
		"rootActionId": handle.RootActionID,
		"runId":        handle.RunID,
	} {
		if want == "" || syntheticString(saved[key]) != want {
			return nil, invalid
		}
	}
	id, ok := productionDraftContextInt(stored["id"])
	if !ok || id < 1 {
		return nil, invalid
	}
	version, ok := productionDraftContextInt(stored["version"])
	if !ok || version < 1 {
		return nil, invalid
	}
	// The saved row may be gone; the reference then reports `changed`, because the
	// text it committed is no longer the body on disk. A row that exists but is not
	// the version and chapter the receipt names is a receipt that no longer
	// describes this database, which is GENERATION_DRAFT_RECEIPT_INVALID rather than
	// `changed` (generation-draft-effects.ts:69).
	draft, err := store.NewDrafts(db).GetFull(ctx, id)
	if err != nil {
		return nil, err
	}
	if draft != nil && (draft.ChapterNumber != chapter || draft.Version != version) {
		return nil, invalid
	}
	if draft == nil || draft.Content != content {
		return map[string]any{"kind": "changed"}, nil
	}
	return map[string]any{"kind": "current", "receipt": map[string]any{
		"success":     true,
		"id":          id,
		"version":     version,
		"content":     content,
		"contentHash": contentHash,
	}}, nil
}

// productionDraftContextHandleOf is handleOf(run) (main-generation-owner.ts:170):
// the project and the epoch are the binding's, the root and the run id are the
// run's own. They are read from the binding and the already verified request
// handle, never echoed back from an unverified field.
func productionDraftContextHandleOf(binding map[string]any, handle syntheticHandle) map[string]any {
	return map[string]any{
		"projectId":    syntheticString(binding["projectId"]),
		"epoch":        syntheticString(binding["epoch"]),
		"rootActionId": handle.RootActionID,
		"runId":        handle.RunID,
	}
}

// productionDraftContextSelectedMatches projects the `selectedDrafts` field of
// GenerationRecoveryContext (main-generation-owner.ts:930-945): every frozen draft
// must still be on disk at the frozen version with the frozen content hash, and the
// field is omitted entirely when any of them is not, because the browser treats its
// presence as "these sources are still exactly what the run froze".
func productionDraftContextSelectedMatches(ctx context.Context, db *sql.DB, manifest map[string]any, binding map[string]any) ([]any, bool, error) {
	ids := productionDraftContextIDs(manifest["selectedDraftIds"])
	if len(ids) == 0 {
		return []any{}, true, nil
	}
	required := productionDraftContextRequired(manifest)
	refs := syntheticSlice(binding["sourceRefs"])
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		row, err := store.NewDrafts(db).GetFull(ctx, id)
		if err != nil {
			return nil, false, err
		}
		ref := productionDraftContextRef(refs, fmt.Sprintf("draft:%d", id))
		if row == nil || ref == nil || ref.revision != row.Version || ref.contentHash != syntheticHash(row.Content) {
			return nil, false, nil
		}
		entry := map[string]any{
			"draftId":       id,
			"chapterNumber": row.ChapterNumber,
			"version":       row.Version,
			"contentHash":   ref.contentHash,
			"content":       row.Content,
		}
		if required[id] {
			entry["required"] = true
		}
		out = append(out, entry)
	}
	return out, true, nil
}

// productionDraftContextRef finds one frozen source reference by its source id.
type productionDraftContextSourceRef struct {
	revision    int64
	contentHash string
}

func productionDraftContextRef(refs []any, sourceID string) *productionDraftContextSourceRef {
	for _, item := range refs {
		ref := syntheticMap(item)
		if ref == nil || syntheticString(ref["sourceId"]) != sourceID {
			continue
		}
		revision, ok := productionDraftContextInt(ref["revision"])
		if !ok {
			return nil
		}
		return &productionDraftContextSourceRef{revision: revision, contentHash: syntheticString(ref["contentHash"])}
	}
	return nil
}

// productionDraftContextRequired is the reference's requiredCandidateDraftIds set
// (main-generation-owner.ts:922-925): a material decision may freeze individual
// candidate drafts as required, and those entries carry `required: true` so the
// renderer keeps them out of the optional-source trimming.
func productionDraftContextRequired(manifest map[string]any) map[int64]bool {
	decision := syntheticMap(manifest["materialDecision"])
	if decision == nil {
		return nil
	}
	out := make(map[int64]bool)
	for _, item := range syntheticSlice(decision["included"]) {
		entry := syntheticMap(item)
		if entry == nil || !syntheticBool(entry["required"]) {
			continue
		}
		sourceID := syntheticString(entry["sourceId"])
		const prefix = "candidate:"
		if !strings.HasPrefix(sourceID, prefix) {
			continue
		}
		if id, err := productionDraftContextParseID(sourceID[len(prefix):]); err == nil {
			out[id] = true
		}
	}
	return out
}

// productionDraftContextPurposes is `attemptedPurposes`
// (main-generation-owner.ts:956): one entry per attempt, the recorded purpose or
// the placeholder when the row has none.
func productionDraftContextPurposes(attempts []productionDraftContextAttempt) []any {
	out := make([]any, 0, len(attempts))
	for _, attempt := range attempts {
		purpose := syntheticString(attempt.Usage["purpose"])
		if purpose == "" {
			purpose = productionDraftContextUnknownPurpose
		}
		out = append(out, purpose)
	}
	return out
}

// productionDraftContextFinishReason is `lastCompositionFinishReason`
// (main-generation-owner.ts:941): the finish reason recorded on the attempt that
// produced the last artifact of the composition. The artifact to attempt mapping is
// the one generation_artifacts holds (generation-run-repository.ts:943), and the
// reason itself is the `result.finishReason` the shipped ledger's settle wrote into
// that attempt's usage receipt (internal/store/generation_ledger.go:1106).
func productionDraftContextFinishReason(ctx context.Context, db *sql.DB, attempts []productionDraftContextAttempt, composition map[string]any) (any, error) {
	if composition == nil || db == nil {
		return nil, nil
	}
	ids := syntheticSlice(composition["artifactIds"])
	if len(ids) == 0 {
		return nil, nil
	}
	last := syntheticString(ids[len(ids)-1])
	if last == "" {
		return nil, nil
	}
	var artifactAttempt string
	err := db.QueryRowContext(ctx, "SELECT attempt_id FROM generation_artifacts WHERE artifact_id=?", last).Scan(&artifactAttempt)
	if errors.Is(err, sql.ErrNoRows) {
		// A composition whose last artifact no longer resolves cannot name a finish
		// reason; the reference would fail the same lookup on an undefined row.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, attempt := range attempts {
		if attempt.ID != artifactAttempt {
			continue
		}
		return productionDraftContextResultField(attempt.Usage, "finishReason"), nil
	}
	return nil, nil
}

// productionDraftContextResultField reads `result.<field>` out of one usage
// receipt, the shape the shipped ledger's settle writes
// (internal/store/generation_ledger.go:1106).
func productionDraftContextResultField(usage map[string]any, field string) any {
	result := syntheticMap(usage["result"])
	if result == nil {
		return nil
	}
	if value, present := result[field]; present {
		if text, ok := value.(string); !ok || text == "" {
			return nil
		}
		return result[field]
	}
	return nil
}

// productionDraftContextInt reads an integer that may have been decoded as a
// number by the ledger's order-preserving decoder or as a float64 by this file's
// own json.Unmarshal, and reports whether the value was an integer at all.
func productionDraftContextInt(value any) (int64, bool) {
	switch value := value.(type) {
	case int64:
		return value, true
	case int:
		return int64(value), true
	case float64:
		out := int64(value)
		if float64(out) != value {
			return 0, false
		}
		return out, true
	case json.Number:
		if out, err := value.Int64(); err == nil {
			return out, true
		}
		parsed, err := value.Float64()
		if err != nil {
			return 0, false
		}
		out := int64(parsed)
		if float64(out) != parsed {
			return 0, false
		}
		return out, true
	case string:
		return productionDraftContextParseID(value)
	}
	return 0, false
}

// productionDraftContextNumbers normalizes one frozen id list for the wire.
func productionDraftContextNumbers(value any) []any {
	ids := productionDraftContextIDs(value)
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, id)
	}
	return out
}

// productionDraftContextIDs normalizes one frozen id list in place.
func productionDraftContextIDs(value any) []int64 {
	values := syntheticSlice(value)
	if values == nil {
		return nil
	}
	out := make([]int64, 0, len(values))
	for _, item := range values {
		if id, ok := productionDraftContextInt(item); ok {
			out = append(out, id)
		}
	}
	return out
}

// productionDraftContextList normalizes one frozen object list for the wire: an
// absent list is an empty list, exactly like the reference's `?? []`.
func productionDraftContextList(value any) []any {
	values := syntheticSlice(value)
	if values == nil {
		return []any{}
	}
	return values
}

// productionDraftContextParseID parses the numeric tail of a `candidate:<id>`
// source id.
func productionDraftContextParseID(text string) (int64, error) {
	if text == "" {
		return 0, errors.New("INVALID_ID")
	}
	var out int64
	for _, c := range text {
		if c < '0' || c > '9' {
			return 0, errors.New("INVALID_ID")
		}
		out = out*10 + int64(c-'0')
		if out > 1<<53 {
			return 0, errors.New("INVALID_ID")
		}
	}
	return out, nil
}
