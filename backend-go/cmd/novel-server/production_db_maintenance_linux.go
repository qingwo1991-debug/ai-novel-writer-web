//go:build linux

package main

// This file serves the three destructive `db:*` writes the db residual table
// still refuses. All three now have a real store port behind them
// (internal/store/dbmaintenance.go); none of them is a seam.
//
//	reference handler                          wire shape (src/shared/ipc-channels.ts)
//	electron/controllers/db-controller.ts:656  db:blueprint-clear-all  (path, context)
//	electron/controllers/db-controller.ts:245  db:project-clear-generated-data
//	                                           (options, path, context)
//	electron/controllers/db-controller.ts:236  db:import-global-facts-commit
//	                                           (request, path, context)
//
// The frozen three-key project context is always the LAST argument and never
// inside an input object, which is why the path index is one less than the arity
// for the two names that carry an input and zero for the one that does not. Each
// row above is cross-checked against the renderer call site that really sends it:
//
//	src/components/editor/use-chapter-blueprints.ts:570
//	  invokeWithProjectSession(session, 'db:blueprint-clear-all', projectKey)
//	src/services/project-clear-service.ts:151
//	  invokeWithProjectSession(session, 'db:project-clear-generated-data', normalized, projectPath)
//	src/services/workflows/commands/import-novel.command.ts:542
//	  invokeWithProjectSession(session, 'db:import-global-facts-commit', commitRequest, context.projectPath)
//
// # The three answers differ, and the difference is the controllers'
//
// db:blueprint-clear-all and db:project-clear-generated-data wrap their whole body
// in try/catch and answer `{ success: false, error: String(err) }`, so every
// refusal after the argument fence - a stale lease, a foreign project, a store
// refusal - is published as a 200 envelope with success false and the symbolic
// code after `Error: ` (db-controller.ts:661-663, :256-259). The blueprint family
// binder already answers its caught names exactly that way
// (internal/transport/blueprints.go:21), and this file follows it.
//
// db:import-global-facts-commit has no catch at all: the controller throws and the
// renderer receives a rejected invocation (db-controller.ts:236-243). Its
// refusals are therefore errors, and every one of them is a symbolic constant, so
// the transport publishes the code instead of a SQLite sentence.
//
// # The single admission
//
// Every handler borrows the admitted author database through
// productionDurableOpen.borrowAuthorDatabase (production_durable_linux.go:313):
// the frozen context is parsed, the caller's cookie/tab identity is named, the
// project path must be one the startup inventory issued a catalog key for, and
// authorruntime re-proves the lease, the admission permit and the handle identity
// before any SQL runs. A stale lease or a foreign path is refused there, before
// the store opens its transaction, which is what makes those refusals leave the
// file byte-identical. The borrow is held until after the store call returns and
// its release closure is deferred by each handler, never by the helper.
//
// # What this file adds to the store's own limits
//
// The store cannot move the finalized chapter `.txt` files the reference sends to
// the project trash (project-clear-repository.ts:41-53); this file does not move
// them either, and reports physicalFilesDeleted as 0 rather than claiming a file
// deletion that never happened. The store also cannot retire blueprint recovery
// runs (generation-run-repository.ts:613-619, see internal/store/dbmaintenance.go),
// so it refuses the clear outright while a generation run is still running
// (BLUEPRINT_CLEAR_RUN_IN_FLIGHT) rather than let a live writer put the rows back.
// A run that is no longer running may still offer a recovery that re-creates
// blueprints after a clear: that can resurrect blueprint rows the author cleared,
// and it can never delete author content.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	productionDBMaintenanceBlueprintClearName = "db:blueprint-clear-all"
	productionDBMaintenanceProjectClearName   = "db:project-clear-generated-data"
	productionDBMaintenanceImportFactsName    = "db:import-global-facts-commit"

	// (expectedProjectPath, context): one payload slot, then the frozen context.
	productionDBMaintenanceBlueprintClearArity = 2
	productionDBMaintenanceBlueprintPathIndex  = 0

	// (options, expectedProjectPath, context) and (request, expectedProjectPath, context).
	productionDBMaintenanceProjectClearArity = 3
	productionDBMaintenanceProjectClearPath  = 1
	productionDBMaintenanceImportFactsArity  = 3
	productionDBMaintenanceImportFactsPath   = 1
)

// productionDBMaintenance is the adapter for the three names. It is stateless
// apart from the durable bridge: every request borrows the admitted author
// database, runs one store transaction and releases the borrow.
type productionDBMaintenance struct {
	durable *productionDurableOpen
}

func newProductionDBMaintenance(durable *productionDurableOpen) *productionDBMaintenance {
	return &productionDBMaintenance{durable: durable}
}

