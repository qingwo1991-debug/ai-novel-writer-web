//go:build linux

package authorruntime

import (
	"context"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// WithPublishBarrier gives a trusted publisher a drained target project under
// the lifecycle's continuously held legacy writer lock. The owner and lease
// must identify an admitted project. All of that project's SQLite readers are
// closed before publish runs; knowledge borrows finish before their owning
// reader closes. New target reads and opens remain fenced through publish.
// Afterward, clients must Open again to obtain a new read lease.
//
// publish must not call this lifecycle's Open, Current, Close, Shutdown, or
// another publish barrier, because those operations enter the transition
// queue held for the callback's duration. It must not release the held lock.
func (l *ReadLifecycle) WithPublishBarrier(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, publish func(context.Context, *writerlock.Lock) error) error {
	if l == nil || l.active == nil {
		return ErrInvalid
	}
	return l.active.WithProjectDrained(ctx, owner, expected, publish)
}
