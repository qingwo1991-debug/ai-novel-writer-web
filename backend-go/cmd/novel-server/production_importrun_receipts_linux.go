//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the import-run family's last seam: the three names the family
// still refuses after the ten its binder serves, and the exact declaration that
// keeps each one refused. It publishes no handler. Its binder installs nothing on
// the wire and returns an error only when the seam table itself has become
// incomplete, so the real lane runs the check at startup instead of leaving a
// note that points at nothing.
//
//	db:import-run-effect-receipt-prepare   refused, no live generation-source proof
//	db:import-run-effect-receipt-commit    refused, the same proof
//	db:import-run-prepare-inspection       refused, the inspection CAS never reaches the wire
//
// # What the receipt assert actually proves
//
// store.ImportRuns.PrepareEffectReceipt and CommitEffectReceipt accept a
// store.ImportRunGenerationSourceAssert (internal/store/importrun_receipts.go:118,
// documented at :112-118). The reference always supplies one
// (electron/controllers/db-controller.ts:425 for prepare, :437 for commit). It is
// not an epoch comparison and it is not a re-read of import_runs. In the
// reference it is:
//
//  1. the live generation owner for this project database must exist, the project
//     database must be in a transaction, and the owner must still be installed
//     (electron/controllers/generation-controller.ts:72-75, GENERATION_OWNER_REQUIRED);
//  2. the handle must resolve to a generation_runs row whose projectId matches the
//     owner's project and whose handle is exactly the row's own
//     {projectId, epoch, rootActionId, runId}
//     (electron/services/main-generation-owner.ts:156-159, requireRun);
//  3. the run's frozen import context must name exactly the requested
//     {runId, stage, batchId} slot
//     (electron/services/main-generation-owner.ts:1462,
//     GENERATION_IMPORT_EFFECT_SLOT_CHANGED);
//  4. the run's stored binding must still equal a binding rebuilt from the
//     CURRENT author sources and the CURRENT model receipt
//     (electron/services/main-generation-owner.ts:1463-1465,
//     generation-source-binding.ts:614-625,
//     GENERATION_IMPORT_EFFECT_SOURCE_CHANGED);
//  5. the root must not be cancelled, at least one attempt must exist, every
//     attempt must have finished with stop, the artifact must carry non-blank text
//     whose hash equals the recorded textHash, and the artifact must not be
//     discarded (electron/services/main-generation-owner.ts:1465-1471).
//
// Steps 2, 3 and 5 are database reads this repository can reproduce. Step 1 is
// the in-process owner, and step 4 is the part that cannot be reproduced from a
// row: it re-reads every source material of the run and compares the rebuilt
// binding, and it needs the model profile the owner currently holds. A binder
// that reads generation_runs can answer "there is a row with this handle and this
// import slot"; it cannot answer "the sources this run froze are still the
// author's current sources". That is the whole reason the assert exists, and the
// difference is not a formality - it is the difference between committing an
// effect for the latest generation and committing one for material the author has
// since replaced.
//
// # The store's own live-run read does not supply it
//
// The previous refusal was checked rather than repeated. The store does own
// import_runs, and the receipt transaction already reads the live run:
//
//	internal/store/importrun.go:121   readImportRunRow selects import_runs only;
//	                                  the table has no generation handle column
//	                                  (internal/admission/baseline_v7_ddl.go:376-414).
//	internal/store/importrun.go:641   importRunAssertExecutionAuthority proves the
//	                                  run is running, the exact owner and epoch, and
//	                                  a lease that has not expired. That is the
//	                                  IMPORT_RUN_EXECUTION_STALE gate, not the
//	                                  generation-source gate.
//	internal/store/importrun_lifecycle.go:796, :1048, :1164, :1230, :1489, :1619
//	                                  the parsing and lifecycle writes. Every one
//	                                  reads import_runs, import_run_sources,
//	                                  import_run_chapters or import_effect_ledger;
//	                                  none reads generation_runs, binding_json or
//	                                  an importSlot.
//
// The only reduced source proof already in the store is dcAssertSources
// (internal/store/draft_commit_tx.go:272-288). It compares a generation binding's
// epoch with a live epoch supplied by a generation owner and checks the root is
// not cancelled; its own comment says the rebuildBinding comparison is out of
// scope. It is bound to the draft lane's bpRun, not to an import-effect handle,
// and this lane has no owner from which to obtain a live epoch. Copying it here
// would prove less than the reference while pretending to prove the same thing.
//
// The lane cannot even mint the row the handle would name:
//
//	synthetic_generation_linux.go:386-419  syntheticSelection has no importSlot,
//	                                       importExecution or importContext field,
//	                                       and the key allowlist refuses every
//	                                       unknown key with GENERATION_BEGIN_INVALID.
//	internal/store/generation_ledger.go:505-516  glOrdinaryBinding refuses an
//	                                       importSlot manifest key with
//	                                       GENERATION_OPERATION_UNSUPPORTED because
//	                                       glFamilyManifestAllowed (:426) does not
//	                                       admit it.
//	electron/services/import-generation-source.ts:20  captureImportGenerationContext,
//	                                       the only builder of the frozen
//	                                       ImportGenerationContext, has no Go
//	                                       counterpart in this module.
//	production_import_finalization_generation_linux.go:51, :86  the two names that
//	                                       would read and execute an import
//	                                       generation run are themselves refused.
//
// A handle-carrying receipt can therefore only name a row the desktop wrote, and
// no proof for that row can be built here. This is why "serve the receipt with a
// database-only assert" is not a narrower capability: the row the assert would
// inspect is outside every run this server can create, and the source-binding
// half of the proof has no Go port.
//
// # Why nil, fail-closed and a database-only proof are all wrong answers
//
// The two store guards disagree about a nil proof:
//
//	prepare  importrun_receipts.go:983-985  a nil proof only makes the handle
//	                                        optional, so an unproven handle is
//	                                        accepted and written;
//	commit   importrun_receipts.go:1000-1003 a handle without a proof is refused
//	                                        outright with GENERATION_GUARD_REQUIRED.
//
// A nil proof would therefore write receipts this lane could never commit. A
// non-nil proof that fails closed refuses every real request twice over. A
// database-only proof that accepts a matching row would omit step 4 and accept a
// stale-source receipt. None of the three is a served operation.
//
// The narrowed kinds do not rescue the pair either. commitEffectReceipt implements
// only project-writing-style; the other three kinds return
// ErrImportRunReceiptKindUnbound (internal/store/importrun_receipts.go:1085-1098).
// A real import's first receipt is project-global-facts (stage global), so even a
// perfect writing-style proof could not carry a run through the family. And the
// reference's real writing-style receipt always carries a generationRunHandle
// (electron/services/__tests__/import-generation.test.ts:197), so a no-handle
// style receipt is not the reference's operation.
//
// Replay is not a way out. The reference returns an existing receipt before the
// proof (electron/repositories/import-run-repository.ts:2366-2374) and returns an
// already committed receipt before the execution check, the checkpoint check and
// the effect (:2427-2432). A handler installed for those branches only would be a
// replay reader, not the write the channel names; it would also publish 200 for a
// request the reference's own prepare would have refused with
// GENERATION_IMPORT_EFFECT_HANDLE_REQUIRED. This lane does not publish that
// substitute.
//
// # db:import-run-prepare-inspection: the CAS is not on the wire
//
// The reference handler peeks the tab-scoped inspection by id, optionally checks
// the author preview fingerprints, consumes the inspection, resolves the encoded
// source identities, begins a parsing run, commits every parsed source and
// finalizes parsing (electron/controllers/db-controller.ts:262-355). The Go store
// has the parsing half (BeginParsing, CommitParsedSource, FailParsedSource,
// FinalizeParsing), but the request cannot reach it:
//
//   - importsource.Store.Peek returns only Summary metadata, never chapter bodies
//     or canonical identities (internal/importsource/inspection.go:158-160);
//   - importsource.Store.Consume requires expectedPreviewCAS and refuses on a
//     missing or different value with IMPORT_INSPECTION_PREVIEW_CHANGED
//     (internal/importsource/inspection.go:165-168);
//   - the only wire response that carries an inspection id is
//     dialog:select-novel-files, and it returns inspectionId, purpose, counts,
//     display names, total words, total bytes and the eight-chapter preview, but
//     never previewCAS (internal/transport/import_selection.go:111-116);
//   - a reference-purpose inspection cannot be minted at all:
//     dialog:select-novel-files refuses reference with
//     IMPORT_DURABLE_PARSING_REQUIRED (internal/transport/import_selection.go:79-82),
//     while BeginParsing refuses every purpose except reference
//     (internal/store/importrun_lifecycle.go:801-802). The only inspection this
//     lane can mint is therefore the author-purpose one, which BeginParsing
//     refuses; the author branch would additionally need the unported prepare() and
//     FinalizedDraftImportRepository.preview (internal/store/importrun_lifecycle.go:26-34).
//
// The wire value that is missing is the preview CAS. The handler cannot invent it
// from the inspection id without calling Peek, and calling Peek would only hand
// the handler a value the renderer never saw, which is exactly the binding the CAS
// exists to prove. The store itself is also not reachable from this adapter: the
// picker owns the only importsource.Store (production_import_selection_linux.go:173-180)
// and the residual adapter is constructed with open.durable alone
// (production_open_linux.go:619). Even if a later pass supplied the store, the
// chapters are behind Consume and Consume is behind the CAS, so the exact refusal
// would move to the missing previewCAS rather than disappear.
//
// # The binder
//
// Bind installs nothing. It validates the seam table and refuses to start when a
// row loses its gate, its port, its arity, its path index, a declaration it names
// or a duplicate/foreign name. production_importrun_residual_linux.go:212-224
// installs the four served names and then calls this binder, so the check runs
// inside the real lane. The served count is unchanged at 246 because no name here
// is bound; the db family's residual table
// (production_db_residual_linux.go:153-215) still names the same three, and the
// count pin (production_served_count_linux_test.go:24) is deliberately untouched.

