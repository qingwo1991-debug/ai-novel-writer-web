//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the lane's answer for the four model-executing generation names
// the browser still needs for the two author journeys it cannot finish - the
// 40-chapter import and the daily-finalization step - and the answer is the
// fail-closed 501 for all four: GO_OPERATION_NOT_MIGRATED
// (internal/transport/server.go:116). The members of those two families this
// lane does serve are bound elsewhere and are not touched here:
//
//	finalization-generation:read     production_graph_reads_linux.go:70
//	finalization-generation:cancel   production_generation_write_linux.go:153
//
// Nothing is bound, imported or written by this file. Each refusal is recorded
// as a seam - the declarations that have to move before the name can be served,
// with the file and the line they live on - so the next change starts from the
// code that has to change instead of from a guess, and so a moved declaration
// fails this lane's own test instead of leaving a note that points at nothing.
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
// electron/services/finalization-generation.ts:46-49). In neither case would a
// Go handler have to hold a prompt - and in neither case can it: the freeze
// itself, and the run under it, are the pieces that do not exist here.
//
// # import-generation:read - refused
//
// The renderer sends {slot: {runId, stage, batchId}} and reads back an
// ImportGenerationRecovery or JSON null (src/shared/import-generation.ts:28).
// The recovery is a lookup over generation_runs whose frozen manifest carries
// importSlotKey, and it projects the run view, its modelId and the frozen
// ImportGenerationContext (electron/services/import-generation.ts:22 throws
// GENERATION_IMPORT_CONTEXT_REQUIRED without it).
//
// No row this lane can write carries that key, so a bound read could only ever
// answer null:
//
//   - The reduced admission has no import slot at all: syntheticSelection
//     (synthetic_generation_linux.go:386) has no field for it and its key
//     allowlist (:410) rejects it, so generation:begin refuses an import
//     selection with GENERATION_BEGIN_INVALID before a root exists.
//   - The ledger refuses the manifest key for every operation: glFamilyManifestAllowed
//     (internal/store/generation_ledger.go:426) admits agentSession, the graph
//     keys and the finalization keys and nothing else, and glOrdinaryBinding
//     refuses any other family key as GENERATION_OPERATION_UNSUPPORTED (:518).
//   - The context the read projects has no Go producer: captureImportGenerationContext
//     (electron/services/import-generation-source.ts:20) binds the run's slot to
//     the import_run_chapters projection and the project core, and this module
//     has no counterpart.
//
// A null here is not inert either. The renderer reads null as "nothing to
// resume" and falls through to generation:begin with the same importSlot
// selection (src/services/workflows/workflow-main-generation.ts:125), which this
// lane answers GENERATION_BEGIN_INVALID - a request-shape refusal that reads as
// the author's mistake. And a read that returned null could not carry the begin
// even if begin accepted it: the very next step re-reads the slot and requires
// the begun run back, or refuses the journey itself (:135-136). The honest
// answer to the whole journey stays the 501 that says this server cannot do it
// yet.
//
// # import-generation:execute - refused
//
// The renderer sends {handle, ordinal, task, execution?} and reads back a
// MainGenerationExecuteReceipt (src/shared/import-generation.ts:29). It is the
// only one of the four whose prompt is already the browser's, so the dispatch
// half is portable; the run it dispatches is not.
//
// The reference derives the invocation nonce as import:<ordinal>, refuses a gap
// (GENERATION_IMPORT_ORDINAL_GAP), refuses a run whose prior attempt did not
// stop, refuses a sealed effect and refuses an execution authority the import
// run no longer holds (electron/services/main-generation-owner.ts:1473-1496,
// electron/services/import-generation.ts:25-38). Every one of those guards
// reads a run whose operation is one of three names this lane cannot open:
//
//   - import-blueprints contains "blueprint", so both the admission
//     (syntheticPlanning, synthetic_generation_linux.go:90, applied at :431) and
//     the ledger (glPlanning, internal/store/generation_ledger.go:492, applied at
//     :518) refuse it as GENERATION_OPERATION_UNSUPPORTED.
//   - import-global-facts and analyze-writing-style are refused by the missing
//     importSlot key (synthetic_generation_linux.go:410,
//     internal/store/generation_ledger.go:426), exactly like the read above.
//
// The import run's own stage transitions are not the gap: they are served as
// their own names (db:import-run-advance-stage,
// production_importrun_write_linux.go:168; db:import-run-effect-receipt-prepare
// and -commit, production_importrun_residual_linux.go:163-164). What is missing
// is the generation run that would consume them - and a handler installed over
// a run that cannot exist would answer a business refusal for every request
// instead of the one code that says the server cannot serve this name.
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
//     refuses every row that does carry the slot key with
//     GENERATION_FINALIZATION_CONTEXT_INVALID (synthetic_graph_linux.go:156)
//     because it cannot prove the frozen context.
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
	// importSlot selection, which is the admission this lane does not have.
	productionImportGenerationRead    = "import-generation:read"
	productionImportGenerationExecute = "import-generation:execute"
	// productionModelExecutingRefusalCode is the one answer the transport gives
	// an inventoried name nobody bound (internal/transport/server.go:116). The
	// seam table below is pinned against it rather than against a handler that
	// could only refuse.
	productionModelExecutingRefusalCode = "GO_OPERATION_NOT_MIGRATED"
)

