//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the answer for the three names a browser asks for after a crash or
// a reload, and the answer is not the same for all three:
//
//	generation:list-recovery             REFUSED  planner recovery context has no Go builder
//	generation:find-blueprint-recovery   REFUSED  the same context projection is the gap
//	generation:list-directory-progress   SERVED   by production_batch_reads_linux.go, not here
//
// # generation:list-directory-progress: the port exists, and this file must not bind it
//
// The reference body is listDirectoryProgress
// (electron/services/main-generation-owner.ts:1550-1553) over
// repository.listDirectoryProgress (electron/repositories/generation-run-repository.ts:599),
// answering DirectoryGenerationCatalog {progress, diagnostics}
// (src/shared/generation-owner-contract.ts:317). That projection exists in Go:
// store.ListDirectoryGenerationProgress (internal/store/batch_progress.go:280)
// walks the same run set (directory runs, retirement markers, author-edit effects
// and runs carrying a directory receipt), derives the same per-receipt proof
// through bpRecovery (:961), and projects the source run's frozen authorInputs the
// way the owner adds them on the way out (:1125-1127). It reads inside one deferred
// transaction and writes nothing.
//
// It is also already installed: the batch/directory slice names it at
// production_batch_reads_linux.go:99, calls it at :203, and that slice is wired at
// production_open_linux.go:701. Binding it here as well would not serve it twice,
// it would fail startup: server.Bind refuses a name it already holds with
// OPERATION_ALREADY_BOUND (internal/transport/server.go:76). So this file
// records the name as served, with the evidence a test re-reads from source
// (productionGenerationRecoveryServedEvidence), and its binder verifies that
// evidence instead of installing a handler that cannot be installed.
//
// Nothing the reference does for this name is out of reach here: the catalog shape,
// the run set, the per-receipt proof (payloadHash against the blueprint commit
// operation, both ranges, the derived remainingRange, the child's epoch) and the
// projected authorInputs are all reproduced, and the only thing this file cannot do
// is install the handler - because it is installed already.
//
// # Refused: generation:list-recovery, because three of its five buckets have no port
//
// The reference (main-generation-owner.ts:1013-1060) builds a
// GenerationRecoveryList: {planning, editors, reviews, batches, drafts, partial}
// (src/shared/generation-owner-contract.ts:254-262). Four of the five buckets are
// per-run recovery reads - planning and drafts through readContext (:912), reviews
// and editors through their own family reads - so a list built here would be a list
// of omissions:
//
//  1. planning: readContext for a planning run needs blueprintRecovery and
//     plotOutlineRecovery, and the writeState inside them needs planningContinuation
//     (:767). None of the three has a Go builder. The run rows themselves CAN exist
//     in an author database written by the desktop app - bpRecovery
//     (internal/store/batch_progress.go:961) reads them for the directory catalog -
//     so the gap is the projection, not the population: bpRecoveryResult
//     (:947-951) carries kind, progress and diagnostic and drops the authorEdit and
//     predecessor that a BlueprintRecovery needs, readBlueprintAuthorEdit has no Go
//     reader at all (generation-run-repository.ts:194), and
//     readPlanningContinuation (:179) has none either.
//  2. drafts: the bucket is readContext over chapter-draft runs plus
//     draftEffects.readRecovery (:1028-1035), and it explicitly lists the
//     short-outline retry state (:1037). This lane's own generation:read-context
//     refuses exactly that state - GENERATION_CONTEXT_UNSUPPORTED for a run whose
//     material decision carries shortOutlinePromptHash or whose attempts carry the
//     reconcile purpose (production_draft_context_linux.go:719-732).
//  3. reviews: the bucket needs review-revision:read-recovery, which this lane does
//     not bind (production_review_revision_linux.go:32).
//  4. editors: this one has a port - editor-inline:read-recovery is served
//     (production_editor_inline_linux.go:168) - which is why the refusal is about
//     the other buckets and not this one.
//
// A list that silently dropped a resumable blueprint run, or the retry state an
// author needs after a length stop, would be read as "there is nothing to resume",
// which is worse than a 501. So the name stays unbound and fails closed at
// GO_OPERATION_NOT_MIGRATED (internal/transport/server.go:116).
//
// # Refused: generation:find-blueprint-recovery, one field short of a port
//
// The reference walks the newest chapter-blueprint-directory runs and returns the
// first GenerationRecoveryContext whose blueprintRecovery.writeState is not
// complete and whose planningContinuation is not complete
// (main-generation-owner.ts:1065-1072). The record it returns is a full
// GenerationRecoveryContext, not the catalog: modelId, handle, operation,
// authorInputs, selectedDraftIds, composition, attemptedPurposes, draftSave and the
// blueprintRecovery union {sourceHandle, leaseEpoch, draft, editRange, writeState,
// saved} (src/shared/generation-owner-contract.ts:139-169 and :185-199).
//
// That union is the missing symbol. Everything the catalog needs lives in
// bpRecovery's Go port; what it does not carry is precisely writeState:
// state is derived from planningContinuation (:767), which reads
// readPlanningContinuation (generation-run-repository.ts:179),
// readBlueprintAuthorEdit (:194), the committed blueprint rows against
// blueprints, and a source-binding rebuild (deps.rebuildBinding). None of those
// four has a Go reader, and the retry-window question - can a planner run still be
// opened here at all - is answered no by the ledger's own gate
// (internal/store/generation_ledger.go:694, GENERATION_OPERATION_UNSUPPORTED for
// every glPlanning operation at :492-494), so a writeState derived in this lane
// would describe rows this lane can read but can never have written. Serving a
// null instead would tell the blueprint page there is nothing to recover.
//
// # Refused: generation:retry-draft-short-outline, for the narrower reason
//
// The only implementation in this module authors its own task:
// syntheticGeneration.RetryDraftShortOutline builds one hardcoded message
// (synthetic_generation_linux.go:1450), so lending it would make this lane the
// author of the text the provider receives. The reference authors nothing - it
// re-dispatches the task the failed attempt froze, read back from the attempt's
// usage receipt (main-generation-owner.ts:189) and proved against the material
// decision's short-outline hash and the stored request hash (:190-196). Faithful
// porting would not be prompt authoring, but this lane's dispatch never records a
// replayTask: OrdinaryService.Execute hands the generation service a request with
// no ReplayTask field (internal/generationowner/ordinary.go:149), so every attempt
// written here has nothing for a faithful retry to re-dispatch. The missing port is
// the frozen task record on the dispatch path; until it exists the only available
// retry is the authored one, and that one this lane refuses
// (production_generation_write_linux.go:92).
//
// Nothing here writes, so a refused call leaves the author database byte-identical;
// the tests prove that against a live lease holding a genuinely interrupted run, a
// completed one and a real failed-attempt id, so the refusal cannot be explained
// away as "there was nothing to answer".

