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
//     (:767), whose `current` step is a rebuildBinding comparison this lane cannot
//     make. The run rows themselves CAN exist in an author database written by the
//     desktop app - bpRecovery (internal/store/batch_progress.go:961) reads them
//     for the directory catalog - so the gap is the projection, not the population:
//     bpRecoveryResult (:947-951) carries kind, progress and diagnostic and drops
//     the authorEdit and predecessor that a BlueprintRecovery needs, and
//     readPlanningContinuation (:179) has no Go reader at all. The author-edit
//     receipt (:194) does have one, and it refuses the reference's own shape:
//     bpAuthorEditCode (internal/store/batch_progress.go:1194) parses the
//     receipt's committedRange with the startChapter/endChapter accessor (:1235)
//     while the desktop writes {from,to} (:205-206), so a real author-saved run is
//     diagnosed instead of listed. The piece table in
//     production_generation_recovery_serve_linux.go re-tested every one of them
//     and records that reader as REFUTED.
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
// readPlanningContinuation (generation-run-repository.ts:179), the committed
// blueprint rows against blueprints, and a source-binding rebuild
// (deps.rebuildBinding). Two of those have a Go answer and neither one closes the
// gap: the blueprint rows are store.Blueprints.All
// (internal/store/blueprints.go:69), and the author-edit receipt has a reader that
// refuses the desktop's shape (bpAuthorEditCode, :1235). The rebuild is only
// substituted by generationowner.ReproduceBinding - a single digest over the Go
// owner's own source selection, not the nine-field fingerprint the comparison
// needs - and readPlanningContinuation still has no reader. The retry-window
// question - can a planner run still be opened here at all - is answered no by the
// ledger's own gate (internal/store/generation_ledger.go:719-720,
// GENERATION_OPERATION_UNSUPPORTED for every glPlanning operation, :504), so a
// writeState derived in this lane would describe rows this lane can read but can
// never have written.
//
// One sentence of the paragraph above was this lane's inference rather than the
// reference's behavior, and it is corrected here: "serving a null instead would
// tell the blueprint page there is nothing to recover" is not what a null means.
// The reference answers null itself when no chapter-blueprint-directory run
// qualifies, and it answers an empty bucket as `[]`, never as null and never as a
// thrown call (production_generation_recovery_serve2_linux.go quotes both). What
// keeps this name refused is therefore not the null: it is membership. A run
// belongs to the answer only when its continuation state is not 'complete'
// (main-generation-owner.ts:1070), and 'complete' is decided after a source
// comparison this lane cannot make (:810, :806). Returning null for a project that
// HAS a blueprint run is the part that would mislead, and returning a state derived
// from the substitute would mislead in both directions.
//
// # generation:retry-draft-short-outline: served elsewhere
//
// This name is no longer refused here. The dispatch now records the frozen task
// as the attempt's replayTask (internal/generationowner/ordinary.go), so a
// faithful retry reads that task back, proves it against the material decision's
// short-outline hash and the stored request hash, and re-dispatches it
// (production_retry_draft_outline_linux.go). This file keeps only the two names
// that still have no port: list-recovery and find-blueprint-recovery.
//
// # Re-check against the current code (this pass)
//
// production_generation_recovery_serve_linux.go re-tested every piece named
// above against the code that has landed since, and recorded one row per piece
// with the declarations that decide it. Three things are now known that the
// refusal above did not say:
//
//   - readBlueprintAuthorEdit is not a missing port: it is a refuted one.
//     bpAuthorEditCode (internal/store/batch_progress.go:1194) reads the receipt
//     and refuses every real one, because it parses committedRange with the
//     startChapter/endChapter accessor (:1235) while the desktop writes {from,to}
//     (:205). The seam row now names that line instead of claiming nobody reads
//     the receipt at all.
//   - ReproduceBinding is not the answer to it, and it is not the answer to
//     rebuildBinding either: it asks "do the current sources reproduce THIS
//     process's digest", not "does the frozen nine-field fingerprint still
//     match".
//   - The blueprint rows reader (store.Blueprints.All) and the short-outline
//     retry readback (production_retry_draft_outline_linux.go) move neither
//     name: the planning bucket is the whole GenerationRecoveryContext, and the
//     drafts bucket is readContext's draftShortOutline, not the frozen task.
//
// The editors and batches buckets stay served by sibling names, and the bucket
// table in production_generation_recovery_reads_linux.go still pins each bucket,
// served or refused, so the two names stay 501 at GO_OPERATION_NOT_MIGRATED for a
// reason that is still true. Serving either one needs a binder that holds the
// owner, and this binder is wired with only the server
// (production_open_linux.go:784), outside the block that creates the owner
// (:595-746). It also needs the admitted author database for the batches bucket,
// which the neighbouring closure passes and this one does not (:779). Neither the
// null/empty question nor a reader of the planner receipt changes that: the two
// names would be served by a call-site change in production_open_linux.go, which
// this pass does not make, so the count stays 279
// (production_served_count_linux_test.go:24).

