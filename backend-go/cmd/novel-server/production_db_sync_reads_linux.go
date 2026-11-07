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

// This file serves the two blueprint character-sync reads that the previous db
// pass refused because the store repairs the baseline as it reads:
//
//	db:blueprint-character-sync-list-pending  (arity 2, path 0)
//	db:blueprint-character-sync-get           (arity 3, path 1)
//
// # The reference really does repair on the read path
//
// Both reference methods call ensureBlueprintCommitSchema(db) before their
// SELECT:
//
//   - listPendingCharacterSyncOperations, electron/repositories/
//     blueprint-repository.ts:732-734
//     ("static listPendingCharacterSyncOperations", then
//     "ensureBlueprintCommitSchema(db)").
//   - getCharacterSyncOperation, electron/repositories/blueprint-repository.ts:
//     755-758, which does the same immediately after its operation-id check.
//
// That helper is not schema DDL: it is one idempotent data repair, an
// INSERT OR IGNORE ... SELECT over the already-admitted commit ledger
//
//	INSERT OR IGNORE INTO blueprint_character_sync_operations (
//	  operation_id, blueprint_commit_operation_id, blueprint_commit_payload_hash,
//	  start_chapter, end_chapter, character_sync_input)
//	SELECT 'blueprint-sync-' || operation_id, operation_id, payload_hash,
//	  start_chapter, end_chapter, character_sync_input
//	FROM blueprint_commit_operations;
//
// (electron/repositories/blueprint-repository.ts:176-194). The write side calls
// the same helper (:561 getCommittedRangeOperation, :607 commitRange), so the
// repair is not a read/write asymmetry the port introduced; it is how the
// reference makes a commit ledger written by an older build readable at all.
//
// store.Blueprints.PendingSync and .GetSync already mirror that statement
// exactly (internal/store/blueprints.go:137-143, executed by PendingSync at :167
// and GetSync at :174), which is why this file binds the existing store methods
// instead of inventing a second repair.
//
// # Why the repair is safe, and why it is visible here
//
//   - Idempotent. The generated key is deterministic ('blueprint-sync-' ||
//     operation_id), it is the table's PRIMARY KEY, and the statement is
//     INSERT OR IGNORE, so a second call inserts nothing. The test below calls
//     each read twice and re-counts the rows.
//   - It cannot touch an author-written row. The statement only INSERTs; it has
//     no UPDATE, no DELETE and no ON CONFLICT clause. A row that already exists
//     under the generated key is skipped, so the author's own status, input,
//     completion receipt and timestamps are never overwritten.
//   - A failed repair is never an empty answer. The store returns the
//     ExecContext error before it runs either SELECT (:167, :174), and the
//     handler below maps any non-symbolic error to BLUEPRINT_OPERATION_FAILED
//     instead of projecting an empty list. Only a successful repair followed by
//     a successful SELECT can answer 200.
//   - The borrow is the lane's writable borrow, not a read-only handle:
//     productionDurableOpen.borrowAuthorDatabase
//     (production_durable_linux.go:313) re-proves the cookie session, the tab,
//     the frozen lease and the pinned v7 content identity, and its release
//     closure adopts the content identity the write committed. The blueprint
//     and project-core writes already use exactly this borrow
//     (production_blueprint_write_linux.go:96-108), and the synthetic lane
//     already serves these two names through the identical seam
//     (synthetic_durable_open_linux.go:606-620). Nothing here opens a database
//     or resolves a path.
//
// # What is still not mirrored
//
// GetSync refuses a completed row with GENERATION_OWNER_REQUIRED
// (internal/store/blueprints.go:178). The reference instead proves the row
// against the authoritative character-sync roster
// (assertAuthoritativeCharacterSyncCompletion,
// electron/repositories/blueprint-repository.ts:420) and returns it. That owner
// is not ported, so this lane serves the pending rows the recovery flow actually
// reads (src/services/workflows/directory-character-sync-recovery.ts:37-60) and
// keeps the completed-row refusal symbolic. The test pins that refusal so it
// cannot regress into a silent success.

const (
	productionDBSyncPendingName = "db:blueprint-character-sync-list-pending"
	productionDBSyncGetName     = "db:blueprint-character-sync-get"

	// The two wire shapes, read off src/shared/ipc-channels.ts:984-991 and the
	// handlers at electron/controllers/db-controller.ts:603-616: the pending list
	// is (project path, context), the single read is (operation id, path, context),
	// and the frozen three-key context is always the LAST argument.
	productionDBSyncPendingArity     = 2
	productionDBSyncPendingPathIndex = 0
	productionDBSyncGetArity         = 3
	productionDBSyncGetPathIndex     = 1
	productionDBSyncIDIndex          = 0
)

