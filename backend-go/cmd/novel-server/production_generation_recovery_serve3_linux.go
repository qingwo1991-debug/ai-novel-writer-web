//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file SERVES the last two recovery names. The earlier passes refused them
// because three of list-recovery's five buckets had no port and find-blueprint-
// recovery's context had no port. This pass settles both with the answer the
// reference itself gives for a lane that cannot carry a planning continuation:
//
//	generation:list-recovery            SERVED  five required buckets; editors and
//	                                            batches real, planning/drafts/
//	                                            reviews [], partial false
//	generation:find-blueprint-recovery  SERVED  null, because no run carries a
//	                                            non-complete planning continuation
//
// The decisive reader is store.FindDirectoryRecoveryNeed
// (internal/store/planning_continuation.go): it reads
// binding_json['$runEffects']['planningContinuation'] the way the reference does
// and reports absent when the run has none. In this lane no run can carry one -
// the ledger refuses every glPlanning operation that could write the effect
// (internal/store/generation_ledger.go) - so the honest answer for every run is
// absent, and that answer is a real SELECT of the stored binding, never a
// hardcoded false.
//
// # Editors and batches are the served sibling projections
//
// editors reuses productionEditorInline.recovery per editor-inline run, and
// batches reuses store.ListGenerationBatchHistory per project - the two names the
// bucket table in production_generation_recovery_reads_linux.go already recorded
// as served by siblings. planning/drafts/reviews stay [] because their receipts
// are absent, not because the reads failed, so partial is false.

// productionGenerationRecoveryOwner is the one generation owner the editors
// bucket projects through. It is set inside the owner block of
// production_open_linux.go (bindProductionGenerationRecoveryOwner), which runs
// before bindProductionGenerationRecovery reaches this file's handlers, and it
// is captured at bind time so each lane's handler closes over its own owner.
var productionGenerationRecoveryOwner *productionGeneration

// bindProductionGenerationRecoveryOwner records the owner the editors bucket
// projects through. It is the owner-block half of the family's wiring: it must
// be called where `owner` is in scope, before the storeful
// bindProductionGenerationRecovery call outside the block.
func bindProductionGenerationRecoveryOwner(owner *productionGeneration, server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	productionGenerationRecoveryOwner = owner
	return nil
}

// productionGenerationRecoveryServe carries the two half-wired dependencies the
// served names need: the durable open for the batches bucket and
// find-blueprint-recovery, and the editor-inline projector for the editors
// bucket.
type productionGenerationRecoveryServe struct {
	durable *productionDurableOpen
	editor  *productionEditorInline
}

// bindProductionGenerationRecoveryServed installs the two names for real. It is
// called by bindProductionGenerationRecovery after the owner block has recorded
// the owner.
func bindProductionGenerationRecoveryServed(durable *productionDurableOpen, server *transport.Server) error {
	if durable == nil {
		return errors.New("PRODUCTION_DURABLE_INVALID")
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	serve := &productionGenerationRecoveryServe{
		durable: durable,
		editor:  newProductionEditorInline(productionGenerationRecoveryOwner),
	}
	if err := server.Bind(productionGenerationRecoveryList, serve.list()); err != nil {
		return err
	}
	return server.Bind(productionGenerationRecoveryFind, serve.find())
}

// list answers generation:list-recovery with the reference's five-bucket shape
// (src/shared/generation-owner-contract.ts:254-262). Arity is the request arity
// zero plus the frozen project session at args[0].
func (r *productionGenerationRecoveryServe) list() transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if r == nil || r.durable == nil {
			return nil, errors.New("PRODUCTION_BATCH_UNAVAILABLE")
		}
		if len(args) != 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := projectsession.ParseContext(args[0])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		out := productionRecoveryListResult{
			Planning: []map[string]any{},
			Editors:  []map[string]any{},
			Reviews:  []map[string]any{},
			Batches:  []store.GenerationBatchHistory{},
			Drafts:   []map[string]any{},
			Partial:  false,
		}
		// The batches bucket is the served generation:list-batches projection.
		batches, err := r.batches(ctx, caller, args[0], expected.ProjectID)
		if err != nil {
			return productionBatchFailure(err)
		}
		out.Batches = batches
		// The editors bucket is the served editor-inline:read-recovery projection.
		editors, err := r.editors(ctx, caller, args[0], expected.ProjectID)
		if err != nil {
			return productionBatchFailure(err)
		}
		out.Editors = editors
		return out, nil
	}
}