const (
	productionGenerationRecoveryList      = "generation:list-recovery"
	productionGenerationRecoveryFind      = "generation:find-blueprint-recovery"
	productionGenerationRecoveryDirectory = "generation:list-directory-progress"
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
// spelled once so the binder, the seam table and the test cannot drift from the
// controller that registers them (electron/controllers/generation-controller.ts:
// 264, :279-281).
func productionGenerationRecoveryRefusedOperations() []string {
	return []string{
		productionGenerationRecoveryList,
		productionGenerationRecoveryFind,
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
				// And none of the five buckets can be filled from here: the family's
				// binder receives the server alone, so it reaches neither the author
				// database nor the owner. The two buckets that HAVE a port are
				// blocked by this line, not by a missing reader.
				{Path: "production_open_linux.go", Line: 784, Marker: "bindProductionGenerationRecovery(server)"},
			},
		},
		{
			Operation: productionGenerationRecoveryFind,
			Missing: []productionGenerationRecoveryLine{
				// The reference walks the newest chapter-blueprint-directory runs and
				// returns the first one whose blueprint draft is not finished.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1065, Marker: "const findBlueprintRecovery = (): GenerationRecoveryContext | null"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1070, Marker: "recovery.blueprintRecovery.writeState !== 'complete'"},
				// writeState is derived from the continuation receipt, which still
				// has no Go reader.
				{Path: "../../../electron/repositories/generation-run-repository.ts", Line: 179, Marker: "readPlanningContinuation(runId: string): PlanningContinuationReceipt | null"},
				// The Go blueprint reader stops at kind/progress/diagnostic, one
				// field short of the union the wire type wants.
				{Path: "../../internal/store/batch_progress.go", Line: 947, Marker: "type bpRecoveryResult struct {"},
				{Path: "../../internal/store/batch_progress.go", Line: 1025, Marker: `return bpRecoveryResult{kind: "available", progress: progress}, nil`},
				// And the ledger refuses to open the operation this name reads, so no
				// such run can be created here either.
				{Path: "../../internal/store/generation_ledger.go", Line: 719, Marker: `if glPlanning(glText(request["operation"])) {`},
				// The selection itself: a run is returned only when its state is not
				// complete, and complete is the end of the state chain below -
				// decided AFTER the currency comparison, so a null answer cannot be
				// told apart from a projected one without that comparison.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 810, Marker: "if (!remainingRange) return { ...base, state: 'complete' }"},
				// And the same call site as list-recovery's: the binder holds no
				// author database, so it could not even enumerate the runs.
				{Path: "production_open_linux.go", Line: 784, Marker: "bindProductionGenerationRecovery(server)"},
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
	// The bucket table of list-recovery is verified against the same lane: the
	// two served buckets stay served by their sibling names and the two refused
	// names stay unbound (production_generation_recovery_reads_linux.go).
	if err := bindProductionGenerationRecoveryReads(server); err != nil {
		return err
	}
	// The per-name verdict and the piece table that supports it are checked
	// against this same lane, so a name can never be recorded as refused while it
	// is bound - or as served while it is not
	// (production_generation_recovery_serve_linux.go).
	if err := bindProductionGenerationRecoveryServe(server); err != nil {
		return err
	}
	// The answer to the null/empty question, and the call site that keeps it from
	// becoming a handler, are recorded beside this binder so the two cannot drift
	// (production_generation_recovery_serve2_linux.go).
	if err := bindProductionGenerationRecoveryServe2(server); err != nil {
		return err
	}
	// Nothing is bound: the two refused names keep failing closed at
	// GO_OPERATION_NOT_MIGRATED, and the served one keeps the handler the
	// batch/directory slice already installed.
	return nil
}
