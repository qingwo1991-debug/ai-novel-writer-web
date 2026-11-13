package admission

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type upgradeOracle struct {
	contract SchemaContract
	fixtures map[string]string
}

// Mandatory original-TypeScript oracle, never a schema inferred from the DB
// under test. CI must generate both donor lanes and Node-M06 upgraded copies.
func loadUpgradeOracle(t *testing.T) upgradeOracle {
	t.Helper()
	indexPath := os.Getenv("GO_SQLITE_CONTRACT")
	if indexPath == "" {
		t.Fatal("GO_SQLITE_CONTRACT is required: run the original TypeScript oracle in GitHub Actions")
	}
	encoded, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	contract, parseErr := ParseContract(file)
	closeErr := file.Close()
	if err := errors.Join(parseErr, closeErr); err != nil {
		t.Fatal(err)
	}
	var index struct {
		Fixtures []struct {
			File string `json:"file"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(encoded, &index); err != nil {
		t.Fatal(err)
	}
	oracle := upgradeOracle{contract: contract, fixtures: make(map[string]string)}
	for _, fixture := range index.Fixtures {
		path := fixture.File
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(indexPath), path)
		}
		oracle.fixtures[filepath.Base(path)] = path
	}
	for _, name := range []string{"baseline-v6.db", "donor-v6.db", "baseline-v6-upgraded-v7.db", "donor-v6-upgraded-v7.db"} {
		if oracle.fixtures[name] == "" {
			t.Fatalf("original TypeScript oracle is missing mandatory upgrade fixture %s", name)
		}
	}
	return oracle
}

func copyUpgradeFixture(t *testing.T, source string) string {
	t.Helper()
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "private ?#快照.db")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func mutateUpgradeFixture(t *testing.T, path string, statements ...string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertUpgradeV6Unchanged(t *testing.T, path, before string) {
	t.Helper()
	if hashFile(t, path) != before {
		t.Fatal("rejected/rolled-back upgrade changed private DB bytes")
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=ro&_query_only=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, merges int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='review_cycle_merges'").Scan(&merges); err != nil {
		t.Fatal(err)
	}
	if version != 6 || merges != 0 {
		t.Fatalf("partial M06 escaped rollback: version=%d merges=%d", version, merges)
	}
}

func TestUpgradeV6MatchesOriginalNodeM06BothDonors(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	for _, lane := range []string{"baseline", "donor"} {
		t.Run(lane, func(t *testing.T) {
			source := oracle.fixtures[lane+"-v6.db"]
			expectedPath := oracle.fixtures[lane+"-v6-upgraded-v7.db"]
			sourceHash, expectedHash := hashFile(t, source), hashFile(t, expectedPath)
			path := copyUpgradeFixture(t, source)
			expected, err := VerifySnapshotWithDomain(context.Background(), expectedPath, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := UpgradeSnapshotV6(context.Background(), path, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			if actual.UserVersion != 7 || actual.SchemaFingerprint != expected.SchemaFingerprint ||
				!reflect.DeepEqual(actual.TableCounts, expected.TableCounts) || actual.Domain == nil ||
				!reflect.DeepEqual(actual.Domain, expected.Domain) {
				t.Fatalf("Go M06 differs from original Node M06: actual=%+v domain=%+v expected=%+v domain=%+v", actual, actual.Domain, expected, expected.Domain)
			}
			if actual.TableCounts["review_cycle_merges"] != 0 {
				t.Fatal("empty-cycle migration invented historical merge snapshots")
			}
			if actual.ContentIdentity.Database != hashFile(t, path) {
				t.Fatal("returned stale physical evidence")
			}
			verified, err := VerifySnapshotWithDomain(context.Background(), path, oracle.contract)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, verified) {
				t.Fatal("final exact-v7 verification differs from returned evidence")
			}
			if hashFile(t, source) != sourceHash || hashFile(t, expectedPath) != expectedHash {
				t.Fatal("upgrade touched original oracle fixtures")
			}
			currentHash := hashFile(t, path)
			_, err = UpgradeSnapshotV6(context.Background(), path, oracle.contract)
			requireCode(t, err, CodeUpgradeUnsupported)
			if hashFile(t, path) != currentHash {
				t.Fatal("second upgrade mutated current v7 DB")
			}
		})
	}
}

func TestUpgradeV6FinalFingerprintFailureRollsBackDDLAndVersion(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v6.db"])
	before := hashFile(t, path)
	var contract SchemaContract
	for _, entry := range oracle.contract {
		if entry.Version == 7 {
			contract = append(contract, VersionContract{Version: 7, Fingerprints: []string{strings.Repeat("a", 64)}})
		} else {
			contract = append(contract, entry)
		}
	}
	_, err := UpgradeSnapshotV6(context.Background(), path, contract)
	requireCode(t, err, CodeUnrecognizedSchema)
	assertUpgradeV6Unchanged(t, path, before)
}

func TestUpgradeV6RejectsSourceFingerprintAndDomainBeforeMutation(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	for _, test := range []struct{ name, mutation, code string }{
		{"unrecognized_index", "CREATE INDEX unexpected_upgrade_index ON contents(body)", CodeUnrecognizedSchema},
		{"invalid_identity", "DELETE FROM character_identity_meta", CodeDomain},
		{"foreign_key", "PRAGMA foreign_keys=OFF; INSERT INTO review_findings(cycle_id,finding_id,source_project_id,source_epoch,source_id,source_revision,source_content_hash,category,kind,problem_text,status) VALUES('missing','finding','project','epoch','draft:1',1,'hash','test','objective','problem','unverified')", CodeForeignKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := copyUpgradeFixture(t, oracle.fixtures["baseline-v6.db"])
			mutateUpgradeFixture(t, path, test.mutation)
			before := hashFile(t, path)
			_, err := UpgradeSnapshotV6(context.Background(), path, oracle.contract)
			requireCode(t, err, test.code)
			assertUpgradeV6Unchanged(t, path, before)
		})
	}
}

func TestUpgradeV6RejectsPopulatedCyclesWithoutPretendingM03Proof(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v6.db"])
	// Structurally valid and FK-clean, deliberately NOT a valid complete M03
	// generation proof. The supported lane must refuse even to attempt that proof.
	mutateUpgradeFixture(t, path,
		"INSERT INTO contents(id,body) VALUES(900001,'synthetic review-cycle fixture')",
		"INSERT INTO drafts(id,chapter_number,version,content_id) VALUES(900001,900001,1,900001)",
		"INSERT INTO reviews(id,base_draft_id,review_index,content_id) VALUES(900001,900001,1,900001)",
		"INSERT INTO generation_roots(root_action_id,idempotency_key,action_json,budget_json) VALUES('upgrade-test-root','upgrade-test-key','{}','{}')",
		"INSERT INTO review_cycles(cycle_id,root_action_id,review_id,review_content_hash,source_hash,finding_set_hash,comparison_version,revision_status,recheck_count) VALUES('unsupported-cycle','upgrade-test-root',900001,'hash','hash','hash',1,'not-generated',0)")
	before := hashFile(t, path)
	_, err := UpgradeSnapshotV6(context.Background(), path, oracle.contract)
	requireCode(t, err, CodeUpgradeUnsupported)
	assertUpgradeV6Unchanged(t, path, before)
}

func TestUpgradeV6RejectsUnsafeCopiesBeforeMutation(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	for _, kind := range []string{"file_permissions", "directory_permissions", "wal", "shm", "journal", "symlink", "hardlink", "wal_header"} {
		t.Run(kind, func(t *testing.T) {
			path := copyUpgradeFixture(t, oracle.fixtures["baseline-v6.db"])
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
			_, err := UpgradeSnapshotV6(context.Background(), target, oracle.contract)
			requireCode(t, err, code)
			if hashFile(t, path) != before {
				t.Fatal("unsafe snapshot was mutated")
			}
		})
	}
}

func TestUpgradeV6MissingCanceledAndInvalidContractNeverCreateDB(t *testing.T) {
	oracle := loadUpgradeOracle(t)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(directory, "missing.db")
	_, err := UpgradeSnapshotV6(context.Background(), missing, oracle.contract)
	requireCode(t, err, CodeMissingSnapshot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = UpgradeSnapshotV6(ctx, missing, oracle.contract)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation; got %v", err)
	}
	_, err = UpgradeSnapshotV6(context.Background(), missing, nil)
	requireCode(t, err, CodeInvalidContract)
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed upgrade created missing DB")
	}
}
