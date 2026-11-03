//go:build linux

package authorruntime

import (
	"context"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// BeginGenerationLedger admits the caller's frozen cookie/tab/lease fence and
// returns a GenerationLedger over the admitted author's OWN database, plus the
// release func. The ledger is created fresh per call over the stable author DB
// handle; the caller must hold the returned release for the whole operation so
// CloseProject waits for the run to finish. Verify runs under author.mu, but
// the handle itself stays pinned by the active coordinator for the call, so
// the ledger may use it after the lock is released.
func (l *ReadLifecycle) BeginGenerationLedger(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (*store.GenerationLedger, func(), error) {
	if l == nil || l.active == nil || ctx == nil {
		return nil, nil, ErrInvalid
	}
	handle, done, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return nil, nil, err
	}
	author, ok := handle.(*admittedAuthor)
	if !ok {
		done()
		return nil, nil, ErrDurableInvalid
	}
	author.mu.Lock()
	closed := author.closed || author.handle == nil || author.permit == nil
	var verifyErr error
	if !closed {
		verifyErr = author.permit.Verify(ctx, author.project)
		if verifyErr == nil {
			verifyErr = author.handle.Verify(ctx)
		}
	}
	author.mu.Unlock()
	if closed {
		done()
		return nil, nil, ErrDurableClosed
	}
	if verifyErr != nil {
		done()
		return nil, nil, verifyErr
	}
	return store.NewGenerationLedger(author.handle.DB(), nil, nil), done, nil
}
