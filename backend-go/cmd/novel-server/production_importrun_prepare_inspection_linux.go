//go:build linux

package main

// This file is the closed record of db:import-run-prepare-inspection. It installs
// nothing: the name is served by
// production_importrun_prepare_inspection_serve_linux.go, next to the picker and
// the author preview it shares its receipt store with. What this file keeps are the
// three declarations the previous pass pinned as the reason the name could not be
// served, so the test can re-read each one and the reason cannot quietly return.
//
// # The three gaps, and what closed each
//
// 1. THE CLOSURE-LOCAL STORE. The picker constructed its importsource.Store inside
//    its own wiring closure (production_open_linux.go:904,
//    `selection, selectionErr := newProductionImportSelection(open, uploads)`), and
//    transport.BindImportSelection (internal/transport/import_selection.go:48) keeps
//    the store to itself, so no other binder could resolve a receipt id it minted.
//    The construction now runs above the third bind slice
//    (`for _, bind := range []func() error{`, production_open_linux.go:772) as
//    `importSelection`, the serve adapter is built from it there
//    (`importPrepareInspection`), and all three names - the picker, the preview and
//    this one - hold the same pointer. Two stores would be two receipt tables, and
//    the second would answer IMPORT_INSPECTION_EXPIRED for every id the first ever
//    issued.
//
// 2. THE PREVIEW THAT REFUSED THE ID. db:import-run-author-preview answered the
//    reference's own string argument with 501 IMPORT_INSPECTION_OWNER_UNAVAILABLE
//    (production_finalized_draft_import_linux.go:224). Its preview arm now resolves
//    that id through the shared store and previews the frozen chapters with the same
//    store read the chapters form uses, so the authorityFingerprint and
//    manifestFingerprint this request carries are produced by a served read.
//
// 3. THE MISSING AUTHOR PREPARE. ImportRunRepository.prepare's author half
//    (electron/repositories/import-run-repository.ts:2005-2138) had no store method;
//    it is store.ImportRuns.PrepareAuthor
//    (internal/store/importrun_author_prepare.go), which reuses the finalized-draft
//    lane's own preview and receipt validation instead of restating them.
//    BeginParsing still refuses every purpose but reference
//    (internal/store/importrun_lifecycle.go:801-802), and that is now correct rather
//    than a blocker: the author arm never enters the parsing lane.
//
// # What was never a gap, and is still not one
//
// The preview CAS does not cross the wire, and it does not have to. The store keeps
// it on the Summary (internal/importsource/inspection.go:49), Peek hands that
// summary to a trusted internal consumer (:158), and Consume demands it (:165). The
// binder peeks the receipt it is about to use and consumes it with that same value,
// exactly as the desktop's controller does (electron/controllers/db-controller.ts:269,
// :282); the renderer never sees or supplies one. The picker still answers an
// inspection id and no CAS, which is the reference's own projection
// (internal/transport/import_selection.go:110-116).
//
// # The wire shape this record carries
//
// The name takes (request 0, expected path 1, frozen three-key session 2) - arity 3,
// counted against src/shared/ipc-channels.ts:926-929 - and answers
// {success:true, preparation} or {success:false, errorCode:AUTHOR_IMPORT_PREVIEW_STALE}
// (src/shared/import-run.ts:296-310, :369-375). Both numbers live here because the
// serve file, this record and the tests must not spell them three ways.

const (
	// productionImportRunPrepareInspectionOperation is the one name this file
	// records, spelled through the residual family's constant so a typo cannot enter
	// the record as a foreign operation.
	productionImportRunPrepareInspectionOperation = productionImportRunResidualPrepareInspectionName
	// productionImportRunPrepareInspectionArity is the wire arity: the request, the
	// expected project path and the frozen context, in that order.
	productionImportRunPrepareInspectionArity = 3
	// productionImportRunPrepareInspectionPathIndex is where the explicit path
	// sits inside that arity.
	productionImportRunPrepareInspectionPathIndex = 1
)

// productionImportRunPrepareInspectionLine is one declaration the record points at:
// the file, the line it was recorded on, and a marker that has to survive
// formatting. The number is an annotation for the failure message; the marker is the
// assertion, compared with productionSeamContains and therefore white-space
// insensitive (production_seam_marker_test.go:1-30).
type productionImportRunPrepareInspectionLine struct {
	Path   string
	Line   int
	Marker string
}

// productionImportRunPrepareInspectionLineOf is the only constructor of a line, so
// a row stays a list of one-line declarations and gofmt cannot change what it means.
func productionImportRunPrepareInspectionLineOf(path string, line int, marker string) productionImportRunPrepareInspectionLine {
	return productionImportRunPrepareInspectionLine{Path: path, Line: line, Marker: marker}
}

// productionImportRunPrepareInspectionClosed lists the declarations that prove each
// of the three gaps closed. Every marker is a declaration this change added, not a
// prose claim: the store method, its manifest gate, the author-commit insert, the
// adapter's receipt resolution, the preview arm's resolution of the id and the id
// form's own resolver.
func productionImportRunPrepareInspectionClosed() []productionImportRunPrepareInspectionLine {
	return []productionImportRunPrepareInspectionLine{
		// Gap 3: the author half exists as a store method.
		productionImportRunPrepareInspectionLineOf("../../internal/store/importrun_author_prepare.go", 107,
			"func (r *ImportRuns) PrepareAuthor(ctx context.Context, candidate ImportRunPrepareAuthorRequest) (map[string]any, error)"),
		// Gap 3: the manifest mismatch is the reference's own code.
		productionImportRunPrepareInspectionLineOf("../../internal/store/importrun_author_prepare.go", 104,
			`errors.New("IMPORT_RUN_AUTHOR_MANIFEST_MISMATCH")`),
		// Gap 3: the run is written at the reference's author stage.
		productionImportRunPrepareInspectionLineOf("../../internal/store/importrun_author_prepare.go", 243,
			"authority_fingerprint, source_display_json, locale, stage, status,"),
		// Gap 1: the handler resolves the id through the shared store, one use.
		productionImportRunPrepareInspectionLineOf("production_importrun_prepare_inspection_serve_linux.go", 322,
			"func (r *productionImportRunPrepareInspectionServe) takeInspection(ctx context.Context,"),
		// Gap 1: the preview's id form resolves the same receipt.
		productionImportRunPrepareInspectionLineOf("production_finalized_draft_import_linux.go", 371,
			"r.inspections.previewFromInspection(ctx, caller, expected, inspectionID)"),
		// Gap 2: the id form is answered by the shared store, not refused.
		productionImportRunPrepareInspectionLineOf("production_importrun_prepare_inspection_serve_linux.go", 273,
			"func (r *productionImportRunPrepareInspectionServe) previewFromInspection(ctx context.Context,"),
	}
}
