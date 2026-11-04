//go:build linux

package authorruntime

import (
	"context"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// RecoveryRepositoryDurable lends the recovery candidate repository on the
// admitted author database under one durable admission.
func (l *ReadLifecycle) RecoveryRepositoryDurable(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (*store.Recovery, func(context.Context) error, func(), error) {
	db, refresh, done, err := l.DurableRepository(ctx, owner, expected)
	if err != nil {
		return nil, nil, nil, err
	}
	return store.NewRecovery(db), refresh, done, nil
}

// SummaryRepositoryDurable lends the continuity/summary repository on the
// admitted author database under one durable admission.
func (l *ReadLifecycle) SummaryRepositoryDurable(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (*store.Summary, func(context.Context) error, func(), error) {
	db, refresh, done, err := l.DurableRepository(ctx, owner, expected)
	if err != nil {
		return nil, nil, nil, err
	}
	return store.NewSummary(db), refresh, done, nil
}
