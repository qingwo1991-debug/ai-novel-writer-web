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

func loadM02UpgradeOracle(t *testing.T) upgradeOracle {
	t.Helper()
	oracle := loadUpgradeOracle(t)
	for _, name := range []string{
		"baseline-v2.db", "donor-v2.db", "baseline-v2-upgraded-v3.db", "donor-v2-upgraded-v3.db",
		"baseline-v2-history.db", "donor-v2-history.db", "baseline-v2-history-upgraded-v3.db", "donor-v2-history-upgraded-v3.db",
	} {
		if oracle.fixtures[name] == "" {
			t.Fatalf("original TypeScript M02 oracle missing %s", name)
		}
	}
	return oracle
}

func m02Summary(t *testing.T, path string) ([]string, []string, []string, int64) {
	t.Helper()
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SELECT original_row_json FROM character_identity_origins ORDER BY source_key")
	if err != nil {
		t.Fatal(err)
	}
	var originals []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		originals = append(originals, value)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	rows, err = db.Query("SELECT source_key || ':' || raw_value || ':' || source_hash FROM character_identity_proposals ORDER BY source_key")
	if err != nil {
		t.Fatal(err)
	}
	var proposals []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		proposals = append(proposals, value)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	rows, err = db.Query("SELECT p.source_key || ':' || c.name FROM character_identity_proposals p JOIN json_each(p.candidate_ids_json) j JOIN characters c ON c.character_id=j.value ORDER BY p.source_key,j.key")
	if err != nil {
		t.Fatal(err)
	}
	var candidates []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		candidates = append(candidates, value)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	var aliases int64
	if err := db.QueryRow("SELECT COUNT(*) FROM character_aliases").Scan(&aliases); err != nil {
		t.Fatal(err)
	}
	return originals, proposals, candidates, aliases
}

func assertM02PlainRelationshipsUnbound(t *testing.T, path string) {
	t.Helper()
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var proposals, candidates int64
	if err := db.QueryRow("SELECT COUNT(*) FROM character_identity_proposals WHERE source_key LIKE 'legacy:characters:%:relationships:%'").Scan(&proposals); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM character_identity_proposals p JOIN json_each(p.candidate_ids_json) j WHERE p.source_key LIKE 'legacy:characters:%:relationships:%'").Scan(&candidates); err != nil {
		t.Fatal(err)
	}
	if proposals != 2 || candidates != 0 {
		t.Fatalf("plain legacy relationships were bound: proposals=%d candidates=%d", proposals, candidates)
	}
}

func TestM02PrivateUpgradeMatchesOriginalTypeScript(t *testing.T) {
	oracle := loadM02UpgradeOracle(t)
	for _, family := range []string{"baseline", "donor"} {
		for _, suffix := range []string{"", "-history"} {
			t.Run(family+suffix, func(t *testing.T) {
				source := oracle.fixtures[family+"-v2"+suffix+".db"]
				wantPath := oracle.fixtures[family+"-v2"+suffix+"-upgraded-v3.db"]
				sourceHash, wantHash := hashFile(t, source), hashFile(t, wantPath)
				path := copyUpgradeFixture(t, source)
				wantCopy := copyUpgradeFixture(t, wantPath)
				want, err := verifyM02SnapshotV3(context.Background(), wantCopy, oracle.contract)
				if err != nil {
					t.Fatal(err)
				}
				got, err := UpgradeSnapshotV2(context.Background(), path, oracle.contract)
				if err != nil {
					t.Fatal(err)
				}
				if got.UserVersion != 3 || got.SchemaFingerprint != want.SchemaFingerprint || !reflect.DeepEqual(got.TableCounts, want.TableCounts) {
					t.Fatalf("M02 schema/count parity mismatch: got=%+v want=%+v", got, want)
				}
				gotOriginals, gotProposals, gotCandidates, gotAliases := m02Summary(t, path)
				wantOriginals, wantProposals, wantCandidates, wantAliases := m02Summary(t, wantPath)
				if !reflect.DeepEqual(gotOriginals, wantOriginals) || !reflect.DeepEqual(gotProposals, wantProposals) || !reflect.DeepEqual(gotCandidates, wantCandidates) || gotAliases != wantAliases {
					t.Fatalf("M02 identity semantics differ: originals=%v/%v proposals=%v/%v candidates=%v/%v aliases=%d/%d", gotOriginals, wantOriginals, gotProposals, wantProposals, gotCandidates, wantCandidates, gotAliases, wantAliases)
				}
				if suffix == "-history" {
					assertM02PlainRelationshipsUnbound(t, path)
					assertM02PlainRelationshipsUnbound(t, wantPath)
				}
				if got.ContentIdentity.Database != hashFile(t, path) {
					t.Fatal("stale M02 evidence")
				}
				fresh, err := verifyM02SnapshotV3(context.Background(), path, oracle.contract)
				if err != nil || !reflect.DeepEqual(got, fresh) {
					t.Fatalf("fresh M02 evidence differs: %v", err)
				}
				_, err = VerifySnapshotWithDomain(context.Background(), path, oracle.contract)
				requireCode(t, err, CodeMigrationRequired)
				beforeRetry := hashFile(t, path)
				_, err = UpgradeSnapshotV2(context.Background(), path, oracle.contract)
				requireCode(t, err, CodeUpgradeUnsupported)
				if hashFile(t, path) != beforeRetry || hashFile(t, source) != sourceHash || hashFile(t, wantPath) != wantHash {
					t.Fatal("M02 mutated oracle or repeated copy")
				}
			})
		}
	}
}

