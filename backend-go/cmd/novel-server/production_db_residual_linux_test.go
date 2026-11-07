//go:build linux

package main

import (
	"context"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// productionDBResidualServed is the db slice this lane actually installs, as
// measured by reading every binder production_open_linux.go calls and every
// table those binders iterate: 100 of the 103 inventoried `db:*` names, leaving
// the three in production_db_residual_linux.go. Both halves are pinned below
// against the finished lane rather than against hand counting, so a batch that
// binds one of the three has to move this number and delete the row.
const productionDBResidualServed = 100

// The execution lease every request below names. The blueprint sync family used
// to share this block with its own operation id; that name is served now, so its
// request shape moved with the row into production_db_sync_complete_linux_test.go.
const (
	productionDBResidualExecution = "db-residual-execution-0"
)

// TestProductionLaneDBResidualIsTheUnservedSlice pins the measurement this batch
// reports. It takes the served set from the lane that was just built
// (transport.Server.BoundOperations), takes the `db:` names from the registry the
// same lane loaded, and requires the two sides to meet: every inventoried db name
// is either bound or in the residual table, no name is in both, and the served
// half is exactly the number reported above.
func TestProductionLaneDBResidualIsTheUnservedSlice(t *testing.T) {
	root, _, _, contract := productionGraphReadsRoot(t)
	registry := migrationRegistry(t)
	handler, cleanup, err := productionHandlerWithContract(registry, productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	boundOperations := productionLaneBoundOperations(t, handler)
	bound := make(map[string]bool, len(boundOperations))
	for _, operation := range boundOperations {
		bound[operation] = true
	}
	residual := make(map[string]bool, len(productionDBResidualOperations()))
	for _, operation := range productionDBResidualOperations() {
		if _, inventoried := registry.Operations[operation]; !inventoried {
			t.Fatalf("%v is refused by this file but is not in the inventory", operation)
		}
		if residual[operation] {
			t.Fatalf("%v is listed twice in the residual table", operation)
		}
		residual[operation] = true
		if bound[operation] {
			t.Fatalf("%v is bound by the lane and must be moved out of the residual table", operation)
		}
	}
	inventory := make([]string, 0, len(registry.Operations))
	for operation := range registry.Operations {
		if productionDBResidualIsDB(operation) {
			inventory = append(inventory, operation)
		}
	}
	sort.Strings(inventory)
	served := make([]string, 0, len(inventory))
	undescribed := make([]string, 0, len(inventory))
	for _, operation := range inventory {
		switch {
		case bound[operation]:
			served = append(served, operation)
		case residual[operation]:
		default:
			undescribed = append(undescribed, operation)
		}
	}
	if len(undescribed) != 0 {
		t.Fatalf("%d inventoried db names are neither served nor documented as a seam: %v",
			len(undescribed), strings.Join(undescribed, ","))
	}
	if len(served) != productionDBResidualServed {
		t.Fatalf("%d of %d inventoried db names are served (measured %d); served=%v",
			len(served), len(inventory), productionDBResidualServed, strings.Join(served, ","))
	}
	if len(served)+len(residual) != len(inventory) {
		t.Fatalf("%d served + %d refused does not cover the %d inventoried db names",
			len(served), len(residual), len(inventory))
	}
	t.Logf("db slice: %d served, %d refused, %d inventoried", len(served), len(residual), len(inventory))
}

// TestProductionLaneRefusesDBResidual pins the three names where this lane
// leaves them: unbound, answering the fail-closed 501
// GO_OPERATION_NOT_MIGRATED, under the live lease, a stale lease and no lease at
// all. One subtest per name, so a failure names the channel that moved.
//
// Every request carries the shape the renderer actually sends - the arity and the
// path position this table records, read off src/shared/ipc-channels.ts and
// cross-checked against the call site named in each seam row - because a refusal
// that only holds for a malformed request is not a refusal: the 501 is decided
// before the envelope, the lease or the payload is looked at, and it has to stay
// that way when the handler is finally installed.
//
// The database is byte-compared over the whole file after every request of the
// family: none of these names may mutate the author's data, and the ones that
// would (the receipt writes) are exactly the ones this batch refused.
func TestProductionLaneRefusesDBResidual(t *testing.T) {
	root, projectPath, database, contract := productionGraphReadsRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "db-residual-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %v %v", opened.Success, opened.DBReady)
	}
	projectID := syntheticString(opened.Project["id"])
	leaseID := syntheticString(opened.Project["sessionLease"])
	if projectID != productionGraphReadsProjectID || leaseID == "" {
		t.Fatalf("project:open returned project %v with lease %v, want the seeded project with a live lease", projectID, leaseID)
	}

	frozen := map[string]any{"projectId": projectID, "projectPath": projectPath, "leaseId": leaseID}
	stale := map[string]any{"projectId": projectID, "projectPath": projectPath,
		"leaseId": "db-residual-stale-lease"}
	unleased := map[string]any{"projectId": projectID, "projectPath": projectPath}

	// The identity is observed after the lane opened the file, because opening it
	// may admit or migrate before the first operation runs; from here on nothing
	// below is allowed to change a byte.
	before := productionGraphReadsBytes(t, database)
	for _, operation := range productionDBResidualOperations() {
		operation := operation
		t.Run(operation, func(t *testing.T) {
			seam := productionDBResidualSeamProof(t, operation)
			for _, envelope := range []struct {
				label string
				value map[string]any
			}{{"live lease", frozen}, {"stale lease", stale}, {"no lease", unleased}} {
				productionDBResidualRefused(t, author, envelope.label, operation,
					productionDBResidualRequest(seam, projectPath, envelope.value))
			}
		})
	}
	// Nothing was repaired, receipted or cleared: the file still holds the same
	// bytes the permit found.
	productionGraphReadsUnchanged(t, database, before, "db:residual")

	t.Run("binder", func(t *testing.T) {
		// The binder this lane wires installs nothing, so adding it to the lane
		// cannot collide with a served name and cannot move the served count. It
		// still has to refuse a nil server and a seam it cannot describe.
		server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := bindProductionDBResidual(server); err != nil {
			t.Fatalf("the refusing binder failed: %v", err)
		}
		if names := server.BoundOperations(); len(names) != 0 {
			t.Fatalf("the binder installed %v, want nothing", names)
		}
		if err := bindProductionDBResidual(nil); err == nil {
			t.Fatal("a nil server was accepted")
		}
		// Each name is inventoried, so an absent handler is exactly 501
		// GO_OPERATION_NOT_MIGRATED and never 404 OPERATION_NOT_EXPOSED.
		for _, operation := range productionDBResidualOperations() {
			fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
			if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
				return nil, nil
			}); err != nil {
				t.Fatalf("%v is not inventoried, so its unbound answer would not be 501: %v", operation, err)
			}
		}
	})
}

