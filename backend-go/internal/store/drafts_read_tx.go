package store

import (
	"context"
	"database/sql"
	"errors"
)

// ReadDraftOperation runs one public draft read inside a caller-owned,
// already-admitted read transaction. No repository or writable connection
// escapes the transaction callback.
func ReadDraftOperation(ctx context.Context, tx *sql.Tx, operation string, number int64) (any, error) {
	if ctx == nil || tx == nil {
		return nil, errors.New("DRAFT_READ_TRANSACTION_REQUIRED")
	}
	switch operation {
	case "db:draft-list":
		return readMetas(ctx, tx, " WHERE d.chapter_number = ? ORDER BY d.version ASC", number)
	case "db:draft-list-all":
		return readMetas(ctx, tx, " ORDER BY d.chapter_number ASC, d.version ASC")
	case "db:draft-get-meta", "db:draft-get-latest", "db:draft-get-finalized", "db:draft-get-full":
		suffix := " WHERE d.id = ?"
		if operation == "db:draft-get-latest" {
			suffix = " WHERE d.chapter_number = ? ORDER BY d.version DESC LIMIT 1"
		} else if operation == "db:draft-get-finalized" {
			suffix = " WHERE d.chapter_number = ? AND d.status = 'finalized' ORDER BY d.version DESC LIMIT 1"
		}
		metas, err := readMetas(ctx, tx, suffix, number)
		if err != nil || len(metas) == 0 {
			return nil, err
		}
		if operation != "db:draft-get-full" {
			return &metas[0], nil
		}
		full := &DraftFull{DraftMeta: metas[0]}
		var body sql.NullString
		err = tx.QueryRowContext(ctx, "SELECT body FROM contents WHERE id = ?", full.ContentID).Scan(&body)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		full.Content = body.String
		return full, nil
	case "db:draft-get-max-finalized-chapter":
		var chapter sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT MAX(chapter_number) FROM drafts WHERE status = 'finalized'").Scan(&chapter); err != nil {
			return nil, err
		}
		return chapter.Int64, nil
	case "db:draft-next-version":
		var version sql.NullInt64
		if err := tx.QueryRowContext(ctx, "SELECT MAX(version) FROM drafts WHERE chapter_number = ?", number).Scan(&version); err != nil {
			return nil, err
		}
		return version.Int64 + 1, nil
	default:
		return nil, errors.New("DRAFT_READ_OPERATION_UNSUPPORTED")
	}
}
