//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's write half of the three story-state
// families whose stores and transport binders already existed: eleven
// operations, bound by four disjoint binders against one write entry point.
//
//	db:narrative-thread-plan-create     arity 3, path 1  store.Narrative.CreatePlan
//	db:narrative-thread-plan-update     arity 4, path 2  store.Narrative.UpdatePlan
//	db:narrative-thread-plan-delete     arity 3, path 1  store.Narrative.DeletePlan
//	db:consistency-exemption-save       arity 4, path 2  store.Narrative.SaveExemption
//	db:consistency-exemption-revoke     arity 3, path 1  store.Narrative.RevokeExemption
//	db:narrative-thread-event-confirm   arity 3, path 1  store.Narrative.ConfirmEvent
//	db:plot-tree-clear                  arity 2, path 0  store.PlotTree.Clear
//	db:post-process-create-run          arity 3, path 1  store.PostHistory.CreateRun
//	db:post-process-mark-step-ok        arity 4, path 2  store.PostHistory.MarkStepOK
//	db:post-process-mark-step-failed    arity 5, path 3  store.PostHistory.MarkStepFailed
//	db:log-llm-call                     arity 3, path 1  store.PostHistory.LogCall
//
// The four binders are disjoint and, taken together with the already bound read
// binders (BindPostHistoryReads, BindPlotTreeReads and - when the lane binds it -
// BindNarrativeReads), serve all three families without binding one name twice:
//
//	BindNarrativeWrites        internal/transport/narrative_write.go:32 - the
//	                           five plan/exemption names and nothing else
//	BindNarrativeEventConfirm  internal/transport/narrative_confirm.go:31 - the
//	                           single confirmation name
//	BindPlotTreeClear          internal/transport/plottree_clear.go:29 - the
//	                           single clear name
//	BindPostHistoryWrites      internal/transport/posthistory_write.go:30 - the
//	                           four receipt/ledger names and nothing else
//
// # db:plot-tree-save is NOT here, and that is a capability decision
//
// BindPlotTree (internal/transport/plottree.go:14) installs three names at once -
// db:plot-tree-read, db:plot-tree-save and db:plot-tree-clear - and
// db:plot-tree-read is already served by BindPlotTreeReads, so using that binder
// would abort startup with OPERATION_ALREADY_BOUND (internal/transport/server.go:76)
// rather than bind one name. More importantly the save name is not a write this
// lane can perform: plottree.go:22 refuses it with
// GENERATION_GRAPH_ADMISSION_REQUIRED without ever calling store.PlotTree.Save,
// and that refusal mirrors the original public controller
// (electron/controllers/db-controller.ts:886-894, which throws inside its own
// try block and therefore always answers `{success:false,error:"Error:
// GENERATION_GRAPH_ADMISSION_REQUIRED"}`) because the real plot tree is written
// inside the main generation transaction (electron/services/main-generation-owner.ts:420/566).
// The gate is a real capability gate, not a missing migration: binding a handler
// that can only refuse would add no capability, would replace the lane's honest
// 501 with a fabricated 200, and would claim a generation owner this lane does
// not build. So the name stays unbound and keeps failing closed.
//
// # The one write entry point
//
// Every method below goes through productionDurableOpen.borrowAuthorDatabase
// (production_durable_linux.go:308), which is the WRITE entry point this lane
// already uses for exactly this kind of mutation: the one legacy snapshot INSERT
// db:save-summary-snapshot (production_continuity_reads_linux.go:39 -> BindSummary),
// the draft body CAS (CommitDraftContent), and the generation ledger writes
// (BeginGenerationLedgerDB). It is not the read-only admission path
// (authorruntime.ReadLifecycle.OpenGrantedProjectReadOnly /
// OpenProjectReadOnly, used by the knowledge reads): a read-only borrow never
// re-verifies the handle after the call, so a write made through it would leave
// the permit pointing at a content identity the write replaced and the very next
// operation on that project would be refused with AUTHOR_WRITE_STALE.
//
// borrowAuthorDatabase parses the request's frozen three-key context, names the
// caller's own cookie/tab owner, refuses a path the startup inventory issued no
// catalog key for, and calls authorruntime.ReadLifecycle.DurableRepository, which
// re-proves the lease, the admission permit and the handle identity before
// lending the author's own database. Its release closure refreshes the permit
// under a private five-second context (so a canceled request cannot leave a
// stale permit) and then releases the borrow: that refresh adopts the content
// identity the write committed and re-verifies the handle. The lease itself is
// not rotated (nothing here is a draft-body CAS), so the caller's frozen context
// stays usable for the next operation - the same shape the post-process writes
// already use.
//
// A caller that does not hold the live write lease is refused by that fence
// before any SQL runs. The refusal code is the family's own, because each
// binder renders the borrow failure in the envelope it already commits to: the
// five narrative plan/exemption names and the four post-process names answer
// HTTP 200 with success=false, db:log-llm-call answers HTTP 200 with a bare
// success=false and no code at all, and db:narrative-thread-event-confirm and
// db:plot-tree-clear answer a transport error (HTTP 500 with
// {"error":{"code":...}}). Nothing in this file widens, renames or silences one
// of those codes.
//
// # Idempotency and CAS: what is passed through, and what does not exist
//
// Not one of these eleven operations carries a revision, version or expected
// body in its channel contract (src/shared/ipc-channels.ts:1054-1098,
// 1177-1185) or in its store statement, so there is nothing to pass through and
// nothing is invented here:
//
//   - plan-create appends a row and returns it: a second identical call creates
//     a second plan. No CAS exists on this contract.
//   - plan-update and plan-delete are keyed by the plan id the browser already
//     holds; the store reports NARRATIVE_THREAD_PLAN_NOT_FOUND when the
//     statement changes zero rows (internal/store/narrative.go:102/113), so a
//     stale id is refused instead of silently succeeding. There is no
//     optimistic-concurrency version to compare - the original has none either.
//   - exemption-save is an upsert on the stable fact key that also reactivates a
//     revoked key (narrative.go:296-297), so it is idempotent by key. There is no
//     CAS.
//   - exemption-revoke sets revoked=1 by key and discards its rows-affected count
//     (narrative.go:305), so revoking an absent key reports success exactly as
//     the original does. That is preserved, not repaired.
//   - event-confirm re-reads the frozen finalization outbox inside its own
//     BEGIN IMMEDIATE transaction, binds the evidence to that snapshot prose and
//     only then inserts (narrative.go:140-173). Its idempotency guard is the
//     table's UNIQUE(plan_id,draft_id,event_type,evidence), not a revision: a
//     repeated identical confirmation is refused by SQLite, not accepted twice.
//   - plot-tree-clear is one statement, `UPDATE project_core SET
//     plot_tree_snapshot='' WHERE id='main'` (store/plottree.go:71), and is
//     idempotent by construction. It needs no source-revision CAS - the binder
//     that owns this name records that difference from db:plot-tree-save at
//     plottree_clear.go:12-17 - and none is added here.
//   - create-run mints its own run id inside the store (secureUUID,
//     store/posthistory.go:37) and writes the run row and every step row in one
//     transaction; a repeat creates a second run, which is what the original
//     does. When the caller supplies finalizedSource the store re-proves the
//     latest finalized draft, its outbox identity and the content hash before
//     inserting (posthistory.go:171-201) - that is the operation's own source
//     proof, passed through untouched.
//   - mark-step-ok and mark-step-failed are keyed by (run_id, step_key), bump
//     attempt_count and recompute the run's all_critical_passed in the same
//     transaction (posthistory.go:253-275). A missing step is
//     POST_PROCESS_STEP_NOT_FOUND. There is no expected-attempt CAS to pass.
//   - log-llm-call is a plain INSERT into llm_calls (posthistory.go:293-322).
//     No CAS.
//
// # Why this file declares its own provider type
//
// productionDurableOpen already has BeginPostHistory and BeginPlotTree for the
// read binders, and production_storystate_reads_linux.go declares its own
// narrative read provider rather than extending a type it does not own. This
// file follows that: one small write provider per transport interface, so the
// binder lines read the same way for all four calls, the write seam is named
// once, and nothing here has to extend a type defined in a file it does not own.

