//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The four bound `db:import-run-*` residual names run against a disposable
// t.TempDir() only: the session lease, the admitted project database and every
// readback live inside the test's own temporary tree.
//
// # Why this test builds the lane itself
//
// productionHandlerWithContract (production_open_linux.go:458) binds the names
// production_open_linux.go's own `for _, bind := range []func() error{` slice
// names, and this batch is not allowed to edit that file. The four names are
// therefore installed on a transport.Server this test constructs over the SAME
// openProduction result and the SAME durable bridge, exactly the way
// production_importrun_write_linux_test.go:251-280 mounts its own binder. Nothing
// else is bound but the project lifecycle, so an accidentally unbound name answers
// 501 and the refusal subtest can prove it.
//
// # Why the receipt is seeded with the store instead of through the lane
//
// This lane publishes no db:import-run-effect-receipt-prepare (see the adapter's
// header: the generation-source proof the reference always supplies has no
// counterpart here, and neither a nil proof nor a fail-closed one can drive a real
// handle-carrying receipt), so the one committed receipt this test reads is written
// by the store's own PrepareEffectReceipt + CommitEffectReceipt over the seeded
// database, before the lane opens - the same thing the desktop app does to the same
// file. Nothing about the hash, the ledger row or the effect is hand-written, so
// the read subtest still proves the real wire shape of a real receipt. The two
// unbound write legs are pinned at 501 in the final subtest, with the missing
// capability quoted.
//
// # What is hand-seeded, and why
//
// import_runs, import_run_sources and import_run_source_chapters have no
// run-creation method in the shipped store (production_importrun_write_linux_test.go:205-208
// seeds the same way), so a scenario's starting row is written as SQL. Every
// column importRunRowColumns reads is NOT NULL with a default, because a NULL
// scanned into an int64 is a scan error rather than a refusal and would make a
// "precondition refused" assertion pass for the wrong reason.

// productionImportRunResidualSchema is the smallest set of tables these four names
// really touch: the run, its frozen sources and chapters, the legacy chapter map,
// the effect ledger and the receipt ledger, plus the project_core row project:open
// and the writing-style effect both need. The DDL deliberately contains none of
// the characters the schema-contract encoder escapes.
var productionImportRunResidualSchema = []string{
	"CREATE TABLE project_core (id TEXT PRIMARY KEY, project_name TEXT, writing_language TEXT, genre TEXT, sub_genre TEXT, target_audience TEXT, total_chapters INTEGER, words_per_chapter INTEGER, creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT, narrative_pov TEXT, core_outline TEXT, world_setting TEXT, golden_finger TEXT, protagonist_profile TEXT, global_guidance TEXT, writing_style TEXT, reference_works TEXT, character_states TEXT, premise TEXT, worldbuilding TEXT, synopsis TEXT, characters_arch TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE import_runs (id TEXT PRIMARY KEY, purpose TEXT NOT NULL DEFAULT '', root_run_id TEXT NOT NULL DEFAULT '', effect_namespace TEXT NOT NULL DEFAULT '', source_fingerprint TEXT NOT NULL DEFAULT '', manifest_fingerprint TEXT NOT NULL DEFAULT '', authority_fingerprint TEXT NOT NULL DEFAULT '', legacy_source_fingerprint TEXT NOT NULL DEFAULT '', source_display_json TEXT NOT NULL DEFAULT '[]', locale TEXT NOT NULL DEFAULT 'zh-CN', stage TEXT NOT NULL DEFAULT 'prepared', status TEXT NOT NULL DEFAULT 'ready', completed_batches_json TEXT NOT NULL DEFAULT '{}', last_error TEXT NOT NULL DEFAULT '', resumable INTEGER NOT NULL DEFAULT 0, cancel_requested INTEGER NOT NULL DEFAULT 0, execution_owner TEXT NOT NULL DEFAULT '', execution_epoch INTEGER NOT NULL DEFAULT 0, lease_expires_at INTEGER NOT NULL DEFAULT 0, total_chapters INTEGER NOT NULL DEFAULT 0, total_content_size INTEGER NOT NULL DEFAULT 0, manifest_chapter_count INTEGER NOT NULL DEFAULT 0, manifest_content_size INTEGER NOT NULL DEFAULT 0, manifest_word_count INTEGER NOT NULL DEFAULT 0, completed_chapters INTEGER NOT NULL DEFAULT 0, base_run_id TEXT, created_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:11', updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:12', completed_at TEXT)",
	"CREATE TABLE import_run_sources (run_id TEXT NOT NULL, source_index INTEGER NOT NULL, source_id TEXT NOT NULL, source_fingerprint TEXT NOT NULL, legacy_source_fingerprint TEXT NOT NULL DEFAULT '', display_json TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL, manifest_fingerprint TEXT NOT NULL DEFAULT '', chapter_count INTEGER NOT NULL DEFAULT 0, content_size INTEGER NOT NULL DEFAULT 0, word_count INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:13', PRIMARY KEY (run_id, source_index))",
	"CREATE TABLE import_run_source_chapters (run_id TEXT NOT NULL, source_id TEXT NOT NULL, source_chapter_number INTEGER NOT NULL, title TEXT NOT NULL DEFAULT '', content_fingerprint TEXT NOT NULL DEFAULT '', content_size INTEGER NOT NULL DEFAULT 0, word_count INTEGER NOT NULL DEFAULT 0, content_snapshot TEXT NOT NULL DEFAULT '', PRIMARY KEY (run_id, source_id, source_chapter_number))",
	"CREATE TABLE import_run_chapters (run_id TEXT NOT NULL, chapter_number INTEGER NOT NULL, source_id TEXT NOT NULL DEFAULT '', source_chapter_number INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', content_fingerprint TEXT NOT NULL DEFAULT '', content_size INTEGER NOT NULL DEFAULT 0, content_snapshot TEXT NOT NULL DEFAULT '', PRIMARY KEY (run_id, chapter_number))",
	"CREATE TABLE import_source_chapter_map (purpose TEXT NOT NULL, source_id TEXT NOT NULL, source_chapter_number INTEGER NOT NULL, chapter_number INTEGER NOT NULL, PRIMARY KEY (purpose, source_id, source_chapter_number))",
	"CREATE TABLE import_effect_ledger (run_id TEXT NOT NULL, stage TEXT NOT NULL, batch_id TEXT NOT NULL, effect_namespace TEXT NOT NULL DEFAULT '', effect_key TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, evidence_kind TEXT NOT NULL DEFAULT '', diagnostic_code TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:14', PRIMARY KEY (run_id, stage, batch_id))",
	"CREATE TABLE import_run_receipts (run_id TEXT NOT NULL, schema_version INTEGER NOT NULL DEFAULT 1, effect_namespace TEXT NOT NULL DEFAULT '', effect_key TEXT NOT NULL DEFAULT '', stage TEXT NOT NULL DEFAULT '', batch_id TEXT NOT NULL DEFAULT '', kind TEXT NOT NULL DEFAULT '', payload_json TEXT NOT NULL DEFAULT '', payload_hash TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'prepared', effect_receipt_json TEXT, created_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:15', updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:16', PRIMARY KEY (run_id, stage, batch_id))",
}

