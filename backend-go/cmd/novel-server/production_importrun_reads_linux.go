//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's read-only slice of the `db:import-run-*`
// family in web/operations.json (18 names). It binds exactly three of them and
// installs no other name, so every write and every name this lane cannot answer
// honestly keeps failing closed at 501 GO_OPERATION_NOT_MIGRATED.
//
// # Why the family binder is not used
//
// transport.BindImportRuns (internal/transport/import_run.go:122-308) is atomic
// across ten names: it publishes db:import-run-get, -list-resumable,
// -list-chapters, -start-resume, -renew-execution, -request-cancel,
// -cancel-at-boundary, -complete-batch, -advance-stage and -complete together, or
// nothing at all. Seven of those ten are writes, so binding it here would hand
// this lane the capability to mint an execution lease, advance a stage and retire
// a run on the author's own database - operations whose evidence (an
// authoritative effect receipt, a tab-bound inspection owner) this lane does not
// own. The names are therefore bound per name with server.Bind, the same way
// newProductionPromptLoad is installed for prompt:load-global
// (production_open_linux.go:580), and the three-name set below is the whole of
// what this file can ever publish.
//
// # The three reads are honestly read-only
//
// store.NewImportRuns (internal/store/importrun.go:35) performs no I/O. The three
// bound methods are pure projections of the admitted author database:
//
//	db:import-run-get            store.ImportRuns.Get              (:998)
//	db:import-run-list-resumable store.ImportRuns.ListResumable     (:1026)
//	db:import-run-list-chapters  store.ImportRuns.ListChapterBatch  (:1067)
//
// All three run inside importRunReadSnapshot (:982-996), a
// `BeginTx(..., ReadOnly: true)` transaction that is committed without a write,
// and every helper they reach re-reads: readImportRunRow (:121), importRunHasLedger
// (:142), importRunCompletedBatches (:153), importRunAuthorInference (:245),
// importRunSourceProgress (:267), importRunUnfinishedSourceDisplay (:279),
// importRunPersistedProgress (:486), importRunSnapshot (:555),
// importRunChapterSnapshot (:624) and importRunCheckpointChapterNumbers (:413).
// There is no INSERT, no UPDATE, no DELETE, no `INSERT OR IGNORE` backfill and no
// BEGIN IMMEDIATE anywhere on that path (the only ExecContext in the file is
// importRunImmediate's reserved-connection transaction API, :948-980, which none
// of the three reads enters), so a served read commits nothing and the borrow's
// release closure is a re-verification, not a write acknowledgement.
//
// # Refused, with the store line that makes each one a mutation
//
//	db:import-run-start-resume        store.ImportRuns.StartOrResume     :1112 -> UPDATE import_runs :1154
//	db:import-run-renew-execution     store.ImportRuns.RenewExecution    :1195 -> UPDATE import_runs SET lease_expires_at :1207
//	db:import-run-request-cancel      store.ImportRuns.RequestCancel     :1226 -> UPDATE import_runs SET cancel_requested = 1 :1235
//	db:import-run-cancel-at-boundary  store.ImportRuns.CancelAtBoundary  :1263 -> UPDATE import_runs :1272
//	db:import-run-complete-batch      store.ImportRuns.CompleteBatch     :1304 -> UPDATE import_effect_ledger :889, INSERT INTO import_effect_ledger :899, UPDATE import_runs :910 (importRunApplyBatchCheckpoint :849) and UPDATE import_runs SET completed_batches_json :934 (importRunWriteCompletedBatches :929)
//	db:import-run-advance-stage       store.ImportRuns.AdvanceStage      :1348 -> UPDATE import_runs :1376
//	db:import-run-complete            store.ImportRuns.Complete          :1406 -> UPDATE import_runs :1422
//
// Refused for having no read-only store path at all in this repository (the
// family binder deliberately omits them too, import_run.go:42-49, so there is
// nothing to bind without writing raw SQL in package main):
//
//	db:import-run-prepare-inspection   run creation; needs the tab-bound in-memory inspection owner
//	db:import-run-author-preview       run creation; same owner
//	db:import-run-finalize-parsing     run creation; same owner
//	db:import-run-restart              run creation; needs the generation-source proof
//	db:import-run-fail                 persists a localized human message from the renderer's i18n catalog
//	db:import-run-effect-receipt-get   needs authoritative effect readback outside this database
//	db:import-run-effect-receipt-prepare  same readback, plus a receipt write
//	db:import-run-effect-receipt-commit   same readback, plus a receipt write
//
// # The fence
//
// Every request goes through borrowAuthorDatabase
// (production_durable_linux.go:308), the identical fence the draft CAS, the
// character reads, the editor stage reads, the blueprint read and the
// continuity/post-process/plot-tree/review reads already use: the frozen
// three-key context is parsed, the caller's own cookie/tab identity is named, the
// project path must be one the startup inventory issued a catalog key for, and
// authorruntime re-proves the lease, the admission permit and the handle identity
// before lending the author's own database. Nothing here opens a database,
// resolves a path or makes an admission decision.

