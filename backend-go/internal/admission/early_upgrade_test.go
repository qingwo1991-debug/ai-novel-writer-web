package admission

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type earlyUpgradeTestLane struct {
	from, to int
	upgrade  func(context.Context, string, SchemaContract) (Evidence, error)
	added    []string
}

func earlyUpgradeTestLanes() []earlyUpgradeTestLane {
	return []earlyUpgradeTestLane{
		{from: 4, to: 5, upgrade: UpgradeSnapshotV4, added: []string{"import_effect_ledger"}},
		{from: 5, to: 6, upgrade: UpgradeSnapshotV5, added: []string{"character_avatar_assets", "character_avatar_unresolved"}},
	}
}

// Missing original-TypeScript fixtures are a hard failure, never a skip or a
// locally self-blessed contract. Generation and all Go checks run ONLY in GHA.
func loadEarlyUpgradeOracle(t *testing.T) upgradeOracle {
	t.Helper()
	oracle := loadUpgradeOracle(t)
	for _, lane := range earlyUpgradeTestLanes() {
		for _, family := range []string{"baseline", "donor"} {
			for _, name := range []string{
				fmt.Sprintf("%s-v%d.db", family, lane.from),
				fmt.Sprintf("%s-v%d-upgraded-v%d.db", family, lane.from, lane.to),
			} {
				if oracle.fixtures[name] == "" {
					t.Fatalf("original TypeScript oracle is missing mandatory early upgrade fixture %s", name)
				}
			}
		}
	}
	return oracle
}

func TestEarlyUpgradeMatchesOriginalTypeScriptM04M05BothDonors(t *testing.T) {
	oracle := loadEarlyUpgradeOracle(t)
	for _, lane := range earlyUpgradeTestLanes() {
		for _, family := range []string{"baseline", "donor"} {
			t.Run(fmt.Sprintf("v%d_%s", lane.from, family), func(t *testing.T) {
				source := oracle.fixtures[fmt.Sprintf("%s-v%d.db", family, lane.from)]
				expectedSource := oracle.fixtures[fmt.Sprintf("%s-v%d-upgraded-v%d.db", family, lane.from, lane.to)]
				sourceHash, expectedHash := hashFile(t, source), hashFile(t, expectedSource)
				path := copyUpgradeFixture(t, source)
				// Even the oracle DB is read only via an isolated private copy; never
				// chmod, checkpoint or otherwise alter shared synthetic fixtures.
				expectedPath := copyUpgradeFixture(t, expectedSource)
				expected, err := verifyEarlySnapshotStage(context.Background(), expectedPath, oracle.contract, lane.to)
				if err != nil {
					t.Fatal(err)
				}
				actual, err := lane.upgrade(context.Background(), path, oracle.contract)
				if err != nil {
					t.Fatal(err)
				}
				if actual.UserVersion != lane.to || actual.SchemaFingerprint != expected.SchemaFingerprint ||
					!reflect.DeepEqual(actual.TableCounts, expected.TableCounts) || actual.Domain == nil ||
					!reflect.DeepEqual(actual.Domain, expected.Domain) {
					t.Fatalf("Go early step differs from original TypeScript: actual=%+v domain=%+v expected=%+v domain=%+v", actual, actual.Domain, expected, expected.Domain)
				}
				for _, table := range lane.added {
					count, present := actual.TableCounts[table]
					if !present || count != 0 {
						t.Fatalf("expected added empty table %s, counts=%v", table, actual.TableCounts)
					}
				}
				if actual.ContentIdentity.Database != hashFile(t, path) {
					t.Fatal("returned stale physical evidence")
				}
				verified, err := verifyEarlySnapshotStage(context.Background(), path, oracle.contract, lane.to)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(actual, verified) {
					t.Fatal("returned intermediate evidence differs from fresh read-only verification")
				}
				// Intermediate evidence must NOT quietly admit v5/v6 as current v7.
				_, err = VerifySnapshotWithDomain(context.Background(), path, oracle.contract)
				requireCode(t, err, CodeMigrationRequired)
				beforeRetry := hashFile(t, path)
				_, err = lane.upgrade(context.Background(), path, oracle.contract)
				requireCode(t, err, CodeUpgradeUnsupported)
				if hashFile(t, path) != beforeRetry {
					t.Fatal("repeated step mutated intermediate copy")
				}
				if hashFile(t, source) != sourceHash || hashFile(t, expectedSource) != expectedHash {
					t.Fatal("upgrade mutated original TypeScript oracle fixtures")
				}
			})
		}
	}
}

