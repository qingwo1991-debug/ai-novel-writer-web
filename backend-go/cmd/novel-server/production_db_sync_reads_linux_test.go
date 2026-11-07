//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The two character-sync reads below run against a disposable t.TempDir() only.
// No real author data root is read or written: the projects, their blueprint
// commit ledgers and their lazy-repair rows live inside the test's temporary
// tree.
const (
	productionDBSyncProjectName = "db-sync-novel"
	productionDBSyncProjectID   = "b6f2c4e8-1a37-4d90-9e52-8c7f3a1d5b04"
	productionDBSyncBrokenName  = "db-sync-broken"
	productionDBSyncBrokenID    = "d1a7e3c9-5b42-4f86-a0d3-7e9c2b5f8146"
	productionDBSyncTab         = "client_1234567890"
	productionDBSyncOpenID      = "db-sync-open-1"
	productionDBSyncAuthorSync  = "blueprint-sync-op-z"
	productionDBSyncDoneSync    = "blueprint-sync-op-done"
	productionDBSyncStaleLease  = "db-sync-stale-lease"
	productionDBSyncAuthorInput = `[{"characterId":"author-owned"}]`
)

// productionDBSyncTrigger refuses every insert into the sync table. It is the
// only way the fixture can make the lazy repair fail inside an admitted file:
// INSERT OR IGNORE suppresses uniqueness and CHECK resolutions, but a trigger's
// RAISE(ABORT) is a statement error, so the store's ExecContext really fails.
const productionDBSyncTrigger = "CREATE TRIGGER refuse_sync_repair BEFORE INSERT ON blueprint_character_sync_operations BEGIN SELECT RAISE(ABORT,'sync repair refused'); END"

