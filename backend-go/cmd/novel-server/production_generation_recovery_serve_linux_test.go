//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
)

// This file pins the serve decision now that both names are served: the decision
// table says so, the seam table is empty, and the planner-continuation piece is
// PORTED with a real Go reader in internal/store/planning_continuation.go.

// productionRecoveryServeStaleLease is a lease string this lane never minted: the
// frozen context is well formed, so a call reaches the operation table instead of
// being refused by argument validation.
const productionRecoveryServeStaleLease = "lease-0000000000-stale"

// TestProductionGenerationRecoveryServeDecisionTable pins the verdict: both names
// served, each with no reason and no seam, and the decision table agreeing with the
// empty seam table it belongs to.
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
		if !decision.Served {
			t.Fatalf("%v is recorded as refused; this pass serves both names", decision.Operation)
		}
		if decision.Reason != "" {
			t.Fatalf("%v is served with a reason %v", decision.Operation, decision.Reason)
		}
		if _, found := productionGenerationRecoverySeamFor(decision.Operation); found {
			t.Fatalf("%v is served but still carries a seam", decision.Operation)
		}
	}
	if !seen[productionGenerationRecoveryFind] || !seen[productionGenerationRecoveryList] {
		t.Fatalf("the decision table does not cover both recovery names: %v", seen)
	}
	if err := productionRecoveryServeAgrees(decisions, productionGenerationRecoverySeams(),
		productionRecoveryServePieces()); err != nil {
		t.Fatalf("the decision table and the seam table disagree: %v", err)
	}
	// A refused name with no seam is still refused, so the agreement cannot drift
	// back to an undocumented refusal.
	unseamed := []productionRecoveryServeDecision{{Operation: productionGenerationRecoveryDirectory,
		Served: false, Reason: productionRecoveryServeReasonBuckets}}
	if err := productionRecoveryServeAgrees(unseamed, productionGenerationRecoverySeams(),
		productionRecoveryServePieces()); err == nil ||
		err.Error() != "PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_UNSEAMED" {
		t.Fatalf("a refused name with no seam was accepted: %v", err)
	}
	if err := bindProductionGenerationRecoveryServe(nil); err == nil ||
		err.Error() != "TRANSPORT_SERVER_REQUIRED" {
		t.Fatalf("the serve binder accepted a nil server: %v", err)
	}
}

// TestProductionGenerationRecoveryServePieceEvidence re-reads every declaration a
// piece verdict names, then pins the three verdicts this pass turns on: the
// blueprint author edit is VALIDATED, the binding rebuild is SUBSTITUTED, and the
// planner continuation is PORTED by the real reader.
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
	if !found || author.Verdict != productionRecoveryServeValidated {
		t.Fatalf("the blueprint author edit is recorded as %v; the Go reader accepts the reference's own receipt now", author.Verdict)
	}
	rebuild, found := productionRecoveryServePieceFor(productionRecoveryServePieceRebuild)
	if !found || rebuild.Verdict != productionRecoveryServeSubstituted {
		t.Fatalf("the binding rebuild is recorded as %v; ReproduceBinding answers a different question", rebuild.Verdict)
	}
	continuation, found := productionRecoveryServePieceFor(productionRecoveryServePieceContinuation)
	if !found || continuation.Verdict != productionRecoveryServePorted {
		t.Fatalf("the planner continuation is recorded as %v; the Go reader is real now", continuation.Verdict)
	}
	// The PORTED verdict has to rest on a real reader in the store file, not on a
	// marker in some other file.
	raw, err := os.ReadFile("../../internal/store/planning_continuation.go")
	if err != nil {
		t.Fatalf("the planner continuation reader is unreadable: %v", err)
	}
	if !productionRecoveryServeReadsContinuation(string(raw)) {
		t.Fatal("planning_continuation.go does not read the effect bag and the receipt; the PORTED verdict is stale")
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
