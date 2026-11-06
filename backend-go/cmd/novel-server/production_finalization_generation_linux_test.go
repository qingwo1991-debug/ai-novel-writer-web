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
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The stale lease below is a lease this session never held. It is not a secret
// and it is not derived from the live one: the point is that it is refused.
const productionFinalizationGenerationStaleLease = "finalization-generation-stale-lease"

// TestProductionLaneRefusesFinalizationGeneration pins the finalization
// generation family's three remaining names where this lane leaves them: unbound,
// answering the fail-closed 501 GO_OPERATION_NOT_MIGRATED. One subtest per
// operation, so a failure names the channel that moved.
//
// The fixture is a disposable data root whose admitted project carries real
// ledger rows written before the admission permit hashed the file: a run that
// claims a finalization slot in this project and one that claims a slot in
// another project. Every handle this test drives is read back out of that file
// over its own mode=ro connection, so nothing is asserted against a constant.
//
// The three names are not "not yet reached": each one is refused with a
// documented missing piece, and each subtest re-reads the code the seam table
// points at. A begin that ran would claim a second slot; an execute that ran
// would reserve an attempt; a commit that ran would write the finalized draft
// and move the outbox row. None of that happens, and the database stays
// byte-identical, which is what makes the 501 honest rather than a stub that
// happens to answer before the write.
func TestProductionLaneRefusesFinalizationGeneration(t *testing.T) {
	root, projectPath, database, contract := productionGraphReadsRoot(t)
	productionGraphReadsSeedLedger(t, database)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "finalization-generation-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	projectID := syntheticString(opened.Project["id"])
	leaseID := syntheticString(opened.Project["sessionLease"])
	if projectID != productionGraphReadsProjectID || leaseID == "" {
		t.Fatalf("project:open returned %q/%q, want the seeded project with a live lease", projectID, leaseID)
	}
	frozen := map[string]any{"projectId": projectID, "projectPath": projectPath, "leaseId": leaseID}
	stale := map[string]any{"projectId": projectID, "projectPath": projectPath,
		"leaseId": productionFinalizationGenerationStaleLease}
	unleased := map[string]any{"projectId": projectID, "projectPath": projectPath}
	slot := productionGraphReadsSlot(productionGraphReadsOwnedFinalizationID)
	rootActionID, runID := productionFinalizationGenerationRun(t, database, productionGraphReadsSlotKey(t, slot))
	handle := map[string]any{"projectId": projectID, "epoch": leaseID,
		"rootActionId": rootActionID, "runId": runID}

	t.Run("finalization-generation:begin", func(t *testing.T) {
		productionFinalizationGenerationSeamProof(t, productionFinalizationBegin)
		before := productionGraphReadsBytes(t, database)
		// The reference request, the same one with the optional parent, a stale
		// lease, no lease at all, and a request shape with nothing in it: every one
		// of them gets the same answer, because the answer is decided before the
		// envelope, the lease or the slot is looked at.
		for _, args := range [][]any{
			{map[string]any{"slot": slot, "modelId": productionTestModelID}, frozen},
			{map[string]any{"slot": slot, "modelId": productionTestModelID, "parentRootActionId": rootActionID}, frozen},
			{map[string]any{"slot": slot, "modelId": productionTestModelID}, stale},
			{map[string]any{"slot": slot, "modelId": productionTestModelID}, unleased},
			{frozen},
		} {
			productionFinalizationGenerationRefused(t, author, productionFinalizationBegin, args)
		}
		// No run claims the slot beyond the one the fixture wrote, so no begin
		// reached the ledger and no second slot was claimed.
		currentRoot, currentRun := productionFinalizationGenerationRun(t, database, productionGraphReadsSlotKey(t, slot))
		if currentRoot != rootActionID || currentRun != runID {
			t.Fatalf("the refused begin changed the slot owner: %q/%q, want %q/%q",
				currentRoot, currentRun, rootActionID, runID)
		}
		productionGraphReadsUnchanged(t, database, before, productionFinalizationBegin)
	})

	t.Run("finalization-generation:execute", func(t *testing.T) {
		productionFinalizationGenerationSeamProof(t, productionFinalizationExecute)
		before := productionGraphReadsBytes(t, database)
		artifact := map[string]any{"artifactId": "finalization-generation-artifact",
			"revision": 1, "textHash": strings.Repeat("a", 64)}
		for _, args := range [][]any{
			{map[string]any{"handle": handle}, frozen},
			{map[string]any{"handle": handle, "retryOf": artifact}, frozen},
			{map[string]any{"handle": handle, "recoverFrom": "finalization-attempt-0001"}, frozen},
			{map[string]any{"handle": handle}, stale},
			{frozen},
		} {
			productionFinalizationGenerationRefused(t, author, productionFinalizationExecute, args)
		}
		// Nothing was reserved, dispatched or settled: the run still has no attempt
		// row, and there is no loopback listener in this test to receive one.
		if attempts := productionFinalizationGenerationCount(t, database,
			"SELECT COUNT(*) FROM generation_attempts WHERE run_id=?", runID); attempts != 0 {
			t.Fatalf("the refused execute left %d attempt rows on the run", attempts)
		}
		productionGraphReadsUnchanged(t, database, before, productionFinalizationExecute)
	})

	t.Run("finalization-generation:commit", func(t *testing.T) {
		productionFinalizationGenerationSeamProof(t, productionFinalizationCommit)
		before := productionGraphReadsBytes(t, database)
		artifact := map[string]any{"artifactId": "finalization-generation-artifact",
			"revision": 1, "textHash": strings.Repeat("a", 64)}
		for _, args := range [][]any{
			{map[string]any{"handle": handle, "artifact": artifact}, frozen},
			{map[string]any{"handle": handle, "artifact": artifact}, stale},
			{frozen},
		} {
			productionFinalizationGenerationRefused(t, author, productionFinalizationCommit, args)
		}
		// The finalized draft and the outbox row are exactly what the fixture
		// seeded: a commit that had run would have rewritten the draft body and
		// moved a finalization_outbox row for it.
		if body := productionDurableBody(t, projectPath); body != productionDurableSeed {
			t.Fatalf("the refused commit rewrote the finalized draft: %q", body)
		}
		if rows := productionFinalizationGenerationCount(t, database, "SELECT COUNT(*) FROM finalization_outbox"); rows != 0 {
			t.Fatalf("the refused commit wrote %d finalization_outbox rows", rows)
		}
		productionGraphReadsUnchanged(t, database, before, productionFinalizationCommit)
	})

	t.Run("served siblings", func(t *testing.T) {
		// finalization-generation:read is bound elsewhere and is still served: the
		// slot nobody owns is JSON null, the slot this project owns is refused as an
		// unprovable context, and a stale lease is a lease refusal.
		absent := productionGraphReadsSlot(productionGraphReadsAbsentFinalizationID)
		if result := productionEditorCall(t, author, "finalization-generation:read",
			[]any{map[string]any{"slot": absent}, frozen}); string(result) != "null" {
			t.Fatalf("finalization-generation:read projected %s for an unowned slot, want null", result)
		}
		productionGraphReadsRefused(t, author, "finalization-generation:read",
			[]any{map[string]any{"slot": slot}, frozen}, "GENERATION_FINALIZATION_CONTEXT_INVALID")
		productionEditorRefused(t, author, "stale lease", "finalization-generation:read",
			[]any{map[string]any{"slot": slot}, stale})
		// finalization-generation:cancel is bound too: it refuses a run that does
		// not exist instead of answering the 501 of an absent name, and it writes
		// nothing while doing so.
		missing := map[string]any{"projectId": projectID, "epoch": leaseID,
			"rootActionId": "finalization-generation-missing-root", "runId": "finalization-generation-missing-run"}
		before := productionGraphReadsBytes(t, database)
		response := author.call(t, "finalization-generation:cancel", []any{map[string]any{"handle": missing}, frozen})
		if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("finalization-generation:cancel is unbound: %d %s", response.Code, response.Body.String())
		}
		productionGraphReadsUnchanged(t, database, before, "finalization-generation:cancel")
	})

	t.Run("binder", func(t *testing.T) {
		// The binder this lane would wire installs nothing over a real server, so
		// inserting it cannot collide with read or cancel.
		server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := bindProductionFinalizationGeneration(server); err != nil {
			t.Fatalf("the refusing binder failed: %v", err)
		}
		if names := server.BoundOperations(); len(names) != 0 {
			t.Fatalf("the binder installed %v, want nothing", names)
		}
		if err := bindProductionFinalizationGeneration(nil); err == nil {
			t.Fatal("a nil server was accepted")
		}
		for _, operation := range productionFinalizationGenerationOperations() {
			// Each name is inventoried, so an absent handler is exactly 501
			// GO_OPERATION_NOT_MIGRATED and never 404 OPERATION_NOT_EXPOSED.
			fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
			if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
				return nil, nil
			}); err != nil {
				t.Fatalf("%s is not inventoried, so its unbound answer would not be 501: %v", operation, err)
			}
		}
		// read and cancel are served by other files and must never be listed here.
		for _, served := range []string{"finalization-generation:read", "finalization-generation:cancel"} {
			if _, found := productionFinalizationGenerationSeam(served); found {
				t.Fatalf("%s is served elsewhere and must not be documented as refused", served)
			}
		}
	})
}

