//go:build linux

package main

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's one batch WRITE: generation:begin-batch,
// the operation that starts a multi-chapter batch (src/shared/generation-owner-contract.ts:311,
// web/operations.json:218, invoked by src/services/workflows/batch-chapter-workflow.ts:180).
//
// # What a batch is, and how little this operation has to write
//
// There is no batch table. A batch IS one generation root plus one run whose
// binding_json.sourceManifest.operation is "batch-chapters" and whose manifest
// carries the frozen batchIntent{mode, range, targetUnits}
// (electron/services/main-generation-owner.ts:1522-1527,
// electron/services/generation-draft-effects.ts:105,
// backend-go/internal/store/batch_progress.go:18-28). Everything else about a
// batch - per-chapter progress, the current chapter handle, the next chapter
// cursor - is DERIVED from that root and its children by batch_progress.go, whose
// read path this lane already serves (production_batch_reads_linux.go). So this
// operation opens a root and a run and writes nothing else: no attempt, no
// artifact, no draft, no outbox row, and no provider call. The reference does the
// same and answers with the derived progress of the batch it just opened - its
// beginBatch ends `return readBatch(run.handle.runId)` (main-generation-owner.ts:
// 1526) - which is exactly what this file returns.
//
// # The admission is generation:begin's, not a second one
//
// The root and the run are opened through the same ledger path generation:begin
// uses (production_generation_begin_linux.go, synthetic_generation_linux.go:
// 815-883): the reused fence (syntheticGeneration.admit:319-343, which re-proves
// the frozen three-key session and the live writer lease before lending the
// author database), the same model-registry lease (registry.Begin), the same
// frozen source build and BindSources, the same canonical plan, the same
// fingerprint and the same single ledger "open" call. This file contributes the
// batch-specific middle: the intent it validates, the manifest keys the batch
// read derives from, and the root budget a multi-chapter root needs. It does not
// open a database, resolve a model, or build a second registry.
//
// # Why the manifest is composed here
//
// syntheticManifest (synthetic_generation_linux.go:704-726) is the reduced
// lane's single-chapter manifest: it freezes operation/modelId/modelLeaseId and
// the selection, but not a batchIntent and not a modelReceipt. Both are required
// for the run to be a batch at all: batch_progress.go:451-470 refuses
// GENERATION_BATCH_INTENT_INVALID without an intent and
// GENERATION_BATCH_IDENTITY_INVALID without sourceManifest.modelReceipt.modelId,
// and electron/services/generation-source-binding.ts:572 freezes both (plus
// batchId, which only a chapter CHILD carries). This file therefore starts from
// syntheticManifest and adds exactly those two keys; the lane's own manifest
// function is used unchanged, so nothing here re-freezes a source hash.
//
// # Idempotency is the ledger's open key, exactly as generation:begin
//
// A replayed uiActionNonce never opens a second batch because it never reaches a
// second INSERT: the ledger derives its idempotency key from
// (projectId, operation, uiActionNonce, frozenInputHash) and its open key from
// that plus the parent (internal/store/generation_contract.go:141-158,
// generation_ledger.go:673-744). When a run already carries that key the ledger
// returns it only if the frozen source manifest, the source refs (modulo epoch),
// the fingerprint and the budget are identical; otherwise it refuses
// GENERATION_NONCE_CONFLICT. Nothing in this file bypasses that: the fingerprint
// and the budget are deterministic for a given selection and intent, so a replay
// collides on the same key instead of minting a new root. (The one value that
// differs between two attempts is the model-lease id, which a replay of
// generation:begin also changes; that is why a replay can answer the conflict
// code rather than the original run. Either answer opens nothing new, which is
// the property the test pins.)
//
// # The one honest reduction: the root budget
//
// The reference scales a batch root's budget by the number of chapters in the
// range (electron/services/main-generation-plan.ts:127-137, :176-180:
// BATCH_ROOT_PHYSICAL_REQUESTS_PER_CHAPTER, max(policy, chapters * perChapter),
// with the token liability scaled by the same factor), because every chapter
// child run charges that one root. The reduced lane's canonical budget
// (syntheticBudget, synthetic_generation_linux.go:689-699) is a single-chapter
// bound, so this file multiplies its three cumulative bounds by the chapter count
// and leaves the per-request output cap alone. A root that kept the
// single-chapter numbers would refuse every chapter after the first
// (GLAssertReservation in the ledger enforces the ROOT policy). This is a scaling
// of the lane's own numbers, not a port of newMainGenerationPolicy.