// productionDBSyncTables is the durable fixture plus the two blueprint ledgers
// the reference's lazy repair reads and writes. The DDL is written the way the
// shipped schema writes it, including the commit-ledger foreign key, because the
// admission fingerprint hashes the stored sqlite_master rows.
func productionDBSyncTables() []string {
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
 completed_at TEXT DEFAULT NULL,
 FOREIGN KEY (blueprint_commit_operation_id)
  REFERENCES blueprint_commit_operations(operation_id) ON DELETE CASCADE)`,
	}, productionDurableSchema...)
}

// productionDBSyncContract is the trusted contract for one or more fixture
// schemas. The verifier hashes sqlite_master in (type, name) order, so entries
// are sorted the same way; a trigger's tbl_name is the table it targets, not the
// trigger's own name.
func productionDBSyncContract(t *testing.T, schemas ...[]string) admission.SchemaContract {
	t.Helper()
	var fingerprints []string
	for _, statements := range schemas {
		entries := make([]productionSchemaEntry, 0, len(statements))
		for _, statement := range statements {
			kind := ""
			rest := ""
			switch {
			case strings.HasPrefix(statement, "CREATE TABLE "):
				kind, rest = "table", strings.TrimPrefix(statement, "CREATE TABLE ")
			case strings.HasPrefix(statement, "CREATE TRIGGER "):
				kind, rest = "trigger", strings.TrimPrefix(statement, "CREATE TRIGGER ")
			case strings.HasPrefix(statement, "CREATE INDEX "):
				kind, rest = "index", strings.TrimPrefix(statement, "CREATE INDEX ")
			default:
				t.Fatalf("unparseable schema statement: %s", statement)
			}
			name, _, found := strings.Cut(rest, " ")
			if !found || name == "" {
				t.Fatalf("unparseable schema statement: %s", statement)
			}
			target := name
			if kind == "trigger" {
				_, after, ok := strings.Cut(statement, " ON ")
				target, _, _ = strings.Cut(after, " ")
				if !ok || target == "" {
					t.Fatalf("trigger without an ON target: %s", statement)
				}
			}
			entries = append(entries, productionSchemaEntry{Kind: kind, Name: name, Table: target, SQL: statement})
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Kind != entries[j].Kind {
				return entries[i].Kind < entries[j].Kind
			}
			return entries[i].Name < entries[j].Name
		})
		encoded, err := json.Marshal(entries)
		if err != nil {
			t.Fatal(err)
		}
		fingerprints = append(fingerprints, sha256Hex(string(encoded)))
	}
	return admission.SchemaContract{{Version: admission.CurrentSchemaVersion, Fingerprints: fingerprints}}
}

// productionDBSyncRoot builds the disposable data root: the good project, the
// same project plus the refusing trigger, and the shipped core row in both. The
// ledger rows are seeded per test, before the lane opens, because the admission
// permit hashes the file at open.
func productionDBSyncRoot(t *testing.T) (root, projectPath, brokenPath, database, brokenDatabase string, contract admission.SchemaContract) {
	t.Helper()
	root = syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	tables := productionDBSyncTables()
	broken := append(append([]string{}, tables...), productionDBSyncTrigger)
	seedProductionProject(t, library, productionDBSyncProjectName, productionDBSyncProjectID,
		admission.CurrentSchemaVersion, tables)
	seedProductionProject(t, library, productionDBSyncBrokenName, productionDBSyncBrokenID,
		admission.CurrentSchemaVersion, broken)
	projectPath = filepath.Join(library, productionDBSyncProjectName)
	brokenPath = filepath.Join(library, productionDBSyncBrokenName)
	seedProductionDurableRows(t, projectPath)
	seedProductionDurableRows(t, brokenPath)
	database = filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	brokenDatabase = filepath.Join(brokenPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	contract = productionDBSyncContract(t, tables, broken)
	return root, projectPath, brokenPath, database, brokenDatabase, contract
}

// productionDBSyncLane opens the real author runtime over the fixture and
// mounts the transport server the browser talks to. It binds project:open (the
// only way to obtain the frozen lease) and the two binders this batch adds. The
// binder lines themselves belong in production_open_linux.go, which this batch
// may not edit; installing the same binders on the same server type through the
// same durable bridge is what lets these tests run before that wiring lands.
func productionDBSyncLane(t *testing.T, root string, contract admission.SchemaContract) *transport.Server {
	t.Helper()
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
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-db-sync")
	if err := server.BindProjects(open.durable); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	if err := newProductionDBSyncReads(open.durable).Bind(server); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	return server
}

// productionDBSyncOpen opens one project and returns the frozen three-key
// context every request must carry. The lease is issued by the lane, not
// invented here.
func productionDBSyncOpen(t *testing.T, server *transport.Server, projectPath, projectID string) (*productionDurableClient, map[string]any) {
	t.Helper()
	client := newProductionDurableClient(t, server, productionDBSyncTab)
	var opened productionDurableOpenWire
	client.decode(t, "project:open", []any{projectPath, productionDBSyncOpenID, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	if opened.Project["id"] != projectID || opened.Project["path"] != projectPath {
		t.Fatalf("project:open returned the wrong identity: %v", opened.Project)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return client, map[string]any{
		"projectId": opened.Project["id"], "projectPath": opened.Project["path"], "leaseId": lease,
	}
}

// productionDBSyncDB opens a direct handle on the fixture database for seeding
// and for proving what the lane persisted. The lane keeps its own handle; this
// one is closed with the test.
func productionDBSyncDB(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// productionDBSyncSeedLedger writes the commit rows and the one author-written
// sync row. The author row's created_at is in the far future on purpose: the
// reference orders by created_at first, so a list that sorted by operation_id
// alone would put it first and fail.
func productionDBSyncSeedLedger(t *testing.T, database string) {
	t.Helper()
	db := productionDBSyncDB(t, database)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO blueprint_commit_operations
 (operation_id, payload_hash, mode, start_chapter, end_chapter, character_sync_input, committed_at)
 VALUES ('op-a', 'hash-a', 'full', 1, 2, '[{"characterId":"char-a"}]', '2024-01-01 00:00:00')`, nil},
		{`INSERT INTO blueprint_commit_operations
 (operation_id, payload_hash, mode, start_chapter, end_chapter, character_sync_input, committed_at)
 VALUES ('op-b', 'hash-b', 'replace-range', 2, 3, '[{"characterId":"char-b"}]', '2024-01-02 00:00:00')`, nil},
		{`INSERT INTO blueprint_commit_operations
 (operation_id, payload_hash, mode, start_chapter, end_chapter, character_sync_input, committed_at)
 VALUES ('op-z', 'hash-z', 'full', 1, 1, '[{"characterId":"char-z"}]', '2024-01-03 00:00:00')`, nil},
		{`INSERT INTO blueprint_commit_operations
 (operation_id, payload_hash, mode, start_chapter, end_chapter, character_sync_input, committed_at)
 VALUES ('op-done', 'hash-done', 'full', 1, 1, '[{"characterId":"char-done"}]', '2024-01-04 00:00:00')`, nil},
		{`INSERT INTO blueprint_character_sync_operations
 (operation_id, blueprint_commit_operation_id, blueprint_commit_payload_hash, status,
  start_chapter, end_chapter, character_sync_input, created_at, updated_at)
 VALUES (?, 'op-z', 'hash-z', 'pending', 1, 1, ?, '2999-01-01 00:00:00', '1999-12-31 23:59:59')`,
			[]any{productionDBSyncAuthorSync, productionDBSyncAuthorInput}},
		{`INSERT INTO blueprint_character_sync_operations
 (operation_id, blueprint_commit_operation_id, blueprint_commit_payload_hash, status,
  start_chapter, end_chapter, character_sync_input, completion_receipt,
  created_at, updated_at, completed_at)
 VALUES (?, 'op-done', 'hash-done', 'completed', 1, 1, '[]', '{"version":1}',
  '2024-02-02 00:00:00', '2024-02-02 00:00:00', '2024-02-02 00:00:00')`,
			[]any{productionDBSyncDoneSync}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed ledger: %v", err)
		}
	}
}