// productionImportRunResidualContract is the trusted contract for
// productionImportRunResidualSchema. The verifier hashes sqlite_master in
// (type, name) order, so the entries are sorted the same way, not in DDL order.
func productionImportRunResidualContract(t *testing.T) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(productionImportRunResidualSchema))
	for _, statement := range productionImportRunResidualSchema {
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

const (
	productionImportRunResidualProjectName = "import-run-residuals-novel"
	productionImportRunResidualProjectID   = "8a1c4f60-5b27-4d93-8e05-3f6b9c1a7d24"
	productionImportRunResidualTab         = "client_import_resid01"
	productionImportRunResidualOwner       = "residual-executor-1"
	productionImportRunResidualStaleLease  = "stale-residual-lease-000001"
	productionImportRunResidualParseRunID  = "residual-parse-run"
	productionImportRunResidualOldRunID    = "residual-old-run"
	productionImportRunResidualNextRunID   = "residual-next-run"
	productionImportRunResidualLiveRunID   = "residual-live-run"
	productionImportRunResidualOtherRunID  = "residual-other-run"
	productionImportRunResidualStyleRunID  = "residual-style-run"
	productionImportRunResidualStyleEpoch  = int64(2)
	// productionImportRunResidualStyle is the writing style the committed receipt
	// carries. It reaches project_core.writing_style through the store's own commit,
	// so the read subtest compares the wire value against a column that was really
	// written by the effect.
	productionImportRunResidualStyle = "克制而具体的叙述"
)

// productionImportRunResidualZeroFingerprint is importRunZeroFingerprint: a parsing
// run has no manifest yet, and Restart writes it back for the restarting stage.
var productionImportRunResidualZeroFingerprint = strings.Repeat("0", 64)

// productionImportRunResidualLiveLease is a lease that is still live when the store
// compares it against time.Now(); it is one hour out, computed at seed time.
func productionImportRunResidualLiveLease() int64 {
	return time.Now().UnixMilli() + int64(time.Hour/time.Millisecond)
}

// productionImportRunResidualLeaseWire is the renderer-shaped execution lease for
// one epoch. JSON arguments arrive as float64, so the epoch is one too.
func productionImportRunResidualLeaseWire(epoch int64) map[string]any {
	return map[string]any{"owner": productionImportRunResidualOwner, "epoch": float64(epoch)}
}

// productionImportRunResidualWords is the declared word count of one chapter. It is
// seeded, not recomputed: finalize only sums the numbers each source declared.
func productionImportRunResidualWords(text string) int64 {
	return int64(utf8.RuneCountInString(text))
}

// productionImportRunResidualSeedCore is the one project_core row project:open
// requires, copied column for column from
// production_importrun_write_linux_test.go:195-203 so this project opens exactly
// like a real finalised one (readProjectOpenCore refuses a NULL
// character_states/total_chapters/... and unparseable timestamps).
const productionImportRunResidualSeedCore = `INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, premise, worldbuilding, synopsis, characters_arch,
 created_at, updated_at)
 VALUES ('main', '导入残余运行小说', 'zh-CN', 'fantasy', '', '', 2, 2000, 'auto', 3,
 '', '', '', '', '', '', '', '', '', '{}', '', '', '', '',
 '2024-05-06 09:10:11', '2024-05-06 09:10:12')`

// productionImportRunResidualChapter is one frozen chapter of a seeded source.
type productionImportRunResidualChapter struct {
	Title string
	Body  string
}

// productionImportRunResidualSource is one import_run_sources row plus the frozen
// chapters it declared. Its counters are derived from Chapters, because finalize
// compares the chapter rows against the source's declared sums and answers
// IMPORT_RUN_MANIFEST_INVALID when they disagree.
type productionImportRunResidualSource struct {
	ID       string
	Status   string
	Chapters []productionImportRunResidualChapter
}

// productionImportRunResidualSeed is one import_runs row. The zero values are the
// interesting ones: an empty owner and epoch 0 mean the run was never executed, and
// LeaseExpiresAt is only set for the scenarios that need a live execution lease.
type productionImportRunResidualSeed struct {
	ID             string
	Purpose        string
	Stage          string
	Status         string
	Resumable      int64
	ExecutionOwner string
	ExecutionEpoch int64
	LeaseExpiresAt int64
	Sources        []productionImportRunResidualSource
}

// productionImportRunResidualSeedProject writes an admitted project whose database
// carries the residual schema and the seeded runs, then folds the write-ahead log
// into the admitted file so the bytes the test reads are the bytes the lane admits.
func productionImportRunResidualSeedProject(t *testing.T, seeds ...productionImportRunResidualSeed) string {
	t.Helper()
	root := syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionImportRunResidualProjectName, productionImportRunResidualProjectID,
		admission.CurrentSchemaVersion, productionImportRunResidualSchema)
	projectPath := filepath.Join(library, productionImportRunResidualProjectName)
	productionImportRunResidualSeedRows(t, projectPath, seeds...)
	return projectPath
}

