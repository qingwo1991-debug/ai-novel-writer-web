//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's WRITE slice of the `db:import-run-*` family
// in web/operations.json (18 names). It publishes exactly seven names and installs
// nothing else:
//
//	db:import-run-start-resume        store.ImportRuns.StartOrResume   internal/store/importrun.go:1112
//	db:import-run-renew-execution     store.ImportRuns.RenewExecution  :1195
//	db:import-run-request-cancel      store.ImportRuns.RequestCancel   :1226
//	db:import-run-cancel-at-boundary  store.ImportRuns.CancelAtBoundary :1263
//	db:import-run-complete-batch      store.ImportRuns.CompleteBatch   :1304
//	db:import-run-advance-stage       store.ImportRuns.AdvanceStage    :1348
//	db:import-run-complete            store.ImportRuns.Complete        :1406
//
// # Why one server.Bind per name instead of the family binder
//
// transport.BindImportRuns (internal/transport/import_run.go:122-315) is atomic
// across ten names. Three of those ten are READS - db:import-run-get,
// db:import-run-list-resumable and db:import-run-list-chapters - that
// production_importrun_reads_linux.go already binds (its
// productionImportRunReadOperations, :137-141). Calling the family binder here
// would therefore fail startup with OPERATION_ALREADY_BOUND, and it would also
// publish three reads this file does not own. Each of the seven writes is
// installed instead with server.Bind, the exported single-name binder
// (internal/transport/server.go:73-79), exactly the way
// production_importrun_reads_linux.go:164-179 and
// production_review_write_linux.go:193-200 install their own slices. Bind refuses
// an uninventoried name (OPERATION_NOT_EXPOSED) and an already bound one
// (OPERATION_ALREADY_BOUND), so a name this file should not publish is a startup
// error instead of silent dead code, and a sibling binder that takes one of these
// names first is a startup error instead of a silent collision.
//
// # The wire arity table
//
// Arity and the path index are counted against the inventoried channel
// declarations in src/shared/ipc-channels.ts, with the frozen three-key context
// ALWAYS the last wire argument and its own argument, never inside an input
// object. The counts below are the family binder's CURRENT table
// (import_run.go:130-139) and its own header (:24-33), which agree: arity counts
// the trailing context, the handler compares len(args) != arity, and the context
// is args[arity-1]. They were each one short until recently, and every real
// renderer call was answered 400 INVALID_ARGUMENTS while they were, so they are
// mirrored here rather than re-derived:
//
//	db:import-run-start-resume        (runId 0, owner 1, path 2, context 3)                      4
//	db:import-run-renew-execution     (runId 0, execution 1, path 2, context 3)                  4
//	db:import-run-request-cancel      (runId 0, execution 1, path 2, context 3)                  4
//	db:import-run-cancel-at-boundary  (runId 0, execution 1, path 2, context 3)                  4
//	db:import-run-complete-batch      (runId 0, stage 1, batchId 2, execution 3, path 4, ctx 5)  6
//	db:import-run-advance-stage       (runId 0, done 1, next 2, execution 3, path 4, ctx 5)      6
//	db:import-run-complete            (runId 0, execution 1, path 2, context 3)                  4
//
// # The write entry point
//
// Every handler borrows through productionDurableOpen.borrowAuthorDatabase
// (production_durable_linux.go:308-355) - the write entry point the lane's other
// mutations already use: db:save-summary-snapshot, the three self-contained draft
// mutations (BeginDrafts, :359), the blueprint/project-core/roster writes
// (production_blueprint_write_linux.go:92-97) and the review/recovery writes
// (production_review_write_linux.go:207-212). That borrow parses the frozen
// three-key context, names the caller's own cookie/tab identity, requires the
// project path to be one the startup inventory issued a catalog key for, and then
// lets authorruntime re-prove the live lease, the admission permit and the handle
// identity before lending the author's own database. Its release closure refreshes
// the permit so it ADOPTS the content identity the request just committed; a
// refused transition commits nothing, so its refresh is only a re-verification. A
// read-only borrow would be the wrong capability here: every one of these seven
// names writes, and the store opens BEGIN IMMEDIATE on a reserved connection
// (importrun.go:948-979) which a read-only transaction cannot do.
//
// Nothing here opens a database, resolves a path, or makes an admission decision,
// and no repository is constructed over a database that failed the pinned v7
// admission. store.ImportRuns runs the original repository's own statements and
// owns every precondition below, so this file contains no SQL at all.
//
// # Refusals: the store's own code, carried by the transport error envelope
//
// Every original handler in electron/controllers/db-controller.ts:386-547 has NO
// try/catch, so a refusal reaches the renderer as a thrown IPC error, NOT the
// {success:false,error} business result the draft family uses. This file keeps
// that shape by copying the family's envelope (import_run.go:75-87) verbatim: a
// public HTTP error passes through untouched, a symbolic repository code
// survives, and any other text - raw SQLite detail, a stored last_error blob - is
// replaced with IMPORT_RUN_OPERATION_FAILED so the browser never sees internal
// state. A successful call answers 200 with the family binder's own success
// payload, so the renderer sees one contract whichever binder serves it.
//
// # What each name enforces, and the code it refuses with
//
//	db:import-run-start-resume        the run must exist, be resumable and be in
//	                                  ready/running/failed/cancelled, its stage
//	                                  must not be 'parsing', no OTHER run may be
//	                                  running under a live lease, and a foreign
//	                                  live executor is busy. Refusals:
//	                                  IMPORT_RUN_NOT_STARTABLE (:1131),
//	                                  IMPORT_RUN_PARSING_INCOMPLETE (:1134),
//	                                  IMPORT_RUN_ANOTHER_RUNNING (:1141),
//	                                  IMPORT_RUN_EXECUTOR_BUSY (:1147),
//	                                  IMPORT_RUN_EXECUTOR_INVALID (:1115).
//	db:import-run-renew-execution     only the exact running owner+epoch inside
//	                                  the lease may renew it, so a lease this run
//	                                  never issued is IMPORT_RUN_EXECUTION_STALE
//	                                  (importRunAssertExecutionAuthority :641-652).
//	db:import-run-request-cancel      same authority, then status must be
//	                                  ready/running (:1236) or IMPORT_RUN_NOT_CANCELLABLE.
//	db:import-run-cancel-at-boundary  same authority, then status must be
//	                                  ready/running/failed (:1276) or
//	                                  IMPORT_RUN_NOT_CANCELLABLE.
//	db:import-run-complete-batch      the stage must be a direct checkpoint stage
//	                                  (IMPORT_RUN_CHECKPOINT_STAGE_INVALID, :198 of
//	                                  the family binder) and the batch's stage must
//	                                  BE the run's running stage
//	                                  (IMPORT_RUN_BATCH_STAGE_MISMATCH, :698), then
//	                                  the batch id and its evidence must be legal
//	                                  (IMPORT_RUN_BATCH_ID_INVALID,
//	                                  IMPORT_RUN_CHECKPOINT_OVERLAP,
//	                                  IMPORT_RUN_EFFECT_HISTORY_UNKNOWN).
//	db:import-run-advance-stage       the completed stage must be the run's own
//	                                  running stage and the next stage must be the
//	                                  plan's next one (IMPORT_RUN_STAGE_MISMATCH,
//	                                  :1358-1368), and the stage must be fully
//	                                  checkpointed first
//	                                  (IMPORT_RUN_STAGE_INCOMPLETE, :778/:796/:836).
//	db:import-run-complete            only a RUNNING refresh stage whose 'refresh'
//	                                  checkpoint is 'done' may finish
//	                                  (IMPORT_RUN_STAGE_MISMATCH :1417,
//	                                  IMPORT_RUN_STAGE_INCOMPLETE :1419), so
//	                                  completing an already-complete run is never a
//	                                  success: its status is no longer 'running', so
//	                                  the authority check refuses it first with
//	                                  IMPORT_RUN_EXECUTION_STALE (:646).
//
// # What stays unbound
//
// db:import-run-restart (run creation), prepare-inspection, author-preview and
// finalize-parsing (the tab-bound in-memory inspection owner), db:import-run-fail
// (a localized human message from the renderer's i18n catalog) and the three
// effect-receipt names (authoritative effect readback outside this database). They
// keep failing closed at GO_OPERATION_NOT_MIGRATED rather than being installed as
// handlers that could only refuse. The three read names stay with
// production_importrun_reads_linux.go, which is also the file whose 501
// expectations for these seven names move to a served answer once this binder is
// installed by production_open_linux.go.

