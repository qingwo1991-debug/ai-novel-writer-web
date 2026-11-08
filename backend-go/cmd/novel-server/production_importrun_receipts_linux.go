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
// incomplete or disagrees with the serve decision
// (production_importrun_receipts_serve_linux.go), so the real lane runs the check
// at startup instead of leaving a note that points at nothing.
//
//	db:import-run-effect-receipt-prepare   refused, the proof has no connection inside the receipt
//	db:import-run-effect-receipt-commit    refused, the same proof and the same connection
//	db:import-run-prepare-inspection       refused, the inspection store is unreachable
//
// # What the receipt assert actually checks
//
// store.ImportRuns.PrepareEffectReceipt and CommitEffectReceipt accept a
// store.ImportRunGenerationSourceAssert (internal/store/importrun_receipts.go:118,
// documented at :112-118). The reference always supplies one
// (electron/controllers/db-controller.ts:425 for prepare, :437 for commit). The
// proof it performs there is five checks, and it is worth stating them precisely,
// because the previous version of this comment got two of them wrong:
//
//  1. the project database must be in a transaction and the live owner for it
//     must still be installed (electron/controllers/generation-controller.ts:72-75,
//     GENERATION_OWNER_REQUIRED). In the reference that owner is a DISPATCHER: it
//     routes to the closure that holds the source dependencies.
//  2. the handle must name a generation_runs row whose projectId matches and
//     whose own {projectId, epoch, rootActionId, runId} equals the handle
//     (electron/services/main-generation-owner.ts:156-167, requireRun called with
//     execution=false). Note what that means: the epoch comparison in requireRun
//     is gated on `execution` (:164) and the assert never passes it, so the
//     assert does NOT compare the run's epoch with the live one. "The assert
//     needs a live lease check" was never true.
//  3. the run's frozen import context must name exactly the requested
//     {runId, stage, batchId} slot (electron/services/main-generation-owner.ts:1462,
//     GENERATION_IMPORT_EFFECT_SLOT_CHANGED).
//  4. the run's stored binding must still equal a binding rebuilt from the
//     CURRENT author sources and the CURRENT model receipt
//     (electron/services/main-generation-owner.ts:1463-1465,
//     generation-source-binding.ts:614-625,
//     GENERATION_IMPORT_EFFECT_SOURCE_CHANGED). That receipt is derived from the
//     current model PROFILE (main-generation-owner.ts:303-307), not from a lease.
//  5. the root must not be cancelled, at least one attempt must exist, every
//     attempt must have finished with stop, the artifact must carry non-blank text
//     whose hash equals the recorded textHash, and the artifact must not be
//     discarded (electron/services/main-generation-owner.ts:1465-1471).
//
// # What this pass re-tested, and what it found
//
// The batch asked whether steps 2 to 5 can now be built from the store's own read
// plus generationowner.ReproduceBinding, so that prepare and commit could be
// served. They still cannot, but not for the reasons this file used to give. Both
// premises the earlier refusal rested on are dead:
//
//   - Step 4's reproduction exists. internal/generationowner/binding.go:63
//     ReproduceBinding rebuilds the binding from the CURRENT sources under the
//     run's own scope and epoch, and internal/generationowner/lease.go:164
//     resolves its record without the live-session currency check, so the
//     source-currency half has a Go implementation.
//   - Step 2 is no longer a capability gap either.
//     internal/store/character_proposal_pending.go:545 already reads
//     "FROM generation_runs WHERE run_id=?" for a generationRunHandle taken out of
//     an import_run_receipts payload, and :546-575 compares projectId,
//     rootActionId, the operation and the whole {runId,stage,batchId} importSlot
//     against that row. The old wording - "none reads generation_runs, binding_json
//     or an importSlot" - was only ever true of internal/store/importrun.go and
//     internal/store/importrun_lifecycle.go.
//
// What still blocks the pair is no longer the reproduction's implementation,
// its inputs or the existence of a run. It is two things, and the change that
// serves these names has to do both:
//
//	a. WIRING. The owner is a bound identifier at the call site: the block opens
//	   at production_open_linux.go:591 and this family's binder is constructed
//	   from inside it at :627, with `open.durable` alone. The binder therefore
//	   does NOT have to move into the owner block - it is already there - it has
//	   to be HANDED the owner: one argument at :627, then one more at
//	   production_importrun_residual_linux.go:247. The loader it needs closes
//	   over the private model list (production_generation_begin_linux.go:133-134)
//	   and cannot be rebuilt from a durable open. That line is in a file the pass
//	   that wrote production_importrun_receipts_owner_linux.go may not edit, so
//	   the shipped lane cannot be served from here - and an owner-bound binder no
//	   lane constructs is the dead code production_importrun_residual_linux.go
//	   refuses to publish.
//	b. THE CONNECTION. The assert runs inside the receipt's BEGIN IMMEDIATE on
//	   the author database's only connection
//	   (internal/store/importrun_receipts.go:987 and :1005, from
//	   importRunImmediate at internal/store/importrun.go:948;
//	   internal/projectfs/author_linux.go:121 opens the pool with
//	   SetMaxOpenConns(1)). The reference may read there because its owner is a
//	   synchronous dispatcher over that same connection, and the controller
//	   refuses the call unless the database is already in a transaction
//	   (electron/controllers/generation-controller.ts:74). A Go callback has no
//	   such access: the type at internal/store/importrun_receipts.go:118 carries
//	   (handle, slot) and nothing else, so its first read - requireRun's, at
//	   electron/services/main-generation-owner.ts:1454 - waits for the connection
//	   the transaction is holding. internal/store/character_proposal_pending.go:37
//	   already names that shape: a deadlock rather than a proof. Store-native
//	   composition of a receipt and a projection on that connection is proven
//	   (:27-37), but a fingerprint reproduction is not a projection: it needs the
//	   run's binding_json, its root's status and its stopped attempts' artifacts,
//	   and the store hands none of them to the callback.
//
// The third premise - that no import-bound generation run can exist - is dead as
// well. The ledger admits importSlot, importSlotKey and importContext for the two
// stage operations (internal/store/generation_ledger.go's glFamilyManifestAllowed
// import case), generation:begin freezes all three
// (production_import_generation_serve_linux.go:288 and :429), and the probe at
// production_import_finalization_generation_linux_test.go:326-364 now asserts that
// acceptance instead of a refusal.
// production_importrun_receipts_owner_linux_test.go begins such a run through the
// frozen session and reads its manifest back.
//
// Step 3's other half is not missing either:
// electron/services/import-generation-source.ts:20 captureImportGenerationContext
// is productionImportGenerationFreeze
// (production_import_generation_serve_linux.go:429), which freezes the slot, its
// key and the context together and refuses a slot the import run no longer owns.
//
// # What the store's own live-run read does supply
//
// The import run's own read is written down here so the boundary is explicit. The
// store owns import_runs, and the receipt transaction reads the live run:
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
//	                                  the parsing and lifecycle writes, over
//	                                  import_runs, import_run_sources,
//	                                  import_run_chapters and import_effect_ledger.
//
// The only reduced source proof already in the store is dcAssertSources
// (internal/store/draft_commit_tx.go:272-288). It compares a generation binding's
// epoch with a live epoch supplied by a generation owner and checks the root is
// not cancelled; its own comment says the rebuildBinding comparison is out of
// scope. It is bound to the draft lane's bpRun, not to an import-effect handle,
// and this lane is never handed an owner to obtain a live epoch from. Copying it
// here would prove less than the reference while pretending to prove the same
// thing.
//
// # Why nil, fail-closed and a database-only proof are all wrong answers
//
// The two store guards disagree about a nil proof:
//
//	prepare  importrun_receipts.go:983-985  a nil proof only makes the handle
//	                                        optional, so an unproven handle is
//	                                        accepted and written;
//	commit   importrun_receipts.go:1000-1004 a handle without a proof is refused
//	                                        outright with GENERATION_GUARD_REQUIRED.
//
// A nil proof would therefore write receipts this lane could never commit. A
// non-nil proof that fails closed refuses every real request twice over. A
// database-only proof that accepts a matching row would omit step 4 and accept a
// stale-source receipt. None of the three is a served operation.
//
// The narrowed kinds do not rescue the pair either. commitEffectReceipt implements
// only project-writing-style; the other three kinds return
// ErrImportRunReceiptKindUnbound (internal/store/importrun_receipts.go:1086-1096).
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
//   - importsource.Summary does carry the keyed CAS
//     (internal/importsource/inspection.go:49, `previewCAS`), but
//     Store.Peek returns only that Summary (:158) - never chapter bodies or
//     canonical identities - and Store.Consume demands the exact
//     expectedPreviewCAS (:165) and answers IMPORT_INSPECTION_PREVIEW_CHANGED
//     when it differs (:167);
//   - the only served response that carries an inspection id is
//     dialog:select-novel-files, and it returns inspectionId, purpose, counts,
//     display names, total words, total bytes and the eight-chapter preview, but
//     never previewCAS (internal/transport/import_selection.go:112). The value
//     the CAS exists to prove is therefore not something the handler can obtain
//     from the renderer, and peeking it server-side would hand back a digest the
//     renderer never saw;
//   - the inspection store is not reachable from this adapter: the picker owns
//     the only importsource.Store (production_import_selection_linux.go:173-180)
//     and the residual adapter is constructed with open.durable alone
//     (production_open_linux.go:627);
//   - a reference-purpose inspection cannot be minted at all:
//     dialog:select-novel-files refuses reference with
//     IMPORT_DURABLE_PARSING_REQUIRED (internal/transport/import_selection.go:81),
//     while BeginParsing refuses every purpose except reference
//     (internal/store/importrun_lifecycle.go:801-802). The only inspection this
//     lane can mint is therefore the author-purpose one, which BeginParsing
//     refuses; the author branch would additionally need the unported prepare()
//     and FinalizedDraftImportRepository.preview
//     (internal/store/importrun_lifecycle.go:26-34) plus
//     ImportSourceIdentityRepository.resolveEncodedSources, which needs the
//     application import-source secret.
//
// # The binder
//
// Bind installs nothing. It validates the seam table and refuses to start when a
// row loses its gate, its port, its arity, its path index, a declaration it names
// or a duplicate/foreign name, and when the table disagrees with the serve
// decision table (a refused name with no decision, or a name still listed here
// while the decision says it is served).
// production_importrun_residual_linux.go:229-249 installs the four served names
// and then calls this binder, so the check runs inside the real lane. The served
// count is unchanged at 273 because no name here is bound; the db family's
// residual table (production_db_residual_linux.go:153-215) still names the same
// three, and the count pin (production_served_count_linux_test.go:24) is
// deliberately untouched.