func assertM02RejectedUnchanged(t *testing.T, path, before string) {
	t.Helper()
	if hashFile(t, path) != before {
		t.Fatal("rejected M02 changed private database bytes")
	}
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, origins int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='character_identity_origins'").Scan(&origins); err != nil {
		t.Fatal(err)
	}
	if version != 2 || origins != 0 {
		t.Fatalf("partial M02 escaped rollback: version=%d origins=%d", version, origins)
	}
}

func TestM02RejectsUnknownSourceAmbiguousLegacyAndUntrustedTarget(t *testing.T) {
	oracle := loadM02UpgradeOracle(t)
	for _, tc := range []struct{ name, mutation, code string }{
		{"unknown_index", "CREATE INDEX unknown_m02_index ON contents(body)", CodeUnrecognizedSchema},
		{"broken_fk", "PRAGMA foreign_keys=OFF; INSERT INTO drafts(chapter_number,version,content_id) VALUES(999,1,999999)", CodeForeignKey},
		{"newer_version", "PRAGMA user_version=8", CodeNewerSchema},
		{"already_v3", "PRAGMA user_version=3", CodeUpgradeUnsupported},
		{"complex_relationship", "UPDATE characters SET relationships='[{\"target\":\"旧角色\"}]' WHERE name='旧角色'", CodeUpgradeUnsupported},
		{"complex_blueprint", "UPDATE blueprints SET characters='[1]' WHERE chapter_number=900002", CodeUpgradeUnsupported},
		{"lone_surrogate_relationship", "UPDATE characters SET relationships='[\"\\ud800\"]' WHERE name='旧角色'", CodeUpgradeUnsupported},
		{"lone_surrogate_blueprint", "UPDATE blueprints SET characters='[\"\\ud800\"]' WHERE chapter_number=900002", CodeUpgradeUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := copyUpgradeFixture(t, oracle.fixtures["baseline-v2-history.db"])
			mutateUpgradeFixture(t, path, tc.mutation)
			before := hashFile(t, path)
			_, err := UpgradeSnapshotV2(context.Background(), path, oracle.contract)
			requireCode(t, err, tc.code)
			if tc.name != "already_v3" && tc.name != "newer_version" {
				assertM02RejectedUnchanged(t, path, before)
			} else if hashFile(t, path) != before {
				t.Fatal("rejected version changed")
			}
		})
	}
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v2-history.db"])
	before := hashFile(t, path)
	contract := make(SchemaContract, 0, len(oracle.contract))
	for _, entry := range oracle.contract {
		if entry.Version == 3 {
			contract = append(contract, VersionContract{Version: 3, Fingerprints: []string{strings.Repeat("a", 64)}})
		} else {
			contract = append(contract, entry)
		}
	}
	_, err := UpgradeSnapshotV2(context.Background(), path, contract)
	requireCode(t, err, CodeUnrecognizedSchema)
	assertM02RejectedUnchanged(t, path, before)
}

func TestM02RejectsUnsafeMissingAndCanceled(t *testing.T) {
	oracle := loadM02UpgradeOracle(t)
	path := copyUpgradeFixture(t, oracle.fixtures["baseline-v2.db"])
	if err := os.WriteFile(path+"-wal", nil, 0600); err != nil {
		t.Fatal(err)
	}
	before := hashFile(t, path)
	_, err := UpgradeSnapshotV2(context.Background(), path, oracle.contract)
	requireCode(t, err, CodeUnsafeSnapshot)
	if hashFile(t, path) != before {
		t.Fatal("unsafe M02 changed DB")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.db")
	_, err = UpgradeSnapshotV2(context.Background(), missing, oracle.contract)
	requireCode(t, err, CodeMissingSnapshot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = UpgradeSnapshotV2(ctx, missing, oracle.contract)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled M02 returned %v", err)
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("M02 created missing private copy")
	}
}
