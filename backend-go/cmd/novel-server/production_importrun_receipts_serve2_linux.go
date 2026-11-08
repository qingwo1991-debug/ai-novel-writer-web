//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file SERVES the import-run family's two receipt writes on top of the one
// generation owner this lane already runs everything else through:
//
//	db:import-run-effect-receipt-prepare  served  (productionImportRunReceiptsServe2.prepare)
//	db:import-run-effect-receipt-commit   served  (productionImportRunReceiptsServe2.commit)
//
// The pair is the last piece of the family the seam file used to refuse, and the
// two reasons it refused were both about the generation-source proof. Both are
// now answered, and neither answer is a shortcut:
//
//	1. THE CONNECTION. The assert is invoked from INSIDE the receipt's BEGIN
//	   IMMEDIATE (internal/store/importrun_receipts.go, through importRunImmediate
//	   at internal/store/importrun.go:948), which reserves the author database's
//	   only connection (internal/projectfs/author_linux.go:121
//	   db.SetMaxOpenConns(1)). The callback used to carry (handle, slot) and
//	   nothing else, so the reference's FIRST step - requireRun's read at
//	   electron/services/main-generation-owner.ts:1454 - had to borrow a second
//	   handle and waited for the one the transaction held: the store's own name
//	   for that shape is "a deadlock rather than a proof"
//	   (internal/store/character_proposal_pending.go:37). The store now performs
//	   that read on its own connection and hands the result to the proof as data
//	   (internal/store/importrun_receipts_generation.go,
//	   ImportRunGenerationEvidence): binding_json verbatim, the root's status, and
//	   every attempt of this run with its usage receipt and artifact. The prover
//	   below reads nothing but that evidence, so there is no second connection to
//	   wait on.
//	2. THE OWNER. The fingerprint reproduction is
//	   generationowner.Registry.ReproduceBinding (internal/generationowner/binding.go:63),
//	   and the registry that owns the run's model lease is a field of the one
//	   *productionGeneration this process builds
//	   (production_generation_begin_linux.go:42-45). This binder is therefore
//	   constructed WITH the owner, exactly like newProductionFinalizationServe,
//	   newProductionLegacyRosterServe, newProductionAgentGenerationServe and
//	   newProductionImportGenerationServe:
//
//		production_open_linux.go, inside the owner block that opens with
//		"if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {",
//		one line after newProductionImportGenerationServe(owner).Bind(server) and
//		before the block's closing "} else {":
//		  newProductionImportRunReceiptsServe2(owner).Bind(server)
//
//	   A lane that never builds that binder keeps both names at
//	   GO_OPERATION_NOT_MIGRATED - Bind installs nothing when the owner is absent
//	   - so an unavailable generation owner can never turn into a fake success.
//
// # What the proof still refuses, and how
//
// The reference's assert (electron/services/main-generation-owner.ts:1453-1472) is
// five checks, and every one of them is decided HERE, inside the transaction, from
// the evidence the store handed over plus the CURRENT sources and the CURRENT
// model profile:
//
//	:1454 requireRun(handle)      a handle whose run is absent is
//	                              GENERATION_RUN_MISSING and a handle that is not
//	                              the run's own identity is
//	                              GENERATION_RUN_IDENTITY_MISMATCH
//	                              (electron/repositories/generation-run-repository.ts:118-126,
//	                              main-generation-owner.ts:156-167). The projectId
//	                              half is compared with the FROZEN session, which is
//	                              this lane's deps.projectId.
//	:1462 imports.context(run).slot
//	                              the run's frozen {runId, stage, batchId} must equal
//	                              the receipt's own slot, or
//	                              GENERATION_IMPORT_EFFECT_SLOT_CHANGED
//	:1463-1465 rebuildBinding     the run's frozen fingerprint must still reproduce,
//	                              or GENERATION_IMPORT_EFFECT_SOURCE_CHANGED. This is
//	                              generationowner.ReproduceBinding over the CURRENT
//	                              sources, resolved through the lease record the
//	                              begin minted, and compared with the frozen
//	                              sourceManifest.goOwnerSourceFingerprint.
//	:1465-1471 root and attempts the root must not be cancelled
//	                              (GENERATION_ROOT_MISSING when the root row is
//	                              gone), the run must have at least one attempt, and
//	                              every attempt must have finished with stop, carry
//	                              an artifact with non-blank text whose textHash
//	                              re-derives, and not be discarded. Anything else is
//	                              GENERATION_IMPORT_EFFECT_SOURCE_CHANGED.
//
// So the assert is not weakened by receiving its run as data: the only thing it
// stopped doing is opening a connection. It is also not vacuous - a run the lane
// really began through generation:begin, dispatched through
// import-generation:execute, and only then receipted is the shape the test drives.
//
// # The two narrowings, stated instead of hidden
//
//   - The reproduction is PROMPT-ONLY. An import stage freezes prompt refs and
//     nothing else (productionImportStageSelectionOf, and the frozen chapter
//     material lives in sourceManifest.importContext, not in sourceRefs), so the
//     reader set this assert builds carries only the prompt reader - the same
//     "synthetic:<key>" text the begin read (syntheticBuildSources). A run whose
//     frozen refs name any other kind is refused rather than reproduced with a
//     reader that would have to touch the author database and re-open the
//     deadlock.
//   - The reproduction is bound to the run's OWN scope, which includes the tab
//     that began it: ReproduceBinding resolves its lease record by exact scope
//     equality (internal/generationowner/lease.go:164). A receipt presented from
//     another tab of the same session therefore refuses with the registry's own
//     MODEL_EXECUTION_LEASE_INVALID instead of reproducing. That is stricter than
//     the reference's per-project rebuild, and stricter is the safe direction:
//     the refusal is symbolic and nothing is written.
//
// # The envelope
//
// db-controller.ts:425 answers { success: true, receipt } and :437 answers
// { success: true, result: { receipt, run, cancelApplied } }, which is exactly what
// the store's own two methods return. Every refusal is the import-run family's own
// HTTP envelope through productionImportRunWriteFailure, the same mapping the four
// served names in production_importrun_residual_linux.go use, so a stale execution
// lease or a changed project path is byte-for-byte the answer a sibling name gives.

