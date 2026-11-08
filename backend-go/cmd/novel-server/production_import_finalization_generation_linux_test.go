//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The two leases below are ones this session never holds. They are not secrets
// and not derived from the live lease: the point is that they are refused.
const (
	productionImportGenerationStaleLease = "import-generation-stale-lease"
	productionImportGenerationSlotRunID  = "import-generation-read-run"
)

// TestProductionLaneRefusesImportGeneration pins the four model-executing
// generation names this change answers where this lane leaves them: unbound,
// answering the fail-closed 501 GO_OPERATION_NOT_MIGRATED. One subtest per
// operation, so a failure names the channel that moved.
//
// The fixture is a disposable data root whose admitted project carries real
// ledger rows written before the admission permit hashed the file, and every
// handle the test drives is read back out of that file over its own mode=ro
// connection. That matters for the two import names as much as for the
// finalization ones: the execute arm below hands the lane a handle to a run that
// really exists in this project, and the answer is still the 501, because the
// answer is decided before the envelope, the lease or the run is looked at.
//
// The pair of subtests after the pins is the honest half of a refusal: the
// lane's own ledger is asked to open an import run, on a copy of the file, and
// refuses every import operation by name - so the run these four names are built
// around cannot be produced here, which is the seam the table records.
func TestProductionLaneRefusesImportGeneration(t *testing.T) {
	root, projectPath, database, contract := productionGraphReadsRoot(t)
	seedRunID, seedRootID := productionGraphReadsSeedLedger(t, database)
	// The ledger probe runs against a copy taken here, before the lane opens the
	// file: the seeding helper closed its own connection, so this copy is the
	// admitted database in one consistent piece, and an experiment on it cannot
	// touch the bytes the digest arms are about.
	probe := productionImportGenerationProbeCopy(t, database)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "import-generation-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	projectID := syntheticString(opened.Project["id"])
	leaseID := syntheticString(opened.Project["sessionLease"])
	if projectID != productionGraphReadsProjectID || leaseID == "" {
		t.Fatalf("project:open returned %s with lease %s, want the seeded project with a live lease", projectID, leaseID)
	}
	frozen := map[string]any{"projectId": projectID, "projectPath": projectPath, "leaseId": leaseID}
	stale := map[string]any{"projectId": projectID, "projectPath": projectPath,
		"leaseId": productionImportGenerationStaleLease}
	unleased := map[string]any{"projectId": projectID, "projectPath": projectPath}
	// A handle to a run the seeded file really holds: the execute arms below are
	// refused despite naming it, not because it is missing.
	importHandle := map[string]any{"projectId": projectID, "epoch": leaseID,
		"rootActionId": seedRootID, "runId": seedRunID}
	finalizationSlot := productionGraphReadsSlot(productionGraphReadsOwnedFinalizationID)
	finalizationRoot, finalizationRun := productionFinalizationGenerationRun(t, database,
		productionGraphReadsSlotKey(t, finalizationSlot))
	finalizationHandle := map[string]any{"projectId": projectID, "epoch": leaseID,
		"rootActionId": finalizationRoot, "runId": finalizationRun}
	artifact := map[string]any{"artifactId": "import-generation-artifact",
		"revision": 1, "textHash": strings.Repeat("a", 64)}

	t.Run("import-generation:read", func(t *testing.T) {
		productionImportGenerationSeamProof(t, productionImportGenerationRead)
		before := productionGraphReadsBytes(t, database)
		// A slot no run owns, the same slot with an extra key, a stale lease, no
		// lease at all and an empty request: the reference request shape and every
		// way of failing to present it get the one answer, because the answer is
		// decided before the slot is looked at.
		slot := map[string]any{"runId": productionImportGenerationSlotRunID, "stage": "global", "batchId": "done"}
		broken := map[string]any{"runId": productionImportGenerationSlotRunID, "stage": "global",
			"batchId": "done", "modelId": productionTestModelID}
		for _, args := range [][]any{
			{map[string]any{"slot": slot}, frozen},
			{map[string]any{"slot": broken}, frozen},
			{map[string]any{"slot": slot}, stale},
			{map[string]any{"slot": slot}, unleased},
			{frozen},
		} {
			productionImportGenerationRefused(t, author, productionImportGenerationRead, args)
		}
		// The read is a look-up over rows this lane never wrote: nothing it could
		// project exists, which is the seam the table records rather than a
		// handler that answers a constant.
		if rows := productionFinalizationGenerationCount(t, database,
			"SELECT COUNT(*) FROM generation_runs WHERE json_extract(binding_json,'$.sourceManifest.importSlotKey') IS NOT NULL"); rows != 0 {
			t.Fatalf("the fixture wrote %d import-bound runs, want none", rows)
		}
		productionGraphReadsUnchanged(t, database, before, productionImportGenerationRead)
	})

	t.Run("import-generation:execute", func(t *testing.T) {
		productionImportGenerationSeamProof(t, productionImportGenerationExecute)
		before := productionGraphReadsBytes(t, database)
		task := map[string]any{"purpose": "import-global-facts", "reasoningStage": "review",
			"output": "structured-data",
			"messages": []any{map[string]any{"role": "user", "content": productionDurableSeed}}}
		execution := map[string]any{"runId": productionImportGenerationSlotRunID,
			"owner": "import-generation-owner", "epoch": leaseID}
		for _, args := range [][]any{
			{map[string]any{"handle": importHandle, "ordinal": float64(0), "task": task, "execution": execution}, frozen},
			{map[string]any{"handle": importHandle, "ordinal": float64(0), "task": task}, frozen},
			{map[string]any{"handle": importHandle, "ordinal": float64(1), "task": task}, frozen},
			{map[string]any{"handle": importHandle, "ordinal": float64(0), "task": task, "execution": execution}, stale},
			{frozen},
		} {
			productionImportGenerationRefused(t, author, productionImportGenerationExecute, args)
		}
		// No attempt was reserved and nothing was dispatched: the lane has no
		// provider seam in this test, so an attempt row would be the only trace a
		// dispatch could leave. The count is scoped to the nonce the reference
		// derives for an import attempt (import:<ordinal>), so the seeded rows of
		// other families cannot make this pass for the wrong reason.
		if attempts := productionFinalizationGenerationCount(t, database,
			"SELECT COUNT(*) FROM generation_attempts WHERE invocation_nonce LIKE 'import:%'"); attempts != 0 {
			t.Fatalf("the refused import execute reserved %d attempts", attempts)
		}
		productionGraphReadsUnchanged(t, database, before, productionImportGenerationExecute)
	})

	t.Run("the ledger refuses an import binding", func(t *testing.T) {
		// The four names above are built on a run whose frozen manifest carries
		// importSlotKey. This is that run's producer, asked for one on the copy of
		// the admitted file taken before the lane opened it: an ordinary operation
		// with the same request shape opens, and all three import stages are refused
		// with the one symbolic code.
		productionImportGenerationLedgerProbe(t, probe)
	})

	t.Run("binder", func(t *testing.T) {
		server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := bindProductionImportGeneration(server); err != nil {
			t.Fatalf("the refusing import binder failed: %v", err)
		}
		if names := server.BoundOperations(); len(names) != 0 {
			t.Fatalf("the import binder installed %v, want nothing", names)
		}
		if err := bindProductionImportGeneration(nil); err == nil {
			t.Fatal("a nil server was accepted by the import binder")
		}
		// The wired binder covers both families and still installs nothing, so
		// inserting it cannot collide with the names other files serve.
		wired := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := bindProductionFinalizationGeneration(wired); err != nil {
			t.Fatalf("the wired binder failed: %v", err)
		}
		if names := wired.BoundOperations(); len(names) != 0 {
			t.Fatalf("the wired binder installed %v, want nothing", names)
		}
		if err := bindProductionFinalizationGeneration(nil); err == nil {
			t.Fatal("a nil server was accepted by the wired binder")
		}
		for _, operation := range productionModelExecutingOperations() {
			// Each name is inventoried, so an absent handler is exactly 501
			// GO_OPERATION_NOT_MIGRATED and never 404 OPERATION_NOT_EXPOSED.
			fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
			if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
				return nil, nil
			}); err != nil {
				t.Fatalf("%s is not inventoried, so its unbound answer would not be 501: %v", operation, err)
			}
			gap, found := productionModelExecutingRefusal(operation)
			if !found || len(gap.Missing) == 0 {
				t.Fatalf("%s has no documented seam", operation)
			}
			if gap.Code != productionModelExecutingRefusalCode {
				t.Fatalf("%s documents %s, want %s", operation, gap.Code, productionModelExecutingRefusalCode)
			}
		}
	})

	t.Run("count", func(t *testing.T) {
		// Nothing was bound, so the number of served operations is unchanged. The
		// count is read from the finished lane instead of recounted by hand, and the
		// four names must be absent from it: binding one of them without moving
		// productionServedOperations fails this test and the count test together.
		served := productionLaneBoundOperations(t, handler)
		if len(served) != productionServedOperations {
			t.Fatalf("%d operations are served (productionServedOperations=%d); served=%s",
				len(served), productionServedOperations, strings.Join(served, ","))
		}
		for _, operation := range productionModelExecutingOperations() {
			for _, name := range served {
				if name == operation {
					t.Fatalf("%s is bound, so it is no longer a refusal; served=%s", operation, strings.Join(served, ","))
				}
			}
		}
	})
}

