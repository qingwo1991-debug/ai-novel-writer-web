//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the db family's residual: the three inventoried `db:*` names this
// lane still answers at the fail-closed 501 GO_OPERATION_NOT_MIGRATED
// (internal/transport/server.go:116) instead of serving, and the exact code that
// has to move before any of them can be installed as a handler.
//
// # What this file measured
//
// The inventory is 281 names (web/operations.json), 103 of them `db:*`. Reading
// every binder the lane really calls - the `for _, bind := range []func() error{`
// lists (production_open_linux.go:528, :544, :698) and the named binders between
// them, plus each table those binders iterate - leaves these three unserved:
//
//	db:import-run-effect-receipt-commit
//	db:import-run-effect-receipt-prepare
//	db:import-run-prepare-inspection
//
// Five names left this table when they were served, and their rows are worth
// remembering because the reason they were refused was wrong:
//
//   - db:blueprint-character-sync-get and -list-pending were refused because
//     they repair as they read. The reference repairs as it reads too
//     (electron/repositories/blueprint-repository.ts:734 and :758 call the
//     insert-only ensureBlueprintCommitSchema at :176-194, with a deterministic
//     key and OR IGNORE), so mirroring it is fidelity: they are served now by
//     production_db_sync_reads_linux.go, and the repair is proven idempotent
//     there. GetSync still refuses a COMPLETED row with GENERATION_OWNER_REQUIRED,
//     because the authoritative-roster proof (:420) has no port here.
//   - db:review-cycle-list-pending-rechecks had "no store list"; one was written
//     (internal/store/reviewcycle_read_tx.go) and the name is served by
//     production_db_cycle_reads_linux.go. The reference's planRecheck classifier
//     is still unported, which can add a cycle to the answer but never drop one.
//   - db:project-core-commit-generated was refused because "the child-effect stamp
//     it must commit with has no writer or shared transaction". The transaction
//     half was wrong: internal/store/dbmaintenance.go:126 is one reserved
//     connection, BEGIN IMMEDIATE and a bounded rollback, and the new store port
//     reuses it. The stamp half was a no-op for every run this lane can begin -
//     the reference's own withChildEffect is `if (!id) return effect()`
//     (electron/services/agent-generation.ts:349) for a child with no
//     agentWorkflowRegistrationId, and generation:begin cannot mint that key. It
//     is served by production_project_core_generated_linux.go.
//   - db:blueprint-character-sync-complete was refused because "no store method
//     writes the completion receipt; the roster proof it needs does not exist".
//     The proof did exist, in parts: the roster read that answers status,
//     revision, projectionHash and entries was already the store's own
//     charactersRoster (character_roster.go:466), and only the fact proof
//     (blueprint-character-sync-evidence.ts:63-91), the
//     character_roster_operations evidence check and the receipt writer were
//     missing. They are internal/store/blueprint_sync_complete.go, one
//     BEGIN IMMEDIATE on the borrowed handle, and the name is served by
//     production_db_sync_complete_linux.go.
//
// The complement is asserted, not assumed: TestProductionLaneDBResidualIsTheUnservedSlice
// takes the bound set from the finished lane (transport.Server.BoundOperations),
// takes the `db:` names from the registry the same lane loaded, and requires
// bound+residual to cover the slice exactly, in both directions.
//
// # Why none of the three is bound here
//
// None of them is a reference refusal this lane could publish as its own. Two
// names of that kind do exist and are served elsewhere rather than here:
// db:blueprint-update-notes and db:plot-tree-save are the original controller's
// own deliberate refusals (electron/controllers/db-controller.ts:636, :886), so
// serving them would be a divergence, not a migration
// (production_reference_refusals_linux.go).
//
// The three names below each have a narrower reason of its own:
//
//   - db:import-run-effect-receipt-prepare and -commit: store.ImportRuns
//     .PrepareEffectReceipt and .CommitEffectReceipt exist
//     (internal/store/importrun_receipts.go:1152, :1259) and both take an
//     ImportRunGenerationSourceAssert (:118). The controller always supplies one
//     and it proves the handle against live in-process runs
//     (electron/controllers/db-controller.ts:425, :437); this repository has no
//     such proof, and the two store guards disagree about a nil one - prepare
//     makes the handle optional (:984) while commit refuses it outright
//     (:1003, GENERATION_GUARD_REQUIRED). A nil proof would write receipts this
//     lane could never commit; a non-nil proof that fails closed refuses every
//     real request twice over.
//   - db:import-run-prepare-inspection: the inspection store is unreachable from
//     any binder but the picker's own closure, the renderer's preceding
//     author-preview step still refuses the id form, and the author prepare has
//     no store method. The preview CAS is NOT the gap: it is a server-side
//     handoff the store itself can prove (see
//     production_importrun_prepare_inspection_linux.go).
//
// # Binder
//
// bindProductionDBResidual installs nothing, so wiring it moves no count. It is
// wired next to the other seam binders, and what that buys is the seam check
// running inside the real lane instead of only inside this test:
//
//	func() error { return bindProductionDBResidual(server) },
//
// When a port above lands, the handler is installed with server.Bind on the
// name's own row, with the arity and path index this table already records, so
// the wire shape cannot drift from the renderer's call site.

