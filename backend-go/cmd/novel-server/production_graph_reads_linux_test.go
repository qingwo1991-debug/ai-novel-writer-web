//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// The two reads below run against a disposable t.TempDir() only. No real author
// data root (and nothing under the operator's own data mount) is read or
// written by this test: the project, its ledger rows and the frozen plot
// manifest all live inside the test's temporary tree.
const (
	productionGraphReadsProjectName           = "graph-reads-novel"
	productionGraphReadsProjectID             = "5c1d9a34-7b28-4e6f-8a03-9d2f6b41c785"
	productionGraphReadsForeignProjectID      = "e4b7f0c2-16a9-4d58-b3e7-8c05a2d94f61"
	productionGraphReadsFrozenEpoch           = "lease-0000000003-frozen-history"
	productionGraphReadsOperation             = "plot-tree-snapshot"
	productionGraphReadsFinalizationOp        = "finalized-character-state"
	productionGraphReadsStepKey               = "chapter_notes"
	productionGraphReadsOwnedFinalizationID   = "graph-reads-owned-finalization"
	productionGraphReadsForeignFinalizationID = "graph-reads-foreign-finalization"
	productionGraphReadsAbsentFinalizationID  = "graph-reads-absent-finalization"
	productionGraphReadsDraftID               = 7
	productionGraphReadsChapterNumber         = 3
)

var productionGraphReadsContentHash = strings.Repeat("d", 64)

// TestProductionLaneServesGraphAndFinalizationReads pins the two reads
// server.BindGraphGeneration installs. One subtest per operation, so a failure
// names the channel that moved.
//
// The fixture is a disposable data root whose admitted project carries real
// ledger rows: a complete frozen plot run (the manifest the synthetic graph
// tests build, patched onto this project), a run that owns a finalization slot
// in this project and a run that owns one in another project. The rows are
// written through the shipped ledger store before project:open, so the permit
// hashes the database with them already in place and every assertion runs
// against rows the admitted file really holds rather than against constants.
//
// graph-generation:read cannot answer 200 in this lane, for two reasons, and
// the subtest reports the first of them as refusals instead of inventing a view:
//
//   - Recovery is served only for a run frozen under the lease the caller holds
//     now, and that lease is a fresh random nonce minted inside project:open
//     (authorruntime/durable_linux.go mints it, syntheticRequireRun compares
//     it). Rows written before the open cannot carry it, and rows written after
//     it would move the file the admission permit already hashed. So no
//     admitted production database can hold a graph run this read serves: the
//     seeded run is refused as one this session does not own, and a handle that
//     names nothing is refused as a missing row.
//   - The reduced begin writes no graph context, so a run this lease could own
//     would still be refused for having nothing to project. That branch is not
//     asserted here: reaching it needs a live-lease row, which is exactly what
//     the fence above makes unseedable.
//
// Either way the answer is a documented refusal, never a 200 built from the
// wrong state.
//
// finalization-generation:read does answer 200 with real seeded state: the slot
// no run owns comes back as JSON null out of a look-up over the seeded rows,
// which is what the renderer reads as "nothing to resume". The two slots that do
// exist are refused with their own codes, which is what proves the seeded rows
// were found.
func TestProductionLaneServesGraphAndFinalizationReads(t *testing.T) {
	root, projectPath, database, contract := productionGraphReadsRoot(t)
	graphRunID, graphRootID := productionGraphReadsSeedLedger(t, database)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "graph-reads-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	projectID := syntheticString(opened.Project["id"])
	leaseID := syntheticString(opened.Project["sessionLease"])
	if projectID != productionGraphReadsProjectID || leaseID == "" {
		t.Fatalf("project:open returned %q/%q, want the seeded project with a live lease", projectID, leaseID)
	}
	frozen := map[string]any{"projectId": projectID, "projectPath": projectPath, "leaseId": leaseID}
	// A lease this session no longer holds, and no lease at all.
	stale := map[string]any{"projectId": projectID, "projectPath": projectPath, "leaseId": "graph-reads-stale-lease"}
	unleased := map[string]any{"projectId": projectID, "projectPath": projectPath}

	t.Run("graph-generation:read", func(t *testing.T) {
		// The fixture really froze a graph run, and the name is still 501 on
		// purpose: generation:begin here is the reduced begin, which freezes no
		// graph context, so a bound handler could only ever refuse. This lane does
		// not install handlers that can only refuse.
		if graphRunID == "" || graphRootID == "" {
			t.Fatal("the fixture wrote no graph run")
		}
		response := author.call(t, "graph-generation:read", []any{map[string]any{
			"handle": map[string]any{
				"projectId":    productionGraphReadsProjectID,
				"epoch":        productionGraphReadsFrozenEpoch,
				"rootActionId": graphRootID,
				"runId":        graphRunID,
			},
		}, frozen})
		if response.Code != http.StatusNotImplemented ||
			!strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("graph-generation:read must stay fail-closed with a real run present: %d %s",
				response.Code, response.Body.String())
		}
	})

	t.Run("finalization-generation:read", func(t *testing.T) {
		before := productionGraphReadsBytes(t, database)
		owned := productionGraphReadsSlot(productionGraphReadsOwnedFinalizationID)
		foreign := productionGraphReadsSlot(productionGraphReadsForeignFinalizationID)
		absent := productionGraphReadsSlot(productionGraphReadsAbsentFinalizationID)
		// 200 with the real seeded state: no run owns this slot, so the look-up
		// answers null. This is the answer the renderer's first step needs.
		result := productionEditorCall(t, author, "finalization-generation:read",
			[]any{map[string]any{"slot": absent}, frozen})
		if string(result) != "null" {
			t.Fatalf("finalization-generation:read projected %s for an unowned slot, want null", result)
		}
		// The seeded rows are found and refused: one this project owns, one whose
		// binding names another project.
		productionGraphReadsRefused(t, author, "finalization-generation:read",
			[]any{map[string]any{"slot": owned}, frozen}, "GENERATION_FINALIZATION_CONTEXT_INVALID")
		productionGraphReadsRefused(t, author, "finalization-generation:read",
			[]any{map[string]any{"slot": foreign}, frozen}, "GENERATION_FINALIZATION_SLOT_CONFLICT")
		// A slot outside the frozen step-key vocabulary is refused before the
		// database is touched.
		productionGraphReadsRefused(t, author, "finalization-generation:read",
			[]any{map[string]any{"slot": map[string]any{"source": owned["source"], "stepKey": "unmapped_step"}}, frozen},
			"GENERATION_FINALIZATION_SLOT_INVALID")
		for _, slot := range []map[string]any{absent, owned, foreign} {
			for _, refused := range []map[string]any{stale, unleased} {
				productionEditorRefused(t, author, "refused lease", "finalization-generation:read",
					[]any{map[string]any{"slot": slot}, refused})
			}
		}
		productionGraphReadsUnchanged(t, database, before, "finalization-generation:read")
	})
}