const (
	// The three operations are aliases of the residual file's own constants, so
	// the seam, the 501 pins, the serve decision and the residual prose can never
	// spell a name two different ways. A future pass that serves one of them has
	// to move the residual constant, the decision row and this seam together,
	// which is the point.
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
			Port:      "the proof has no connection: the assert is invoked inside the receipt's BEGIN IMMEDIATE, which holds the author database's only connection, and the callback type at importrun_receipts.go:118 carries neither a connection nor the run it must read; the owner that would supply the model-profile revision is in scope at the binder's call site but is not passed to it",
			Missing: []productionImportRunReceiptLine{
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 118, "type ImportRunGenerationSourceAssert func(handle any, slot ImportRunGenerationSlot) error"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 984, "GENERATION_IMPORT_EFFECT_HANDLE_REQUIRED"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 987, "return assertSources(handle, ImportRunGenerationSlot{RunID: runID, Stage: stage, BatchID: batchID})"),
				productionImportRunReceiptLineOf("../../internal/store/importrun.go", 121, "func readImportRunRow(ctx context.Context, q importRunQueryer, runID string) (importRunRow, bool, error)"),
				productionImportRunReceiptLineOf("../../internal/store/importrun.go", 641, "func importRunAssertExecutionAuthority(ctx context.Context, q importRunQueryer, runID string, execution ImportRunExecution, now int64) (importRunRow, error)"),
				productionImportRunReceiptLineOf("../../internal/store/importrun.go", 948, "func (r *ImportRuns) importRunImmediate(ctx context.Context, fn func(*sql.Conn) error) (retErr error)"),
				productionImportRunReceiptLineOf("../../internal/store/character_proposal_pending.go", 37, "which is a deadlock rather than a proof"),
				productionImportRunReceiptLineOf("../../internal/store/character_proposal_pending.go", 545, "FROM generation_runs WHERE run_id=?"),
				productionImportRunReceiptLineOf("../../internal/store/character_proposal_pending.go", 573, "ErrCharacterProposalPendingProofUnavailable"),
				productionImportRunReceiptLineOf("../../internal/generationowner/binding.go", 63, "func (r *Registry) ReproduceBinding("),
				productionImportRunReceiptLineOf("../../internal/generationowner/lease.go", 159, "// resolveReproduce is resolve minus the live-session currency check."),
				productionImportRunReceiptLineOf("../../internal/generationowner/lease.go", 163, "MODEL_EXECUTION_LEASE_INVALID"),
				productionImportRunReceiptLineOf("production_open_linux.go", 627, "newProductionImportRunResiduals(open.durable).Bind(server)"),
				productionImportRunReceiptLineOf("production_open_linux.go", 591, "if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {"),
				productionImportRunReceiptLineOf("production_generation_begin_linux.go", 134, "records, err := config.ListPrivateModels()"),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 425, "prepareEffectReceipt(request, execution, Date.now(), assertImportGenerationSourcesCurrent)"),
				productionImportRunReceiptLineOf("../../../electron/controllers/generation-controller.ts", 72, "assertImportGenerationSourcesCurrent(handle: MainGenerationRunHandle"),
				productionImportRunReceiptLineOf("../../../electron/services/main-generation-owner.ts", 1453, "assertImportGenerationSources: (handle: MainGenerationRunHandle"),
				productionImportRunReceiptLineOf("../../../electron/services/main-generation-owner.ts", 1462, "imports.context(run).slot"),
				productionImportRunReceiptLineOf("../../../electron/services/main-generation-owner.ts", 1463, "const current = deps.rebuildBinding(run.binding, currentModelReceipt(run.binding))"),
				productionImportRunReceiptLineOf("../../../electron/services/main-generation-owner.ts", 1471, "GENERATION_IMPORT_EFFECT_SOURCE_CHANGED"),
			},
		},
		{
			Operation: productionImportRunReceiptsCommitOperation,
			Arity:     6,
			PathIndex: 4,
			Gate:      "GENERATION_GUARD_REQUIRED",
			Port:      "the same proof and the same connection: a handle-carrying prepared receipt is refused without it, and only an already committed row replays before it",
			Missing: []productionImportRunReceiptLine{
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 1260, "execution ImportRunExecution, assertSources ImportRunGenerationSourceAssert"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 1004, "GENERATION_GUARD_REQUIRED"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 1306, "importRunReceiptAssertCommitGeneration(assertSources, runID, stage, batchID, receipt.Payload)"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 1086, "The three unbound kinds name a family this lane does not have"),
				productionImportRunReceiptLineOf("../../internal/store/importrun_receipts.go", 1095, "return nil, ErrImportRunReceiptKindUnbound"),
				productionImportRunReceiptLineOf("../../internal/generationowner/binding.go", 63, "func (r *Registry) ReproduceBinding("),
				productionImportRunReceiptLineOf("../../internal/generationowner/lease.go", 163, "MODEL_EXECUTION_LEASE_INVALID"),
				productionImportRunReceiptLineOf("production_open_linux.go", 627, "newProductionImportRunResiduals(open.durable).Bind(server)"),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 437, "(handle, slot) => assertImportGenerationSourcesCurrent(handle, slot, true)"),
				productionImportRunReceiptLineOf("../../../electron/services/main-generation-owner.ts", 1471, "GENERATION_IMPORT_EFFECT_SOURCE_CHANGED"),
			},
		},
		{
			Operation: productionImportRunReceiptsInspectionOperation,
			Arity:     3,
			PathIndex: 1,
			Gate:      "IMPORT_DURABLE_PARSING_REQUIRED",
			Port:      "the inspection store never reaches this adapter and the only served mint drops the preview CAS, so Consume's expectedPreviewCAS has no honest source",
			Missing: []productionImportRunReceiptLine{
				productionImportRunReceiptLineOf("../../internal/importsource/inspection.go", 49, "PreviewCAS string `json:\"previewCAS\"`"),
				productionImportRunReceiptLineOf("../../internal/importsource/inspection.go", 158, "func (s *Store) Peek(receipt string,binding Binding) (Summary,error)"),
				productionImportRunReceiptLineOf("../../internal/importsource/inspection.go", 165, "func (s *Store) Consume(receipt,expectedPreviewCAS string,binding Binding)"),
				productionImportRunReceiptLineOf("../../internal/importsource/inspection.go", 167, "IMPORT_INSPECTION_PREVIEW_CHANGED"),
				productionImportRunReceiptLineOf("../../internal/transport/import_selection.go", 81, "IMPORT_DURABLE_PARSING_REQUIRED"),
				productionImportRunReceiptLineOf("../../internal/transport/import_selection.go", 112, `"inspectionId": summary.InspectionID`),
				productionImportRunReceiptLineOf("../../internal/store/importrun_lifecycle.go", 801, `if candidate.Purpose != "reference"`),
				productionImportRunReceiptLineOf("../../internal/store/importrun_lifecycle.go", 1048, "func (r *ImportRuns) CommitParsedSource(ctx context.Context, runID, sourceID string, chapters []ImportRunChapterInput)"),
				productionImportRunReceiptLineOf("production_import_selection_linux.go", 173, "importsource.Store"),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 262, "operations.handle('db:import-run-prepare-inspection', async ("),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 269, "importInspectionStore.peek(request.inspectionId, webContentsId, request.purpose)"),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 282, "importInspectionStore.consume(request.inspectionId, webContentsId)"),
				productionImportRunReceiptLineOf("../../../electron/controllers/db-controller.ts", 283, "ImportSourceIdentityRepository.resolveEncodedSources("),
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
// than the family's refusal, and when the serve decision and the seam disagree.
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
// table as a foreign operation.
func productionImportRunReceiptsIsFamily(operation string) bool {
	switch operation {
	case productionImportRunReceiptsPrepareOperation, productionImportRunReceiptsCommitOperation,
		productionImportRunReceiptsInspectionOperation:
		return true
	}
	return false
}
