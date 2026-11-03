package admission

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestUpgradeSnapshotChainMatchesOriginalCurrentAndPreservesSource(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	for _, family := range []string{"baseline", "donor"} {
		t.Run(family, func(t *testing.T) {
			source := oracle.fixtures[family+"-v0.db"]
			current := oracle.fixtures[family+"-v6-upgraded-v7.db"]
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
				!reflect.DeepEqual(got.TableCounts, want.TableCounts) || !reflect.DeepEqual(got.Domain, want.Domain) {
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
