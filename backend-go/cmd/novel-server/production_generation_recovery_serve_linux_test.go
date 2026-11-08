//go:build linux

package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixture below is a project that really holds the states the two refused
// names would answer about, so the refusal cannot be mistaken for an empty
// project. Three chapter-blueprint-directory runs are seeded the way the desktop
// writes them, which is the only way they can exist here: the ledger refuses every
// blueprint operation (internal/store/generation_ledger.go:719-720), so no writer
// in this lane can mint one.
//
//	productionRecoveryServeResumable  committed 1..2 of a requested 1..3 - the
//	                                  state the blueprint page exists for
//	productionRecoveryServeSettled    committed 1..2 of a requested 1..2 - no
//	                                  remaining chapter, nothing to resume
//	productionRecoveryServeSaved      the resumable range plus the exact
//	                                  $runEffects.blueprintAuthorEdit receipt the
//	                                  desktop writes - and the receipt the store's
//	                                  reader currently refuses
const (
	productionRecoveryServeResumableRun       = "ci-recovery-serve-resumable-1"
	productionRecoveryServeResumableRoot      = "ci-recovery-serve-resumable-root-1"
	productionRecoveryServeResumableOperation = "ci-recovery-serve-commit-resumable"

	productionRecoveryServeSettledRun       = "ci-recovery-serve-settled-1"
	productionRecoveryServeSettledRoot      = "ci-recovery-serve-settled-root-1"
	productionRecoveryServeSettledOperation = "ci-recovery-serve-commit-settled"

	productionRecoveryServeSavedRun       = "ci-recovery-serve-saved-1"
	productionRecoveryServeSavedRoot      = "ci-recovery-serve-saved-root-1"
	productionRecoveryServeSavedOperation = "ci-recovery-serve-commit-saved"

	// productionRecoveryServeStaleLease is a lease string this lane never minted:
	// the frozen context is well formed, so the call reaches the operation table
	// instead of being refused by argument validation.
	productionRecoveryServeStaleLease = "lease-0000000000-stale"
)

// productionRecoveryServeCallShape is one frozen context a browser could send for
// the same run: the project's own session, a session naming another project, and
// this project with a lease the lane never minted.
type productionRecoveryServeCallShape struct {
	Name    string
	Context map[string]any
}

