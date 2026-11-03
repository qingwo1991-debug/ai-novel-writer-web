package store

import (
	"context"
	"database/sql"
)

// ReadReviewRevisionListTx reads only the two metadata lists needed while a
// draft tab loads. The caller owns the already admitted read transaction.
// It cannot borrow a writable repository or acknowledge a review mutation.
func ReadReviewRevisionListTx(ctx context.Context, tx *sql.Tx, operation string, baseDraftID int64) ([]map[string]any, error) {
	if ctx == nil || tx == nil || baseDraftID < 1 || baseDraftID > 9007199254740991 {
		return nil, ErrReviewRevisionInvalidInput
	}
	switch operation {
	case "db:review-list":
		return rrMaps(ctx, tx, "SELECT "+rrReviewMeta+" FROM reviews r WHERE r.base_draft_id=? ORDER BY r.review_index ASC", baseDraftID)
	case "db:revision-get-pending":
		return rrMaps(ctx, tx, "SELECT "+rrRevisionMeta+" FROM revisions r WHERE r.base_draft_id=? AND r.status='pending' ORDER BY r.revision_index ASC", baseDraftID)
	default:
		return nil, ErrReviewRevisionInvalidInput
	}
}

// ReviewRevisionListWire preserves better-sqlite3's Buffer representation for
// legacy BLOB metadata. Ordinary TEXT, NULL and numeric SQLite classes retain
// their original JSON meanings.
func ReviewRevisionListWire(rows []map[string]any) []map[string]any {
	if rows == nil {
		return []map[string]any{}
	}
	wire := make([]map[string]any, len(rows))
	for i, row := range rows {
		item := make(map[string]any, len(row))
		for key, value := range row {
			if blob, ok := value.([]byte); ok {
				data := make([]int, len(blob))
				for j, octet := range blob {
					data[j] = int(octet)
				}
				item[key] = map[string]any{"type": "Buffer", "data": data}
			} else {
				item[key] = value
			}
		}
		wire[i] = item
	}
	return wire
}
