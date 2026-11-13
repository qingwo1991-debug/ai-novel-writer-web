package admission

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Use a conservative JSON upper bound in SQLite BEFORE the Go driver fetches
// bodies. UTF-8 text may need six output bytes per source byte; Buffer JSON needs
// up to four per byte. Reject oversized rows rather than fetching and then OOM.
// This deliberately may reject a row whose actual JSON would fit. The bound is
// per ROW, not per table or database, and does not claim a 4MiB process RSS cap.
func preflightDomainRows(ctx context.Context, tx *sql.Tx, table string, columns []string) error {
	terms := make([]string, 0, len(columns))
	for _, name := range columns {
		col := quoteIdentifier(name)
		terms = append(terms, "CASE typeof("+col+") WHEN 'text' THEN min(length(CAST("+col+" AS BLOB)),"+"67108864"+")*6+3 WHEN 'blob' THEN min(length("+col+"),67108864)*4+32 ELSE 64 END")
	}
	if len(terms) == 0 {
		return errDomainEncoding
	}
	var tooLarge int
	err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+quoteIdentifier(table)+" WHERE 2+"+strings.Join(terms, "+")+">67108864)").Scan(&tooLarge)
	if err != nil {
		return err
	}
	if tooLarge != 0 {
		return errors.New("SCHEMA_DOMAIN_ROW_TOO_LARGE")
	}
	return nil
}
