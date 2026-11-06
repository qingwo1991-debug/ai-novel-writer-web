//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the db family's residual: the thirteen inventoried `db:*` names
// this lane answers at the fail-closed 501 GO_OPERATION_NOT_MIGRATED
// (internal/transport/server.go:116) instead of serving, and the exact code that
// has to move before any of them can be installed as a handler.
//
// # What this batch measured
//
// The inventory is 281 names (web/operations.json). This lane installs 231 of
// them (production_served_count_linux_test.go:23) and 103 of the 281 are `db:*`,
// so the db slice this file is about is 103 wide. Reading every binder the lane
// really calls - the three `for _, bind := range []func() error{` lists
// (production_open_linux.go:528, :544, :698) and the named binders between them,
// plus each table those binders iterate - puts 90 of the 103 in the served set and
// leaves these 13:
//
//	db:blueprint-character-sync-complete      db:import-run-prepare-inspection
//	db:blueprint-character-sync-get           db:plot-tree-save
//	db:blueprint-character-sync-list-pending  db:project-clear-generated-data
//	db:blueprint-clear-all                    db:project-core-commit-generated
//	db:blueprint-update-notes                 db:review-cycle-list-pending-rechecks
//	db:import-global-facts-commit             db:import-run-effect-receipt-prepare
//	                                          db:import-run-effect-receipt-commit
//
// The complement is asserted, not assumed: TestProductionLaneDBResidualIsTheUnservedSlice
// takes the bound set from the finished lane (transport.Server.BoundOperations),
// takes the `db:` names from the registry the same lane loaded, and requires
// bound+residual to cover the slice exactly, in both directions.
//
// # Why none of the thirteen is bound here
//
// This batch was asked to look for the cheap shape - a store function that
// already exists with no provider method in front of it - and then to bind it.
// Seven of the thirteen have no store function at all: store.Blueprints has none
// of clearAll, completeCharacterSyncOperation or updateNotes, store.ProjectCore
// has no CommitGenerated, no store file names ImportGlobalFactsRepository.commit,
// no store file names ProjectClearRepository.clearGeneratedData, and the
// review-cycle family has exactly one read (ReadAbsentReviewCycleTx) and no
// listPendingRechecks. Writing any of them would mean inventing the semantics it
// carries - a completion receipt, an authoritative-roster proof, a child-effect
// stamp, a multi-table destructive boundary - which this batch may not invent.
//
// The other six DO have a store function behind them, and each is refused for a
// narrower reason of its own, recorded in its seam row below:
//
//   - db:blueprint-character-sync-get and -list-pending: Blueprints.GetSync and
//     PendingSync exist, and both execute the reference's own lazy repair, one
//     `INSERT OR IGNORE` into blueprint_character_sync_operations, before they
//     select (internal/store/blueprints.go:137, the statement at :165 and :170).
//     The only borrow that can hold that statement is the writable
//     borrowAuthorDatabase (production_durable_linux.go:313), so binding either
//     name would let a read mutate the author's database - the one thing the
//     read fence of this lane does not do, and the one thing a test can prove
//     only by giving it up. GetSync additionally refuses a completed row for want
//     of the authoritative-roster proof (internal/store/blueprints.go:178), so
//     the row the recovery flow actually reads - the completed one
//     (src/services/workflows/directory-character-sync-recovery.ts:53-60) - could
//     not be answered at all.
//
//   - db:import-run-effect-receipt-prepare and -commit: store.ImportRuns
//     .PrepareEffectReceipt and .CommitEffectReceipt exist
//     (internal/store/importrun_receipts.go:1152, :1259) and both take an
//     ImportRunGenerationSourceAssert (:118). The controller always supplies one
//     and it proves the handle against live in-process runs
//     (electron/controllers/db-controller.ts:425, :437); this repository has no
//     such proof, and the two store guards disagree about a nil one - prepare
//     merely makes the handle optional (:984) while commit refuses the handle
//     outright (:1003, GENERATION_GUARD_REQUIRED). A nil proof therefore writes
//     receipts this lane could never commit; a non-nil proof that fails closed
//     refuses every real request twice over. production_importrun_residual_linux.go
//     reaches the same conclusion and keeps them refused there.
//
//   - db:plot-tree-save: store.PlotTree.Save exists, but the reference refuses
//     the renderer's write unconditionally
//     (electron/controllers/db-controller.ts:886-895, GENERATION_GRAPH_ADMISSION_REQUIRED),
//     so there is nothing to wire: a handler here could only restate that
//     refusal, and a name that can only refuse stays unbound on purpose. Its
//     family siblings -read and -clear are served
//     (production_open_linux.go:729, :753).
//
//   - db:import-run-prepare-inspection: the inspection store and the parsing
//     writes both exist (internal/importsource/inspection.go:158, :165;
//     internal/store/importrun_lifecycle.go:796, :1048), but the reference's handler
//     starts
//     by consuming an inspection id and its preview CAS, and this lane never puts
//     that CAS on the wire (internal/transport/import_selection.go:110-116). So
//     the id a request carries cannot reach the chapters BeginParsing requires,
//     which is the same gap production_importrun_residual_linux.go:36-52 records
//     for the author-preview sibling.
//
// # Binder
//
// bindProductionDBResidual installs nothing, so wiring it moves no count: the
// served set stays at productionServedOperations
// (production_served_count_linux_test.go:23). It is wired the way the
// finalization and agent-generation slices are - one entry in the binder list that
// starts at production_open_linux.go:698, next to bindProductionAgentGeneration
// (production_open_linux.go:708) - and what that buys is the seam check running
// inside the real lane instead of only inside this test:
//
//	func() error { return bindProductionDBResidual(server) },
//
// When a port above lands, the handler is installed with server.Bind on the
// name's own row, with the arity and path index this table already records, so
// the wire shape cannot drift from the renderer's call site.

