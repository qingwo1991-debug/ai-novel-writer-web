//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the re-test of the one refused name
// db:import-run-prepare-inspection, and the corrected record of why it stays
// refused. It installs nothing: the name keeps failing closed at
// GO_OPERATION_NOT_MIGRATED, and this file's Bind only validates the corrected
// seam inside the real lane.
//
// # The refused conclusion, re-read against the current sources
//
// The residual seam said the blocker was the preview CAS: "the inspection CAS
// never appears on the wire (internal/importsource/inspection.go:165), so an id
// minted by this lane could never reach BeginParsing", and
// production_importrun_receipts_linux.go:107-126 argued that peeking the CAS
// server-side "would hand back a digest the renderer never saw". Both rest on a
// premise the current files refute:
//
//   - internal/transport/import_selection.go:110-116 projects
//     inspectionId, purpose, sourceCount, sourceDisplayNames, chapterCount,
//     totalWords, totalBytes and preview - which is exactly the desktop's own
//     ImportInspectionSummary (electron/services/import-inspection-store.ts:153
//     has consume(inspectionId, webContentsId), no CAS anywhere). Dropping the
//     CAS is therefore the reference's own shape, not a lane omission.
//   - internal/importsource/inspection.go:49 still carries
//     `PreviewCAS string `json:"previewCAS"`` on Summary; :158 Peek returns that
//     summary to a trusted internal consumer, and :165-171 Consume demands it.
//     A server-side handler does not need the renderer to round-trip the CAS: it
//     peeks the receipt the id names, reads Summary.PreviewCAS, and consumes with
//     it - the exact peek-then-consume the desktop controller performs
//     (electron/controllers/db-controller.ts:269, :282). The CAS is produced
//     from the staged upload's bytes (production_import_selection_linux.go
//     capture reads the staged blob, Inspect freezes it, and the CAS is the
//     keyed digest over the frozen source aliases and every chapter, inspection.go
//     :150-165), so a browser upload of forty chapters produces a CAS the lane can
//     prove without the desktop's directory read.
//
// So the CAS is NOT the gap. Three things are, and each is pinned below.
//
// # The three gaps that actually keep it refused
//
// 1. No shared inspection owner. The only importsource.Store in the lane is
//    constructed inside the picker's own wiring closure
//    (production_open_linux.go:898 `selection, selectionErr :=
//    newProductionImportSelection(open, uploads)`), whose last line hands it to
//    transport.BindImportSelection and then drops it. transport keeps the store
//    inside that binder (import_selection.go:48), so no prepare binder can call
//    Peek or Consume on an id the picker minted. Every prepare handler built from
//    open.durable alone would answer IMPORT_INSPECTION_EXPIRED for every id the
//    picker ever issued.
//
// 2. The renderer's preceding step is still closed. The author flow that calls
//    this name first asks db:import-run-author-preview with the same inspection
//    id (src/components/dialogs/ImportNovelDialog.tsx:131-132) to obtain the
//    authorityFingerprint and manifestFingerprint the prepare request carries
//    (src/shared/import-run.ts:300-304). That name's served binder still refuses
//    the id form with 501 IMPORT_INSPECTION_OWNER_UNAVAILABLE
//    (production_finalized_draft_import_linux.go:224, :420-423), so the browser
//    cannot reach prepare-inspection's author branch at all - the two
//    fingerprints cannot be produced by any served read.
//
// 3. The author branch has no store method, and the reference branch is
//    unreachable. ImportRunRepository.prepare's author half
//    (electron/repositories/import-run-repository.ts:2005-2138: preview, the
//    resumable short-circuit, fenceUncommittedAuthorRun, hasCommittedAuthorFinalizationReceipt,
//    the author-commit INSERT and the ledger row) has no counterpart - internal/store
//    owns BeginParsing/CommitParsedSource/FinalizeParsing but no author prepare,
//    and BeginParsing refuses every purpose but 'reference'
//    (internal/store/importrun_lifecycle.go:801-802). A reference inspection can
//    never exist: dialog:select-novel-files refuses the reference purpose with
//    IMPORT_DURABLE_PARSING_REQUIRED (import_selection.go:81), and the renderer's
//    reference flow returns through the picker's own preparation before it would
//    ever call this name (ImportNovelDialog.tsx:363-370). Serving only the
//    reference half would bind a handler whose input cannot be minted and whose
//    caller never sends it.
//
// # What would close each gap
//
//  1. Hoist the picker's selection out of its closure so one *importsource.Store
//     is shared with the prepare binder (and with author-preview), then bind the
//     prepare adapter beside the picker line, not the owner block - it needs the
//     durable borrow fence AND the picker's store, neither of which the owner
//     block provides.
//  2. Teach the author-preview binder (its own family's file) to resolve the
//     inspection id through that shared store.
//  3. Port ImportRunRepository.prepare's author half into internal/store (a new
//     file, same package), reusing importRunNormalizeChapters,
//     importRunMatchingResumableRun, importRunReceiptValidate and
//     Drafts.FinalizedImportPreview, and add an importsource.Store method that
//     resolves the consumed inspection's EncodedIdentity through
//     ResolveEncodedSources under the store's own key.
//
// # Binder and count
//
// Bind installs nothing, so the served count is unchanged (277, not 278). It
// refuses a nil adapter/server, an already-bound name (so a sibling that later
// serves it cannot leave a stale note), and a malformed record; it validates the
// corrected gate, port and declaration lists at startup. The one wiring line is
// in production_importrun_residual_linux.go, directly after the receipts seam
// call (newProductionImportRunReceipts(r.durable).Bind(server)):
//
//	if err := newProductionImportRunPrepareInspection(r.durable).Bind(server); err != nil {
//		return err
//	}
//
// This adapter is built with open.durable alone and opens nothing; a real port
// would additionally need the shared store (gap 1) and the author prepare (gap 3).

