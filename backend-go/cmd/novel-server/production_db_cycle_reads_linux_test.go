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

// The pending-recheck read below runs against a disposable t.TempDir() only. No
// real author data root is read or written.
const (
	productionDBCycleProjectName = "db-cycle-novel"
	productionDBCycleProjectID   = "e3c9f1a7-52d4-4b08-9a6e-7d1f0b8c2e35"
	productionDBCycleTab         = "client_1234567890"
	productionDBCycleOpenID      = "db-cycle-open-1"
	productionDBCycleStaleLease  = "db-cycle-stale-lease"

	// The three extra bodies the fixture stores. Draft 1 keeps
	// productionDurableSeed, so the shipped durable seeder and this fixture
	// cannot disagree about it.
	productionDBCycleDraft2 = "第一章合并稿：门后有风。"
	productionDBCycleDraft3 = "第三章合并稿：雨停了。"
	productionDBCycleDraft4 = "第二章草稿：未合并。"
)

// productionDBCycleTables is the durable fixture plus the two review tables the
// reference's pending-recheck query joins. The columns are the reference's own
// projection, nothing more.
func productionDBCycleTables() []string {
	return append([]string{
		`CREATE TABLE revisions (id INTEGER PRIMARY KEY, base_draft_id INTEGER NOT NULL,
 revision_index INTEGER NOT NULL, revision_type TEXT, status TEXT NOT NULL,
 merged_to_draft_id INTEGER, content_id INTEGER, word_count INTEGER,
 created_at TEXT, updated_at TEXT)`,
		`CREATE TABLE review_cycles (cycle_id TEXT PRIMARY KEY, root_action_id TEXT NOT NULL,
 review_id INTEGER NOT NULL, review_content_hash TEXT NOT NULL, source_hash TEXT NOT NULL,
 finding_set_hash TEXT NOT NULL, comparison_version INTEGER NOT NULL,
 revision_status TEXT NOT NULL, merged_hash TEXT, recheck_count INTEGER NOT NULL DEFAULT 0,
 recheck_attempt_id TEXT, revision_id INTEGER, created_at TEXT, updated_at TEXT)`,
	}, productionDurableSchema...)
}

// productionDBCycleContract is the trusted contract for the fixture schema. The
// verifier hashes sqlite_master in (type, name) order; every statement here is a
// table, so sorting by name is that same order.
func productionDBCycleContract(t *testing.T, statements []string) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(statements))
	for _, statement := range statements {
		name, _, found := strings.Cut(strings.TrimPrefix(statement, prefix), " ")
		if !found || name == "" || !strings.HasPrefix(statement, prefix) {
			t.Fatalf("unparseable schema statement: %s", statement)
		}
		entries = append(entries, productionSchemaEntry{Kind: "table", Name: name, Table: name, SQL: statement})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return admission.SchemaContract{{Version: admission.CurrentSchemaVersion, Fingerprints: []string{sha256Hex(string(encoded))}}}
}

// productionDBCycleRoot builds the disposable data root. The schema and the
// project are fixed here; the cycle rows are seeded per test, before the lane
// opens, because the admission permit hashes the file at open.
func productionDBCycleRoot(t *testing.T) (root, projectPath, database string, contract admission.SchemaContract) {
	t.Helper()
	root = syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	schema := productionDBCycleTables()
	seedProductionProject(t, library, productionDBCycleProjectName, productionDBCycleProjectID,
		admission.CurrentSchemaVersion, schema)
	projectPath = filepath.Join(library, productionDBCycleProjectName)
	seedProductionDurableRows(t, projectPath)
	database = filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	contract = productionDBCycleContract(t, schema)
	return root, projectPath, database, contract
}

// productionDBCycleLane opens the author runtime and mounts a transport server
// with project:open plus the one binder this batch adds. The binder line itself
// belongs in production_open_linux.go, which this batch may not edit; installing
// the same binder on the same server type through the same durable bridge is
// what lets the test run before that wiring lands.
func productionDBCycleLane(t *testing.T, root string, contract admission.SchemaContract) *transport.Server {
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
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-db-cycle")
	if err := server.BindProjects(open.durable); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	if err := newProductionDBCycleReads(open.durable).Bind(server); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	return server
}