// productionBatchWrite publishes generation:begin-batch over the owner this lane
// already built. It holds no database, no registry and no executor of its own:
// everything it does travels the owner's admission, lease and ledger.
type productionBatchWrite struct {
	owner *productionGeneration
}

// The one name this file publishes, its wire arity (the request plus the frozen
// three-key project session, like every generation: channel), the operation name
// the frozen manifest carries, and the two bounds of assertGenerationBatchIntent.
const (
	productionBatchWriteName        = "generation:begin-batch"
	productionBatchWriteArity       = 1
	productionBatchWriteOperation   = "batch-chapters"
	productionBatchWriteOutput      = "visible-text"
	productionBatchWriteMaxChapters = int64(10)
	// productionBatchWriteMaxTargetUnits is the intent's targetUnits ceiling
	// (generation-draft-effects.ts:28, batch_progress.go:71).
	productionBatchWriteMaxTargetUnits = int64(1_000_000)
	// productionBatchWriteMaxNonceRunes is the uiActionNonce length bound the
	// begin admission applies (main-generation-owner.ts:413-417).
	productionBatchWriteMaxNonceRunes = 256
)

// productionBatchWriteIntent is the validated GenerationBatchIntent
// (src/shared/generation-owner-contract.ts:225-229): the three fields the batch
// read derives mode/range/targetUnits from.
type productionBatchWriteIntent struct {
	Mode         string
	StartChapter int64
	EndChapter   int64
	TargetUnits  int64
}

// productionBatchWriteRequest is one validated begin-batch request: the frozen
// intent plus the reduced selection the lane's own admission accepts.
type productionBatchWriteRequest struct {
	Selection syntheticSelection
	Intent    productionBatchWriteIntent
}

// newProductionBatchWrite wraps the one generation owner this lane built, so
// begin-batch shares generation:begin's registry, model leases and admission
// instead of constructing a second owner. It must be called with the same owner
// newProductionGeneration returned.
func newProductionBatchWrite(owner *productionGeneration) *productionBatchWrite {
	return &productionBatchWrite{owner: owner}
}

// Bind installs generation:begin-batch and nothing else. A nil owner installs
// nothing and is not an error (the name then keeps failing closed at
// GO_OPERATION_NOT_MIGRATED), exactly like transport.BindGenerationReads with a
// nil provider; the caller only calls this in the branch that built an owner.
//
// The handler borrows this package's copy of the generation family adapter
// (productionGenerationMutation, production_generation_write_linux.go:174-203):
// wire arity is the request plus the frozen expected context, the context must be
// the exact three-key map, and every failure leaves as the mutating business DTO
// {"success":false,"error":"Error: CODE"} with HTTP 200 - the envelope
// generation:begin answers with (internal/transport/generation.go:306-319).
func (w *productionBatchWrite) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if w == nil || w.owner == nil {
		return nil
	}
	return server.Bind(productionBatchWriteName, productionGenerationMutation(productionBatchWriteArity,
		func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			return w.owner.BeginBatch(ctx, caller, expected, args[0])
		}))
}