const (
	// productionImportRunReceiptsServe2RunMissing is the reference's absent-run
	// code (electron/repositories/generation-run-repository.ts:126).
	productionImportRunReceiptsServe2RunMissing = "GENERATION_RUN_MISSING"
	// productionImportRunReceiptsServe2Identity is requireRun's identity refusal
	// (main-generation-owner.ts:158-161): the handle must be the run's own
	// {projectId, epoch, rootActionId, runId} and the run must belong to this
	// project.
	productionImportRunReceiptsServe2Identity = "GENERATION_RUN_IDENTITY_MISMATCH"
	// productionImportRunReceiptsServe2SlotChanged is :1462.
	productionImportRunReceiptsServe2SlotChanged = "GENERATION_IMPORT_EFFECT_SLOT_CHANGED"
	// productionImportRunReceiptsServe2SourceChanged is :1471: the frozen sources
	// no longer reproduce, the root is cancelled, or an attempt/artifact does not
	// verify.
	productionImportRunReceiptsServe2SourceChanged = "GENERATION_IMPORT_EFFECT_SOURCE_CHANGED"
	// productionImportRunReceiptsServe2RootMissing is the store's own code for a
	// root row that is gone (internal/store/draft_commit_tx.go:283), which is what
	// repository.budget(rootActionId) answers in the reference.
	productionImportRunReceiptsServe2RootMissing = "GENERATION_ROOT_MISSING"
)

// productionImportRunReceiptsServe2Binding is the part of the frozen RunBinding
// this assert reads. It is decoded from generation_runs.binding_json verbatim, so
// a field the begin wrote under another name simply does not match instead of
// being guessed at.
type productionImportRunReceiptsServe2Binding struct {
	ProjectID      string `json:"projectId"`
	Epoch          string `json:"epoch"`
	SourceManifest struct {
		Operation                string   `json:"operation"`
		ModelLeaseID             string   `json:"modelLeaseId"`
		GoOwnerSourceFingerprint string   `json:"goOwnerSourceFingerprint"`
		PromptKeys               []string `json:"promptKeys"`
		ImportSlot               struct {
			RunID   string `json:"runId"`
			Stage   string `json:"stage"`
			BatchID string `json:"batchId"`
		} `json:"importSlot"`
	} `json:"sourceManifest"`
	SourceRefs []struct {
		SourceID string `json:"sourceId"`
	} `json:"sourceRefs"`
}

// productionImportRunReceiptsServe2 serves the two names over the one generation
// owner generation:execute already dispatches through. It borrows the durable
// runtime from that owner for its own database access, so admission, the registry
// and the single-writer fence are the lane's own.
type productionImportRunReceiptsServe2 struct {
	owner *productionGeneration
}

