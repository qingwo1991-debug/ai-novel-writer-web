package admission

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// baselineV7PinnedObjects is an independent, per-object shadow of the pinned v7
// fingerprint: the sha256 of the DDL text SQLite must record for every object,
// taken from the exported schema the pinned digest covers. It is embedded here
// rather than derived from baseline_v7_ddl.go, so a one-character edit to that
// asset is named below instead of merely moving the aggregate digest.
var baselineV7PinnedObjects = []struct {
	kind   string
	name   string
	digest string
}{
	{"index", "idx_blueprint_character_sync_status", "ab2fe8444f513835bf79c3bff7bfdc35f6655549b5ea5068c16fd7762cfef2a9"},
	{"index", "idx_chapter_deletion_status", "ae225ae38edf106fb660fcc6fe69ac8ac6e471e6b7ebbfc130e8abd49dc5f640"},
	{"index", "idx_character_avatar_revision", "520506b61ab20514f3a5af987d619f5f0aed1cbfe9cbe320794125cd1678adb2"},
	{"index", "idx_drafts_chapter", "768db49e8c5199dca2febad1724642dc26ee53db4966530affc59ae6409144d2"},
	{"index", "idx_drafts_chapter_version", "5b47f5b85f4abd1c8f703ea5ca36875c4ce980e1e0d69553b1b607bdda15ca08"},
	{"index", "idx_finalization_outbox_status", "c4dd20ba5d72fe0d54577eff6e7f052847de9681b17b87c857036f4c4e6e5277"},
	{"index", "idx_import_effect_ledger_state", "9e79734c93cd05a7a0d94f51b7b3f835962686c3b2ba72533f61dea04e089c50"},
	{"index", "idx_import_run_chapters_page", "b345b7892031fda372c55ecc48f4ae2bfb0b25f016e6de06a0956f84faea574f"},
	{"index", "idx_import_run_knowledge_receipts_affiliation", "705625d4a3f62e5ab6e8965dabd2c274adce5f366ea7cce141374db1abf2489a"},
	{"index", "idx_import_run_receipts_state", "66dfbd657ee2116c2343ddf0b7c55df42fed683c1d4e4a004e32eba621431f48"},
	{"index", "idx_import_run_source_chapters", "fc0068160287d6edab283c7d9f9d4071b4bfbd23f9f58cdb0773f64eb90be883"},
	{"index", "idx_import_runs_purpose_source_status", "47e996db150c3756cdb8c46f89dd40d480e48cfb0855b25e44857af749ccd970"},
	{"index", "idx_import_runs_resumable", "3057feea71a6e2d3f31f4b1d0ad545358bbdc4c8da3c1316157230bb5193bdfc"},
	{"index", "idx_import_runs_source_status", "2dde5aaf6f8889ef0a05dfbd22ff37728fd3947ba8c27d4f102998a0acecc382"},
	{"index", "idx_import_source_aliases_source", "c146a399dd0e9bb8306b34d72b3e081b503263a6f8edf8d3d4ea55f7513ae2a5"},
	{"index", "idx_llm_calls_time", "8ccc8a9b7b47f8a4d2117042b91da6d07a135933f0ecd3f8be3179dba34e4147"},
	{"index", "idx_narrative_thread_confirmations_plan", "d4ff54ccc2061176221bd1decf1fbc4a76f5fecc09862d4a368b197d5a7821be"},
	{"index", "idx_post_runs_source", "30b2b1bed71ae4aa26a131369b18e2fde1a1ff85a4b11a11ebca2cf5ffb8fd32"},
	{"index", "idx_recovery_candidates_pending", "8518da2df0861f65f3d894419f865d0f6c4dfda8ca78488c84619820ea7e9a0e"},
	{"index", "idx_reviews_draft_index", "6eede351685fd28d29ef88dd4562dbed470b179ba839810571078ed9340b1220"},
	{"index", "idx_revisions_draft_index", "3a08f24c1f99ae91eaab1937b73683765fe0ad4d235fc6eee48ab8c4fd41c097"},
	{"index", "idx_summary_snapshots_draft", "078f49e8b5f003f4167b78793842312809753924b0a702569f7b264cc31aa067"},
	{"table", "blueprint_character_sync_operations", "9990259419355c91abe4732289cbac5c317e3b8a6fa9eedcbce4e1d691550e9b"},
	{"table", "blueprint_commit_operations", "ef2c903bc51d0541fc37a66708812bda787a12df208686ca90bf7c36fc749e4e"},
	{"table", "blueprints", "cffa61f9ff79c0d27af01db43897657d44c83ade982e26c7194da19ea0cd9b87"},
	{"table", "chapter_deletion_operations", "2d5d232f79478db2adc872ac22888034df31c167fedcf108b4fcca0fcc326101"},
	{"table", "character_aliases", "44880f22651677023a1b0d2b26dafb3fd2551edbb35e4c922b02c7741252668f"},
	{"table", "character_avatar_assets", "ad8752d730de6027cb535333b6446d239adbc2f70428c280a49fa72113c3d612"},
	{"table", "character_avatar_unresolved", "4bef7951507f3b59db81af125f79de83796dceb2b71a733463e0e67a1943f422"},
	{"table", "character_identity_approvals", "a5f60821ce4deeac46ca04fa0837e979b62d75cafbb71d136a641957cf1e4668"},
	{"table", "character_identity_meta", "7442d31c0524beef475880f503fd42fac1730f0166814272cb96090cda26b381"},
	{"table", "character_identity_origins", "33830680d28d50ecf0f535fd3a328e467d71ef095891911bc02bc249d67278b3"},
	{"table", "character_identity_proposals", "fedbf43bab9f0fd8c4ad88cad458502f6c8a62943065e43b2f0256d9614073ee"},
	{"table", "character_relationships", "4cc1b64bd84d9c7fa2cdbe17694cb8d5947d2272bcf15d2fe7b2a7e731ef84e3"},
	{"table", "character_roster_meta", "da1d8514ea1b1727470f344512397b8aa233564845dbf47b588c6b68fd35325c"},
	{"table", "character_roster_operations", "2dc84b97053e81ded0d939fee30eb749497f189fa52f8805a924d307a57568a9"},
	{"table", "characters", "18f121c34eba018d897b043c09cb31f4bb0d2cbfafa03e3fb53505b1bb10c808"},
	{"table", "consistency_exemptions", "9603761cfc83a4c166566c766f11061a2c376cc651284ce60fd9b46fd17b5b12"},
	{"table", "contents", "54493909a1acf5bbae01cd07a9a9a12f475bcd244ff4f188db9d1cb1faad6baf"},
	{"table", "continuity_projection_meta", "f9c904de97cbc428b31a2ced012dae9f20d4c03cff28b34024438cd317424079"},
	{"table", "drafts", "8e8bdebb5826998ea1318854cea17a96b1dc27d34deb1a93df612083e100627f"},
	{"table", "finalization_outbox", "d49e4866bafcbc4853ad56605c7209288e1a26a08981110a2fac085a0adf01f4"},
	{"table", "finalized_draft_import_operations", "169b2f3621acf6ec3ea594e4f683da70a2a56cce755664cc6c7a0b9f1dbc85c0"},
	{"table", "generation_artifacts", "c35cec3088c23fa5a8005e0e51c26281f2b7a659165ef385f7ae0cba5a7d6788"},
	{"table", "generation_attempts", "0d4e8d2c69b2037410967488a2cf3206bb8de99f951b4e33df4fb78bc1457091"},
	{"table", "generation_roots", "fd08b89e719f09a904620fe1c302809a1af772c9db96b2ba29b5b09365903aa5"},
	{"table", "generation_runs", "936deead03f43fe47d3fcf309b03ff6eeb2233e1735e04e7cd4d83edde31541a"},
	{"table", "import_effect_ledger", "345ca6c1217155941448d28b3b1e59fcd789351bd2574842169e79fd162be80d"},
	{"table", "import_global_fact_operations", "d86fa79605b543f54bc4690b57e6cde28e236a55e13953ccb9f1414587652120"},
	{"table", "import_legacy_identity_bridge", "dac7b453a07fd8c42851de571e70473aeb116d2018c9becbaa9b8956ac4bdf87"},
	{"table", "import_reference_documents", "bc4e419bf75d3529a5d33dbe3fe806f2172381372cb55c314f7b00cba5adbb1d"},
	{"table", "import_run_chapters", "e54a0ba23685883c5086abe08810b0b90d0374becdc2bf39bc87bfceec0c32be"},
	{"table", "import_run_knowledge_receipts", "6b317c49fe3e2eac5f60e98e5fbf41169fe988460e543a38024d55953703852e"},
	{"table", "import_run_receipts", "7f3ecf02124c08e1f42ea027f5ab7cda468ead352943a77a7c17f6ff74e3ecb1"},
	{"table", "import_run_source_chapters", "144055d24638b22ff4944bc99c6f207d65b5a9b462863ccd0f8628f5682bb85a"},
	{"table", "import_run_sources", "3a33baeab96778e3678c9a89ecb9652415d4464af12d22a077e3f5ef06848885"},
	{"table", "import_runs", "3198b0bd65b49914e1c9506ad5e7b80a7c5b7300c2082a1882e6a0703c7f90a4"},
	{"table", "import_source_aliases", "ed0946c2640c495486d1e40b247510ea4ba565e8d024bace8ffd8141e31774e2"},
	{"table", "import_source_chapter_map", "77d9e5c819c06d8e1c1427586cdd9c3ea9553ac19089f336c13906da4612a83d"},
	{"table", "llm_calls", "a2b11f9d489aa62e50decc15b5d2c3900dbe5bc77f72e13de018cc9df9c71a4b"},
	{"table", "narrative_thread_confirmations", "71300fefa0ee7795ad22b4f2e66d2b03177ac647c61ed73d63a65bf6dbf700e5"},
	{"table", "narrative_thread_plans", "eaa1e8387cb383a960be3603077ba082aad5d9587d91f721c5227a123dab6976"},
	{"table", "post_process_runs", "6fa0c4cf7b9c1ced76299e6337f1463760c0b28375f5b87bbdb4c4c81bed0a31"},
	{"table", "post_process_steps", "88e7c0277df0f5b47f30695236342f5e3ef0e23494ee5b25027ae2fde3d55c63"},
	{"table", "project_core", "454905cc6b1638117e1aef3da75d0ba0d7b42403fa07280df26b6ba028955435"},
	{"table", "recovery_candidates", "775e9fbf5997eac80247b5009cf3f7a05e61d6bdbdfb3d930c94390acf996efc"},
	{"table", "review_cycle_merges", "ce831374f8ad7ecc4fa57669941b3bdc36181b16bed7f6cd329ce6a6b15f802c"},
	{"table", "review_cycles", "095eb92ab03f75c399b1a760f90e7648255059f34620b8ab59d002d22dd7fb7c"},
	{"table", "review_findings", "22f296c2e434e7d5bc122140f5bb94d7c5175155a1f15d42adaba2c0ac50bda3"},
	{"table", "reviews", "6db520063797e43084a177a6df07533d60998f8229ca8b0a32c2ac2937b7c0f8"},
	{"table", "revisions", "e84f4bc2059e946de327920b655718c8b43cf1ea608bace5ab995607f24e3742"},
	{"table", "summary_snapshots", "46bb225714b472f9bfc825f241a9aa6228155d641662139ad808a4ac89116e5d"},
	{"table", "text_metric_versions", "0e7e6f862959d75df38c992075bc1e874f313902bed8ed6813966f19a28c1b60"},
	{"trigger", "retain_character_avatar_on_delete", "f9861d5d9a9ba4b343e20d5a882f387983c5aec7a9bcd3057f809ead213cb537"},
}

