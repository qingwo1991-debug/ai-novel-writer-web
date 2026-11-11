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
// recovery's context had no port. This pass settles the planning continuation:
//
//	generation:list-recovery            SERVED  five required buckets; editors and
//	                                            batches sibling projections, planning
//	                                            the directory recovery contexts,
//	                                            drafts/reviews [], partial false
//	generation:find-blueprint-recovery  SERVED  the newest unavailable or continued
//	                                            directory recovery context, else null
//
// The decisive reader is store.FindDirectoryRecoveryNeed
// (internal/store/planning_continuation.go): it reads
// binding_json['$runEffects']['planningContinuation'] the way the reference does
// and reports absent when the run has none. A proved continuation short-circuits
// planningContinuation to state 'continued' BEFORE any fingerprint compare
// (main-generation-owner.ts:787), so this lane can project it from the stored
// receipts alone: the planning continuation, its remaining range, and the
// blueprint author-edit receipt readBlueprintRecovery returns as authorEdit.
//
// # Editors and batches are the served sibling projections
//
// editors reuses productionEditorInline.recovery per editor-inline run, and
// batches reuses store.ListGenerationBatchHistory per project - the two names the
// bucket table in production_generation_recovery_reads_linux.go already recorded
// as served by siblings. drafts/reviews stay [] because their ports are absent,
// not because the reads failed, so partial stays false for them.

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
		// The planning bucket is the reference's listRecovery collection
		// (main-generation-owner.ts:1021-1024): contexts whose blueprint recovery
		// is unavailable or whose planning continuation is not complete.
		planning, partial, err := r.planning(ctx, caller, args[0], expected.ProjectID, expected.LeaseID)
		if err != nil {
			return productionBatchFailure(err)
		}
		out.Planning = planning
		out.Partial = partial
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
// chapter-blueprint-directory run has an unavailable blueprint recovery or a
// proved planning continuation, and otherwise returns the newest matching
// context. The continued arm is the reference's own short-circuit
// (main-generation-owner.ts:787); the writeState arms that need the nine-field
// rebuildBinding comparison are deliberately not attempted.
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
		if need.Continuation == nil {
			// A non-continuation need is not a shape this reader produces; the
			// writeState arms behind it are the nine-field rebuildBinding comparison
			// this lane has no port of (production_generation_recovery_serve_linux.go).
			// Keep the refusal symbolic instead of inventing a state.
			return nil, errors.New("GENERATION_PLANNING_CONTINUATION_UNPROJECTABLE")
		}
		// planningContinuation short-circuits a proved receipt to 'continued'
		// before any fingerprint compare (main-generation-owner.ts:787), so the
		// continued context is projectable from the stored receipts alone.
		return productionFindContinuedContext(need, expected.LeaseID), nil
	}
}

// planning is the reference's listRecovery planning bucket
// (main-generation-owner.ts:1021-1024): every directory run whose readContext
// sets blueprintRecovery and whose planning continuation is not complete. The
// unavailable branch and the proved-continuation branch are the two this lane can
// derive; a per-run failure is reported as partial, exactly like the reference's
// catch, and does not stop the list.
func (r *productionGenerationRecoveryServe) planning(ctx context.Context, caller transport.Caller, raw any, projectID, leaseEpoch string) ([]map[string]any, bool, error) {
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, raw)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, false, err
	}
	if db == nil || release == nil {
		return nil, false, errors.New("PRODUCTION_DURABLE_INVALID")
	}
	needs, partial, err := store.ListDirectoryRecoveryNeeds(ctx, db, projectID)
	if err != nil {
		return nil, false, err
	}
	planning := make([]map[string]any, 0, len(needs))
	for _, need := range needs {
		if need.Unavailable {
			planning = append(planning, productionFindUnavailableContext(need, leaseEpoch))
			continue
		}
		planning = append(planning, productionFindContinuedContext(need, leaseEpoch))
	}
	return planning, partial, nil
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

// productionFindContinuedContext builds the reference's continued context
// (main-generation-owner.ts:786-787, :938-942). planningContinuation is the
// short-circuit value: state 'continued', the child handle and the remaining
// range derived from the run's own receipts. blueprintRecovery carries that same
// range as editRange, the author-edit receipt as saved, and writeState
// 'author-saved' when the receipt exists, else 'continued'
// (:942: blueprintSaved ? 'author-saved' : continuation.state).
func productionFindContinuedContext(need *store.DirectoryRecoveryNeed, leaseEpoch string) map[string]any {
	handle := map[string]any{
		"projectId":    need.Run.ProjectID,
		"epoch":        need.Run.Epoch,
		"rootActionId": need.Run.RootActionID,
		"runId":        need.Run.RunID,
	}
	nextHandle := map[string]any{
		"projectId":    need.Continuation.NextHandle.ProjectID,
		"epoch":        need.Continuation.NextHandle.Epoch,
		"rootActionId": need.Continuation.NextHandle.RootActionID,
		"runId":        need.Continuation.NextHandle.RunID,
	}
	writeState := "continued"
	if need.AuthorEdit != nil {
		writeState = "author-saved"
	}
	return map[string]any{
		"modelId":   need.Run.ModelID,
		"handle":    handle,
		"operation": "chapter-blueprint-directory",
		"authorInputs":               need.Run.AuthorInputs,
		"selectedDraftIds":           []int64{},
		"selectedFinalizedDraftIds":  []int64{},
		"selectedBlueprintChapterNumbers": need.Run.SelectedBlueprintChapterNumbers,
		"composition":                nil,
		"lastCompositionFinishReason": nil,
		"attemptedPurposes":          []string{},
		"draftSave":                  map[string]any{"kind": "absent"},
		"planningContinuation": map[string]any{
			"sourceHandle":   handle,
			"remainingRange": need.RemainingRange,
			"targetUnits":    need.TargetUnits,
			"state":          "continued",
			"nextHandle":     nextHandle,
		},
		"blueprintRecovery": map[string]any{
			"sourceHandle": handle,
			"leaseEpoch":   leaseEpoch,
			"draft":        "",
			"editRange":    need.RemainingRange,
			"saved":        need.AuthorEdit,
			"writeState":   writeState,
		},
	}
}