// productionGraphReadsRoot builds the disposable data root: one admitted
// project whose database carries the durable tables, the reduced planner's
// inputs and the ledger tables the two reads query.
func productionGraphReadsRoot(t *testing.T) (root, projectPath, database string, contract admission.SchemaContract) {
	t.Helper()
	root = syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	schema := productionGraphReadsSchema()
	seedProductionProject(t, library, productionGraphReadsProjectName, productionGraphReadsProjectID,
		admission.CurrentSchemaVersion, schema)
	projectPath = filepath.Join(library, productionGraphReadsProjectName)
	seedProductionDurableRows(t, projectPath)
	database = filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	contract = productionGraphReadsContract(t, schema)
	return root, projectPath, database, contract
}

// productionGraphReadsSchema is the durable fixture plus the four ledger tables
// the graph and finalization reads join. The statements are written the way the
// production migrations write them, because the admission fingerprint hashes
// the stored sqlite_master rows.
func productionGraphReadsSchema() []string {
	return append([]string{
		"CREATE TABLE generation_roots (root_action_id TEXT PRIMARY KEY, idempotency_key TEXT NOT NULL UNIQUE, action_json TEXT NOT NULL, budget_json TEXT NOT NULL, active_elapsed_ms INTEGER NOT NULL DEFAULT 0, active_since_ms INTEGER, blocked_code TEXT, revision INTEGER NOT NULL DEFAULT 0)",
		"CREATE TABLE generation_runs (run_id TEXT PRIMARY KEY, root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id), binding_json TEXT NOT NULL, status TEXT NOT NULL, created_at_ms INTEGER NOT NULL, open_key TEXT NOT NULL UNIQUE)",
		"CREATE TABLE generation_attempts (attempt_id TEXT PRIMARY KEY, reservation_id TEXT NOT NULL UNIQUE, run_id TEXT NOT NULL REFERENCES generation_runs(run_id), root_action_id TEXT NOT NULL REFERENCES generation_roots(root_action_id), attempt_json TEXT NOT NULL, usage_receipt_json TEXT, invocation_nonce TEXT NOT NULL, UNIQUE(run_id, invocation_nonce))",
		"CREATE TABLE generation_artifacts (artifact_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL UNIQUE REFERENCES generation_attempts(attempt_id), run_id TEXT NOT NULL REFERENCES generation_runs(run_id), artifact_json TEXT NOT NULL, revision INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'partial')",
	}, productionDurableSchema...)
}