const (
	// productionImportRunPrepareInspectionOperation is the one name this file
	// records, spelled once by the residual family and never re-typed here.
	productionImportRunPrepareInspectionOperation = productionImportRunResidualPrepareInspectionName
	// productionImportRunPrepareInspectionArity is the wire arity:
	// (request 0, path 1, frozen context 2), src/shared/ipc-channels.ts:926-929.
	productionImportRunPrepareInspectionArity = 3
	// productionImportRunPrepareInspectionPathIndex is where the explicit path
	// sits inside that arity.
	productionImportRunPrepareInspectionPathIndex = 1
	// productionImportRunPrepareInspectionGate is the code that surfaces when the
	// missing capability is reached: the tab-scoped inspection owner.
	productionImportRunPrepareInspectionGate = "IMPORT_INSPECTION_OWNER_UNAVAILABLE"
	// productionImportRunPrepareInspectionPort is the corrected one-sentence gap:
	// the picker's store is closure-local, the renderer's preceding author-preview
	// still refuses the id form, and the author prepare has no store method.
	productionImportRunPrepareInspectionPort = "the picker's inspection store is closure-local, the renderer's author-preview step still refuses the id form, and the author prepare has no store method"
)

// productionImportRunPrepareInspectionLine is one declaration that a future port
// has to move (Missing) or one declaration the old refusal wrongly read as a
// blocker (Refuted). The test re-reads each marker with productionSeamContains,
// so a shifted line is a diagnosis and a deleted declaration is a failure.
type productionImportRunPrepareInspectionLine struct {
	Path   string
	Line   int
	Marker string
}

// productionImportRunPrepareInspectionLineOf is the only constructor of a line,
// so a row stays a list of one-line declarations and formatting cannot change
// what it means.
func productionImportRunPrepareInspectionLineOf(path string, line int, marker string) productionImportRunPrepareInspectionLine {
	return productionImportRunPrepareInspectionLine{Path: path, Line: line, Marker: marker}
}

// productionImportRunPrepareInspectionRefuted lists the declarations the old
// refusal quoted as the CAS blocker. Every one is still real, but none of them
// is the blocker: the CAS is a server-side handoff, exactly as the desktop's own
// peek-then-consume is.
func productionImportRunPrepareInspectionRefuted() []productionImportRunPrepareInspectionLine {
	return []productionImportRunPrepareInspectionLine{
		productionImportRunPrepareInspectionLineOf("../../internal/importsource/inspection.go", 49, "PreviewCAS string `json:\"previewCAS\"`"),
		productionImportRunPrepareInspectionLineOf("../../internal/importsource/inspection.go", 158, "func (s *Store) Peek(receipt string,binding Binding) (Summary,error)"),
		productionImportRunPrepareInspectionLineOf("../../internal/importsource/inspection.go", 165, "func (s *Store) Consume(receipt,expectedPreviewCAS string,binding Binding)"),
		productionImportRunPrepareInspectionLineOf("../../internal/importsource/inspection.go", 159, "return cloneSummary(entry.inspection.Summary),nil"),
		productionImportRunPrepareInspectionLineOf("../../internal/transport/import_selection.go", 112, `"inspectionId": summary.InspectionID`),
		productionImportRunPrepareInspectionLineOf("../../../electron/services/import-inspection-store.ts", 153, "consume(inspectionId: string, webContentsId: number): ImportInspection"),
	}
}

// productionImportRunPrepareInspectionMissing lists the declarations that keep
// the name refused today. This is the corrected answer that replaces the old
// "the CAS never reaches the wire" row in the db residual seam.
func productionImportRunPrepareInspectionMissing() []productionImportRunPrepareInspectionLine {
	return []productionImportRunPrepareInspectionLine{
		productionImportRunPrepareInspectionLineOf("production_open_linux.go", 898, "selection, selectionErr := newProductionImportSelection(open, uploads)"),
		productionImportRunPrepareInspectionLineOf("../../internal/transport/import_selection.go", 48, "func (s *Server) BindImportSelection(store *importsource.Store, provider ImportSelectionProvider) error"),
		productionImportRunPrepareInspectionLineOf("production_finalized_draft_import_linux.go", 224, "productionFinalizedDraftImportInspectionCode = \"IMPORT_INSPECTION_OWNER_UNAVAILABLE\""),
		productionImportRunPrepareInspectionLineOf("../../../src/components/dialogs/ImportNovelDialog.tsx", 132, "inspection.inspectionId,"),
		productionImportRunPrepareInspectionLineOf("../../../electron/controllers/db-controller.ts", 262, "operations.handle('db:import-run-prepare-inspection', async ("),
		productionImportRunPrepareInspectionLineOf("../../internal/store/importrun_lifecycle.go", 801, `if candidate.Purpose != "reference"`),
		productionImportRunPrepareInspectionLineOf("../../internal/store/importrun_lifecycle.go", 26, "What is deliberately NOT here: prepare() for the author-manuscript purpose"),
	}
}