// productionFinalizationGenerationRefused asserts the fail-closed answer: the
// name is not installed, so the transport answers 501 before the envelope, the
// lease or the request shape is examined.
func productionFinalizationGenerationRefused(t *testing.T, client *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("%s answered %d %s, want 501 GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s answered %d %s without GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
	}
}

// productionFinalizationGenerationSeamProof re-reads every line the seam table
// names, so the documentation cannot rot: a moved declaration or a renamed
// refusal fails here instead of leaving a note that points at nothing.
func productionFinalizationGenerationSeamProof(t *testing.T, operation string) {
	t.Helper()
	seam, found := productionFinalizationGenerationSeam(operation)
	if !found {
		t.Fatalf("%s has no documented seam", operation)
	}
	if len(seam.Missing) == 0 {
		t.Fatalf("%s documents no missing piece", operation)
	}
	for _, line := range seam.Missing {
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%s: %s: %v", operation, line.Path, err)
		}
		// The marker is searched for across the whole file rather than only at the
		// recorded line: a seam that is still documented but has been shifted by an
		// unrelated edit is not a regression, while a seam that was deleted is. The
		// recorded line is still reported so a reader can find it quickly.
		if !strings.Contains(string(raw), line.Marker) {
			t.Fatalf("%s: no longer documents %q anywhere in %s (it was recorded at line %d)",
				operation, line.Marker, line.Path, line.Line)
		}
	}
}

