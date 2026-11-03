package store

import (
	"context"
	"database/sql"
)

// ReadReviewRevisionFullTx reads one full review or revision from an already
// admitted private read transaction. A missing row preserves the original
// nullable IPC result.
func ReadReviewRevisionFullTx(ctx context.Context, tx *sql.Tx, operation string, id int64) (map[string]any, error) {
	if ctx == nil || tx == nil || id < 1 || id > 9007199254740991 {
		return nil, ErrReviewRevisionInvalidInput
	}
	switch operation {
	case "db:review-get-full":
		return rrFull(rrMaps(ctx, tx, "SELECT "+rrReviewMeta+rrSourceColumns+` FROM reviews r
 LEFT JOIN contents c ON c.id=r.content_id WHERE r.id=?`, id))
	case "db:revision-get-full":
		return rrFull(rrMaps(ctx, tx, "SELECT "+rrRevisionMeta+rrSourceColumns+` FROM revisions r
 LEFT JOIN contents c ON c.id=r.content_id WHERE r.id=?`, id))
	default:
		return nil, ErrReviewRevisionInvalidInput
	}
}

// ReviewRevisionFullWire retains legacy Buffer JSON for malformed historical
// BLOB columns while leaving ordinary text, numeric and null classes intact.
func ReviewRevisionFullWire(row map[string]any) map[string]any {
	if row == nil {
		return nil
	}
	return ReviewRevisionListWire([]map[string]any{row})[0]
}
