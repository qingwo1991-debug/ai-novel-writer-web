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

func loadM01UpgradeOracle(t *testing.T) upgradeOracle {
	t.Helper()
	oracle := loadUpgradeOracle(t)
	for _, name := range []string{
		"baseline-v1.db", "donor-v1.db", "baseline-v1-upgraded-v2.db", "donor-v1-upgraded-v2.db",
	} {
		if oracle.fixtures[name] == "" {
			t.Fatalf("original TypeScript oracle missing M01 fixture %s", name)
		}
	}
	return oracle
}

func TestM01PrivateUpgradeMatchesOriginalTypeScript(t *testing.T) {
	oracle := loadM01UpgradeOracle(t)
	for _, tc := range []struct{ name, source, upgraded string }{
		{"baseline", "baseline-v1.db", "baseline-v1-upgraded-v2.db"},
		{"donor", "donor-v1.db", "donor-v1-upgraded-v2.db"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := oracle.fixtures[tc.source]
			expectedSource := oracle.fixtures[tc.upgraded]
			sourceHash, expectedHash := hashFile(t, source), hashFile(t, expectedSource)
			path := copyUpgradeFixture(t, source)
			expectedPath := copyUpgradeFixture(t, expectedSource)
			expected, err := verifyM01SnapshotV2(context.Background(), expectedPath, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := UpgradeSnapshotV1(context.Background(), path, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			if actual.UserVersion != 2 || actual.SchemaFingerprint != expected.SchemaFingerprint ||
				!reflect.DeepEqual(actual.TableCounts, expected.TableCounts) || actual.Domain == nil ||
				!reflect.DeepEqual(actual.Domain, expected.Domain) {
				t.Fatalf("Go M01 differs from original TypeScript: actual=%+v expected=%+v", actual, expected)
			}
			for _, table := range m01GenerationTables {
				if actual.TableCounts[table] != 0 {
					t.Fatalf("M01 invented %s historical rows", table)
				}
			}
			if actual.TableCounts["contents"] != 1 {
				t.Fatal("M01 lost existing author content")
			}
			if actual.ContentIdentity.Database != hashFile(t, path) {
				t.Fatal("M01 returned stale physical evidence")
			}
			verified, err := verifyM01SnapshotV2(context.Background(), path, oracle.contract)
			if err != nil || !reflect.DeepEqual(actual, verified) {
				t.Fatalf("fresh M01 readback differs: %v", err)
			}
			if hashFile(t, source) != sourceHash || hashFile(t, expectedSource) != expectedHash {
				t.Fatal("M01 touched original TypeScript fixture")
			}
			_, err = VerifySnapshotWithDomain(context.Background(), path, oracle.contract)
			requireCode(t, err, CodeMigrationRequired)
			beforeRetry := hashFile(t, path)
			_, err = UpgradeSnapshotV1(context.Background(), path, oracle.contract)
			requireCode(t, err, CodeUpgradeUnsupported)
			if hashFile(t, path) != beforeRetry {
				t.Fatal("repeat M01 changed v2 copy")
			}
		})
	}
}

func TestM01PrivateUpgradeRejectsUnknownAndUntrustedSchemas(t *testing.T) {
	oracle := loadM01UpgradeOracle(t)
	for _, tc := range []struct{ name, mutation, code string }{
		{"unexpected_index", "CREATE INDEX unexpected_m01_index ON contents(body)", CodeUnrecognizedSchema},
		{"wrong_version", "PRAGMA user_version=2", CodeUpgradeUnsupported},
		{"newer_version", "PRAGMA user_version=8", CodeNewerSchema},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := copyUpgradeFixture(t, oracle.fixtures["baseline-v1.db"])
			mutateUpgradeFixture(t, path, tc.mutation)
			before := hashFile(t, path)
			_, err := UpgradeSnapshotV1(context.Background(), path, oracle.contract)
			requireCode(t, err, tc.code)
			if hashFile(t, path) != before {
				t.Fatal("rejected M01 changed private copy")
			}
		})
	}
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v1.db"])
	before := hashFile(t, path)
	var wrongTarget SchemaContract
	for _, entry := range oracle.contract {
		if entry.Version == 2 {
			wrongTarget = append(wrongTarget, VersionContract{Version: 2, Fingerprints: []string{strings.Repeat("a", 64)}})
		} else {
			wrongTarget = append(wrongTarget, entry)
		}
	}
	_, err := UpgradeSnapshotV1(context.Background(), path, wrongTarget)
	requireCode(t, err, CodeUnrecognizedSchema)
	if hashFile(t, path) != before {
		t.Fatal("untrusted target changed private copy")
	}
}

func TestM01PrivateUpgradeRejectsUnsafeCopyMissingAndCanceled(t *testing.T) {
	oracle := loadM01UpgradeOracle(t)
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v1.db"])
	if err := os.WriteFile(path+"-wal", nil, 0600); err != nil {
		t.Fatal(err)
	}
	before := hashFile(t, path)
	_, err := UpgradeSnapshotV1(context.Background(), path, oracle.contract)
	requireCode(t, err, CodeUnsafeSnapshot)
	if hashFile(t, path) != before {
		t.Fatal("M01 touched rejected WAL copy")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.db")
	_, err = UpgradeSnapshotV1(context.Background(), missing, oracle.contract)
	requireCode(t, err, CodeMissingSnapshot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = UpgradeSnapshotV1(ctx, missing, oracle.contract)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled M01 returned %v", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("M01 created missing copy")
	}
}