const (
	// The three operations are aliases of the residual file's own constants, so
	// the seam, the 501 pins and the residual prose can never spell a name two
	// different ways. A future pass that serves one of them has to move the
	// residual constant and this seam together, which is the point.
	productionImportRunReceiptsPrepareOperation    = productionImportRunResidualReceiptPrepareName
	productionImportRunReceiptsCommitOperation     = productionImportRunResidualReceiptCommitName
	productionImportRunReceiptsInspectionOperation = productionImportRunResidualPrepareInspectionName
)

// productionImportRunReceiptLine is one declaration a future port has to move,
// together with the file and line where this seam last saw it. The test reads the
// file and matches the marker with productionSeamContains, so a shifted line is a
// diagnosis rather than a regression; a deleted declaration is a failure.
type productionImportRunReceiptLine struct {
	Path   string
	Line   int
	Marker string
}

// productionImportRunReceiptSeam is one refused name: the wire shape the
// renderer really sends, the symbolic code that would surface if the missing
// capability were reached, the missing port in one sentence, and the declarations
// that keep it missing.
//
// Arity counts every wire argument including the frozen three-key context, which
// is always the last argument and never inside an input object. PathIndex is the
// argument that must be the frozen project path. Both are read off
// src/shared/ipc-channels.ts for the channel and checked against the controller's
// own registration.
type productionImportRunReceiptSeam struct {
	Operation string
	Arity     int
	PathIndex int
	Gate      string
	Port      string
	Missing   []productionImportRunReceiptLine
}

