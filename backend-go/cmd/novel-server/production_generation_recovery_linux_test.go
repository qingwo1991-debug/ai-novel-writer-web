//go:build linux

package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The runs these tests need are the runs a recovery read has to tell apart. The
// first two are the directory ones: one chapter-blueprint-directory run whose
// committed range stops short of the range it requested (the author can continue
// it), and one whose committed range covers what it requested (there is nothing
// left to write). The last is the same row in another project's database.
const (
	productionRecoveryDirectoryOpenNonce = "generation-recovery-directory-open"
	productionRecoveryDirectoryEpoch     = "lease-0000000004-directory"

	productionRecoveryResumableRun       = "ci-recovery-directory-resumable-1"
	productionRecoveryResumableRoot      = "ci-recovery-directory-resumable-root-1"
	productionRecoveryResumableOperation = "ci-recovery-blueprint-commit-resumable"

	productionRecoverySettledRun       = "ci-recovery-directory-settled-1"
	productionRecoverySettledRoot      = "ci-recovery-directory-settled-root-1"
	productionRecoverySettledOperation = "ci-recovery-blueprint-commit-settled"

	productionRecoveryForeignRun       = "ci-recovery-directory-foreign-1"
	productionRecoveryForeignRoot      = "ci-recovery-directory-foreign-root-1"
	productionRecoveryForeignOperation = "ci-recovery-blueprint-commit-foreign"
	productionRecoveryForeignProject   = "6f1c8a4e-2d37-4b90-8e51-3a7f0c29d5b6"

	// The two runs the refusal test needs: one admitted and never dispatched, and
	// one dispatched and settled.
	productionRecoveryInterruptedNonce = "generation-recovery-interrupted"
	productionRecoveryCompletedNonce   = "generation-recovery-completed"
	productionRecoverySettledNonce     = "generation-recovery-settled"
)

// The views below are JSON views of the wire, written out here instead of reusing
// the store's structs: if the store's tags drifted from
// src/shared/generation-owner-contract.ts:317, asserting through the store's own
// types would hide it.
type productionRecoveryRangeView struct {
	StartChapter int64 `json:"startChapter"`
	EndChapter   int64 `json:"endChapter"`
}

type productionRecoveryHandleView struct {
	ProjectID    string `json:"projectId"`
	Epoch        string `json:"epoch"`
	RootActionID string `json:"rootActionId"`
	RunID        string `json:"runId"`
}

type productionRecoveryInputView struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type productionRecoveryProgressView struct {
	OperationID    string                        `json:"operationId"`
	PayloadHash    string                        `json:"payloadHash"`
	SourceHandle   productionRecoveryHandleView  `json:"sourceHandle"`
	RequestedRange productionRecoveryRangeView   `json:"requestedRange"`
	CommittedRange productionRecoveryRangeView   `json:"committedRange"`
	RemainingRange *productionRecoveryRangeView  `json:"remainingRange"`
	AuthorInputs   []productionRecoveryInputView `json:"authorInputs"`
}

type productionRecoveryDiagnosticView struct {
	RunID string `json:"runId"`
	Kind  string `json:"kind"`
	Code  string `json:"code"`
}

type productionRecoveryCatalogView struct {
	Progress    []productionRecoveryProgressView   `json:"progress"`
	Diagnostics []productionRecoveryDiagnosticView `json:"diagnostics"`
}

// productionRecoveryDirectorySeed is one chapter-blueprint-directory run written
// the way recordDirectoryProgress writes it
// (generation-run-repository.ts:620-636): a receipt hanging off the run's last
// attempt, the blueprint commit operation the receipt names, and the run row whose
// manifest froze the requested range. The writer is not bound in this lane (the
// ledger refuses to open the operation, internal/store/generation_ledger.go:694),
// so a catalog reader of that table can only be tested against rows written the
// way that writer would have written them - the same reduction
// production_batch_reads_linux_test.go:24-45 documents for its own fixture.
type productionRecoveryDirectorySeed struct {
	RunID       string
	RootID      string
	ProjectID   string
	OperationID string
	Requested   productionRecoveryRangeView
	Committed   productionRecoveryRangeView
}

// productionRecoveryLane is one started production lane over one admitted project,
// its only open session's frozen context, and the operation server the finished
// lane mounted, so the binder can be called against the real bound set.
type productionRecoveryLane struct {
	client      *productionDurableClient
	projectPath string
	frozen      map[string]any
	server      *transport.Server
}

