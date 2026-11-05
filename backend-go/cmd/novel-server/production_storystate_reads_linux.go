//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's thin adapter from the durable author
// admission to the two transport providers the story-state read binders borrow:
// transport.DraftProvider (BindDraftAuthorityReads, the three authoritative
// export reads) and transport.NarrativeProvider (BindNarrativeReads, the two
// narrative thread reads and the consistency exemption list).
//
// Both go through borrowAuthorDatabase, the identical fence the draft CAS, the
// editor reads, the character and stage reads, the review/revision reads, the
// continuity/post-process/plot-tree reads and the import-run reads already use:
// the request's frozen three-key context is parsed, the caller's own cookie/tab
// identity is named, the project path must be one the startup inventory issued a
// catalog key for, and authorruntime re-proves the lease, the admission permit
// and the handle identity before lending the author's own database. Nothing here
// opens a database, resolves a path, or makes an admission decision, and no
// repository is constructed over a database that failed the pinned v7 admission.
// A read commits nothing, so the borrow's release closure (refresh, then done)
// is a re-verification rather than a write acknowledgement.
//
// # Both binders are pure-read binders
//
// BindDraftAuthorityReads (internal/transport/draft_author.go:199-288) installs
// exactly the three names below and nothing else - no db:draft-create,
// db:draft-update-status or db:draft-delete (those three belong to
// BindDraftAuthorWrites), and no live read or body CAS name:
//
//	db:draft-authority-sequence        store.Drafts.AuthoritySequence     store/draft_authority.go:109
//	                                   -> draftAuthorityRows :59, one SELECT
//	                                      over drafts JOIN contents
//	db:draft-export-snapshot           store.Drafts.ExportSnapshot        store/draft_authority.go:181
//	                                   -> exportSnapshotRows :204, a
//	                                      BeginTx(ReadOnly:true) that only
//	                                      SELECTs drafts, contents and
//	                                      finalization_outbox and then commits
//	                                      without a write
//	db:draft-export-authority-current  store.Drafts.ExportAuthorityCurrent store/draft_authority.go:313
//	                                   -> the same exportSnapshotRows reader
//
// BindNarrativeReads (internal/transport/narrative_read.go:26-117) installs
// exactly the three names below and nothing else - the six mutating names of
// that family (plan-create, plan-update, plan-delete, event-confirm,
// exemption-save, exemption-revoke) are not installed here, so a lane that binds
// only this half can never write a plan, confirm a thread event or revoke an
// exemption:
//
//	db:narrative-thread-list           store.Narrative.List   store/narrative.go:235
//	                                  -> narrativeList :204 (SELECT-only) inside a
//	                                     BeginTx(ReadOnly:true) that commits without
//	                                     a write
//	db:narrative-thread-list-relevant  store.Narrative.Relevant store/narrative.go:248
//	                                  -> delegates to List (:249), then filters in
//	                                     memory; it reads no further rows
//	db:consistency-exemption-list      store.Narrative.Exemptions store/narrative.go:282
//	                                  -> one SELECT over consistency_exemptions
//
// The one transaction either binder can commit is a read-only one; neither
// binder reserves a BEGIN IMMEDIATE, inserts, updates or deletes anything, so
// serving every one of these six names leaves the admitted database byte for
// byte identical (proven in production_storystate_reads_linux_test.go).
//
// # Why a second DraftProvider adapter exists
//
// productionDurableOpen.BeginDrafts (production_durable_linux.go:359) already
// satisfies transport.DraftProvider, and the production lane may keep using it
// for BindDraftAuthorityReads. This file still declares its own provider type
// per transport interface so the lane's binding line reads the same way for both
// story-state binders and so nothing in this change has to extend a type defined
// in a file it does not own. The borrow it performs is the same call, with the
// same release closure and the same refusals.

// productionDraftAuthorityReads is the authoritative-export read provider. It is
// stateless: every call borrows the admitted author database for its own
// request, so a lane opened before the runtime exists (or after a refused
// startup) answers the symbolic PRODUCTION_CLOSED refusal instead of holding a
// handle this process no longer owns.
type productionDraftAuthorityReads struct {
	durable *productionDurableOpen
}

// newProductionDraftAuthorityReads lends BindDraftAuthorityReads its provider.
func newProductionDraftAuthorityReads(durable *productionDurableOpen) *productionDraftAuthorityReads {
	return &productionDraftAuthorityReads{durable: durable}
}

// BeginDrafts lends the draft repository the three authoritative export reads
// project. store.NewDrafts performs no I/O; the three methods bound here only
// SELECT, so a refused lease, a foreign tab, a closed runtime or a handle whose
// content identity no longer matches is refused by the fence before any SQL runs.
func (r *productionDraftAuthorityReads) BeginDrafts(ctx context.Context, caller transport.Caller, raw any) (*store.Drafts, func(), error) {
	if r == nil || r.durable == nil {
		return nil, nil, errors.New("PRODUCTION_CLOSED")
	}
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewDrafts(db), release, nil
}

// productionNarrativeReads is the narrative thread / consistency exemption read
// provider, built the same way and over the same fence.
type productionNarrativeReads struct {
	durable *productionDurableOpen
}

// newProductionNarrativeReads lends BindNarrativeReads its provider.
func newProductionNarrativeReads(durable *productionDurableOpen) *productionNarrativeReads {
	return &productionNarrativeReads{durable: durable}
}

// BeginNarrative lends the narrative repository the two thread reads and the one
// exemption list. store.NewNarrative performs no I/O; the three methods bound
// here never reach CreatePlan, UpdatePlan, DeletePlan, ConfirmEvent,
// SaveExemption or RevokeExemption, which are the only mutating methods of that
// repository, so a read here cannot write a plan, a confirmation or an
// exemption.
func (r *productionNarrativeReads) BeginNarrative(ctx context.Context, caller transport.Caller, raw any) (*store.Narrative, func(), error) {
	if r == nil || r.durable == nil {
		return nil, nil, errors.New("PRODUCTION_CLOSED")
	}
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	return store.NewNarrative(db), release, nil
}
