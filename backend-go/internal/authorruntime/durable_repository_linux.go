//go:build linux

package authorruntime

import (
	"context"
	"database/sql"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

// DurableRepository admits the author project for this exact session, tab and
// frozen context and hands back the author's own database handle together with
// the refresh and release closures for that single admission.
//
// The borrowing repositories write the author's database through the raw handle
// rather than through the durable write path, so the permit's recorded content
// identity stops matching as soon as the first row is committed. refresh adopts
// the committed identity under the still-held legacy lock and re-verifies the
// handle; callers must invoke it after their writes and before the fence is
// observed again, or the next operation is refused with AUTHOR_WRITE_STALE. A
// pure read leaves the identity unchanged and refresh is then a no-op
// re-verification.
func (l *ReadLifecycle) DurableRepository(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (*sql.DB, func(context.Context) error, func(), error) {
	return l.durableRepository(ctx, owner, expected, true)
}

// DurableRepositoryStrict is DurableRepository without adopting a changed
// content identity: a borrow whose answer must be provably the source a run
// froze uses it, so a database that changed under the run is refused instead of
// described.
func (l *ReadLifecycle) DurableRepositoryStrict(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (*sql.DB, func(context.Context) error, func(), error) {
	return l.durableRepository(ctx, owner, expected, false)
}

func (l *ReadLifecycle) durableRepository(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, adopt bool) (*sql.DB, func(context.Context) error, func(), error) {
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
	closed := author.closed || author.handle == nil || author.permit == nil
	var verifyErr error
	if !closed {
		if adopt {
			verifyErr = author.permit.VerifyAdopt(ctx, author.project)
			if verifyErr == nil {
				verifyErr = author.handle.VerifyAdopt(ctx)
			}
		} else {
			verifyErr = author.permit.Verify(ctx, author.project)
			if verifyErr == nil {
				verifyErr = author.handle.Verify(ctx)
			}
		}
	}
	author.mu.Unlock()
	if closed {
		done()
		return nil, nil, nil, ErrDurableClosed
	}
	if verifyErr != nil {
		done()
		return nil, nil, nil, verifyErr
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
		return author.handle.Verify(refreshCtx)
	}
	return author.handle.DB(), refresh, done, nil
}
