package admission

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"reflect"
)

// These strings are byte-for-byte the original TypeScript constants. SQLite
// stores their whitespace in sqlite_master; it is trusted fingerprint material.
const earlyM04ImportEffectLedgerSQL = `
CREATE TABLE import_effect_ledger (
 run_id TEXT NOT NULL REFERENCES import_runs(id) ON DELETE CASCADE,
 stage TEXT NOT NULL,
 batch_id TEXT NOT NULL,
 effect_namespace TEXT NOT NULL,
 effect_key TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('complete','partial','unknown')),
 evidence_kind TEXT NOT NULL CHECK(evidence_kind IN ('receipt','knowledge-receipt','legacy-checkpoint','terminal')),
 diagnostic_code TEXT NOT NULL DEFAULT '',
 created_at TEXT NOT NULL DEFAULT (datetime('now')),
 updated_at TEXT NOT NULL DEFAULT (datetime('now')),
 PRIMARY KEY(run_id,stage,batch_id),
 UNIQUE(effect_namespace,effect_key),
 CHECK((state='unknown' AND diagnostic_code<>'') OR (state<>'unknown'))
);
CREATE INDEX idx_import_effect_ledger_state ON import_effect_ledger(run_id,state,stage);`

const earlyM05CharacterAssetSQL = `
CREATE TABLE character_avatar_assets (
 character_id TEXT PRIMARY KEY REFERENCES characters(character_id) ON DELETE CASCADE,
 asset_revision INTEGER NOT NULL CHECK(asset_revision > 0),
 relative_path TEXT NOT NULL UNIQUE,
 content_hash TEXT NOT NULL CHECK(length(content_hash) = 64),
 mime TEXT NOT NULL CHECK(mime IN ('image/png','image/jpeg','image/webp')),
 byte_size INTEGER NOT NULL CHECK(byte_size > 0),
 source_reference TEXT NOT NULL DEFAULT ''
);
CREATE TABLE character_avatar_unresolved (
 record_id TEXT PRIMARY KEY,
 disposition TEXT NOT NULL CHECK(disposition IN ('orphan','ambiguous','unknown-fork','reference-only')),
 source_reference TEXT NOT NULL,
 source_relative_path TEXT NOT NULL,
 preserved_relative_path TEXT UNIQUE CHECK(CASE WHEN disposition = 'reference-only' THEN preserved_relative_path IS NULL ELSE preserved_relative_path IS NOT NULL END),
 content_hash TEXT CHECK(CASE WHEN disposition = 'reference-only' THEN content_hash IS NULL ELSE content_hash IS NOT NULL AND length(content_hash) = 64 END),
 mime TEXT CHECK(CASE WHEN disposition = 'reference-only' THEN mime IS NULL ELSE mime IS NOT NULL AND mime IN ('image/png','image/jpeg','image/webp','application/octet-stream') END),
 byte_size INTEGER CHECK(CASE WHEN disposition = 'reference-only' THEN byte_size IS NULL ELSE byte_size IS NOT NULL AND byte_size > 0 END),
 candidate_character_ids_json TEXT NOT NULL
);
CREATE INDEX idx_character_avatar_revision ON character_avatar_assets(asset_revision);
CREATE TRIGGER retain_character_avatar_on_delete BEFORE DELETE ON characters
WHEN EXISTS(SELECT 1 FROM character_avatar_assets WHERE character_id=OLD.character_id)
BEGIN
 INSERT OR IGNORE INTO character_avatar_unresolved(
  record_id,disposition,source_reference,source_relative_path,preserved_relative_path,
  content_hash,mime,byte_size,candidate_character_ids_json
 ) SELECT 'character-delete:' || OLD.character_id || ':' || asset_revision,
  'orphan','character-delete:' || OLD.character_id,relative_path,relative_path,
  content_hash,mime,byte_size,json_array(OLD.character_id)
 FROM character_avatar_assets WHERE character_id=OLD.character_id;
END;
`

type earlyUpgradeLane struct {
	from, to int
	ddl      string
	added    []string
}

// UpgradeSnapshotV4 applies ONLY M04 (v4 -> v5) to an authorized exclusive
// caller-owned PRIVATE copy. All UpgradeSnapshotV6 provenance, permission,
// no-sidecar, no-symlink and rollback-journal requirements also apply here.
//
// This is NOT the complete import-history migration. Both import_runs and
// import_run_receipts, and both M03 review tables, MUST be empty. Populated
// history is rejected before mutation: the original receipt/checkpoint/knowledge
// proofs and M03 generation proof have not been ported. With those proofs
// vacuous, exact original DDL creates an empty ledger and preserves ALL original
// rows. M02, integrity, FK and trusted source/target fingerprints are checked.
//
// The returned Evidence describes intermediate v5, NOT current-schema admission
// or production readiness. No automatic chain to v7 is attempted. Any error,
// including a final read error after commit, requires discarding the private copy.
func UpgradeSnapshotV4(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
	return upgradeEarlySnapshot(ctx, path, expected, earlyUpgradeLane{
		from: 4, to: 5, ddl: earlyM04ImportEffectLedgerSQL,
		added: []string{"import_effect_ledger"},
	})
}