// BeginBatch answers generation:begin-batch: it validates the frozen intent,
// admits the live lease, opens one generation root and one batch run, and returns
// the batch progress read back from those rows.
//
// The order is the reference's: assertGenerationBatchIntent runs before anything
// else (main-generation-owner.ts:1523), so a malformed intent refuses with
// GENERATION_BATCH_INTENT_INVALID and touches neither the author database nor a
// model lease. Only then do the two gates generation:begin also carries apply (the
// committed model-egress policy, and the owner this lane built from it), and only
// then is the frozen session admitted.
//
// Nothing here dispatches. The run has no attempt row when this returns, so a
// provider call is still generation:execute's separately gated job.
func (g *productionGeneration) BeginBatch(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if g == nil || g.open == nil || g.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	request, err := productionBatchWriteParse(arg)
	if err != nil {
		return nil, err
	}
	// The same gate generation:begin applies (production_generation_begin_linux.go:
	// 52-56): a lane whose committed model-egress policy is unusable never opens a
	// run, because a run opened here is a run this lane's generation:execute would
	// have to dispatch without a boundary.
	if _, err := g.open.ModelEgress(); err != nil {
		return nil, err
	}
	inner := g.inner
	_, scope, ledger, db, done, err := inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	selection := request.Selection
	receipt, err := inner.registry.Begin(ctx, scope, selection.ModelID, syntheticGenerationPurpose)
	if err != nil {
		return nil, err
	}
	set, err := syntheticBuildSources(ctx, db, selection)
	if err != nil {
		return nil, err
	}
	binding, err := inner.registry.BindSources(ctx, scope, receipt.LeaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, err
	}
	plan, err := inner.syntheticCanonicalPlan(ctx, scope, receipt.LeaseID, selection)
	if err != nil {
		return nil, err
	}
	budget, err := productionBatchWriteBudget(plan, request.Intent)
	if err != nil {
		return nil, err
	}
	fingerprint, err := syntheticFingerprint(ctx, db, scope, binding, set, selection, receipt, budget)
	if err != nil {
		return nil, err
	}
	outputContract := syntheticOutputContract(selection)
	frozenInput, err := syntheticCanonical([]any{fingerprint, set.CoreText, outputContract})
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	refs, err := syntheticRefsAsAny(binding.Refs())
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	open := map[string]any{
		"projectId":         scope.Project,
		"epoch":             scope.Epoch,
		"fingerprint":       fingerprint,
		"contextSnapshotId": syntheticHash(set.CoreText),
		"sourceManifest":    productionBatchWriteManifest(selection, request.Intent, scope, receipt, binding.Fingerprint()),
		"sourceRefs":        refs,
		"operation":         productionBatchWriteOperation,
		"uiActionNonce":     selection.UIActionNonce,
		"frozenInputHash":   syntheticHash(frozenInput),
		"budget":            budget,
	}
	// The batch root is a root: no parentRootActionId. Only a chapter child run
	// freezes a batchId naming this root (generation-source-binding.ts:572), and
	// this operation never opens one.
	args, err := syntheticArgs(open)
	if err != nil {
		return nil, errors.New("GENERATION_BEGIN_INVALID")
	}
	runValue, err := ledger.CallJSON(ctx, "open", args)
	if err != nil {
		return nil, err
	}
	runID := syntheticString(syntheticMap(runValue)["runId"])
	if runID == "" {
		return nil, errors.New("GENERATION_RUN_MISSING")
	}
	// The reference returns readBatch(run.handle.runId), not the run view
	// (main-generation-owner.ts:1526-1527). Serving the same derivation over the
	// rows just written also refuses here - under this operation's own refusal
	// codes - any batch root whose frozen manifest the read path cannot derive,
	// instead of handing the renderer a handle to a batch it cannot read.
	return store.ReadGenerationBatchProgress(ctx, db, scope.Project, runID)
}

