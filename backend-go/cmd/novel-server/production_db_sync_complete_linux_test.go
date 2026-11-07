//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// db:blueprint-character-sync-complete runs here over a disposable t.TempDir()
// only: the project, its blueprint ledgers, its identity/roster fact tables and
// the roster operation receipt the proof reads all live inside the test's own
// temporary tree. No real author data root is read or written.
//
// # What these tests have to prove
//
//  1. a roster the proof accepts completes the row and writes the reference's own
//     receipt, in the file and not only in the answer
//     (TestProductionDBSyncCompleteProvesTheRosterAndWritesItsReceipt);
//  2. a roster that is not authoritative refuses by name and changes nothing
//     (TestProductionDBSyncCompleteRefusesANonAuthoritativeRoster);
//  3. a row that cannot be completed refuses before any write
//     (TestProductionDBSyncCompleteRefusesAnUncompletableRow);
//  4. a foreign project and a stale lease are refused with the file
//     byte-identical (TestProductionDBSyncCompleteRefusesForeignProjectAndStaleLease);
//  5. the binder installs exactly one inventoried name, and every reference line
//     this port quotes still says what it is quoted for
//     (TestProductionDBSyncCompleteBindsOneName).
//
// # Why the byte comparison is meaningful
//
// productionWriteSnapshot / productionWriteAssertUnchanged read the whole
// database file, so a refusal is only "no write" when the bytes are the same
// bytes. The refusals below may run the reference's own insert-only lazy repair
// (blueprint-repository.ts:176-194, called at :770) before they refuse; that
// repair inserts nothing when the commit ledger is already represented, and a
// no-op INSERT OR IGNORE, an empty BEGIN IMMEDIATE/COMMIT and a no-op UPDATE are
// all byte-stable in SQLite, so the assertion is exact rather than approximate.
// Every refusal is ALSO pinned at the row level, because the row is the fact the
// renderer acts on.

const (
	productionDBSyncCompleteProjectName = "sync-complete-novel"
	productionDBSyncCompleteProjectID   = "c4e8a1f6-0b27-4d39-8f51-6a2c9e0d7b13"
	productionDBSyncCompleteTab         = "client_sync_complete01"
	productionDBSyncCompleteOpenID      = "db-sync-complete-open-1"
	productionDBSyncCompleteStaleLease  = "db-sync-complete-stale-lease"

	// The commit operation the sync row belongs to, and the generated key the
	// reference and the store's lazy repair both use for the sync row itself
	// ('blueprint-sync-' || operation_id).
	productionDBSyncCompleteCommitOperation = "db-sync-complete-commit-1"
	productionDBSyncCompleteSyncOperation   = "blueprint-sync-db-sync-complete-commit-1"

	// The commit row's own payload digest, and the desktop's roster commit receipt
	// for the sync operation: one lowercase sha256 digest each. They are different
	// facts - the blueprint range that was committed, and the roster commit that
	// then satisfied it - so the fixture never reuses one for the other. The
	// projection digest is filled in from the roster meta row the fixture's own
	// projection refresh wrote, never hand-computed.
	productionDBSyncCompleteCommitPayloadHash = "89abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567"
	productionDBSyncCompleteRosterPayloadHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	productionDBSyncCompleteCharacterA = "ci-sync-complete-a"
	productionDBSyncCompleteCharacterB = "ci-sync-complete-b"

	// The frozen blueprint facts the commit row recorded: one declared new
	// character candidate that is already a roster fact, and one structured
	// relationship hint recorded in both directions.
	productionDBSyncCompleteSyncInput = `[{"chapterNumber":1,"title":"第一章 敲门","role":"setup","purpose":"开篇","keyEvents":"他推开门","characters":["林岚","赵铭"],"suspenseHook":"门外有人","userGuidance":"","notes":"","notesUpdatedAt":"","newCharacterCandidates":[{"name":"赵铭","role":"supporting"}],"relationshipHints":[{"from":"林岚","to":"赵铭","relation":"师徒"}]}]`

	// The author-written timestamps the completion must move. A second completion
	// must leave them exactly as the first one wrote them.
	productionDBSyncCompleteSeededUpdatedAt = "1999-12-31 23:59:59"
)

