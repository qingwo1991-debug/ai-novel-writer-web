//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// ReadReviewRevisionJSON serializes metadata under the same exact project
// lease, source proof and private read transaction as other editor reads.
// Review and revision mutations remain outside this API.
func (l *ReadLifecycle) ReadReviewRevisionJSON(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, operation string, baseDraftID int64) (json.RawMessage, error) {
	if operation != "db:review-list" && operation != "db:revision-get-pending" && operation != "db:revision-list" {
		return nil, ErrInvalid
	}
	var wire json.RawMessage
	err := l.Read(ctx, owner, expected, func(tx *sql.Tx) error {
		rows, err := store.ReadReviewRevisionListTx(ctx, tx, operation, baseDraftID)
		if err != nil {
			return err
		}
		wire, err = json.Marshal(store.ReviewRevisionListWire(rows))
		return err
	})
	if err != nil {
		return nil, err
	}
	return wire, nil
}