// productionDBSyncCount reads one scalar straight out of the fixture file.
func productionDBSyncCount(t *testing.T, database, query string, args ...any) int64 {
	t.Helper()
	db := productionDBSyncDB(t, database)
	var count int64
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return count
}

// productionDBSyncAuthorRow reads back the author-written row, so a repair that
// overwrote it fails on the value rather than only on a count.
func productionDBSyncAuthorRow(t *testing.T, database string) (input, updatedAt string) {
	t.Helper()
	db := productionDBSyncDB(t, database)
	if err := db.QueryRow(`SELECT character_sync_input,updated_at FROM blueprint_character_sync_operations
 WHERE operation_id=?`, productionDBSyncAuthorSync).Scan(&input, &updatedAt); err != nil {
		t.Fatalf("read author sync row: %v", err)
	}
	return input, updatedAt
}

// productionDBSyncBytes snapshots the whole database file.
func productionDBSyncBytes(t *testing.T, database string) []byte {
	t.Helper()
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// productionDBSyncUnchanged proves a refused call did not touch the author file.
func productionDBSyncUnchanged(t *testing.T, database string, before []byte, label string) {
	t.Helper()
	if after := productionDBSyncBytes(t, database); !bytes.Equal(after, before) {
		t.Fatalf("%s rewrote the admitted author database", label)
	}
}

// productionDBSyncRefused asserts a non-200 refusal whose code is one of the
// lease/context refusals the borrow can raise. It never pins a single code,
// because a stale lease, a foreign path and an unowned project are all honest
// refusals and none of them may answer with data.
func productionDBSyncRefused(t *testing.T, client *productionDurableClient, label, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%s: %s answered 200 under a refused lease: %s", label, operation, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %s: %v %s", label, operation, err, response.Body.String())
	}
	switch body.Error.Code {
	case "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE", "PROJECT_NOT_OPEN",
		"AUTHOR_WRITE_STALE", "PROJECT_SESSION_REQUIRED", "PROJECT_SESSION_CONTEXT_REQUIRED",
		"PROJECT_CONTEXT_CHANGED":
	default:
		t.Fatalf("%s: %s refused with %v, which is not a lease refusal: %s",
			label, operation, body.Error.Code, response.Body.String())
	}
}