// Bind installs the three names with server.Bind, the exported single-name
// binder: the blueprint family binder publishes the whole blueprint surface and
// deliberately excludes clear-all, so binding through it is not possible without
// changing another family's file. A nil adapter, a nil durable bridge or a nil
// server is refused instead of being published as handlers that can only fail.
//
// The production lane calls this once, next to the other family binders, with
//
//	func() error { return newProductionDBMaintenance(open.durable).Bind(server) },
func (d *productionDBMaintenance) Bind(server *transport.Server) error {
	if d == nil || d.durable == nil || server == nil {
		return errors.New("PRODUCTION_DB_MAINTENANCE_BINDER_INVALID")
	}
	for _, bind := range []struct {
		name    string
		handler transport.Operation
	}{
		{productionDBMaintenanceBlueprintClearName, d.clearBlueprints},
		{productionDBMaintenanceProjectClearName, d.clearGeneratedData},
		{productionDBMaintenanceImportFactsName, d.commitImportGlobalFacts},
	} {
		if err := server.Bind(bind.name, bind.handler); err != nil {
			return err
		}
	}
	return nil
}

// clearBlueprints serves db:blueprint-clear-all (db-controller.ts:656-664):
// BlueprintRepository.clearAll in one transaction, answered `{ success: true }`.
// Calling it twice is the same success because the deletes have no receipt and
// nothing is left to delete the second time (blueprint-repository.ts:806-811).
func (d *productionDBMaintenance) clearBlueprints(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if _, err := productionDBMaintenanceArguments(args, productionDBMaintenanceBlueprintClearArity,
		productionDBMaintenanceBlueprintPathIndex); err != nil {
		return nil, err
	}
	db, release, err := d.borrow(ctx, caller, args, productionDBMaintenanceBlueprintClearArity)
	if release != nil {
		defer release()
	}
	if err != nil {
		return productionDBMaintenanceCaught(err)
	}
	if err := store.NewDatabaseMaintenance(db).ClearBlueprints(ctx); err != nil {
		// A live generation run is the one refusal this lane adds to the reference's
		// own answer, and it is a business answer rather than a server fault.
		if errors.Is(err, store.ErrGenerationRunInFlight) {
			return map[string]any{"success": false, "error": err.Error(),
				"errorCode": err.Error()}, nil
		}
		return productionDBMaintenanceCaught(err)
	}
	return map[string]any{"success": true}, nil
}

// clearGeneratedData serves db:project-clear-generated-data (db-controller.ts:245-260):
// the three scopes in the reference's order, answered
// `{ success: true, cleared, physicalFilesDeleted }`. The author's own content -
// project_core, characters, aliases, the roster meta and every draft, review and
// revision body outside the requested scopes - survives because the store deletes
// only the eight generated-text tables and (when asked) the three blueprint ones.
//
// Two refusals are this file's own and both happen before the transaction opens:
// an options argument that is not an object, and a `creativeFields` request whose
// roster still holds unadopted legacy cards. The second is the reference's
// adoption arm (project-clear-repository.ts:121-126), which this lane has no port
// for; clearing anyway would delete cards the author never had persisted.
func (d *productionDBMaintenance) clearGeneratedData(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	expected, err := productionDBMaintenanceArguments(args, productionDBMaintenanceProjectClearArity,
		productionDBMaintenanceProjectClearPath)
	if err != nil {
		return nil, err
	}
	options, ok := args[0].(map[string]any)
	if !ok || options == nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	db, release, err := d.borrow(ctx, caller, args, productionDBMaintenanceProjectClearArity)
	if release != nil {
		defer release()
	}
	if err != nil {
		return productionDBMaintenanceCaught(err)
	}
	result, err := store.NewDatabaseMaintenance(db).ClearGeneratedData(ctx, store.ClearGeneratedRequest{
		Options: store.ClearGeneratedOptions{
			GeneratedText:  productionDBMaintenanceFlag(options["generatedText"]),
			Blueprints:     productionDBMaintenanceFlag(options["blueprints"]),
			CreativeFields: productionDBMaintenanceFlag(options["creativeFields"]),
		},
		Scope: store.RosterAuthorScope{ProjectID: expected.ProjectID, Epoch: expected.LeaseID},
	})
	if err != nil {
		return productionDBMaintenanceCaught(err)
	}
	return map[string]any{
		"success": true,
		"cleared": result.Cleared,
		// The store owns SQL only; no chapter .txt file is moved by this lane.
		"physicalFilesDeleted": int64(0),
	}, nil
}

