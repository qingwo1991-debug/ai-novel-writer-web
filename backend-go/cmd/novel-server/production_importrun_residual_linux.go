//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's RESIDUAL slice of the `db:import-run-*`
// family in web/operations.json (18 names). It publishes the four names whose
// whole capability the shipped store really has, and nothing else:
//
//	db:import-run-finalize-parsing        store.ImportRuns.FinalizeParsing  internal/store/importrun_lifecycle.go:1230
//	db:import-run-restart                 store.ImportRuns.Restart          internal/store/importrun_lifecycle.go:1489
//	db:import-run-fail                    store.ImportRuns.Fail             internal/store/importrun_lifecycle.go:1619
//	db:import-run-effect-receipt-get      store.ImportRuns.GetEffectReceipt internal/store/importrun_receipts.go:1106
//
// # Refused, with the capability that does not exist
//
// Four names stay at GO_OPERATION_NOT_MIGRATED. Each one is a capability gap, not
// deferred work, and the gap is named here so it can be closed deliberately.
//
//	db:import-run-effect-receipt-prepare   see "the generation-source proof" below
//	db:import-run-effect-receipt-commit    see "the generation-source proof" below
//	db:import-run-author-preview           needs FinalizedDraftImportRepository.preview, the
//	                                       finalized-draft preview/validation path the store
//	                                       deliberately left out
//	                                       (internal/store/importrun_lifecycle.go:26-34), plus the
//	                                       tab-bound inspection owner below.
//	db:import-run-prepare-inspection       needs a server-side handle-to-inspection handoff
//	                                       that hands back the parsed chapter bodies for one
//	                                       inspection id. importsource.Store.Peek returns
//	                                       metadata only and Consume demands the preview CAS
//	                                       (internal/importsource/inspection.go:158-169), and
//	                                       this lane deliberately never puts that CAS on the
//	                                       wire (internal/transport/import_selection.go:110-116),
//	                                       so a request carrying only inspectionId/runId
//	                                       (src/shared/import-run.ts:296-310) cannot reach the
//	                                       chapters BeginParsing/CommitParsedSource require.
//	                                       The reference's whole handler is consume ->
//	                                       resolveEncodedSources -> beginParsing ->
//	                                       commitParsedSource per source -> finalizeParsing
//	                                       (electron/controllers/db-controller.ts:284-355), and
//	                                       its first two steps have no counterpart bound in this
//	                                       lane at all: consume needs the tab-scoped CAS, and
//	                                       ImportSourceIdentityRepository.resolveEncodedSources
//	                                       needs the application import-source secret.
//	                                       The author purpose would additionally need the
//	                                       unported prepare() (:26-34), because BeginParsing
//	                                       refuses every purpose but 'reference' (:801-802).
//
// # The generation-source proof, and why both receipt writes stay unbound
//
// PrepareEffectReceipt and CommitEffectReceipt take an
// store.ImportRunGenerationSourceAssert (internal/store/importrun_receipts.go:118,
// documented at :112-118). The controller always supplies one
// (electron/controllers/db-controller.ts:425 for prepare, :437 for commit), and it
// proves the handle against the live in-process generation runs
// (electron/controllers/generation-controller.ts:72-75 ->
// electron/services/main-generation-owner.ts:1453-1472). This repository has no
// such proof: the only reduced substitute is the unexported dcAssertSources
// (internal/store/draft_commit_tx.go:272-288), which proves a draft-commit epoch
// and root, not an import-effect handle, and is bound to the draft lane's own run
// row.
//
// Passing nil is not the way out. The reference's real style receipt ALWAYS
// carries a generationRunHandle (electron/services/__tests__/import-generation.test.ts:197,
// and src/services/workflows/import-run-orchestrator.ts:444-449 commits the payload
// the generation run produced), and the two store guards then disagree:
//
//   - prepare with nil accepts a handle WITHOUT any proof
//     (importrun_receipts.go:984-989: a nil proof only makes the handle optional);
//   - commit with nil REFUSES a handle outright, with the store's own
//     GENERATION_GUARD_REQUIRED (importrun_receipts.go:1002-1003).
//
// So a nil proof lets this lane write a receipt that it could never commit, and
// accepts a generation binding the reference refuses. A non-nil proof that fails
// closed instead refuses every real request twice over. Neither is a binder that
// serves the operation, so both names keep answering 501 with this reason rather
// than being published as handlers that can only refuse or, worse, that can write
// unproven state. db:import-run-effect-receipt-get needs no proof at all (the
// reference has none there either), which is why it is bound below.
//
// # Why one server.Bind per name
//
// transport.BindImportRuns (internal/transport/import_run.go:122) is atomic across
// ten names and would collide with production_importrun_reads_linux.go, which
// already binds three of them, so startup would fail with OPERATION_ALREADY_BOUND.
// Each name is installed with server.Bind, the exported single-name binder
// (internal/transport/server.go:73-76), exactly the way
// production_importrun_reads_linux.go:164-178 and
// production_importrun_write_linux.go:215-232 install their own slices. Bind
// refuses an uninventoried name (OPERATION_NOT_EXPOSED) and an already bound one
// (OPERATION_ALREADY_BOUND), so a name this file should not publish, or one a
// sibling published first, is a startup error instead of silent dead code.
//
// # The wire arity table
//
// Arity and the path index are counted against the inventoried channel
// declarations in src/shared/ipc-channels.ts, with the frozen three-key context
// ALWAYS the last wire argument and its own argument, never inside an input
// object:
//
//	db:import-run-finalize-parsing        (runId 0, path 1, ctx 2)                                       3   :934-937
//	db:import-run-restart                 (runId 0, nextRunId 1, path 2, ctx 3)                         4   :958
//	db:import-run-fail                    (runId 0, stage 1, errorMessage 2, execution 3, path 4, ctx 5) 6   :969-972
//	db:import-run-effect-receipt-get      (runId 0, stage 1, batchId 2, path 3, ctx 4)                  5   :944-947
//
// # The one shape that is not a success envelope
//
// db:import-run-effect-receipt-get returns ImportRunEffectReceipt | null (:946),
// not { success, ... }: a missing receipt is the reference's own NULL
// (importrun_receipts.go:1100-1105, "A missing receipt row is null, never a
// refusal - that is the reference's own answer, and the run is not even read in
// that case"), so this binder answers JSON null and writes nothing. The other
// three answer the business envelope their channels declare: the renderer reads
// `result.run` out of db:import-run-fail (src/services/workflows/import-workflow.ts:432-438),
// and db:import-run-finalize-parsing declares `preparation?: ImportRunPreparationResult`
// (src/shared/ipc-channels.ts:934-937) - the shape src/shared/import-run.ts:358-367 names
// and electron/controllers/db-controller.ts:364 returns.
//
// # One reference step this adapter cannot perform: the failure's wording
//
// db:import-run-fail's argument is the caller's own message, and the reference
// rewrites a symbolic code into a sentence in the run's own locale before storing
// it (electron/controllers/db-controller.ts:524-526 -> persistedImportRunFailure,
// electron/controllers/import-run-failure.ts:40-42, over the src/i18n/app-errors
// catalog). That catalog has no counterpart in this repository, so the message is
// handed to store.ImportRuns.Fail exactly as the renderer sent it and stored
// verbatim (internal/store/importrun_lifecycle.go:1615-1618: "the original stores
// that text as given"). Nothing is invented and no run state is misreported; a
// symbolic code is simply persisted as the code instead of a translated sentence.
//
// # The fence
//
// Every name goes through borrowAuthorDatabase (production_durable_linux.go:313),
// the identical fence every other production read and write uses. This adapter
// opens no database, resolves no path, makes no admission decision, and opens no
// transaction of its own: each store method is one immediate transaction the store
// owns.

