package admission

import (
	"context"
	"database/sql"
	"testing"
)

func identityFixture(t *testing.T) (string, *sql.DB, SchemaContract) {
	t.Helper()
	path, db := fixtureDB(t)
	for _, statement := range []string{
		"CREATE TABLE character_identity_meta (id TEXT PRIMARY KEY, revision INTEGER, legacy_count INTEGER)",
		"CREATE TABLE characters (character_id TEXT PRIMARY KEY, identity_revision INTEGER, legacy_key TEXT)",
		"CREATE TABLE character_identity_origins (character_id TEXT, original_row_json TEXT, original_hash TEXT)",
		"CREATE TABLE character_identity_proposals (candidate_ids_json TEXT, resolved_character_id TEXT, approval_id TEXT)",
		"CREATE TABLE character_aliases (character_id TEXT, name TEXT, source_key TEXT, valid_from INTEGER, valid_through INTEGER)",
		"INSERT INTO character_identity_meta VALUES ('main',0,0)",
	} {
		if _, err := db.Exec(statement); err != nil { t.Fatal(err) }
	}
	tx, err := db.Begin()
	if err != nil { t.Fatal(err) }
	fingerprint, _, err := schemaFingerprint(context.Background(), tx)
	tx.Rollback()
	if err != nil { t.Fatal(err) }
	return path, db, SchemaContract{{Version: 7, Fingerprints: []string{fingerprint}}}
}

func TestIdentityInvariantsAllowEmptyButInitializedMetadata(t *testing.T) {
	path, db, contract := identityFixture(t)
	db.Close()
	if _, err := VerifySnapshot(context.Background(), path, contract); err != nil { t.Fatal(err) }
}

func TestIdentityInvariantsRejectDamagedDomainWithoutRepair(t *testing.T) {
	for _, test := range []struct { name, mutation string }{
		{"missing_meta", "DELETE FROM character_identity_meta"},
		{"negative_revision", "UPDATE character_identity_meta SET revision=-1"},
		{"unsafe_revision", "UPDATE character_identity_meta SET revision=9007199254740992"},
		{"wrong_legacy_count", "UPDATE character_identity_meta SET legacy_count=1"},
		{"blank_character_id", "INSERT INTO characters VALUES (' ',0,NULL)"},
		{"ahead_character_revision", "INSERT INTO characters VALUES ('id',1,NULL)"},
		{"unknown_candidate", "INSERT INTO character_identity_proposals VALUES ('[\"unknown\"]',NULL,NULL)"},
		{"not_array", "INSERT INTO character_identity_proposals VALUES ('{}',NULL,NULL)"},
		{"null_candidates", "INSERT INTO character_identity_proposals VALUES ('null',NULL,NULL)"},
		{"missing_approval", "INSERT INTO character_identity_proposals VALUES ('[]','id',NULL)"},
		{"alias_unknown_character", "INSERT INTO character_aliases VALUES ('unknown','name','source',0,NULL)"},
		{"alias_future_revision", "INSERT INTO characters VALUES ('id',0,NULL); INSERT INTO character_aliases VALUES ('id','name','source',1,NULL)"},
		{"bad_original_hash", "INSERT INTO characters VALUES ('id',0,'name'); UPDATE character_identity_meta SET legacy_count=1; INSERT INTO character_identity_origins VALUES ('id','{\"name\":\"name\"}','bad')"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path, db, contract := identityFixture(t)
			if _, err := db.Exec(test.mutation); err != nil { t.Fatal(err) }
			db.Close()
			before := hashFile(t, path)
			_, err := VerifySnapshot(context.Background(), path, contract)
			requireCode(t, err, CodeDomain)
			if hashFile(t, path) != before { t.Fatal("domain verification repaired snapshot") }
		})
	}
}