// productionStoryStateWrites is the production lane's write provider for the
// narrative, post-process/LLM-ledger and plot-tree families. It is stateless:
// every call borrows the admitted author database for its own request and hands
// the repository exactly that borrow's release closure, so a lane opened before
// the runtime exists (or after a refused startup) answers the symbolic
// PRODUCTION_CLOSED refusal instead of holding a handle this process no longer
// owns.
type productionStoryStateWrites struct {
	durable *productionDurableOpen
}

// newProductionStoryStateWrites lends the four story-state write binders their
// provider over the lane's own admitted runtime.
func newProductionStoryStateWrites(durable *productionDurableOpen) *productionStoryStateWrites {
	return &productionStoryStateWrites{durable: durable}
}

// BeginNarrative lends the narrative repository the five plan/exemption writes
// BindNarrativeWrites installs and the single confirmation BindNarrativeEventConfirm
// installs. store.NewNarrative performs no I/O; every one of those six methods
// writes the borrowed database in its own transaction, and the borrow's release
// closure adopts the committed content identity afterwards.
func (w *productionStoryStateWrites) BeginNarrative(ctx context.Context, caller transport.Caller, raw any) (*store.Narrative, func(), error) {
	if w == nil || w.durable == nil {
		return nil, nil, errors.New("PRODUCTION_CLOSED")
	}
	db, release, err := w.durable.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewNarrative(db), release, nil
}

// BeginPostHistory lends the post-process receipt and LLM ledger repository the
// four mutations BindPostHistoryWrites installs (create-run, mark-step-ok,
// mark-step-failed, log-llm-call). The five reads of that family stay with
// BindPostHistoryReads, so the two binders never overlap.
func (w *productionStoryStateWrites) BeginPostHistory(ctx context.Context, caller transport.Caller, raw any) (*store.PostHistory, func(), error) {
	if w == nil || w.durable == nil {
		return nil, nil, errors.New("PRODUCTION_CLOSED")
	}
	db, release, err := w.durable.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewPostHistory(db), release, nil
}

// BeginPlotTree lends the plot tree repository the one mutation
// BindPlotTreeClear installs. store.PlotTree.Clear is that single UPDATE and
// never replaces the stored snapshot, so this borrow can only blank it; the read
// stays with BindPlotTreeReads and db:plot-tree-save stays unbound.
func (w *productionStoryStateWrites) BeginPlotTree(ctx context.Context, caller transport.Caller, raw any) (*store.PlotTree, func(), error) {
	if w == nil || w.durable == nil {
		return nil, nil, errors.New("PRODUCTION_CLOSED")
	}
	db, release, err := w.durable.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewPlotTree(db), release, nil
}