// productionDBSyncCompleteSchema is the durable fixture plus the two blueprint
// ledgers the completion reads and writes, the character-roster operation receipt
// the proof reads, and the identity/roster fact tables the authoritative roster
// is derived from. The roster DDL is productionWriteSchema's own text, so the one
// admission fingerprint this fixture pins hashes the same statements a real
// project's sqlite_master holds.
func productionDBSyncCompleteSchema() []string {
	return append([]string{
		`CREATE TABLE blueprint_commit_operations (
 operation_id TEXT PRIMARY KEY,
 payload_hash TEXT NOT NULL,
 mode TEXT NOT NULL,
 start_chapter INTEGER NOT NULL,
 end_chapter INTEGER NOT NULL,
 character_sync_input TEXT NOT NULL,
 committed_at TEXT NOT NULL DEFAULT (datetime('now')))`,
		`CREATE TABLE blueprint_character_sync_operations (
 operation_id TEXT PRIMARY KEY,
 blueprint_commit_operation_id TEXT NOT NULL UNIQUE,
 blueprint_commit_payload_hash TEXT NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending',
 start_chapter INTEGER NOT NULL,
 end_chapter INTEGER NOT NULL,
 character_sync_input TEXT NOT NULL,
 completion_receipt TEXT DEFAULT NULL,
 created_at TEXT NOT NULL DEFAULT (datetime('now')),
 updated_at TEXT NOT NULL DEFAULT (datetime('now')),
 completed_at TEXT DEFAULT NULL)`,
		`CREATE TABLE character_roster_operations (
 operation_id TEXT PRIMARY KEY,
 payload_hash TEXT NOT NULL,
 committed_revision INTEGER NOT NULL,
 projection_hash TEXT NOT NULL,
 created_at TEXT NOT NULL DEFAULT (datetime('now')))`,
	}, productionWriteSchema...)
}