// productionDBResidualSeamProof re-reads the declaration every seam row points
// at, and returns the row so the request can be built from the same shape the
// row records. The marker is searched for across the whole file rather than only
// at the recorded line: a seam that is still documented but has been shifted by
// an unrelated edit is not a regression, while a seam that was deleted is.
func productionDBResidualSeamProof(t *testing.T, operation string) productionDBResidualSeam {
	t.Helper()
	seam, found := productionDBResidualSeamFor(operation)
	if !found {
		t.Fatalf("%v has no documented seam", operation)
	}
	if seam.Port == "" || len(seam.Missing) == 0 {
		t.Fatalf("%v documents no missing port or no declaration behind it", operation)
	}
	if seam.Gate != "" && !productionDBResidualSymbolic(seam.Gate) {
		t.Fatalf("%v records %v as its gate, which is not a symbolic code", operation, seam.Gate)
	}
	for _, line := range seam.Missing {
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%v: %v: %v", operation, line.Path, err)
		}
		if !productionSeamContains(string(raw), line.Marker) {
			t.Fatalf("%v no longer documents %v anywhere in %v (recorded at line %d): %v",
				operation, line.Marker, line.Path, line.Line,
				productionSeamDiagnosis(string(raw), line.Marker, line.Line))
		}
	}
	return seam
}

// productionDBResidualRefused asserts the one answer this family gives today.
// It names the label and the operation, never a pointer, and it does not accept
// a 4xx in place of the 501: the name is unbound, not malformed.
func productionDBResidualRefused(t *testing.T, client *productionDurableClient, label, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented ||
		!strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%v under %v = %d %s, want 501 GO_OPERATION_NOT_MIGRATED",
			operation, label, response.Code, response.Body.String())
	}
}

// productionDBResidualRequest builds the argument list the renderer sends for one
// seam: the recorded arity, the frozen project path at the recorded index, and the
// frozen three-key session as its own last argument. Payload values are the
// reference's own placeholder shapes; none of them is read while the name is
// unbound, and each is the value the call site in src/services actually passes.
func productionDBResidualRequest(seam productionDBResidualSeam, projectPath string, expected map[string]any) []any {
	args := make([]any, seam.Arity)
	args[seam.PathIndex] = projectPath
	args[seam.Arity-1] = expected
	execution := map[string]any{"owner": productionDBResidualExecution, "epoch": 1}
	switch seam.Operation {
	case productionDBResidualGlobalFactsCommit:
		args[0] = map[string]any{"facts": []any{}, "mode": "replace"}
	case productionDBResidualReceiptPrepare:
		args[0] = map[string]any{"runId": "import-run-0", "stage": "global", "batchId": "batch-0",
			"effectKey": "effect-0", "kind": "global-facts", "payload": map[string]any{}}
		args[1] = execution
	case productionDBResidualReceiptCommit:
		args[0] = "import-run-0"
		args[1] = "global"
		args[2] = "batch-0"
		args[3] = execution
	case productionDBResidualPrepareInspection:
		args[0] = map[string]any{"inspectionId": "inspection-0", "purpose": "reference"}
	case productionDBResidualProjectClearGenerated:
		args[0] = map[string]any{"scopes": []any{}}
	}
	return args
}
