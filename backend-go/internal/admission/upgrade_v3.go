package admission

import (
	"context"
	"database/sql"
	"reflect"
)

// m03ReviewCycleSQL is byte-for-byte M03_REVIEW_CYCLE_SQL from the original
// TypeScript migration. SQLite persists its whitespace in sqlite_master.
const m03ReviewCycleSQL = `
CREATE TABLE review_cycles (
 cycle_id TEXT PRIMARY KEY,
 root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id),
 review_id INTEGER NOT NULL UNIQUE REFERENCES reviews(id) ON DELETE CASCADE,
 review_content_hash TEXT NOT NULL,
 confirmation_review_id INTEGER UNIQUE REFERENCES reviews(id),
 confirmation_content_hash TEXT,
 revision_id INTEGER UNIQUE REFERENCES revisions(id),
 source_hash TEXT NOT NULL,
 finding_set_hash TEXT NOT NULL,
 comparison_version INTEGER NOT NULL,
 revision_status TEXT NOT NULL CHECK(revision_status IN ('not-generated','generated','merge-committed')),
 merged_hash TEXT,
 recheck_count INTEGER NOT NULL CHECK(recheck_count IN (0,1)),
 recheck_attempt_id TEXT UNIQUE REFERENCES generation_attempts(attempt_id),
 CHECK((confirmation_review_id IS NULL) = (confirmation_content_hash IS NULL)),
 CHECK((revision_status='not-generated' AND revision_id IS NULL AND merged_hash IS NULL AND recheck_count=0 AND recheck_attempt_id IS NULL)
    OR (revision_status='generated' AND revision_id IS NOT NULL AND merged_hash IS NULL AND recheck_count=0 AND recheck_attempt_id IS NULL)
    OR (revision_status='merge-committed' AND revision_id IS NOT NULL AND merged_hash IS NOT NULL
      AND ((recheck_count=0 AND recheck_attempt_id IS NULL) OR (recheck_count=1 AND recheck_attempt_id IS NOT NULL))))
);
CREATE TABLE review_findings (
 cycle_id TEXT NOT NULL REFERENCES review_cycles(cycle_id) ON DELETE CASCADE,
 finding_id TEXT NOT NULL,
 source_project_id TEXT NOT NULL,
 source_epoch TEXT NOT NULL,
 source_id TEXT NOT NULL,
 source_revision INTEGER NOT NULL,
 source_content_hash TEXT NOT NULL,
 span_start INTEGER,
 span_end INTEGER,
 span_unit TEXT,
 excerpt_hash TEXT,
 occurrence INTEGER,
 category TEXT NOT NULL,
 target_id TEXT,
 kind TEXT NOT NULL CHECK(kind IN ('objective','literary')),
 problem_text TEXT NOT NULL,
 expected_text TEXT,
 status TEXT NOT NULL CHECK(status IN ('unverified','unresolved','unknown','resolved','author-waived')),
 evidence_hash TEXT,
 confirmation_item_index INTEGER,
 PRIMARY KEY(cycle_id,finding_id),
 CHECK((status IN ('unverified','author-waived') AND span_start IS NULL AND span_end IS NULL AND span_unit IS NULL
    AND excerpt_hash IS NULL AND occurrence IS NULL AND target_id IS NULL)
   OR (status<>'unverified' AND span_start IS NOT NULL AND span_end IS NOT NULL AND span_unit='utf16-code-unit'
    AND excerpt_hash IS NOT NULL AND occurrence IS NOT NULL AND target_id IS NOT NULL)),
 CHECK((status='author-waived' AND confirmation_item_index IS NOT NULL)
   OR (status<>'author-waived' AND confirmation_item_index IS NULL))
);`

// UpgradeSnapshotV3 applies only original M03 (v3 -> v4) to a caller-owned,
// exclusive PRIVATE database copy. It never opens an author's source database.
// M03 adds two empty tables and does not infer/backfill cycles from existing
// reviews. Exact trusted v3/v4 fingerprints, M02 identity, FK/integrity and all
// pre-existing row content are checked before and after the single transaction.
// This returns intermediate v4 evidence, not current v7 project admission.
// On any error, including one after commit, the caller must discard the copy.
func UpgradeSnapshotV3(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
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
	tables, _, err := checkM03Stage(ctx, tx, expected, 3)
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
	if _, err := tx.ExecContext(ctx, m03ReviewCycleSQL); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 4"); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	finalTables, _, err := checkM03Stage(ctx, tx, expected, 4)
	if err != nil {
		return zero, err
	}
	if len(finalTables) != len(tables)+2 {
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
	return verifyM03SnapshotV4(ctx, absolute, expected)
}

// checkM03Stage accepts only the exact source or target of this private lane.
// Populated M03 tables have an unported generation/confirmation proof and are
// therefore rejected even when the schema fingerprint is recognized.
func checkM03Stage(ctx context.Context, tx *sql.Tx, expected SchemaContract, stage int) ([]string, string, error) {
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
	if version != stage || (stage != 3 && stage != 4) {
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
	for _, table := range []string{
		"character_identity_meta", "character_identity_origins", "reviews", "revisions",
		"generation_roots", "generation_runs", "generation_attempts",
	} {
		if !present[table] {
			return nil, "", failure(CodeUpgradeUnsupported, nil)
		}
	}
	for _, table := range []string{"review_cycles", "review_findings"} {
		if stage == 3 && present[table] || stage == 4 && !present[table] {
			return nil, "", failure(CodeUpgradeUnsupported, nil)
		}
		if stage == 4 {
			var count int64
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
				return nil, "", failure(CodeDomain, err)
			}
			if count != 0 {
				return nil, "", failure(CodeUpgradeUnsupported, nil)
			}
		}
	}
	if err := verifyCharacterIdentity(ctx, tx); err != nil {
		return nil, "", failure(CodeDomain, err)
	}
	return tables, fingerprint, nil
}

func verifyM03SnapshotV4(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
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
	tables, fingerprint, err := checkM03Stage(ctx, tx, expected, 4)
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
	return Evidence{UserVersion: 4, SchemaFingerprint: fingerprint, TableCounts: counts,
		ContentIdentity: ContentIdentity{Database: before.database, WAL: before.wal}, Domain: &domain}, nil
}