// TestProductionDBSyncListPendingRepairsOnceAndOrders is the pending-list half:
// two commit rows with no sync row are backfilled by the first call, the second
// call does not duplicate them, the author-written row survives untouched, and
// the answer is in created_at then operation_id order.
func TestProductionDBSyncListPendingRepairsOnceAndOrders(t *testing.T) {
	root, projectPath, _, database, _, contract := productionDBSyncRoot(t)
	productionDBSyncSeedLedger(t, database)
	server := productionDBSyncLane(t, root, contract)
	author, frozen := productionDBSyncOpen(t, server, projectPath, productionDBSyncProjectID)

	if before := productionDBSyncCount(t, database, "SELECT COUNT(*) FROM blueprint_character_sync_operations"); before != 2 {
		t.Fatalf("fixture starts with %d sync rows, want the two author-written ones", before)
	}
	first := author.call(t, productionDBSyncPendingName, []any{projectPath, frozen})
	if first.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", productionDBSyncPendingName, first.Code, first.Body.String())
	}
	var pending []map[string]any
	if err := json.Unmarshal(productionDBSyncResult(t, first), &pending); err != nil {
		t.Fatalf("%s: %v", productionDBSyncPendingName, err)
	}
	// op-a and op-b were repaired by this call; op-z is the author's row. The
	// repaired rows share one created_at, so operation_id breaks the tie, and
	// op-z's 2999 timestamp puts it last despite sorting first by id.
	wantOrder := []string{"blueprint-sync-op-a", "blueprint-sync-op-b", productionDBSyncAuthorSync}
	if len(pending) != len(wantOrder) {
		t.Fatalf("the first pending read returned %d rows, want %d: %v", len(pending), len(wantOrder), pending)
	}
	for index, want := range wantOrder {
		if got := syntheticString(pending[index]["operationId"]); got != want {
			t.Fatalf("pending row %d is %v, want %v (created_at then operation_id order)", index, got, want)
		}
		if got := syntheticString(pending[index]["status"]); got != "pending" {
			t.Fatalf("pending row %d has status %v, want pending", index, got)
		}
	}
	if input, ok := pending[2]["characterSyncInput"].([]any); !ok || len(input) != 1 {
		t.Fatalf("the author row's input was not projected: %v", pending[2]["characterSyncInput"])
	}
	if after := productionDBSyncCount(t, database, "SELECT COUNT(*) FROM blueprint_character_sync_operations"); after != 4 {
		t.Fatalf("the first call left %d sync rows, want 4 (two repaired, two written)", after)
	}
	input, updatedAt := productionDBSyncAuthorRow(t, database)
	if input != productionDBSyncAuthorInput || updatedAt != "1999-12-31 23:59:59" {
		t.Fatalf("the repair overwrote the author row: input %v, updated_at %v", input, updatedAt)
	}

	// The second call is a no-op repair: same rows, same order, no duplicate.
	second := author.call(t, productionDBSyncPendingName, []any{projectPath, frozen})
	if second.Code != http.StatusOK {
		t.Fatalf("%s second call: %d %s", productionDBSyncPendingName, second.Code, second.Body.String())
	}
	var again []map[string]any
	if err := json.Unmarshal(productionDBSyncResult(t, second), &again); err != nil {
		t.Fatalf("%s second call: %v", productionDBSyncPendingName, err)
	}
	if len(again) != len(pending) {
		t.Fatalf("the second pending read returned %d rows, want %d", len(again), len(pending))
	}
	if after := productionDBSyncCount(t, database, "SELECT COUNT(*) FROM blueprint_character_sync_operations"); after != 4 {
		t.Fatalf("the second call duplicated rows: %d sync rows, want 4", after)
	}
	// Refusals: a foreign project's path and a lease this lane never issued. Both
	// are decided before any SQL and must leave the file byte-identical.
	foreign := filepath.Join(root, productionLibraryDirectory, "foreign-novel")
	foreignFrozen := map[string]any{"projectId": frozen["projectId"], "projectPath": foreign, "leaseId": frozen["leaseId"]}
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": productionDBSyncStaleLease}
	for _, envelope := range []struct {
		label string
		path  string
		ctx   map[string]any
	}{
		{"foreign project", foreign, foreignFrozen},
		{"stale lease", projectPath, stale},
	} {
		before := productionDBSyncBytes(t, database)
		productionDBSyncRefused(t, author, envelope.label, productionDBSyncPendingName, []any{envelope.path, envelope.ctx})
		productionDBSyncUnchanged(t, database, before, "the refused pending read")
	}
}