// productionDBSyncCompleteSeed writes the author rows the subtests start from:
// one project core row, the identity and roster meta singletons, two characters,
// the two directions of one relationship, the commit row that froze the sync
// input, and the pending sync row for it.
//
// The roster is normalized by the store's OWN projection refresh, exactly like a
// real author project, so the fixture starts from the self-consistent "ready"
// state the proof demands instead of hand-computed roster hashes. The roster
// operation receipt is written afterwards, from the revision and projection the
// refresh itself stored, so the evidence names a revision the roster really
// reached and a projection digest that really is the current one.
func productionDBSyncCompleteSeed(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	statements := []string{
		`INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, premise, worldbuilding, synopsis, characters_arch,
 created_at, updated_at)
 VALUES ('main', '完成验证小说', 'zh-CN', 'fantasy', '', '', 12, 3000, 'auto', 3,
 '', '', '', '', '', '', '', '', '', '{}', '', '', '', '',
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
		`INSERT INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)`,
		`INSERT INTO character_roster_meta (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'empty', '', '', '')`,
		`INSERT INTO characters (character_id, legacy_key, name, role, gender, age, appearance,
 personality, background, abilities, motivation, relationships, arc, notes, cs_provenance,
 static_provenance, identity_revision, retired)
 VALUES ('` + productionDBSyncCompleteCharacterA + `', NULL, '林岚', 'protagonist',
 '', '', '', '', '', '', '', '', '', '', '{}', '{}', 0, 0)`,
		`INSERT INTO characters (character_id, legacy_key, name, role, gender, age, appearance,
 personality, background, abilities, motivation, relationships, arc, notes, cs_provenance,
 static_provenance, identity_revision, retired)
 VALUES ('` + productionDBSyncCompleteCharacterB + `', NULL, '赵铭', 'supporting',
 '', '', '', '', '', '', '', '', '', '', '{}', '{}', 0, 0)`,
		`INSERT INTO character_relationships (relationship_id, source_character_id, target_character_id, relation)
 VALUES ('rel-sync-1', '` + productionDBSyncCompleteCharacterA + `', '` + productionDBSyncCompleteCharacterB + `', '师徒')`,
		`INSERT INTO character_relationships (relationship_id, source_character_id, target_character_id, relation)
 VALUES ('rel-sync-2', '` + productionDBSyncCompleteCharacterB + `', '` + productionDBSyncCompleteCharacterA + `', '师徒')`,
		`INSERT INTO blueprint_commit_operations
 (operation_id, payload_hash, mode, start_chapter, end_chapter, character_sync_input, committed_at)
 VALUES (?, ?, 'full', 1, 1, ?, '2024-05-06 07:08:09')`,
		`INSERT INTO blueprint_character_sync_operations
 (operation_id, blueprint_commit_operation_id, blueprint_commit_payload_hash, status,
  start_chapter, end_chapter, character_sync_input, created_at, updated_at)
 VALUES (?, ?, ?, 'pending', 1, 1, ?, '2024-05-06 07:08:09', ?)`,
	}
	arguments := [][]any{
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		{productionDBSyncCompleteCommitOperation, productionDBSyncCompleteCommitPayloadHash, productionDBSyncCompleteSyncInput},
		{productionDBSyncCompleteSyncOperation, productionDBSyncCompleteCommitOperation,
			productionDBSyncCompleteCommitPayloadHash, productionDBSyncCompleteSyncInput,
			productionDBSyncCompleteSeededUpdatedAt},
	}
	for index, statement := range statements {
		if _, err := db.Exec(statement, arguments[index]...); err != nil {
			_ = db.Close()
			t.Fatalf("seed statement %d: %v", index, err)
		}
	}
	// The projection refresh is the store's own writer, so the roster meta row and
	// the compatibility projection are self-consistent without the test inventing
	// a hash. It runs over the same file, before admission.
	if err := store.NewCharacters(db).RefreshIdentityProjection(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("normalize the seeded roster: %v", err)
	}
	meta := productionWriteQuery(t, database, `SELECT revision,projection_hash FROM character_roster_meta WHERE id='main'`)
	if len(meta) != 1 {
		_ = db.Close()
		t.Fatalf("the seeded roster meta row is missing: %v", meta)
	}
	revision, projectionHash := productionWriteNumber(meta[0], "revision"), productionWriteText(meta[0], "projection_hash")
	if revision < 1 || projectionHash == "" {
		_ = db.Close()
		t.Fatalf("the refreshed roster is not authoritative: revision %v, projection %v", revision, projectionHash)
	}
	if _, err := db.Exec(`INSERT INTO character_roster_operations (operation_id, payload_hash, committed_revision, projection_hash)
 VALUES (?, ?, ?, ?)`, productionDBSyncCompleteSyncOperation, productionDBSyncCompleteRosterPayloadHash,
		revision, projectionHash); err != nil {
		_ = db.Close()
		t.Fatalf("seed the roster operation receipt: %v", err)
	}
	// The lane admits the file itself, so the seeded writes must be checkpointed
	// into it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
}

// productionDBSyncCompleteFixture is one opened production lane with one opened
// project and the seeded sync row.
type productionDBSyncCompleteFixture struct {
	t                *testing.T
	projectPath      string
	database         string
	author           *productionDurableClient
	frozen           map[string]any
	durable          *productionDurableOpen
	rosterRevision   int64
	rosterProjection string
}

// newProductionDBSyncCompleteFixture builds the temporary data root, opens the
// real production runtime over it, binds this batch's single-name binder next to
// project:open and opens the one admitted project for one browser tab. Opening
// the project is what issues the lease every request below names.
func newProductionDBSyncCompleteFixture(t *testing.T) *productionDBSyncCompleteFixture {
	t.Helper()
	root := syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	schema := productionDBSyncCompleteSchema()
	seedProductionProject(t, library, productionDBSyncCompleteProjectName, productionDBSyncCompleteProjectID,
		admission.CurrentSchemaVersion, schema)
	projectPath := filepath.Join(library, productionDBSyncCompleteProjectName)
	productionDBSyncCompleteSeed(t, projectPath)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	meta := productionWriteQuery(t, database, `SELECT revision,projection_hash FROM character_roster_meta WHERE id='main'`)
	if len(meta) != 1 {
		t.Fatalf("the seeded roster meta row is missing: %v", meta)
	}
	contract := productionDBSyncContract(t, schema)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-db-sync-complete")
	if err := server.BindProjects(open.durable); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	if err := newProductionDBSyncComplete(open.durable).Bind(server); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	author := newProductionDurableClient(t, server, productionDBSyncCompleteTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, productionDBSyncCompleteOpenID, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted completion project: %v %v", opened.Success, opened.DBReady)
	}
	if opened.Project["id"] != productionDBSyncCompleteProjectID || opened.Project["path"] != projectPath {
		t.Fatalf("project:open returned the wrong project identity: %v", opened.Project)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return &productionDBSyncCompleteFixture{
		t:                t,
		projectPath:      projectPath,
		database:         database,
		author:           author,
		durable:          open.durable,
		rosterRevision:   productionWriteNumber(meta[0], "revision"),
		rosterProjection: productionWriteText(meta[0], "projection_hash"),
		frozen: map[string]any{
			"projectId":   opened.Project["id"],
			"projectPath": opened.Project["path"],
			"leaseId":     opened.Project["sessionLease"],
		},
	}
}

// call sends the reference's (operation id, project path, frozen context) shape.
func (f *productionDBSyncCompleteFixture) call(id, path string, frozen map[string]any) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.author.call(f.t, productionDBSyncCompleteName, []any{id, path, frozen})
}

// complete sends one completion with the live lease and returns the decoded
// business result. A refused request still answers 200 with the reference's
// {success:false,error} envelope, so a non-200 here would mean the binder was
// never reached.
func (f *productionDBSyncCompleteFixture) complete(id string) map[string]any {
	f.t.Helper()
	response := f.author.call(f.t, productionDBSyncCompleteName, []any{id, f.projectPath, f.frozen})
	if response.Code != http.StatusOK {
		f.t.Fatalf("%s: %d %s", productionDBSyncCompleteName, response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), f.projectPath) {
		f.t.Fatalf("%s echoed the project path", productionDBSyncCompleteName)
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		f.t.Fatalf("%s: %v %s", productionDBSyncCompleteName, err, response.Body.String())
	}
	return wire.Result
}

// exec writes one author-side statement through a separate handle on the same
// file and closes it again, so the refusal below starts from a file nobody holds
// a write lock on.
func (f *productionDBSyncCompleteFixture) exec(statement string, args ...any) {
	f.t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(f.database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		f.t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(statement, args...); err != nil {
		_ = db.Close()
		f.t.Fatalf("fixture write: %v", err)
	}
	if err := db.Close(); err != nil {
		f.t.Fatal(err)
	}
}

// productionDBSyncCompleteRow reads the completion columns straight out of the
// author's file, so a receipt counts as written only when the database says so
// and not when a handler echoed its own answer.
func productionDBSyncCompleteRow(t *testing.T, database string) map[string]any {
	t.Helper()
	rows := productionWriteQuery(t, database, `SELECT status,completion_receipt,completed_at,updated_at
 FROM blueprint_character_sync_operations WHERE operation_id=?`, productionDBSyncCompleteSyncOperation)
	if len(rows) != 1 {
		t.Fatalf("%s is not the one seeded sync row: %d rows", productionDBSyncCompleteSyncOperation, len(rows))
	}
	return rows[0]
}

// productionDBSyncCompleteAssertCommitted pins the receipt the reference writes:
// the operation it belongs to, the commit it completes, status 'committed', and a
// rosterReceipt that carries evidence only - the operation, the payload digest,
// the revision reached and idempotent:false.
func productionDBSyncCompleteAssertCommitted(t *testing.T, result map[string]any, revision int64) map[string]any {
	t.Helper()
	if result["success"] != true {
		t.Fatalf("%s did not report a completion: %v", productionDBSyncCompleteName, result)
	}
	operation, ok := result["operation"].(map[string]any)
	if !ok {
		t.Fatalf("%s answered no operation: %v", productionDBSyncCompleteName, result)
	}
	if got := syntheticString(operation["status"]); got != "completed" {
		t.Fatalf("%s returned status %v, want completed", productionDBSyncCompleteName, got)
	}
	if got := syntheticString(operation["operationId"]); got != productionDBSyncCompleteSyncOperation {
		t.Fatalf("%s returned operation %v, want the requested row", productionDBSyncCompleteName, got)
	}
	if got := syntheticString(operation["completedAt"]); got == "" {
		t.Fatalf("%s completed the row without a completed_at", productionDBSyncCompleteName)
	}
	receipt, ok := operation["completionReceipt"].(map[string]any)
	if !ok {
		t.Fatalf("%s returned no completion receipt: %v", productionDBSyncCompleteName, operation)
	}
	want := map[string]any{
		"blueprintCommitOperationId": productionDBSyncCompleteCommitOperation,
		"operationId":                productionDBSyncCompleteSyncOperation,
		"status":                     "committed",
		"rosterReceipt": map[string]any{
			"operationId": productionDBSyncCompleteSyncOperation,
			"payloadHash": productionDBSyncCompleteRosterPayloadHash,
			"revision":    float64(revision),
			"idempotent":  false,
		},
	}
	if !reflect.DeepEqual(receipt, want) {
		t.Fatalf("the completion receipt is %v, want %v", receipt, want)
	}
	return operation
}

// TestProductionDBSyncCompleteProvesTheRosterAndWritesItsReceipt is the half the
// name was refused for: the roster the fixture adopted is authoritative, the
// frozen facts still hold in it, the roster operation receipt proves the commit,
// and the completion lands in the file. The second completion is the reference's
// idempotent branch - the row is proved and returned, never rewritten.
func TestProductionDBSyncCompleteProvesTheRosterAndWritesItsReceipt(t *testing.T) {
	fixture := newProductionDBSyncCompleteFixture(t)
	first := fixture.complete(productionDBSyncCompleteSyncOperation)
	operation := productionDBSyncCompleteAssertCommitted(t, first, fixture.rosterRevision)

	row := productionDBSyncCompleteRow(t, fixture.database)
	if got := syntheticString(row["status"]); got != "completed" {
		t.Fatalf("the file holds status %v, want completed", got)
	}
	if got := syntheticString(row["completed_at"]); got == "" {
		t.Fatal("the file holds no completed_at, so no receipt was committed")
	}
	if got := syntheticString(row["updated_at"]); got == productionDBSyncCompleteSeededUpdatedAt {
		t.Fatalf("the file still holds the seeded updated_at %v, so nothing was written", got)
	}
	stored, ok := row["completion_receipt"].(string)
	if !ok || stored == "" {
		t.Fatalf("the file holds no completion receipt: %v", row["completion_receipt"])
	}
	// The receipt is the reference's own canonical bytes, not merely the same
	// fields: canonicalize sorts these keys as UTF-16 does (blueprintCommitOperationId,
	// operationId, rosterReceipt, status; idempotent, operationId, payloadHash,
	// revision inside the receipt), so JSON.stringify(canonicalize(receipt)) and this
	// port's encoding are the same text.
	wantStored := `{"blueprintCommitOperationId":"` + productionDBSyncCompleteCommitOperation +
		`","operationId":"` + productionDBSyncCompleteSyncOperation +
		`","rosterReceipt":{"idempotent":false,"operationId":"` + productionDBSyncCompleteSyncOperation +
		`","payloadHash":"` + productionDBSyncCompleteRosterPayloadHash +
		`","revision":` + strconv.FormatInt(fixture.rosterRevision, 10) +
		`},"status":"committed"}`
	if stored != wantStored {
		t.Fatalf("the file holds the receipt %v, want %v", stored, wantStored)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(stored), &decoded); err != nil {
		t.Fatalf("the stored receipt is not JSON: %v", err)
	}
	if !reflect.DeepEqual(decoded, operation["completionReceipt"]) {
		t.Fatalf("the file holds the receipt %v, the answer carried %v", decoded, operation["completionReceipt"])
	}

	// The reference's deciding lines (:773-775): a completed row is proved and
	// returned, not updated - so the second call answers the same receipt and
	// leaves every byte of the file alone.
	before := productionWriteSnapshot(t, fixture.database)
	firstCompletedAt := syntheticString(operation["completedAt"])
	second := fixture.complete(productionDBSyncCompleteSyncOperation)
	again := productionDBSyncCompleteAssertCommitted(t, second, fixture.rosterRevision)
	if got := syntheticString(again["completedAt"]); got != firstCompletedAt {
		t.Fatalf("the second completion moved completed_at from %v to %v", firstCompletedAt, got)
	}
	productionWriteAssertUnchanged(t, fixture.database, before, "the idempotent second completion")

	// The completed row is still the one this lane can prove: the roster operation
	// evidence is what makes the receipt 'committed' rather than 'already-satisfied'.
	evidence := productionWriteQuery(t, fixture.database, `SELECT committed_revision,projection_hash
 FROM character_roster_operations WHERE operation_id=?`, productionDBSyncCompleteSyncOperation)
	if len(evidence) != 1 {
		t.Fatalf("the roster operation receipt the proof read is missing: %d rows", len(evidence))
	}
	if got := productionWriteNumber(evidence[0], "committed_revision"); got != fixture.rosterRevision {
		t.Fatalf("the proof accepted revision %v, want the roster's %v", got, fixture.rosterRevision)
	}
	if got := productionWriteText(evidence[0], "projection_hash"); got != fixture.rosterProjection {
		t.Fatalf("the proof accepted projection %v, want the roster's %v", got, fixture.rosterProjection)
	}

	// The reference's other receipt (:387-393): when the roster carries the facts
	// but no committed roster operation recorded this sync, the completion still
	// succeeds and its receipt says only that - status 'already-satisfied' and no
	// rosterReceipt at all, because there is no commit to name.
	t.Run("without roster evidence", func(t *testing.T) {
		fresh := newProductionDBSyncCompleteFixture(t)
		fresh.exec(`DELETE FROM character_roster_operations`)
		result := fresh.complete(productionDBSyncCompleteSyncOperation)
		if result["success"] != true {
			t.Fatalf("%s refused a roster that already carries the facts: %v", productionDBSyncCompleteName, result)
		}
		operation, ok := result["operation"].(map[string]any)
		if !ok {
			t.Fatalf("%s answered no operation: %v", productionDBSyncCompleteName, result)
		}
		receipt, ok := operation["completionReceipt"].(map[string]any)
		if !ok {
			t.Fatalf("%s returned no completion receipt: %v", productionDBSyncCompleteName, operation)
		}
		want := map[string]any{
			"blueprintCommitOperationId": productionDBSyncCompleteCommitOperation,
			"operationId":                productionDBSyncCompleteSyncOperation,
			"status":                     "already-satisfied",
		}
		if !reflect.DeepEqual(receipt, want) {
			t.Fatalf("the completion receipt is %v, want %v", receipt, want)
		}
		if _, present := receipt["rosterReceipt"]; present {
			t.Fatalf("a receipt with no roster commit names one: %v", receipt)
		}
		row := productionDBSyncCompleteRow(t, fresh.database)
		if got := syntheticString(row["status"]); got != "completed" {
			t.Fatalf("the file holds status %v, want completed", got)
		}
	})
}

// TestProductionDBSyncCompleteRefusesANonAuthoritativeRoster is the refusal the
// row's own gate names: a roster whose derived status is not 'ready' or 'empty'
// (blueprint-repository.ts:377-379) is not authoritative, so the completion must
// answer that code and leave the file byte-identical with the row still pending.
func TestProductionDBSyncCompleteRefusesANonAuthoritativeRoster(t *testing.T) {
	fixture := newProductionDBSyncCompleteFixture(t)
	// The reference derives the status from the persistent migration state, not
	// from a caller's claim: 'legacy_markdown_pending' with preserved prose and no
	// matching projection is 'legacy_repair_required', which :377 refuses.
	fixture.exec(`UPDATE character_roster_meta
 SET migration_state='legacy_markdown_pending',legacy_markdown='旧的名单',projection_hash='',fact_hash=''
 WHERE id='main'`)
	before := productionWriteSnapshot(t, fixture.database)
	result := fixture.complete(productionDBSyncCompleteSyncOperation)
	if result["success"] != false {
		t.Fatalf("%s accepted a roster that is not authoritative: %v", productionDBSyncCompleteName, result)
	}
	if got := syntheticString(result["error"]); got != "Error: BLUEPRINT_CHARACTER_SYNC_ROSTER_NOT_READY" {
		t.Fatalf("%s refused with %v, want the roster-not-ready code", productionDBSyncCompleteName, got)
	}
	productionWriteAssertUnchanged(t, fixture.database, before, "the roster-not-ready refusal")
	row := productionDBSyncCompleteRow(t, fixture.database)
	if got := syntheticString(row["status"]); got != "pending" {
		t.Fatalf("the refused completion left the row at %v, want pending", got)
	}
	if row["completion_receipt"] != nil || row["completed_at"] != nil {
		t.Fatalf("the refused completion wrote a receipt: %v", row)
	}
}

// TestProductionDBSyncCompleteRefusesAnUncompletableRow covers the rows that can
// never be completed: one the ledger never minted, one whose id is blank (the
// reference's own pre-database check, :768) and one whose stored state is
// contradictory (a pending row that already carries a receipt, :322-328). None of
// them may reach a write.
func TestProductionDBSyncCompleteRefusesAnUncompletableRow(t *testing.T) {
	fixture := newProductionDBSyncCompleteFixture(t)
	for _, request := range []struct {
		label string
		id    string
		code  string
	}{
		{"an id the ledger never minted", "blueprint-sync-never-committed",
			"Error: BLUEPRINT_CHARACTER_SYNC_OPERATION_MISSING"},
		{"a blank id", "   ", "Error: BLUEPRINT_CHARACTER_SYNC_OPERATION_ID_REQUIRED"},
	} {
		before := productionWriteSnapshot(t, fixture.database)
		result := fixture.complete(request.id)
		if result["success"] != false {
			t.Fatalf("%s completed %s: %v", productionDBSyncCompleteName, request.label, result)
		}
		if got := syntheticString(result["error"]); got != request.code {
			t.Fatalf("%s refused %s with %v, want %v", productionDBSyncCompleteName, request.label, got, request.code)
		}
		productionWriteAssertUnchanged(t, fixture.database, before, request.label)
	}
	// A pending row that already carries a receipt is not complete-able either: the
	// row reader refuses the contradictory state before the proof runs.
	fixture.exec(`UPDATE blueprint_character_sync_operations SET completion_receipt='{"version":1}'
 WHERE operation_id=?`, productionDBSyncCompleteSyncOperation)
	before := productionWriteSnapshot(t, fixture.database)
	result := fixture.complete(productionDBSyncCompleteSyncOperation)
	if result["success"] != false {
		t.Fatalf("%s completed a row in a contradictory state: %v", productionDBSyncCompleteName, result)
	}
	if got := syntheticString(result["error"]); got != "Error: BLUEPRINT_CHARACTER_SYNC_STATE_INVALID" {
		t.Fatalf("%s refused the contradictory row with %v, want the state-invalid code", productionDBSyncCompleteName, got)
	}
	productionWriteAssertUnchanged(t, fixture.database, before, "the contradictory-row refusal")
}

// TestProductionDBSyncCompleteRefusesForeignProjectAndStaleLease pins the two
// admission refusals this name shares with its sibling reads: a context naming
// another project, and a context naming a lease this lane never issued. Both are
// decided before any SQL and both leave the file byte-identical.
func TestProductionDBSyncCompleteRefusesForeignProjectAndStaleLease(t *testing.T) {
	fixture := newProductionDBSyncCompleteFixture(t)
	foreignPath := filepath.Join(filepath.Dir(fixture.projectPath), "foreign-novel")
	foreign := map[string]any{
		"projectId":   fixture.frozen["projectId"],
		"projectPath": foreignPath,
		"leaseId":     fixture.frozen["leaseId"],
	}
	before := productionWriteSnapshot(t, fixture.database)
	response := fixture.call(productionDBSyncCompleteSyncOperation, foreignPath, foreign)
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "PROJECT_CONTEXT_CHANGED") {
		t.Fatalf("%s under a foreign project = %d, want the context-changed refusal",
			productionDBSyncCompleteName, response.Code)
	}
	productionWriteAssertUnchanged(t, fixture.database, before, "the foreign-project refusal")

	stale := map[string]any{
		"projectId":   fixture.frozen["projectId"],
		"projectPath": fixture.projectPath,
		"leaseId":     productionDBSyncCompleteStaleLease,
	}
	before = productionWriteSnapshot(t, fixture.database)
	productionDBSyncRefused(t, fixture.author, "stale lease", productionDBSyncCompleteName,
		[]any{productionDBSyncCompleteSyncOperation, fixture.projectPath, stale})
	productionWriteAssertUnchanged(t, fixture.database, before, "the stale-lease refusal")
	row := productionDBSyncCompleteRow(t, fixture.database)
	if got := syntheticString(row["status"]); got != "pending" {
		t.Fatalf("a refused request moved the row to %v", got)
	}
}