// productionRecoveryOpen builds the fixture, seeds the rows the caller needs
// before the lane opens, starts the production lane and opens one author session
// over it.
func productionRecoveryOpen(t *testing.T, seed func(t *testing.T, database string)) *productionRecoveryLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	if seed != nil {
		seed(t, productionRecoveryDatabasePath(projectPath))
	}
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	author, frozen := productionExecuteSession(t, handler, projectPath, productionRecoveryDirectoryOpenNonce)
	return &productionRecoveryLane{
		client:      author,
		projectPath: projectPath,
		frozen:      frozen,
		server:      productionRecoveryServer(t, handler),
	}
}

// productionRecoveryDatabasePath is the admitted author database of one project.
func productionRecoveryDatabasePath(projectPath string) string {
	return filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
}

// productionRecoveryServer returns the operation server the finished route table
// mounts at /api/. The count test reaches the same value through its own helper;
// this copy keeps the ownership of the two files separate.
func productionRecoveryServer(t *testing.T, handler http.Handler) *transport.Server {
	t.Helper()
	mux, ok := handler.(*http.ServeMux)
	if !ok {
		t.Fatalf("the production lane returned %T, not the route table it builds", handler)
	}
	mounted, pattern := mux.Handler(&http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/api/"}})
	server, ok := mounted.(*transport.Server)
	if !ok {
		t.Fatalf("the production route table mounts %T at %v, not the operation server", mounted, pattern)
	}
	return server
}

// productionRecoveryRangeText renders one optional range for a failure message.
// It exists so a nil pointer is never handed to a format verb as a pointer.
func productionRecoveryRangeText(value *productionRecoveryRangeView) string {
	if value == nil {
		return "null"
	}
	return fmt.Sprintf("%d..%d", value.StartChapter, value.EndChapter)
}

// productionRecoveryFingerprint is the nine frozen hashes a run binding carries,
// the shape seedProductionBatch writes through productionBatchFingerprint.
func productionRecoveryFingerprint(hash string) string {
	keys := []string{"chapterBriefHash", "authorGuidanceHash", "dependencyHash", "contextSnapshotHash",
		"templateHash", "skillSnapshotHash", "modelLeaseRevision", "policyHash", "outputContractHash"}
	fields := make([]string, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, strconv.Quote(key)+":"+strconv.Quote(hash))
	}
	return "{" + strings.Join(fields, ",") + "}"
}

