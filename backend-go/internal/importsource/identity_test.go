package importsource

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/hex"
	"strings"
	"testing"
)

func TestLegacyIdentityBridgeFailsClosedBeforeAliasWrite(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE import_source_aliases(alias_digest TEXT PRIMARY KEY,alias_kind TEXT NOT NULL,source_id TEXT NOT NULL); CREATE TABLE import_legacy_identity_bridge(id TEXT PRIMARY KEY,ciphertext_hex TEXT,iv_hex TEXT,auth_tag_hex TEXT); INSERT INTO import_legacy_identity_bridge VALUES ('main','synthetic','synthetic','synthetic')`)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{107}, 32)
	encoded, err := EncodeIdentities([]Identity{{"captured:synthetic", "stage:synthetic"}}, Reference, key, false)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveEncodedSources(context.Background(), tx, encoded, Reference, key)
	if err == nil || err.Error() != "IMPORT_SOURCE_LEGACY_BRIDGE_INVALID" {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM import_source_aliases").Scan(&count); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if count != 0 {
		_ = tx.Rollback()
		t.Fatal("legacy fallback wrote aliases")
	}
	_ = tx.Rollback()
}

func testEncryptedBridge(t *testing.T, db *sql.DB, key, salt []byte) {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	iv := bytes.Repeat([]byte{7}, aead.NonceSize())
	sealed := aead.Seal(nil, iv, salt, []byte(legacyBridgeAAD))
	_, err = db.Exec(`INSERT INTO import_legacy_identity_bridge(id,ciphertext_hex,iv_hex,auth_tag_hex) VALUES('main',?,?,?)`, hex.EncodeToString(sealed[:len(sealed)-aead.Overhead()]), hex.EncodeToString(iv), hex.EncodeToString(sealed[len(sealed)-aead.Overhead():]))
	if err != nil {
		t.Fatal(err)
	}
}

func TestLegacyBridgeAuthenticatesAndPreservesFingerprints(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE import_source_aliases(alias_digest TEXT PRIMARY KEY,alias_kind TEXT NOT NULL,source_id TEXT NOT NULL); CREATE TABLE import_legacy_identity_bridge(id TEXT PRIMARY KEY,ciphertext_hex TEXT,iv_hex TEXT,auth_tag_hex TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	key, salt := bytes.Repeat([]byte{11}, 32), bytes.Repeat([]byte{23}, 32)
	testEncryptedBridge(t, db, key, salt)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	identities := []Identity{{CanonicalLocation: "/author/A.txt", FileIdentity: "dev:1:ino:12"}, {CanonicalLocation: "/author/B.txt", FileIdentity: "dev:1:ino:2"}}
	encoded, err := EncodeProjectIdentities(context.Background(), tx, identities, Reference, key, false)
	if err != nil || encoded[0].LegacyStableIdentity != "dev:1:ino:12" {
		t.Fatalf("encoded %+v: %v", encoded, err)
	}
	resolved, err := ResolveEncodedSources(context.Background(), tx, encoded, Reference, key)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.SourceIDs) != 2 || len(resolved.LegacySourceFingerprints) != 2 {
		t.Fatalf("missing legacy output %+v", resolved)
	}
	collection := keyedDigest(salt, "", jsonExact(struct {
		Purpose    Purpose  `json:"purpose"`
		Identities []string `json:"identities"`
	}{Reference, []string{"dev:1:ino:12", "dev:1:ino:2"}}))
	if resolved.LegacyCollectionFingerprint != collection {
		t.Fatalf("legacy collection mismatch %s != %s", resolved.LegacyCollectionFingerprint, collection)
	}
	for i, identity := range []string{"dev:1:ino:12", "dev:1:ino:2"} {
		want := keyedDigest(salt, "", jsonExact(struct {
			Purpose    Purpose  `json:"purpose"`
			Identities []string `json:"identities"`
		}{Reference, []string{identity}}))
		if resolved.LegacySourceFingerprints[i] != want {
			t.Fatalf("legacy source %d mismatch", i)
		}
	}
	if strings.Contains(jsonExact(encoded), "dev:1:ino") {
		t.Fatal("server-only legacy identity appeared in JSON")
	}
}

func TestLegacyBridgeRejectsUnprovedCollationBeforeAliasWrite(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE import_source_aliases(alias_digest TEXT PRIMARY KEY,alias_kind TEXT NOT NULL,source_id TEXT NOT NULL); CREATE TABLE import_legacy_identity_bridge(id TEXT PRIMARY KEY,ciphertext_hex TEXT,iv_hex TEXT,auth_tag_hex TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	key, salt := bytes.Repeat([]byte{11}, 32), bytes.Repeat([]byte{23}, 32)
	testEncryptedBridge(t, db, key, salt)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	encoded, err := EncodeProjectIdentities(context.Background(), tx, []Identity{{CanonicalLocation: "/作者/甲.txt"}, {CanonicalLocation: "/作者/乙.txt"}}, Reference, key, false)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveEncodedSources(context.Background(), tx, encoded, Reference, key)
	if err == nil || err.Error() != "IMPORT_SOURCE_LEGACY_COLLATION_UNSUPPORTED" {
		t.Fatal(err)
	}
	var count int
	if err = tx.QueryRow("SELECT count(*) FROM import_source_aliases").Scan(&count); err != nil || count != 0 {
		t.Fatalf("aliases mutated: %d %v", count, err)
	}
}

func TestLegacyBridgeRejectsAuthenticationFailure(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE import_source_aliases(alias_digest TEXT PRIMARY KEY,alias_kind TEXT NOT NULL,source_id TEXT NOT NULL); CREATE TABLE import_legacy_identity_bridge(id TEXT PRIMARY KEY,ciphertext_hex TEXT,iv_hex TEXT,auth_tag_hex TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	key, salt := bytes.Repeat([]byte{11}, 32), bytes.Repeat([]byte{23}, 32)
	testEncryptedBridge(t, db, key, salt)
	_, err = db.Exec(`UPDATE import_legacy_identity_bridge SET auth_tag_hex = ? WHERE id = 'main'`, strings.Repeat("0", 32))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = EncodeProjectIdentities(context.Background(), tx, []Identity{{CanonicalLocation: "/author/one.txt", FileIdentity: "dev:1:ino:1"}}, Reference, key, false)
	if err == nil || err.Error() != "IMPORT_SOURCE_LEGACY_BRIDGE_INVALID" {
		t.Fatal(err)
	}
}

func TestEmptyLegacyIdentityBridgeAllowsAliasResolution(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`CREATE TABLE import_source_aliases(alias_digest TEXT PRIMARY KEY,alias_kind TEXT NOT NULL,source_id TEXT NOT NULL); CREATE TABLE import_legacy_identity_bridge(id TEXT PRIMARY KEY,ciphertext_hex TEXT,iv_hex TEXT,auth_tag_hex TEXT)`)
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{107}, 32)
	encoded, err := EncodeIdentities([]Identity{{"captured:synthetic", "stage:synthetic"}}, Reference, key, false)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	resolved, err := ResolveEncodedSources(context.Background(), tx, encoded, Reference, key)
	if err != nil || len(resolved.SourceIDs) != 1 || resolved.SourceIDs[0] == "" {
		t.Fatalf("empty bridge rejected: %+v %v", resolved, err)
	}
}