// TestProductionGenerationRecoveryServeRefusesSeededRecovery is the whole served
// half of this file, in the negative: the project holds a resumable blueprint
// recovery, a settled one and an author-saved one - all three proved through the
// SERVED directory catalog, which reads the same rows - and both recovery names
// still answer the registry's fail-closed 501 for every frozen context shape,
// leaving the author database byte-identical. The catalog also pins the defect
// this pass found: the author-saved run comes back as a diagnostic, not as
// progress, because bpAuthorEditCode reads its committedRange with the wrong
// accessor.
func TestProductionGenerationRecoveryServeRefusesSeededRecovery(t *testing.T) {
	lane := productionRecoveryOpen(t, func(t *testing.T, database string) {
		productionRecoverySeedDirectory(t, database, []productionRecoveryDirectorySeed{
			{
				RunID:       productionRecoveryServeResumableRun,
				RootID:      productionRecoveryServeResumableRoot,
				ProjectID:   productionEditorProjectID,
				OperationID: productionRecoveryServeResumableOperation,
				Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 3},
				Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
			},
			{
				RunID:       productionRecoveryServeSettledRun,
				RootID:      productionRecoveryServeSettledRoot,
				ProjectID:   productionEditorProjectID,
				OperationID: productionRecoveryServeSettledOperation,
				Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
				Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
			},
			{
				RunID:       productionRecoveryServeSavedRun,
				RootID:      productionRecoveryServeSavedRoot,
				ProjectID:   productionEditorProjectID,
				OperationID: productionRecoveryServeSavedOperation,
				Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 3},
				Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
			},
		})
		productionRecoveryServeSeedAuthorEdit(t, database,
			productionRecoveryServeSavedRoot, productionRecoveryServeSavedRun, productionRecoveryServeSavedOperation,
			productionRecoveryRangeView{StartChapter: 1, EndChapter: 3},
			productionRecoveryRangeView{StartChapter: 1, EndChapter: 2})
	})

	// The population, not the projection, is what fails: the served directory
	// catalog lists the two runs whose receipts the store can read, and reports the
	// author-saved one as a diagnostic. That diagnostic is the defect this file
	// records as REFUTED - bpAuthorEditCode parses the receipt's committedRange with
	// the startChapter/endChapter accessor (internal/store/batch_progress.go:1235)
	// while the receipt carries {from,to} - and it is why a real author-saved
	// blueprint run is missing from a served catalog today.
	var catalog productionRecoveryCatalogView
	lane.client.decode(t, productionGenerationRecoveryDirectory, []any{lane.frozen}, &catalog)
	if len(catalog.Progress) != 3 {
		t.Fatalf("the served catalog reported %d readable directory receipts; the resumable, settled and author-saved runs all carry receipts the store accepts: %v",
			len(catalog.Progress), catalog.Progress)
	}
	if len(catalog.Diagnostics) != 0 {
		t.Fatalf("the served catalog reported %d diagnostics; every seeded receipt is the reference's own shape",
			len(catalog.Diagnostics))
	}
	byOperation := make(map[string]productionRecoveryProgressView, len(catalog.Progress))
	for _, item := range catalog.Progress {
		byOperation[item.OperationID] = item
	}
	resumable, found := byOperation[productionRecoveryServeResumableOperation]
	if !found {
		t.Fatalf("the resumable operation %v is not in the served catalog: %v",
			productionRecoveryServeResumableOperation, catalog.Progress)
	}
	if resumable.RemainingRange == nil ||
		*resumable.RemainingRange != (productionRecoveryRangeView{StartChapter: 3, EndChapter: 3}) {
		t.Fatalf("the resumable run reported remainingRange %v; chapter 3 is still unwritten",
			productionRecoveryRangeText(resumable.RemainingRange))
	}
	settled, found := byOperation[productionRecoveryServeSettledOperation]
	if !found {
		t.Fatalf("the settled operation %v is not in the served catalog: %v",
			productionRecoveryServeSettledOperation, catalog.Progress)
	}
	if settled.RemainingRange != nil {
		t.Fatalf("the settled run reported remainingRange %v; it has nothing left to write",
			productionRecoveryRangeText(settled.RemainingRange))
	}
	// The author-saved run is listed as progress at last: bpAuthorEditCode reads
	// the receipt's committedRange with the {from,to} accessor the reference
	// writes, so the reference's own receipt shape is accepted, not diagnosed.
	saved, found := byOperation[productionRecoveryServeSavedOperation]
	if !found {
		t.Fatalf("the author-saved operation %v is not in the served catalog: %v",
			productionRecoveryServeSavedOperation, catalog.Progress)
	}
	if saved.RemainingRange != nil {
		t.Fatalf("the author-saved run reported remainingRange %v; it wrote through its committed end",
			productionRecoveryRangeText(saved.RemainingRange))
	}

	// The three frozen contexts are all well formed and all name state this
	// project really holds. None of them changes the answer, and the answer is the
	// same bytes every time: a refused name is not admitted per project.
	shapes := []productionRecoveryServeCallShape{
		{Name: "own project", Context: lane.frozen},
		{Name: "foreign project", Context: map[string]any{
			"projectId":   productionRecoveryForeignProject,
			"projectPath": lane.projectPath,
			"leaseId":     lane.frozen["leaseId"],
		}},
		{Name: "stale lease", Context: map[string]any{
			"projectId":   lane.frozen["projectId"],
			"projectPath": lane.projectPath,
			"leaseId":     productionRecoveryServeStaleLease,
		}},
	}
	digestBefore := productionMCPDatabaseDigest(t, lane.projectPath)
	for _, operation := range []string{productionGenerationRecoveryFind, productionGenerationRecoveryList} {
		first := ""
		firstShape := ""
		for _, shape := range shapes {
			response := lane.client.call(t, operation, []any{shape.Context})
			if response.Code != http.StatusNotImplemented ||
				!strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
				t.Fatalf("%v over the %v session = %d %s, want 501 GO_OPERATION_NOT_MIGRATED",
					operation, shape.Name, response.Code, response.Body.String())
			}
			if first == "" {
				first, firstShape = response.Body.String(), shape.Name
				continue
			}
			if response.Body.String() != first {
				t.Fatalf("%v answered the %v session with %s but the %v session with %s; a refused name has one answer",
					operation, shape.Name, response.Body.String(), firstShape, first)
			}
		}
	}
	digestAfter := productionMCPDatabaseDigest(t, lane.projectPath)
	if digestBefore != digestAfter {
		t.Fatal("a refused recovery moved a byte of the author database")
	}

	// The binder is the file's own claim, so it is called against the finished
	// lane: it accepts the lane and installs nothing, and both names stay unbound.
	boundBefore := lane.server.BoundOperations()
	if err := bindProductionGenerationRecoveryServe(lane.server); err != nil {
		t.Fatalf("the serve binder refused the finished lane: %v", err)
	}
	boundAfter := lane.server.BoundOperations()
	if len(boundAfter) != len(boundBefore) {
		t.Fatalf("the serve binder installed %d operations", len(boundAfter)-len(boundBefore))
	}
	for _, decision := range productionRecoveryServeDecisions() {
		if productionRecoveryBoundContains(boundAfter, decision.Operation) {
			t.Fatalf("%v is bound in the production lane although this file refuses it", decision.Operation)
		}
	}
}

