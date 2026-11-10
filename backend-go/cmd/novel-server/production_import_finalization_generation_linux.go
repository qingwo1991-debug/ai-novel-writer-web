//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file used to record the lane's fail-closed 501 answer for four
// model-executing generation names. All four are served now, and what is left
// here is the record of that change plus the binder that refuses a lane whose
// record disagrees with it:
//
//	finalization-generation:begin/execute/commit  production_finalization_generation_serve_linux.go
//	import-generation:read/execute                production_import_generation_serve_linux.go
//
// The import half is the newest: the reference opens an import run through
// generation:begin with an importSlot selection and has no begin of its own
// (electron/services/import-generation.ts:37, src/shared/import-generation.ts:28-29,
// src/shared/generation-owner-contract.ts:266-268), so serving its read and
// execute meant admitting that selection, freezing its three manifest members and
// widening the ledger's family allowlist - all of which landed beside this note
// rather than in it. The seam table below stays as the place a future refusal
// has to declare itself; it is empty today, and both binders below assert that.
//
// Nothing is bound, imported or written by this file.
//
// # The one legal model call, and where the prompt text comes from
//
// This lane cannot author a prompt and does not. The only model call it can
// legally make is the one behind a run it admitted itself: generation:begin
// freezes the source binding, syntheticGeneration.Execute (synthetic_generation_linux.go:1297)
// takes the task out of the browser's own request (request["task"], :1337),
// generationowner.OrdinaryService reserves and marks the attempt, and the one
// executor the lane builds - production_generation_begin_linux.go:188, over the
// committed model-egress policy - is the only client that reaches a provider.
// The other model path, llm:generate over internal/llmlease
// (production_llm_execution_linux.go:202), is a lease the renderer mints for a
// saved profile; it is equally not a place where prompt prose can be written in
// Go.
//
// That is why the task text matters here: for the import family the renderer
// sends the task it rendered from its own committed prompt assets
// (src/shared/import-generation.ts:29, the same GenerationTask that
// generation:execute already dispatches), and for the finalization family the
// task is frozen at begin beside the context it was rendered from
// (electron/services/generation-source-binding.ts:587-591, read back by
// electron/services/finalization-generation.ts:46-49). In neither case does a Go
// handler have to hold a prompt - and neither family's served half does.
//
// # import-generation:read - served
//
// The renderer sends {slot: {runId, stage, batchId}} and reads back an
// ImportGenerationRecovery or JSON null (src/shared/import-generation.ts:28).
// The recovery is a lookup over generation_runs whose frozen manifest carries
// importSlotKey, and it projects the run view, its modelId and the frozen
// ImportGenerationContext (electron/services/import-generation.ts:22 throws
// GENERATION_IMPORT_CONTEXT_REQUIRED without it).
//
// The three pieces this file said were missing now exist:
//
//   - The reduced admission carries the import selection: syntheticSelection has
//     importSlot/importExecution, its key allowlist accepts them and
//     productionImportGenerationSelect admits them only for the stage operation
//     that owns the slot.
//   - The ledger admits the three manifest members for exactly those two
//     operations (glFamilyManifestAllowed, internal/store/generation_ledger.go),
//     and keeps refusing the same keys for every other operation.
//   - The context has a Go producer: productionImportGenerationFreeze reads the
//     import_runs row and the import_run_chapters projection and verifies each
//     chapter against its stored fingerprint, which is what the read hands back.
//
// # import-generation:execute - served
//
// The renderer sends {handle, ordinal, task, execution?} and reads back a
// MainGenerationExecuteReceipt (src/shared/import-generation.ts:29), and the
// dispatch is generation:execute's own path over the run's frozen lease. The
// reference's guards are kept: the nonce is import:<ordinal>, a gap is
// GENERATION_IMPORT_ORDINAL_GAP, a prior attempt that did not stop is
// GENERATION_IMPORT_PREVIOUS_ATTEMPT_INCOMPLETE, a sealed stage is
// GENERATION_IMPORT_EFFECT_SEALED and an execution authority the import run no
// longer holds is IMPORT_RUN_EXECUTION_STALE (electron/services/main-generation-owner.ts:1473-1496,
// electron/services/import-generation.ts:25-38).
//
// One stage stays refused, and deliberately: import-blueprints contains
// "blueprint", so both the admission (syntheticPlanning,
// synthetic_generation_linux.go:90, applied at :431) and the ledger (glPlanning,
// internal/store/generation_ledger.go:492, applied at :518) refuse it as
// GENERATION_OPERATION_UNSUPPORTED. Neither planning gate is widened here.
//
// The import run's own stage transitions are served as their own names
// (db:import-run-advance-stage, production_importrun_write_linux.go:168;
// db:import-run-effect-receipt-prepare and -commit,
// production_importrun_residual_linux.go:163-164), and the generation run that
// consumes them is now the one generation:begin can open.
//
// # finalization-generation:execute - refused, and the previous reason was wrong
//
// The renderer sends {handle, retryOf?, recoverFrom?} (src/shared/finalization-generation.ts:42).
// The earlier note in production_finalization_generation_linux.go said the
// contract carries no task, so a handler would have to author one. That is not
// what happens: main freezes the task at begin (generation-source-binding.ts:590-591)
// and execute only re-proves its hash (electron/services/finalization-generation.ts:46-49).
// Reading a frozen task back would not make this lane a prompt author, so the
// task is not the seam. What is actually missing is everything around it:
//
//   - No row can become this session's run. begin is refused by this lane
//     (production_finalization_generation_linux.go:164), and the served read
//     only projects a row whose committed slot reproduces the requested key
//     (synthetic_graph_linux.go:199) - it refuses a row whose committed slot
//     cannot be proved with GENERATION_FINALIZATION_CONTEXT_INVALID
//     (synthetic_graph_linux.go:211).
//   - The freeze has no Go producer. finalizationGenerationTask
//     (electron/services/finalization-generation-source.ts:47) renders a
//     PromptTemplate through renderPrompt, and no Go file in this module names
//     either; the task and context keys exist here only as allowlist entries
//     (internal/store/generation_ledger.go:437).
//   - The dispatch is not one call. The reference runs up to three ordinals on
//     an unparseable character-cards answer, and each repair task is authored
//     from literal bilingual prose in main
//     (electron/services/main-generation-owner.ts:1285-1303). Porting that
//     would be authoring prompts in Go, which this lane forbids outright - which
//     is also why the earlier note was right about the outcome and wrong about
//     the reason.
//
// # finalization-generation:commit - refused
//
// The renderer sends {handle, artifact} and reads back a
// FinalizationGenerationEffect (src/shared/finalization-generation.ts:43). The
// effect stamp itself is not the gap: the ledger can patch
// generation_attempts.usage_receipt_json with finalizationEffect
// (internal/store/generation_ledger.go:471, recordEffect at :1211). The gap is
// the domain write the stamp must be atomic with, and the receipt it must carry:
//
//	db:continuity-save-finalized                    internal/transport/summary.go:45
//	db:continuity-save-character-state-candidates   internal/transport/summary.go:45
//	db:blueprint-update-notes                       internal/transport/blueprints.go:26
//
// all three inventoried denials answering GENERATION_FINALIZATION_ADMISSION_REQUIRED,
// with no store behind them that could take the write: internal/store/summary.go:446
// reads a finalized source but has no finalized-continuity writer, and the
// blueprints have no notes updater at all (production_blueprint_write_linux.go:40).
// The receipt is not a projection either: chapter_notes needs
// buildFinalizedContinuityFacts and character_cards needs the finalized proof,
// neither of which exists in Go. Inventing that derivation is how a resume path
// corrupts a book, so it is not invented here.
//
// What is left is this: the exact seam, name by name, so the next change starts
// from the code that has to move.