// newProductionImportRunReceiptsServe2 returns the binder for the lane built by
// productionHandlerWithContract. It resolves nothing and opens nothing.
func newProductionImportRunReceiptsServe2(owner *productionGeneration) *productionImportRunReceiptsServe2 {
	return &productionImportRunReceiptsServe2{owner: owner}
}

// ready reports whether the owner can supply everything the proof needs: the
// admitted open for the borrow fence and the registry that holds the run's model
// lease.
func (r *productionImportRunReceiptsServe2) ready() bool {
	return r != nil && r.owner != nil && r.owner.open != nil && r.owner.inner != nil &&
		r.owner.inner.registry != nil && r.owner.open.durable != nil
}

// Bind installs the two served names. A nil or unavailable owner is not an error:
// both keep failing closed at GO_OPERATION_NOT_MIGRATED, exactly like the sibling
// owner-bound binders. It is wired from production_open_linux.go's owner block,
// beside newProductionImportGenerationServe(owner).Bind(server).
func (r *productionImportRunReceiptsServe2) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if !r.ready() {
		return nil
	}
	if err := server.Bind(productionImportRunReceiptsPrepareOperation,
		r.operation(productionImportRunReceiptOperation{
			name: productionImportRunReceiptsPrepareOperation, arity: 4, pathIndex: 2,
		})); err != nil {
		return err
	}
	return server.Bind(productionImportRunReceiptsCommitOperation,
		r.operation(productionImportRunReceiptOperation{
			name: productionImportRunReceiptsCommitOperation, arity: 6, pathIndex: 4,
		}))
}

// productionImportRunReceiptOperation is one served name: its exact wire arity and
// the index of the frozen project path, both counted against
// src/shared/ipc-channels.ts (prepare: request, execution, path, ctx; commit:
// runId, stage, batchId, execution, path, ctx).
type productionImportRunReceiptOperation struct {
	name      string
	arity     int
	pathIndex int
}

// operation builds the handler of one served name. The argument order, the frozen
// context parsing and every pre-store refusal mirror
// production_importrun_residual_linux.go's handler, so the renderer sees one
// contract whichever binder serves an import-run name.
func (r *productionImportRunReceiptsServe2) operation(operation productionImportRunReceiptOperation) transport.Operation {
	name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if !r.ready() {
			return nil, errors.New("IMPORT_RUN_RECEIPT_PROVIDER_UNAVAILABLE")
		}
		// Arity first: a stale lease or a project path in the wrong position is a
		// malformed request, never authority.
		if len(args) != arity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		// The frozen context is always its own last argument.
		expected, err := productionImportRunWriteExpectedContext(args, arity-1)
		if err != nil {
			return nil, err
		}
		// Every original handler calls assertRequiredExpectedProjectPath first, so
		// the explicit path must be present and must name the frozen project. This
		// refusal is answered before the borrow, exactly like the sibling slice, so a
		// foreign path never reaches the author database.
		path, ok := args[pathIndex].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		if path != expected["projectPath"] {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		request, err := productionImportRunReceiptsServe2Request(name, args)
		if err != nil {
			return nil, err
		}
		db, release, err := r.owner.open.durable.borrowAuthorDatabase(ctx, caller, expected)
		if release != nil {
			defer release()
		}
		if err != nil {
			return nil, productionImportRunWriteFailure(err)
		}
		if db == nil {
			return nil, errors.New("IMPORT_RUN_RECEIPT_PROVIDER_UNAVAILABLE")
		}
		return r.call(ctx, caller, expected, db, name, request)
	}
}

// productionImportRunReceiptsServe2Wire is one decoded request: the prepare
// request object and the commit's three positional arguments, resolved into the
// store's own shapes. execution is shared because both channels carry the same
// ImportRunExecutionLease.
type productionImportRunReceiptsServe2Wire struct {
	runID     string
	stage     string
	batchID   string
	effectKey string
	kind      string
	payload   any
	execution store.ImportRunExecution
}