const (
	productionGenerationRecoveryList      = "generation:list-recovery"
	productionGenerationRecoveryFind      = "generation:find-blueprint-recovery"
	productionGenerationRecoveryDirectory = "generation:list-directory-progress"
	productionGenerationRecoveryRetry     = "generation:retry-draft-short-outline"
)

// productionGenerationRecoveryLine is one declaration that has to exist for a
// claim in this file to stay true: the path of the file, the line inside it, and a
// marker that must appear in that file. The tests re-read every one of them, so a
// moved or deleted declaration fails this lane's test instead of leaving a note
// that points at nothing.
type productionGenerationRecoveryLine struct {
	Path   string
	Line   int
	Marker string
}

// productionGenerationRecoverySeam is one refused name and the missing piece that
// keeps it unbound.
type productionGenerationRecoverySeam struct {
	Operation string
	Missing   []productionGenerationRecoveryLine
}

// productionGenerationRecoveryServed is one name this file does not bind because a
// sibling slice already serves it, plus the declarations that make that true.
type productionGenerationRecoveryServed struct {
	Operation string
	Evidence  []productionGenerationRecoveryLine
}

// productionGenerationRecoveryServedOperations is the one name of the three that
// has a port in this lane, spelled once so the binder and the test cannot drift.
func productionGenerationRecoveryServedOperations() []string {
	return []string{productionGenerationRecoveryDirectory}
}