const (
	// productionImportGenerationRead and productionImportGenerationExecute are
	// the import family's model-executing half, registered with arity 1
	// (electron/controllers/generation-controller.ts:246-247). The family has no
	// begin of its own: an import run is opened by generation:begin with an
	// importSlot selection, which is the admission
	// production_import_generation_serve_linux.go now freezes.
	productionImportGenerationRead    = "import-generation:read"
	productionImportGenerationExecute = "import-generation:execute"
	// productionModelExecutingRefusalCode is the one answer the transport gives
	// an inventoried name nobody bound (internal/transport/server.go:116). It is
	// what the two names answered while this file refused them, and the binder
	// test still pins it so a future refusal cannot quietly change the code.
	productionModelExecutingRefusalCode = "GO_OPERATION_NOT_MIGRATED"
)

// productionModelExecutingLine is one declaration that has to move before a name
// can be served: the Go file, the line inside it, and a marker that must appear
// somewhere on that file. The table it belongs to is empty now; a future refusal
// re-populates it, and TestProductionImportGenerationBinderRecord re-reads the
// table rather than a hand-kept list.
type productionModelExecutingLine struct {
	Path   string
	Line   int
	Marker string
}

// productionModelExecutingName is one refused name with the symbolic answer it
// keeps and the code that has to move first.
type productionModelExecutingName struct {
	Operation string
	Code      string
	Missing   []productionModelExecutingLine
}