// productionImportRunReceiptLineOf is the only constructor of a seam line, so a
// row stays a list of one-line declarations and formatting cannot change what it
// means.
func productionImportRunReceiptLineOf(path string, line int, marker string) productionImportRunReceiptLine {
	return productionImportRunReceiptLine{Path: path, Line: line, Marker: marker}
}

// productionImportRunReceiptSeams is the whole answer of this file. The order is
// the order the three names are reported in, so the test walks one list.
func productionImportRunReceiptSeams() []productionImportRunReceiptSeam {
	return []productionImportRunReceiptSeam{
		{
			Operation: productionImportRunReceiptsPrepareOperation,
			Arity:     4,
			PathIndex: 2,
			Gate:      "GENERATION_IMPORT_EFFECT_HANDLE_REQUIRED",
			Port:      "no live generation-source proof: the store's live-run read proves the import run lease, not the generation handle's slot and current sources",
			Missing: []productionImportRunReceiptLine{
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 118, "type ImportRunGenerationSourceAssert func(handle any, slot ImportRunGenerationSlot) error"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 983, "GENERATION_IMPORT_EFFECT_HANDLE_REQUIRED"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 987, "return assertSources(handle, ImportRunGenerationSlot{RunID: runID, Stage: stage, BatchID: batchID})"),
				productionImportRunReceiptLineOf("../../internal/store/importrun.go", 121, "func readImportRunRow(ctx context.Context, q importRunQueryer, runID string) (importRunRow, bool, error)"),
				productionImportRunReceiptLineOf("../../internal/store/importrun.go", 641, "func importRunAssertExecutionAuthority(ctx context.Context, q importRunQueryer, runID string, execution ImportRunExecution, now int64) (importRunRow, error)"),
				productionImportRunReceiptLineOf("synthetic_generation_linux.go", 386, "type syntheticSelection struct {"),
				productionImportRunReceiptLineOf("../../internal/store/generation_ledger.go", 505, `"importSlot"`),
				productionImportRunReceiptLineOf("../../../electron/services/import-generation-source.ts", 20, "captureImportGenerationContext"),
				productionImportRunReceiptLineOf("production_import_finalization_generation_linux.go", 51, "// # import-generation:read - refused"),
				productionImportRunReceiptLineOf("production_import_finalization_generation_linux.go", 86, "// # import-generation:execute - refused"),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 425, "prepareEffectReceipt(request, execution, Date.now(), assertImportGenerationSourcesCurrent)"),
				productionImportRunReceiptLineOf("../../../electron/controllers/generation-controller.ts", 72, "assertImportGenerationSourcesCurrent(handle: MainGenerationRunHandle"),
				productionImportRunReceiptLineOf("../../../electron/services/main-generation-owner.ts", 1462, "imports.context(run).slot"),
				productionImportRunReceiptLineOf("../../../electron/services/main-generation-owner.ts", 1465, "compareGenerationSourceBindings(run.binding, current)"),
				productionImportRunReceiptLineOf("../../../electron/services/main-generation-owner.ts", 303, "function currentModelReceipt(binding: RunBinding): SafeGenerationModelReceipt"),
			},
		},
		{
			Operation: productionImportRunReceiptsCommitOperation,
			Arity:     6,
			PathIndex: 4,
			Gate:      "GENERATION_GUARD_REQUIRED",
			Port:      "the same missing proof: a handle-carrying prepared receipt is refused without it, and only an already committed row replays before it",
			Missing: []productionImportRunReceiptLine{
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 1260, "execution ImportRunExecution, assertSources ImportRunGenerationSourceAssert"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 1003, "GENERATION_GUARD_REQUIRED"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 1306, "importRunReceiptAssertCommitGeneration(assertSources, runID, stage, batchID, receipt.Payload)"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 1086, "The three unbound kinds name a family this lane does not have"),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 437, "(handle, slot) => assertImportGenerationSourcesCurrent(handle, slot, true)"),
				productionImportRunReceiptLineOf("../../../electron/services/main-generation-owner.ts", 1471, "GENERATION_IMPORT_EFFECT_SOURCE_CHANGED"),
			},
		},
		{
			Operation: productionImportRunReceiptsInspectionOperation,
			Arity:     3,
			PathIndex: 1,
			Gate:      "IMPORT_DURABLE_PARSING_REQUIRED",
			Port:      "the inspection CAS never reaches the wire: Peek has metadata only and Consume demands expectedPreviewCAS, while the only response carrying inspectionId omits previewCAS",
			Missing: []productionImportRunReceiptLine{
				productionImportRunReceiptLineOf("../../internal/importsource/inspection.go", 158, "func (s *Store) Peek(receipt string,binding Binding) (Summary,error)"),
				productionImportRunReceiptLineOf("../../internal/importsource/inspection.go", 165, "func (s *Store) Consume(receipt,expectedPreviewCAS string,binding Binding)"),
				productionImportRunReceiptLineOf("../../internal/importsource/inspection.go", 167, "IMPORT_INSPECTION_PREVIEW_CHANGED"),
				productionImportRunReceiptLineOf("../../internal/transport/import_selection.go", 81, "IMPORT_DURABLE_PARSING_REQUIRED"),
				productionImportRunReceiptLineOf("../../internal/transport/import_selection.go", 112, `"inspectionId": summary.InspectionID`),
				productionImportRunReceiptLineOf("../../internal/store/importrun_lifecycle.go", 801, `if candidate.Purpose != "reference"`),
				productionImportRunReceiptLineOf("../../internal/store/importrun_lifecycle.go", 1048, "func (r *ImportRuns) CommitParsedSource(ctx context.Context, runID, sourceID string, chapters []ImportRunChapterInput)"),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 262, "operations.handle('db:import-run-prepare-inspection', async ("),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 269, "importInspectionStore.peek(request.inspectionId, webContentsId, request.purpose)"),
			},
		},
	}
}