// productionImportRunPrepareInspectionResidualMissing maps the corrected
// declaration list onto the db residual seam's own line type, so the residual
// row and this file spell the markers once.
func productionImportRunPrepareInspectionResidualMissing() []productionDBResidualLine {
	lines := productionImportRunPrepareInspectionMissing()
	out := make([]productionDBResidualLine, len(lines))
	for index, line := range lines {
		out[index] = productionDBResidualLineOf(line.Path, line.Line, line.Marker)
	}
	return out
}

// productionImportRunPrepareInspectionCodeShape reports whether a code is the
// uppercase, underscore-separated shape every refusal in this lane publishes
// under. It is a shape check, not a vocabulary: the seam table is where the
// vocabulary lives.
func productionImportRunPrepareInspectionCodeShape(code string) bool {
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

// productionImportRunPrepareInspectionIsFamily is the closed name set: the one
// name this file records, spelled through the residual family's constant, so a
// typo cannot enter the table as a foreign operation.
func productionImportRunPrepareInspectionIsFamily(operation string) bool {
	return operation == productionImportRunPrepareInspectionOperation
}

// productionImportRunPrepareInspection is the no-op binder this file
// contributes. It holds the borrowed durable lane only so a future port can be
// installed from the same place without changing its constructor.
type productionImportRunPrepareInspection struct {
	durable *productionDurableOpen
}

// newProductionImportRunPrepareInspection returns the seam adapter for the lane
// built by productionHandlerWithContract. It resolves nothing and opens nothing.
func newProductionImportRunPrepareInspection(durable *productionDurableOpen) *productionImportRunPrepareInspection {
	return &productionImportRunPrepareInspection{durable: durable}
}

// Bind installs nothing and validates the corrected record. It refuses a nil
// adapter, a nil server, a name that a sibling already bound (so a stale note
// cannot survive a future port), a gate that is not symbolic, an arity/path
// index that cannot describe a real request, and an empty or malformed
// declaration list. It deliberately does not read the source files: the
// production binary's working directory is not the test's. The test does the
// productionSeamContains re-read.
func (r *productionImportRunPrepareInspection) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil {
		return errors.New("IMPORT_RUN_PREPARE_INSPECTION_PROVIDER_REQUIRED")
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	bound := make(map[string]bool, 1)
	for _, name := range server.BoundOperations() {
		bound[name] = true
	}
	return productionImportRunPrepareInspectionValidate(
		productionImportRunPrepareInspectionOperation,
		productionImportRunPrepareInspectionArity,
		productionImportRunPrepareInspectionPathIndex,
		productionImportRunPrepareInspectionGate,
		productionImportRunPrepareInspectionPort,
		productionImportRunPrepareInspectionRefuted(),
		productionImportRunPrepareInspectionMissing(),
		bound,
	)
}

// productionImportRunPrepareInspectionValidate is the whole record check, split
// out of Bind so the test can feed a fabricated record and a fabricated bound
// set. A name that is already bound, a name outside the family, a gate that is
// not symbolic, an arity/path index that cannot describe a real request, an
// empty port or declaration list, and a blank declaration line are each refused
// by name.
func productionImportRunPrepareInspectionValidate(operation string, arity, pathIndex int,
	gate, port string, refuted, missing []productionImportRunPrepareInspectionLine,
	bound map[string]bool) error {
	if bound[operation] {
		return errors.New("IMPORT_RUN_PREPARE_INSPECTION_ALREADY_BOUND")
	}
	if !productionImportRunPrepareInspectionIsFamily(operation) {
		return errors.New("IMPORT_RUN_PREPARE_INSPECTION_NAME_INVALID")
	}
	if arity < 2 || pathIndex < 0 || pathIndex > arity-2 {
		return errors.New("IMPORT_RUN_PREPARE_INSPECTION_SHAPE_INVALID")
	}
	if port == "" || len(refuted) == 0 || len(missing) == 0 {
		return errors.New("IMPORT_RUN_PREPARE_INSPECTION_SEAM_INCOMPLETE")
	}
	if !productionImportRunPrepareInspectionCodeShape(gate) {
		return errors.New("IMPORT_RUN_PREPARE_INSPECTION_GATE_INVALID")
	}
	for _, lines := range [][]productionImportRunPrepareInspectionLine{refuted, missing} {
		for _, line := range lines {
			if line.Path == "" || line.Line < 0 || line.Marker == "" {
				return errors.New("IMPORT_RUN_PREPARE_INSPECTION_LINE_INVALID")
			}
		}
	}
	return nil
}
