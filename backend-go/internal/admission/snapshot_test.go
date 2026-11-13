package admission

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const parentDDL = "CREATE TABLE parents (id INTEGER PRIMARY KEY, label TEXT NOT NULL)"
const childDDL = "CREATE TABLE children (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parents(id), body TEXT)"

// This oracle constructs the known synthetic DDL objects independently from the
// verifier's sqlite_master reader. Production trust comes only from TS exports.
func syntheticContract() SchemaContract {
	objects := []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Table string `json:"tbl_name"`
		SQL   string `json:"sql"`
	}{
		{"table", "children", "children", childDDL},
		{"table", "parents", "parents", parentDDL},
	}
	encoded, _ := json.Marshal(objects)
	digest := sha256.Sum256(encoded)
	return SchemaContract{{Version: 7, Fingerprints: []string{hex.EncodeToString(digest[:])}}}
}

func fixtureDB(t *testing.T) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "staged ?#数据库.db")
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=rwc"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, statement := range []string{parentDDL, childDDL, "PRAGMA user_version = 7",
		"INSERT INTO parents VALUES (7, '合成')", "INSERT INTO children VALUES (11, 7, '原文' || char(13,10) || 'C:\\author\\notes')"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return path, db
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var admissionError *Error
	if !errors.As(err, &admissionError) || admissionError.Code != code {
		t.Fatalf("error = %v; want admission code %s", err, code)
	}
	if err.Error() != code {
		t.Fatalf("error exposed additional details: %q", err.Error())
	}
}

func hashFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func TestVerifyExactSnapshotPreservesBytesAndCounts(t *testing.T) {
	path, db := fixtureDB(t)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := hashFile(t, path)
	evidence, err := VerifySnapshot(context.Background(), path, syntheticContract())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.UserVersion != 7 || evidence.SchemaFingerprint != syntheticContract()[0].Fingerprints[0] {
		t.Fatalf("unexpected schema evidence: %+v", evidence)
	}
	if evidence.TableCounts["parents"] != 1 || evidence.TableCounts["children"] != 1 || len(evidence.TableCounts) != 2 {
		t.Fatalf("unexpected counts: %+v", evidence.TableCounts)
	}
	empty := sha256.Sum256(nil)
	if evidence.ContentIdentity.Database != before || evidence.ContentIdentity.WAL != hex.EncodeToString(empty[:]) || hashFile(t, path) != before {
		t.Fatal("verification changed database bytes or returned incorrect physical identity")
	}
	for _, suffix := range []string{"-wal", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected sidecar %s: %v", suffix, err)
		}
	}
}

func TestRejectsVersionAndDDLTampering(t *testing.T) {
	for _, test := range []struct{ name, mutation, code string }{
		{"older", "PRAGMA user_version = 6", CodeMigrationRequired},
		{"unversioned", "PRAGMA user_version = 0", CodeMigrationRequired},
		{"newer", "PRAGMA user_version = 8", CodeNewerSchema},
		{"negative", "PRAGMA user_version = -1", CodeInvalidVersion},
		{"extra_column", "ALTER TABLE children ADD COLUMN unexpected TEXT", CodeUnrecognizedSchema},
		{"extra_index", "CREATE INDEX unexpected ON children(body)", CodeUnrecognizedSchema},
		{"extra_view", "CREATE VIEW unexpected AS SELECT * FROM children", CodeUnrecognizedSchema},
		{"extra_trigger", "CREATE TRIGGER unexpected AFTER INSERT ON children BEGIN SELECT 1; END", CodeUnrecognizedSchema},
		{"missing_table", "DROP TABLE children", CodeUnrecognizedSchema},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, db := fixtureDB(t)
			if _, err := db.Exec(test.mutation); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before := hashFile(t, path)
			_, err := VerifySnapshot(context.Background(), path, syntheticContract())
			requireCode(t, err, test.code)
			if hashFile(t, path) != before {
				t.Fatal("rejected snapshot changed")
			}
		})
	}
}

func TestVersionFingerprintPairNotFingerprintAlone(t *testing.T) {
	path, db := fixtureDB(t)
	db.Close()
	contract := SchemaContract{
		{Version: 6, Fingerprints: syntheticContract()[0].Fingerprints},
		{Version: 7, Fingerprints: []string{strings.Repeat("a", 64)}},
	}
	_, err := VerifySnapshot(context.Background(), path, contract)
	requireCode(t, err, CodeUnrecognizedSchema)
}

func TestEmptyForgedCurrentVersionIsNotAdmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forged.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version=7"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	db.Close()
	_, err = VerifySnapshot(context.Background(), path, syntheticContract())
	requireCode(t, err, CodeUnrecognizedSchema)
	// Even a mistakenly empty-schema fixture is not an admission authority.
	emptyDigest := sha256.Sum256([]byte("[]"))
	_, err = VerifySnapshot(context.Background(), path, SchemaContract{{Version: 7, Fingerprints: []string{hex.EncodeToString(emptyDigest[:])}}})
	requireCode(t, err, CodeUnrecognizedSchema)
}