// productionImportRunResidualSeedRows commits the seeded runs, their frozen sources
// and chapters through hand-written INSERTs, then checkpoints the file.
func productionImportRunResidualSeedRows(t *testing.T, projectPath string, seeds ...productionImportRunResidualSeed) {
	t.Helper()
	statements := []string{productionImportRunResidualSeedCore}
	for _, seed := range seeds {
		statements = append(statements, fmt.Sprintf(`INSERT INTO import_runs (id, purpose, root_run_id,
 effect_namespace, source_fingerprint, manifest_fingerprint, source_display_json, locale, stage, status,
 resumable, execution_owner, execution_epoch, lease_expires_at, total_chapters, total_content_size)
 VALUES ('%s', '%s', '', 'import:%s:%s', '%s', '%s', '[{"displayName":"门.txt","mediaType":"text/plain","size":37}]',
 'zh-CN', '%s', '%s', %d, '%s', %d, %d, 0, 0)`,
			seed.ID, seed.Purpose, seed.Purpose, seed.ID, productionImportRunDigest("source:"+seed.ID),
			productionImportRunResidualZeroFingerprint, seed.Stage, seed.Status, seed.Resumable,
			seed.ExecutionOwner, seed.ExecutionEpoch, seed.LeaseExpiresAt))
		for index, source := range seed.Sources {
			words, size := int64(0), int64(0)
			for _, chapter := range source.Chapters {
				words += productionImportRunResidualWords(chapter.Body)
				size += int64(len(chapter.Body))
			}
			statements = append(statements, fmt.Sprintf(`INSERT INTO import_run_sources (run_id, source_index,
 source_id, source_fingerprint, legacy_source_fingerprint, display_json, status, manifest_fingerprint,
 chapter_count, content_size, word_count, last_error)
 VALUES ('%s', %d, '%s', '%s', '', '{"displayName":"门.txt","mediaType":"text/plain","size":37}', '%s', '', %d, %d, %d, '')`,
				seed.ID, int64(index), source.ID, productionImportRunDigest("source-file:"+source.ID),
				source.Status, int64(len(source.Chapters)), size, words))
			for number, chapter := range source.Chapters {
				statements = append(statements, fmt.Sprintf(`INSERT INTO import_run_source_chapters (run_id,
 source_id, source_chapter_number, title, content_fingerprint, content_size, word_count, content_snapshot)
 VALUES ('%s', '%s', %d, '%s', '%s', %d, %d, '%s')`,
					seed.ID, source.ID, int64(number+1), chapter.Title,
					productionImportRunDigest(chapter.Body), int64(len(chapter.Body)),
					productionImportRunResidualWords(chapter.Body), chapter.Body))
			}
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
	productionEditorCheckpoint(t, db, database)
}

// productionImportRunResidualSeedReceipt writes one committed
// project-writing-style receipt with the store's own prepare and commit. The lane
// publishes no prepare binder (see the adapter's header), so this is how the file
// comes to hold the receipt the read subtest asserts against: exactly what the
// desktop app leaves behind, hashed by the store rather than by the test.
func productionImportRunResidualSeedReceipt(t *testing.T, projectPath, runID string, epoch int64) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	repository := store.NewImportRuns(db)
	execution := store.ImportRunExecution{Owner: productionImportRunResidualOwner, Epoch: epoch}
	prepared, err := repository.PrepareEffectReceipt(ctx, execution, store.ImportRunPrepareEffectReceiptRequest{
		RunID: runID, Stage: "style", BatchID: "done", EffectKey: "writing-style",
		Kind: "project-writing-style",
		// No generationRunHandle: this file's seeding call supplies no proof either, so
		// the handle-less shape is the only one the store accepts on both legs. The
		// adapter's header explains why the lane itself refuses the operation instead of
		// rewriting receipts into this shape.
		Payload: map[string]any{"writingStyle": productionImportRunResidualStyle},
	}, nil)
	if err != nil {
		_ = db.Close()
		t.Fatalf("seed receipt: %v", err)
	}
	receipt, _ := prepared.(map[string]any)
	if receipt == nil || productionImportRunWriteText(receipt, "state") != "prepared" {
		_ = db.Close()
		t.Fatalf("seed receipt prepared as %v", prepared)
	}
	committed, err := repository.CommitEffectReceipt(ctx, runID, "style", "done", execution, nil)
	if err != nil {
		_ = db.Close()
		t.Fatalf("seed receipt commit: %v", err)
	}
	committedReceipt, _ := committed["receipt"].(map[string]any)
	if committedReceipt == nil || productionImportRunWriteText(committedReceipt, "state") != "committed" {
		_ = db.Close()
		t.Fatalf("seed receipt committed as %v", committed)
	}
	productionEditorCheckpoint(t, db, database)
}

// productionImportRunResidualLane opens the real production lane over the disposable
// data root and installs exactly two binders on it: the project lifecycle (so
// project:open can issue the session lease these calls name) and this batch's four
// residual names. Nothing else is installed, so every other dispatch name keeps
// failing closed at 501.
func productionImportRunResidualLane(t *testing.T, root string) http.Handler {
	t.Helper()
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, productionConfig{
		Schema: productionImportRunResidualContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-import-run-residuals")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	if bindErr := newProductionImportRunResiduals(open.durable).Bind(server); bindErr != nil {
		t.Fatal(bindErr)
	}
	return server
}

// productionImportRunResidualFixture is one opened residual lane with one opened
// project.
type productionImportRunResidualFixture struct {
	t           *testing.T
	projectPath string
	database    string
	author      *productionDurableClient
	frozen      map[string]any
}

// newProductionImportRunResidualFixture seeds the given runs, opens the lane and
// opens the project, returning the frozen three-key context every call carries.
func newProductionImportRunResidualFixture(t *testing.T, seeds ...productionImportRunResidualSeed) *productionImportRunResidualFixture {
	t.Helper()
	projectPath := productionImportRunResidualSeedProject(t, seeds...)
	root := filepath.Dir(filepath.Dir(projectPath))
	handler := productionImportRunResidualLane(t, root)
	author := newProductionDurableClient(t, handler, productionImportRunResidualTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "import-run-residual-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted residual project: %+v", opened)
	}
	if opened.Project["id"] != productionImportRunResidualProjectID || opened.Project["path"] != projectPath {
		t.Fatalf("project:open returned the wrong project identity: %v", opened.Project)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return &productionImportRunResidualFixture{
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

// newProductionImportRunResidualReceiptFixture seeds one running style-stage run and
// the committed receipt its read subtest asserts against, then opens the lane.
func newProductionImportRunResidualReceiptFixture(t *testing.T) *productionImportRunResidualFixture {
	t.Helper()
	projectPath := productionImportRunResidualSeedProject(t, productionImportRunResidualSeed{
		ID: productionImportRunResidualStyleRunID, Purpose: "reference", Stage: "style", Status: "running",
		Resumable: 1, ExecutionOwner: productionImportRunResidualOwner,
		ExecutionEpoch: productionImportRunResidualStyleEpoch,
		LeaseExpiresAt: productionImportRunResidualLiveLease(),
	})
	productionImportRunResidualSeedReceipt(t, projectPath, productionImportRunResidualStyleRunID,
		productionImportRunResidualStyleEpoch)
	root := filepath.Dir(filepath.Dir(projectPath))
	handler := productionImportRunResidualLane(t, root)
	author := newProductionDurableClient(t, handler, productionImportRunResidualTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "import-run-residual-open-2", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted residual project: %+v", opened)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return &productionImportRunResidualFixture{
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

// staleContext is the same frozen session with a lane lease this lane never issued.
func (f *productionImportRunResidualFixture) staleContext() map[string]any {
	return map[string]any{
		"projectId":   f.frozen["projectId"],
		"projectPath": f.frozen["projectPath"],
		"leaseId":     productionImportRunResidualStaleLease,
	}
}

// result sends one call and returns its decoded success envelope. A non-200 is
// fatal: a bound name answers 200 only after its transition ran.
func (f *productionImportRunResidualFixture) result(operation string, args []any) map[string]any {
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

// value sends one call whose result is NOT an envelope and returns the decoded
// value plus its exact JSON text, so a null can be asserted as the reference's own
// shape instead of as a missing field.
func (f *productionImportRunResidualFixture) value(operation string, args []any) (any, string) {
	f.t.Helper()
	response := f.author.call(f.t, operation, args)
	if response.Code != http.StatusOK {
		f.t.Fatalf("%s: %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if strings.Contains(response.Body.String(), f.projectPath) {
		f.t.Fatalf("%s echoed the project path: %s", operation, response.Body.String())
	}
	raw := strings.TrimSpace(string(wire.Result))
	if raw == "" || raw == "null" {
		return nil, raw
	}
	var value any
	if err := json.Unmarshal(wire.Result, &value); err != nil {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	return value, raw
}

// refused asserts one refused call carries the code the STORE itself raised. This
// family is not the draft family: a refusal is a transport error with a symbolic
// code, and a 501 or a generic message here means the binder was never reached or
// the detail was swallowed.
func (f *productionImportRunResidualFixture) refused(operation string, args []any, code string) {
	f.t.Helper()
	response := f.author.call(f.t, operation, args)
	if response.Code == http.StatusOK {
		f.t.Fatalf("%s answered 200 for a refused call: %s", operation, response.Body.String())
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

// productionImportRunResidualStaysUnbound pins a name this slice deliberately does
// not publish: it must still fail closed at 501 with the migration code rather than
// answer a handler that could only refuse or, worse, write.
func productionImportRunResidualStaysUnbound(t *testing.T, client *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("%s answered %d, but this file publishes no handler for it: %s",
			operation, response.Code, response.Body.String())
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if wire.Error.Code != "GO_OPERATION_NOT_MIGRATED" {
		t.Fatalf("%s failed closed with %q instead of GO_OPERATION_NOT_MIGRATED: %s",
			operation, wire.Error.Code, response.Body.String())
	}
}

// TestProductionImportRunResidualsServeAdmittedAuthorDatabase pins the four bound
// names and the four refusals: each commit is read back off disk over a separate
// mode=ro connection, each transition's own precondition is asserted on a refused
// replay, and every refused call is proved to leave the admitted bytes identical.
func TestProductionImportRunResidualsServeAdmittedAuthorDatabase(t *testing.T) {
	// db:import-run-finalize-parsing: a fully parsed run is adopted into a prepared
	// one, its chapters are copied and mapped, and a replay is NOT a success.
	t.Run("db:import-run-finalize-parsing", func(t *testing.T) {
		first := productionImportRunResidualChapter{Title: "第一章 门", Body: "门开了，灯还没有亮。"}
		second := productionImportRunResidualChapter{Title: "第二章 灯", Body: "灯亮了，门却关上了。"}
		f := newProductionImportRunResidualFixture(t, productionImportRunResidualSeed{
			ID: productionImportRunResidualParseRunID, Purpose: "reference", Stage: "parsing",
			Status: "ready", Resumable: 1,
			Sources: []productionImportRunResidualSource{{
				ID: "residual-source-a", Status: "completed",
				Chapters: []productionImportRunResidualChapter{first, second},
			}},
		})
		result := f.result(productionImportRunResidualFinalizeParsingName, []any{
			productionImportRunResidualParseRunID, f.projectPath, f.frozen,
		})
		preparation, _ := result["preparation"].(map[string]any)
		if preparation == nil {
			t.Fatalf("finalize-parsing returned no preparation: %v", result)
		}
		if productionImportRunWriteText(preparation, "classification") != "new" {
			t.Fatalf("finalize-parsing classified the run as %v", preparation["classification"])
		}
		run, _ := preparation["run"].(map[string]any)
		if run == nil || productionImportRunWriteText(run, "stage") != "prepared" ||
			productionImportRunWriteText(run, "status") != "ready" {
			t.Fatalf("finalize-parsing did not report a prepared run: %v", preparation)
		}
		numbers, _ := preparation["newChapterNumbers"].([]any)
		if len(numbers) != 2 || numbers[0] != float64(1) || numbers[1] != float64(2) {
			t.Fatalf("finalize-parsing adopted %v; want the two frozen chapters numbered 1 and 2", preparation["newChapterNumbers"])
		}
		inspection, _ := preparation["inspection"].(map[string]any)
		if inspection == nil {
			t.Fatalf("finalize-parsing returned no inspection: %v", preparation)
		}
		if productionImportRunWriteFloat(t, inspection, "chapterCount") != 2 ||
			productionImportRunWriteFloat(t, inspection, "sourceCount") != 1 {
			t.Fatalf("finalize-parsing reported %v chapters from %v sources", inspection["chapterCount"], inspection["sourceCount"])
		}
		if productionImportRunWriteText(inspection, "inspectionId") != productionImportRunResidualParseRunID ||
			productionImportRunWriteText(inspection, "purpose") != "reference" {
			t.Fatalf("finalize-parsing reported the wrong inspection identity: %v", inspection)
		}
		names, _ := inspection["sourceDisplayNames"].([]any)
		if len(names) != 1 || names[0] != "门.txt" {
			t.Fatalf("finalize-parsing reported source names %v", inspection["sourceDisplayNames"])
		}
		wantWords := productionImportRunResidualWords(first.Body) + productionImportRunResidualWords(second.Body)
		wantBytes := int64(len(first.Body) + len(second.Body))
		row := productionImportRunWriteRow(t, f.database, `SELECT stage, status, resumable, manifest_fingerprint,
 total_chapters, total_content_size, manifest_chapter_count, manifest_content_size, manifest_word_count
 FROM import_runs WHERE id = ?`, productionImportRunResidualParseRunID)
		if productionImportRunWriteText(row, "stage") != "prepared" ||
			productionImportRunWriteText(row, "status") != "ready" {
			t.Fatalf("finalize-parsing did not commit the prepared stage: %v", row)
		}
		if productionImportRunWriteText(row, "manifest_fingerprint") == productionImportRunResidualZeroFingerprint {
			t.Fatalf("finalize-parsing committed no manifest fingerprint: %v", row)
		}
		if productionImportRunWriteNumber(row, "total_chapters") != 2 ||
			productionImportRunWriteNumber(row, "manifest_chapter_count") != 2 {
			t.Fatalf("finalize-parsing committed the wrong chapter counts: %v", row)
		}
		if productionImportRunWriteNumber(row, "total_content_size") != wantBytes ||
			productionImportRunWriteNumber(row, "manifest_content_size") != wantBytes ||
			productionImportRunWriteNumber(row, "manifest_word_count") != wantWords {
			t.Fatalf("finalize-parsing committed the wrong manifest sums: %v (want %d bytes, %d words)", row, wantBytes, wantWords)
		}
		chapters := productionImportRunWriteQuery(t, f.database, `SELECT chapter_number, title, content_snapshot
 FROM import_run_chapters WHERE run_id = ? ORDER BY chapter_number`, productionImportRunResidualParseRunID)
		if len(chapters) != 2 || productionImportRunWriteText(chapters[0], "title") != first.Title ||
			productionImportRunWriteText(chapters[1], "title") != second.Title ||
			productionImportRunWriteText(chapters[1], "content_snapshot") != second.Body {
			t.Fatalf("finalize-parsing copied %v chapters", chapters)
		}
		mappings := productionImportRunWriteQuery(t, f.database, `SELECT chapter_number, source_id
 FROM import_source_chapter_map WHERE purpose = ? ORDER BY chapter_number`, "reference")
		if len(mappings) != 2 || productionImportRunWriteNumber(mappings[0], "chapter_number") != 1 ||
			productionImportRunWriteNumber(mappings[1], "chapter_number") != 2 ||
			productionImportRunWriteText(mappings[1], "source_id") != "residual-source-a" {
			t.Fatalf("finalize-parsing mapped %v chapters", mappings)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		// Not idempotent, by design: the run has left 'parsing'.
		f.refused(productionImportRunResidualFinalizeParsingName, []any{
			productionImportRunResidualParseRunID, f.projectPath, f.frozen,
		}, "IMPORT_RUN_PARSING_CLOSED")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused second finalize-parsing")
	})

	// db:import-run-restart: the run the user gave up on is fenced and the next run
	// is created from its frozen facts. A run that is no longer resumable is NOT.
	t.Run("db:import-run-restart", func(t *testing.T) {
		f := newProductionImportRunResidualFixture(t, productionImportRunResidualSeed{
			ID: productionImportRunResidualOldRunID, Purpose: "reference", Stage: "parsing",
			Status: "failed", Resumable: 1, ExecutionEpoch: 3,
			Sources: []productionImportRunResidualSource{{
				ID: "residual-source-b", Status: "completed",
				Chapters: []productionImportRunResidualChapter{{Title: "第一章 灰", Body: "灰落下来了。"}},
			}},
		})
		result := f.result(productionImportRunResidualRestartName, []any{
			productionImportRunResidualOldRunID, productionImportRunResidualNextRunID, f.projectPath, f.frozen,
		})
		run, _ := result["run"].(map[string]any)
		if run == nil || productionImportRunWriteText(run, "id") != productionImportRunResidualNextRunID ||
			productionImportRunWriteText(run, "stage") != "parsing" ||
			productionImportRunWriteText(run, "status") != "ready" {
			t.Fatalf("restart did not report the next run: %v", result)
		}
		if run["resumable"] != false {
			t.Fatalf("restart reported the new run as resumable: %v", run["resumable"])
		}
		old := productionImportRunWriteRow(t, f.database, `SELECT stage, status, resumable, execution_owner,
 execution_epoch, lease_expires_at, last_error FROM import_runs WHERE id = ?`, productionImportRunResidualOldRunID)
		if productionImportRunWriteText(old, "stage") != "parsing" ||
			productionImportRunWriteText(old, "status") != "failed" {
			t.Fatalf("restart re-staged the run it fenced: %v", old)
		}
		if productionImportRunWriteNumber(old, "resumable") != 0 ||
			productionImportRunWriteText(old, "execution_owner") != "" ||
			productionImportRunWriteNumber(old, "execution_epoch") != 4 ||
			productionImportRunWriteNumber(old, "lease_expires_at") != 0 {
			t.Fatalf("restart did not retire the old run's lease: %v", old)
		}
		if productionImportRunWriteText(old, "last_error") != "Restarted by user" {
			t.Fatalf("restart left the old run's last error as %q", productionImportRunWriteText(old, "last_error"))
		}
		next := productionImportRunWriteRow(t, f.database, `SELECT purpose, stage, status, root_run_id,
 manifest_fingerprint, total_chapters, manifest_chapter_count FROM import_runs WHERE id = ?`,
			productionImportRunResidualNextRunID)
		if productionImportRunWriteText(next, "purpose") != "reference" ||
			productionImportRunWriteText(next, "stage") != "parsing" ||
			productionImportRunWriteText(next, "status") != "ready" {
			t.Fatalf("restart created the wrong run: %v", next)
		}
		// A run that never left 'parsing' parsed nothing that was adopted, so the copy
		// starts with zeroed bookkeeping, pending sources and no chapters at all.
		if productionImportRunWriteText(next, "manifest_fingerprint") != productionImportRunResidualZeroFingerprint {
			t.Fatalf("restart carried a manifest fingerprint into a parsing run: %v", next)
		}
		if productionImportRunWriteNumber(next, "total_chapters") != 0 ||
			productionImportRunWriteNumber(next, "manifest_chapter_count") != 0 {
			t.Fatalf("restart carried chapter bookkeeping into a parsing run: %v", next)
		}
		sources := productionImportRunWriteQuery(t, f.database, `SELECT status, chapter_count, word_count,
 manifest_fingerprint FROM import_run_sources WHERE run_id = ? ORDER BY source_index`,
			productionImportRunResidualNextRunID)
		if len(sources) != 1 || productionImportRunWriteText(sources[0], "status") != "pending" ||
			productionImportRunWriteNumber(sources[0], "chapter_count") != 0 ||
			productionImportRunWriteNumber(sources[0], "word_count") != 0 ||
			productionImportRunWriteText(sources[0], "manifest_fingerprint") != "" {
			t.Fatalf("restart copied the sources as %v", sources)
		}
		if copied := productionImportRunWriteQuery(t, f.database,
			`SELECT COUNT(*) AS total FROM import_run_chapters WHERE run_id = ?`,
			productionImportRunResidualNextRunID); len(copied) != 1 ||
			productionImportRunWriteNumber(copied[0], "total") != 0 {
			t.Fatalf("restart copied chapters into a parsing run: %v", copied)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		f.refused(productionImportRunResidualRestartName, []any{
			productionImportRunResidualOldRunID, "residual-third-run", f.projectPath, f.frozen,
		}, "IMPORT_RUN_NOT_RESTARTABLE")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused second restart")
	})

	// db:import-run-fail: the run records its own failure at the stage it was in and
	// retires the lease. A replay, and a stage the run never reached, are refused.
	t.Run("db:import-run-fail", func(t *testing.T) {
		f := newProductionImportRunResidualFixture(t,
			productionImportRunResidualSeed{
				ID: productionImportRunResidualLiveRunID, Purpose: "reference", Stage: "knowledge",
				Status: "running", Resumable: 1, ExecutionOwner: productionImportRunResidualOwner,
				ExecutionEpoch: 7, LeaseExpiresAt: productionImportRunResidualLiveLease(),
			},
			productionImportRunResidualSeed{
				ID: productionImportRunResidualOtherRunID, Purpose: "reference", Stage: "knowledge",
				Status: "running", Resumable: 1, ExecutionOwner: productionImportRunResidualOwner,
				ExecutionEpoch: 7, LeaseExpiresAt: productionImportRunResidualLiveLease(),
			},
		)
		// The caller's own sentence, stored as given.
		reason := "导入在知识整理阶段中断，可重试。"
		result := f.result(productionImportRunResidualFailName, []any{
			productionImportRunResidualLiveRunID, "knowledge", reason,
			productionImportRunResidualLeaseWire(7), f.projectPath, f.frozen,
		})
		run, _ := result["run"].(map[string]any)
		if run == nil || productionImportRunWriteText(run, "status") != "failed" ||
			productionImportRunWriteText(run, "lastError") != reason {
			t.Fatalf("fail did not report the failed run: %v", result)
		}
		row := productionImportRunWriteRow(t, f.database, `SELECT status, resumable, execution_owner,
 execution_epoch, lease_expires_at, last_error FROM import_runs WHERE id = ?`,
			productionImportRunResidualLiveRunID)
		if productionImportRunWriteText(row, "status") != "failed" ||
			productionImportRunWriteNumber(row, "resumable") != 1 {
			t.Fatalf("fail did not commit a resumable failure: %v", row)
		}
		if productionImportRunWriteText(row, "execution_owner") != "" ||
			productionImportRunWriteNumber(row, "execution_epoch") != 8 ||
			productionImportRunWriteNumber(row, "lease_expires_at") != 0 {
			t.Fatalf("fail did not retire the lease: %v", row)
		}
		if productionImportRunWriteText(row, "last_error") != reason {
			t.Fatalf("fail stored %q; want the caller's own sentence %q",
				productionImportRunWriteText(row, "last_error"), reason)
		}
		settled := productionImportRunWriteSnapshot(t, f.database)
		// The run is no longer running, so the lease is stale before the reason is read.
		f.refused(productionImportRunResidualFailName, []any{
			productionImportRunResidualLiveRunID, "knowledge", reason,
			productionImportRunResidualLeaseWire(7), f.projectPath, f.frozen,
		}, "IMPORT_RUN_EXECUTION_STALE")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused fail replay")
		// The store compares the stage AFTER the lease, so a live execution naming a
		// stage the run never reached is its own refusal.
		f.refused(productionImportRunResidualFailName, []any{
			productionImportRunResidualOtherRunID, "global", reason,
			productionImportRunResidualLeaseWire(7), f.projectPath, f.frozen,
		}, "IMPORT_RUN_STAGE_MISMATCH")
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the refused fail of a stage the run never reached")
		untouched := productionImportRunWriteRow(t, f.database, `SELECT status, stage, execution_epoch
 FROM import_runs WHERE id = ?`, productionImportRunResidualOtherRunID)
		if productionImportRunWriteText(untouched, "status") != "running" ||
			productionImportRunWriteText(untouched, "stage") != "knowledge" ||
			productionImportRunWriteNumber(untouched, "execution_epoch") != 7 {
			t.Fatalf("a refused fail moved the run it named: %v", untouched)
		}
	})

	// db:import-run-effect-receipt-get: a pure read of the receipt ledger, in the
	// reference's own shape - the receipt itself, or null when there is none - and it
	// writes nothing at all. The receipt it reads was written by the store's own
	// prepare+commit (see the file header), so the shape is a real one.
	t.Run("db:import-run-effect-receipt-get", func(t *testing.T) {
		f := newProductionImportRunResidualReceiptFixture(t)
		// Snapshot BEFORE the reads: a read that writes anything at all would show up.
		settled := productionImportRunWriteSnapshot(t, f.database)
		value, _ := f.value(productionImportRunResidualReceiptGetName, []any{
			productionImportRunResidualStyleRunID, "style", "done", f.projectPath, f.frozen,
		})
		receipt, _ := value.(map[string]any)
		if receipt == nil {
			t.Fatalf("effect-receipt-get answered %v for a committed receipt", value)
		}
		if productionImportRunWriteText(receipt, "state") != "committed" ||
			productionImportRunWriteText(receipt, "effectKey") != "writing-style" ||
			productionImportRunWriteText(receipt, "kind") != "project-writing-style" ||
			productionImportRunWriteText(receipt, "stage") != "style" ||
			productionImportRunWriteText(receipt, "batchId") != "done" ||
			productionImportRunWriteText(receipt, "runId") != productionImportRunResidualStyleRunID {
			t.Fatalf("effect-receipt-get reported %v", receipt)
		}
		if productionImportRunWriteText(receipt, "effectNamespace") !=
			"import:reference:"+productionImportRunResidualStyleRunID {
			t.Fatalf("effect-receipt-get reported the namespace %q",
				productionImportRunWriteText(receipt, "effectNamespace"))
		}
		effect, _ := receipt["effectReceipt"].(map[string]any)
		if effect == nil || productionImportRunWriteText(effect, "writingStyle") != productionImportRunResidualStyle {
			t.Fatalf("effect-receipt-get reported the effect %v", receipt["effectReceipt"])
		}
		// The effect the receipt claims really is the column the commit wrote.
		core := productionImportRunWriteRow(t, f.database,
			`SELECT writing_style FROM project_core WHERE id = 'main'`)
		if productionImportRunWriteText(core, "writing_style") != productionImportRunResidualStyle {
			t.Fatalf("the committed effect is not in project_core: %v", core)
		}
		stored := productionImportRunWriteRow(t, f.database, `SELECT state, effect_receipt_json
 FROM import_run_receipts WHERE run_id = ? AND stage = ? AND batch_id = ?`,
			productionImportRunResidualStyleRunID, "style", "done")
		if productionImportRunWriteText(stored, "state") != "committed" ||
			!strings.Contains(productionImportRunWriteText(stored, "effect_receipt_json"), productionImportRunResidualStyle) {
			t.Fatalf("the receipt row effect-receipt-get read is %v", stored)
		}
		// No receipt for that batch: the reference answers null, not a refusal.
		absent, raw := f.value(productionImportRunResidualReceiptGetName, []any{
			productionImportRunResidualStyleRunID, "style", "no-such-batch", f.projectPath, f.frozen,
		})
		if absent != nil || raw != "null" {
			t.Fatalf("effect-receipt-get answered %q for a missing receipt; want null", raw)
		}
		productionImportRunWriteAssertUnchanged(t, f.database, settled,
			"the two effect-receipt-get reads")
	})

	// The four names this slice refuses. Each stays at 501 GO_OPERATION_NOT_MIGRATED
	// rather than being published as a handler that could only refuse - or, for the
	// two receipt writes, that could accept unproven generation state.
	t.Run("refused", func(t *testing.T) {
		f := newProductionImportRunResidualFixture(t)
		settled := productionImportRunWriteSnapshot(t, f.database)
		execution := productionImportRunResidualLeaseWire(3)
		// db:import-run-author-preview and db:import-run-prepare-inspection: no
		// server-side inspection handoff (importsource.Store.Peek returns metadata only
		// and Consume needs the preview CAS this lane never puts on the wire,
		// internal/importsource/inspection.go:158-169 and
		// internal/transport/import_selection.go:110-116), and no author-purpose
		// prepare() (internal/store/importrun_lifecycle.go:26-34).
		productionImportRunResidualStaysUnbound(t, f.author,
			productionImportRunResidualAuthorPreviewName, []any{"inspection-1", f.projectPath, f.frozen})
		productionImportRunResidualStaysUnbound(t, f.author,
			productionImportRunResidualPrepareInspectionName, []any{
				map[string]any{"inspectionId": "inspection-1", "runId": productionImportRunResidualParseRunID,
					"purpose": "reference", "locale": "zh-CN"},
				f.projectPath, f.frozen,
			})
		// db:import-run-effect-receipt-prepare and -commit: no generation-source proof.
		// The reference always supplies one (electron/controllers/db-controller.ts:425,
		// :437) and every real style receipt carries a generationRunHandle
		// (electron/services/__tests__/import-generation.test.ts:197), so a nil proof
		// would accept an unproven handle at prepare and then refuse it at commit
		// (internal/store/importrun_receipts.go:984-989 against :1002-1003).
		productionImportRunResidualStaysUnbound(t, f.author,
			productionImportRunResidualReceiptPrepareName, []any{
				map[string]any{"runId": productionImportRunResidualStyleRunID, "stage": "style",
					"batchId": "done", "effectKey": "writing-style", "kind": "project-writing-style",
					"payload": map[string]any{"writingStyle": productionImportRunResidualStyle}},
				execution, f.projectPath, f.frozen,
			})
		productionImportRunResidualStaysUnbound(t, f.author,
			productionImportRunResidualReceiptCommitName, []any{
				productionImportRunResidualStyleRunID, "style", "done", execution, f.projectPath, f.frozen,
			})
		productionImportRunWriteAssertUnchanged(t, f.database, settled, "the four refused names")
	})
}

// TestProductionImportRunResidualsRefuseWithoutALiveLease proves none of the four
// bound names can run on a frozen context naming a lease this lane never issued,
// and that no refusal writes: the fence is the admission, not the store's guards.
func TestProductionImportRunResidualsRefuseWithoutALiveLease(t *testing.T) {
	f := newProductionImportRunResidualFixture(t,
		productionImportRunResidualSeed{
			ID: productionImportRunResidualParseRunID, Purpose: "reference", Stage: "parsing",
			Status: "ready", Resumable: 1,
			Sources: []productionImportRunResidualSource{{
				ID: "residual-source-c", Status: "completed",
				Chapters: []productionImportRunResidualChapter{{Title: "第一章 影", Body: "影子长了。"}},
			}},
		},
		productionImportRunResidualSeed{
			ID: productionImportRunResidualLiveRunID, Purpose: "reference", Stage: "knowledge",
			Status: "running", Resumable: 1, ExecutionOwner: productionImportRunResidualOwner,
			ExecutionEpoch: 5, LeaseExpiresAt: productionImportRunResidualLiveLease(),
		},
	)
	settled := productionImportRunWriteSnapshot(t, f.database)
	stale := f.staleContext()
	requests := []struct {
		operation string
		args      []any
	}{
		{productionImportRunResidualFinalizeParsingName, []any{
			productionImportRunResidualParseRunID, f.projectPath, stale}},
		{productionImportRunResidualRestartName, []any{
			productionImportRunResidualParseRunID, "residual-fenced-next", f.projectPath, stale}},
		{productionImportRunResidualFailName, []any{
			productionImportRunResidualLiveRunID, "knowledge", "不应被写下的失败原因",
			productionImportRunResidualLeaseWire(5), f.projectPath, stale}},
		{productionImportRunResidualReceiptGetName, []any{
			productionImportRunResidualLiveRunID, "style", "done", f.projectPath, stale}},
	}
	for _, request := range requests {
		response := f.author.call(t, request.operation, request.args)
		if response.Code == http.StatusOK {
			t.Fatalf("%s served a frozen context this lane never issued: %s",
				request.operation, response.Body.String())
		}
		var wire struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatalf("%s: %v %s", request.operation, err, response.Body.String())
		}
		if wire.Error.Code == "GO_OPERATION_NOT_MIGRATED" {
			t.Fatalf("%s is not bound at all: %s", request.operation, response.Body.String())
		}
		productionImportRunWriteAssertUnchanged(t, f.database, settled,
			"the unleased "+request.operation)
	}
	row := productionImportRunWriteRow(t, f.database, `SELECT stage, status, resumable, execution_epoch,
 last_error FROM import_runs WHERE id = ?`, productionImportRunResidualLiveRunID)
	if productionImportRunWriteText(row, "status") != "running" ||
		productionImportRunWriteNumber(row, "execution_epoch") != 5 ||
		productionImportRunWriteText(row, "last_error") != "" {
		t.Fatalf("an unleased request moved the run it named: %v", row)
	}
	parsing := productionImportRunWriteRow(t, f.database, `SELECT stage, resumable FROM import_runs WHERE id = ?`,
		productionImportRunResidualParseRunID)
	if productionImportRunWriteText(parsing, "stage") != "parsing" ||
		productionImportRunWriteNumber(parsing, "resumable") != 1 {
		t.Fatalf("an unleased request moved the parsing run: %v", parsing)
	}
	if copied := productionImportRunWriteQuery(t, f.database,
		`SELECT COUNT(*) AS total FROM import_run_chapters WHERE run_id = ?`,
		productionImportRunResidualParseRunID); len(copied) != 1 ||
		productionImportRunWriteNumber(copied[0], "total") != 0 {
		t.Fatalf("an unleased finalize copied chapters: %v", copied)
	}
}