const (
	productionImportRunWriteStartResumeName      = "db:import-run-start-resume"
	productionImportRunWriteRenewExecutionName   = "db:import-run-renew-execution"
	productionImportRunWriteRequestCancelName    = "db:import-run-request-cancel"
	productionImportRunWriteCancelAtBoundaryName = "db:import-run-cancel-at-boundary"
	productionImportRunWriteCompleteBatchName    = "db:import-run-complete-batch"
	productionImportRunWriteAdvanceStageName     = "db:import-run-advance-stage"
	productionImportRunWriteCompleteName         = "db:import-run-complete"
)

// productionImportRunWriteOperation is one bound write: its operation name, its
// exact wire arity (trailing frozen context INCLUDED) and the index of the frozen
// project path. See the table above for where each value comes from.
type productionImportRunWriteOperation struct {
	name      string
	arity     int
	pathIndex int
}

// productionImportRunWriteOperations is the complete published set, in the family
// binder's own order. It is a closed list rather than a filter over the binder's
// ten names, so a read can never enter it by accident.
var productionImportRunWriteOperations = [...]productionImportRunWriteOperation{
	{name: productionImportRunWriteStartResumeName, arity: 4, pathIndex: 2},
	{name: productionImportRunWriteRenewExecutionName, arity: 4, pathIndex: 2},
	{name: productionImportRunWriteRequestCancelName, arity: 4, pathIndex: 2},
	{name: productionImportRunWriteCancelAtBoundaryName, arity: 4, pathIndex: 2},
	{name: productionImportRunWriteCompleteBatchName, arity: 6, pathIndex: 4},
	{name: productionImportRunWriteAdvanceStageName, arity: 6, pathIndex: 4},
	{name: productionImportRunWriteCompleteName, arity: 4, pathIndex: 2},
}