func TestRejectsForeignKeyViolationWithoutRepair(t *testing.T) {
	path, db := fixtureDB(t)
	if _, err := db.Exec("INSERT INTO children VALUES (12, 999, 'orphan')"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before := hashFile(t, path)
	_, err := VerifySnapshot(context.Background(), path, syntheticContract())
	requireCode(t, err, CodeForeignKey)
	if hashFile(t, path) != before {
		t.Fatal("foreign key rejection repaired data")
	}
}

func TestRejectsCorruptionMissingAndUnsafeFiles(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing.db")
	_, err := VerifySnapshot(context.Background(), missing, syntheticContract())
	requireCode(t, err, CodeMissingSnapshot)
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing DB was created")
	}
	for _, data := range [][]byte{nil, []byte("not a SQLite database"), append([]byte("SQLite format 3\x00"), make([]byte, 496)...)} {
		path := filepath.Join(root, "corrupt.db")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		_, err := VerifySnapshot(context.Background(), path, syntheticContract())
		requireCode(t, err, CodeIntegrity)
	}
	path, db := fixtureDB(t)
	db.Close()
	link := filepath.Join(root, "symlink.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	_, err = VerifySnapshot(context.Background(), link, syntheticContract())
	requireCode(t, err, CodeUnsafeSnapshot)
	hardlink := filepath.Join(root, "hardlink.db")
	if err := os.Link(path, hardlink); err != nil {
		t.Fatal(err)
	}
	_, err = VerifySnapshot(context.Background(), hardlink, syntheticContract())
	requireCode(t, err, CodeUnsafeSnapshot)
	if err := os.Remove(hardlink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, path+"-wal"); err != nil {
		t.Fatal(err)
	}
	_, err = VerifySnapshot(context.Background(), path, syntheticContract())
	requireCode(t, err, CodeUnsafeSnapshot)
}

func TestCopiedWALSnapshotReadsCommittedFrames(t *testing.T) {
	path, source := fixtureDB(t)
	for _, statement := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0",
		"INSERT INTO children VALUES (22,7,'committed only in copied WAL')"} {
		if _, err := source.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	originalDB, originalWAL := hashFile(t, path), hashFile(t, path+"-wal")
	copyPath := filepath.Join(t.TempDir(), "snapshot.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		data, err := os.ReadFile(path + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copyPath+suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	evidence, err := VerifySnapshot(context.Background(), copyPath, syntheticContract())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.TableCounts["children"] != 2 {
		t.Fatalf("copied WAL commit ignored: %+v", evidence.TableCounts)
	}
	if evidence.ContentIdentity.Database != originalDB || evidence.ContentIdentity.WAL != originalWAL {
		t.Fatal("copied physical identity differs from original")
	}
	if hashFile(t, path) != originalDB || hashFile(t, path+"-wal") != originalWAL {
		t.Fatal("verification touched source database/WAL")
	}
	// A copied WAL without SHM is allowed: SQLite can build reader scratch state
	// in the private writable staging directory, never alongside the author DB.
	if err := os.Remove(copyPath + "-shm"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	evidence, err = VerifySnapshot(context.Background(), copyPath, syntheticContract())
	if err != nil {
		t.Fatal(err)
	}
	if evidence.TableCounts["children"] != 2 {
		t.Fatal("WAL without copied SHM ignored")
	}
}

func TestJSCompatibleStringEscaping(t *testing.T) {
	value := "<>&\u2028\u2029中文\"\\\b\f\n\r\t\x00\x1f"
	want := "\"<>&\u2028\u2029中文\\\"\\\\\\b\\f\\n\\r\\t\\u0000\\u001f\""
	if got := string(appendJSString(nil, value)); got != want {
		t.Fatalf("JSON encoding = %q; want %q", got, want)
	}
}

func TestCanceledContextDoesNotOpenSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := VerifySnapshot(ctx, filepath.Join(t.TempDir(), "missing.db"), syntheticContract())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v; want cancellation", err)
	}
}

func TestInvalidContractFailsBeforeOpening(t *testing.T) {
	for _, contract := range []SchemaContract{
		nil,
		{{Version: 7}},
		{{Version: 7, Fingerprints: []string{"not-a-digest"}}},
		{{Version: 8, Fingerprints: []string{strings.Repeat("a", 64)}}},
		{{Version: 6, Fingerprints: []string{strings.Repeat("a", 64)}}},
		{{Version: 7, Fingerprints: []string{strings.Repeat("a", 64), strings.Repeat("a", 64)}}},
	} {
		_, err := VerifySnapshot(context.Background(), "", contract)
		requireCode(t, err, CodeInvalidContract)
	}
}

func TestParseTrustedContract(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	valid := `{"formatVersion":1,"currentVersion":7,"recognized":[{"version":6,"fingerprint":"` + fingerprint + `"},{"version":7,"fingerprint":"` + fingerprint + `"}],"fixtures":[]}`
	contract, err := ParseContract(strings.NewReader(valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(contract) != 2 || contract[0].Version != 6 || contract[1].Version != 7 {
		t.Fatalf("unexpected grouped contract: %+v", contract)
	}
	for _, encoded := range []string{"{}", valid + `{}`, strings.Replace(valid, `"formatVersion":1`, `"formatVersion":2`, 1), strings.Replace(valid, `"fixtures":[]`, `"unexpected":true`, 1)} {
		_, err := ParseContract(strings.NewReader(encoded))
		requireCode(t, err, CodeInvalidContract)
	}
}
