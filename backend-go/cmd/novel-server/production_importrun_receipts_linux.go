//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the import-run family's last seam. It now holds NO name:
//
//	db:import-run-prepare-inspection       served  production_importrun_prepare_inspection_serve_linux.go
//
// The two receipt writes it also used to hold are SERVED, and the reason they can
// be is recorded here because this is the file whose argument they defeated:
//
//	db:import-run-effect-receipt-prepare   served  productionImportRunReceiptsServe2.prepare
//	db:import-run-effect-receipt-commit    served  productionImportRunReceiptsServe2.commit
//
// # What the old refusal said, and which half was wrong
//
// The seam said the proof could not run for two reasons, and both were right about
// the code as it stood:
//
//	a. THE CONNECTION. The assert is invoked from INSIDE the receipt's BEGIN
//	   IMMEDIATE (internal/store/importrun_receipts.go, through importRunImmediate
//	   at internal/store/importrun.go:948), which reserves the author database's
//	   only connection (internal/projectfs/author_linux.go:121
//	   db.SetMaxOpenConns(1)). The callback type carried (handle, slot), so the
//	   reference's first step - requireRun's read at
//	   electron/services/main-generation-owner.ts:1454 - had to open a second
//	   handle and waited for the one the transaction held. The store's own word for
//	   a prover that does that is "a deadlock rather than a proof"
//	   (internal/store/character_proposal_pending.go:37), and the wait was measured
//	   ending in context.DeadlineExceeded.
//	b. THE OWNER. The fingerprint reproduction is
//	   generationowner.ReproduceBinding (internal/generationowner/binding.go:63),
//	   whose registry holds the run's model lease. That registry is created with
//	   the one *productionGeneration this process builds
//	   (production_generation_begin_linux.go:42-45), and the receipt binder was
//	   constructed with `open.durable` alone
//	   (production_open_linux.go:627 -> production_importrun_residual_linux.go:247).
//
// The wrong half of the old wording was the sentence "no wiring change fixes
// that". It was wrong about which wiring: the receipt binder does not need an
// extra argument at :627, it needs to be an owner-bound binder of its own, which
// is how four sibling families are already installed from the same block
// (newProductionFinalizationServe(owner), newProductionLegacyRosterServe(owner),
// newProductionAgentGenerationServe(owner), newProductionImportGenerationServe(owner)).
// The one line is:
//
//	production_open_linux.go, inside the block that opens with
//	"if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {",
//	directly after newProductionImportGenerationServe(owner).Bind(server) and
//	before the block's closing "} else {":
//	  newProductionImportRunReceiptsServe2(owner).Bind(server)
//
// Until that line is installed the shipped lane keeps both names at
// GO_OPERATION_NOT_MIGRATED, because productionImportRunReceiptsServe2.Bind
// installs nothing without an owner - it never publishes a handler that could only
// refuse. The pair is not in this table either way: a seam is a documented
// refusal, and the lane has a binder for both names.
//
// # What unblocked the connection half
//
// The run's read moved into the store, where the connection already is. The
// callback type (internal/store/importrun_receipts.go:130) now carries the run as
// DATA:
//
//	ImportRunGenerationSourceAssert func(handle any, slot ImportRunGenerationSlot,
//	                                 run ImportRunGenerationEvidence) error
//
// and internal/store/importrun_receipts_generation.go reads it on the receipt
// transaction's own connection: generation_runs.binding_json verbatim, the root's
// status, and every attempt of this run with its usage receipt and its artifact's
// status and JSON. Nothing is decoded, hashed or decided by the store; every
// refusal is still the prover's. That is what the reference's assert gets from the
// dispatcher, and it is why no second connection exists to wait on.
//
// # The proof the served names install
//
// production_importrun_receipts_serve2_linux.go is
// electron/services/main-generation-owner.ts:1453-1472 over that evidence plus
// generationowner.ReproduceBinding:
//
//	1. the handle's run must exist (GENERATION_RUN_MISSING) and be this project's
//	   own {projectId, epoch, rootActionId, runId}
//	   (GENERATION_RUN_IDENTITY_MISMATCH), main-generation-owner.ts:156-167;
//	2. its frozen import slot must be the receipt's slot
//	   (GENERATION_IMPORT_EFFECT_SLOT_CHANGED), :1462;
//	3. its frozen fingerprint must still reproduce from the CURRENT sources under
//	   its own scope (GENERATION_IMPORT_EFFECT_SOURCE_CHANGED), :1463-1465;
//	4. its root must not be cancelled (GENERATION_ROOT_MISSING when the row is
//	   gone) and every attempt of the run must have stopped with an artifact whose
//	   text and textHash re-derive and which is not discarded
//	   (GENERATION_IMPORT_EFFECT_SOURCE_CHANGED), :1465-1471.
//
// The reference's controller always supplies this proof for both names
// (electron/controllers/db-controller.ts:425 for prepare, :437 for commit), so the
// served handlers install it on both legs, exactly as the desktop does. That also
// retires the old "the two store guards disagree about nil" objection: neither leg
// is handed nil any more.
//
// # The one name this file used to refuse, and what closed it
//
// db:import-run-prepare-inspection was refused because the request could not
// reach the parsing store: the inspection store was closure-local to the picker,
// the only served mint dropped the preview CAS, and a reference-purpose receipt
// could not be minted at all (dialog:select-novel-files answered
// IMPORT_DURABLE_PARSING_REQUIRED). All three are closed now:
//
//   - the picker's store is hoisted and shared with this name's adapter
//     (production_open_linux.go:596-597, :969);
//   - the CAS never needs to cross the wire: the adapter peeks the receipt it is
//     about to spend and consumes it with that same server-side value
//     (production_importrun_prepare_inspection_serve_linux.go, takeInspection),
//     exactly as the desktop's controller does (db-controller.ts:269, :282);
//   - the picker admits both purposes (internal/transport/import_selection.go),
//     and the reference arm of the handler runs the parsing lane the store already
//     has: BeginParsing, CommitParsedSource per source (FailParsedSource on a
//     refusal) and FinalizeParsing (internal/store/importrun_lifecycle.go).
//
// The seam table below is therefore empty, and Bind refuses a non-empty one: a row
// here would be a refusal whose operation the serve decision says is served.
//
// # The binder
//
// Bind installs nothing. It validates the seam table and refuses to start when a
// row loses its gate, its port, its arity, its path index, a declaration it names
// or a duplicate/foreign name, and when the table disagrees with the serve decision
// table (a refused name with no decision, or a name still listed here while the
// decision says it is served). production_importrun_residual_linux.go:249-271
// installs the four served names and then calls this binder, so the check runs
// inside the real lane.