// productionImportRunWrites lends the admitted author database to those seven
// writes. The adapter is stateless: every call borrows for its own request, so a
// lane opened before the runtime exists (or after a refused startup) answers the
// symbolic PRODUCTION_CLOSED refusal instead of holding a handle this process no
// longer owns.
type productionImportRunWrites struct {
	durable *productionDurableOpen
}

// newProductionImportRunWrites returns the write adapter for the lane built by
// productionHandlerWithContract. It resolves nothing and opens nothing at
// construction time.
func newProductionImportRunWrites(durable *productionDurableOpen) *productionImportRunWrites {
	return &productionImportRunWrites{durable: durable}
}

// Bind installs the seven writes, one server.Bind per name. A nil adapter is
// refused instead of being published as seven handlers that can only fail, and
// every name is checked against the registry by Bind itself
// (internal/transport/server.go:73-79), so a name that is not inventoried or is
// already bound is a startup error rather than silent dead code.
func (w *productionImportRunWrites) Bind(server *transport.Server) error {
	if w == nil || w.durable == nil {
		return errors.New("IMPORT_RUN_WRITE_PROVIDER_REQUIRED")
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	handlers := make(map[string]transport.Operation, len(productionImportRunWriteOperations))
	for _, operation := range productionImportRunWriteOperations {
		handlers[operation.name] = w.operation(operation)
	}
	for _, operation := range productionImportRunWriteOperations {
		if err := server.Bind(operation.name, handlers[operation.name]); err != nil {
			return err
		}
	}
	return nil
}

// borrowWriteDatabase is the lane's single write admission: borrowAuthorDatabase,
// the identical borrow every other production mutation writes through. It is
// deliberately not a read-only borrow, because all seven names below commit.
func (w *productionImportRunWrites) borrowWriteDatabase(ctx context.Context, caller transport.Caller, expected any) (*sql.DB, func(), error) {
	if w == nil || w.durable == nil {
		return nil, nil, errors.New("IMPORT_RUN_WRITE_PROVIDER_UNAVAILABLE")
	}
	return w.durable.borrowAuthorDatabase(ctx, caller, expected)
}

// operation builds the handler of one bound write. Argument order, the arity
// check and every refusal code mirror the family binder's handler for the same
// name (import_run.go:151-307), so the renderer sees one contract whichever binder
// serves it; the only substituted step is the admission itself, which is the
// lane's durable borrow instead of a provider adapter.
func (w *productionImportRunWrites) operation(operation productionImportRunWriteOperation) transport.Operation {
	name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if w == nil || w.durable == nil {
			return nil, errors.New("IMPORT_RUN_WRITE_PROVIDER_UNAVAILABLE")
		}
		// Arity first, exactly like the family binder: arity counts the trailing
		// frozen context, so a stale lease or a project path in the wrong
		// position is a malformed request, never authority.
		if len(args) != arity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		// The frozen context is always its own last argument, and its identity is
		// cloned so no later layer can rewrite the captured request in place.
		expected, err := productionImportRunWriteExpectedContext(args, arity-1)
		if err != nil {
			return nil, err
		}
		path, ok := args[pathIndex].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		// The path argument must BE the frozen project path: one project's lease
		// can never be aimed at another project's rows.
		if path != expected["projectPath"] {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		// Every one of these seven names carries the run id at index 0, and an
		// unknown id is a refusal from the repository, not a malformed request.
		runID, ok := productionImportRunWriteArgument(args, 0)
		if !ok {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		var stage, batchID, nextStage, owner string
		switch name {
		case productionImportRunWriteStartResumeName:
			if owner, ok = args[1].(string); !ok || !utf8.ValidString(owner) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		case productionImportRunWriteCompleteBatchName:
			if stage, ok = args[1].(string); !ok || !utf8.ValidString(stage) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			// The controller refuses a stage outside the direct checkpoint set
			// BEFORE the repository is entered (db-controller.ts:495-497), so it
			// wins over every repository refusal.
			if !productionImportRunWriteDirectCheckpointStage(stage) {
				return nil, productionImportRunWriteFailure(errors.New("IMPORT_RUN_CHECKPOINT_STAGE_INVALID"))
			}
			if batchID, ok = args[2].(string); !ok || !utf8.ValidString(batchID) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		case productionImportRunWriteAdvanceStageName:
			if stage, ok = args[1].(string); !ok || !utf8.ValidString(stage) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			if nextStage, ok = args[2].(string); !ok || !utf8.ValidString(nextStage) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		}
		// The execution lease is always its own argument, and start-resume is the
		// one name that carries an owner instead. A lease the repository cannot
		// compare (missing owner, fractional or unsafe epoch) fails the authority
		// check in the original, so it is that refusal and not INVALID_ARGUMENTS.
		executionIndex := -1
		switch name {
		case productionImportRunWriteRenewExecutionName, productionImportRunWriteRequestCancelName,
			productionImportRunWriteCancelAtBoundaryName, productionImportRunWriteCompleteName:
			executionIndex = 1
		case productionImportRunWriteCompleteBatchName, productionImportRunWriteAdvanceStageName:
			executionIndex = 3
		}
		var execution store.ImportRunExecution
		if executionIndex >= 0 {
			if execution, ok = productionImportRunWriteExecution(args[executionIndex]); !ok {
				return nil, productionImportRunWriteFailure(errors.New("IMPORT_RUN_EXECUTION_STALE"))
			}
		}
		// The live write lease, the permit and the handle identity are proven
		// here. A refusal leaves the release nil, so nothing is unlocked twice.
		db, release, err := w.borrowWriteDatabase(ctx, caller, expected)
		if release != nil {
			defer release()
		}
		if err != nil {
			return nil, productionImportRunWriteFailure(err)
		}
		if db == nil || release == nil {
			return nil, errors.New("IMPORT_RUN_PROVIDER_UNAVAILABLE")
		}
		repository := store.NewImportRuns(db)
		switch name {
		case productionImportRunWriteStartResumeName:
			start, err := repository.StartOrResume(ctx, runID, owner)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return map[string]any{"success": true, "start": start}, nil
		case productionImportRunWriteRenewExecutionName:
			renewed, err := repository.RenewExecution(ctx, runID, execution)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return map[string]any{"success": true, "execution": renewed}, nil
		case productionImportRunWriteRequestCancelName:
			run, err := repository.RequestCancel(ctx, runID, execution)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return map[string]any{"success": true, "run": run}, nil
		case productionImportRunWriteCancelAtBoundaryName:
			run, err := repository.CancelAtBoundary(ctx, runID, execution)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return map[string]any{"success": true, "run": run}, nil
		case productionImportRunWriteCompleteBatchName:
			result, err := repository.CompleteBatch(ctx, runID, stage, batchID, execution)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			response := map[string]any{"success": true}
			for key, entry := range result {
				response[key] = entry
			}
			return response, nil
		case productionImportRunWriteAdvanceStageName:
			run, err := repository.AdvanceStage(ctx, runID, stage, nextStage, execution)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return map[string]any{"success": true, "run": run}, nil
		case productionImportRunWriteCompleteName:
			run, err := repository.Complete(ctx, runID, execution)
			if err != nil {
				return nil, productionImportRunWriteFailure(err)
			}
			return map[string]any{"success": true, "run": run}, nil
		}
		return nil, errors.New("IMPORT_RUN_WRITE_OPERATION_UNKNOWN")
	}
}

// productionImportRunWriteExpectedContext is draftExpectedContext's contract
// (internal/transport/drafts.go:164-178), which the family binder relies on
// (:155): the frozen context is a map carrying non-empty projectId, leaseId and
// projectPath strings, and only those three are cloned forward. Anything else is a
// malformed request (400 PROJECT_SESSION_REQUIRED), never authority.
func productionImportRunWriteExpectedContext(args []any, index int) (map[string]any, error) {
	if index >= 0 && index < len(args) {
		if raw, ok := args[index].(map[string]any); ok {
			id, idOK := raw["projectId"].(string)
			lease, leaseOK := raw["leaseId"].(string)
			path, pathOK := raw["projectPath"].(string)
			if idOK && leaseOK && pathOK && id != "" && lease != "" && path != "" {
				return map[string]any{"projectId": id, "leaseId": lease, "projectPath": path}, nil
			}
		}
	}
	return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
}

// productionImportRunWriteExecution reads the renderer's execution lease exactly
// like the family binder's importRunExecution (import_run.go:92-106): the original
// dereferences the object directly and a malformed lease fails its authority
// check, so an unusable argument is that same refusal rather than
// INVALID_ARGUMENTS.
func productionImportRunWriteExecution(value any) (store.ImportRunExecution, bool) {
	input, ok := value.(map[string]any)
	if !ok || input == nil {
		return store.ImportRunExecution{}, false
	}
	owner, ok := input["owner"].(string)
	if !ok || owner == "" || len(owner) > 160 || !utf8.ValidString(owner) {
		return store.ImportRunExecution{}, false
	}
	epoch, ok := productionImportRunWriteSafeInteger(input["epoch"], -9007199254740991)
	if !ok {
		return store.ImportRunExecution{}, false
	}
	return store.ImportRunExecution{Owner: owner, Epoch: epoch}, true
}

// productionImportRunWriteArgument reads one string argument without coercing it,
// exactly like the family binder's importRunArgument (import_run.go:108-116).
func productionImportRunWriteArgument(args []any, index int) (string, bool) {
	if index < 0 || index >= len(args) {
		return "", false
	}
	value, ok := args[index].(string)
	if !ok || !utf8.ValidString(value) {
		return "", false
	}
	return value, true
}

// productionImportRunWriteSafeInteger accepts only a whole JavaScript-safe
// number, the contract draftSafeInteger (internal/transport/drafts.go:183-215)
// enforces for the same epoch field: no strings, booleans or null, no fractions,
// nothing outside +/-9007199254740991. JSON arguments arrive as float64 from
// security.JSONBody; json.Number is accepted for callers that keep their own
// decoder.
func productionImportRunWriteSafeInteger(value any, minimum int64) (int64, bool) {
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case json.Number:
		if len(value) > 64 || !json.Valid([]byte(value)) {
			return 0, false
		}
		parsed, err := value.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < float64(minimum) ||
		number > 9007199254740991 || math.Trunc(number) != number {
		return 0, false
	}
	return int64(number), true
}

// productionImportRunWriteSymbolicCode bounds the vocabulary a repository refusal
// may surface. It is the family binder's own expression (import_run.go:71).
var productionImportRunWriteSymbolicCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// productionImportRunWriteFailure is this file's copy of the family's refusal
// envelope (import_run.go:75-87): a public HTTP error passes through untouched, a
// symbolic repository code survives, and any other text is replaced so the
// browser never sees raw SQL or a stored last_error blob.
func productionImportRunWriteFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if productionImportRunWriteSymbolicCode.MatchString(err.Error()) {
		return errors.New(err.Error())
	}
	return errors.New("IMPORT_RUN_OPERATION_FAILED")
}

// productionImportRunWriteDirectCheckpointStage mirrors isImportRunDirectCheckpointStage
// (src/shared/import-run.ts, and import_run.go:320-326): only these four stages
// are checkpointed by direct evidence; every other stage needs a receipt.
func productionImportRunWriteDirectCheckpointStage(stage string) bool {
	switch stage {
	case "knowledge", "author-publish", "author-postprocess", "refresh":
		return true
	}
	return false
}
