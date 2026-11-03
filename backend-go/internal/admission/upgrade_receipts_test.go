package admission

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

type m04LedgerRow struct {
	run, stage, batch, namespace, effect, state, evidence, diagnostic string
}

func readM04ReceiptLedger(t *testing.T, path string) []m04LedgerRow {
	t.Helper()
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT run_id,stage,batch_id,effect_namespace,effect_key,state,evidence_kind,diagnostic_code
 FROM import_effect_ledger ORDER BY run_id,stage,batch_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var output []m04LedgerRow
	for rows.Next() {
		var row m04LedgerRow
		if err := rows.Scan(&row.run, &row.stage, &row.batch, &row.namespace, &row.effect, &row.state, &row.evidence, &row.diagnostic); err != nil {
			t.Fatal(err)
		}
		output = append(output, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return output
}

func receiptOraclePaths(t *testing.T) (upgradeOracle, string, string) {
	t.Helper()
	oracle := loadUpgradeOracle(t)
	source := oracle.fixtures["baseline-v4-receipts.db"]
	expected := oracle.fixtures["baseline-v4-receipts-upgraded-v5.db"]
	if source == "" || expected == "" {
		t.Fatal("original TypeScript populated M04 fixtures are mandatory")
	}
	return oracle, source, expected
}

func TestUpgradeV4ReceiptsMatchesOriginalTypeScriptM04(t *testing.T) {
	oracle, source, expected := receiptOraclePaths(t)
	sourceHash, expectedHash := hashFile(t, source), hashFile(t, expected)
	path := copyUpgradeFixture(t, source)
	actual, err := UpgradeSnapshotV4Receipts(context.Background(), path, oracle.contract)
	if err != nil {
		t.Fatal(err)
	}
	if actual.UserVersion != 5 || actual.TableCounts["import_effect_ledger"] != 2 || actual.Domain == nil || actual.ContentIdentity.Database != hashFile(t, path) {
		t.Fatalf("incomplete post-upgrade evidence: %+v", actual)
	}
	if got, want := readM04ReceiptLedger(t, path), readM04ReceiptLedger(t, expected); !reflect.DeepEqual(got, want) {
		t.Fatalf("Go receipt ledger differs from original M04: got=%+v want=%+v", got, want)
	}
	if hashFile(t, source) != sourceHash || hashFile(t, expected) != expectedHash {
		t.Fatal("upgrade modified shared TypeScript oracle fixture")
	}
	_, err = VerifySnapshotWithDomain(context.Background(), path, oracle.contract)
	requireCode(t, err, CodeMigrationRequired)
	_, err = UpgradeSnapshotV4Receipts(context.Background(), path, oracle.contract)
	requireCode(t, err, CodeUpgradeUnsupported)
}

func assertReceiptV4Unchanged(t *testing.T, path, before string) {
	t.Helper()
	if hashFile(t, path) != before {
		t.Fatal("rejected receipt migration modified private database")
	}
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, ledger int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='import_effect_ledger'").Scan(&ledger); err != nil {
		t.Fatal(err)
	}
	if version != 4 || ledger != 0 {
		t.Fatalf("partial M04 escaped rollback: version=%d ledger=%d", version, ledger)
	}
}

func TestUpgradeV4ReceiptsRejectsUnprovedHistoryBeforeMutation(t *testing.T) {
	oracle, source, _ := receiptOraclePaths(t)
	for _, tc := range []struct{ name, mutation string }{
		{"checkpoint", `UPDATE import_runs SET completed_batches_json='{"global":["unknown"]}'`},
		{"progress", `UPDATE import_runs SET completed_chapters=1`},
		{"unrelated-stage", `UPDATE import_runs SET stage='knowledge'`},
		{"missing-receipts", `DELETE FROM import_run_receipts`},
		{"knowledge-receipt", `INSERT INTO import_run_chapters(run_id,chapter_number,content_fingerprint,content_size,content_snapshot) VALUES('synthetic-m04-run',1,'hash',1,'x')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := copyUpgradeFixture(t, source)
			mutateUpgradeFixture(t, path, tc.mutation)
			before := hashFile(t, path)
			_, err := UpgradeSnapshotV4Receipts(context.Background(), path, oracle.contract)
			requireCode(t, err, CodeUpgradeUnsupported)
			assertReceiptV4Unchanged(t, path, before)
		})
	}
}

func TestUpgradeV4ReceiptsTargetFingerprintFailureRollsBack(t *testing.T) {
	oracle, source, _ := receiptOraclePaths(t)
	path := copyUpgradeFixture(t, source)
	before := hashFile(t, path)
	var contract SchemaContract
	for _, entry := range oracle.contract {
		if entry.Version == 5 {
			contract = append(contract, VersionContract{Version: 5, Fingerprints: []string{strings.Repeat("a", 64)}})
		} else {
			contract = append(contract, entry)
		}
	}
	_, err := UpgradeSnapshotV4Receipts(context.Background(), path, contract)
	requireCode(t, err, CodeUnrecognizedSchema)
	assertReceiptV4Unchanged(t, path, before)
}