// baselineV7Record is one sqlite_master row in the order admission reads it.
type baselineV7Record struct {
	kind  string
	name  string
	table string
	ddl   string
}

// baselineV7Open opens an empty database at path and registers its cleanup.
func baselineV7Open(t *testing.T, path string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=rwc"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	return db
}

// baselineV7DonorObjects reproduces the donor variant the pinned contract also
// recognises: one avatar column spliced into characters at the marker the
// TypeScript contract builder uses. Only the baseline shape is provisioned in
// production, so this variant exists solely to prove the characters text.
func baselineV7DonorObjects(t *testing.T) []baselineV7Object {
	t.Helper()
	const marker = "      cs_location TEXT DEFAULT '',"
	objects := make([]baselineV7Object, len(baselineV7Objects))
	copy(objects, baselineV7Objects)
	found := false
	for index := range objects {
		if objects[index].kind != "table" || objects[index].name != "characters" {
			continue
		}
		if !strings.Contains(objects[index].ddl, marker) {
			t.Fatalf("characters DDL lost the baseline marker %q", marker)
		}
		objects[index].ddl = strings.Replace(objects[index].ddl, marker, "      avatar TEXT NOT NULL DEFAULT '',\n"+marker, 1)
		found = true
	}
	if !found {
		t.Fatal("baseline v7 asset has no characters table")
	}
	return objects
}

