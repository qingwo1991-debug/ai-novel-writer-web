//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// productionEditorWrites is the write half of the editor surface this lane can
// serve on the author's own database: the four self-contained blueprint
// mutations, the two project-core mutations and the character roster commit.
//
// Every one of the seven names is served by a single store call, so no
// orchestration is composed in this file. What this file owns is the ONE
// admission path every existing production mutation already uses: the borrow
// installed by productionDurableOpen.borrowAuthorDatabase, which re-proves the
// cookie session, the browser tab, the frozen leaseId and the pinned v7 handle
// content identity against authorruntime before a repository is handed back, and
// refreshes the committed content identity on release. db:save-summary-snapshot
// (BindSummary), db:draft-update-content (BindDraftLiveWrites) and
// generation:begin/execute (newProductionGeneration) reach the same author
// database through exactly the same permit, so a write bound here is refused by
// the same fence that refuses theirs: no lease means no repository, and a stale
// lease means no SQL at all.
//
// Nothing here invents authority. store.Blueprints.Upsert/UpsertMany/Delete/
// CommitRange run the original repository's statements, store.ProjectCore.Update/
// CommitSynopsis keep the original's allowlist and eleven-baseline CAS, and
// store.RosterAuthor.CommitAuthorRoster runs the original author pipeline
// (identity commit, prose and state writes, projection refresh, roster-meta bump
// and its own readback gate) under a frozen-context callback this file re-observes
// from the lifecycle that admitted the borrow.
//
// db:blueprint-update-notes is deliberately NOT bound, here or anywhere else in
// this lane. Its original handler (electron/controllers/db-controller.ts:636)
// refuses unconditionally - "Derived finalization notes are committed with their
// artifact and ACK in one main transaction" - so its refusal is a genuine
// capability gate, not a missing wire: satisfying it needs a generation
// finalization admission plus the artifact/ACK transaction that carries it, and
// neither this lane nor store.Blueprints holds that authority (there is no
// UpdateNotes store method at all). The transport's own BindBlueprints therefore
// keeps answering it with GENERATION_FINALIZATION_ADMISSION_REQUIRED, and a name
// that could only ever refuse is left unbound so it keeps failing closed at
// GO_OPERATION_NOT_MIGRATED instead of being installed as a handler.
//
// db:blueprint-character-sync-get and db:blueprint-character-sync-list-pending
// are not touched here either: they backfill with INSERT OR IGNORE and belong to
// another batch's refusal list.
type productionEditorWrites struct {
	durable *productionDurableOpen
}

// newProductionEditorWrites wraps the lane's durable bridge. The bridge is the
// only object that can name the author database, so the adapter cannot open,
// stage or migrate anything by itself.
func newProductionEditorWrites(durable *productionDurableOpen) *productionEditorWrites {
	return &productionEditorWrites{durable: durable}
}

// Bind installs the three disjoint write families. Each transport binder below
// publishes only the names listed above - BindBlueprintWrites does not also bind
// the reads, BindProjectCoreWrites does not also bind db:project-core-get, and
// BindCharacterRosterCommit does not also bind the three character reads - so the
// production lane can bind all three next to its existing read binders without an
// OPERATION_ALREADY_BOUND collision.
func (w *productionEditorWrites) Bind(server *transport.Server) error {
	if w == nil || w.durable == nil {
		return errors.New("PRODUCTION_EDITOR_WRITES_UNAVAILABLE")
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if err := server.BindBlueprintWrites(w); err != nil {
		return err
	}
	if err := server.BindProjectCoreWrites(w); err != nil {
		return err
	}
	return server.BindCharacterRosterCommit(w)
}

// borrow is the lane's single write admission. It is not a new path: the durable
// bridge already refuses a closed lane, an unlisted project path, a caller that
// does not own the slot and a lease that no longer matches, and it refreshes the
// content identity the write committed before the fence is observed again.
func (w *productionEditorWrites) borrow(ctx context.Context, caller transport.Caller, raw any) (*sql.DB, func(), error) {
	if w == nil || w.durable == nil {
		return nil, nil, errors.New("PRODUCTION_EDITOR_WRITES_UNAVAILABLE")
	}
	return w.durable.borrowAuthorDatabase(ctx, caller, raw)
}

// BeginBlueprints lends the admitted author database for the four blueprint
// mutations. It delegates to the durable bridge's own BeginBlueprints, which is
// the borrow the editor's db:blueprint-get already uses, so the write and the
// read can never disagree about which handle or which lease they are on.
func (w *productionEditorWrites) BeginBlueprints(ctx context.Context, caller transport.Caller, raw any) (*store.Blueprints, func(), error) {
	if w == nil || w.durable == nil {
		return nil, nil, errors.New("PRODUCTION_EDITOR_WRITES_UNAVAILABLE")
	}
	return w.durable.BeginBlueprints(ctx, caller, raw)
}

// BeginProjectCore lends the same admitted author database for
// db:project-core-update and db:project-core-synopsis-commit. store.ProjectCore
// is constructed over that handle and performs no I/O by itself: Update rejects a
// charactersArch key before any SQL runs, and CommitSynopsis is one atomic
// compare-and-set over the target body and all eleven source baselines, so a
// stale baseline is a lost CAS rather than a silent overwrite.
func (w *productionEditorWrites) BeginProjectCore(ctx context.Context, caller transport.Caller, raw any) (*store.ProjectCore, func(), error) {
	db, release, err := w.borrow(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewProjectCore(db), release, nil
}

// CommitCharacterRoster serves db:character-roster-commit: one admitted handle,
// one frozen session, one author pipeline.
//
// The author scope's project is the admitted project and its epoch is the admitted
// lease, exactly as the verified synthetic lane binds it: the identity approval
// records which frozen session authorized the roster, and the lease is the value
// that changes when that session is replaced. The callback store.RosterAuthor
// invokes inside its own immediate transaction re-observes the live context from
// the lifecycle that admitted this borrow, so a commit that outlived its lease is
// refused before it writes (the store asks twice: once at the transaction head and
// once around the identity commit).
func (w *productionEditorWrites) CommitCharacterRoster(ctx context.Context, caller transport.Caller, request map[string]any, raw any) (map[string]any, error) {
	if w == nil || w.durable == nil || ctx == nil {
		return nil, errors.New("PRODUCTION_EDITOR_WRITES_UNAVAILABLE")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	owner, err := productionOwner(caller)
	if err != nil {
		return nil, err
	}
	w.durable.mu.Lock()
	lifecycle := w.durable.lifecycle
	closed := w.durable.closed
	w.durable.mu.Unlock()
	if closed || lifecycle == nil {
		return nil, errors.New("PRODUCTION_CLOSED")
	}
	db, release, err := w.borrow(ctx, caller, raw)
	if err != nil {
		return nil, err
	}
	defer release()
	authorize := func() error {
		current, present, currentErr := lifecycle.CurrentProjectDurable(ctx, owner)
		if currentErr != nil {
			return currentErr
		}
		if !present || current != expected {
			return errors.New("PROJECT_SESSION_STALE")
		}
		return nil
	}
	scope := store.RosterAuthorScope{ProjectID: expected.ProjectID, Epoch: expected.LeaseID}
	return store.NewRosterAuthor(db).CommitAuthorRoster(ctx, request, scope, authorize)
}
