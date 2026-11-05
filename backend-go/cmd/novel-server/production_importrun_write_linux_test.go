//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The seven `db:import-run-*` bookkeeping writes below run against a disposable
// t.TempDir() only: the writer lease, the admitted project database and every
// readback live inside the test's own temporary tree.
//
// The fixture is the original-v7 shape the other production write batches build
// (production_blueprint_write_linux_test.go:49-213): an admitted library project
// whose schema is the smallest set of tables these writes really touch
// (import_runs and its three companions plus the effect ledger), seeded before the
// lane opens so the admitted content identity covers the rows, and read back
// afterwards over a SEPARATE mode=ro connection so a commit is only persisted when
// the file the lane wrote says so.
//
// # Why this test builds the lane itself
//
// productionHandlerWithContract (production_open_linux.go:458) is the sibling
// batches' seam, but it binds the names production_open_linux.go's own
// `for _, bind := range []func() error{` slice names, and this batch is not
// allowed to edit that file. The seven names are therefore installed on a
// transport.Server this test constructs over the SAME openProduction result and
// the SAME durable bridge, exactly the way
// synthetic_grant_open_http_linux_test.go:88-92 mounts a test-only binder. Nothing
// else is bound but project:open, so an accidentally unbound name answers 501
// GO_OPERATION_NOT_MIGRATED and the assertions below fail instead of passing on an
// empty envelope.
//
// # What each subtest proves
//
// There is ONE subtest per operation - never a loop over the names - so a failure
// names the operation that moved. Every subtest commits through the real HTTP
// handler, then READS the run/chapter/ledger rows back, then asserts the store's
// OWN precondition code for that transition on a refused second call and proves
// the refusal left the database byte-identical. One extra subtest drives all seven
// names with a stale lane lease: no live lease means no repository, and no row may
// move.
const (
	productionImportRunWriteProjectName = "import-run-writes-novel"
	productionImportRunWriteProjectID   = "5d7e2a91-3c48-4b06-9f1d-8a6b0e2c74d3"
	productionImportRunWriteTab         = "client_import_writes01"
	productionImportRunWriteOwner       = "pipeline-owner-1"
	productionImportRunWriteStaleLease  = "stale-import-run-lease-0001"
	// The seeded run ids. Nothing here is derived from a guess about the store: the
	// assertions compare the rows the fixture committed with the rows a served
	// write left behind.
	productionImportRunWriteStartID   = "import-run-write-start"
	productionImportRunWriteLiveID    = "import-run-write-live"
	productionImportRunWriteSettledID = "import-run-write-settled"
	productionImportRunWriteSiblingID = "import-run-write-sibling"
	// productionImportRunWriteChapters is the run's own chapter count, so
	// db:import-run-complete has something real to retire.
	productionImportRunWriteChapters = int64(2)
)

// productionImportRunWriteSchema is the smallest author schema these seven writes
// really touch. Every statement is written exactly as it must appear in
// sqlite_master, because the pinned contract hashes that text, and
// productionImportRunWriteContract builds the fingerprint from these same strings
// instead of trusting a hand-written digest.
//
// The DDL deliberately contains none of the characters the schema-contract
// encoder escapes: the contract builder marshals these strings with
// encoding/json (which HTML-escapes <, > and &), while the verifier re-encodes
// sqlite_master with its own JavaScript-compatible encoder
// (internal/admission/snapshot.go:234-256) that does not. A statement containing
// `<>` would therefore never match its own fingerprint; a comparison written into
// this fixture uses `!=` (or is avoided entirely).
//
// import_runs carries every column importRunRowColumns reads
// (internal/store/importrun.go:114-119), NOT NULL with defaults so the seed only
// has to name the columns a scenario really depends on: a NULL scanned into an
// int64 or a string is a scan error, not a refusal, and would make a "precondition
// refused" assertion pass for the wrong reason.
var productionImportRunWriteSchema = []string{
	"CREATE TABLE project_core (id TEXT PRIMARY KEY, project_name TEXT, writing_language TEXT, genre TEXT, sub_genre TEXT, target_audience TEXT, total_chapters INTEGER, words_per_chapter INTEGER, creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT, narrative_pov TEXT, core_outline TEXT, world_setting TEXT, golden_finger TEXT, protagonist_profile TEXT, global_guidance TEXT, writing_style TEXT, reference_works TEXT, character_states TEXT, premise TEXT, worldbuilding TEXT, synopsis TEXT, characters_arch TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE import_runs (id TEXT PRIMARY KEY, purpose TEXT NOT NULL DEFAULT '', root_run_id TEXT NOT NULL DEFAULT '', effect_namespace TEXT NOT NULL DEFAULT '', source_fingerprint TEXT NOT NULL DEFAULT '', manifest_fingerprint TEXT NOT NULL DEFAULT '', authority_fingerprint TEXT NOT NULL DEFAULT '', legacy_source_fingerprint TEXT NOT NULL DEFAULT '', source_display_json TEXT NOT NULL DEFAULT '[]', locale TEXT NOT NULL DEFAULT 'zh-CN', stage TEXT NOT NULL DEFAULT 'prepared', status TEXT NOT NULL DEFAULT 'ready', completed_batches_json TEXT NOT NULL DEFAULT '{}', last_error TEXT NOT NULL DEFAULT '', resumable INTEGER NOT NULL DEFAULT 0, cancel_requested INTEGER NOT NULL DEFAULT 0, execution_owner TEXT NOT NULL DEFAULT '', execution_epoch INTEGER NOT NULL DEFAULT 0, lease_expires_at INTEGER NOT NULL DEFAULT 0, total_chapters INTEGER NOT NULL DEFAULT 0, total_content_size INTEGER NOT NULL DEFAULT 0, manifest_chapter_count INTEGER NOT NULL DEFAULT 0, manifest_content_size INTEGER NOT NULL DEFAULT 0, manifest_word_count INTEGER NOT NULL DEFAULT 0, completed_chapters INTEGER NOT NULL DEFAULT 0, base_run_id TEXT, created_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:11', updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:12', completed_at TEXT)",
	"CREATE TABLE import_run_sources (run_id TEXT NOT NULL, source_index INTEGER NOT NULL, source_id TEXT NOT NULL, source_fingerprint TEXT NOT NULL, display_json TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL, chapter_count INTEGER NOT NULL DEFAULT 0, content_size INTEGER NOT NULL DEFAULT 0, word_count INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:13', PRIMARY KEY (run_id, source_index))",
	"CREATE TABLE import_run_chapters (run_id TEXT NOT NULL, chapter_number INTEGER NOT NULL, source_id TEXT NOT NULL DEFAULT '', source_chapter_number INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', content_fingerprint TEXT NOT NULL DEFAULT '', content_size INTEGER NOT NULL DEFAULT 0, content_snapshot TEXT NOT NULL DEFAULT '', PRIMARY KEY (run_id, chapter_number))",
	"CREATE TABLE import_effect_ledger (run_id TEXT NOT NULL, stage TEXT NOT NULL, batch_id TEXT NOT NULL, effect_namespace TEXT NOT NULL DEFAULT '', effect_key TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, evidence_kind TEXT NOT NULL DEFAULT '', diagnostic_code TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:14', PRIMARY KEY (run_id, stage, batch_id))",
}