// productionImportGenerationRefused asserts the fail-closed answer: the name is
// not installed, so the transport answers 501 before the envelope, the lease or
// the request shape is examined.
func productionImportGenerationRefused(t *testing.T, client *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("%s answered %d %s, want 501 GO_OPERATION_NOT_MIGRATED",
			operation, response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), productionModelExecutingRefusalCode) {
		t.Fatalf("%s answered %d %s without %s",
			operation, response.Code, response.Body.String(), productionModelExecutingRefusalCode)
	}
}

// productionImportGenerationSeamProof re-reads every line the seam table names,
// so the documentation cannot rot: a moved declaration or a renamed refusal is
// reported here instead of leaving a note that points at nothing.
func productionImportGenerationSeamProof(t *testing.T, operation string) {
	t.Helper()
	gap, found := productionModelExecutingRefusal(operation)
	if !found {
		t.Fatalf("%s has no documented seam", operation)
	}
	if len(gap.Missing) == 0 {
		t.Fatalf("%s documents no missing piece", operation)
	}
	for _, line := range gap.Missing {
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%s: %s: %v", operation, line.Path, err)
		}
		// The marker is searched for across the whole file rather than only at the
		// recorded line: a seam that is still documented but has been shifted by an
		// unrelated edit is not a regression, while a seam that was deleted is. The
		// recorded line is still reported so a reader can find it quickly.
		if !productionSeamContains(string(raw), line.Marker) {
			t.Fatalf("%s: %s no longer documents %s (it was recorded at line %d)",
				operation, line.Path, line.Marker, line.Line)
		}
	}
}