// productionImportRunReceiptSeamFor returns the row for one name. A name the
// table does not list has no seam, which is how a typo refuses instead of
// publishing a handler nobody described.
func productionImportRunReceiptSeamFor(operation string) (productionImportRunReceiptSeam, bool) {
	for _, seam := range productionImportRunReceiptSeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionImportRunReceiptSeam{}, false
}

// productionImportRunReceiptSeamSymbolic reports whether a code is the uppercase,
// underscore-separated shape every refusal in this lane is published under. It is
// a shape check, not a vocabulary: the seam table is where the vocabulary lives.
func productionImportRunReceiptSeamSymbolic(code string) bool {
	if code == "" {
		return false
	}
	for index := 0; index < len(code); index++ {
		character := code[index]
		switch {
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '_':
		default:
			return false
		}
	}
	return true
}

// productionImportRunReceipts is the no-op binder this file contributes. It holds
// the borrowed durable lane only so a future port can be installed from the same
// place without changing its constructor or its caller.
type productionImportRunReceipts struct {
	durable *productionDurableOpen
}

// newProductionImportRunReceipts returns the seam adapter for the lane built by
// productionHandlerWithContract. It resolves nothing and opens nothing.
func newProductionImportRunReceipts(durable *productionDurableOpen) *productionImportRunReceipts {
	return &productionImportRunReceipts{durable: durable}
}