// productionRecoverySeedDirectory writes the directory runs described by seeds and
// checkpointed the database, so the file the read opens is the file the test
// seeded.
func productionRecoverySeedDirectory(t *testing.T, database string, seeds []productionRecoveryDirectorySeed) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, seed := range seeds {
		requested := fmt.Sprintf(`{"startChapter":%d,"endChapter":%d}`, seed.Requested.StartChapter, seed.Requested.EndChapter)
		remaining := "null"
		if seed.Committed.EndChapter < seed.Requested.EndChapter {
			remaining = fmt.Sprintf(`{"startChapter":%d,"endChapter":%d}`, seed.Committed.EndChapter+1, seed.Requested.EndChapter)
		}
		chapters := make([]string, 0, seed.Requested.EndChapter)
		for chapter := int64(1); chapter <= seed.Requested.EndChapter; chapter++ {
			chapters = append(chapters, strconv.FormatInt(chapter, 10))
		}
		payloadHash := sha256Hex(seed.OperationID)
		// generation_runs carries a foreign key onto generation_roots, so the root
		// comes first. The manifest is the one the renderer freezes for a directory
		// run: the operation, the structured output contract, the chapter numbers
		// the author asked for, and the requested range as a frozen author input.
		binding := `{"projectId":` + strconv.Quote(seed.ProjectID) +
			`,"epoch":` + strconv.Quote(productionRecoveryDirectoryEpoch) +
			`,"fingerprint":` + productionRecoveryFingerprint(strings.Repeat("e", 64)) +
			`,"contextSnapshotId":"ci-recovery-context-snapshot"` +
			`,"sourceManifest":{"operation":"chapter-blueprint-directory","outputContract":"structured-data"` +
			`,"selectedBlueprintChapterNumbers":[` + strings.Join(chapters, ",") + `]` +
			`,"authorInputs":[{"id":"directory:requested-range","text":` + strconv.Quote(requested) + `}]},"sourceRefs":[]}`
		if _, err := db.Exec(`INSERT INTO generation_roots (root_action_id, idempotency_key, action_json, budget_json)
 VALUES (?, ?, '{}', '{}')`, seed.RootID, seed.RootID+"-key"); err != nil {
			t.Fatalf("seed the directory root %v: %v", seed.RootID, err)
		}
		if _, err := db.Exec(`INSERT INTO generation_runs (run_id, root_action_id, binding_json, status, created_at_ms, open_key)
 VALUES (?, ?, ?, 'running', 1700000000100, ?)`, seed.RunID, seed.RootID, binding, seed.RunID+"-open"); err != nil {
			t.Fatalf("seed the directory run %v: %v", seed.RunID, err)
		}
		receipt := `{"artifactIdentity":{"epoch":` + strconv.Quote(productionRecoveryDirectoryEpoch) +
			`},"directoryProgress":{"operationId":` + strconv.Quote(seed.OperationID) +
			`,"payloadHash":` + strconv.Quote(payloadHash) +
			`,"sourceHandle":{"projectId":` + strconv.Quote(seed.ProjectID) +
			`,"epoch":` + strconv.Quote(productionRecoveryDirectoryEpoch) +
			`,"rootActionId":` + strconv.Quote(seed.RootID) + `,"runId":` + strconv.Quote(seed.RunID) + `}` +
			`,"requestedRange":` + requested +
			`,"committedRange":{"startChapter":` + strconv.FormatInt(seed.Committed.StartChapter, 10) +
			`,"endChapter":` + strconv.FormatInt(seed.Committed.EndChapter, 10) + `}` +
			`,"remainingRange":` + remaining + `}}`
		if _, err := db.Exec(`INSERT INTO generation_attempts (attempt_id, reservation_id, run_id, root_action_id,
 attempt_json, usage_receipt_json, invocation_nonce) VALUES (?, ?, ?, ?, '{}', ?, ?)`,
			seed.RunID+"-attempt", seed.RunID+"-reservation", seed.RunID, seed.RootID, receipt, seed.RunID+"-nonce"); err != nil {
			t.Fatalf("seed the directory receipt of %v: %v", seed.RunID, err)
		}
		if _, err := db.Exec(`INSERT INTO blueprint_commit_operations (operation_id, payload_hash, mode,
 start_chapter, end_chapter, character_sync_input) VALUES (?, ?, 'replace-range', ?, ?, '[]')`,
			seed.OperationID, payloadHash, seed.Committed.StartChapter, seed.Committed.EndChapter); err != nil {
			t.Fatalf("seed the blueprint commit operation %v: %v", seed.OperationID, err)
		}
	}
	productionEditorCheckpoint(t, db, database)
}