// productionImportRunWriteContract is the trusted contract for
// productionImportRunWriteSchema. The verifier hashes sqlite_master in (type, name)
// order, so the entries are sorted the same way instead of in DDL order.
func productionImportRunWriteContract(t *testing.T) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(productionImportRunWriteSchema))
	for _, statement := range productionImportRunWriteSchema {
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

// productionImportRunWriteBatch is one effect-ledger row the fixture starts from:
// a checkpoint the run has already earned.
type productionImportRunWriteBatch struct {
	Stage   string
	BatchID string
	State   string
}

// productionImportRunWriteSeed is one run row the fixture starts from. The zero
// values are the interesting ones: an empty owner and epoch 0 mean the run has
// never been executed, and LeaseExpiresAt is only set for the scenarios that need
// a live execution lease.
type productionImportRunWriteSeed struct {
	ID             string
	Purpose        string
	Stage          string
	Status         string
	Resumable      int64
	ExecutionOwner string
	ExecutionEpoch int64
	LeaseExpiresAt int64
	Batches        []productionImportRunWriteBatch
}

// productionImportRunWriteLiveLease is a lease that is still live when the store
// compares it against time.Now(). It is one hour out, computed at seed time from
// the wall clock, because the store's authority check is wall-clock based.
func productionImportRunWriteLiveLease() int64 {
	return time.Now().UnixMilli() + int64(time.Hour/time.Millisecond)
}

// productionImportRunWriteLiveLeaseShort is a live lease that expires SOONER than
// the store's own re-stamp window (importRunLeaseMS is 15 minutes,
// internal/store/importrun.go:46-47). Only db:import-run-renew-execution compares
// lease magnitudes: the store re-stamps to now+15m rather than adding to the old
// value, so a seeded hour would look like a shortening.
func productionImportRunWriteLiveLeaseShort() int64 {
	return time.Now().UnixMilli() + int64(10*time.Minute/time.Millisecond)
}

// productionImportRunWriteLeaseWire is the renderer-shaped execution lease for one
// epoch. JSON arguments arrive as float64, so the epoch is one too.
func productionImportRunWriteLeaseWire(epoch int64) map[string]any {
	return map[string]any{"owner": productionImportRunWriteOwner, "epoch": float64(epoch)}
}

// productionImportRunWriteSeedRows commits the seeded runs and their checkpoints
// through hand-written INSERTs (the shipped store owns no run-creation method),
// then folds the write-ahead log into the admitted file so the bytes the test reads
// are the bytes the lane admits.
func productionImportRunWriteSeedRows(t *testing.T, projectPath string, seeds ...productionImportRunWriteSeed) {
	t.Helper()
	statements := []string{
		`INSERT INTO project_core (id, project_name, writing_language, total_chapters, words_per_chapter,
 created_at, updated_at) VALUES ('main', '导入运行写入小说', 'zh-CN', 2, 2000,
 '2024-05-06 09:10:11', '2024-05-06 09:10:12')`,
	}
	for _, seed := range seeds {
		statements = append(statements, fmt.Sprintf(`INSERT INTO import_runs (id, purpose, stage, status,
 resumable, execution_owner, execution_epoch, lease_expires_at, total_chapters, total_content_size)
 VALUES ('%s', '%s', '%s', '%s', %d, '%s', %d, %d, %d, %d)`,
			seed.ID, seed.Purpose, seed.Stage, seed.Status, seed.Resumable, seed.ExecutionOwner,
			seed.ExecutionEpoch, seed.LeaseExpiresAt, productionImportRunWriteChapters, int64(0)))
		for _, batch := range seed.Batches {
			statements = append(statements, fmt.Sprintf(`INSERT INTO import_effect_ledger (run_id, stage,
 batch_id, effect_namespace, effect_key, state, evidence_kind, diagnostic_code)
 VALUES ('%s', '%s', '%s', '', 'checkpoint:%s:%s', '%s', 'checkpoint', '')`,
				seed.ID, batch.Stage, batch.BatchID, batch.Stage, batch.BatchID, batch.State))
		}
	}
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	// The lane admits the file itself, so the seeded write must be checkpointed
	// into it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
}

// productionImportRunWriteLane opens the real production lane over the disposable
// data root and installs exactly two binders on it: the project lifecycle (so
// project:open can issue the lease these writes name) and this batch's seven
// import-run writes. Nothing else is installed, so any other dispatch name keeps
// failing closed at 501.
func productionImportRunWriteLane(t *testing.T, root string) http.Handler {
	t.Helper()
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, productionConfig{
		Schema: productionImportRunWriteContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-import-run-writes")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	if bindErr := newProductionImportRunWrites(open.durable).Bind(server); bindErr != nil {
		t.Fatal(bindErr)
	}
	return server
}

// productionImportRunWriteFixture is one opened import-run write lane with one
// opened project.
type productionImportRunWriteFixture struct {
	t           *testing.T
	projectPath string
	database    string
	author      *productionDurableClient
	frozen      map[string]any
}

// newProductionImportRunWriteFixture builds the temporary data root, opens the
// lane over it and opens the one admitted project for one browser tab. Opening the
// project is what issues the live lease every write below names.
func newProductionImportRunWriteFixture(t *testing.T, seeds ...productionImportRunWriteSeed) *productionImportRunWriteFixture {
	t.Helper()
	root := syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionImportRunWriteProjectName, productionImportRunWriteProjectID,
		admission.CurrentSchemaVersion, productionImportRunWriteSchema)
	projectPath := filepath.Join(library, productionImportRunWriteProjectName)
	productionImportRunWriteSeedRows(t, projectPath, seeds...)
	handler := productionImportRunWriteLane(t, root)
	author := newProductionDurableClient(t, handler, productionImportRunWriteTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "import-run-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted import-run write project: %+v", opened)
	}
	if opened.Project["id"] != productionImportRunWriteProjectID || opened.Project["path"] != projectPath {
		t.Fatalf("project:open returned the wrong project identity: %v", opened.Project)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return &productionImportRunWriteFixture{
		t:           t,
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		author:      author,
		frozen: map[string]any{
			"projectId":   opened.Project["id"],
			"projectPath": opened.Project["path"],
			"leaseId":     lease,
		},
	}
}

