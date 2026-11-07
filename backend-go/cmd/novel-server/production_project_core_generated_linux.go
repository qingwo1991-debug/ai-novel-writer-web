//go:build linux

package main

// This file serves db:project-core-commit-generated, the db family's generated
// core write, from the store transaction in
// internal/store/projectcore_generated_tx.go.
//
// # The reference, and the wire shape
//
//	electron/controllers/db-controller.ts:197-209
//	  operations.handle('db:project-core-commit-generated', async (
//	    _event, request: { data: Partial<ProjectCoreData>; generationRunHandle: MainGenerationRunHandle }, expectedProjectPath: string,
//	  ) => {
//	    try {
//	      assertRequiredExpectedProjectPath(getCurrentProjectPath(), expectedProjectPath)
//	      const database = getProjectDb()
//	      if (!database || !request.generationRunHandle) throw new Error('GENERATION_OWNER_REQUIRED')
//	      database.transaction(() => withGenerationAgentChildEffect(request.generationRunHandle, () => {
//	        assertGenerationSourcesCurrent(request.generationRunHandle)
//	        ProjectCoreRepository.update(request.data)
//	      })).immediate()
//	      return { success: true }
//	    } catch (error) { return { success: false, error: String(error) } }
//	  })
//
// so the arguments are (request, expectedProjectPath, context): arity three, the
// frozen project path at index one, and the request is `{ data,
// generationRunHandle }` (src/shared/ipc-channels.ts:913-916). The two renderer
// call sites are src/stores/project-store.ts:973 (commitGeneratedNovelConfig, the
// architecture workflow's formal commit) and
// src/services/workflows/commands/architecture.command.ts:663, which commits one
// architecture field with the generation run that produced it.
//
// # The answer
//
// Both arms of the controller are 200 envelopes: `{ success: true }` (:208) or
// `{ success: false, error: String(error) }` (:209), and String(err) prefixes a
// thrown Error with `Error: `. Nothing here can become a 5xx, so this adapter
// publishes the same envelope the family's own binder publishes for
// db:project-core-update (internal/transport/projectcore.go:25, :38) instead of
// re-deriving an error shape. A replay under the same run handle reaches the same
// line: there is no operation ledger on this channel and no duplicate branch, so
// the second identical call re-runs the unconditional UPDATE and answers
// `{ success: true }` again (:208).
//
// # The admission
//
// Every request borrows the admitted author database through
// productionDurableOpen.borrowAuthorDatabase (production_durable_linux.go:313):
// the frozen three-key context is parsed, the caller's cookie/tab identity is
// named, the path must be one the startup inventory issued a catalog key for, and
// authorruntime re-proves the lease, the admission permit and the handle identity
// before any SQL runs. A foreign path or a stale lease is refused there, before
// the store opens its transaction, which is what makes those refusals leave the
// file byte-identical. The borrow is held until after the store call returns and
// its release closure is deferred here, never inside the helper.
//
// # What this file does not raise
//
//   - GENERATION_OWNER_REQUIRED is answered here, before the borrow, because the
//     reference raises it at :203 on the request alone (`!request.generationRunHandle`).
//   - GENERATION_AGENT_CHILD_EFFECT_UNPORTED is the store's own refusal for a run
//     that carries agentWorkflowRegistrationId; the header of
//     internal/store/projectcore_generated_tx.go states why this lane refuses that
//     stamp rather than inventing it.
//   - GENERATION_SOURCE_CHANGED is not raised: its last step rebuilds the frozen
//     binding, and no Go file ports rebuildBinding (the same reduction
//     internal/store/draft_commit_tx.go:54-58 already records).

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	productionProjectCoreGeneratedName = "db:project-core-commit-generated"

	// (request, expectedProjectPath, context): one payload slot, the path, then
	// the frozen three-key session.
	productionProjectCoreGeneratedArity = 3
	productionProjectCoreGeneratedPath  = 1

	// productionProjectCoreGeneratedUnavailable is this lane's own code for a
	// bridge that handed back no database or no release closure. It never names a
	// path or a SQLite sentence.
	productionProjectCoreGeneratedUnavailable = "PROJECT_CORE_GENERATED_UNAVAILABLE"

	// productionProjectCoreGeneratedFailed is the fallback for a failure that is
	// not symbolic. It is the family's existing fallback
	// (internal/transport/projectcore.go:53) rather than a new vocabulary.
	productionProjectCoreGeneratedFailed = "PROJECT_CORE_OPERATION_FAILED"
)

// productionProjectCoreGenerated is the adapter for the one name: it is
// stateless apart from the durable bridge, so every request borrows the admitted
// author database, runs one store transaction and releases the borrow.
type productionProjectCoreGenerated struct {
	durable *productionDurableOpen
}

func newProductionProjectCoreGenerated(durable *productionDurableOpen) *productionProjectCoreGenerated {
	return &productionProjectCoreGenerated{durable: durable}
}

