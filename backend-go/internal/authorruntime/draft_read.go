//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// ReadDraftJSON serializes one draft read while the exact project lease and
// private SQLite read transaction are held. The returned bytes carry no DB
// handle and cannot acknowledge a save to the author's source project.
func (l *ReadLifecycle) ReadDraftJSON(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, operation string, number int64) (json.RawMessage, error) {
	if !draftReadOperation(operation) {
		return nil, ErrInvalid
	}
	var wire json.RawMessage
	err := l.Read(ctx, owner, expected, func(tx *sql.Tx) error {
		value, err := store.ReadDraftOperation(ctx, tx, operation, number)
		if err != nil {
			return err
		}
		wire, err = json.Marshal(value)
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// ReadDraftJSONDurable executes one public draft read directly on the admitted
// author database under the frozen cookie/tab/lease context, the durable
// counterpart of ReadDraftJSON. The durable lane holds the real writable
// project.db and the legacy writer lock is exclusive, so a plain deferred read
// transaction on the author's own mode=rw connection sees a committed snapshot
// while no other writer can exist. The exact session, tab and project lease stay
// held through JSON serialization and handle re-verification; the permit is not
// refreshed because a read leaves the on-disk content identity unchanged, and a
// handle that no longer matches it fails closed instead of serving prose.
func (l *ReadLifecycle) ReadDraftJSONDurable(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, operation string, number int64) (json.RawMessage, error) {
	if l == nil || l.active == nil || ctx == nil || !draftReadOperation(operation) {
		return nil, ErrInvalid
	}
	handle, done, err := l.active.Begin(ctx, owner, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	author, ok := handle.(*admittedAuthor)
	if !ok {
		return nil, ErrDurableInvalid
	}
	author.mu.Lock()
	defer author.mu.Unlock()
	if author.closed || author.handle == nil || author.permit == nil {
		return nil, ErrDurableClosed
	}
	if err := author.permit.Verify(ctx, author.project); err != nil {
		return nil, err
	}
	if err := author.handle.Verify(ctx); err != nil {
		return nil, err
	}
	tx, err := author.handle.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	value, readErr := store.ReadDraftOperation(ctx, tx, operation, number)
	rollbackErr := tx.Rollback()
	var wire json.RawMessage
	if readErr == nil {
		wire, readErr = json.Marshal(value)
	}
	verifyErr := author.handle.Verify(ctx)
	if err := errors.Join(readErr, rollbackErr, verifyErr); err != nil {
		return nil, err
	}
	return wire, nil
}

func draftReadOperation(operation string) bool {
	switch operation {
	case "db:draft-list", "db:draft-list-all", "db:draft-get-meta", "db:draft-get-full",
		"db:draft-get-latest", "db:draft-get-finalized", "db:draft-get-max-finalized-chapter", "db:draft-next-version":
		return true
	default:
		return false
	}
}