// commitImportGlobalFacts serves db:import-global-facts-commit
// (db-controller.ts:236-243) and answers the reference's own envelope,
// `{ success: true, receipt }`. The receipt is the identity-schema arm: the
// imported core, the ledger row and one staged character proposal, in one
// transaction, with the author's roster untouched.
//
// A second identical call is the same success with `idempotent: true`
// (import-global-facts-repository.ts:178), not a refusal; the same operation id
// with a different payload is IMPORT_GLOBAL_FACTS_OPERATION_CONFLICT (:175).
func (d *productionDBMaintenance) commitImportGlobalFacts(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	expected, err := productionDBMaintenanceArguments(args, productionDBMaintenanceImportFactsArity,
		productionDBMaintenanceImportFactsPath)
	if err != nil {
		return nil, err
	}
	request, ok := args[0].(map[string]any)
	if !ok || request == nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	db, release, err := d.borrow(ctx, caller, args, productionDBMaintenanceImportFactsArity)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, productionDBMaintenanceFailure(err)
	}
	receipt, err := store.NewDatabaseMaintenance(db).CommitGlobalFacts(ctx, expected.ProjectID, request)
	if err != nil {
		return nil, productionDBMaintenanceFailure(err)
	}
	return map[string]any{"success": true, "receipt": receipt}, nil
}

// productionDBMaintenanceArguments is the argument fence every name in this file
// shares: the exact arity, the frozen three-key context in the last slot, the path
// argument as a non-empty UTF-8 string, and that path equal to the context's own
// project path. It is the same fence the review family binder applies
// (internal/transport/reviewrevision.go:23) and the same projection the browser
// stores (projectsession.ParseContext).
func productionDBMaintenanceArguments(args []any, arity, pathIndex int) (projectsession.Context, error) {
	var expected projectsession.Context
	if len(args) != arity {
		return expected, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	expected, err := projectsession.ParseContext(args[arity-1])
	if err != nil {
		return expected, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	path, ok := args[pathIndex].(string)
	if !ok || path == "" || !utf8.ValidString(path) {
		return expected, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
	}
	if path != expected.ProjectPath {
		return expected, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	return expected, nil
}

// borrow is the one admission all three names use: the same durable borrow every
// other production write uses, so a stale lease, a path this tab never opened or a
// handle whose content identity moved refuses before any SQL runs. The caller owns
// the returned release closure and defers it after its own store call, because the
// borrow has to outlive the transaction that uses it.
func (d *productionDBMaintenance) borrow(ctx context.Context, caller transport.Caller, args []any, arity int) (*sql.DB, func(), error) {
	if d == nil || d.durable == nil || ctx == nil {
		return nil, nil, errors.New("PRODUCTION_DB_MAINTENANCE_UNAVAILABLE")
	}
	db, release, err := d.durable.borrowAuthorDatabase(ctx, caller, args[arity-1])
	if err != nil {
		if release != nil {
			release()
		}
		return nil, nil, err
	}
	if db == nil || release == nil {
		if release != nil {
			release()
		}
		return nil, nil, errors.New("PRODUCTION_DB_MAINTENANCE_UNAVAILABLE")
	}
	return db, release, nil
}

// productionDBMaintenanceFlag is the reference's truthiness read of one
// ProjectClearOptions key; anything that is not a boolean is falsy.
func productionDBMaintenanceFlag(value any) bool {
	flag, _ := value.(bool)
	return flag
}

// productionDBMaintenanceCaught is the answer the two try/catch handlers publish:
// a 200 envelope naming the refusal, never an error the transport would turn into
// a body the renderer cannot read. `Error: ` is what String(err) prepends to a
// symbolic code, and it is the shape internal/transport/blueprints.go already uses.
func productionDBMaintenanceCaught(err error) (any, error) {
	return map[string]any{"success": false, "error": "Error: " + productionDBMaintenanceCode(err)}, nil
}

// productionDBMaintenanceFailure keeps an uncaught refusal symbolic: a public HTTP
// error is rebuilt, a cancellation keeps its own code, a symbolic store or borrow
// code survives, and anything else becomes the family fallback instead of a raw
// SQLite sentence or a path.
func productionDBMaintenanceFailure(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return security.NewHTTPError(public.Status, public.Code)
	}
	if productionDBMaintenanceSymbolic(err.Error()) {
		return errors.New(err.Error())
	}
	return errors.New("PRODUCTION_DB_MAINTENANCE_FAILED")
}

// productionDBMaintenanceCode is the code a caught refusal names: a public HTTP
// error keeps its code, a symbolic message is already the code, and anything else
// is the family fallback.
func productionDBMaintenanceCode(err error) string {
	if err == nil {
		return "PRODUCTION_DB_MAINTENANCE_FAILED"
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil && public.Code != "" {
		return public.Code
	}
	if productionDBMaintenanceSymbolic(err.Error()) {
		return err.Error()
	}
	return "PRODUCTION_DB_MAINTENANCE_FAILED"
}

// productionDBMaintenanceSymbolic reports whether a message is one uppercase
// symbolic constant, which is the only shape this file publishes as a code.
func productionDBMaintenanceSymbolic(text string) bool {
	if len(text) < 2 || text[0] < 'A' || text[0] > 'Z' {
		return false
	}
	for index := 1; index < len(text); index++ {
		character := text[index]
		if (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '_' {
			continue
		}
		return false
	}
	return true
}