// find answers generation:find-blueprint-recovery. It returns null when no
// chapter-blueprint-directory run carries a non-complete planning continuation,
// which is every run this lane can read, and it honors the reference's
// writeState:'unavailable' branch (main-generation-owner.ts:935-937) instead of
// skipping it into null.
func (r *productionGenerationRecoveryServe) find() transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if r == nil || r.durable == nil {
			return nil, errors.New("PRODUCTION_BATCH_UNAVAILABLE")
		}
		if len(args) != 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := projectsession.ParseContext(args[0])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, args[0])
		if release != nil {
			defer release()
		}
		if err != nil {
			return productionBatchFailure(err)
		}
		if db == nil || release == nil {
			return nil, errors.New("PRODUCTION_DURABLE_INVALID")
		}
		need, err := store.FindDirectoryRecoveryNeed(ctx, db, expected.ProjectID)
		if err != nil {
			return productionBatchFailure(err)
		}
		if need == nil {
			// The reference's own null: nothing to recover.
			return nil, nil
		}
		if need.Unavailable {
			// :935-937: a run whose blueprint recovery is unavailable is answered,
			// not skipped. Only the unavailable union is projectable here; the
			// live-candidate draft is the empty text a directory run carries.
			return productionFindUnavailableContext(need, expected.LeaseID), nil
		}
		// A proved planning continuation would need the continued writeState,
		// which is decided after the rebuildBinding comparison this lane has no
		// port of (production_generation_recovery_serve_linux.go). Refuse rather
		// than project a state this lane cannot derive.
		return nil, errors.New("GENERATION_PLANNING_CONTINUATION_UNPROJECTABLE")
	}
}

// batches is store.ListGenerationBatchHistory, the served generation:list-batches
// projection, borrowed from the durable open under the frozen session.
func (r *productionGenerationRecoveryServe) batches(ctx context.Context, caller transport.Caller, raw any, projectID string) ([]store.GenerationBatchHistory, error) {
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, raw)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, err
	}
	if db == nil || release == nil {
		return nil, errors.New("PRODUCTION_DURABLE_INVALID")
	}
	return store.ListGenerationBatchHistory(ctx, db, projectID)
}

// editors projects every editor-inline run of the project through the served
// editor-inline:read-recovery projection. A lane without an owner answers [].
func (r *productionGenerationRecoveryServe) editors(ctx context.Context, caller transport.Caller, raw any, projectID string) ([]map[string]any, error) {
	editors := []map[string]any{}
	if r.editor == nil || !r.editor.ready() {
		return editors, nil
	}
	_, scope, ledger, db, done, err := r.editor.owner.inner.admit(ctx, caller, raw)
	if err != nil {
		return nil, err
	}
	defer done()
	runs, err := ledger.ListRuns(ctx, projectID)
	if err != nil {
		return nil, err
	}
	for _, summary := range runs {
		if syntheticString(summary["operation"]) != productionEditorInlineOperation {
			continue
		}
		handle := syntheticHandle{
			ProjectID:    syntheticString(summary["projectId"]),
			Epoch:        syntheticString(summary["epoch"]),
			RootActionID: syntheticString(summary["rootActionId"]),
			RunID:        syntheticString(summary["runId"]),
		}
		recovery, err := r.editor.recovery(ctx, scope, ledger, db, handle)
		if err != nil {
			return nil, err
		}
		editors = append(editors, syntheticMap(recovery))
	}
	return editors, nil
}

// productionRecoveryListResult is GenerationRecoveryList
// (src/shared/generation-owner-contract.ts:254-262): five required arrays plus a
// boolean, no bucket optional and no bucket nullable.
type productionRecoveryListResult struct {
	Planning []map[string]any                 `json:"planning"`
	Editors  []map[string]any                 `json:"editors"`
	Reviews  []map[string]any                 `json:"reviews"`
	Batches  []store.GenerationBatchHistory   `json:"batches"`
	Drafts   []map[string]any                 `json:"drafts"`
	Partial  bool                             `json:"partial"`
}

// productionFindUnavailableContext builds the reference's writeState:'unavailable'
// context (main-generation-owner.ts:935-937). The fields outside the blueprint
// union are the run's own frozen values; the live-candidate draft and the source
// currency are not projected for an unavailable run, which is the one branch that
// needs neither.
func productionFindUnavailableContext(need *store.DirectoryRecoveryNeed, leaseEpoch string) map[string]any {
	return map[string]any{
		"modelId":   need.Run.ModelID,
		"handle":    map[string]any{"projectId": need.Run.ProjectID, "epoch": need.Run.Epoch, "rootActionId": need.Run.RootActionID, "runId": need.Run.RunID},
		"operation": "chapter-blueprint-directory",
		"authorInputs":               need.Run.AuthorInputs,
		"selectedDraftIds":           []int64{},
		"selectedFinalizedDraftIds":  []int64{},
		"selectedBlueprintChapterNumbers": need.Run.SelectedBlueprintChapterNumbers,
		"composition":                nil,
		"lastCompositionFinishReason": nil,
		"attemptedPurposes":          []string{},
		"draftSave":                  map[string]any{"kind": "absent"},
		"blueprintRecovery": map[string]any{
			"sourceHandle": map[string]any{"projectId": need.Run.ProjectID, "epoch": need.Run.Epoch, "rootActionId": need.Run.RootActionID, "runId": need.Run.RunID},
			"leaseEpoch":   leaseEpoch,
			"draft":        "",
			"editRange":    nil,
			"saved":        nil,
			"writeState":   "unavailable",
			"diagnostic":   need.Diagnostic,
		},
	}
}