// TestProductionGenerationRecoveryServesDirectoryProgress is the whole served half
// of this file: generation:list-directory-progress answers 200 over HTTP, names the
// resumable directory operation and the settled one as two different states of the
// same derivation, ignores the run that lives in another project's database, leaves
// the author database byte-identical, and is served by the batch/directory slice
// rather than by this file's binder - which is proved by calling that binder
// against the finished lane and watching the bound set stay the same size.
func TestProductionGenerationRecoveryServesDirectoryProgress(t *testing.T) {
	// The foreign project is built first and never opened: its rows exist on disk in
	// its own database, which is the only fence this name has. The name takes no
	// project argument at all (batch_progress.go:275-279), so the session, not the
	// request, chooses the file the catalog is derived from.
	_, foreignPath, _, _, _ := productionEditorReadsRoot(t)
	productionRecoverySeedDirectory(t, productionRecoveryDatabasePath(foreignPath), []productionRecoveryDirectorySeed{{
		RunID:       productionRecoveryForeignRun,
		RootID:      productionRecoveryForeignRoot,
		ProjectID:   productionRecoveryForeignProject,
		OperationID: productionRecoveryForeignOperation,
		Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 4},
		Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 1},
	}})
	lane := productionRecoveryOpen(t, func(t *testing.T, database string) {
		productionRecoverySeedDirectory(t, database, []productionRecoveryDirectorySeed{
			{
				RunID:       productionRecoveryResumableRun,
				RootID:      productionRecoveryResumableRoot,
				ProjectID:   productionEditorProjectID,
				OperationID: productionRecoveryResumableOperation,
				Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 3},
				Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
			},
			{
				RunID:       productionRecoverySettledRun,
				RootID:      productionRecoverySettledRoot,
				ProjectID:   productionEditorProjectID,
				OperationID: productionRecoverySettledOperation,
				Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
				Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
			},
		})
	})

	digestBefore := productionMCPDatabaseDigest(t, lane.projectPath)
	var catalog productionRecoveryCatalogView
	lane.client.decode(t, productionGenerationRecoveryDirectory, []any{lane.frozen}, &catalog)
	digestAfter := productionMCPDatabaseDigest(t, lane.projectPath)
	if digestBefore != digestAfter {
		t.Fatal("generation:list-directory-progress moved a byte of the author database")
	}

	// Two committed runs, two receipts: the catalog lists every available directory
	// recovery, and the resumable one is the one whose remaining range is not null -
	// which is what resolveDirectoryContinuation filters on
	// (src/services/workflows/directory-workflow.ts:71-78).
	if len(catalog.Progress) != 2 {
		t.Fatalf("list-directory-progress returned %d receipts; the project has two directory runs: %v",
			len(catalog.Progress), catalog.Progress)
	}
	if len(catalog.Diagnostics) != 0 {
		t.Fatalf("list-directory-progress diagnosed a readable run: %d diagnostics, first %v",
			len(catalog.Diagnostics), catalog.Diagnostics[0])
	}
	byOperation := make(map[string]productionRecoveryProgressView, len(catalog.Progress))
	for _, item := range catalog.Progress {
		byOperation[item.OperationID] = item
		if item.SourceHandle.RunID == productionRecoveryForeignRun || item.OperationID == productionRecoveryForeignOperation {
			t.Fatalf("list-directory-progress served another project's run: %v", item)
		}
	}

	resumable, found := byOperation[productionRecoveryResumableOperation]
	if !found {
		t.Fatalf("list-directory-progress dropped the resumable operation %v: %v",
			productionRecoveryResumableOperation, catalog.Progress)
	}
	if resumable.RemainingRange == nil ||
		*resumable.RemainingRange != (productionRecoveryRangeView{StartChapter: 3, EndChapter: 3}) {
		t.Fatalf("the resumable operation reported remainingRange %v; chapters 3..3 are still unwritten",
			productionRecoveryRangeText(resumable.RemainingRange))
	}
	if resumable.RequestedRange != (productionRecoveryRangeView{StartChapter: 1, EndChapter: 3}) ||
		resumable.CommittedRange != (productionRecoveryRangeView{StartChapter: 1, EndChapter: 2}) {
		t.Fatalf("the resumable operation reported ranges %v/%v; the receipt froze 1..3 over 1..2",
			resumable.RequestedRange, resumable.CommittedRange)
	}
	if resumable.SourceHandle != (productionRecoveryHandleView{
		ProjectID:    productionEditorProjectID,
		Epoch:        productionRecoveryDirectoryEpoch,
		RootActionID: productionRecoveryResumableRoot,
		RunID:        productionRecoveryResumableRun,
	}) {
		t.Fatalf("the resumable operation reported the wrong source handle: %v", resumable.SourceHandle)
	}
	if len(resumable.AuthorInputs) != 1 || resumable.AuthorInputs[0].ID != "directory:requested-range" ||
		resumable.AuthorInputs[0].Text != `{"startChapter":1,"endChapter":3}` {
		t.Fatalf("the resumable operation did not project the source run's frozen author inputs: %v",
			resumable.AuthorInputs)
	}

	settled, found := byOperation[productionRecoverySettledOperation]
	if !found {
		t.Fatalf("list-directory-progress dropped the settled operation %v: %v",
			productionRecoverySettledOperation, catalog.Progress)
	}
	if settled.RemainingRange != nil {
		t.Fatalf("the settled operation reported remainingRange %v; its committed range covers what it requested",
			productionRecoveryRangeText(settled.RemainingRange))
	}

	// The family's fence, once per malformed call: one wire argument, the frozen
	// session, and a call without it is refused before any database work.
	for _, args := range [][]any{{}, {lane.frozen, lane.frozen}} {
		response := lane.client.call(t, productionGenerationRecoveryDirectory, args)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_ARGUMENTS") {
			t.Fatalf("list-directory-progress accepted %d arguments: %d %s",
				len(args), response.Code, response.Body.String())
		}
	}
	if response := lane.client.call(t, productionGenerationRecoveryDirectory, []any{map[string]any{}}); response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "PROJECT_SESSION_REQUIRED") {
		t.Fatalf("list-directory-progress accepted a call with no frozen session: %d %s",
			response.Code, response.Body.String())
	}

	// The finished lane serves every recovery name: the directory catalog through
	// the batch/directory slice, and list-recovery plus find-blueprint-recovery
	// through this family's binder.
	bound := lane.server.BoundOperations()
	if !productionGenerationRecoveryServedBy(bound) {
		t.Fatal("the finished lane does not serve every recovery name")
	}
	for _, operation := range productionGenerationRecoveryRefusedOperations() {
		for _, name := range bound {
			if name == operation {
				t.Fatalf("%v is bound in the production lane although it is refused", operation)
			}
		}
	}
}