// productionGenerationRecoveryRefusedOperations is the two names that stay 501,
// plus the retry this file also refuses, spelled once so the binder, the seam table
// and the test cannot drift from the controller that registers them
// (electron/controllers/generation-controller.ts:264, :279-281).
func productionGenerationRecoveryRefusedOperations() []string {
	return []string{
		productionGenerationRecoveryList,
		productionGenerationRecoveryFind,
		productionGenerationRecoveryRetry,
	}
}

// productionGenerationRecoveryServedEvidence is the served name's proof. It is not
// an argument: each row names a declaration in another file that the test re-reads
// from disk, so "already served" cannot quietly become "served by nobody" - which
// is exactly what would happen if the batch/directory slice lost this name.
func productionGenerationRecoveryServedEvidence() []productionGenerationRecoveryServed {
	return []productionGenerationRecoveryServed{
		{
			Operation: productionGenerationRecoveryDirectory,
			Evidence: []productionGenerationRecoveryLine{
				// The sibling slice names it, calls the store port for it, and is
				// wired into the finished lane.
				{Path: "production_batch_reads_linux.go", Line: 99, Marker: `productionBatchDirectoryName = "generation:list-directory-progress"`},
				{Path: "production_batch_reads_linux.go", Line: 203, Marker: "store.ListDirectoryGenerationProgress(ctx, db)"},
				{Path: "production_open_linux.go", Line: 701, Marker: "newProductionBatchReads(open.durable).Bind(server)"},
				// The port: the catalog derivation itself.
				{Path: "../../internal/store/batch_progress.go", Line: 280,
					Marker: "func ListDirectoryGenerationProgress(ctx context.Context, db *sql.DB) (*DirectoryGenerationCatalog, error)"},
				// The inventory entry that makes the binding legal at all.
				{Path: "../../../web/operations.json", Line: 224, Marker: `"generation:list-directory-progress": "project"`},
			},
		},
	}
}

// productionGenerationRecoverySeams is the whole refusal answer of this file: for
// each refused name, the exact code that has to move first. The markers are quotes
// from the files, so the test proves the gap is still there and still described the
// way this table describes it.
func productionGenerationRecoverySeams() []productionGenerationRecoverySeam {
	return []productionGenerationRecoverySeam{
		{
			Operation: productionGenerationRecoveryList,
			Missing: []productionGenerationRecoveryLine{
				// The reference body this name is: a per-run readContext over the
				// runs the owner knows, bucketed by operation kind.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1013, Marker: "const listRecovery = (): GenerationRecoveryList"},
				// Its first bucket is the planner projection this lane cannot build.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1021, Marker: "planning: (handle: MainGenerationRunHandle)"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 912, Marker: "const readContext = (handle: MainGenerationRunHandle): GenerationRecoveryContext"},
				// The writeState inside that projection needs a continuation reader
				// this lane has no port of.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 767, Marker: "const planningContinuation = (run: DurableGenerationRun): PlanningContinuation | undefined"},
				// The drafts bucket lists the short-outline retry state, which this
				// lane's read-context refuses.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1037, Marker: "recovery.draftShortOutline?.retry?.kind === 'available'"},
				{Path: "production_draft_context_linux.go", Line: 720, Marker: "!= productionDraftContextOperation"},
				// The reviews bucket's only port is unbound in this lane.
				{Path: "production_review_revision_linux.go", Line: 32, Marker: "review-revision:read-recovery    NOT bound (see below)"},
			},
		},
		{
			Operation: productionGenerationRecoveryFind,
			Missing: []productionGenerationRecoveryLine{
				// The reference walks the newest chapter-blueprint-directory runs and
				// returns the first one whose blueprint draft is not finished.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1065, Marker: "const findBlueprintRecovery = (): GenerationRecoveryContext | null"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1070, Marker: "recovery.blueprintRecovery.writeState !== 'complete'"},
				// writeState is derived from the continuation receipt, which has no
				// Go reader at all.
				{Path: "../../../electron/repositories/generation-run-repository.ts", Line: 179, Marker: "readPlanningContinuation(runId: string): PlanningContinuationReceipt | null"},
				{Path: "../../../electron/repositories/generation-run-repository.ts", Line: 194, Marker: "readBlueprintAuthorEdit(runId: string): BlueprintAuthorEditReceipt | null"},
				// The Go blueprint reader stops at kind/progress/diagnostic, one
				// field short of the union the wire type wants.
				{Path: "../../internal/store/batch_progress.go", Line: 947, Marker: "type bpRecoveryResult struct {"},
				{Path: "../../internal/store/batch_progress.go", Line: 1025, Marker: `return bpRecoveryResult{kind: "available", progress: progress}, nil`},
				// And the ledger refuses to open the operation this name reads, so no
				// such run can be created here either.
				{Path: "../../internal/store/generation_ledger.go", Line: 694, Marker: `if glPlanning(glText(request["operation"])) {`},
			},
		},
		{
			Operation: productionGenerationRecoveryRetry,
			Missing: []productionGenerationRecoveryLine{
				// The only retry in this module authors its own message.
				{Path: "synthetic_generation_linux.go", Line: 1450, Marker: "retry draft short outline after length stop"},
				// The reference re-dispatches the task the failed attempt froze.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 189, Marker: "const task = usage.replayTask"},
				// Which this lane's dispatch never records.
				{Path: "../../internal/generationowner/ordinary.go", Line: 149, Marker: "RunID: runID, InvocationNonce: nonce, RequestHash: hash,"},
				// The refusal this file keeps, for the narrower reason above.
				{Path: "production_generation_write_linux.go", Line: 92, Marker: "It is not a ledger transition, it is a provider call"},
			},
		},
	}
}

