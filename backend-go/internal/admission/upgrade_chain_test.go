package admission

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The original and Go full chains run at different wall-clock seconds. These
// are the two populated migration-owned metadata timestamps using datetime('now').
// Normalize them on disposable result copies, retaining a complete logical
// domain comparison of every other field and row. Both values must first have
// the exact SQLite UTC datetime shape; the original oracle files stay intact.
func normalizeFullChainClockRows(t *testing.T, path string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: path}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for _, row := range []struct {
		selectSQL string
		updateSQL string
	}{
		{"SELECT updated_at FROM character_roster_meta WHERE id='main'", "UPDATE character_roster_meta SET updated_at='2000-01-01 00:00:00' WHERE id='main'"},
		{"SELECT updated_at FROM text_metric_versions WHERE metric='draft-units'", "UPDATE text_metric_versions SET updated_at='2000-01-01 00:00:00' WHERE metric='draft-units'"},
	} {
		var original string
		if err := tx.QueryRow(row.selectSQL).Scan(&original); err != nil {
			t.Fatal(err)
		}
		parsed, err := time.Parse("2006-01-02 15:04:05", original)
		if err != nil || parsed.Format("2006-01-02 15:04:05") != original {
			t.Fatalf("migration metadata timestamp is not SQLite datetime: %q", original)
		}
		result, err := tx.Exec(row.updateSQL)
		if err != nil {
			t.Fatal(err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			t.Fatalf("expected one migration metadata row, changed=%d err=%v", changed, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeSnapshotChainMatchesOriginalCurrentAndPreservesSource(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	for _, family := range []string{"baseline", "donor"} {
		t.Run(family, func(t *testing.T) {
			source := oracle.fixtures[family+"-v0.db"]
			current := oracle.fixtures[family+"-v0-upgraded-v7.db"]
			if source == "" || current == "" {
				t.Fatal("original TypeScript chain fixtures missing")
			}
			sourceHash, currentHash := hashFile(t, source), hashFile(t, current)
			copyPath := copyUpgradeFixture(t, source)
			got, err := UpgradeSnapshotChain(context.Background(), copyPath, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			currentCopy := copyUpgradeFixture(t, current)
			want, err := VerifySnapshotWithDomain(context.Background(), currentCopy, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			if got.UserVersion != CurrentSchemaVersion || got.SchemaFingerprint != want.SchemaFingerprint ||
				!reflect.DeepEqual(got.TableCounts, want.TableCounts) {
				t.Fatalf("Go chain differs from original current schema: got=%+v want=%+v", got, want)
			}
			if got.ContentIdentity.Database != hashFile(t, copyPath) {
				t.Fatal("chain returned stale physical identity")
			}
			if hashFile(t, source) != sourceHash || hashFile(t, current) != currentHash {
				t.Fatal("chain changed original TypeScript oracle fixture")
			}
			beforeRetry := hashFile(t, copyPath)
			reopened, err := UpgradeSnapshotChain(context.Background(), copyPath, oracle.contract)
			if err != nil || !reflect.DeepEqual(reopened, got) || hashFile(t, copyPath) != beforeRetry {
				t.Fatalf("current private copy did not verify without mutation: %v", err)
			}
			normalizeFullChainClockRows(t, copyPath)
			normalizeFullChainClockRows(t, currentCopy)
			normalizedGo, err := VerifySnapshotWithDomain(context.Background(), copyPath, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			normalizedOriginal, err := VerifySnapshotWithDomain(context.Background(), currentCopy, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(normalizedGo.Domain, normalizedOriginal.Domain) {
				t.Fatalf("Go full-chain logical rows differ from original: got=%+v want=%+v", normalizedGo.Domain, normalizedOriginal.Domain)
			}
			if hashFile(t, source) != sourceHash || hashFile(t, current) != currentHash {
				t.Fatal("normalization changed original TypeScript oracle fixture")
			}
		})
	}
}

func TestUpgradeSnapshotChainRejectsPopulatedM04WithoutTouchingSource(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	source := oracle.fixtures["baseline-v4.db"]
	if source == "" {
		t.Fatal("original TypeScript v4 fixture missing")
	}
	sourceHash := hashFile(t, source)
	copyPath := copyUpgradeFixture(t, source)
	mutateUpgradeFixture(t, copyPath, earlyTestImportRunSQL)
	copyHash := hashFile(t, copyPath)
	_, err := UpgradeSnapshotChain(context.Background(), copyPath, oracle.contract)
	requireCode(t, err, CodeUpgradeUnsupported)
	if hashFile(t, source) != sourceHash || hashFile(t, copyPath) != copyHash {
		t.Fatal("rejected populated history changed source or private copy")
	}
}

func TestUpgradeSnapshotChainFailureAfterEarlierStepsKeepsOriginal(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	source := oracle.fixtures["baseline-v0.db"]
	if source == "" {
		t.Fatal("original TypeScript v0 fixture missing")
	}
	sourceHash := hashFile(t, source)
	copyPath := copyUpgradeFixture(t, source)
	contract := make(SchemaContract, len(oracle.contract))
	for i, entry := range oracle.contract {
		contract[i] = entry
		if entry.Version == 4 {
			contract[i].Fingerprints = []string{strings.Repeat("a", 64)}
		}
	}
	_, err := UpgradeSnapshotChain(context.Background(), copyPath, contract)
	requireCode(t, err, CodeUnrecognizedSchema)
	if hashFile(t, source) != sourceHash {
		t.Fatal("failed private chain changed original source")
	}
	if info, err := os.Stat(copyPath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("private failure copy not available for caller disposal: %v", err)
	}
}
