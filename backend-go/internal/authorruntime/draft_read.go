//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"encoding/json"

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

func draftReadOperation(operation string) bool {
	switch operation {
	case "db:draft-list", "db:draft-list-all", "db:draft-get-meta", "db:draft-get-full",
		"db:draft-get-latest", "db:draft-get-finalized", "db:draft-get-max-finalized-chapter", "db:draft-next-version":
		return true
	default:
		return false
	}
}