// productionImportRunReceiptsServe2Request decodes one name's own arguments. A
// field the channel declares as a string that does not arrive as one is a
// malformed request: the renderer's own type is the contract, and coercing it
// here would answer the store's refusal for a request the browser could not have
// sent.
func productionImportRunReceiptsServe2Request(name string, args []any) (productionImportRunReceiptsServe2Wire, error) {
	invalid := security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	wire := productionImportRunReceiptsServe2Wire{}
	switch name {
	case productionImportRunReceiptsPrepareOperation:
		object, ok := args[0].(map[string]any)
		if !ok || object == nil {
			return productionImportRunReceiptsServe2Wire{}, invalid
		}
		for _, field := range []struct {
			key    string
			target *string
		}{
			{"runId", &wire.runID}, {"stage", &wire.stage}, {"batchId", &wire.batchID},
			{"effectKey", &wire.effectKey}, {"kind", &wire.kind},
		} {
			text, ok := object[field.key].(string)
			if !ok || !utf8.ValidString(text) {
				return productionImportRunReceiptsServe2Wire{}, invalid
			}
			*field.target = text
		}
		// The payload crosses untouched, including a null one: the store's own
		// canonicalizer decides PAYLOAD_INVALID, and pre-empting it here would answer
		// another code than the reference.
		wire.payload = object["payload"]
		execution, ok := productionImportRunWriteExecution(args[1])
		if !ok {
			// The original dereferences the lease object directly, so an unusable one
			// fails its authority check rather than the argument check.
			return productionImportRunReceiptsServe2Wire{}, productionImportRunWriteFailure(
				errors.New("IMPORT_RUN_EXECUTION_STALE"))
		}
		wire.execution = execution
		return wire, nil
	case productionImportRunReceiptsCommitOperation:
		var ok bool
		if wire.runID, ok = productionImportRunWriteArgument(args, 0); !ok {
			return productionImportRunReceiptsServe2Wire{}, invalid
		}
		if wire.stage, ok = productionImportRunWriteArgument(args, 1); !ok {
			return productionImportRunReceiptsServe2Wire{}, invalid
		}
		if wire.batchID, ok = productionImportRunWriteArgument(args, 2); !ok {
			return productionImportRunReceiptsServe2Wire{}, invalid
		}
		execution, ok := productionImportRunWriteExecution(args[3])
		if !ok {
			return productionImportRunReceiptsServe2Wire{}, productionImportRunWriteFailure(
				errors.New("IMPORT_RUN_EXECUTION_STALE"))
		}
		wire.execution = execution
		return wire, nil
	}
	return productionImportRunReceiptsServe2Wire{}, errors.New("IMPORT_RUN_RECEIPT_OPERATION_UNKNOWN")
}

// call invokes the store method behind one name. Both are one immediate
// transaction the store owns; this adapter opens none, and the second argument is
// the proof, built per request because it needs the caller's own scope.
func (r *productionImportRunReceiptsServe2) call(ctx context.Context, caller transport.Caller,
	expected map[string]any, db *sql.DB, name string, wire productionImportRunReceiptsServe2Wire) (any, error) {
	repository := store.NewImportRuns(db)
	proof := r.generationSources(ctx, caller, expected)
	switch name {
	case productionImportRunReceiptsPrepareOperation:
		receipt, err := repository.PrepareEffectReceipt(ctx, wire.execution,
			store.ImportRunPrepareEffectReceiptRequest{
				RunID: wire.runID, Stage: wire.stage, BatchID: wire.batchID,
				EffectKey: wire.effectKey, Kind: wire.kind, Payload: wire.payload,
			}, proof)
		if err != nil {
			return nil, productionImportRunWriteFailure(err)
		}
		return map[string]any{"success": true, "receipt": receipt}, nil
	case productionImportRunReceiptsCommitOperation:
		result, err := repository.CommitEffectReceipt(ctx, wire.runID, wire.stage, wire.batchID,
			wire.execution, proof)
		if err != nil {
			return nil, productionImportRunWriteFailure(err)
		}
		return map[string]any{"success": true, "result": result}, nil
	}
	return nil, errors.New("IMPORT_RUN_RECEIPT_OPERATION_UNKNOWN")
}

