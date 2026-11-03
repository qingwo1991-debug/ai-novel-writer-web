package admission

import (
	"context"
	"database/sql"
	"errors"
)

// VerifyM03MergedWithoutRecheckCycles verifies the populated M03 subset whose
// revision was committed but whose recheck has not run. It only reads the
// caller-owned snapshot transaction and has no production admission caller.
// Rechecked cycles and every other status remain unsupported.
func VerifyM03MergedWithoutRecheckCycles(ctx context.Context, tx *sql.Tx) error {
	if err := VerifyM03ReviewCycleSchema(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT cycle_id,revision_status,recheck_count,recheck_attempt_id
		FROM review_cycles ORDER BY cycle_id`)
	if err != nil {
		return err
	}
	type cycle struct {
		id, status     string
		recheckCount   int64
		recheckAttempt sql.NullString
	}
	var cycles []cycle
	for rows.Next() {
		var c cycle
		if err := rows.Scan(&c.id, &c.status, &c.recheckCount, &c.recheckAttempt); err != nil {
			rows.Close()
			return err
		}
		cycles = append(cycles, c)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, c := range cycles {
		if c.status != "merge-committed" || c.recheckCount != 0 || c.recheckAttempt.Valid {
			return errors.New("M03 merged cycle status or recheck unsupported")
		}
		basic, err := m03VerifyCycleBasicEvidence(ctx, tx, c.id)
		if err != nil {
			return err
		}
		if basic.revisionStatus != "merge-committed" {
			return errors.New("M03 merged cycle status changed")
		}
		if err := m03VerifyRevisionCycleCore(ctx, tx, basic); err != nil {
			return err
		}
	}
	return nil
}