// UpgradeSnapshotV5 applies ONLY the installed M05 SCHEMA step (v5 -> v6), with
// the same private-copy rules as UpgradeSnapshotV4. Both review tables, both
// import run/receipt tables, and the source ledger MUST be empty, because the
// original registry re-verifies M03 and M04. All other original rows, including
// donor avatar/reference fields and M02 identity evidence, are preserved exactly.
//
// Original applyM05CharacterAssets is DDL-only; its two added tables MUST remain
// empty. The separate filesystem/avatar-binding migration adapter is NOT ported:
// this API never reads, copies, resolves or certifies avatar files. Evidence is
// intermediate v6, not full-project migration, v7 admission or production proof.
// Discard the private copy on ANY error, even a post-commit verification error.
func UpgradeSnapshotV5(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
	return upgradeEarlySnapshot(ctx, path, expected, earlyUpgradeLane{
		from: 5, to: 6, ddl: earlyM05CharacterAssetSQL,
		added: []string{"character_avatar_assets", "character_avatar_unresolved"},
	})
}

func upgradeEarlySnapshot(ctx context.Context, path string, expected SchemaContract, lane earlyUpgradeLane) (Evidence, error) {
	var zero Evidence
	if err := validateContract(expected); err != nil {
		return zero, err
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
	tables, _, err := checkEarlyStage(ctx, tx, expected, lane.from)
	if err != nil {
		return zero, err
	}
	for _, table := range lane.added {
		for _, existing := range tables {
			if existing == table {
				return zero, failure(CodeUpgradeUnsupported, nil)
			}
		}
	}
	domainBefore, err := logicalDomain(ctx, tx, tables)
	if err != nil {
		return zero, failure(CodeDomain, err)
	}
	// Nothing has written yet. The immediate transaction reserves the DB, while
	// caller-owned directory exclusivity covers replacement and external writers.
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
	if _, err := tx.ExecContext(ctx, lane.ddl); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	versionSQL := "PRAGMA user_version = 5"
	if lane.to == 6 {
		versionSQL = "PRAGMA user_version = 6"
	}
	if _, err := tx.ExecContext(ctx, versionSQL); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	finalTables, _, err := checkEarlyStage(ctx, tx, expected, lane.to)
	if err != nil {
		return zero, err
	}
	if len(finalTables) != len(tables)+len(lane.added) {
		return zero, failure(CodeUnrecognizedSchema, nil)
	}
	// Exclude ONLY explicitly added empty tables. No original author-row changes
	// are expected in these narrow original M04/M05 schema lanes.
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
	return verifyEarlySnapshotStage(ctx, absolute, expected, lane.to)
}

func openEarlySnapshot(path string, readOnly bool) (*sql.DB, error) {
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	query := url.Values{}
	query.Set("mode", "rw") // Never create a missing copy.
	query.Set("_txlock", "immediate")
	if readOnly {
		query.Set("mode", "ro")
		query.Set("_query_only", "on")
		query.Del("_txlock")
	}
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}

// checkEarlyStage deliberately does not generalize public admission to old
// schemas. It verifies only the private, vacuous-proof lanes described above.
func checkEarlyStage(ctx context.Context, tx *sql.Tx, expected SchemaContract, stage int) ([]string, string, error) {
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
	if version != stage {
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
	required := []string{
		"character_identity_meta", "character_identity_origins", "review_cycles", "review_findings",
		"import_runs", "import_run_receipts", "import_run_chapters",
		"import_run_knowledge_receipts", "import_reference_documents",
	}
	empty := []string{"review_cycles", "review_findings", "import_runs", "import_run_receipts"}
	if stage >= 5 {
		required = append(required, "import_effect_ledger")
		empty = append(empty, "import_effect_ledger")
	}
	if stage >= 6 {
		required = append(required, "character_avatar_assets", "character_avatar_unresolved")
		empty = append(empty, "character_avatar_assets", "character_avatar_unresolved")
	}
	for _, table := range required {
		if !present[table] {
			return nil, "", failure(CodeUpgradeUnsupported, nil)
		}
	}
	for _, table := range empty {
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
			return nil, "", failure(CodeDomain, err)
		}
		if count != 0 {
			return nil, "", failure(CodeUpgradeUnsupported, nil)
		}
	}
	if err := verifyCharacterIdentity(ctx, tx); err != nil {
		return nil, "", failure(CodeDomain, err)
	}
	return tables, fingerprint, nil
}

// Unlike VerifySnapshotWithDomain (which correctly requires current v7), this
// private helper returns exact-stage evidence after closing the writer. It must
// not be exposed as general old-schema admission or skip the narrow proof checks.
func verifyEarlySnapshotStage(ctx context.Context, path string, expected SchemaContract, stage int) (Evidence, error) {
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
	tables, fingerprint, err := checkEarlyStage(ctx, tx, expected, stage)
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
	return Evidence{
		UserVersion: stage, SchemaFingerprint: fingerprint, TableCounts: counts,
		ContentIdentity: ContentIdentity{Database: before.database, WAL: before.wal}, Domain: &domain,
	}, nil
}
