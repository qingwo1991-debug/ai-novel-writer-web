//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// ReadAbsentReviewCycleJSON returns null only for a review with no saved
// cycle, under the same exact private read lease as other editor reads.
func (l *ReadLifecycle) ReadAbsentReviewCycleJSON(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, reviewID int64) (json.RawMessage, error) {
	err := l.Read(ctx, owner, expected, func(tx *sql.Tx) error {
		return store.ReadAbsentReviewCycleTx(ctx, tx, reviewID)
	})
	if err != nil {
		return nil, err
	}
	return json.RawMessage("null"), nil
}