// Bind installs nothing and validates the seam. It refuses a nil adapter, a nil
// server, a duplicate name, a name outside the family, a gate that is not
// symbolic, an arity/path index that cannot describe a real request, and a seam
// whose declarations are blank. It also refuses to proceed when one of the three
// names is already bound, because that would make this table a stale note rather
// than the family's refusal.
//
// It deliberately does not read the source files: the production binary's working
// directory is not the test's, and a startup that reads the source tree would
// fail for a reason unrelated to the author's database. The test does the
// productionSeamContains re-read.
func (r *productionImportRunReceipts) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil {
		return errors.New("IMPORT_RUN_RECEIPTS_PROVIDER_REQUIRED")
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	seams := productionImportRunReceiptSeams()
	if len(seams) != 3 {
		return errors.New("IMPORT_RUN_RECEIPTS_SEAM_COUNT_INVALID")
	}
	bound := make(map[string]bool, len(seams))
	for _, name := range server.BoundOperations() {
		bound[name] = true
	}
	seen := make(map[string]bool, len(seams))
	for _, seam := range seams {
		if !productionImportRunReceiptsIsFamily(seam.Operation) {
			return errors.New("IMPORT_RUN_RECEIPTS_NAME_INVALID")
		}
		if seen[seam.Operation] {
			return errors.New("IMPORT_RUN_RECEIPTS_NAME_DUPLICATE")
		}
		seen[seam.Operation] = true
		if bound[seam.Operation] {
			return errors.New("IMPORT_RUN_RECEIPTS_SEAM_ALREADY_BOUND")
		}
		if seam.Arity < 2 || seam.PathIndex < 0 || seam.PathIndex > seam.Arity-2 {
			return errors.New("IMPORT_RUN_RECEIPTS_SHAPE_INVALID")
		}
		if seam.Port == "" || len(seam.Missing) == 0 {
			return errors.New("IMPORT_RUN_RECEIPTS_SEAM_INCOMPLETE")
		}
		if !productionImportRunReceiptSeamSymbolic(seam.Gate) {
			return errors.New("IMPORT_RUN_RECEIPTS_GATE_INVALID")
		}
		for _, line := range seam.Missing {
			if line.Path == "" || line.Line < 0 || line.Marker == "" {
				return errors.New("IMPORT_RUN_RECEIPTS_LINE_INVALID")
			}
		}
	}
	return nil
}

// productionImportRunReceiptsIsFamily is the closed name set in this file: all
// three names belong to the same inventoried family, so a typo cannot enter the
// table as a foreign operation.
func productionImportRunReceiptsIsFamily(operation string) bool {
	switch operation {
	case productionImportRunReceiptsPrepareOperation, productionImportRunReceiptsCommitOperation,
		productionImportRunReceiptsInspectionOperation:
		return true
	}
	return false
}