func assertEarlyUpgradeUnchanged(t *testing.T, path, before string, lane earlyUpgradeTestLane) {
	t.Helper()
	if hashFile(t, path) != before {
		t.Fatal("rejected/rolled-back early upgrade changed private DB bytes")
	}
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != lane.from {
		t.Fatalf("partial early version update escaped rollback: got %d want %d", version, lane.from)
	}
	for _, table := range lane.added {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name=?", table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("partial early DDL escaped rollback: %s", table)
		}
	}
}

func TestEarlyUpgradeTargetFingerprintFailureRollsBackDDLAndVersion(t *testing.T) {
	oracle := loadEarlyUpgradeOracle(t)
	for _, lane := range earlyUpgradeTestLanes() {
		t.Run(fmt.Sprintf("v%d", lane.from), func(t *testing.T) {
			path := copyUpgradeFixture(t, oracle.fixtures[fmt.Sprintf("baseline-v%d.db", lane.from)])
			before := hashFile(t, path)
			var contract SchemaContract
			for _, entry := range oracle.contract {
				if entry.Version == lane.to {
					contract = append(contract, VersionContract{Version: lane.to, Fingerprints: []string{strings.Repeat("a", 64)}})
				} else {
					contract = append(contract, entry)
				}
			}
			_, err := lane.upgrade(context.Background(), path, contract)
			requireCode(t, err, CodeUnrecognizedSchema)
			assertEarlyUpgradeUnchanged(t, path, before, lane)
		})
	}
}

func TestEarlyUpgradeRejectsUnknownSchemaIdentityAndFKBeforeMutation(t *testing.T) {
	oracle := loadEarlyUpgradeOracle(t)
	for _, lane := range earlyUpgradeTestLanes() {
		for _, test := range []struct{ name, mutation, code string }{
			{"unexpected_index", "CREATE INDEX unexpected_early_index ON contents(body)", CodeUnrecognizedSchema},
			{"invalid_identity", "DELETE FROM character_identity_meta", CodeDomain},
			{"broken_fk", "PRAGMA foreign_keys=OFF; INSERT INTO import_run_chapters(run_id,chapter_number,content_fingerprint,content_size,content_snapshot) VALUES('missing',1,'hash',1,'x')", CodeForeignKey},
		} {
			t.Run(fmt.Sprintf("v%d_%s", lane.from, test.name), func(t *testing.T) {
				path := copyUpgradeFixture(t, oracle.fixtures[fmt.Sprintf("baseline-v%d.db", lane.from)])
				mutateUpgradeFixture(t, path, test.mutation)
				before := hashFile(t, path)
				_, err := lane.upgrade(context.Background(), path, oracle.contract)
				requireCode(t, err, test.code)
				assertEarlyUpgradeUnchanged(t, path, before, lane)
			})
		}
	}
}

const earlyTestImportRunSQL = `INSERT INTO import_runs(
 id,purpose,root_run_id,effect_namespace,source_fingerprint,manifest_fingerprint,locale,
 stage,status,total_chapters,manifest_chapter_count
) VALUES('early-run','reference','early-run','import:reference:early-run','source','manifest','zh-CN','knowledge','ready',1,1)`

func TestEarlyUpgradeRefusesPopulatedImportHistoryRatherThanFakeProof(t *testing.T) {
	oracle := loadEarlyUpgradeOracle(t)
	for _, lane := range earlyUpgradeTestLanes() {
		for _, kind := range []string{"run", "receipt", "legacy_checkpoint", "ledger"} {
			if kind == "ledger" && lane.from < 5 {
				continue
			}
			t.Run(fmt.Sprintf("v%d_%s", lane.from, kind), func(t *testing.T) {
				path := copyUpgradeFixture(t, oracle.fixtures[fmt.Sprintf("baseline-v%d.db", lane.from)])
				mutateUpgradeFixture(t, path, earlyTestImportRunSQL)
				switch kind {
				case "receipt":
					mutateUpgradeFixture(t, path, `INSERT INTO import_run_receipts(
 run_id,effect_namespace,effect_key,stage,batch_id,kind,payload_json,payload_hash,state
) VALUES('early-run','import:reference:early-run','key','global','batch','project-global-facts','{}','hash','prepared')`)
				case "legacy_checkpoint":
					mutateUpgradeFixture(t, path, `UPDATE import_runs SET completed_batches_json='{"knowledge":["unproven-checkpoint"]}' WHERE id='early-run'`)
				case "ledger":
					mutateUpgradeFixture(t, path, `INSERT INTO import_effect_ledger(
 run_id,stage,batch_id,effect_namespace,effect_key,state,evidence_kind,diagnostic_code
) VALUES('early-run','knowledge','legacy','import:reference:early-run','legacy','unknown','legacy-checkpoint','UNPROVEN')`)
				}
				before := hashFile(t, path)
				_, err := lane.upgrade(context.Background(), path, oracle.contract)
				requireCode(t, err, CodeUpgradeUnsupported)
				assertEarlyUpgradeUnchanged(t, path, before, lane)
			})
		}
	}
}