// productionBatchWriteParse validates one begin-batch request.
//
// The intent is validated FIRST and with the reference's exact rules
// (assertGenerationBatchIntent, electron/services/generation-draft-effects.ts:
// 22-29):
//
//	if (!intent || !['draft_review', 'auto_finalize'].includes(intent.mode) || !intent.range
//	  || !Number.isSafeInteger(intent.range.startChapter) || !Number.isSafeInteger(intent.range.endChapter)
//	  || intent.range.startChapter < 1 || intent.range.endChapter < intent.range.startChapter
//	  || intent.range.endChapter - intent.range.startChapter >= MAX_BATCH_CHAPTERS
//	  || !Number.isSafeInteger(intent.targetUnits) || intent.targetUnits < 1 || intent.targetUnits > 1_000_000)
//	  throw new Error('GENERATION_BATCH_INTENT_INVALID')
//
// MAX_BATCH_CHAPTERS is 10 (src/shared/generation-owner-contract.ts:224), so a
// ten-chapter span (start 1, end 10) is admitted and an eleven-chapter span is
// refused. Number.isSafeInteger is reproduced by productionBatchInteger
// (production_batch_reads_linux.go:261-287): a whole JavaScript-safe number and
// nothing else, never a numeric string.
//
// The request allowlist is this file's, and it is NARROWER than the family's
// internal selection allowlist (main-generation-owner.ts:413-417): the begin-batch
// wire request is BeginGenerationBatchRequest
// (src/shared/generation-owner-contract.ts:230-236), i.e. the five selection
// fields the reference destructures plus the three intent fields, and nothing
// else. In particular batchId and batchIntent are NOT part of it - beginBatch is
// what turns a request into those (main-generation-owner.ts:1524-1526) - so a
// caller that sends either is refused with GENERATION_BEGIN_INVALID rather than
// being able to name a different batch lineage. The five admitted selection
// fields are then handed, translated, to the lane's own selection admission
// (syntheticParseSelection, synthetic_generation_linux.go:391-476), so prompt
// keys, skill stages and author inputs are validated by exactly the rules
// generation:begin applies to them and this file owns no copy of those rules.
func productionBatchWriteParse(arg any) (productionBatchWriteRequest, error) {
	object := syntheticMap(arg)
	if object == nil {
		return productionBatchWriteRequest{}, errors.New("GENERATION_BATCH_INTENT_INVALID")
	}
	intent, err := productionBatchWriteIntentOf(object)
	if err != nil {
		return productionBatchWriteRequest{}, err
	}
	allowed := map[string]bool{
		"uiActionNonce": true,
		"modelId":       true,
		"promptKeys":    true,
		"skillStages":   true,
		"authorInputs":  true,
		"mode":          true,
		"range":         true,
		"targetUnits":   true,
	}
	for key := range object {
		if !allowed[key] {
			return productionBatchWriteRequest{}, errors.New("GENERATION_BEGIN_INVALID")
		}
	}
	selection, err := productionBatchWriteSelection(object)
	if err != nil {
		return productionBatchWriteRequest{}, err
	}
	return productionBatchWriteRequest{Selection: selection, Intent: intent}, nil
}

// productionBatchWriteIntentOf is assertGenerationBatchIntent over the raw
// request. Every branch refuses GENERATION_BATCH_INTENT_INVALID, and the range is
// normalised to the two validated integers so the frozen intent carries exactly
// mode/range/targetUnits and no renderer-supplied extra.
func productionBatchWriteIntentOf(object map[string]any) (productionBatchWriteIntent, error) {
	invalid := errors.New("GENERATION_BATCH_INTENT_INVALID")
	mode, ok := object["mode"].(string)
	if !ok || (mode != "draft_review" && mode != "auto_finalize") {
		return productionBatchWriteIntent{}, invalid
	}
	chapterRange := syntheticMap(object["range"])
	if chapterRange == nil {
		return productionBatchWriteIntent{}, invalid
	}
	start, startOK := productionBatchInteger(chapterRange["startChapter"])
	end, endOK := productionBatchInteger(chapterRange["endChapter"])
	target, targetOK := productionBatchInteger(object["targetUnits"])
	if !startOK || !endOK || !targetOK || start < 1 || end < start ||
		end-start >= productionBatchWriteMaxChapters || target < 1 || target > productionBatchWriteMaxTargetUnits {
		return productionBatchWriteIntent{}, invalid
	}
	return productionBatchWriteIntent{Mode: mode, StartChapter: start, EndChapter: end, TargetUnits: target}, nil
}

// productionBatchWriteSelection projects the five admitted request fields onto
// the frozen selection the reference's beginBatch passes to begin
// (main-generation-owner.ts:1524-1526): operation "batch-chapters", an empty
// draft selection on both sides, visible-text output, and the renderer's own
// prompt keys, skill stages and author inputs. The nonce and model gates are the
// begin admission's (main-generation-owner.ts:413-417): a blank or over-long nonce
// and a blank model id refuse GENERATION_BEGIN_INVALID, and promptKeys must be the
// non-empty array that check requires.
func productionBatchWriteSelection(object map[string]any) (syntheticSelection, error) {
	invalid := errors.New("GENERATION_BEGIN_INVALID")
	nonce, ok := object["uiActionNonce"].(string)
	if !ok || !utf8.ValidString(nonce) || strings.TrimSpace(nonce) == "" ||
		utf8.RuneCountInString(nonce) > productionBatchWriteMaxNonceRunes {
		return syntheticSelection{}, invalid
	}
	modelID, ok := object["modelId"].(string)
	if !ok || !utf8.ValidString(modelID) || modelID == "" {
		return syntheticSelection{}, invalid
	}
	if keys := syntheticSlice(object["promptKeys"]); len(keys) == 0 {
		return syntheticSelection{}, invalid
	}
	return syntheticParseSelection(map[string]any{
		"operation":                 productionBatchWriteOperation,
		"uiActionNonce":             nonce,
		"modelId":                   modelID,
		"output":                    productionBatchWriteOutput,
		"selectedDraftIds":          []any{},
		"selectedFinalizedDraftIds": []any{},
		"promptKeys":                object["promptKeys"],
		"skillStages":               object["skillStages"],
		"authorInputs":              object["authorInputs"],
	})
}

