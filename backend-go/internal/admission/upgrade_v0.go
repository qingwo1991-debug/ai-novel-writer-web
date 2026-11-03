package admission

import (
	"context"
	"database/sql"
	"reflect"
)

// m00BlueprintSQL is the exact SQL passed to db.exec by the installed
// ensureBaselineBlueprintTables implementation. A recognized v0 source already
// has all other baseline tables; IF NOT EXISTS retains an existing pair.
const m00BlueprintSQL = `
      CREATE TABLE IF NOT EXISTS blueprint_commit_operations (
        operation_id TEXT PRIMARY KEY,
        payload_hash TEXT NOT NULL,
        mode TEXT NOT NULL CHECK(mode IN ('full', 'replace-range')),
        start_chapter INTEGER NOT NULL,
        end_chapter INTEGER NOT NULL,
        character_sync_input TEXT NOT NULL,
        committed_at TEXT NOT NULL DEFAULT (datetime('now'))
      );

      CREATE TABLE IF NOT EXISTS blueprint_character_sync_operations (
        operation_id TEXT PRIMARY KEY,
        blueprint_commit_operation_id TEXT NOT NULL UNIQUE,
        blueprint_commit_payload_hash TEXT NOT NULL,
        status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending', 'completed')),
        start_chapter INTEGER NOT NULL,
        end_chapter INTEGER NOT NULL,
        character_sync_input TEXT NOT NULL,
        completion_receipt TEXT DEFAULT NULL,
        created_at TEXT NOT NULL DEFAULT (datetime('now')),
        updated_at TEXT NOT NULL DEFAULT (datetime('now')),
        completed_at TEXT DEFAULT NULL,
        FOREIGN KEY (blueprint_commit_operation_id)
          REFERENCES blueprint_commit_operations(operation_id) ON DELETE CASCADE
      );
      CREATE INDEX IF NOT EXISTS idx_blueprint_character_sync_status
        ON blueprint_character_sync_operations(status, created_at);
    `

// UpgradeSnapshotV0 applies only the installed M00 step to an exclusive,
// caller-owned PRIVATE copy with an exact registered v0 fingerprint. The
// installed registry's v0 catalog already contains initializeLegacyBaselineSchema
// output. This lane never attempts that broad historical initialization on an
// unknown author database. It creates only the two lazy blueprint tables when
// absent and advances user_version to 1. Existing rows are preserved exactly.
// The returned evidence is intermediate v1, not a current-schema admission.
// Discard the copy after any error, including one after commit.
func UpgradeSnapshotV0(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
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
	tables, _, hadPair, err := checkM00Stage(ctx, tx, expected, 0)
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
	if _, err := tx.ExecContext(ctx, m00BlueprintSQL); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	finalTables, _, finalPair, err := checkM00Stage(ctx, tx, expected, 1)
	if err != nil {
		return zero, err
	}
	if !finalPair || len(finalTables) != len(tables)+boolTableDelta(hadPair) {
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
	return verifyM00SnapshotV1(ctx, absolute, expected)
}

func boolTableDelta(hadPair bool) int {
	if hadPair {
		return 0
	}
	return 2
}

func checkM00Stage(ctx context.Context, tx *sql.Tx, expected SchemaContract, stage int) ([]string, string, bool, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, "", false, failure(CodeIntegrity, err)
	}
	if version > CurrentSchemaVersion {
		return nil, "", false, failure(CodeNewerSchema, nil)
	}
	if version != stage || (stage != 0 && stage != 1) {
		return nil, "", false, failure(CodeUpgradeUnsupported, nil)
	}
	if err := checkIntegrity(ctx, tx); err != nil {
		return nil, "", false, err
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return nil, "", false, err
	}
	fingerprint, tables, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return nil, "", false, failure(CodeIntegrity, err)
	}
	if !upgradeFingerprintMatches(expected, stage, fingerprint) || len(tables) == 0 {
		return nil, "", false, failure(CodeUnrecognizedSchema, nil)
	}
	present := make(map[string]bool, len(tables))
	for _, table := range tables {
		present[table] = true
	}
	for _, table := range []string{"project_core", "blueprints", "characters", "contents", "drafts"} {
		if !present[table] {
			return nil, "", false, failure(CodeUpgradeUnsupported, nil)
		}
	}
	commit, sync := present["blueprint_commit_operations"], present["blueprint_character_sync_operations"]
	if commit != sync || stage == 1 && !commit {
		return nil, "", false, failure(CodeUpgradeUnsupported, nil)
	}
	return tables, fingerprint, commit, nil
}

func verifyM00SnapshotV1(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
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
	tables, fingerprint, _, err := checkM00Stage(ctx, tx, expected, 1)
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
	return Evidence{UserVersion: 1, SchemaFingerprint: fingerprint, TableCounts: counts,
		ContentIdentity: ContentIdentity{Database: before.database, WAL: before.wal}, Domain: &domain}, nil
}