const (
	// The three operation names are aliases of the residual file's own constants, so
	// the seam, the serve decision and the residual prose can never spell a name two
	// different ways. Two of them are now served and are named here only so the
	// alias table stays complete: production_importrun_receipts_serve2_linux.go
	// binds prepare and commit through these same constants.
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

// productionImportRunReceiptSeams is the whole answer of this file: no rows. Every
// name the family's seam used to hold has a binder, and the last one -
// db:import-run-prepare-inspection - moved to
// production_importrun_prepare_inspection_serve_linux.go with the parsing store
// behind it. The table is kept, and kept empty, so the completeness test can prove
// the family is covered rather than simply short.
func productionImportRunReceiptSeams() []productionImportRunReceiptSeam {
	return nil
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
// server, and any row in a table the family has finished: every name this file
// used to hold is served, so a row here would be a refusal the serve decision
// contradicts. The remaining row checks are kept for the day a refusal returns.
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
	if len(seams) != 0 {
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
	// The serve decision and this seam are two halves of one answer. A name listed
	// here that the decision says is served would make the seam a stale note; a
	// refused name with no decision row would be a refusal whose reason nobody
	// recorded.
	if err := productionImportRunReceiptServeAgrees(productionImportRunReceiptServeDecisions(), seams); err != nil {
		return err
	}
	return nil
}

// productionImportRunReceiptsIsFamily is the closed name set in this file: all
// three names belong to the same inventoried family, so a typo cannot enter the
// table as a foreign operation. Two of them are served by
// production_importrun_receipts_serve2_linux.go and have no row above.
func productionImportRunReceiptsIsFamily(operation string) bool {
	switch operation {
	case productionImportRunReceiptsPrepareOperation, productionImportRunReceiptsCommitOperation,
		productionImportRunReceiptsInspectionOperation:
		return true
	}
	return false
}