// productionModelExecutingLine is one declaration that has to move before a name
// can be served: the Go file, the line inside it, and a marker that must appear
// somewhere on that file. TestProductionLaneRefusesImportGeneration re-reads
// every one of them, so a deleted seam fails this lane's test instead of leaving
// a note that points at nothing.
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

// productionImportGenerationGaps is the whole answer of this file's import half:
// for each refused name, the exact code that has to move first.
func productionImportGenerationGaps() []productionModelExecutingName {
	return []productionModelExecutingName{
		{
			Operation: productionImportGenerationRead,
			Code:      productionModelExecutingRefusalCode,
			Missing: []productionModelExecutingLine{
				{Path: "synthetic_generation_linux.go", Line: 386, Marker: "type syntheticSelection struct {"},
				{Path: "synthetic_generation_linux.go", Line: 410, Marker: "allowed := map[string]bool{"},
				{Path: "../../internal/store/generation_ledger.go", Line: 426, Marker: "func glFamilyManifestAllowed(key, operation string) bool {"},
				{Path: "../../internal/store/generation_ledger.go", Line: 518, Marker: `if glPlanning(glText(manifest["operation"])) {`},
			},
		},
		{
			Operation: productionImportGenerationExecute,
			Code:      productionModelExecutingRefusalCode,
			Missing: []productionModelExecutingLine{
				{Path: "synthetic_generation_linux.go", Line: 90, Marker: "func syntheticPlanning(operation string) bool {"},
				{Path: "synthetic_generation_linux.go", Line: 431, Marker: "if syntheticPlanning(selection.Operation) {"},
				{Path: "../../internal/store/generation_ledger.go", Line: 492, Marker: "func glPlanning(operation string) bool {"},
				{Path: "synthetic_generation_linux.go", Line: 410, Marker: "allowed := map[string]bool{"},
			},
		},
	}
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

// productionModelExecutingOperations is the two import-family names this change
// answers. The finalization family's three names are served now by
// production_finalization_generation_serve_linux.go, so they are not in this table
// anymore.
func productionModelExecutingOperations() []string {
	return []string{productionImportGenerationRead, productionImportGenerationExecute}
}

// productionModelExecutingGaps is the import family's seam table. The finalization
// family moved to production_finalization_generation_serve_linux.go, which binds its
// three names, so this table no longer lends their rows.
func productionModelExecutingGaps() []productionModelExecutingName {
	return productionImportGenerationGaps()
}

// productionModelExecutingRefusal returns the documented missing piece for one of
// the four names, or false when the name is not one of them.
func productionModelExecutingRefusal(operation string) (productionModelExecutingName, bool) {
	for _, gap := range productionModelExecutingGaps() {
		if gap.Operation == operation {
			return gap, true
		}
	}
	return productionModelExecutingName{}, false
}

// bindProductionImportGeneration installs the import family's model-executing
// half. It installs nothing, and it refuses to start a lane whose refusal is
// undocumented: both names must carry the fail-closed code and at least one
// named missing piece, so the day one of them gains a body the table has to
// change with it instead of silently becoming a lie.
//
// The nil-server refusal is the same one every other binder in this package
// answers, so wiring it into production_open_linux.go cannot mask a construction
// bug. It is reached from bindProductionFinalizationGeneration, the binder the
// lane already calls (production_open_linux.go:702), because the four names are
// one answer and a second unwired binder would be a note nobody runs.
func bindProductionImportGeneration(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, operation := range productionImportGenerationOperations() {
		gap, found := productionImportGenerationGap(operation)
		if !found || len(gap.Missing) == 0 {
			return errors.New("PRODUCTION_IMPORT_GENERATION_SEAM_INCOMPLETE")
		}
		if gap.Code != productionModelExecutingRefusalCode {
			return errors.New("PRODUCTION_IMPORT_GENERATION_REFUSAL_CHANGED")
		}
	}
	// Nothing is bound: both names keep failing closed at
	// GO_OPERATION_NOT_MIGRATED, exactly the answer the desktop-mirroring tests
	// already pin for the synthetic lane
	// (synthetic_generation_http_linux_test.go:475-479).
	return nil
}