// productionFinalizationGenerationRun reads the one run that claims a slot back
// out of the author database over its own read-only connection, so a handle the
// test drives is a row the file really holds. It also fails when a second run
// claims the same slot, which is how a refused begin proves it claimed nothing.
func productionFinalizationGenerationRun(t *testing.T, database, slotKey string) (rootActionID, runID string) {
	t.Helper()
	db := productionFinalizationGenerationReadOnly(t, database)
	defer func() { _ = db.Close() }()
	rows, err := db.Query("SELECT run_id,root_action_id,binding_json FROM generation_runs ORDER BY rowid")
	if err != nil {
		t.Fatalf("read the seeded runs back: %v", err)
	}
	defer func() { _ = rows.Close() }()
	claims := 0
	for rows.Next() {
		var candidateRun, candidateRoot, binding string
		if err := rows.Scan(&candidateRun, &candidateRoot, &binding); err != nil {
			t.Fatalf("read the seeded runs back: %v", err)
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(binding), &decoded); err != nil {
			t.Fatalf("decode a seeded binding: %v", err)
		}
		if syntheticString(syntheticMap(decoded["sourceManifest"])["finalizationGenerationSlotKey"]) != slotKey {
			continue
		}
		claims++
		rootActionID, runID = candidateRoot, candidateRun
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the seeded runs back: %v", err)
	}
	if claims != 1 {
		t.Fatalf("%d runs claim the slot, want exactly the one the fixture wrote", claims)
	}
	return rootActionID, runID
}

// productionFinalizationGenerationCount reads one integer back over a read-only
// connection: what a write would have added is only absent when the file says so.
func productionFinalizationGenerationCount(t *testing.T, database, query string, args ...any) int {
	t.Helper()
	db := productionFinalizationGenerationReadOnly(t, database)
	defer func() { _ = db.Close() }()
	var found int
	if err := db.QueryRow(query, args...).Scan(&found); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return found
}

// productionFinalizationGenerationReadOnly opens the admitted author database
// without write authority, so nothing this test reads can repair what it is
// checking.
func productionFinalizationGenerationReadOnly(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database read-only: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}