// staleContext is the same frozen session with a lane lease this lane never
// issued.
func (f *productionImportRunWriteFixture) staleContext() map[string]any {
	return map[string]any{
		"projectId":   f.frozen["projectId"],
		"projectPath": f.frozen["projectPath"],
		"leaseId":     productionImportRunWriteStaleLease,
	}
}

// result sends one write and returns its decoded success envelope. A non-200 is
// fatal: a bound import-run write answers 200 only after the store committed, so a
// transport error means the transition never ran.
func (f *productionImportRunWriteFixture) result(operation string, args []any) map[string]any {
	f.t.Helper()
	response := f.author.call(f.t, operation, args)
	if response.Code != http.StatusOK {
		f.t.Fatalf("%s: %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if wire.Result["success"] != true || wire.Result["error"] != nil {
		f.t.Fatalf("%s was not reported as a commit: %v", operation, wire.Result)
	}
	if strings.Contains(response.Body.String(), f.projectPath) {
		f.t.Fatalf("%s echoed the project path: %s", operation, response.Body.String())
	}
	return wire.Result
}

// refused asserts one refused transition carries the code the STORE itself
// raised. This family is not the draft family: a refusal is a transport error with
// a symbolic code, and a 501 or a generic REQUEST_FAILED here means the binder was
// never reached or the detail was swallowed.
func (f *productionImportRunWriteFixture) refused(operation string, args []any, code string) {
	f.t.Helper()
	response := f.author.call(f.t, operation, args)
	if response.Code == http.StatusOK {
		f.t.Fatalf("%s answered 200 for a refused transition: %s", operation, response.Body.String())
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if wire.Error.Code == "GO_OPERATION_NOT_MIGRATED" {
		f.t.Fatalf("%s is not bound at all: %s", operation, response.Body.String())
	}
	if wire.Error.Code != code {
		f.t.Fatalf("%s refused with %q; want the store's own %q: %s",
			operation, wire.Error.Code, code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), f.projectPath) {
		f.t.Fatalf("%s leaked the project path: %s", operation, response.Body.String())
	}
}

// refusedWithoutLease asserts a request whose frozen context names a lease this
// lane never issued is refused by the admission fence itself, before a single row
// is read.
func (f *productionImportRunWriteFixture) refusedWithoutLease(operation string, args []any) {
	f.t.Helper()
	response := f.author.call(f.t, operation, args)
	if response.Code == http.StatusOK {
		f.t.Fatalf("%s wrote without a live lease: %s", operation, response.Body.String())
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	for _, code := range []string{
		"PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE", "PROJECT_SESSION_REQUIRED",
		"PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_NOT_OPEN", "AUTHOR_WRITE_STALE",
		"PROJECT_HANDLE_CLOSED", "PRODUCTION_CLOSED",
	} {
		if wire.Error.Code == code {
			return
		}
	}
	f.t.Fatalf("%s refused a stale lease with %q; want the admission fence's own lease code: %s",
		operation, wire.Error.Code, response.Body.String())
}

// productionImportRunWriteQuery reads through a SEPARATE read-only connection on
// the author's own database file, so a commit is only "persisted" when the file the
// lane wrote says so and not when a handler echoed its own input.
func productionImportRunWriteQuery(t *testing.T, database, query string, args ...any) []map[string]any {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range pointers {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("read %q: %v", query, err)
		}
		row := make(map[string]any, len(columns))
		for index, name := range columns {
			row[name] = values[index]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return out
}

// productionImportRunWriteRow reads exactly one row and fails when the readback
// does not have the shape the assertions assume.
func productionImportRunWriteRow(t *testing.T, database, query string, args ...any) map[string]any {
	t.Helper()
	rows := productionImportRunWriteQuery(t, database, query, args...)
	if len(rows) != 1 {
		t.Fatalf("read %q: %d rows; want exactly 1: %v", query, len(rows), rows)
	}
	return rows[0]
}

func productionImportRunWriteText(row map[string]any, column string) string {
	text, ok := row[column].(string)
	if !ok {
		return ""
	}
	return text
}

func productionImportRunWriteNumber(row map[string]any, column string) int64 {
	number, ok := row[column].(int64)
	if !ok {
		return -1
	}
	return number
}

// productionImportRunWriteFloat reads one JSON number out of a decoded envelope.
func productionImportRunWriteFloat(t *testing.T, row map[string]any, column string) float64 {
	t.Helper()
	number, ok := row[column].(float64)
	if !ok {
		t.Fatalf("the envelope field %q is not a JSON number: %v", column, row)
	}
	return number
}

// productionImportRunWriteSnapshot is the exact byte image of the admitted
// database file. A refused write must leave this image identical: no partial row,
// no journal.
func productionImportRunWriteSnapshot(t *testing.T, database string) []byte {
	t.Helper()
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func productionImportRunWriteAssertUnchanged(t *testing.T, database string, before []byte, why string) {
	t.Helper()
	after := productionImportRunWriteSnapshot(t, database)
	if !bytes.Equal(before, after) {
		t.Fatalf("%s changed the admitted database bytes (%d -> %d)", why, len(before), len(after))
	}
}

// TestProductionImportRunWritesServeAdmittedAuthorDatabase pins the seven bound
// bookkeeping writes: each one commits through the real HTTP handler over the
// author's own database, each commit is read back off disk, each transition's own
// precondition is asserted on a refused second call, and the stale-lease subtest
// proves none of them can write without the live lease.
func TestProductionImportRunWritesServeAdmittedAuthorDatabase(t *testing.T) {
	// db:import-run-start-resume: mint the execution lease and move the run into
	// running. A run that is not resumable is never a success.
	t.Run("db:import-run-start-resume", func(t *testing.T) {
		f := newProductionImportRunWriteFixture(t,
			productionImportRunWriteSeed{ID: productionImportRunWriteStartID, Purpose: "reference",
				Stage: "global", Status: "ready", Resumable: 1, ExecutionEpoch: 3},
			productionImportRunWriteSeed{ID: productionImportRunWriteSettledID, Purpose: "reference",
				Stage: "completed", Status: "completed", ExecutionEpoch: 2},
		)
		result := f.result(productionImportRunWriteStartResumeName, []any{
			productionImportRunWriteStartID, productionImportRunWriteOwner, f.projectPath, f.frozen,
		})
		start, _ := result["start"].(map[string]any)
		execution, _ := start["execution"].(map[string]any)
		if start == nil || execution == nil {
			t.Fatalf("start-resume returned no start/execution envelope: %v", result)
		}
		if run, _ := start["run"].(map[string]any); productionImportRunWriteText(run, "status") != "running" {
			t.Fatalf("start-resume did not report a running run: %v", start)
		}
		expiresAt := int64(productionImportRunWriteFloat(t, execution, "expiresAt"))
		if expiresAt <= 0 {
			t.Fatalf("start-resume minted no expiry: %v", execution)
		}
		if epoch := productionImportRunWriteFloat(t, execution, "epoch"); epoch != 4 {
			t.Fatalf("start-resume minted epoch %v; want the retired epoch 3 bumped to 4", epoch)
		}
		row := productionImportRunWriteRow(t, f.database, `SELECT stage, status, resumable, cancel_requested,
 execution_owner, execution_epoch, lease_expires_at FROM import_runs WHERE id = ?`, productionImportRunWriteStartID)
		if productionImportRunWriteText(row, "stage") != "global" {
			t.Fatalf("start-resume moved the stage of a non-prepared run: %v", row)
		}
		if productionImportRunWriteText(row, "status") != "running" ||
			productionImportRunWriteNumber(row, "resumable") != 1 {
			t.Fatalf("start-resume did not commit a resumable running row: %v", row)
		}
		if productionImportRunWriteText(row, "execution_owner") != productionImportRunWriteOwner ||
			productionImportRunWriteNumber(row, "execution_epoch") != 4 {
			t.Fatalf("start-resume did not commit the minted lease: %v", row)
		}
		if productionImportRunWriteNumber(row, "lease_expires_at") != expiresAt {
			t.Fatalf("the committed lease %d is not the reported expiry %d: %v",
				productionImportRunWriteNumber(row, "lease_expires_at"), expiresAt, row)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		// A retired run (resumable = 0, no last_error) is not startable: the store's
		// own precondition, not a generic failure.
		f.refused(productionImportRunWriteStartResumeName, []any{
			productionImportRunWriteSettledID, productionImportRunWriteOwner, f.projectPath, f.frozen,
		}, "IMPORT_RUN_NOT_STARTABLE")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused resume of a retired run")
		row = productionImportRunWriteRow(t, f.database, `SELECT status, execution_owner, execution_epoch,
 lease_expires_at FROM import_runs WHERE id = ?`, productionImportRunWriteSettledID)
		if productionImportRunWriteText(row, "status") != "completed" ||
			productionImportRunWriteText(row, "execution_owner") != "" ||
			productionImportRunWriteNumber(row, "execution_epoch") != 2 ||
			productionImportRunWriteNumber(row, "lease_expires_at") != 0 {
			t.Fatalf("the refused resume moved the retired run: %v", row)
		}
	})

	// db:import-run-renew-execution: only the exact live owner+epoch may re-stamp
	// the lease, and the row must adopt exactly the reported expiry.
	t.Run("db:import-run-renew-execution", func(t *testing.T) {
		seeded := productionImportRunWriteLiveLeaseShort()
		f := newProductionImportRunWriteFixture(t, productionImportRunWriteSeed{
			ID: productionImportRunWriteLiveID, Purpose: "reference", Stage: "global", Status: "running",
			Resumable: 1, ExecutionOwner: productionImportRunWriteOwner, ExecutionEpoch: 7, LeaseExpiresAt: seeded,
		})
		result := f.result(productionImportRunWriteRenewExecutionName, []any{
			productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(7), f.projectPath, f.frozen,
		})
		execution, _ := result["execution"].(map[string]any)
		if execution == nil {
			t.Fatalf("renew-execution returned no execution envelope: %v", result)
		}
		if productionImportRunWriteText(execution, "owner") != productionImportRunWriteOwner ||
			productionImportRunWriteFloat(t, execution, "epoch") != 7 {
			t.Fatalf("renew-execution re-minted the lease instead of extending it: %v", execution)
		}
		expiresAt := int64(productionImportRunWriteFloat(t, execution, "expiresAt"))
		row := productionImportRunWriteRow(t, f.database, `SELECT execution_owner, execution_epoch,
 lease_expires_at FROM import_runs WHERE id = ?`, productionImportRunWriteLiveID)
		if productionImportRunWriteNumber(row, "lease_expires_at") != expiresAt {
			t.Fatalf("the committed lease %d is not the reported expiry %d: %v",
				productionImportRunWriteNumber(row, "lease_expires_at"), expiresAt, row)
		}
		if expiresAt <= seeded {
			t.Fatalf("renew-execution did not extend the lease: %d -> %d", seeded, expiresAt)
		}
		if productionImportRunWriteText(row, "execution_owner") != productionImportRunWriteOwner ||
			productionImportRunWriteNumber(row, "execution_epoch") != 7 {
			t.Fatalf("renew-execution changed the authority it was renewing: %v", row)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		// A lease this run never issued is refused by the store's own authority
		// check.
		f.refused(productionImportRunWriteRenewExecutionName, []any{
			productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(8), f.projectPath, f.frozen,
		}, "IMPORT_RUN_EXECUTION_STALE")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused renew from a foreign epoch")
		row = productionImportRunWriteRow(t, f.database, `SELECT lease_expires_at FROM import_runs WHERE id = ?`,
			productionImportRunWriteLiveID)
		if productionImportRunWriteNumber(row, "lease_expires_at") != expiresAt {
			t.Fatalf("the refused renew moved the committed lease: %v", row)
		}
	})

	// db:import-run-request-cancel: only marks the intent. The run keeps running
	// until a boundary applies it.
	t.Run("db:import-run-request-cancel", func(t *testing.T) {
		f := newProductionImportRunWriteFixture(t, productionImportRunWriteSeed{
			ID: productionImportRunWriteLiveID, Purpose: "reference", Stage: "global", Status: "running",
			Resumable: 1, ExecutionOwner: productionImportRunWriteOwner, ExecutionEpoch: 5,
			LeaseExpiresAt: productionImportRunWriteLiveLease(),
		})
		result := f.result(productionImportRunWriteRequestCancelName, []any{
			productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(5), f.projectPath, f.frozen,
		})
		run, _ := result["run"].(map[string]any)
		if run == nil || run["cancelRequested"] != true || productionImportRunWriteText(run, "status") != "running" {
			t.Fatalf("request-cancel did not report a running run with a cancel intent: %v", result)
		}
		row := productionImportRunWriteRow(t, f.database, `SELECT status, cancel_requested, resumable,
 execution_owner, execution_epoch FROM import_runs WHERE id = ?`, productionImportRunWriteLiveID)
		if productionImportRunWriteNumber(row, "cancel_requested") != 1 {
			t.Fatalf("request-cancel did not commit the intent: %v", row)
		}
		if productionImportRunWriteText(row, "status") != "running" ||
			productionImportRunWriteText(row, "execution_owner") != productionImportRunWriteOwner ||
			productionImportRunWriteNumber(row, "execution_epoch") != 5 {
			t.Fatalf("request-cancel retired more than the intent: %v", row)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		// A cancel intent signed by a lease this run never issued is refused as a
		// stale execution, not accepted as another tab's request.
		f.refused(productionImportRunWriteRequestCancelName, []any{
			productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(4), f.projectPath, f.frozen,
		}, "IMPORT_RUN_EXECUTION_STALE")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused cancel intent")
	})

	// db:import-run-cancel-at-boundary: applies the cancel and retires the lease in
	// one step, so the boundary row is the evidence.
	t.Run("db:import-run-cancel-at-boundary", func(t *testing.T) {
		f := newProductionImportRunWriteFixture(t, productionImportRunWriteSeed{
			ID: productionImportRunWriteLiveID, Purpose: "reference", Stage: "global", Status: "running",
			Resumable: 1, ExecutionOwner: productionImportRunWriteOwner, ExecutionEpoch: 5,
			LeaseExpiresAt: productionImportRunWriteLiveLease(),
		})
		result := f.result(productionImportRunWriteCancelAtBoundaryName, []any{
			productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(5), f.projectPath, f.frozen,
		})
		run, _ := result["run"].(map[string]any)
		if run == nil || productionImportRunWriteText(run, "status") != "cancelled" ||
			run["cancelRequested"] != true || run["resumable"] != true {
			t.Fatalf("cancel-at-boundary did not report a cancelled resumable run: %v", result)
		}
		row := productionImportRunWriteRow(t, f.database, `SELECT status, resumable, cancel_requested,
 execution_owner, execution_epoch, lease_expires_at FROM import_runs WHERE id = ?`, productionImportRunWriteLiveID)
		if productionImportRunWriteText(row, "status") != "cancelled" ||
			productionImportRunWriteNumber(row, "resumable") != 1 ||
			productionImportRunWriteNumber(row, "cancel_requested") != 1 {
			t.Fatalf("the cancellation boundary row was not committed: %v", row)
		}
		if productionImportRunWriteText(row, "execution_owner") != "" ||
			productionImportRunWriteNumber(row, "execution_epoch") != 6 ||
			productionImportRunWriteNumber(row, "lease_expires_at") != 0 {
			t.Fatalf("the cancellation boundary did not retire the lease: %v", row)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		// The boundary retires the epoch, so replaying the same lease is now stale.
		f.refused(productionImportRunWriteCancelAtBoundaryName, []any{
			productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(5), f.projectPath, f.frozen,
		}, "IMPORT_RUN_EXECUTION_STALE")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the replayed cancellation boundary")
		row = productionImportRunWriteRow(t, f.database, `SELECT execution_epoch, lease_expires_at
 FROM import_runs WHERE id = ?`, productionImportRunWriteLiveID)
		if productionImportRunWriteNumber(row, "execution_epoch") != 6 ||
			productionImportRunWriteNumber(row, "lease_expires_at") != 0 {
			t.Fatalf("the refused replay retired the lease twice: %v", row)
		}
	})

	// db:import-run-complete-batch: the batch rows ARE the effect ledger, so the
	// readback is the ledger row this checkpoint wrote, and the JSON fallback must
	// stay untouched because ledger evidence wins.
	t.Run("db:import-run-complete-batch", func(t *testing.T) {
		f := newProductionImportRunWriteFixture(t, productionImportRunWriteSeed{
			ID: productionImportRunWriteLiveID, Purpose: "reference", Stage: "refresh", Status: "running",
			Resumable: 1, ExecutionOwner: productionImportRunWriteOwner, ExecutionEpoch: 2,
			LeaseExpiresAt: productionImportRunWriteLiveLease(),
		})
		result := f.result(productionImportRunWriteCompleteBatchName, []any{
			productionImportRunWriteLiveID, "refresh", "done",
			productionImportRunWriteLeaseWire(2), f.projectPath, f.frozen,
		})
		if result["newlyCompleted"] != true || result["cancelApplied"] != false {
			t.Fatalf("complete-batch did not report a fresh, non-cancelling checkpoint: %v", result)
		}
		batches := productionImportRunWriteQuery(t, f.database, `SELECT stage, batch_id, state, evidence_kind
 FROM import_effect_ledger WHERE run_id = ?`, productionImportRunWriteLiveID)
		if len(batches) != 1 {
			t.Fatalf("complete-batch committed %d ledger rows; want exactly the refresh/done checkpoint: %v",
				len(batches), batches)
		}
		if productionImportRunWriteText(batches[0], "stage") != "refresh" ||
			productionImportRunWriteText(batches[0], "batch_id") != "done" ||
			productionImportRunWriteText(batches[0], "state") != "complete" ||
			productionImportRunWriteText(batches[0], "evidence_kind") != "legacy-checkpoint" {
			t.Fatalf("complete-batch wrote the wrong effect-ledger row: %v", batches[0])
		}
		row := productionImportRunWriteRow(t, f.database, `SELECT completed_batches_json, status, resumable,
 execution_owner FROM import_runs WHERE id = ?`, productionImportRunWriteLiveID)
		if productionImportRunWriteText(row, "completed_batches_json") != "{}" {
			t.Fatalf("complete-batch wrote the JSON fallback next to a ledger row: %v", row)
		}
		if productionImportRunWriteText(row, "status") != "running" ||
			productionImportRunWriteNumber(row, "resumable") != 1 ||
			productionImportRunWriteText(row, "execution_owner") != productionImportRunWriteOwner {
			t.Fatalf("complete-batch changed the run outside the checkpoint: %v", row)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		// A checkpoint for a stage the run is not running in is refused by the
		// store's own stage precondition.
		f.refused(productionImportRunWriteCompleteBatchName, []any{
			productionImportRunWriteLiveID, "author-publish", "chapter:1",
			productionImportRunWriteLeaseWire(2), f.projectPath, f.frozen,
		}, "IMPORT_RUN_BATCH_STAGE_MISMATCH")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused checkpoint of a foreign stage")
		// A stage that cannot be checkpointed by direct evidence is refused before
		// the repository is entered.
		f.refused(productionImportRunWriteCompleteBatchName, []any{
			productionImportRunWriteLiveID, "blueprints", "1-2-00000000",
			productionImportRunWriteLeaseWire(2), f.projectPath, f.frozen,
		}, "IMPORT_RUN_CHECKPOINT_STAGE_INVALID")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused receipt-only checkpoint")
	})

	// db:import-run-advance-stage: the stage must be fully checkpointed first, and
	// the advance writes the next stage onto the run row.
	t.Run("db:import-run-advance-stage", func(t *testing.T) {
		f := newProductionImportRunWriteFixture(t,
			productionImportRunWriteSeed{
				ID: productionImportRunWriteLiveID, Purpose: "reference", Stage: "global", Status: "running",
				Resumable: 1, ExecutionOwner: productionImportRunWriteOwner, ExecutionEpoch: 4,
				LeaseExpiresAt: productionImportRunWriteLiveLease(),
				Batches:        []productionImportRunWriteBatch{{Stage: "global", BatchID: "done", State: "complete"}},
			},
			productionImportRunWriteSeed{
				ID: productionImportRunWriteSiblingID, Purpose: "reference", Stage: "global", Status: "running",
				Resumable: 1, ExecutionOwner: productionImportRunWriteOwner, ExecutionEpoch: 4,
				LeaseExpiresAt: productionImportRunWriteLiveLease(),
			},
		)
		result := f.result(productionImportRunWriteAdvanceStageName, []any{
			productionImportRunWriteLiveID, "global", "style",
			productionImportRunWriteLeaseWire(4), f.projectPath, f.frozen,
		})
		run, _ := result["run"].(map[string]any)
		if run == nil || productionImportRunWriteText(run, "stage") != "style" ||
			productionImportRunWriteText(run, "status") != "running" {
			t.Fatalf("advance-stage did not report the advanced run: %v", result)
		}
		row := productionImportRunWriteRow(t, f.database, `SELECT stage, status, resumable, completed_chapters
 FROM import_runs WHERE id = ?`, productionImportRunWriteLiveID)
		if productionImportRunWriteText(row, "stage") != "style" {
			t.Fatalf("advance-stage did not commit the next stage: %v", row)
		}
		if productionImportRunWriteText(row, "status") != "running" ||
			productionImportRunWriteNumber(row, "resumable") != 1 {
			t.Fatalf("advance-stage retired a run that is still executing: %v", row)
		}
		if productionImportRunWriteNumber(row, "completed_chapters") != 0 {
			t.Fatalf("advance-stage invented chapter progress: %v", row)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		// The sibling is at the same stage with NO 'done' checkpoint, so its stage
		// is incomplete: the store refuses the advance with its own code.
		f.refused(productionImportRunWriteAdvanceStageName, []any{
			productionImportRunWriteSiblingID, "global", "style",
			productionImportRunWriteLeaseWire(4), f.projectPath, f.frozen,
		}, "IMPORT_RUN_STAGE_INCOMPLETE")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused advance of an unchecked stage")
		row = productionImportRunWriteRow(t, f.database, `SELECT stage FROM import_runs WHERE id = ?`,
			productionImportRunWriteSiblingID)
		if productionImportRunWriteText(row, "stage") != "global" {
			t.Fatalf("the refused advance moved the unchecked run: %v", row)
		}
	})

	// db:import-run-complete: only a running refresh stage whose checkpoint is
	// 'done' may finish, and finishing retires the run and its lease. Completing an
	// already-complete run is NOT a success.
	t.Run("db:import-run-complete", func(t *testing.T) {
		f := newProductionImportRunWriteFixture(t,
			productionImportRunWriteSeed{
				ID: productionImportRunWriteLiveID, Purpose: "reference", Stage: "refresh", Status: "running",
				Resumable: 1, ExecutionOwner: productionImportRunWriteOwner, ExecutionEpoch: 6,
				LeaseExpiresAt: productionImportRunWriteLiveLease(),
				Batches:        []productionImportRunWriteBatch{{Stage: "refresh", BatchID: "done", State: "complete"}},
			},
			productionImportRunWriteSeed{
				ID: productionImportRunWriteSiblingID, Purpose: "reference", Stage: "global", Status: "running",
				Resumable: 1, ExecutionOwner: productionImportRunWriteOwner, ExecutionEpoch: 6,
				LeaseExpiresAt: productionImportRunWriteLiveLease(),
			},
		)
		result := f.result(productionImportRunWriteCompleteName, []any{
			productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(6), f.projectPath, f.frozen,
		})
		run, _ := result["run"].(map[string]any)
		if run == nil || productionImportRunWriteText(run, "status") != "completed" ||
			productionImportRunWriteText(run, "stage") != "completed" {
			t.Fatalf("complete did not report the retired run: %v", result)
		}
		row := productionImportRunWriteRow(t, f.database, `SELECT stage, status, resumable, cancel_requested,
 execution_owner, execution_epoch, lease_expires_at, completed_chapters, total_chapters, completed_at
 FROM import_runs WHERE id = ?`, productionImportRunWriteLiveID)
		if productionImportRunWriteText(row, "stage") != "completed" ||
			productionImportRunWriteText(row, "status") != "completed" ||
			productionImportRunWriteNumber(row, "resumable") != 0 ||
			productionImportRunWriteNumber(row, "cancel_requested") != 0 {
			t.Fatalf("complete did not retire the run row: %v", row)
		}
		if productionImportRunWriteText(row, "execution_owner") != "" ||
			productionImportRunWriteNumber(row, "execution_epoch") != 7 ||
			productionImportRunWriteNumber(row, "lease_expires_at") != 0 {
			t.Fatalf("complete did not retire the execution lease: %v", row)
		}
		if productionImportRunWriteNumber(row, "completed_chapters") != productionImportRunWriteChapters ||
			productionImportRunWriteNumber(row, "total_chapters") != productionImportRunWriteChapters {
			t.Fatalf("complete did not settle the chapter count: %v", row)
		}
		if productionImportRunWriteText(row, "completed_at") == "" {
			t.Fatalf("complete left no completion timestamp: %v", row)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		// Completing an already-complete run: its status is no longer 'running', so
		// the authority check refuses it. It is not a second success.
		f.refused(productionImportRunWriteCompleteName, []any{
			productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(6), f.projectPath, f.frozen,
		}, "IMPORT_RUN_EXECUTION_STALE")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused completion of a retired run")
		// A running run that is not at the refresh stage cannot be completed: the
		// store's own stage precondition.
		f.refused(productionImportRunWriteCompleteName, []any{
			productionImportRunWriteSiblingID, productionImportRunWriteLeaseWire(6), f.projectPath, f.frozen,
		}, "IMPORT_RUN_STAGE_MISMATCH")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused completion of an early stage")
		row = productionImportRunWriteRow(t, f.database, `SELECT stage, status, completed_at FROM import_runs
 WHERE id = ?`, productionImportRunWriteSiblingID)
		if productionImportRunWriteText(row, "stage") != "global" ||
			productionImportRunWriteText(row, "status") != "running" ||
			productionImportRunWriteText(row, "completed_at") != "" {
			t.Fatalf("the refused completion moved the early-stage run: %v", row)
		}
	})

	// A stale lane lease: every one of the seven names is refused by the admission
	// fence itself, and not one row moves.
	t.Run("db:import-run-writes-refuse-a-stale-lease", func(t *testing.T) {
		f := newProductionImportRunWriteFixture(t, productionImportRunWriteSeed{
			ID: productionImportRunWriteLiveID, Purpose: "reference", Stage: "global", Status: "running",
			Resumable: 1, ExecutionOwner: productionImportRunWriteOwner, ExecutionEpoch: 7,
			LeaseExpiresAt: productionImportRunWriteLiveLease(),
		})
		settled := productionImportRunWriteSnapshot(t, f.database)
		stale := f.staleContext()
		requests := []struct {
			operation string
			args      []any
		}{
			{productionImportRunWriteStartResumeName, []any{
				productionImportRunWriteLiveID, productionImportRunWriteOwner, f.projectPath, stale}},
			{productionImportRunWriteRenewExecutionName, []any{
				productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(7), f.projectPath, stale}},
			{productionImportRunWriteRequestCancelName, []any{
				productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(7), f.projectPath, stale}},
			{productionImportRunWriteCancelAtBoundaryName, []any{
				productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(7), f.projectPath, stale}},
			{productionImportRunWriteCompleteBatchName, []any{
				productionImportRunWriteLiveID, "refresh", "done",
				productionImportRunWriteLeaseWire(7), f.projectPath, stale}},
			{productionImportRunWriteAdvanceStageName, []any{
				productionImportRunWriteLiveID, "global", "style",
				productionImportRunWriteLeaseWire(7), f.projectPath, stale}},
			{productionImportRunWriteCompleteName, []any{
				productionImportRunWriteLiveID, productionImportRunWriteLeaseWire(7), f.projectPath, stale}},
		}
		for _, request := range requests {
			f.refusedWithoutLease(request.operation, request.args)
			productionImportRunWriteAssertUnchanged(t, f.database, settled,
				"the stale-lease "+request.operation)
		}
		row := productionImportRunWriteRow(t, f.database, `SELECT stage, status, resumable, cancel_requested,
 execution_owner, execution_epoch, lease_expires_at FROM import_runs WHERE id = ?`, productionImportRunWriteLiveID)
		if productionImportRunWriteText(row, "stage") != "global" ||
			productionImportRunWriteText(row, "status") != "running" ||
			productionImportRunWriteNumber(row, "cancel_requested") != 0 ||
			productionImportRunWriteNumber(row, "execution_epoch") != 7 {
			t.Fatalf("a stale-lease request moved the run it named: %v", row)
		}
		if batches := productionImportRunWriteQuery(t, f.database,
			`SELECT COUNT(*) AS total FROM import_effect_ledger`); len(batches) != 1 ||
			productionImportRunWriteNumber(batches[0], "total") != 0 {
			t.Fatalf("a stale-lease checkpoint wrote an effect-ledger row: %v", batches)
		}
	})
}