// productionImportGenerationOperations is the import family's model-executing
// half, in the order the desktop controller registers it
// (electron/controllers/generation-controller.ts:246-247).
func productionImportGenerationOperations() []string {
	return []string{productionImportGenerationRead, productionImportGenerationExecute}
}

// productionImportGenerationGaps is the whole answer of this file's import half.
// It is empty now: both names are served by
// production_import_generation_serve_linux.go, which binds them beside the same
// generation owner, so there is no declaration left that has to move first. The
// table is kept (and asserted empty by the binder) so a name that slipped back to
// refused without its binder would fail this lane's own test instead of
// disappearing from the record.
func productionImportGenerationGaps() []productionModelExecutingName {
	return nil
}

// productionImportGenerationGap returns the documented missing piece for one
// import name, or false when the name is not one of the two.
func productionImportGenerationGap(operation string) (productionModelExecutingName, bool) {
	for _, gap := range productionImportGenerationGaps() {
		if gap.Operation == operation {
			return gap, true
		}
	}
	return productionModelExecutingName{}, false
}

// productionModelExecutingOperations and productionModelExecutingRefusal are gone
// with the refusal they served: both import names are installed by
// production_import_generation_serve_linux.go, and productionImportGenerationGaps
// is the one table that still has to stay empty for this file to be honest.

// bindProductionImportGeneration is the import family's entry point beside the
// finalization binder. It installs nothing - the two names are installed by
// production_import_generation_serve_linux.go's own binder, which needs the
// generation owner this server-only binder does not hold - and it refuses to
// start a lane whose record disagrees with that: the seam table must be empty and
// every name it used to refuse must be absent from it.
//
// The nil-server refusal is the same one every other binder in this package
// answers, so wiring it into production_open_linux.go cannot mask a construction
// bug. It is reached from bindProductionFinalizationGeneration, the binder the
// lane already calls (production_open_linux.go:702).
func bindProductionImportGeneration(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if gaps := productionImportGenerationGaps(); len(gaps) != 0 {
		return errors.New("PRODUCTION_IMPORT_GENERATION_SEAM_STALE")
	}
	for _, operation := range productionImportGenerationOperations() {
		if _, refused := productionImportGenerationGap(operation); refused {
			return errors.New("PRODUCTION_IMPORT_GENERATION_SEAM_INCOMPLETE")
		}
	}
	return nil
}