// TestProductionGenerationRecoveryDirectoryProgressEmpty is the empty case: a
// project with no directory run answers an empty catalog, not a 501 and not a null.
func TestProductionGenerationRecoveryDirectoryProgressEmpty(t *testing.T) {
	lane := productionRecoveryOpen(t, nil)
	digestBefore := productionMCPDatabaseDigest(t, lane.projectPath)
	var catalog productionRecoveryCatalogView
	lane.client.decode(t, productionGenerationRecoveryDirectory, []any{lane.frozen}, &catalog)
	digestAfter := productionMCPDatabaseDigest(t, lane.projectPath)
	if digestBefore != digestAfter {
		t.Fatal("an empty generation:list-directory-progress moved a byte of the author database")
	}
	if catalog.Progress == nil {
		t.Fatal("the empty catalog answered null where the contract says an empty list")
	}
	if len(catalog.Progress) != 0 || len(catalog.Diagnostics) != 0 {
		t.Fatalf("a project with no directory run reported %d receipts and %d diagnostics",
			len(catalog.Progress), len(catalog.Diagnostics))
	}
}

// TestProductionLaneServesGenerationRecovery pins the two previously refused names
// at the finished lane's bound set: the family binder installs both, so neither
// falls back to the registry's 501.
func TestProductionLaneServesGenerationRecovery(t *testing.T) {
	lane := productionRecoveryOpen(t, nil)
	bound := lane.server.BoundOperations()
	for _, operation := range []string{productionGenerationRecoveryList, productionGenerationRecoveryFind} {
		if !productionRecoveryBoundContains(bound, operation) {
			t.Fatalf("%v is not bound in the finished production lane", operation)
		}
	}
}

// TestProductionGenerationRecoverySeams re-reads every served-evidence row and
// pins the served/refused tables. Nothing is refused now, so the seam table is
// empty and the served table covers all three names.
func TestProductionGenerationRecoverySeams(t *testing.T) {
	refused := productionGenerationRecoveryRefusedOperations()
	if len(refused) != 0 {
		t.Fatalf("the refusal list covers %d names, want none", len(refused))
	}
	if len(productionGenerationRecoverySeams()) != 0 {
		t.Fatal("the seam table is not empty although no name is refused")
	}

	served := make(map[string]bool, 3)
	for _, operation := range productionGenerationRecoveryServedOperations() {
		if served[operation] {
			t.Fatalf("%v is listed twice as served", operation)
		}
		served[operation] = true
		record, found := productionGenerationRecoveryServedEvidenceFor(operation)
		if !found || len(record.Evidence) == 0 {
			t.Fatalf("%v is recorded as served with no evidence", operation)
		}
		productionRecoveryMarkers(t, operation, record.Evidence)
	}
	if len(served) != 3 || !served[productionGenerationRecoveryDirectory] ||
		!served[productionGenerationRecoveryList] || !served[productionGenerationRecoveryFind] {
		t.Fatalf("the served list covers %d names, want the three recovery names", len(served))
	}

	// The tables are keyed, not positional.
	if _, found := productionGenerationRecoverySeamFor("generation:read"); found {
		t.Fatal("the seam table claims a name this file does not refuse")
	}
	if _, found := productionGenerationRecoveryServedEvidenceFor(productionGenerationRecoveryList); !found {
		t.Fatal("the served table no longer names generation:list-recovery")
	}
	if productionGenerationRecoveryServedBy([]string{"generation:list"}) {
		t.Fatal("the served check accepted a lane that does not serve every recovery name")
	}
	// The binder cannot be wired to a nil server.
	if err := bindProductionGenerationRecovery(nil, nil); err == nil || err.Error() != "TRANSPORT_SERVER_REQUIRED" {
		t.Fatalf("the binder accepted a nil server: %v", err)
	}
}

// productionRecoveryMarkers re-reads one table's rows from disk.
func productionRecoveryMarkers(t *testing.T, operation string, lines []productionGenerationRecoveryLine) {
	t.Helper()
	for _, line := range lines {
		if line.Line < 1 || line.Marker == "" {
			t.Fatalf("%v has an incomplete row for %v", operation, line.Path)
		}
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%v: %v: %v", operation, line.Path, err)
		}
		if !productionSeamContains(string(raw), line.Marker) {
			t.Fatalf("%v no longer documents %v anywhere in %v (recorded at line %d)",
				operation, line.Marker, line.Path, line.Line)
		}
	}
}