// The names, spelled once each so the binder, the seam table and the
// test cannot disagree with the controller that registers them.
const (
	productionDBResidualBlueprintClearAll     = "db:blueprint-clear-all"
	productionDBResidualGlobalFactsCommit     = "db:import-global-facts-commit"
	productionDBResidualReceiptCommit         = "db:import-run-effect-receipt-commit"
	productionDBResidualReceiptPrepare        = "db:import-run-effect-receipt-prepare"
	productionDBResidualPrepareInspection     = "db:import-run-prepare-inspection"
	productionDBResidualProjectClearGenerated = "db:project-clear-generated-data"
	productionDBResidualInventoryPrefix       = "db:"
)

// productionDBResidualLine is one declaration that has to move before a name can
// be served: the path of the file, the line inside it, and a marker that must be
// somewhere in that file. TestProductionLaneRefusesDBResidual re-reads every one
// of them, so a seam that gets deleted or reworded fails this lane's test
// instead of leaving a note that points at nothing.
type productionDBResidualLine struct {
	Path   string
	Line   int
	Marker string
}

// productionDBResidualSeam is one refused name: the wire shape the renderer
// actually sends, the symbolic code the reference's own handler would publish if
// it refused, the missing port in one sentence, and the declarations that keep
// it missing.
//
// Arity counts every wire argument including the frozen three-key context, which
// is always the last one and never inside an input object; PathIndex is the
// argument that must BE the frozen project path. Both are read off
// src/shared/ipc-channels.ts for the channel and cross-checked against the
// renderer call site named in each row.
type productionDBResidualSeam struct {
	Operation string
	Arity     int
	PathIndex int
	Gate      string
	Port      string
	Missing   []productionDBResidualLine
}

// productionDBResidualLineOf is the only constructor of a seam line. It exists so
// a seam row is a list of one-line declarations: a keyed literal per marker would
// put a comment inside an aligned field block, and the alignment would then be the
// only thing deciding whether this file is formatted.
func productionDBResidualLineOf(path string, line int, marker string) productionDBResidualLine {
	return productionDBResidualLine{Path: path, Line: line, Marker: marker}
}

// productionDBResidualSeams is the whole answer of this file. Every row names a
// port that does not exist (or a store function that cannot be used as it is),
// never a handler this lane could install today.
func productionDBResidualSeams() []productionDBResidualSeam {
	return []productionDBResidualSeam{
		{
			Operation: productionDBResidualPrepareInspection,
			Arity:     productionImportRunPrepareInspectionArity,
			PathIndex: productionImportRunPrepareInspectionPathIndex,
			Gate:      productionImportRunPrepareInspectionGate,
			Port:      productionImportRunPrepareInspectionPort,
			Missing:   productionImportRunPrepareInspectionResidualMissing(),
		},
	}
}

// productionDBResidualOperations is the family's three residual names in the order
// the seam table lists them, so the binder and the test walk one list.
func productionDBResidualOperations() []string {
	return []string{
		productionDBResidualPrepareInspection,
	}
}

// productionDBResidualSeamFor returns the row for one name. A name the table does
// not list has no seam, which is how a typo refuses instead of publishing a
// handler nobody described.
func productionDBResidualSeamFor(operation string) (productionDBResidualSeam, bool) {
	for _, seam := range productionDBResidualSeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionDBResidualSeam{}, false
}

// productionDBResidualIsDB tells the completeness check which registry names are
// this file's business. It is the only prefix rule in the file.
func productionDBResidualIsDB(operation string) bool {
	return len(operation) > len(productionDBResidualInventoryPrefix) &&
		operation[:len(productionDBResidualInventoryPrefix)] == productionDBResidualInventoryPrefix
}

// bindProductionDBResidual installs nothing, because every name it covers is
// still missing the port its seam row names. What it does is refuse to start on
// an incomplete seam: a row without a port, without the wire shape the renderer
// sends, or with a path index inside the frozen context would be a note that
// points at nothing, and a duplicate name would be a table that lies about its
// size.
func bindProductionDBResidual(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	seen := make(map[string]bool, len(productionDBResidualOperations()))
	for _, operation := range productionDBResidualOperations() {
		if !productionDBResidualIsDB(operation) {
			return errors.New("PRODUCTION_DB_RESIDUAL_NAME_INVALID")
		}
		if seen[operation] {
			return errors.New("PRODUCTION_DB_RESIDUAL_NAME_DUPLICATE")
		}
		seen[operation] = true
		seam, found := productionDBResidualSeamFor(operation)
		if !found {
			return errors.New("PRODUCTION_DB_RESIDUAL_SEAM_MISSING")
		}
		// The frozen three-key context is the last wire argument and is never the
		// path, so a real row always has at least one payload slot before it.
		if seam.Arity < 2 || seam.PathIndex < 0 || seam.PathIndex > seam.Arity-2 {
			return errors.New("PRODUCTION_DB_RESIDUAL_SHAPE_INVALID")
		}
		if seam.Port == "" || len(seam.Missing) == 0 {
			return errors.New("PRODUCTION_DB_RESIDUAL_SEAM_INCOMPLETE")
		}
		if seam.Gate != "" && !productionDBResidualSymbolic(seam.Gate) {
			return errors.New("PRODUCTION_DB_RESIDUAL_GATE_INVALID")
		}
	}
	return nil
}

// productionDBResidualSymbolic reports whether a code is the uppercase,
// underscore-separated shape every refusal in this lane is published under. It is
// a shape check, not a vocabulary: the seam table is where the vocabulary lives.
func productionDBResidualSymbolic(code string) bool {
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