// generationSources builds the store proof for one request. It is the reference's
// assertImportGenerationSourcesCurrent (electron/controllers/generation-controller.ts:72-75
// -> main-generation-owner.ts:1453-1472) with the run supplied by the store
// instead of read through a second connection.
func (r *productionImportRunReceiptsServe2) generationSources(ctx context.Context, caller transport.Caller,
	expected map[string]any) store.ImportRunGenerationSourceAssert {
	projectID := syntheticString(expected["projectId"])
	return func(handle any, slot store.ImportRunGenerationSlot, run store.ImportRunGenerationEvidence) error {
		if !r.ready() {
			return errors.New("IMPORT_RUN_RECEIPT_PROVIDER_UNAVAILABLE")
		}
		// :1454 requireRun(handle), whose first line is repository.get(runId):
		// a run this database does not hold is GENERATION_RUN_MISSING.
		if !run.RunPresent {
			return errors.New(productionImportRunReceiptsServe2RunMissing)
		}
		var binding productionImportRunReceiptsServe2Binding
		if err := json.Unmarshal(run.BindingJSON, &binding); err != nil {
			// A binding that does not decode cannot be the run's own identity,
			// which is requireRun's own answer for a row it cannot trust.
			return errors.New(productionImportRunReceiptsServe2Identity)
		}
		handleObject := syntheticMap(handle)
		if handleObject == nil {
			return errors.New(productionImportRunReceiptsServe2Identity)
		}
		// :158-161 the run must be this project's and the handle must be the run's
		// own {projectId, epoch, rootActionId, runId}. The project compared against
		// is the FROZEN session's, which stands in for the owner's deps.projectId.
		if binding.ProjectID != projectID ||
			binding.ProjectID != syntheticString(handleObject["projectId"]) ||
			binding.Epoch != syntheticString(handleObject["epoch"]) ||
			run.RootActionID != syntheticString(handleObject["rootActionId"]) ||
			run.RunID != syntheticString(handleObject["runId"]) {
			return errors.New(productionImportRunReceiptsServe2Identity)
		}
		// :1462 the frozen import slot must name exactly the requested one.
		frozen := binding.SourceManifest.ImportSlot
		if frozen.RunID != slot.RunID || frozen.Stage != slot.Stage || frozen.BatchID != slot.BatchID {
			return errors.New(productionImportRunReceiptsServe2SlotChanged)
		}
		// :1463-1465 the frozen binding must still reproduce from the CURRENT
		// sources under the run's own scope, and match the frozen fingerprint.
		if err := r.reproduce(ctx, caller, binding); err != nil {
			return err
		}
		// :1465-1471 the root must not be cancelled, the run must have an attempt,
		// and every attempt must be a stopped one with a verifiable artifact.
		return productionImportRunReceiptsServe2AssertAttempts(run)
	}
}

// reproduce is the reference's rebuildBinding comparison
// (:1463-1465, compareGenerationSourceBindings at
// electron/services/generation-source-binding.ts:622-625) as far as this lane can
// prove it: a fresh fingerprint from the CURRENT sources and the CURRENT model
// revision under the run's own scope, compared with the manifest's frozen
// goOwnerSourceFingerprint.
//
// The error it returns is the registry's own symbolic code (a lease that is gone,
// expired or held for another model), and the mismatch is the reference's
// SOURCE_CHANGED. A reproduction error is NOT flattened into SOURCE_CHANGED: the
// two answers mean different things to the author, and the store lets both cross
// to the renderer unchanged.
func (r *productionImportRunReceiptsServe2) reproduce(ctx context.Context, caller transport.Caller,
	binding productionImportRunReceiptsServe2Binding) error {
	leaseID := binding.SourceManifest.ModelLeaseID
	fingerprint := binding.SourceManifest.GoOwnerSourceFingerprint
	if leaseID == "" || fingerprint == "" {
		// A manifest without the two members the reproduction needs cannot reproduce,
		// which is the reference's own SOURCE_CHANGED, not a proof that passes.
		return errors.New(productionImportRunReceiptsServe2SourceChanged)
	}
	selection, err := productionImportRunReceiptsServe2Selection(binding)
	if err != nil {
		return err
	}
	// The scope is the run's OWN (binding project/epoch) and not the live session's:
	// the receipt's execution lease already proved the live lease, and the epoch
	// comparison requireRun performs is gated on execution
	// (electron/services/main-generation-owner.ts:164), which the assert never
	// passes.
	scope := generationowner.Scope{
		Caller:  caller.SessionID,
		Session: caller.SessionID,
		Tab:     caller.ClientID,
		Project: binding.ProjectID,
		Epoch:   binding.Epoch,
	}
	reproduced, err := r.owner.inner.registry.ReproduceBinding(ctx, scope, leaseID,
		productionImportRunReceiptsServe2Sources(), selection)
	if err != nil {
		return err
	}
	if reproduced.Fingerprint() != fingerprint {
		return errors.New(productionImportRunReceiptsServe2SourceChanged)
	}
	return nil
}