// Bind installs exactly one name with server.Bind, the exported single-name
// binder (internal/transport/server.go:73): BindProjectCore deliberately excludes
// this write (internal/transport/projectcore.go:17-19), so binding through that
// family binder is not possible without changing another family's file. A nil
// adapter, a nil durable bridge or a nil server is refused instead of being
// published as a handler that can only fail.
func (p *productionProjectCoreGenerated) Bind(server *transport.Server) error {
	if p == nil || p.durable == nil || server == nil {
		return errors.New("PRODUCTION_PROJECT_CORE_GENERATED_BINDER_INVALID")
	}
	return server.Bind(productionProjectCoreGeneratedName, p.commit)
}

// commit serves db:project-core-commit-generated. The proof order is the
// reference's: the request envelope and the path (:201), the handle (:203), the
// admission, then the store's run identity, epoch and root proofs, and the core
// row last (:206) - so a request naming charactersArch with a stale handle answers
// the handle's refusal, exactly as the reference does.
func (p *productionProjectCoreGenerated) commit(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if p == nil || p.durable == nil {
		return productionProjectCoreGeneratedCaught(errors.New(productionProjectCoreGeneratedUnavailable))
	}
	expected, err := productionDBMaintenanceArguments(args, productionProjectCoreGeneratedArity, productionProjectCoreGeneratedPath)
	if err != nil {
		return productionProjectCoreGeneratedCaught(err)
	}
	request, ok := args[0].(map[string]any)
	if !ok || request == nil {
		return productionProjectCoreGeneratedCaught(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
	}
	// `request.data` reaches Object.hasOwn in ProjectCoreRepository.update:158, so
	// an absent or non-object data is a refusal, not an empty update.
	data, ok := request["data"].(map[string]any)
	if !ok || data == nil {
		return productionProjectCoreGeneratedCaught(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
	}
	// :203 `if (!database || !request.generationRunHandle) throw new
	// Error('GENERATION_OWNER_REQUIRED')`.
	raw, present := request["generationRunHandle"]
	if !present || raw == nil {
		return productionProjectCoreGeneratedCaught(store.ErrGenerationOwnerRequired)
	}
	handle, ok := productionProjectCoreGeneratedHandle(raw)
	if !ok {
		return productionProjectCoreGeneratedCaught(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
	}
	db, release, err := productionProjectCoreGeneratedBorrow(ctx, p.durable, caller, args[productionProjectCoreGeneratedArity-1])
	if release != nil {
		defer release()
	}
	if err != nil {
		return productionProjectCoreGeneratedCaught(err)
	}
	if err := store.NewProjectCore(db).CommitGenerated(ctx, store.ProjectCoreGeneratedRequest{
		Data:      data,
		Handle:    handle,
		ProjectID: expected.ProjectID,
		LiveEpoch: expected.LeaseID,
	}); err != nil {
		return productionProjectCoreGeneratedCaught(err)
	}
	// :208 `return { success: true }`.
	return map[string]any{"success": true}, nil
}

// productionProjectCoreGeneratedHandle is the four-key MainGenerationRunHandle
// (src/services/generation/generation-runtime.ts:77-82). A field of the wrong JSON
// type becomes the empty string rather than a fabricated value, so the store's
// run lookup or its identity comparison refuses it.
func productionProjectCoreGeneratedHandle(value any) (store.BatchRunHandle, bool) {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return store.BatchRunHandle{}, false
	}
	return store.BatchRunHandle{
		ProjectID:    syntheticString(object["projectId"]),
		Epoch:        syntheticString(object["epoch"]),
		RootActionID: syntheticString(object["rootActionId"]),
		RunID:        syntheticString(object["runId"]),
	}, true
}

// productionProjectCoreGeneratedBorrow is the same admission the db maintenance
// family uses: the durable borrow, with the release closure handed to this
// handler so it outlives the transaction that runs under it. A bridge that
// returns no database or no closure is the lane's own unavailable code, never a
// panic and never a half-admitted write.
func productionProjectCoreGeneratedBorrow(ctx context.Context, durable *productionDurableOpen, caller transport.Caller, raw any) (*sql.DB, func(), error) {
	if durable == nil || ctx == nil {
		return nil, nil, errors.New(productionProjectCoreGeneratedUnavailable)
	}
	db, release, err := durable.borrowAuthorDatabase(ctx, caller, raw)
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
		return nil, nil, errors.New(productionProjectCoreGeneratedUnavailable)
	}
	return db, release, nil
}

// productionProjectCoreGeneratedCaught is the controller's catch arm (:209): a
// 200 envelope naming the refusal, never an error the transport would turn into a
// body the renderer cannot read.
func productionProjectCoreGeneratedCaught(err error) (any, error) {
	return map[string]any{"success": false, "error": "Error: " + productionProjectCoreGeneratedCode(err)}, nil
}

// productionProjectCoreGeneratedCode is the code a refusal names: a public HTTP
// error keeps its own code (the argument fence), a symbolic store or admission
// code is already the code, and anything else becomes the family fallback instead
// of a raw SQLite sentence.
func productionProjectCoreGeneratedCode(err error) string {
	if err == nil {
		return productionProjectCoreGeneratedFailed
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil && public.Code != "" {
		return public.Code
	}
	if productionDBMaintenanceSymbolic(err.Error()) {
		return err.Error()
	}
	return productionProjectCoreGeneratedFailed
}
