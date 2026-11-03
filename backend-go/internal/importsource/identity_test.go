package importsource

import (
	"bytes"
	"context"
	"database/sql"
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
	if err == nil || err.Error() != "IMPORT_SOURCE_LEGACY_BRIDGE_UNSUPPORTED" {
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