const (
	productionImportRunGetName           = "db:import-run-get"
	productionImportRunListResumableName = "db:import-run-list-resumable"
	productionImportRunListChaptersName  = "db:import-run-list-chapters"
)

// productionImportRunRead is one bound read: its operation name, its exact wire
// arity and the index of the frozen project path. Arity and the path index are
// counted against the inventoried channel declarations in
// src/shared/ipc-channels.ts, with the frozen three-key context ALWAYS the last
// wire argument and its own argument, never inside an input object:
//
//	db:import-run-get            (runId 0, path 1, context 2)                :938
//	db:import-run-list-resumable (path 0, context 1)                         :939
//	db:import-run-list-chapters  (runId 0, after 1, limit 2, path 3, ctx 4)  :940-943
//
// Every project-scoped channel gains that trailing context from the renderer's own
// client (src/services/ipc-client.ts:71-84, which appends it at :83;
// isProjectScopedChannel at :47 lists `db:` at :61), which is why each arity here
// is the declared one plus the context. The family binder's table
// (internal/transport/import_run.go:130-139) is one short on every one of its ten
// names - it declares get 2, list-resumable 1, list-chapters 4, start-resume 3 and
// complete-batch 5 while its own header (:24-33) documents 3, 2, 5, 4 and 6 - so a
// request shaped the way the renderer really sends it would fail its
// `len(args) != arity` check with 400 INVALID_ARGUMENTS, and the three reads have
// the same off-by-one as the writes. This file follows the wire, the inventoried
// declarations and that header comment, not the short table, and it is why the
// family binder is not reused here even though it is the same family: see also
// internal/transport/blueprints_read.go:32, which counts the trailing context the
// way this file does.
type productionImportRunRead struct {
	name      string
	arity     int
	pathIndex int
}

// productionImportRunReadOperations is the complete published set. It is a
// closed list rather than a filter over the family binder's ten names, so a
// write can never enter it by accident: adding one takes an explicit new entry
// here plus a store method that does not mutate.
var productionImportRunReadOperations = [...]productionImportRunRead{
	{name: productionImportRunGetName, arity: 3, pathIndex: 1},
	{name: productionImportRunListResumableName, arity: 2, pathIndex: 0},
	{name: productionImportRunListChaptersName, arity: 5, pathIndex: 3},
}

// productionImportRunReads lends the admitted author database to those three
// reads. The adapter is stateless: every call borrows for its own request, so a
// lane opened before the runtime exists (or after a refused startup) answers the
// symbolic PRODUCTION_CLOSED refusal instead of holding a handle this process no
// longer owns.
type productionImportRunReads struct {
	durable *productionDurableOpen
}

// newProductionImportRunReads returns the read adapter for the lane built by
// productionHandlerWithContract. It resolves nothing and opens nothing at
// construction time.
func newProductionImportRunReads(durable *productionDurableOpen) *productionImportRunReads {
	return &productionImportRunReads{durable: durable}
}

// Bind installs the three reads, one server.Bind per name. A nil adapter is
// refused instead of being published as three handlers that can only fail, and
// every name is checked against the registry by Bind itself
// (internal/transport/server.go:73-79), so a name that is not inventoried or is
// already bound is a startup error rather than silent dead code.
func (r *productionImportRunReads) Bind(server *transport.Server) error {
	if r == nil || server == nil {
		return errors.New("IMPORT_RUN_READ_BINDER_INVALID")
	}
	handlers := make(map[string]transport.Operation, len(productionImportRunReadOperations))
	for _, operation := range productionImportRunReadOperations {
		handlers[operation.name] = r.operation(operation)
	}
	for _, operation := range productionImportRunReadOperations {
		if err := server.Bind(operation.name, handlers[operation.name]); err != nil {
			return err
		}
	}
	return nil
}