func TestEarlyUpgradeRefusesPopulatedM03CyclesBeforeMutation(t *testing.T) {
	oracle := loadEarlyUpgradeOracle(t)
	for _, lane := range earlyUpgradeTestLanes() {
		t.Run(fmt.Sprintf("v%d", lane.from), func(t *testing.T) {
			path := copyUpgradeFixture(t, oracle.fixtures[fmt.Sprintf("baseline-v%d.db", lane.from)])
			// FK-clean but intentionally NOT a valid complete M03 generation proof.
			mutateUpgradeFixture(t, path,
				"INSERT INTO contents(id,body) VALUES(900002,'synthetic early review')",
				"INSERT INTO drafts(id,chapter_number,version,content_id) VALUES(900002,900002,1,900002)",
				"INSERT INTO reviews(id,base_draft_id,review_index,content_id) VALUES(900002,900002,1,900002)",
				"INSERT INTO generation_roots(root_action_id,idempotency_key,action_json,budget_json) VALUES('early-root','early-key','{}','{}')",
				"INSERT INTO review_cycles(cycle_id,root_action_id,review_id,review_content_hash,source_hash,finding_set_hash,comparison_version,revision_status,recheck_count) VALUES('early-cycle','early-root',900002,'hash','hash','hash',1,'not-generated',0)")
			before := hashFile(t, path)
			_, err := lane.upgrade(context.Background(), path, oracle.contract)
			requireCode(t, err, CodeUpgradeUnsupported)
			assertEarlyUpgradeUnchanged(t, path, before, lane)
		})
	}
}

func TestEarlyUpgradePrivatePathGuardsAndWALRefusal(t *testing.T) {
	oracle := loadEarlyUpgradeOracle(t)
	for _, lane := range earlyUpgradeTestLanes() {
		for _, kind := range []string{"file_permissions", "directory_permissions", "wal", "shm", "journal", "symlink", "hardlink", "wal_header"} {
			t.Run(fmt.Sprintf("v%d_%s", lane.from, kind), func(t *testing.T) {
				path := copyUpgradeFixture(t, oracle.fixtures[fmt.Sprintf("baseline-v%d.db", lane.from)])
				target := path
				code := CodeUnsafeSnapshot
				switch kind {
				case "file_permissions":
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				case "directory_permissions":
					if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
						t.Fatal(err)
					}
				case "wal", "shm", "journal":
					if err := os.WriteFile(path+"-"+kind, nil, 0600); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					target = filepath.Join(filepath.Dir(path), "alias.db")
					if err := os.Symlink(path, target); err != nil {
						t.Fatal(err)
					}
				case "hardlink":
					target = filepath.Join(filepath.Dir(path), "alias.db")
					if err := os.Link(path, target); err != nil {
						t.Fatal(err)
					}
				case "wal_header":
					mutateUpgradeFixture(t, path, "PRAGMA journal_mode=WAL")
					code = CodeUpgradeUnsupported
				}
				before := hashFile(t, path)
				_, err := lane.upgrade(context.Background(), target, oracle.contract)
				requireCode(t, err, code)
				if hashFile(t, path) != before {
					t.Fatal("unsafe private snapshot was mutated")
				}
			})
		}
	}
}

func TestEarlyUpgradeMissingCanceledInvalidContractAndWrongVersions(t *testing.T) {
	oracle := loadEarlyUpgradeOracle(t)
	for _, lane := range earlyUpgradeTestLanes() {
		t.Run(fmt.Sprintf("v%d", lane.from), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			missing := filepath.Join(dir, "missing.db")
			_, err := lane.upgrade(context.Background(), missing, oracle.contract)
			requireCode(t, err, CodeMissingSnapshot)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, err = lane.upgrade(ctx, missing, oracle.contract)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("expected cancellation, got %v", err)
			}
			_, err = lane.upgrade(context.Background(), missing, nil)
			requireCode(t, err, CodeInvalidContract)
			if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed early upgrade created missing DB")
			}
			for _, version := range []int{0, 3, 7, 8} {
				path := copyUpgradeFixture(t, oracle.fixtures[fmt.Sprintf("baseline-v%d.db", lane.from)])
				mutateUpgradeFixture(t, path, fmt.Sprintf("PRAGMA user_version=%d", version))
				before := hashFile(t, path)
				_, err := lane.upgrade(context.Background(), path, oracle.contract)
				code := CodeUpgradeUnsupported
				if version > CurrentSchemaVersion {
					code = CodeNewerSchema
				}
				requireCode(t, err, code)
				if hashFile(t, path) != before {
					t.Fatal("wrong-version early snapshot was mutated")
				}
			}
		})
	}
}
