package admission

import (
	"context"
	"database/sql"
	"reflect"
)

// UpgradeSnapshotV4Receipts migrates one narrow nonempty M04 history lane on
// an exclusive caller-owned PRIVATE copy. The source has exactly one global
// import run and one or more effect receipts, no completed-batch JSON
// checkpoints, chapter/knowledge rows, or review cycles. Original M04 then
// inserts one ledger row per receipt and no inferred legacy progress rows.
//
// This is not a general M04 implementation. The caller must discard the copy
// on ANY error (including a post-commit read error), retain the original, and
// establish source provenance and writer exclusion independently. In
// particular this function never upgrades an author path in place.
func UpgradeSnapshotV4Receipts(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
	var zero Evidence
	if ctx == nil {
		return zero, failure(CodeUpgradeUnsupported, nil)
	}
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
	tables, err := receiptOnlyStage(ctx, tx, expected, 4)
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
	if _, err := tx.ExecContext(ctx, earlyM04ImportEffectLedgerSQL); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO import_effect_ledger(
 run_id,stage,batch_id,effect_namespace,effect_key,state,evidence_kind,diagnostic_code
) SELECT run_id,stage,batch_id,effect_namespace,effect_key,
 CASE state WHEN 'committed' THEN 'complete' ELSE 'partial' END,
 'receipt',CASE state WHEN 'prepared' THEN 'EFFECT_PREPARED_NOT_COMMITTED' ELSE '' END
 FROM import_run_receipts ORDER BY run_id,stage,batch_id`); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 5"); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	finalTables, err := receiptOnlyStage(ctx, tx, expected, 5)
	if err != nil {
		return zero, err
	}
	if len(finalTables) != len(tables)+1 {
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
	return verifyReceiptOnlyStage(ctx, absolute, expected)
}

// receiptOnlyStage validates the whole narrow domain both before and after
// migration. Do not use it to admit old schema versions as current projects.
func receiptOnlyStage(ctx context.Context, tx *sql.Tx, expected SchemaContract, stage int) ([]string, error) {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return nil, failure(CodeIntegrity, err)
	}
	if version != stage {
		return nil, failure(CodeUpgradeUnsupported, nil)
	}
	if err := checkIntegrity(ctx, tx); err != nil {
		return nil, err
	}
	if err := checkForeignKeys(ctx, tx); err != nil {
		return nil, err
	}
	fingerprint, tables, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return nil, failure(CodeIntegrity, err)
	}
	if !upgradeFingerprintMatches(expected, stage, fingerprint) || len(tables) == 0 {
		return nil, failure(CodeUnrecognizedSchema, nil)
	}
	present := make(map[string]bool, len(tables))
	for _, table := range tables {
		present[table] = true
	}
	for _, required := range []string{"character_identity_meta", "character_identity_origins", "review_cycles", "review_findings", "import_runs", "import_run_receipts", "import_run_chapters", "import_run_knowledge_receipts", "import_reference_documents", "import_run_sources", "import_run_source_chapters"} {
		if !present[required] {
			return nil, failure(CodeUpgradeUnsupported, nil)
		}
	}
	if stage == 4 && present["import_effect_ledger"] || stage == 5 && !present["import_effect_ledger"] {
		return nil, failure(CodeUpgradeUnsupported, nil)
	}
	for _, table := range []string{"review_cycles", "review_findings", "import_run_chapters", "import_run_knowledge_receipts", "import_reference_documents", "import_run_sources", "import_run_source_chapters"} {
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
			return nil, failure(CodeDomain, err)
		}
		if count != 0 {
			return nil, failure(CodeUpgradeUnsupported, nil)
		}
	}
	var runCount, receiptCount int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_runs WHERE stage='global' AND status='running' AND completed_batches_json='{}' AND completed_chapters=0`).Scan(&runCount); err != nil {
		return nil, failure(CodeDomain, err)
	}
	var allRuns int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM import_runs").Scan(&allRuns); err != nil {
		return nil, failure(CodeDomain, err)
	}
	if runCount != 1 || allRuns != 1 {
		return nil, failure(CodeUpgradeUnsupported, nil)
	}
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM import_run_receipts").Scan(&receiptCount); err != nil {
		return nil, failure(CodeDomain, err)
	}
	if receiptCount < 1 || receiptCount > 1024 {
		return nil, failure(CodeUpgradeUnsupported, nil)
	}
	var invalid int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_run_receipts AS r JOIN import_runs AS run ON run.id=r.run_id
 WHERE r.stage<>'global' OR r.batch_id='' OR r.effect_key='' OR r.effect_namespace<>run.effect_namespace
 OR r.state NOT IN ('prepared','committed')`).Scan(&invalid); err != nil {
		return nil, failure(CodeDomain, err)
	}
	if invalid != 0 {
		return nil, failure(CodeUpgradeUnsupported, nil)
	}
	if err := verifyCharacterIdentity(ctx, tx); err != nil {
		return nil, failure(CodeDomain, err)
	}
	if stage == 5 {
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_effect_ledger AS ledger
 LEFT JOIN import_run_receipts AS receipt ON receipt.run_id=ledger.run_id AND receipt.stage=ledger.stage AND receipt.batch_id=ledger.batch_id
 WHERE receipt.run_id IS NULL OR ledger.effect_namespace<>receipt.effect_namespace OR ledger.effect_key<>receipt.effect_key
 OR ledger.state<>CASE receipt.state WHEN 'committed' THEN 'complete' ELSE 'partial' END
 OR ledger.evidence_kind<>'receipt' OR ledger.diagnostic_code<>CASE receipt.state WHEN 'prepared' THEN 'EFFECT_PREPARED_NOT_COMMITTED' ELSE '' END`).Scan(&invalid); err != nil {
			return nil, failure(CodeDomain, err)
		}
		if invalid != 0 {
			return nil, failure(CodeDomain, nil)
		}
		var ledgerCount int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM import_effect_ledger").Scan(&ledgerCount); err != nil {
			return nil, failure(CodeDomain, err)
		}
		if ledgerCount != receiptCount {
			return nil, failure(CodeDomain, nil)
		}
	}
	return tables, nil
}

func verifyReceiptOnlyStage(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
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
	tables, err := receiptOnlyStage(ctx, tx, expected, 5)
	if err != nil {
		return zero, err
	}
	fingerprint, _, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return zero, failure(CodeIntegrity, err)
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
	return Evidence{UserVersion: 5, SchemaFingerprint: fingerprint, TableCounts: counts,
		ContentIdentity: ContentIdentity{Database: before.database, WAL: before.wal}, Domain: &domain}, nil
}