// The thirteen names, spelled once each so the binder, the seam table and the
// test cannot disagree with the controller that registers them.
const (
	productionDBResidualBlueprintSyncComplete = "db:blueprint-character-sync-complete"
	productionDBResidualBlueprintSyncGet      = "db:blueprint-character-sync-get"
	productionDBResidualBlueprintSyncPending  = "db:blueprint-character-sync-list-pending"
	productionDBResidualBlueprintClearAll     = "db:blueprint-clear-all"
	productionDBResidualBlueprintUpdateNotes  = "db:blueprint-update-notes"
	productionDBResidualGlobalFactsCommit     = "db:import-global-facts-commit"
	productionDBResidualReceiptCommit         = "db:import-run-effect-receipt-commit"
	productionDBResidualReceiptPrepare        = "db:import-run-effect-receipt-prepare"
	productionDBResidualPrepareInspection     = "db:import-run-prepare-inspection"
	productionDBResidualPlotTreeSave          = "db:plot-tree-save"
	productionDBResidualProjectClearGenerated = "db:project-clear-generated-data"
	productionDBResidualProjectCoreCommitGen  = "db:project-core-commit-generated"
	productionDBResidualPendingReviewRechecks = "db:review-cycle-list-pending-rechecks"
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
			// The reference serves this read. The gate recorded here is this port's
			// own refusal, raised for a completed row it cannot prove.
			Operation: productionDBResidualBlueprintSyncGet,
			Arity:     3,
			PathIndex: 1,
			Gate:      "GENERATION_OWNER_REQUIRED",
			Port:      "store.Blueprints.GetSync exists but repairs as it reads, and refuses a completed row",
			Missing:   []productionDBResidualLine{
				// The repair is one DML statement the reference also runs inside its
				// read (ensureBlueprintCommitSchema), so it is faithful and still a
				// write: no read-only borrow can carry it.
				productionDBResidualLineOf("../../internal/store/blueprints.go", 137, "INSERT OR IGNORE INTO blueprint_character_sync_operations("),
				productionDBResidualLineOf("../../internal/store/blueprints.go", 178, `if row!=nil&&row["status"]=="completed"{return nil,ErrGenerationOwnerRequired}`),
				productionDBResidualLineOf("../../../electron/repositories/blueprint-repository.ts", 755, "static getCharacterSyncOperation(operationId: string)"),
				// And the proof that would make a completed row readable, which is the
				// same missing owner.
				productionDBResidualLineOf("../../../electron/repositories/blueprint-repository.ts", 420, "function assertAuthoritativeCharacterSyncCompletion("),
			},
		},
		{
			Operation: productionDBResidualBlueprintSyncPending,
			Arity:     2,
			PathIndex: 0,
			Port:      "store.Blueprints.PendingSync exists but repairs as it reads",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../internal/store/blueprints.go", 165, "func (b *Blueprints) PendingSync(ctx context.Context)"),
				productionDBResidualLineOf("../../internal/store/blueprints.go", 137, "INSERT OR IGNORE INTO blueprint_character_sync_operations("),
				// The transport already says why this read cannot be published as one:
				// the binder it belongs to is not the one this lane can take.
				productionDBResidualLineOf("../../internal/transport/blueprints_read.go", 62, "A read that writes"),
				productionDBResidualLineOf("../../../electron/repositories/blueprint-repository.ts", 732, "static listPendingCharacterSyncOperations()"),
			},
		},
		{
			Operation: productionDBResidualBlueprintSyncComplete,
			Arity:     3,
			PathIndex: 1,
			Gate:      "BLUEPRINT_CHARACTER_SYNC_ROSTER_NOT_READY",
			Port:      "no store method writes the completion receipt; the roster proof it needs does not exist",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../../electron/repositories/blueprint-repository.ts", 764, "static completeCharacterSyncOperation("),
				productionDBResidualLineOf("../../../electron/repositories/blueprint-repository.ts", 377, "BLUEPRINT_CHARACTER_SYNC_ROSTER_NOT_READY"),
				productionDBResidualLineOf("../../internal/transport/blueprints.go", 15, "BindBlueprints deliberately excludes clear-all and completion of character sync"),
			},
		},
		{
			Operation: productionDBResidualBlueprintClearAll,
			Arity:     2,
			PathIndex: 0,
			Port:      "no store method deletes every blueprint row; Blueprints.Delete takes one chapter key",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../../electron/repositories/blueprint-repository.ts", 806, "static clearAll(): void"),
				productionDBResidualLineOf("../../internal/store/blueprints.go", 129, "func (b *Blueprints) Delete(ctx context.Context,chapter any)"),
				productionDBResidualLineOf("../../internal/transport/blueprints.go", 15, "BindBlueprints deliberately excludes clear-all and completion of character sync"),
			},
		},
		{
			Operation: productionDBResidualBlueprintUpdateNotes,
			Arity:     4,
			PathIndex: 2,
			Gate:      "GENERATION_FINALIZATION_ADMISSION_REQUIRED",
			Port:      "no store method updates chapter notes; the reference gate needs the artifact/ACK transaction",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../../electron/controllers/db-controller.ts", 636, "operations.handle('db:blueprint-update-notes', async ("),
				productionDBResidualLineOf("../../internal/transport/blueprints.go", 26, `"db:blueprint-update-notes":return fail(errors.New("GENERATION_FINALIZATION_ADMISSION_REQUIRED"))`),
				// The transaction the notes would have to ride: artifact and ACK under
				// one owner proof, which is what the finalization outbox owns.
				productionDBResidualLineOf("../../internal/store/finalization_outbox.go", 228, "func (r *FinalizationOutbox) CommitWithOwnerProof("),
			},
		},
		{
			Operation: productionDBResidualGlobalFactsCommit,
			Arity:     3,
			PathIndex: 1,
			Port:      "no store function implements ImportGlobalFactsRepository.commit",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../../electron/repositories/import-global-facts-repository.ts", 162, "static commit(candidate: ImportGlobalFactsRequest"),
				productionDBResidualLineOf("../../internal/store/importrun_receipts.go", 50, "project-global-facts"),
				productionDBResidualLineOf("../../../electron/controllers/db-controller.ts", 236, "operations.handle('db:import-global-facts-commit', async ("),
			},
		},
		{
			Operation: productionDBResidualReceiptPrepare,
			Arity:     4,
			PathIndex: 2,
			Gate:      "GENERATION_IMPORT_EFFECT_HANDLE_REQUIRED",
			Port:      "store.ImportRuns.PrepareEffectReceipt needs a generation-source proof this module does not have",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../internal/store/importrun_receipts.go", 1153, "ImportRunPrepareEffectReceiptRequest, assertSources"),
				productionDBResidualLineOf("../../internal/store/importrun_receipts.go", 118, "type ImportRunGenerationSourceAssert func"),
				// A nil proof only makes the handle optional, so prepare would accept a
				// binding commit then refuses.
				productionDBResidualLineOf("../../internal/store/importrun_receipts.go", 984, "GENERATION_IMPORT_EFFECT_HANDLE_REQUIRED"),
				productionDBResidualLineOf("../../../electron/controllers/db-controller.ts", 418, "operations.handle('db:import-run-effect-receipt-prepare', async ("),
			},
		},
		{
			Operation: productionDBResidualReceiptCommit,
			Arity:     6,
			PathIndex: 4,
			Gate:      "GENERATION_GUARD_REQUIRED",
			Port:      "store.ImportRuns.CommitEffectReceipt needs the same proof, and refuses a handle without it",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../internal/store/importrun_receipts.go", 1260, "execution ImportRunExecution, assertSources ImportRunGenerationSourceAssert"),
				productionDBResidualLineOf("../../internal/store/importrun_receipts.go", 1003, "GENERATION_GUARD_REQUIRED"),
				productionDBResidualLineOf("../../../electron/controllers/db-controller.ts", 428, "operations.handle('db:import-run-effect-receipt-commit', async ("),
			},
		},
		{
			Operation: productionDBResidualPrepareInspection,
			Arity:     3,
			PathIndex: 1,
			Gate:      "IMPORT_INSPECTION_OWNER_UNAVAILABLE",
			Port:      "no inspection owner: the inspected store's consume CAS is never put on the wire",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../../electron/controllers/db-controller.ts", 262, "operations.handle('db:import-run-prepare-inspection', async ("),
				productionDBResidualLineOf("../../internal/importsource/inspection.go", 165, "func (s *Store) Consume(receipt,expectedPreviewCAS string,binding Binding)"),
				productionDBResidualLineOf("../../internal/store/importrun_lifecycle.go", 796, "func (r *ImportRuns) BeginParsing(ctx context.Context, candidate ImportRunBeginParsingRequest)"),
				productionDBResidualLineOf("../../internal/store/importrun_lifecycle.go", 1048, "func (r *ImportRuns) CommitParsedSource(ctx context.Context, runID, sourceID string, chapters []ImportRunChapterInput)"),
				// The only place this lane mints an inspection id, and the id it never
				// publishes back to a later prepare.
				productionDBResidualLineOf("../../internal/transport/import_selection.go", 112, `"inspectionId": summary.InspectionID`),
			},
		},
		{
			Operation: productionDBResidualPlotTreeSave,
			Arity:     4,
			PathIndex: 2,
			Gate:      "GENERATION_GRAPH_ADMISSION_REQUIRED",
			Port:      "the reference refuses every renderer write, so the binder could only restate that refusal",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../../electron/controllers/db-controller.ts", 886, "operations.handle('db:plot-tree-save', async ("),
				productionDBResidualLineOf("../../internal/transport/plottree.go", 22, `name=="db:plot-tree-save"{return fail(errors.New("GENERATION_GRAPH_ADMISSION_REQUIRED"))}`),
				productionDBResidualLineOf("../../internal/transport/plottree_read.go", 16, "db:plot-tree-save stays unbound because the original refuses it"),
			},
		},
		{
			Operation: productionDBResidualProjectClearGenerated,
			Arity:     3,
			PathIndex: 1,
			Port:      "no store function implements ProjectClearRepository.clearGeneratedData",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../../electron/repositories/project-clear-repository.ts", 87, "static clearGeneratedData(options: ProjectClearOptions"),
				productionDBResidualLineOf("../../../electron/controllers/db-controller.ts", 245, "operations.handle('db:project-clear-generated-data', async ("),
			},
		},
		{
			Operation: productionDBResidualProjectCoreCommitGen,
			Arity:     3,
			PathIndex: 1,
			Gate:      "GENERATION_OWNER_REQUIRED",
			Port:      "store.ProjectCore.Update exists, but the child-effect stamp it must commit with has no writer or shared transaction",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../../electron/controllers/db-controller.ts", 204, "database.transaction(() => withGenerationAgentChildEffect(request.generationRunHandle, () => {"),
				productionDBResidualLineOf("../../../electron/controllers/generation-controller.ts", 67, "export function withGenerationAgentChildEffect"),
				// The one store write the transaction would carry, on its own
				// transaction handle: there is no seam to join a second write to.
				productionDBResidualLineOf("../../internal/store/projectcore.go", 156, "func (r *ProjectCore) Update(ctx context.Context, data map[string]any) error"),
			},
		},
		{
			Operation: productionDBResidualPendingReviewRechecks,
			Arity:     2,
			PathIndex: 0,
			Port:      "no store read: the review-cycle family has only the absent-cycle read, not the pending-recheck projection",
			Missing:   []productionDBResidualLine{
				productionDBResidualLineOf("../../../electron/repositories/review-cycle-repository.ts", 505, "static listPendingRechecks(db: Database.Database)"),
				productionDBResidualLineOf("../../internal/store/reviewcycle_read_tx.go", 12, "REVIEW_CYCLE_PROJECTION_UNAVAILABLE"),
				productionDBResidualLineOf("../../internal/store/reviewcycle_read_tx.go", 17, "func ReadAbsentReviewCycleTx("),
				productionDBResidualLineOf("../../internal/transport/editor_review_cycle_read.go", 30, "recheck projection"),
			},
		},
	}
}

// productionDBResidualOperations is the family's thirteen names in the order the
// seam table lists them, so the binder and the test walk one list.
func productionDBResidualOperations() []string {
	return []string{
		productionDBResidualBlueprintSyncGet,
		productionDBResidualBlueprintSyncPending,
		productionDBResidualBlueprintSyncComplete,
		productionDBResidualBlueprintClearAll,
		productionDBResidualBlueprintUpdateNotes,
		productionDBResidualGlobalFactsCommit,
		productionDBResidualReceiptPrepare,
		productionDBResidualReceiptCommit,
		productionDBResidualPrepareInspection,
		productionDBResidualPlotTreeSave,
		productionDBResidualProjectClearGenerated,
		productionDBResidualProjectCoreCommitGen,
		productionDBResidualPendingReviewRechecks,
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
