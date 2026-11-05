package transport

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// The durable lane's import-run bookkeeping operations. They are served from the
// SAME borrowed author database every other store in this lane uses
// (store.ImportRuns borrows, it never opens), so a lane that also binds
// BindDrafts/BindFinalization cannot collide: none of these ten names is in
// another binder's row set.
//
// Arity and the path index are counted against the inventoried channel
// declarations in src/shared/ipc-channels.ts, with the frozen context ALWAYS the
// last wire argument and its own argument, never inside an input object:
//
//	db:import-run-get                (runId 0, path 1, context 2)             :921
//	db:import-run-list-resumable     (path 0, context 1)                      :923
//	db:import-run-list-chapters      (runId 0, after 1, limit 2, path 3, ctx 4) :925
//	db:import-run-start-resume       (runId 0, owner 1, path 2, context 3)    :936
//	db:import-run-renew-execution    (runId 0, execution 1, path 2, context 3) :941
//	db:import-run-request-cancel     (runId 0, execution 1, path 2, context 3) :946
//	db:import-run-cancel-at-boundary (runId 0, execution 1, path 2, context 3) :951
//	db:import-run-complete-batch     (runId 0, stage 1, batchId 2, execution 3, path 4, ctx 5) :956
//	db:import-run-advance-stage      (runId 0, done 1, next 2, execution 3, path 4, ctx 5)      :965
//	db:import-run-complete           (runId 0, execution 1, path 2, context 3) :978
//
// Every one of those handlers in electron/controllers/db-controller.ts:386-547
// has NO try/catch: a refusal reaches the renderer as a thrown IPC error, which
// is the transport error envelope and NOT the {success:false,error} business
// result the draft family uses. importRunFailure keeps that shape by emitting a
// symbolic code only, exactly like plotTreeClearFailure
// (backend-go/internal/transport/plottree_clear.go:31-45).
//
// What is deliberately NOT here, and stays at GO_OPERATION_NOT_MIGRATED:
// db:import-run-fail (persists a localized human message through the renderer's
// i18n catalog, which has no Go counterpart), db:import-run-restart (creates
// run rows; run creation belongs with parsing), the two effect-receipt
// operations and the effect-receipt get (they need authoritative effect readback
// from the global-facts / blueprint-range / finalized-draft repositories), and
// prepare-inspection / author-preview / finalize-parsing (they need the
// tab-bound in-memory inspection owner).

// ImportRunProvider lends the admitted author database and refreshes the frozen
// lease that the borrow re-verified.
type ImportRunProvider interface {
	BeginImportRuns(ctx context.Context, caller Caller, expected any) (*store.ImportRuns, func(), error)
}

// ImportRunProviderFunc adapts a plain function, so a test can pin the exact
// operation set without a live project.
type ImportRunProviderFunc func(ctx context.Context, caller Caller, expected any) (*store.ImportRuns, func(), error)