// productionImportRunReceiptsServe2Sources is the ONLY reader set this proof may
// build: no reader touches the author database, because the proof runs inside the
// receipt's BEGIN IMMEDIATE and that transaction holds the author database's only
// connection. An import stage freezes prompt refs only, so the prompt reader -
// pure, and byte-identical to the one the begin used
// (syntheticBuildSources in synthetic_generation_linux.go) - is the whole set the
// reproduction needs. Any other kind is unsupported here rather than read.
func productionImportRunReceiptsServe2Sources() generationowner.Sources {
	return generationowner.Sources{
		Prompt: func(_ context.Context, _ generationowner.Scope, key generationowner.SourceKey) (generationowner.SourceValue, error) {
			parts := strings.SplitN(key.ID, ":", 2)
			if key.Kind != "prompt" || len(parts) != 2 || !syntheticPromptSourceKey(parts[0]) ||
				(parts[1] != "zh-CN" && parts[1] != "en-US") {
				return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNSUPPORTED")
			}
			text, err := syntheticCanonical(map[string]any{
				"key":             key.ID,
				"writingLanguage": parts[1],
				"content":         "synthetic:" + key.ID,
			})
			if err != nil {
				return generationowner.SourceValue{}, errors.New("GENERATION_SOURCE_UNAVAILABLE")
			}
			return generationowner.SourceValue{Revision: 1, Text: text}, nil
		},
	}
}

// productionImportRunReceiptsServe2Selection is the frozen SELECTION the begin
// used, re-derived from the manifest's own promptKeys and the language the frozen
// refs name. It is not taken from the caller: the selection a binding is
// re-fingerprinted over has to be the one it was built from.
func productionImportRunReceiptsServe2Selection(binding productionImportRunReceiptsServe2Binding) ([]generationowner.SourceKey, error) {
	invalid := errors.New(productionImportRunReceiptsServe2SourceChanged)
	language := ""
	for _, ref := range binding.SourceRefs {
		kind, id, cut := strings.Cut(ref.SourceID, ":")
		if !cut || kind != "prompt" {
			// A frozen non-prompt source is outside the one reader this proof has.
			return nil, invalid
		}
		parts := strings.SplitN(id, ":", 2)
		if len(parts) != 2 || (parts[1] != "zh-CN" && parts[1] != "en-US") {
			return nil, invalid
		}
		if language == "" {
			language = parts[1]
			continue
		}
		if language != parts[1] {
			return nil, invalid
		}
	}
	if len(binding.SourceManifest.PromptKeys) == 0 {
		return nil, invalid
	}
	selection := make([]generationowner.SourceKey, 0, len(binding.SourceManifest.PromptKeys))
	for _, key := range binding.SourceManifest.PromptKeys {
		id := key
		if !strings.Contains(id, ":") {
			if language == "" {
				return nil, invalid
			}
			id = key + ":" + language
		}
		selection = append(selection, generationowner.SourceKey{Kind: "prompt", ID: id})
	}
	return selection, nil
}

// productionImportRunReceiptsServe2AssertAttempts is the reference's root and
// attempt half (:1465-1471) over the evidence the store read on its own
// connection. Every branch is a refusal, and the one acceptance requires a
// non-empty stopped attempt set whose artifacts re-derive.
func productionImportRunReceiptsServe2AssertAttempts(run store.ImportRunGenerationEvidence) error {
	sourceChanged := errors.New(productionImportRunReceiptsServe2SourceChanged)
	if !run.RootPresent {
		return errors.New(productionImportRunReceiptsServe2RootMissing)
	}
	if run.RootStatus == "cancelled" {
		return sourceChanged
	}
	if len(run.Attempts) == 0 {
		return sourceChanged
	}
	for _, attempt := range run.Attempts {
		if !attempt.ArtifactPresent || attempt.ArtifactStatus == "discarded" {
			return sourceChanged
		}
		// :1468 receipt.result?.finishReason !== 'stop'
		var usage struct {
			Result struct {
				FinishReason string `json:"finishReason"`
			} `json:"result"`
		}
		if len(attempt.UsageReceiptJSON) == 0 || json.Unmarshal(attempt.UsageReceiptJSON, &usage) != nil ||
			usage.Result.FinishReason != "stop" {
			return sourceChanged
		}
		// :1468 !artifact || !artifact.text.trim() || textHash(artifact.text) !== artifact.textHash
		var artifact struct {
			Text     string `json:"text"`
			TextHash string `json:"textHash"`
		}
		if json.Unmarshal(attempt.ArtifactJSON, &artifact) != nil {
			return sourceChanged
		}
		if strings.TrimSpace(artifact.Text) == "" {
			return sourceChanged
		}
		if artifact.TextHash == "" || syntheticHash(artifact.Text) != artifact.TextHash {
			return sourceChanged
		}
	}
	return nil
}