// productionDBSyncReads is the adapter for the two names. It is stateless: every
// request borrows the admitted author database through the lane's own bridge, so
// a lane opened after a refused startup (or one whose lease moved) answers a
// symbolic refusal instead of holding a handle this process no longer owns.
type productionDBSyncReads struct {
	durable *productionDurableOpen
}

func newProductionDBSyncReads(durable *productionDurableOpen) *productionDBSyncReads {
	return &productionDBSyncReads{durable: durable}
}

// Bind installs the two reads with server.Bind, the exported single-name binder.
// The family binder cannot be used: BindBlueprintReads publishes db:blueprint-get
// too, which this lane already binds, and BindBlueprintGetRead deliberately
// publishes only that one name. A nil adapter or server is refused instead of
// being published as handlers that can only fail, and Bind re-checks each name
// against the registry.
func (r *productionDBSyncReads) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil || server == nil {
		return errors.New("PRODUCTION_DB_SYNC_BINDER_INVALID")
	}
	for _, bind := range []struct {
		name    string
		handler transport.Operation
	}{
		{productionDBSyncPendingName, r.read(productionDBSyncPendingName)},
		{productionDBSyncGetName, r.read(productionDBSyncGetName)},
	} {
		if err := server.Bind(bind.name, bind.handler); err != nil {
			return err
		}
	}
	return nil
}

// read serves one of the two names. Argument order, the arity check, the
// context/path comparison and the typed leading id are copied from the family's
// read binder (internal/transport/blueprints_read.go:113-145), so the renderer
// sees one contract whichever binder answers it. The borrow happens only after
// every argument has been validated, so a malformed or foreign request never
// reaches the admitted handle.
func (r *productionDBSyncReads) read(operation string) transport.Operation {
	arity, pathIndex := productionDBSyncPendingArity, productionDBSyncPendingPathIndex
	if operation == productionDBSyncGetName {
		arity, pathIndex = productionDBSyncGetArity, productionDBSyncGetPathIndex
	}
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if r == nil || r.durable == nil {
			return nil, errors.New("PRODUCTION_DB_SYNC_UNAVAILABLE")
		}
		if len(args) != arity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := projectsession.ParseContext(args[arity-1])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		path, ok := args[pathIndex].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		if path != expected.ProjectPath {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		// db:blueprint-character-sync-get is the one typed leading argument. The
		// store still owns the id's own emptiness rule
		// (BLUEPRINT_CHARACTER_SYNC_OPERATION_ID_REQUIRED), exactly like the
		// reference, so this only refuses a non-string.
		id := ""
		if operation == productionDBSyncGetName {
			if id, ok = args[productionDBSyncIDIndex].(string); !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		}
		if ctx == nil {
			return nil, errors.New("BLUEPRINT_OPERATION_FAILED")
		}
		db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, args[arity-1])
		if release != nil {
			defer release()
		}
		if err != nil {
			return nil, productionDBSyncFailure(err)
		}
		if db == nil || release == nil {
			return nil, errors.New("BLUEPRINT_PROVIDER_UNAVAILABLE")
		}
		repository := store.NewBlueprints(db)
		var result any
		switch operation {
		case productionDBSyncPendingName:
			// PendingSync performs the reference's lazy repair as its first
			// statement and returns the rows in the reference's own order
			// (created_at ASC, operation_id ASC).
			result, err = repository.PendingSync(ctx)
		case productionDBSyncGetName:
			// GetSync performs the same repair, then one WHERE operation_id=?.
			// A missing id stays the reference's nullable null; a completed row
			// stays the symbolic refusal documented above.
			result, err = repository.GetSync(ctx, id)
		default:
			return nil, errors.New("BLUEPRINT_OPERATION_FAILED")
		}
		if err != nil {
			return nil, productionDBSyncFailure(err)
		}
		return result, nil
	}
}

// productionDBSyncFailure keeps a refusal symbolic. It is the family binder's
// own mapping (internal/transport/blueprints_read.go:186-222): cancellation and
// deadlines keep their codes, a public HTTP error is rebuilt rather than
// forwarded verbatim, a symbolic store or borrow code survives, and any other
// text - a raw SQLite detail above all - is replaced by the family's fallback.
// That fallback is what makes a failed repair fail the read instead of
// presenting an empty list.
func productionDBSyncFailure(err error) error {
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
	if productionDBSyncSymbolic(err.Error()) {
		return errors.New(err.Error())
	}
	return errors.New("BLUEPRINT_OPERATION_FAILED")
}

// productionDBSyncSymbolic reports whether a message is one uppercase symbolic
// constant, which is the only shape a store sentinel or a lease refusal has. A
// wrapped path, a SQLite sentence or a Go error with spaces never matches, so
// it can never travel to the browser as a code.
func productionDBSyncSymbolic(text string) bool {
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