// productionGenerationRecoverySeamFor looks one name up, so a typo in the binder
// refuses at startup instead of being silently served or silently forgotten.
func productionGenerationRecoverySeamFor(operation string) (productionGenerationRecoverySeam, bool) {
	for _, seam := range productionGenerationRecoverySeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionGenerationRecoverySeam{}, false
}

// productionGenerationRecoveryServedEvidenceFor does the same for the served name.
func productionGenerationRecoveryServedEvidenceFor(operation string) (productionGenerationRecoveryServed, bool) {
	for _, served := range productionGenerationRecoveryServedEvidence() {
		if served.Operation == operation {
			return served, true
		}
	}
	return productionGenerationRecoveryServed{}, false
}

// productionGenerationRecoveryServedBy reports whether the finished lane really
// installs the one name this file records as served elsewhere.
func productionGenerationRecoveryServedBy(bound []string) bool {
	for _, operation := range productionGenerationRecoveryServedOperations() {
		found := false
		for _, name := range bound {
			if name == operation {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// bindProductionGenerationRecovery installs nothing, so wiring it moves no count:
// it verifies this file's claims against the lane that is actually being built
// instead. It has one ordering requirement - it must run after the batch/directory
// slice (production_open_linux.go:701), because that slice is what installs the
// name this file records as served - and it says so with a symbolic code rather
// than serving a second handler that Bind would reject anyway
// (OPERATION_ALREADY_BOUND).
func bindProductionGenerationRecovery(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, operation := range productionGenerationRecoveryServedOperations() {
		evidence, found := productionGenerationRecoveryServedEvidenceFor(operation)
		if !found || len(evidence.Evidence) == 0 {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_EVIDENCE_INCOMPLETE")
		}
	}
	if !productionGenerationRecoveryServedBy(server.BoundOperations()) {
		return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVED_MISSING")
	}
	for _, operation := range productionGenerationRecoveryRefusedOperations() {
		seam, found := productionGenerationRecoverySeamFor(operation)
		if !found || len(seam.Missing) == 0 {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SEAM_INCOMPLETE")
		}
	}
	// Nothing is bound: the three refused names keep failing closed at
	// GO_OPERATION_NOT_MIGRATED, and the served one keeps the handler the
	// batch/directory slice already installed.
	return nil
}
