package admission

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func loadM03UpgradeOracle(t *testing.T) upgradeOracle {
	t.Helper()
	oracle := loadUpgradeOracle(t)
	for _, name := range []string{
		"baseline-v3.db", "donor-v3.db", "baseline-v3-upgraded-v4.db", "donor-v3-upgraded-v4.db",
		"baseline-v3-history.db", "baseline-v3-history-upgraded-v4.db",
	} {
		if oracle.fixtures[name] == "" {
			t.Fatalf("original TypeScript oracle is missing mandatory M03 fixture %s", name)
		}
	}
	return oracle
}

func assertM03RejectedCopyUnchanged(t *testing.T, path, before string) {
	t.Helper()
	if hashFile(t, path) != before {
		t.Fatal("rejected M03 mutated private database bytes")
	}
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, cycles, findings int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='review_cycles'").Scan(&cycles); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='review_findings'").Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if version != 3 || cycles != 0 || findings != 0 {
		t.Fatalf("partial M03 escaped rollback: version=%d cycles=%d findings=%d", version, cycles, findings)
	}
}

func TestM03PrivateUpgradeMatchesOriginalTypeScript(t *testing.T) {
	oracle := loadM03UpgradeOracle(t)
	for _, tc := range []struct{ name, source, expected string }{
		{"baseline", "baseline-v3.db", "baseline-v3-upgraded-v4.db"},
		{"donor", "donor-v3.db", "donor-v3-upgraded-v4.db"},
		{"baseline-history", "baseline-v3-history.db", "baseline-v3-history-upgraded-v4.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := oracle.fixtures[tc.source]
			expectedSource := oracle.fixtures[tc.expected]
			sourceHash, expectedHash := hashFile(t, source), hashFile(t, expectedSource)
			path := copyUpgradeFixture(t, source)
			expectedPath := copyUpgradeFixture(t, expectedSource)
			expected, err := verifyM03SnapshotV4(context.Background(), expectedPath, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := UpgradeSnapshotV3(context.Background(), path, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			if actual.UserVersion != 4 || actual.SchemaFingerprint != expected.SchemaFingerprint ||
				!reflect.DeepEqual(actual.TableCounts, expected.TableCounts) || actual.Domain == nil ||
				!reflect.DeepEqual(actual.Domain, expected.Domain) {
				t.Fatalf("Go M03 differs from original TypeScript: actual=%+v expected=%+v", actual, expected)
			}
			if actual.TableCounts["review_cycles"] != 0 || actual.TableCounts["review_findings"] != 0 {
				t.Fatal("M03 invented historical cycle or finding records")
			}
			if tc.name == "baseline-history" && actual.TableCounts["reviews"] != 1 {
				t.Fatal("M03 lost an existing historical review")
			}
			if actual.ContentIdentity.Database != hashFile(t, path) {
				t.Fatal("returned stale physical evidence")
			}
			verified, err := verifyM03SnapshotV4(context.Background(), path, oracle.contract)
			if err != nil || !reflect.DeepEqual(actual, verified) {
				t.Fatalf("fresh M03 readback differs: %v", err)
			}
			if hashFile(t, source) != sourceHash || hashFile(t, expectedSource) != expectedHash {
				t.Fatal("M03 touched original TypeScript oracle fixture")
			}
			_, err = VerifySnapshotWithDomain(context.Background(), path, oracle.contract)
			requireCode(t, err, CodeMigrationRequired)
			beforeRetry := hashFile(t, path)
			_, err = UpgradeSnapshotV3(context.Background(), path, oracle.contract)
			requireCode(t, err, CodeUpgradeUnsupported)
			if hashFile(t, path) != beforeRetry {
				t.Fatal("repeat M03 changed v4 copy")
			}
		})
	}
}

func TestM03PrivateUpgradeRejectsUntrustedTargetBeforeCommit(t *testing.T) {
	oracle := loadM03UpgradeOracle(t)
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v3.db"])
	before := hashFile(t, path)
	var contract SchemaContract
	for _, entry := range oracle.contract {
		if entry.Version == 4 {
			contract = append(contract, VersionContract{Version: 4, Fingerprints: []string{strings.Repeat("a", 64)}})
		} else {
			contract = append(contract, entry)
		}
	}
	_, err := UpgradeSnapshotV3(context.Background(), path, contract)
	requireCode(t, err, CodeUnrecognizedSchema)
	assertM03RejectedCopyUnchanged(t, path, before)
}

func TestM03PrivateUpgradeRejectsUnknownSourceAndUnsafeCopy(t *testing.T) {
	oracle := loadM03UpgradeOracle(t)
	for _, tc := range []struct{ name, mutation, code string }{
		{"unexpected_index", "CREATE INDEX unexpected_m03_index ON contents(body)", CodeUnrecognizedSchema},
		{"wrong_version", "PRAGMA user_version=4", CodeUpgradeUnsupported},
		{"newer_version", "PRAGMA user_version=8", CodeNewerSchema},
		{"identity_deleted", "DELETE FROM character_identity_meta", CodeDomain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := copyUpgradeFixture(t, oracle.fixtures["baseline-v3.db"])
			mutateUpgradeFixture(t, path, tc.mutation)
			before := hashFile(t, path)
			_, err := UpgradeSnapshotV3(context.Background(), path, oracle.contract)
			requireCode(t, err, tc.code)
			if hashFile(t, path) != before {
				t.Fatal("rejected M03 source changed")
			}
		})
	}
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v3.db"])
	if err := os.WriteFile(path+"-wal", nil, 0600); err != nil {
		t.Fatal(err)
	}
	before := hashFile(t, path)
	_, err := UpgradeSnapshotV3(context.Background(), path, oracle.contract)
	requireCode(t, err, CodeUnsafeSnapshot)
	if hashFile(t, path) != before {
		t.Fatal("M03 touched rejected WAL copy")
	}
}

func TestM03PrivateUpgradeMissingAndCanceled(t *testing.T) {
	oracle := loadM03UpgradeOracle(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.db")
	_, err := UpgradeSnapshotV3(context.Background(), missing, oracle.contract)
	requireCode(t, err, CodeMissingSnapshot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = UpgradeSnapshotV3(ctx, missing, oracle.contract)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled M03 returned %v", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("M03 created a missing private copy")
	}
}
