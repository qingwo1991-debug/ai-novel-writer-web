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

// ReadEditorStageJSONDurable executes one of the two stage reads the editor
// needs on open directly on the admitted author database under the frozen
// cookie/tab/lease context, the durable counterpart of ReadEditorStageJSON.
// Both are pure SELECTs, so the durable lane can serve them from the author's
// own project.db instead of the read-only admission's private snapshot: the
// exclusive legacy writer lock means no other writer exists, and the read
// transaction is never committed.
func (l *ReadLifecycle) ReadEditorStageJSONDurable(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, operation string) (json.RawMessage, error) {
	if operation != "db:project-core-get" && operation != "db:blueprint-get-all" {
		return nil, ErrInvalid
	}
	return l.readAdmittedAuthorJSON(ctx, owner, expected, func(txCtx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		var result any
		var err error
		switch operation {
		case "db:project-core-get":
			result, err = store.ReadProjectCoreTx(txCtx, tx)
		case "db:blueprint-get-all":
			result, err = store.ReadBlueprintsTx(txCtx, tx)
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(editorStageWire(result))
	})
}

// ReadCurrentOverviewJSONDurable is the durable counterpart of
// ReadCurrentOverviewJSON: it aggregates the same six stage facts inside one
// read transaction on the admitted author database and serializes them and the
// revision hash before the transaction is rolled back. A project without a core
// row keeps the desktop's unavailable shape instead of a fabricated overview.
func (l *ReadLifecycle) ReadCurrentOverviewJSONDurable(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) (json.RawMessage, error) {
	return l.readAdmittedAuthorJSON(ctx, owner, expected, func(txCtx context.Context, tx *sql.Tx) (json.RawMessage, error) {
		overview, err := store.ReadCurrentProjectOverviewTx(txCtx, tx, expected.ProjectID)
		if err != nil {
			return nil, err
		}
		if overview == nil {
			return json.RawMessage(`{"state":"unavailable"}`), nil
		}
		return json.Marshal(overview)
	})
}

// readAdmittedAuthorJSON runs one read-only transaction on the admitted author
// database for the exact caller lease and returns only serialized bytes, so no
// handle escapes to transport. The permit and the pinned database identity are
// verified before the transaction opens and again after it rolls back: a read
// leaves the on-disk content identity unchanged, so a mismatch is a refusal
// rather than a re-admission. The transaction is always rolled back, never
// committed.
func (l *ReadLifecycle) readAdmittedAuthorJSON(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, read func(context.Context, *sql.Tx) (json.RawMessage, error)) (json.RawMessage, error) {
	if l == nil || l.active == nil || ctx == nil || read == nil || expected.ProjectID == "" {
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
	if err := author.permit.VerifyAdopt(ctx, author.project); err != nil {
		return nil, err
	}
	if err := author.handle.VerifyAdopt(ctx); err != nil {
		return nil, err
	}
	tx, err := author.handle.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	wire, readErr := read(ctx, tx)
	rollbackErr := tx.Rollback()
	verifyErr := author.handle.VerifyAdopt(ctx)
	if err := errors.Join(readErr, rollbackErr, verifyErr); err != nil {
		return nil, err
	}
	return wire, nil
}
