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

func loadM00Oracle(t *testing.T) upgradeOracle {
	t.Helper()
	oracle := loadUpgradeOracle(t)
	for _, family := range []string{"baseline", "donor"} {
		for _, name := range []string{family + "-v0.db", family + "-v0-upgraded-v1.db", family + "-v1.db"} {
			if oracle.fixtures[name] == "" {
				t.Fatalf("original TypeScript oracle lacks M00 fixture %s", name)
			}
		}
	}
	return oracle
}

func TestM00PrivateUpgradeMatchesOriginalTypeScript(t *testing.T) {
	oracle := loadM00Oracle(t)
	for _, family := range []string{"baseline", "donor"} {
		t.Run(family, func(t *testing.T) {
			source := oracle.fixtures[family+"-v0.db"]
			expectedSource := oracle.fixtures[family+"-v0-upgraded-v1.db"]
			sourceHash, expectedHash := hashFile(t, source), hashFile(t, expectedSource)
			path := copyUpgradeFixture(t, source)
			expectedPath := copyUpgradeFixture(t, expectedSource)
			expected, err := verifyM00SnapshotV1(context.Background(), expectedPath, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := UpgradeSnapshotV0(context.Background(), path, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			if actual.UserVersion != 1 || actual.SchemaFingerprint != expected.SchemaFingerprint ||
				!reflect.DeepEqual(actual.TableCounts, expected.TableCounts) || actual.Domain == nil ||
				!reflect.DeepEqual(actual.Domain, expected.Domain) {
				t.Fatalf("Go M00 differs from original TypeScript: actual=%+v expected=%+v", actual, expected)
			}
			if actual.TableCounts["blueprint_commit_operations"] != 0 || actual.TableCounts["blueprint_character_sync_operations"] != 0 {
				t.Fatal("M00 invented blueprint operation rows")
			}
			if actual.ContentIdentity.Database != hashFile(t, path) {
				t.Fatal("returned stale M00 evidence")
			}
			verified, err := verifyM00SnapshotV1(context.Background(), path, oracle.contract)
			if err != nil || !reflect.DeepEqual(actual, verified) {
				t.Fatalf("M00 readback differs: %v", err)
			}
			if hashFile(t, source) != sourceHash || hashFile(t, expectedSource) != expectedHash {
				t.Fatal("M00 touched original TypeScript fixture")
			}
			_, err = VerifySnapshotWithDomain(context.Background(), path, oracle.contract)
			requireCode(t, err, CodeMigrationRequired)
			beforeRetry := hashFile(t, path)
			_, err = UpgradeSnapshotV0(context.Background(), path, oracle.contract)
			requireCode(t, err, CodeUpgradeUnsupported)
			if hashFile(t, path) != beforeRetry {
				t.Fatal("repeated M00 changed v1 private copy")
			}
		})
	}
}

func TestM00AcceptsRegisteredLazyV0WithoutDeletingBlueprintHistory(t *testing.T) {
	oracle := loadM00Oracle(t)
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v1.db"])
	mutateUpgradeFixture(t, path, "PRAGMA user_version=0", `INSERT INTO blueprint_commit_operations
 (operation_id,payload_hash,mode,start_chapter,end_chapter,character_sync_input)
 VALUES('m00-old-op','hash','full',1,2,'{}')`)
	// The registered v0 catalog includes this same lazy-table schema even when
	// the actual rows were created before M00. M00 must leave them intact.
	actual, err := UpgradeSnapshotV0(context.Background(), path, oracle.contract)
	if err != nil {
		t.Fatal(err)
	}
	if actual.TableCounts["blueprint_commit_operations"] != 1 {
		t.Fatal("M00 lost existing blueprint operation")
	}
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var payload string
	if err := db.QueryRow("SELECT payload_hash FROM blueprint_commit_operations WHERE operation_id='m00-old-op'").Scan(&payload); err != nil || payload != "hash" {
		t.Fatalf("old operation changed: %q, %v", payload, err)
	}
}

func TestM00PrivateUpgradeRejectsUnknownSourceAndTarget(t *testing.T) {
	oracle := loadM00Oracle(t)
	for _, tc := range []struct{ name, mutation, code string }{
		{"unexpected_index", "CREATE INDEX unexpected_m00_index ON contents(body)", CodeUnrecognizedSchema},
		{"wrong_version", "PRAGMA user_version=1", CodeUpgradeUnsupported},
		{"newer_version", "PRAGMA user_version=8", CodeNewerSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := copyUpgradeFixture(t, oracle.fixtures["baseline-v0.db"])
			mutateUpgradeFixture(t, path, tc.mutation)
			before := hashFile(t, path)
			_, err := UpgradeSnapshotV0(context.Background(), path, oracle.contract)
			requireCode(t, err, tc.code)
			if hashFile(t, path) != before {
				t.Fatal("rejected M00 changed copy")
			}
		})
	}
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v0.db"])
	before := hashFile(t, path)
	var invalid SchemaContract
	for _, entry := range oracle.contract {
		if entry.Version == 1 {
			invalid = append(invalid, VersionContract{Version: 1, Fingerprints: []string{strings.Repeat("a", 64)}})
		} else {
			invalid = append(invalid, entry)
		}
	}
	_, err := UpgradeSnapshotV0(context.Background(), path, invalid)
	requireCode(t, err, CodeUnrecognizedSchema)
	if hashFile(t, path) != before {
		t.Fatal("bad target fingerprint changed copy")
	}
	if err := os.WriteFile(path+"-wal", nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err = UpgradeSnapshotV0(context.Background(), path, oracle.contract)
	requireCode(t, err, CodeUnsafeSnapshot)
}

func TestM00PrivateUpgradeMissingAndCancelled(t *testing.T) {
	oracle := loadM00Oracle(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.db")
	_, err := UpgradeSnapshotV0(context.Background(), missing, oracle.contract)
	requireCode(t, err, CodeMissingSnapshot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = UpgradeSnapshotV0(ctx, missing, oracle.contract)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled M00 returned %v", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("M00 created missing copy")
	}
}