// TestProductionDBSyncGetRepairsOnceAndReadsOneRow is the single-row half: the
// database holds more than one sync row, the call still selects exactly the
// requested one, its first call repairs the missing row, its second call does
// not duplicate it, an absent id stays the reference's null and a completed row
// keeps the symbolic refusal this lane cannot yet lift.
func TestProductionDBSyncGetRepairsOnceAndReadsOneRow(t *testing.T) {
	root, projectPath, _, database, _, contract := productionDBSyncRoot(t)
	productionDBSyncSeedLedger(t, database)
	server := productionDBSyncLane(t, root, contract)
	author, frozen := productionDBSyncOpen(t, server, projectPath, productionDBSyncProjectID)

	if before := productionDBSyncCount(t, database, "SELECT COUNT(*) FROM blueprint_character_sync_operations"); before != 2 {
		t.Fatalf("fixture starts with %d sync rows, want the two author-written ones", before)
	}
	args := func(id string, ctx map[string]any) []any {
		return []any{id, projectPath, ctx}
	}
	first := author.call(t, productionDBSyncGetName, args("blueprint-sync-op-b", frozen))
	if first.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", productionDBSyncGetName, first.Code, first.Body.String())
	}
	var operation map[string]any
	if err := json.Unmarshal(productionDBSyncResult(t, first), &operation); err != nil {
		t.Fatalf("%s: %v", productionDBSyncGetName, err)
	}
	if got := syntheticString(operation["operationId"]); got != "blueprint-sync-op-b" {
		t.Fatalf("get returned %v, want the requested row", got)
	}
	if got := syntheticString(operation["blueprintCommitOperationId"]); got != "op-b" {
		t.Fatalf("get returned commit operation %v, want op-b", got)
	}
	if got := syntheticString(operation["status"]); got != "pending" {
		t.Fatalf("get returned status %v, want pending", got)
	}
	input, ok := operation["characterSyncInput"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("get returned the wrong input: %v", operation["characterSyncInput"])
	}
	// The repair created both missing rows; selecting one of them is only a real
	// statement because the other exists.
	if after := productionDBSyncCount(t, database, "SELECT COUNT(*) FROM blueprint_character_sync_operations"); after != 4 {
		t.Fatalf("the get repair left %d sync rows, want 4", after)
	}

	second := author.call(t, productionDBSyncGetName, args("blueprint-sync-op-b", frozen))
	if second.Code != http.StatusOK {
		t.Fatalf("%s second call: %d %s", productionDBSyncGetName, second.Code, second.Body.String())
	}
	if after := productionDBSyncCount(t, database, "SELECT COUNT(*) FROM blueprint_character_sync_operations"); after != 4 {
		t.Fatalf("the second get duplicated rows: %d sync rows, want 4", after)
	}
	authorInput, authorUpdatedAt := productionDBSyncAuthorRow(t, database)
	if authorInput != productionDBSyncAuthorInput || authorUpdatedAt != "1999-12-31 23:59:59" {
		t.Fatalf("the get repair overwrote the author row: input %v, updated_at %v", authorInput, authorUpdatedAt)
	}

	// A missing id is the reference's null, not an error and not a fabricated row.
	var absent map[string]any
	author.decode(t, productionDBSyncGetName, args("blueprint-sync-op-absent", frozen), &absent)
	if absent != nil {
		t.Fatalf("an absent id returned a row: %v", absent)
	}

	// The completed row stays refused: this lane has no authoritative roster
	// proof, and answering the raw receipt would be worse than the refusal.
	completed := author.call(t, productionDBSyncGetName, args(productionDBSyncDoneSync, frozen))
	if completed.Code == http.StatusOK || !strings.Contains(completed.Body.String(), "GENERATION_OWNER_REQUIRED") {
		t.Fatalf("a completed row was not refused with GENERATION_OWNER_REQUIRED: %d %s",
			completed.Code, completed.Body.String())
	}

	foreign := filepath.Join(root, productionLibraryDirectory, "foreign-novel")
	foreignFrozen := map[string]any{"projectId": frozen["projectId"], "projectPath": foreign, "leaseId": frozen["leaseId"]}
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": productionDBSyncStaleLease}
	for _, envelope := range []struct {
		label string
		path  string
		ctx   map[string]any
	}{
		{"foreign project", foreign, foreignFrozen},
		{"stale lease", projectPath, stale},
	} {
		before := productionDBSyncBytes(t, database)
		productionDBSyncRefused(t, author, envelope.label, productionDBSyncGetName,
			[]any{"blueprint-sync-op-a", envelope.path, envelope.ctx})
		productionDBSyncUnchanged(t, database, before, "the refused single-row read")
	}
}