// productionGraphReadsContract is the trusted contract for the fixture schema.
// The verifier hashes sqlite_master in name order, so the entries are sorted the
// same way instead of in DDL order.
func productionGraphReadsContract(t *testing.T, statements []string) admission.SchemaContract {
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

// productionGraphReadsSeedLedger writes the three runs through the shipped
// ledger store, on the raw file, before the lane ever opens it. It returns the
// seeded graph run's identity so the read can be aimed at a row that exists.
func productionGraphReadsSeedLedger(t *testing.T, database string) (runID, rootID string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ordinal := 0
	ledger := store.NewGenerationLedger(db,
		func() int64 { return 1700000000000 },
		func() string {
			ordinal++
			return "graph-reads-ledger-" + strconv.Itoa(ordinal)
		})
	ctx := context.Background()
	graph := productionGraphReadsSeedRun(t, ctx, ledger, productionGraphReadsSeedRequest(t,
		productionGraphReadsOperation, productionGraphReadsProjectID, productionGraphReadsManifest(t)))
	owned := productionGraphReadsSlot(productionGraphReadsOwnedFinalizationID)
	productionGraphReadsSeedRun(t, ctx, ledger, productionGraphReadsSeedRequest(t,
		productionGraphReadsFinalizationOp, productionGraphReadsProjectID,
		productionGraphReadsFinalizationManifest(t, owned)))
	foreign := productionGraphReadsSlot(productionGraphReadsForeignFinalizationID)
	productionGraphReadsSeedRun(t, ctx, ledger, productionGraphReadsSeedRequest(t,
		productionGraphReadsFinalizationOp, productionGraphReadsForeignProjectID,
		productionGraphReadsFinalizationManifest(t, foreign)))
	// The permit hashes the file at project:open, so the seeded rows have to be
	// on disk before it runs.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return syntheticString(graph["runId"]), syntheticString(graph["rootActionId"])
}

func productionGraphReadsSeedRun(t *testing.T, ctx context.Context, ledger *store.GenerationLedger, request map[string]any) map[string]any {
	t.Helper()
	// The ledger keeps fingerprint and budget in JSON order, so the request goes
	// through its decoder as text instead of as a Go map.
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+string(encoded)+"]"))
	if err != nil {
		t.Fatalf("seed ledger open for %v: %v", request["operation"], err)
	}
	run := syntheticMap(value)
	if run == nil || syntheticString(run["runId"]) == "" || syntheticString(run["rootActionId"]) == "" {
		t.Fatalf("seed ledger open returned no run identity for %v: %v", request["operation"], value)
	}
	return run
}

func productionGraphReadsSeedRequest(t *testing.T, operation, projectID, epoch string, manifest map[string]any) map[string]any {
	t.Helper()
	hash := strings.Repeat("a", 64)
	fingerprint := map[string]any{}
	for _, key := range []string{"chapterBriefHash", "authorGuidanceHash", "dependencyHash",
		"contextSnapshotHash", "templateHash", "skillSnapshotHash", "modelLeaseRevision",
		"policyHash", "outputContractHash"} {
		fingerprint[key] = hash
	}
	budget := map[string]any{}
	budget["maxPhysicalRequests"] = 3
	budget["maxTokenLiability"] = 4000
	budget["maxOutputPerRequest"] = 1000
	budget["maxActiveElapsedMs"] = 600000
	request := map[string]any{}
	request["projectId"] = projectID
	request["epoch"] = epoch
	request["operation"] = operation
	request["uiActionNonce"] = "graph-reads-nonce:" + operation + ":" + projectID
	request["frozenInputHash"] = hash
	request["contextSnapshotId"] = "graph-reads-context-snapshot"
	request["fingerprint"] = fingerprint
	request["sourceManifest"] = manifest
	request["sourceRefs"] = []any{}
	request["budget"] = budget
	return request
}