// productionImportGenerationProbeCopy copies the admitted database into the
// test's own temporary tree. The copy is taken while nothing holds the file
// open, so it is one consistent page set; the probe writes only to the copy and
// the digest arms keep measuring the admitted file.
func productionImportGenerationProbeCopy(t *testing.T, database string) string {
	t.Helper()
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	copied := filepath.Join(t.TempDir(), "import-generation-ledger-probe.sqlite3")
	if err := os.WriteFile(copied, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return copied
}

// productionImportGenerationLedgerProbe asks the lane's own ledger to open one
// run per import stage on the copy, and refuses to pass unless it refuses all
// three. The control open at the top proves the request shape is one the ledger
// otherwise accepts, so the three refusals are the import binding itself and not
// a malformed request.
func productionImportGenerationLedgerProbe(t *testing.T, copied string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(copied)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	ordinal := 0
	ledger := store.NewGenerationLedger(db,
		func() int64 { return 1700000000000 },
		func() string {
			ordinal++
			return "import-generation-probe-" + strconv.Itoa(ordinal)
		})
	ctx := context.Background()
	control := map[string]any{"operation": "review-chapter"}
	if err := productionImportGenerationLedgerOpen(t, ctx, ledger, productionGraphReadsSeedRequest(t,
		"review-chapter", productionGraphReadsProjectID, productionGraphReadsFrozenEpoch, control)); err != nil {
		t.Fatalf("the probe's own request shape was refused for an ordinary operation: %v", err)
	}
	// The import manifest is the one the reference freezes for an import run:
	// the slot, its key and the captured context, beside the stage operation.
	importManifest := func(operation string) map[string]any {
		manifest := map[string]any{}
		manifest["operation"] = operation
		manifest["importSlot"] = map[string]any{"runId": productionImportGenerationSlotRunID,
			"stage": "global", "batchId": "done"}
		manifest["importSlotKey"] = strings.Repeat("b", 64)
		manifest["importContext"] = map[string]any{"manifestFingerprint": strings.Repeat("c", 64)}
		return manifest
	}
	for _, operation := range []string{"import-global-facts", "analyze-writing-style", "import-blueprints"} {
		err := productionImportGenerationLedgerOpen(t, ctx, ledger, productionGraphReadsSeedRequest(t,
			operation, productionGraphReadsProjectID, productionGraphReadsFrozenEpoch, importManifest(operation)))
		if err == nil {
			t.Fatalf("the ledger opened an %s run, so an import run can exist in this lane", operation)
		}
		if !strings.Contains(err.Error(), "GENERATION_OPERATION_UNSUPPORTED") {
			t.Fatalf("the ledger refused %s with %v, want GENERATION_OPERATION_UNSUPPORTED", operation, err)
		}
	}
}

// productionImportGenerationLedgerOpen is the ledger's own open call with one
// request map, encoded the way the fixture encodes it so the store decodes the
// binding in JSON key order.
func productionImportGenerationLedgerOpen(t *testing.T, ctx context.Context, ledger *store.GenerationLedger, request map[string]any) error {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ledger.CallJSON(ctx, "open", json.RawMessage("["+string(encoded)+"]"))
	return err
}