// TestProductionDBSyncRepairFailureIsNotAnEmptyList pins the safety property the
// previous pass was right to care about: when the repair itself fails, the read
// must not answer 200 with an empty list. The fixture's trigger aborts every
// insert, so the store's ExecContext fails before either SELECT; the lane has to
// report a symbolic failure and the file must hold no sync row afterwards.
func TestProductionDBSyncRepairFailureIsNotAnEmptyList(t *testing.T) {
	root, _, brokenPath, _, brokenDatabase, contract := productionDBSyncRoot(t)
	db := productionDBSyncDB(t, brokenDatabase)
	if _, err := db.Exec(`INSERT INTO blueprint_commit_operations
 (operation_id, payload_hash, mode, start_chapter, end_chapter, character_sync_input, committed_at)
 VALUES ('op-broken', 'hash-broken', 'full', 1, 1, '[]', '2024-01-01 00:00:00')`); err != nil {
		t.Fatalf("seed the broken project ledger: %v", err)
	}
	server := productionDBSyncLane(t, root, contract)
	author, frozen := productionDBSyncOpen(t, server, brokenPath, productionDBSyncBrokenID)

	for _, call := range []struct {
		operation string
		args      []any
	}{
		{productionDBSyncPendingName, []any{brokenPath, frozen}},
		{productionDBSyncGetName, []any{"blueprint-sync-op-broken", brokenPath, frozen}},
	} {
		response := author.call(t, call.operation, call.args)
		if response.Code == http.StatusOK {
			t.Fatalf("%s answered 200 after the repair failed: %s", call.operation, response.Body.String())
		}
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v %s", call.operation, err, response.Body.String())
		}
		if body.Error.Code != "BLUEPRINT_OPERATION_FAILED" {
			t.Fatalf("%s reported %v after the repair failed, want BLUEPRINT_OPERATION_FAILED: %s",
				call.operation, body.Error.Code, response.Body.String())
		}
	}
	if rows := productionDBSyncCount(t, brokenDatabase, "SELECT COUNT(*) FROM blueprint_character_sync_operations"); rows != 0 {
		t.Fatalf("the refused repair left %d sync rows, want none", rows)
	}
}

// productionDBSyncResult returns the result member of a 200 envelope.
func productionDBSyncResult(t *testing.T, response *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("decode operation result: %v", err)
	}
	return wire.Result
}