// baselineV7Provision builds a fresh database from the asset and returns the
// digest admission computes for it together with every recorded object.
func baselineV7Provision(t *testing.T, donor bool) (string, []baselineV7Record) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "baseline-v7.db")
	db := baselineV7Open(t, path)
	objects := baselineV7Objects
	if donor {
		objects = baselineV7DonorObjects(t)
	}
	if err := writeBaselineV7(path, db, objects); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != baselineV7SchemaVersion {
		t.Fatalf("user_version = %d; want %d", version, baselineV7SchemaVersion)
	}
	violations, err := baselineV7RowCount(ctx, db, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 0 {
		t.Fatalf("foreign_key_check reported %d violations", len(violations))
	}
	records, err := baselineV7ReadSchema(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	// schemaFingerprint is admission's own encoder (snapshot.go:212): it
	// emits the unescaped JSON.stringify form for <, >, &, U+2028 and U+2029,
	// which this DDL exercises through CHECK (... <> ...). Writing the digest
	// by hand with encoding/json would escape those and prove nothing.
	fingerprint, tables, err := schemaFingerprint(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != 51 {
		t.Fatalf("schema fingerprint covered %d tables; want 51", len(tables))
	}
	return fingerprint, records
}

// baselineV7ReadSchema reads sqlite_master exactly as admission does.
func baselineV7ReadSchema(ctx context.Context, db *sql.DB) ([]baselineV7Record, error) {
	rows, err := db.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []baselineV7Record
	for rows.Next() {
		var record baselineV7Record
		var ddl sql.NullString
		if err := rows.Scan(&record.kind, &record.name, &record.table, &ddl); err != nil {
			return nil, err
		}
		record.ddl = ddl.String
		records = append(records, record)
	}
	return records, rows.Err()
}

// sqlRows returns the raw values of every row, which is all a pragma check
// needs.
func baselineV7RowCount(ctx context.Context, db *sql.DB, query string) ([]int, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var count []int
	for rows.Next() {
		count = append(count, len(count))
	}
	return count, rows.Err()
}

// firstBaselineV7Drift names the first object that differs from the pinned
// per-object digests, so one CI run reports what drifted (a missing, extra,
// renamed or edited object) instead of only that a digest differs.
func firstBaselineV7Drift(records []baselineV7Record) string {
	if len(records) != len(baselineV7PinnedObjects) {
		return fmt.Sprintf("object count %d, pinned %d", len(records), len(baselineV7PinnedObjects))
	}
	for index, pinned := range baselineV7PinnedObjects {
		record := records[index]
		if record.kind != pinned.kind || record.name != pinned.name {
			return fmt.Sprintf("%s %s at position %d, pinned %s %s", record.kind, record.name, index, pinned.kind, pinned.name)
		}
		digest := sha256.Sum256([]byte(record.ddl))
		if hex.EncodeToString(digest[:]) != pinned.digest {
			return fmt.Sprintf("%s %s", pinned.kind, pinned.name)
		}
	}
	return "none"
}

// pinnedV7Fingerprints returns the pinned fingerprints for the version this
// asset materialises, sorted so a failure prints them stably.
func pinnedV7Fingerprints(t *testing.T) []string {
	t.Helper()
	contract, err := PinnedSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	var fingerprints []string
	for _, version := range contract {
		if version.Version != baselineV7SchemaVersion {
			continue
		}
		for _, fingerprint := range version.Fingerprints {
			if seen[fingerprint] {
				continue
			}
			seen[fingerprint] = true
			fingerprints = append(fingerprints, fingerprint)
		}
	}
	if len(fingerprints) == 0 {
		t.Fatalf("pinned contract holds no version %d fingerprint", baselineV7SchemaVersion)
	}
	sort.Strings(fingerprints)
	return fingerprints
}

// TestBaselineV7DDLReproducesPinnedFingerprint is the whole point of the asset:
// it applies baseline_v7_ddl.go to a fresh database, hashes sqlite_master the
// way admission does, and asserts the digest is one the pinned contract in
// pinned_contract.go admits at version 7. A drift fails with both digests and
// the first differing object name.
func TestBaselineV7DDLReproducesPinnedFingerprint(t *testing.T) {
	pinned := pinnedV7Fingerprints(t)
	baseline, records := baselineV7Provision(t, false)
	if drift := firstBaselineV7Drift(records); drift != "none" {
		t.Fatalf("baseline v7 DDL drifted from the pinned object set: first differing object %s; produced digest %s; pinned %v", drift, baseline, pinned)
	}
	if !baselineV7Contains(pinned, baseline) {
		t.Fatalf("baseline v7 DDL digest %s is not pinned; pinned %v; first differing object %s", baseline, pinned, firstBaselineV7Drift(records))
	}
	donor, _ := baselineV7Provision(t, true)
	if donor == baseline {
		t.Fatalf("donor variant reproduced the baseline digest %s, so it proves nothing", baseline)
	}
	if !baselineV7Contains(pinned, donor) {
		t.Fatalf("donor v7 DDL digest %s is not pinned; pinned %v; baseline digest %s; first differing object characters table", donor, pinned, baseline)
	}
}

// TestBaselineV7StatementsAreACopy proves a caller cannot mutate the package
// asset through the exported statement list.
func TestBaselineV7StatementsAreACopy(t *testing.T) {
	statements := BaselineV7Statements()
	if len(statements) != len(baselineV7PinnedObjects) {
		t.Fatalf("BaselineV7Statements returned %d statements; pinned object count is %d", len(statements), len(baselineV7PinnedObjects))
	}
	statements[0] = "DROP TABLE project_core"
	if BaselineV7Statements()[0] != baselineV7Objects[0].ddl {
		t.Fatal("BaselineV7Statements exposes the package asset for mutation")
	}
}

// TestWriteBaselineV7RefusesExistingSchema keeps provisioning from silently
// rewriting a database that already holds a schema.
func TestWriteBaselineV7RefusesExistingSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "baseline-v7.db")
	db := baselineV7Open(t, path)
	if err := WriteBaselineV7(path, db); err != nil {
		t.Fatal(err)
	}
	if err := WriteBaselineV7(path, db); err == nil {
		t.Fatal("WriteBaselineV7 stamped a database that already had a schema")
	}
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != baselineV7SchemaVersion {
		t.Fatalf("user_version = %d; want %d", version, baselineV7SchemaVersion)
	}
	if err := WriteBaselineV7("", db); err == nil {
		t.Fatal("WriteBaselineV7 accepted an empty destination path")
	}
}

func baselineV7Contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
