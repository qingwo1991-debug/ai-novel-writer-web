package admission

import (
	"context"
	"database/sql"
	"reflect"
)

// m01GenerationSQL is byte-for-byte M01_GENERATION_SQL from the original
// TypeScript migration. SQLite stores this whitespace in sqlite_master.
const m01GenerationSQL = `
CREATE TABLE generation_roots (
 root_action_id TEXT PRIMARY KEY, idempotency_key TEXT NOT NULL UNIQUE,
 action_json TEXT NOT NULL, budget_json TEXT NOT NULL,
 active_elapsed_ms INTEGER NOT NULL DEFAULT 0, active_since_ms INTEGER,
 blocked_code TEXT, revision INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE generation_runs (
 run_id TEXT PRIMARY KEY, root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id),
 binding_json TEXT NOT NULL, status TEXT NOT NULL, created_at_ms INTEGER NOT NULL, open_key TEXT NOT NULL UNIQUE
);
CREATE TABLE generation_attempts (
 attempt_id TEXT PRIMARY KEY, reservation_id TEXT NOT NULL UNIQUE,
 run_id TEXT NOT NULL REFERENCES generation_runs(run_id), root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id),
 attempt_json TEXT NOT NULL, usage_receipt_json TEXT, invocation_nonce TEXT NOT NULL,
 UNIQUE(run_id, invocation_nonce)
);
CREATE TABLE generation_artifacts (
 artifact_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL UNIQUE REFERENCES generation_attempts(attempt_id),
 run_id TEXT NOT NULL REFERENCES generation_runs(run_id), artifact_json TEXT NOT NULL,
 revision INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'partial'
);`

var m01GenerationTables = []string{"generation_roots", "generation_runs", "generation_attempts", "generation_artifacts"}

// UpgradeSnapshotV1 applies only original M01 (v1 -> v2) on an authorized,
// exclusive, caller-owned PRIVATE copy. M01 adds four empty ledger tables and
// does not backfill historical work. Exact recognized fingerprints, source
// integrity, foreign keys and all original rows are checked transactionally.
// Its evidence is intermediate v2, never a public/current-schema admission.
// Any error, including after commit, requires discarding the private copy.
func UpgradeSnapshotV1(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
	var zero Evidence
	if err := validateContract(expected); err != nil {
		return zero, err
	}
	if ctx == nil {
		return zero, failure(CodeUpgradeUnsupported, nil)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	absolute, err := snapshotPath(path)
	if err != nil {
		return zero, err
	}
	if err := privateUpgradePath(absolute); err != nil {
		return zero, err
	}
	before, err := snapshotIdentity(ctx, absolute)
	if err != nil {
		return zero, err
	}
	db, err := openEarlySnapshot(absolute, false)
	if err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	defer tx.Rollback()
	tables, _, err := checkM01Stage(ctx, tx, expected, 1)
	if err != nil {
		return zero, err
	}
	domainBefore, err := logicalDomain(ctx, tx, tables)
	if err != nil {
		return zero, failure(CodeDomain, err)
	}
	if err := privateUpgradePath(absolute); err != nil {
		return zero, err
	}
	still, err := snapshotIdentity(ctx, absolute)
	if err != nil {
		return zero, err
	}
	if still != before {
		return zero, failure(CodeSnapshotChanged, nil)
	}
	if _, err := tx.ExecContext(ctx, m01GenerationSQL); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	finalTables, _, err := checkM01Stage(ctx, tx, expected, 2)
	if err != nil {
		return zero, err
	}
	if len(finalTables) != len(tables)+len(m01GenerationTables) {
		return zero, failure(CodeUnrecognizedSchema, nil)
	}
	domainAfter, err := logicalDomain(ctx, tx, tables)
	if err != nil {
		return zero, failure(CodeDomain, err)
	}
	if !reflect.DeepEqual(domainBefore, domainAfter) {
		return zero, failure(CodeDomain, nil)
	}
	if err := tx.Commit(); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if err := db.Close(); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	return verifyM01SnapshotV2(ctx, absolute, expected)
}

func checkM01Stage(ctx context.Context, tx *sql.Tx, expected SchemaContract, stage int) ([]string, string, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, "", failure(CodeIntegrity, err)
	}
	if version < 0 {
		return nil, "", failure(CodeInvalidVersion, nil)
	}
	if version > CurrentSchemaVersion {
		return nil, "", failure(CodeNewerSchema, nil)
	}
	if version != stage || (stage != 1 && stage != 2) {
		return nil, "", failure(CodeUpgradeUnsupported, nil)
	}
	if err := checkIntegrity(ctx, tx); err != nil {
		return nil, "", err
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return nil, "", err
	}
	fingerprint, tables, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return nil, "", failure(CodeIntegrity, err)
	}
	if !upgradeFingerprintMatches(expected, stage, fingerprint) || len(tables) == 0 {
		return nil, "", failure(CodeUnrecognizedSchema, nil)
	}
	present := make(map[string]bool, len(tables))
	for _, table := range tables {
		present[table] = true
	}
	for _, table := range []string{"characters", "blueprints", "contents", "drafts"} {
		if !present[table] {
			return nil, "", failure(CodeUpgradeUnsupported, nil)
		}
	}
	for _, table := range m01GenerationTables {
		if stage == 1 && present[table] || stage == 2 && !present[table] {
			return nil, "", failure(CodeUpgradeUnsupported, nil)
		}
		if stage == 2 {
			var count int64
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
				return nil, "", failure(CodeDomain, err)
			}
			if count != 0 {
				return nil, "", failure(CodeUpgradeUnsupported, nil)
			}
		}
	}
	return tables, fingerprint, nil
}

func verifyM01SnapshotV2(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
	var zero Evidence
	if err := privateUpgradePath(path); err != nil {
		return zero, err
	}
	before, err := snapshotIdentity(ctx, path)
	if err != nil {
		return zero, err
	}
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		return zero, failure(CodeSnapshotRead, err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return zero, failure(CodeIntegrity, err)
	}
	defer tx.Rollback()
	tables, fingerprint, err := checkM01Stage(ctx, tx, expected, 2)
	if err != nil {
		return zero, err
	}
	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
			return zero, failure(CodeIntegrity, err)
		}
		counts[table] = count
	}
	domain, err := logicalDomain(ctx, tx, tables)
	if err != nil {
		return zero, failure(CodeDomain, err)
	}
	if err := tx.Rollback(); err != nil {
		return zero, failure(CodeSnapshotRead, err)
	}
	if err := db.Close(); err != nil {
		return zero, failure(CodeSnapshotRead, err)
	}
	if err := privateUpgradePath(path); err != nil {
		return zero, err
	}
	after, err := snapshotIdentity(ctx, path)
	if err != nil {
		return zero, err
	}
	if before != after {
		return zero, failure(CodeSnapshotChanged, nil)
	}
	return Evidence{UserVersion: 2, SchemaFingerprint: fingerprint, TableCounts: counts,
		ContentIdentity: ContentIdentity{Database: before.database, WAL: before.wal}, Domain: &domain}, nil
}
