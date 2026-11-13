//go:build linux

package authorruntime

import (
	"context"
	"database/sql"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// BeginGenerationLedgerRead is BeginGenerationLedgerDB for read-only generation
// views. Reads never change the database, so the content-identity verify is
// skipped: a concurrent write by another tab legitimately moves the digest
// between this read's admission and its borrow, and refusing the read with
// AUTHOR_WRITE_STALE only hides data the writer already committed. Liveness
// (closed handle) and the session fence remain enforced by active.Begin.
func (l *ReadLifecycle) BeginGenerationLedgerRead(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (*store.GenerationLedger, *sql.DB, func(), error) {
	if l == nil || l.active == nil || ctx == nil {
		return nil, nil, nil, ErrInvalid
	}
	handle, done, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return nil, nil, nil, err
	}
	author, ok := handle.(*admittedAuthor)
	if !ok {
		done()
		return nil, nil, nil, ErrDurableInvalid
	}
	author.mu.Lock()
	closed := author.closed || author.handle == nil
	author.mu.Unlock()
	if closed {
		done()
		return nil, nil, nil, ErrDurableClosed
	}
	db := author.handle.DB()
	return store.NewGenerationLedger(db, nil, nil), db, done, nil
}

// BeginGenerationLedgerDB is BeginGenerationLedger plus the admitted author's
// own database handle. The reduced synthetic generation lane needs both under a
// single single-writer admission: the ledger writes the durable run rows while
// the same handle derives the source binding from real project state
// (project-core, drafts, blueprints, prompts). One admission means one release
// function, so callers must run it exactly once after they are done.
//
// The ledger writes the author's database through the raw handle rather than
// through the durable write path, so the permit's recorded content identity
// stops matching as soon as the first run row is committed. refresh adopts the
// committed identity under the still-held legacy lock and re-verifies the
// handle; callers must invoke it after their writes and before the fence is
// observed again, or the next operation is refused with AUTHOR_WRITE_STALE.
func (l *ReadLifecycle) BeginGenerationLedgerDB(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (*store.GenerationLedger, *sql.DB, func(context.Context) error, func(), error) {
	if l == nil || l.active == nil || ctx == nil {
		return nil, nil, nil, nil, ErrInvalid
	}
	handle, done, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	author, ok := handle.(*admittedAuthor)
	if !ok {
		done()
		return nil, nil, nil, nil, ErrDurableInvalid
	}
	author.mu.Lock()
	closed := author.closed || author.handle == nil || author.permit == nil
	var verifyErr error
	if !closed {
		verifyErr = author.permit.VerifyAdopt(ctx, author.project)
		if verifyErr == nil {
			verifyErr = author.handle.VerifyAdopt(ctx)
		}
	}
	author.mu.Unlock()
	if closed {
		done()
		return nil, nil, nil, nil, ErrDurableClosed
	}
	if verifyErr != nil {
		done()
		return nil, nil, nil, nil, verifyErr
	}
	refresh := func(refreshCtx context.Context) error {
		if refreshCtx == nil {
			refreshCtx = ctx
		}
		author.mu.Lock()
		defer author.mu.Unlock()
		if author.closed || author.handle == nil || author.permit == nil {
			return ErrDurableClosed
		}
		if err := author.permit.Refresh(refreshCtx); err != nil {
			return err
		}
		// The post-refresh check adopts: another admitted write may have
		// committed between the refresh and this check, and that change is the
		// lane's own, not a source the caller must refuse.
		return author.handle.VerifyAdopt(refreshCtx)
	}
	db := author.handle.DB()
	return store.NewGenerationLedger(db, nil, nil), db, refresh, done, nil
}