// productionDBCycleOpen opens the fixture project and returns the frozen
// three-key context every request must carry.
func productionDBCycleOpen(t *testing.T, server *transport.Server, projectPath string) (*productionDurableClient, map[string]any) {
	t.Helper()
	client := newProductionDurableClient(t, server, productionDBCycleTab)
	var opened productionDurableOpenWire
	client.decode(t, "project:open", []any{projectPath, productionDBCycleOpenID, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the fixture project: %+v", opened)
	}
	if opened.Project["id"] != productionDBCycleProjectID || opened.Project["path"] != projectPath {
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

// productionDBCycleDB opens a direct handle on the fixture database for seeding.
func productionDBCycleDB(t *testing.T, database string) *sql.DB {
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

// productionDBCycleSeed writes nine cycles and the drafts and revisions they
// point at. Four survive the reference's WHERE clause and its current-merge
// proof: cycle-a and cycle-z (same draft and chapter), cycle-m (same chapter,
// higher draft id) and cycle-b (later chapter). The other five each fail one
// gate: a superseded revision, a merged hash that no longer describes the draft,
// a recheck already counted, a cycle that never reached merge-committed, and a
// draft that is not revised.
func productionDBCycleSeed(t *testing.T, database string) {
	t.Helper()
	db := productionDBCycleDB(t, database)
	first := sha256Hex(productionDurableSeed)
	second := sha256Hex(productionDBCycleDraft2)
	third := sha256Hex(productionDBCycleDraft3)
	fourth := sha256Hex(productionDBCycleDraft4)
	statements := []struct {
		query string
		args  []any
	}{
		{`UPDATE drafts SET status='revised' WHERE id=1`, nil},
		{`INSERT INTO contents (id,body) VALUES (2,?),(3,?),(4,?)`,
			[]any{productionDBCycleDraft2, productionDBCycleDraft3, productionDBCycleDraft4}},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (2, 1, 2, 'revised', 'write', 2, 12, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (3, 3, 1, 'revised', 'write', 3, 9, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (4, 2, 1, 'draft', 'write', 4, 8, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		// r10 is superseded by r11 on the same base draft; r11 is the newest
		// merged revision of draft 1 and therefore the only current one.
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, content_id, word_count)
 VALUES (10, 1, 1, 'review-fix', 'merged', 1, 1, 11)`, nil},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, content_id, word_count)
 VALUES (11, 1, 2, 'review-fix', 'merged', 1, 1, 12)`, nil},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, content_id, word_count)
 VALUES (12, 3, 1, 'review-fix', 'merged', 3, 3, 9)`, nil},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, content_id, word_count)
 VALUES (13, 4, 1, 'review-fix', 'merged', 4, 4, 8)`, nil},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, content_id, word_count)
 VALUES (14, 2, 1, 'review-fix', 'merged', 2, 2, 12)`, nil},
		// The four the query returns, in the order the reference's ORDER BY
		// requires: chapter 1 draft 1 cycle-a, chapter 1 draft 1 cycle-z,
		// chapter 1 draft 2 cycle-m, chapter 3 draft 3 cycle-b.
		{`INSERT INTO review_cycles (cycle_id, root_action_id, review_id, review_content_hash,
 source_hash, finding_set_hash, comparison_version, revision_status, merged_hash,
 recheck_count, revision_id)
 VALUES ('cycle-a', 'root-a', 100, 'review-a', 'source-a', 'findings-a', 1,
 'merge-committed', ?, 0, 11)`, []any{first}},
		{`INSERT INTO review_cycles (cycle_id, root_action_id, review_id, review_content_hash,
 source_hash, finding_set_hash, comparison_version, revision_status, merged_hash,
 recheck_count, revision_id)
 VALUES ('cycle-z', 'root-z', 101, 'review-z', 'source-z', 'findings-z', 1,
 'merge-committed', ?, 0, 11)`, []any{first}},
		{`INSERT INTO review_cycles (cycle_id, root_action_id, review_id, review_content_hash,
 source_hash, finding_set_hash, comparison_version, revision_status, merged_hash,
 recheck_count, revision_id)
 VALUES ('cycle-m', 'root-m', 102, 'review-m', 'source-m', 'findings-m', 1,
 'merge-committed', ?, 0, 14)`, []any{second}},
		{`INSERT INTO review_cycles (cycle_id, root_action_id, review_id, review_content_hash,
 source_hash, finding_set_hash, comparison_version, revision_status, merged_hash,
 recheck_count, revision_id)
 VALUES ('cycle-b', 'root-b', 103, 'review-b', 'source-b', 'findings-b', 1,
 'merge-committed', ?, 0, 12)`, []any{third}},
		// Dropped by isCurrentMergedCycle: r10 is not the newest merged revision.
		{`INSERT INTO review_cycles (cycle_id, root_action_id, review_id, review_content_hash,
 source_hash, finding_set_hash, comparison_version, revision_status, merged_hash,
 recheck_count, revision_id)
 VALUES ('cycle-old', 'root-old', 104, 'review-old', 'source-old', 'findings-old', 1,
 'merge-committed', ?, 0, 10)`, []any{first}},
		// Dropped by isCurrentMergedCycle: the hash no longer describes draft 1.
		{`INSERT INTO review_cycles (cycle_id, root_action_id, review_id, review_content_hash,
 source_hash, finding_set_hash, comparison_version, revision_status, merged_hash,
 recheck_count, revision_id)
 VALUES ('cycle-stale', 'root-stale', 105, 'review-stale', 'source-stale', 'findings-stale', 1,
 'merge-committed', ?, 0, 11)`, []any{strings.Repeat("f", 64)}},
		// Dropped by the WHERE clause: a recheck was already counted.
		{`INSERT INTO review_cycles (cycle_id, root_action_id, review_id, review_content_hash,
 source_hash, finding_set_hash, comparison_version, revision_status, merged_hash,
 recheck_count, revision_id)
 VALUES ('cycle-done', 'root-done', 106, 'review-done', 'source-done', 'findings-done', 1,
 'merge-committed', ?, 1, 12)`, []any{third}},
		// Dropped by the WHERE clause: not merge-committed.
		{`INSERT INTO review_cycles (cycle_id, root_action_id, review_id, review_content_hash,
 source_hash, finding_set_hash, comparison_version, revision_status, merged_hash,
 recheck_count, revision_id)
 VALUES ('cycle-gen', 'root-gen', 107, 'review-gen', 'source-gen', 'findings-gen', 1,
 'generated', ?, 0, 14)`, []any{second}},
		// Dropped by the WHERE clause: the merged-into draft is not revised.
		{`INSERT INTO review_cycles (cycle_id, root_action_id, review_id, review_content_hash,
 source_hash, finding_set_hash, comparison_version, revision_status, merged_hash,
 recheck_count, revision_id)
 VALUES ('cycle-draft', 'root-draft', 108, 'review-draft', 'source-draft', 'findings-draft', 1,
 'merge-committed', ?, 0, 13)`, []any{fourth}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed review cycle: %v", err)
		}
	}
}

// productionDBCycleBytes snapshots the whole database file.
func productionDBCycleBytes(t *testing.T, database string) []byte {
	t.Helper()
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// productionDBCycleUnchanged proves the call did not touch the author file.
func productionDBCycleUnchanged(t *testing.T, database string, before []byte, label string) {
	t.Helper()
	if after := productionDBCycleBytes(t, database); !bytes.Equal(after, before) {
		t.Fatalf("%s rewrote the admitted author database", label)
	}
}

// productionDBCycleRefused asserts a non-200 lease/context refusal.
func productionDBCycleRefused(t *testing.T, client *productionDurableClient, label string, args []any) {
	t.Helper()
	response := client.call(t, productionDBCyclePendingRechecksName, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%s: %s answered 200 under a refused lease: %s",
			label, productionDBCyclePendingRechecksName, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	switch body.Error.Code {
	case "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE", "PROJECT_NOT_OPEN",
		"AUTHOR_WRITE_STALE", "PROJECT_SESSION_REQUIRED", "PROJECT_SESSION_CONTEXT_REQUIRED",
		"PROJECT_CONTEXT_CHANGED":
	default:
		t.Fatalf("%s refused with %v, which is not a lease refusal: %s",
			label, body.Error.Code, response.Body.String())
	}
}

// productionDBCycleResult returns the result member of a 200 envelope.
func productionDBCycleResult(t *testing.T, response *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("decode operation result: %v", err)
	}
	return wire.Result
}

// TestProductionDBCyclePendingRechecksServesReferenceOrder drives the read over
// real HTTP through the frozen session. The fixture carries nine cycles, four of
// which survive every ported gate; the expected order exercises all three ORDER
// BY columns (chapter, draft id, cycle id), and every dropped cycle fails a named
// gate so a query that widened the predicate would be caught by the count.
//
// The read writes nothing: the file is byte-compared around the successful call
// and around both refusals.
func TestProductionDBCyclePendingRechecksServesReferenceOrder(t *testing.T) {
	root, projectPath, database, contract := productionDBCycleRoot(t)
	productionDBCycleSeed(t, database)
	server := productionDBCycleLane(t, root, contract)
	author, frozen := productionDBCycleOpen(t, server, projectPath)

	before := productionDBCycleBytes(t, database)
	response := author.call(t, productionDBCyclePendingRechecksName, []any{projectPath, frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", productionDBCyclePendingRechecksName, response.Code, response.Body.String())
	}
	var pending []map[string]any
	if err := json.Unmarshal(productionDBCycleResult(t, response), &pending); err != nil {
		t.Fatalf("%s: %v", productionDBCyclePendingRechecksName, err)
	}
	first := sha256Hex(productionDurableSeed)
	second := sha256Hex(productionDBCycleDraft2)
	third := sha256Hex(productionDBCycleDraft3)
	want := []struct {
		cycleID  string
		reviewID float64
		rootID   string
		merged   string
		draftID  float64
		chapter  float64
		version  float64
		status   string
		content  string
	}{
		{"cycle-a", 100, "root-a", first, 1, 1, 1, "revised", productionDurableSeed},
		{"cycle-z", 101, "root-z", first, 1, 1, 1, "revised", productionDurableSeed},
		{"cycle-m", 102, "root-m", second, 2, 1, 2, "revised", productionDBCycleDraft2},
		{"cycle-b", 103, "root-b", third, 3, 3, 1, "revised", productionDBCycleDraft3},
	}
	if len(pending) != len(want) {
		t.Fatalf("the pending-recheck read returned %d rows, want %d: %v", len(pending), len(want), pending)
	}
	for index, expected := range want {
		row := pending[index]
		if got := syntheticString(row["cycleId"]); got != expected.cycleID {
			t.Fatalf("row %d cycleId = %v, want %v (ORDER BY d.chapter_number,d.id,rc.cycle_id)",
				index, got, expected.cycleID)
		}
		if got, _ := row["reviewId"].(float64); got != expected.reviewID {
			t.Fatalf("row %d reviewId = %v, want %v", index, got, expected.reviewID)
		}
		if got := syntheticString(row["rootActionId"]); got != expected.rootID {
			t.Fatalf("row %d rootActionId = %v, want %v", index, got, expected.rootID)
		}
		if got := syntheticString(row["mergedHash"]); got != expected.merged {
			t.Fatalf("row %d mergedHash = %v, want %v", index, got, expected.merged)
		}
		if got := syntheticString(row["disposition"]); got != "required" {
			t.Fatalf("row %d disposition = %v, want required", index, got)
		}
		draft, ok := row["sourceDraft"].(map[string]any)
		if !ok {
			t.Fatalf("row %d has no sourceDraft: %v", index, row)
		}
		for _, field := range []struct {
			key      string
			expected float64
		}{
			{"id", expected.draftID}, {"chapterNumber", expected.chapter}, {"version", expected.version},
		} {
			if got, _ := draft[field.key].(float64); got != field.expected {
				t.Fatalf("row %d sourceDraft.%s = %v, want %v", index, field.key, got, field.expected)
			}
		}
		if got := syntheticString(draft["status"]); got != expected.status {
			t.Fatalf("row %d sourceDraft.status = %v, want %v", index, got, expected.status)
		}
		if got := syntheticString(draft["content"]); got != expected.content {
			t.Fatalf("row %d sourceDraft.content = %v, want %v", index, got, expected.content)
		}
	}
	// Every dropped cycle must be absent, each for its own gate.
	dropped := map[string]bool{"cycle-old": true, "cycle-stale": true, "cycle-done": true,
		"cycle-gen": true, "cycle-draft": true}
	for _, row := range pending {
		if dropped[syntheticString(row["cycleId"])] {
			t.Fatalf("a cycle that fails a reference gate was projected: %v", row["cycleId"])
		}
	}
	productionDBCycleUnchanged(t, database, before, "the pending-recheck read")

	// Refusals: a foreign project's path and a lease this lane never issued.
	foreign := filepath.Join(root, productionLibraryDirectory, "foreign-novel")
	foreignFrozen := map[string]any{
		"projectId": frozen["projectId"], "projectPath": foreign, "leaseId": frozen["leaseId"],
	}
	stale := map[string]any{
		"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": productionDBCycleStaleLease,
	}
	for _, envelope := range []struct {
		label string
		path  string
		ctx   map[string]any
	}{
		{"foreign project", foreign, foreignFrozen},
		{"stale lease", projectPath, stale},
	} {
		snapshot := productionDBCycleBytes(t, database)
		productionDBCycleRefused(t, author, envelope.label, []any{envelope.path, envelope.ctx})
		productionDBCycleUnchanged(t, database, snapshot, "the refused pending-recheck read")
	}
}