// productionRecoveryServeSeedAuthorEdit stamps the frozen blueprint author-edit
// effect the desktop writes with recordBlueprintAuthorEdit
// (electron/repositories/generation-run-repository.ts:215-225). The receipt is the
// one readBlueprintAuthorEdit validates (:194-214), rebuilt here from the rows the
// directory seed wrote - the requested range, the committed range, the remaining
// range and the commit operation's payload hash.
func productionRecoveryServeSeedAuthorEdit(t *testing.T, database, rootID, runID, operationID string,
	requested, committed productionRecoveryRangeView) {
	t.Helper()
	var remaining any
	if committed.EndChapter < requested.EndChapter {
		remaining = map[string]any{"from": committed.EndChapter + 1, "to": requested.EndChapter}
	}
	receipt := map[string]any{
		"kind":        "blueprint-author-edit",
		"operationId": operationID,
		"sourceHandle": map[string]any{
			"projectId":    productionEditorProjectID,
			"epoch":        productionRecoveryDirectoryEpoch,
			"rootActionId": rootID,
			"runId":        runID,
		},
		"requestedRange": map[string]any{"from": requested.StartChapter, "to": requested.EndChapter},
		"committedRange": map[string]any{"from": committed.StartChapter, "to": committed.EndChapter},
		"remainingRange": remaining,
		"payloadHash":    sha256Hex(operationID),
	}
	effect, err := json.Marshal(map[string]any{"blueprintAuthorEdit": receipt})
	if err != nil {
		t.Fatal(err)
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	// The effect bag is written under the same quoted key the reference uses
	// ($runEffects), so the receipt is found by the path the reader reads.
	if _, err := db.Exec(`UPDATE generation_runs SET binding_json=json_set(binding_json,'$."$runEffects"',json(?))
 WHERE run_id=?`, string(effect), runID); err != nil {
		_ = db.Close()
		t.Fatalf("stamp the blueprint author edit of %v: %v", runID, err)
	}
	productionEditorCheckpoint(t, db, database)
}

// TestProductionGenerationRecoveryServeDecisionTable pins the verdict itself: both
// names refused, each with a symbolic reason, each still carrying a seam, and the
// decision table agreeing with the seam table it belongs to - including the one row
// this pass dropped.
func TestProductionGenerationRecoveryServeDecisionTable(t *testing.T) {
	decisions := productionRecoveryServeDecisions()
	if len(decisions) != 2 {
		t.Fatalf("the decision table covers %d names, want the two this file exists for", len(decisions))
	}
	seen := make(map[string]bool, len(decisions))
	for _, decision := range decisions {
		if decision.Operation == "" || seen[decision.Operation] {
			t.Fatalf("the decision table repeats or omits an operation: %v", decision.Operation)
		}
		seen[decision.Operation] = true
		if decision.Served {
			t.Fatalf("%v is recorded as served; this pass serves neither name", decision.Operation)
		}
		if decision.Reason != productionRecoveryServeReasonWriteState &&
			decision.Reason != productionRecoveryServeReasonBuckets {
			t.Fatalf("%v is refused with the non-symbolic reason %v", decision.Operation, decision.Reason)
		}
		if _, found := productionGenerationRecoverySeamFor(decision.Operation); !found {
			t.Fatalf("%v is refused with no seam; the refusal would name no gap", decision.Operation)
		}
	}
	if !seen[productionGenerationRecoveryFind] || !seen[productionGenerationRecoveryList] {
		t.Fatalf("the decision table does not cover both recovery names: %v", seen)
	}
	if err := productionRecoveryServeAgrees(decisions, productionGenerationRecoverySeams(),
		productionRecoveryServePieces()); err != nil {
		t.Fatalf("the decision table and the seam table disagree: %v", err)
	}

	// Every drift the agreement exists to catch, one case each.
	servedButSeamed := []productionRecoveryServeDecision{{Operation: productionGenerationRecoveryFind, Served: true}}
	if err := productionRecoveryServeAgrees(servedButSeamed, productionGenerationRecoverySeams(),
		productionRecoveryServePieces()); err == nil ||
		err.Error() != "PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_STILL_SEAMED" {
		t.Fatalf("a served name that is still a seam was accepted: %v", err)
	}
	unseamed := []productionRecoveryServeDecision{{Operation: productionGenerationRecoveryDirectory,
		Served: false, Reason: productionRecoveryServeReasonBuckets}}
	if err := productionRecoveryServeAgrees(unseamed, productionGenerationRecoverySeams(),
		productionRecoveryServePieces()); err == nil ||
		err.Error() != "PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_UNSEAMED" {
		t.Fatalf("a refused name with no seam was accepted: %v", err)
	}
	invented := []productionRecoveryServeDecision{{Operation: productionGenerationRecoveryList,
		Served: false, Reason: "PRODUCTION_GENERATION_RECOVERY_BECAUSE"}}
	if err := productionRecoveryServeAgrees(invented, productionGenerationRecoverySeams(),
		productionRecoveryServePieces()); err == nil ||
		err.Error() != "PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_REASON_INVALID" {
		t.Fatalf("a refusal with an invented reason was accepted: %v", err)
	}
	if err := productionRecoveryServeAgrees(nil, nil, nil); err == nil ||
		err.Error() != "PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_INCOMPLETE" {
		t.Fatalf("an empty decision set was accepted: %v", err)
	}
	if err := bindProductionGenerationRecoveryServe(nil); err == nil ||
		err.Error() != "TRANSPORT_SERVER_REQUIRED" {
		t.Fatalf("the serve binder accepted a nil server: %v", err)
	}

	// The correction this pass makes: the find seam keeps the author-edit receipt,
	// but now by the Go accessor that refuses it rather than by a reference
	// declaration standing in for a reader that does not exist - and it still names
	// the planner continuation, which keeps the name refused either way.
	seam, found := productionGenerationRecoverySeamFor(productionGenerationRecoveryFind)
	if !found {
		t.Fatal("the find seam disappeared")
	}
	accessor, continuation := false, false
	for _, line := range seam.Missing {
		if strings.Contains(line.Marker, "bpRangeObject(receipt") {
			accessor = true
		}
		if strings.Contains(line.Marker, "readBlueprintAuthorEdit(runId: string)") {
			t.Fatalf("the find seam still cites the reference declaration as though Go had no reader for the receipt; cite the accessor that refuses it instead")
		}
		if strings.Contains(line.Marker, "readPlanningContinuation") {
			continuation = true
		}
	}
	if !accessor {
		t.Fatal("the find seam no longer names the author-edit accessor that refuses the desktop's receipt")
	}
	if !continuation {
		t.Fatal("the find seam no longer names the planner continuation that keeps it refused")
	}
}

// TestProductionGenerationRecoveryServePieceEvidence re-reads every declaration a
// piece verdict names, then checks the two verdicts this pass turns on: the
// blueprint author edit is REFUTED by a Go reader whose range accessor disagrees
// with the receipt, the binding rebuild is SUBSTITUTED by an answer to a different
// question, and no Go code in the two files that read binding_json has grown a
// planner-continuation reader behind the refusal.
func TestProductionGenerationRecoveryServePieceEvidence(t *testing.T) {
	pieces := productionRecoveryServePieces()
	if len(pieces) == 0 {
		t.Fatal("the piece table is empty")
	}
	for _, piece := range pieces {
		if piece.Name == "" || !productionRecoveryServeVerdictKnown(piece.Verdict) || len(piece.Evidence) == 0 {
			t.Fatalf("the piece %v has an incomplete verdict", piece.Name)
		}
		productionRecoveryMarkers(t, piece.Name, piece.Evidence)
	}

	author, found := productionRecoveryServePieceFor(productionRecoveryServePieceAuthorEdit)
	if !found || author.Verdict != productionRecoveryServeRefuted {
		t.Fatalf("the blueprint author edit is recorded as %v; a Go reader exists and refuses the reference's own receipt", author.Verdict)
	}
	// The refuted verdict has to rest on all three halves of the mismatch: the Go
	// reader, the accessor that rejects the receipt, and the reference's own
	// .from/.to read of it.
	ported, mismatch, reference := false, false, false
	for _, line := range author.Evidence {
		if strings.Contains(line.Marker, "func bpAuthorEditCode(") {
			ported = true
		}
		if strings.Contains(line.Marker, "bpRangeObject(receipt") {
			mismatch = true
		}
		if strings.Contains(line.Marker, "receipt.committedRange.from") {
			reference = true
		}
	}
	if !ported || !mismatch || !reference {
		t.Fatalf("the refuted verdict is not grounded: reader %v, accessor %v, reference %v",
			ported, mismatch, reference)
	}
	rebuild, found := productionRecoveryServePieceFor(productionRecoveryServePieceRebuild)
	if !found || rebuild.Verdict != productionRecoveryServeSubstituted {
		t.Fatalf("the binding rebuild is recorded as %v; ReproduceBinding answers a different question", rebuild.Verdict)
	}
	if author.Verdict == rebuild.Verdict {
		t.Fatal("the receipt reader and the binding rebuild carry one verdict although they are different questions")
	}

	// The ABSENT continuation verdict, checked where a reader would have to live:
	// a real one must name both the effect bag and the receipt key in code, and no
	// such line exists in either file that already reads binding_json.
	for _, path := range []string{"../../internal/store/batch_progress.go", "../../internal/store/generation_ledger.go"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v: %v", path, err)
		}
		if productionRecoveryServeReadsContinuation(string(raw)) {
			t.Fatalf("%v names the planner continuation in code; the ABSENT verdict is stale", path)
		}
	}
}

// productionRecoveryServeReadsContinuation reports whether one Go source names
// both the frozen effect bag and the planner continuation outside a line comment -
// the shape a reader of that receipt has to have.
func productionRecoveryServeReadsContinuation(source string) bool {
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if strings.Contains(line, "$runEffects") && strings.Contains(line, "planningContinuation") {
			return true
		}
	}
	return false
}
