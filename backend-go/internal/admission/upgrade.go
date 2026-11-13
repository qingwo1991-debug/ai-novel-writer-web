package admission

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
)

const (
	CodeUpgradeUnsupported = "SCHEMA_UPGRADE_UNSUPPORTED"
	CodeUpgradeFailed      = "SCHEMA_UPGRADE_FAILED"
)

// This is byte-for-byte M06_REVIEW_CYCLE_MERGE_SQL from
// electron/migrations/m03-review-cycle.ts. Whitespace is schema identity material.
const m06ReviewCycleMergeSQL = `CREATE TABLE review_cycle_merges (
 cycle_id TEXT PRIMARY KEY REFERENCES review_cycles(cycle_id) ON DELETE CASCADE,
 body TEXT NOT NULL
);`

// UpgradeSnapshotV6 upgrades only an already-authorized, exclusive, caller-owned
// PRIVATE copy, never an original project path. As with VerifySnapshot, the caller
// must provide provenance and exclude directory replacement/concurrent writers;
// permissions cannot establish that a database is a copy. In addition to the
// verifier's no-symlink/single-link guards, this mutating API requires an exact
// 0700 containing directory, 0600 DB, rollback-journal header, and NO sidecars.
// It does not copy, checkpoint, repair, or switch journal modes.
//
// The supported lane is deliberately narrower than Node M06: both review_cycles
// and review_findings MUST be empty. Original M06 verifies the complete M03
// generation/review/recheck proof and backfills merged draft bodies. That proof
// is not ported here; populated cycles fail BEFORE any DDL or version mutation.
// On the empty lane its proof/backfill is vacuous. Existing M02 identity, exact
// trusted v6/v7 fingerprints, integrity/FK checks, and logical preservation of
// every pre-existing user table are required inside one immediate transaction.
// Logical proof uses private external sorting; single encoded rows cap at64MiB.
//
// The DDL and user_version commit together only after all transactional checks.
// A final read-only VerifySnapshotWithDomain returns fresh v7 evidence. A final
// I/O/cancellation error can occur AFTER commit; callers must discard the private
// copy on ANY error, not retry it against an author's original database.
func UpgradeSnapshotV6(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
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

	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := url.Values{}
	query.Set("mode", "rw") // Never create a missing snapshot.
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_txlock", "immediate")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return zero, failure(CodeIntegrity, err)
	}
	if version < 0 {
		return zero, failure(CodeInvalidVersion, nil)
	}
	if version > CurrentSchemaVersion {
		return zero, failure(CodeNewerSchema, nil)
	}
	if version != 6 {
		return zero, failure(CodeUpgradeUnsupported, nil)
	}
	if err := checkIntegrity(ctx, tx); err != nil {
		return zero, err
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return zero, err
	}
	fingerprint, tables, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return zero, failure(CodeIntegrity, err)
	}
	if !upgradeFingerprintMatches(expected, 6, fingerprint) || len(tables) == 0 {
		return zero, failure(CodeUnrecognizedSchema, nil)
	}
	present := make(map[string]bool, len(tables))
	for _, table := range tables {
		present[table] = true
	}
	if !present["review_cycles"] || !present["review_findings"] || !present["character_identity_meta"] || present["review_cycle_merges"] {
		return zero, failure(CodeUpgradeUnsupported, nil)
	}
	for _, table := range []string{"review_cycles", "review_findings"} {
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
			return zero, failure(CodeDomain, err)
		}
		if count != 0 {
			return zero, failure(CodeUpgradeUnsupported, nil)
		}
	}
	if err := verifyCharacterIdentity(ctx, tx); err != nil {
		return zero, failure(CodeDomain, err)
	}
	domainBefore, err := logicalDomain(ctx, tx, tables)
	if err != nil {
		return zero, failure(CodeDomain, err)
	}
	// Nothing has written yet. Detect physical drift before the first mutation;
	// the SQLite RESERVED lock and caller's exclusivity cover the remainder.
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

	if _, err := tx.ExecContext(ctx, m06ReviewCycleMergeSQL); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 7"); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if err := checkIntegrity(ctx, tx); err != nil {
		return zero, err
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return zero, err
	}
	if err := verifyCharacterIdentity(ctx, tx); err != nil {
		return zero, failure(CodeDomain, err)
	}
	finalFingerprint, finalTables, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return zero, failure(CodeIntegrity, err)
	}
	if !upgradeFingerprintMatches(expected, 7, finalFingerprint) || len(finalTables) != len(tables)+1 {
		return zero, failure(CodeUnrecognizedSchema, nil)
	}
	// Compare only ORIGINAL tables, so the explicitly allowed M06 schema addition
	// cannot conceal an author-row change. Also prove the new backfill is empty.
	domainAfter, err := logicalDomain(ctx, tx, tables)
	if err != nil {
		return zero, failure(CodeDomain, err)
	}
	if !reflect.DeepEqual(domainBefore, domainAfter) {
		return zero, failure(CodeDomain, nil)
	}
	var mergeCount int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM review_cycle_merges").Scan(&mergeCount); err != nil {
		return zero, failure(CodeDomain, err)
	}
	if mergeCount != 0 {
		return zero, failure(CodeDomain, nil)
	}
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return zero, failure(CodeIntegrity, err)
	}
	if version != CurrentSchemaVersion {
		return zero, failure(CodeInvalidVersion, nil)
	}
	if err := tx.Commit(); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if err := db.Close(); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	return VerifySnapshotWithDomain(ctx, absolute, expected)
}

func upgradeFingerprintMatches(expected SchemaContract, version int, fingerprint string) bool {
	for _, entry := range expected {
		if entry.Version != version {
			continue
		}
		for _, accepted := range entry.Fingerprints {
			if accepted == fingerprint {
				return true
			}
		}
	}
	return false
}

func privateUpgradePath(path string) error {
	dir, err := os.Lstat(filepath.Dir(path))
	if err != nil || !dir.IsDir() || dir.Mode().Perm() != 0700 {
		return failure(CodeUnsafeSnapshot, err)
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return failure(CodeMissingSnapshot, err)
	}
	if err != nil {
		return failure(CodeSnapshotRead, err)
	}
	if !info.Mode().IsRegular() || !singleLink(info) || info.Mode().Perm() != 0600 {
		return failure(CodeUnsafeSnapshot, nil)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, err := os.Lstat(path + suffix)
		if !errors.Is(err, os.ErrNotExist) {
			return failure(CodeUnsafeSnapshot, err)
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return failure(CodeSnapshotRead, err)
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(info, opened) {
		file.Close()
		return failure(CodeSnapshotChanged, statErr)
	}
	var header [20]byte
	_, readErr := file.ReadAt(header[:], 0)
	closeErr := file.Close()
	if readErr != nil {
		return failure(CodeIntegrity, readErr)
	}
	if closeErr != nil {
		return failure(CodeSnapshotRead, closeErr)
	}
	// WAL databases can create sidecars even without DDL. Reject before SQLite
	// opens the writable connection, rather than silently checkpointing a copy.
	if header[18] != 1 || header[19] != 1 {
		return failure(CodeUpgradeUnsupported, nil)
	}
	return nil
}
