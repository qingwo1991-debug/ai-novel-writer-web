//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the last name of the blueprint character-sync family:
//
//	db:blueprint-character-sync-complete   (arity 3, path 1, id 0)
//
// # The reference
//
//	electron/controllers/db-controller.ts:618-629
//	  operations.handle('db:blueprint-character-sync-complete', async (
//	    _event, operationId: string, expectedProjectPath: string,
//	  ) => {
//	    try {
//	      assertRequiredExpectedProjectPath(getCurrentProjectPath(), expectedProjectPath)
//	      return {
//	        success: true,
//	        operation: BlueprintRepository.completeCharacterSyncOperation(operationId),
//	      }
//	    } catch (err) {
//	      return { success: false, error: String(err) }
//	    }
//	  })
//
//	electron/repositories/blueprint-repository.ts:764-796
//	  completeCharacterSyncOperation: one immediate transaction that reads the row,
//	  returns a completed row unchanged after proving it, otherwise computes the
//	  authoritative receipt, updates `status='completed'` and the receipt column
//	  under `WHERE operation_id=? AND status='pending'`, reads the row back and
//	  proves the receipt that is now stored.
//
// The wire shape is therefore (operation id, project path, frozen context): the
// id is the typed leading argument, the path is the second, and the frozen
// three-key session is last - the same shape the family's single read already
// serves (production_db_sync_reads_linux.go:103-106), read off
// src/shared/ipc-channels.ts and cross-checked against the call site above.
//
// # Why the answer is an envelope here and an error in the sibling reads
//
// The reference wraps this handler in a try/catch and answers 200 with
// `{ success: false, error }`, while get/list-pending let the throw escape. That
// asymmetry is the reference's, so it is the one this file keeps: a refusal after
// the arguments are validated is the reference's own envelope, and only a
// malformed request or a foreign frozen context is a transport error, decided
// before any database is borrowed. The code inside the envelope is the family's
// mapper (productionDBSyncFailure, production_db_sync_reads_linux.go:226-244), so
// a raw SQLite sentence never reaches the browser and a symbolic store or lease
// sentinel does.
//
// # Authority
//
// Nothing here invents a proof. store.Blueprints.CompleteSync runs the roster
// proof, the receipt write and its read-back inside one BEGIN IMMEDIATE on the
// borrowed, already-admitted author database
// (internal/store/blueprint_sync_complete.go). This adapter owns exactly what
// every existing production mutation owns: the borrow installed by
// productionDurableOpen.borrowAuthorDatabase, which re-proves the cookie session,
// the tab, the frozen lease and the pinned v7 content identity before a
// repository is handed back. No lease means no repository, a stale lease means no
// SQL, and a refused request therefore cannot change a byte - the only statement
// that can precede a refusal is the reference's insert-only lazy repair.
type productionDBSyncComplete struct {
	durable *productionDurableOpen
}

const (
	productionDBSyncCompleteName      = "db:blueprint-character-sync-complete"
	productionDBSyncCompleteArity     = 3
	productionDBSyncCompletePathIndex = 1
	productionDBSyncCompleteIDIndex   = 0
)

// newProductionDBSyncComplete wraps the lane's durable bridge, the only object
// that can name the author database. The adapter cannot open a project, resolve a
// path or migrate a schema by itself.
func newProductionDBSyncComplete(durable *productionDurableOpen) *productionDBSyncComplete {
	return &productionDBSyncComplete{durable: durable}
}

// Bind installs the one name. It is the single-name binder rather than a family
// binder because every sibling in this family is already served elsewhere
// (production_db_sync_reads_linux.go binds the two reads and the write family
// binds db:blueprint-commit-range), so a family binder would collide. A nil
// adapter, a nil bridge and a nil server are refused instead of being published
// as handlers that can only fail, and Bind re-checks the name against the
// registry.
func (c *productionDBSyncComplete) Bind(server *transport.Server) error {
	if c == nil || c.durable == nil || server == nil {
		return errors.New("PRODUCTION_DB_SYNC_COMPLETE_BINDER_INVALID")
	}
	return server.Bind(productionDBSyncCompleteName, c.complete())
}

// complete serves db:blueprint-character-sync-complete. Argument order, the arity
// check, the context/path comparison and the typed leading id are copied from the
// family's read binder (production_db_sync_reads_linux.go:150-217), so the
// renderer sees one contract whichever name of the family answers it. The borrow
// happens only after every argument has been validated, so a malformed or foreign
// request never reaches the admitted handle.
func (c *productionDBSyncComplete) complete() transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if c == nil || c.durable == nil {
			return nil, errors.New("PRODUCTION_DB_SYNC_COMPLETE_UNAVAILABLE")
		}
		if len(args) != productionDBSyncCompleteArity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := projectsession.ParseContext(args[productionDBSyncCompleteArity-1])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		path, ok := args[productionDBSyncCompletePathIndex].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		if path != expected.ProjectPath {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		// The store owns the id's own emptiness rule
		// (BLUEPRINT_CHARACTER_SYNC_OPERATION_ID_REQUIRED), exactly like the
		// reference: this only refuses a non-string.
		id, ok := args[productionDBSyncCompleteIDIndex].(string)
		if !ok {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if ctx == nil {
			return productionDBSyncCompleteRefusal(errors.New("BLUEPRINT_OPERATION_FAILED")), nil
		}
		db, release, err := c.durable.borrowAuthorDatabase(ctx, caller, args[productionDBSyncCompleteArity-1])
		if release != nil {
			defer release()
		}
		if err != nil {
			return productionDBSyncCompleteRefusal(err), nil
		}
		if db == nil || release == nil {
			return productionDBSyncCompleteRefusal(errors.New("BLUEPRINT_PROVIDER_UNAVAILABLE")), nil
		}
		// One store call: the proof, the receipt column and the completed status are
		// one transaction in the store, so this file cannot compose a half-write.
		operation, err := store.NewBlueprints(db).CompleteSync(ctx, id)
		if err != nil {
			return productionDBSyncCompleteRefusal(err), nil
		}
		// The reference's success envelope, verbatim (:623-626).
		return map[string]any{"success": true, "operation": operation}, nil
	}
}

// productionDBSyncCompleteRefusal is the reference's catch branch: 200, the
// success flag false, and the symbolic code inside the message the renderer
// already prints. The code is never a pointer, a path or a SQLite sentence.
func productionDBSyncCompleteRefusal(err error) map[string]any {
	return map[string]any{
		"success": false,
		"error":   "Error: " + productionDBSyncCompleteCode(err),
	}
}

// productionDBSyncCompleteCode reuses the family's mapper so this name and its two
// reads cannot disagree about which refusals are symbolic or about the fallback.
func productionDBSyncCompleteCode(err error) string {
	if err == nil {
		return "BLUEPRINT_OPERATION_FAILED"
	}
	return productionDBSyncFailure(err).Error()
}