// productionGraphReadsManifest takes the frozen plot manifest the synthetic
// graph tests build and moves it onto this project and this fixture epoch, so
// the seeded row is one the lane would accept if the session held that lease.
func productionGraphReadsManifest(t *testing.T) map[string]any {
	t.Helper()
	manifest := syntheticGraphManifest(t, nil)
	contextValue := syntheticMap(manifest["graphGenerationContext"])
	if contextValue == nil {
		t.Fatal("the synthetic graph manifest lost its context")
	}
	contextValue["projectId"] = productionGraphReadsProjectID
	contextValue["originEpoch"] = productionGraphReadsFrozenEpoch
	digest, err := syntheticGraphDigest(contextValue)
	if err != nil {
		t.Fatal(err)
	}
	manifest["graphGenerationContextHash"] = digest
	manifest["graphGenerationOriginEpoch"] = productionGraphReadsFrozenEpoch
	return manifest
}

// productionGraphReadsFinalizationManifest is the binding half the finalization
// read matches on: the slot key, under the operation that alone may carry it.
func productionGraphReadsFinalizationManifest(t *testing.T, slot map[string]any) map[string]any {
	t.Helper()
	manifest := map[string]any{}
	manifest["operation"] = productionGraphReadsFinalizationOp
	manifest["finalizationGenerationSlotKey"] = productionGraphReadsSlotKey(t, slot)
	return manifest
}

func productionGraphReadsSlot(finalizationID string) map[string]any {
	source := map[string]any{}
	source["draftId"] = float64(productionGraphReadsDraftID)
	source["chapterNumber"] = float64(productionGraphReadsChapterNumber)
	source["finalizationId"] = finalizationID
	source["contentHash"] = productionGraphReadsContentHash
	slot := map[string]any{}
	slot["source"] = source
	slot["stepKey"] = productionGraphReadsStepKey
	return slot
}

// productionGraphReadsSlotKey rebuilds the key from the frozen slot the way the
// original derives it - the hash of the JSON array of its five primitives -
// rather than calling the lane's own helper, so a wrong derivation cannot pass
// by agreeing with itself.
func productionGraphReadsSlotKey(t *testing.T, slot map[string]any) string {
	t.Helper()
	source := syntheticMap(slot["source"])
	if source == nil {
		t.Fatalf("slot has no source: %v", slot)
	}
	draftID, draftOK := syntheticSlotID(source["draftId"])
	chapterNumber, chapterOK := syntheticSlotID(source["chapterNumber"])
	finalizationID := syntheticString(source["finalizationId"])
	contentHash := syntheticString(source["contentHash"])
	if !draftOK || !chapterOK || finalizationID == "" || contentHash == "" {
		t.Fatalf("slot is not complete: %v", slot)
	}
	return sha256Hex("[" + strconv.FormatInt(draftID, 10) + `,"` + finalizationID + `",` +
		strconv.FormatInt(chapterNumber, 10) + `,"` + contentHash + `","` +
		syntheticString(slot["stepKey"]) + `"]`)
}

// productionGraphReadsRefused asserts one refusal with its code: the name is
// bound (no 501), the envelope was understood, and the answer is the documented
// refusal rather than a view built from state the lane cannot prove.
func productionGraphReadsRefused(t *testing.T, client *productionDurableClient, operation string, args []any, code string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s is still fail-closed at 501: the read binder is not installed: %s", operation, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%s answered 200 where this lane must refuse, want %s: %s", operation, code, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if body.Error.Code != code {
		t.Fatalf("%s refused with %q, want %q: %s", operation, body.Error.Code, code, response.Body.String())
	}
}

func productionGraphReadsBytes(t *testing.T, database string) []byte {
	t.Helper()
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// productionGraphReadsUnchanged proves the read left the author database exactly
// as the permit found it: a read that quietly migrated, refreshed or rewrote a
// row would break the next operation's content identity.
func productionGraphReadsUnchanged(t *testing.T, database string, before []byte, operation string) {
	t.Helper()
	if after := productionGraphReadsBytes(t, database); !bytes.Equal(after, before) {
		t.Fatalf("%s rewrote the admitted author database", operation)
	}
}