// productionBatchWriteManifest is the batch root's frozen source manifest: the
// lane's reduced manifest plus the two keys that make the run a batch.
//
//	batchIntent  the validated intent. batch_progress.go derives mode/range/
//	             targetUnits from it and refuses GENERATION_BATCH_INTENT_INVALID
//	             without it; list-batches marks a batch root by its presence
//	             (batch_progress.go:225-231), exactly like runSummaries
//	             (main-generation-owner.ts:346-351).
//	modelReceipt bpSnapshot reads modelReceipt.modelId and refuses
//	             GENERATION_BATCH_IDENTITY_INVALID when it is absent
//	             (batch_progress.go:466-471); the reference freezes the same
//	             receipt (generation-source-binding.ts:572, :119-126), and the
//	             lease the lane already began holds every field of it.
//
// No batchId is written: a batch ROOT carries the intent and only its chapter
// children carry the id (main-generation-owner.ts:346-351, :1524-1526).
func productionBatchWriteManifest(selection syntheticSelection, intent productionBatchWriteIntent, scope generationowner.Scope, receipt generationowner.Receipt, fingerprint string) map[string]any {
	manifest := syntheticManifest(selection, scope, receipt.LeaseID, fingerprint)
	manifest["batchIntent"] = map[string]any{
		"mode": intent.Mode,
		"range": map[string]any{
			"startChapter": intent.StartChapter,
			"endChapter":   intent.EndChapter,
		},
		"targetUnits": intent.TargetUnits,
	}
	manifest["modelReceipt"] = map[string]any{
		"modelId":             receipt.ModelID,
		"provider":            receipt.Provider,
		"protocol":            receipt.Protocol,
		"modelName":           receipt.ModelName,
		"modelRevision":       receipt.ModelRevision,
		"endpointFingerprint": receipt.EndpointFingerprint,
		"capabilityEvidence":  receipt.CapabilityEvidence,
	}
	return manifest
}

// productionBatchWriteBudget is the lane's canonical root budget scaled by the
// chapters the intent spans (see the file comment: the reference multiplies the
// per-request allowance and the root liability by the chapter count because every
// chapter child run charges this one root). The per-request output cap is NOT
// scaled: it bounds one request, not the root.
func productionBatchWriteBudget(plan generationowner.Plan, intent productionBatchWriteIntent) (map[string]any, error) {
	base, err := syntheticBudget(plan)
	if err != nil {
		return nil, err
	}
	chapters := intent.EndChapter - intent.StartChapter + 1
	if chapters < 1 {
		return nil, errors.New("GENERATION_BATCH_INTENT_INVALID")
	}
	// scale multiplies one of the lane's cumulative bounds. Both integer spellings
	// the lane could use are accepted, so this stays correct if the budget map
	// ever starts carrying int64; anything else is a budget this file must not
	// silently round.
	scale := func(key string) (any, error) {
		switch value := base[key].(type) {
		case int:
			return value * int(chapters), nil
		case int64:
			return value * chapters, nil
		default:
			return nil, errors.New("GENERATION_PLANNING_BUDGET_INVALID")
		}
	}
	requests, err := scale("maxPhysicalRequests")
	if err != nil {
		return nil, err
	}
	liability, err := scale("maxTokenLiability")
	if err != nil {
		return nil, err
	}
	elapsed, err := scale("maxActiveElapsedMs")
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"maxPhysicalRequests": requests,
		"maxTokenLiability":   liability,
		"maxOutputPerRequest": base["maxOutputPerRequest"],
		"maxActiveElapsedMs":  elapsed,
	}, nil
}