const (
	productionImportRunResidualFinalizeParsingName = "db:import-run-finalize-parsing"
	productionImportRunResidualRestartName         = "db:import-run-restart"
	productionImportRunResidualFailName            = "db:import-run-fail"
	productionImportRunResidualReceiptGetName      = "db:import-run-effect-receipt-get"
)

// The four names this slice deliberately leaves unbound. They exist here only so
// the test can pin their 501 together with the capability that is missing; no
// binder in this file publishes any of them.
const (
	productionImportRunResidualAuthorPreviewName     = "db:import-run-author-preview"
	productionImportRunResidualPrepareInspectionName = "db:import-run-prepare-inspection"
	productionImportRunResidualReceiptPrepareName    = "db:import-run-effect-receipt-prepare"
	productionImportRunResidualReceiptCommitName     = "db:import-run-effect-receipt-commit"
)

// productionImportRunResidualOperation is one bound name: its exact wire arity and
// the index of the frozen project path.
type productionImportRunResidualOperation struct {
	name      string
	arity     int
	pathIndex int
}

// productionImportRunResidualOperations is the complete published set, in the
// order the arity table above lists it. It is a closed list rather than a filter,
// so adding a name takes an explicit new entry here plus a store method behind it.
var productionImportRunResidualOperations = [...]productionImportRunResidualOperation{
	{name: productionImportRunResidualFinalizeParsingName, arity: 3, pathIndex: 1},
	{name: productionImportRunResidualRestartName, arity: 4, pathIndex: 2},
	{name: productionImportRunResidualFailName, arity: 6, pathIndex: 4},
	{name: productionImportRunResidualReceiptGetName, arity: 5, pathIndex: 3},
}

// productionImportRunResiduals lends the admitted author database to those four
// names. The adapter is stateless: every call borrows for its own request, so a
// lane opened before the runtime exists (or after a refused startup) answers the
// symbolic refusal instead of holding a handle this process no longer owns.
type productionImportRunResiduals struct {
	durable *productionDurableOpen
}

// newProductionImportRunResiduals returns the residual adapter for the lane built
// by productionHandlerWithContract. It resolves nothing and opens nothing at
// construction time.
func newProductionImportRunResiduals(durable *productionDurableOpen) *productionImportRunResiduals {
	return &productionImportRunResiduals{durable: durable}
}