// TestProductionDBSyncCompleteBindsOneName pins the wiring and every reference
// line this port quotes. The binder must install exactly one inventoried name and
// refuse a nil bridge or a nil server instead of publishing a handler that can
// only fail; the quoted lines are what decide the answer, so a reference edit that
// removes one has to fail here instead of leaving a comment pointing at nothing.
func TestProductionDBSyncCompleteBindsOneName(t *testing.T) {
	fixture := newProductionDBSyncCompleteFixture(t)
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := newProductionDBSyncComplete(fixture.durable).Bind(server); err != nil {
		t.Fatalf("the single-name binder failed: %v", err)
	}
	names := server.BoundOperations()
	if len(names) != 1 || names[0] != productionDBSyncCompleteName {
		t.Fatalf("the binder installed %v, want exactly %v", names, productionDBSyncCompleteName)
	}
	if err := newProductionDBSyncComplete(nil).Bind(server); err == nil {
		t.Fatal("a nil bridge was published as a handler")
	}
	if err := newProductionDBSyncComplete(fixture.durable).Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}

	// The repository: the decision, the proof and the receipt the port writes.
	repository := productionDBSyncCompleteSource(t, "../../../electron/repositories/blueprint-repository.ts")
	for _, marker := range []string{
		// The line that decides idempotent-or-mismatch: a completed row is proved
		// and returned unchanged, and the proof is what can refuse it.
		"if (operation.status === 'completed') {",
		"assertAuthoritativeCharacterSyncCompletion(db, operation)",
		"if (JSON.stringify(canonicalize(operation.completionReceipt))",
		"BLUEPRINT_CHARACTER_SYNC_RECEIPT_MISMATCH",
		// The roster branch this port asserts.
		"const roster = CharacterRosterRepository.read()",
		"if (roster.status !== 'ready' && roster.status !== 'empty')",
		"BLUEPRINT_CHARACTER_SYNC_ROSTER_NOT_READY",
		"const factError = blueprintCharacterSyncFactError(operation.characterSyncInput, roster.entries)",
		"FROM character_roster_operations",
		"if (!rosterOperation) {",
		"'already-satisfied'",
		"rosterOperation.committed_revision === roster.revision",
		"status: 'committed',",
		// The writer: the CAS update and its read-back.
		"static completeCharacterSyncOperation(",
		"WHERE operation_id = ? AND status = 'pending'",
		"BLUEPRINT_CHARACTER_SYNC_READ_BACK_FAILED",
	} {
		if !productionSeamContains(repository, marker) {
			t.Fatalf("the reference no longer says %v, so this port is quoting nothing", marker)
		}
	}
	// The controller: the wire shape (id, path, context) and the envelope.
	controller := productionDBSyncCompleteSource(t, "../../../electron/controllers/db-controller.ts")
	for _, marker := range []string{
		"operations.handle('db:blueprint-character-sync-complete', async (",
		"_event, operationId: string, expectedProjectPath: string,",
		"operation: BlueprintRepository.completeCharacterSyncOperation(operationId),",
	} {
		if !productionSeamContains(controller, marker) {
			t.Fatalf("the controller no longer says %v, so this binder is guessing the wire", marker)
		}
	}
	// The inventory: the channel whose args make the arity and the path index this
	// file records (arity 3 with the frozen context last, path index 1, id 0).
	channels := productionDBSyncCompleteSource(t, "../../../src/shared/ipc-channels.ts")
	if !productionSeamContains(channels, "'db:blueprint-character-sync-complete': {") {
		t.Fatal("the channel is not inventoried, so an unbound answer would not be 501")
	}
	if _, ok := migrationRegistry(t).Operations[productionDBSyncCompleteName]; !ok {
		t.Fatalf("%v is not in the registry this lane loaded", productionDBSyncCompleteName)
	}
}

// productionDBSyncCompleteSource reads one reference file. The seam markers are
// compared with productionSeamContains, which ignores every whitespace character,
// so a reindented or rewrapped declaration still matches while a deleted or
// reworded one does not.
func productionDBSyncCompleteSource(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: %v", path, err)
	}
	return string(raw)
}