func (f ImportRunProviderFunc) BeginImportRuns(ctx context.Context, caller Caller, expected any) (*store.ImportRuns, func(), error) {
	if f == nil {
		return nil, nil, errors.New("IMPORT_RUN_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

// importRunOperationCodes bounds the vocabulary this binder may surface. A
// refusal the store raises with any other text (raw SQLite, a stored last_error
// blob) is replaced, so the browser never sees internal detail.
var importRunSymbolicCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// importRunFailure is the family's refusal envelope: pass a public HTTP error
// through untouched, keep a symbolic repository code, and swallow anything else.
func importRunFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if importRunSymbolicCode.MatchString(err.Error()) {
		return errors.New(err.Error())
	}
	return errors.New("IMPORT_RUN_OPERATION_FAILED")
}

// importRunExecution reads the renderer's execution lease. The original dereferences
// the object directly and a malformed lease fails its authority check, so an
// unusable argument is that same refusal rather than INVALID_ARGUMENTS.
func importRunExecution(value any) (store.ImportRunExecution, bool) {
	input, ok := value.(map[string]any)
	if !ok || input == nil {
		return store.ImportRunExecution{}, false
	}
	owner, ok := input["owner"].(string)
	if !ok || owner == "" || len(owner) > 160 || !utf8.ValidString(owner) {
		return store.ImportRunExecution{}, false
	}
	epoch, ok := draftSafeInteger(input["epoch"], -9007199254740991)
	if !ok {
		return store.ImportRunExecution{}, false
	}
	return store.ImportRunExecution{Owner: owner, Epoch: epoch}, true
}

func importRunArgument(args []any, index int) (string, bool) {
	if index < 0 || index >= len(args) {
		return "", false
	}
	value, ok := args[index].(string)
	if !ok || !utf8.ValidString(value) {
		return "", false
	}
	return value, true
}

// BindImportRuns binds the ten self-contained import-run bookkeeping operations.
// Each handler re-verifies the frozen project path before touching the database,
// because every original handler calls assertRequiredExpectedProjectPath first.
func (s *Server) BindImportRuns(provider ImportRunProvider) error {
	if provider == nil {
		return errors.New("IMPORT_RUN_PROVIDER_REQUIRED")
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:import-run-get", 3, 1},
		{"db:import-run-list-resumable", 2, 0},
		{"db:import-run-list-chapters", 5, 3},
		{"db:import-run-start-resume", 4, 2},
		{"db:import-run-renew-execution", 4, 2},
		{"db:import-run-request-cancel", 4, 2},
		{"db:import-run-cancel-at-boundary", 4, 2},
		{"db:import-run-complete-batch", 6, 4},
		{"db:import-run-advance-stage", 6, 4},
		{"db:import-run-complete", 4, 2},
	}
	// arity counts the trailing frozen context too, because the check below is
	// len(args) != arity and draftExpectedContext reads args[arity-1] as that
	// context. The ten values above were each one short, so every real renderer
	// call was answered 400 INVALID_ARGUMENTS; the header at the top of this
	// binder has the correct counts.
	names := make([]string, 0, len(operations))
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		names = append(names, name)
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != arity {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return nil, err
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
			}
			if path != expected["projectPath"] {
				return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
			}
			// Only the run-less listing may be called without a run id; every
			// other operation passes one through to the repository untouched,
			// because an unknown id is a refusal or a null in the original, not
			// a malformed request.
			runID := ""
			if name != "db:import-run-list-resumable" {
				if runID, ok = importRunArgument(args, 0); !ok {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			}
			var stage, batchID, nextStage, owner string
			var after, limit int64
			switch name {
			case "db:import-run-list-chapters":
				if after, ok = draftSafeInteger(args[1], -9007199254740991); !ok {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
				if limit, ok = draftSafeInteger(args[2], -9007199254740991); !ok {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			case "db:import-run-start-resume":
				if owner, ok = args[1].(string); !ok || !utf8.ValidString(owner) {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			case "db:import-run-complete-batch":
				if stage, ok = args[1].(string); !ok || !utf8.ValidString(stage) {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
				// The controller refuses a stage outside the direct
				// checkpoint set BEFORE the repository is entered
				// (db-controller.ts:495-497), so it wins over every
				// repository refusal.
				if !importRunDirectCheckpointStage(stage) {
					return nil, importRunFailure(errors.New("IMPORT_RUN_CHECKPOINT_STAGE_INVALID"))
				}
				if batchID, ok = args[2].(string); !ok || !utf8.ValidString(batchID) {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			case "db:import-run-advance-stage":
				if stage, ok = args[1].(string); !ok || !utf8.ValidString(stage) {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
				if nextStage, ok = args[2].(string); !ok || !utf8.ValidString(nextStage) {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			}
			// The execution lease is always its own argument. A lease the
			// repository cannot compare (missing owner, fractional or unsafe
			// epoch) fails the authority check in the original, so it is
			// that refusal and not INVALID_ARGUMENTS.
			executionIndex := -1
			switch name {
			case "db:import-run-renew-execution", "db:import-run-request-cancel",
				"db:import-run-cancel-at-boundary", "db:import-run-complete":
				executionIndex = 1
			case "db:import-run-complete-batch", "db:import-run-advance-stage":
				executionIndex = 3
			}
			var execution store.ImportRunExecution
			if executionIndex >= 0 {
				if execution, ok = importRunExecution(args[executionIndex]); !ok {
					return nil, importRunFailure(errors.New("IMPORT_RUN_EXECUTION_STALE"))
				}
			}
			repository, release, err := provider.BeginImportRuns(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return nil, importRunFailure(err)
			}
			if repository == nil || release == nil {
				return nil, errors.New("IMPORT_RUN_PROVIDER_UNAVAILABLE")
			}
			switch name {
			case "db:import-run-get":
				result, err := repository.Get(ctx, runID)
				if err != nil {
					return nil, importRunFailure(err)
				}
				return result, nil
			case "db:import-run-list-resumable":
				result, err := repository.ListResumable(ctx)
				if err != nil {
					return nil, importRunFailure(err)
				}
				return result, nil
			case "db:import-run-list-chapters":
				result, err := repository.ListChapterBatch(ctx, runID, after, limit)
				if err != nil {
					return nil, importRunFailure(err)
				}
				return result, nil
			case "db:import-run-start-resume":
				start, err := repository.StartOrResume(ctx, runID, owner)
				if err != nil {
					return nil, importRunFailure(err)
				}
				return map[string]any{"success": true, "start": start}, nil
			case "db:import-run-renew-execution":
				execution, err := repository.RenewExecution(ctx, runID, execution)
				if err != nil {
					return nil, importRunFailure(err)
				}
				return map[string]any{"success": true, "execution": execution}, nil
			case "db:import-run-request-cancel":
				run, err := repository.RequestCancel(ctx, runID, execution)
				if err != nil {
					return nil, importRunFailure(err)
				}
				return map[string]any{"success": true, "run": run}, nil
			case "db:import-run-cancel-at-boundary":
				run, err := repository.CancelAtBoundary(ctx, runID, execution)
				if err != nil {
					return nil, importRunFailure(err)
				}
				return map[string]any{"success": true, "run": run}, nil
			case "db:import-run-complete-batch":
				result, err := repository.CompleteBatch(ctx, runID, stage, batchID, execution)
				if err != nil {
					return nil, importRunFailure(err)
				}
				response := map[string]any{"success": true}
				for key, entry := range result {
					response[key] = entry
				}
				return response, nil
			case "db:import-run-advance-stage":
				run, err := repository.AdvanceStage(ctx, runID, stage, nextStage, execution)
				if err != nil {
					return nil, importRunFailure(err)
				}
				return map[string]any{"success": true, "run": run}, nil
			case "db:import-run-complete":
				run, err := repository.Complete(ctx, runID, execution)
				if err != nil {
					return nil, importRunFailure(err)
				}
				return map[string]any{"success": true, "run": run}, nil
			}
			return nil, errors.New("IMPORT_RUN_OPERATION_UNKNOWN")
		}
	}
	for _, name := range names {
		if err := s.Bind(name, handlers[name]); err != nil {
			return err
		}
	}
	return nil
}

// importRunDirectCheckpointStage mirrors isImportRunDirectCheckpointStage
// (src/shared/import-run.ts): only these four stages are checkpointed by direct
// evidence; every other stage needs a receipt.
func importRunDirectCheckpointStage(stage string) bool {
	switch stage {
	case "knowledge", "author-publish", "author-postprocess", "refresh":
		return true
	}
	return false
}