// Bind installs the four names, one server.Bind per name. A nil adapter is refused
// instead of being published as handlers that can only fail, and every name is
// checked against the registry by Bind itself, so a name that is not inventoried
// or is already bound is a startup error rather than silent dead code.
func (r *productionImportRunResiduals) Bind(server *transport.Server) error {
	if r == nil || server == nil {
		return errors.New("IMPORT_RUN_RESIDUAL_BINDER_INVALID")
	}
	handlers := make(map[string]transport.Operation, len(productionImportRunResidualOperations))
	for _, operation := range productionImportRunResidualOperations {
		handlers[operation.name] = r.operation(operation)
	}
	for _, operation := range productionImportRunResidualOperations {
		if err := server.Bind(operation.name, handlers[operation.name]); err != nil {
			return err
		}
	}
	return nil
}

// operation builds the handler of one bound name. The argument order, the frozen
// context parsing and every refusal code mirror production_importrun_write_linux.go's
// handler, so the renderer sees one contract whichever binder serves a name.
func (r *productionImportRunResiduals) operation(operation productionImportRunResidualOperation) transport.Operation {
	name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if r == nil || r.durable == nil {
			return nil, errors.New("IMPORT_RUN_RESIDUAL_PROVIDER_UNAVAILABLE")
		}
		// Arity first: a stale lease or a project path in the wrong position is a
		// malformed request, never authority.
		if len(args) != arity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		// The frozen context is always its own last argument.
		expected, err := productionImportRunWriteExpectedContext(args, arity-1)
		if err != nil {
			return nil, err
		}
		// Every original handler calls assertRequiredExpectedProjectPath first, so
		// the explicit path must be present and must name the frozen project.
		path, ok := args[pathIndex].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		if path != expected["projectPath"] {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		// The run id is argument 0 for all four names; each name's own fields are
		// read here, before the borrow, exactly like the sibling write lane.
		runID, ok := productionImportRunWriteArgument(args, 0)
		if !ok {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		var stage, batchID, nextRunID string
		var reason any
		var execution store.ImportRunExecution
		switch name {
		case productionImportRunResidualRestartName:
			if nextRunID, ok = productionImportRunWriteArgument(args, 1); !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		case productionImportRunResidualFailName:
			if stage, ok = productionImportRunWriteArgument(args, 1); !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			// The reason crosses untouched, including a non-string: the store's own
			// guard refuses a non-string AFTER its stage comparison
			// (importrun_lifecycle.go:1629-1635), and coercing it here would answer
			// a different code than the reference.
			reason = args[2]
			if execution, ok = productionImportRunWriteExecution(args[3]); !ok {
				return nil, productionImportRunWriteFailure(errors.New("IMPORT_RUN_EXECUTION_STALE"))
			}
		case productionImportRunResidualReceiptGetName:
			if stage, ok = productionImportRunWriteArgument(args, 1); !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			if batchID, ok = productionImportRunWriteArgument(args, 2); !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		}
		db, release, err := r.borrow(ctx, caller, expected)
		if release != nil {
			defer release()
		}
		if err != nil {
			return nil, productionImportRunWriteFailure(err)
		}
		if db == nil || release == nil {
			return nil, errors.New("IMPORT_RUN_RESIDUAL_PROVIDER_UNAVAILABLE")
		}
		repository := store.NewImportRuns(db)
		switch name {
		case productionImportRunResidualFinalizeParsingName:
			preparation, err := repository.FinalizeParsing(ctx, runID)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return map[string]any{"success": true, "preparation": preparation}, nil
		case productionImportRunResidualRestartName:
			run, err := repository.Restart(ctx, runID, nextRunID)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return map[string]any{"success": true, "run": run}, nil
		case productionImportRunResidualFailName:
			run, err := repository.Fail(ctx, runID, stage, reason, execution)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return map[string]any{"success": true, "run": run}, nil
		case productionImportRunResidualReceiptGetName:
			// The one read in this slice, and the one name that is NOT an envelope:
			// a missing receipt is null (importrun_receipts.go:1100-1105).
			receipt, err := repository.GetEffectReceipt(ctx, runID, stage, batchID)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return receipt, nil
		}
		return nil, errors.New("IMPORT_RUN_RESIDUAL_OPERATION_UNKNOWN")
	}
}

// borrow is this slice's single admission: the same borrowAuthorDatabase fence the
// sibling lanes use, with a refusal that names the closed/absent lane instead of
// dereferencing a nil bridge.
func (r *productionImportRunResiduals) borrow(ctx context.Context, caller transport.Caller, expected any) (*sql.DB, func(), error) {
	if r == nil || r.durable == nil {
		return nil, nil, errors.New("IMPORT_RUN_RESIDUAL_PROVIDER_UNAVAILABLE")
	}
	return r.durable.borrowAuthorDatabase(ctx, caller, expected)
}
