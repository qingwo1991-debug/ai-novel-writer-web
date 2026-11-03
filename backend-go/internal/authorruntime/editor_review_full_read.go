//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// ReadReviewRevisionFullJSON serializes a full row before releasing the exact
// admitted private project read lease. It never returns a database handle.
func (l *ReadLifecycle) ReadReviewRevisionFullJSON(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, operation string, id int64) (json.RawMessage, error) {
	if operation != "db:review-get-full" && operation != "db:revision-get-full" {
		return nil, ErrInvalid
	}
	var wire json.RawMessage
	err := l.Read(ctx, owner, expected, func(tx *sql.Tx) error {
		row, err := store.ReadReviewRevisionFullTx(ctx, tx, operation, id)
		if err != nil {
			return err
		}
		wire, err = json.Marshal(store.ReviewRevisionFullWire(row))
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}