// operation builds the handler of one bound read. The argument order and every
// refusal code mirror the family binder's handler for the same three names, so
// the renderer sees one contract whichever binder serves it.
func (r *productionImportRunReads) operation(operation productionImportRunRead) transport.Operation {
	name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if r == nil {
			return nil, errors.New("IMPORT_RUN_READ_PROVIDER_UNAVAILABLE")
		}
		// Arity first, exactly like the family binder: a stale lease or a
		// project path in the wrong position is a malformed request, never
		// authority.
		if len(args) != arity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		// The frozen context is always its own last argument.
		expected, err := projectsession.ParseContext(args[arity-1])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		// Every original handler calls assertRequiredExpectedProjectPath first,
		// so the explicit path must be present and must name the frozen project.
		path, ok := productionImportRunReadArgument(args, pathIndex)
		if !ok || path == "" {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		if path != expected.ProjectPath {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		// Only the run-less listing may be called without a run id; the other
		// two pass theirs straight through, because an unknown id is a null in
		// the original, not a malformed request.
		runID := ""
		if name != productionImportRunListResumableName {
			if runID, ok = productionImportRunReadArgument(args, 0); !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		}
		var after, limit int64
		if name == productionImportRunListChaptersName {
			if after, ok = productionImportRunReadInteger(args[1]); !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			if limit, ok = productionImportRunReadInteger(args[2]); !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		}
		db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, args[arity-1])
		if release != nil {
			defer release()
		}
		if err != nil {
			return nil, productionImportRunReadFailure(err)
		}
		if db == nil || release == nil {
			return nil, errors.New("IMPORT_RUN_READ_PROVIDER_UNAVAILABLE")
		}
		repository := store.NewImportRuns(db)
		var result any
		switch name {
		case productionImportRunGetName:
			result, err = repository.Get(ctx, runID)
		case productionImportRunListResumableName:
			result, err = repository.ListResumable(ctx)
		case productionImportRunListChaptersName:
			result, err = repository.ListChapterBatch(ctx, runID, after, limit)
		default:
			return nil, errors.New("IMPORT_RUN_READ_OPERATION_UNKNOWN")
		}
		if err != nil {
			return nil, productionImportRunReadFailure(err)
		}
		return result, nil
	}
}

// productionImportRunReadSymbolicCode bounds the vocabulary a repository refusal
// may surface. It is the family binder's own expression (import_run.go:71).
var productionImportRunReadSymbolicCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// productionImportRunReadFailure is this file's copy of the family's refusal
// envelope (import_run.go:75-88): a public HTTP error passes through untouched,
// a symbolic repository code survives, and any other text - raw SQLite detail, a
// stored last_error blob - is replaced so the browser never sees internal state.
func productionImportRunReadFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if productionImportRunReadSymbolicCode.MatchString(err.Error()) {
		return errors.New(err.Error())
	}
	return errors.New("IMPORT_RUN_OPERATION_FAILED")
}

// productionImportRunReadArgument reads one string argument without coercing it,
// exactly like the family binder's importRunArgument (import_run.go:108-116).
func productionImportRunReadArgument(args []any, index int) (string, bool) {
	if index < 0 || index >= len(args) {
		return "", false
	}
	value, ok := args[index].(string)
	if !ok || !utf8.ValidString(value) {
		return "", false
	}
	return value, true
}

// productionImportRunReadInteger accepts only a whole JavaScript-safe number, the
// contract draftSafeInteger (internal/transport/drafts.go:183) enforces for the
// same two arguments: no strings, booleans or null, no fractions, nothing outside
// +/-9007199254740991. JSON arguments arrive as float64 from security.JSONBody
// (internal/security/body.go:78-93); json.Number is accepted for callers that
// keep their own decoder.
func productionImportRunReadInteger(value any) (int64, bool) {
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
	if math.IsNaN(number) || math.IsInf(number, 0) || number < -9007199254740991 ||
		number > 9007199254740991 || math.Trunc(number) != number {
		return 0, false
	}
	return int64(number), true
}
