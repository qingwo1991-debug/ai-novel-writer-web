//go:build linux

package main

import (
	"strings"
	"testing"
)

// This file proves the re-examined record in
// production_character_proposal_residual_linux.go: the transactional claim the family
// used to refuse with is retired, the lines that retire it are still there, and the
// two names are still the family's refused ones. The line reader is the one the
// record's own test defines
// (production_character_proposal_pending_linux_test.go), so both notes are read the
// same way: a marker must be on the cited line, not merely somewhere in the file.
func TestProductionCharacterProposalResidualVerdict(t *testing.T) {
	// The verdict is the one sentence this record exists to keep true, so it has to
	// name the retraction and the surviving blocker. A verdict that named neither
	// would be the prose assertion this record was rewritten to replace.
	verdict := productionCharacterProposalResidualVerdict
	for _, want := range []string{
		"internal/store/summary.go:681",
		"finalized-character-generation-proof.ts:13",
		"importCharacterEntriesFromArtifacts",
	} {
		if !strings.Contains(verdict, want) {
			t.Fatalf("the verdict does not name %v", want)
		}
	}
	// The two sentences the old record published are gone, not reworded: the family
	// must not refuse for a reason the store disproves.
	for _, gone := range []string{
		"cannot be composed in one transaction",
		"no exported store call composes",
		"the only BEGIN IMMEDIATE handle is unexported (internal/store/reviewrevision.go:41) and the proposal reader and writer take a *sql.DB",
	} {
		if strings.Contains(verdict, gone) {
			t.Fatalf("the verdict still carries the retracted claim %v", gone)
		}
	}

	seams := productionCharacterProposalResidualSeams()
	if len(seams) != 2 {
		t.Fatalf("the record covers %v names; the family refuses two", len(seams))
	}
	if seams[0].Operation != productionCharacterProposalListPendingName ||
		seams[1].Operation != productionCharacterProposalReadPendingName {
		t.Fatalf("the record covers %v and %v, not the two pending-finalized names",
			seams[0].Operation, seams[1].Operation)
	}
	// Both names are inventoried by this family and neither is one of the four the
	// binder installs, which is what makes "refused" a statement about binding
	// rather than about the inventory. The served four are named rather than
	// counted from the front: the inventory is not ordered served-first - stage,
	// read, list-pending, read-pending, approve, cancel - so a positional rule
	// would pass by accident today and fail the moment the family is reordered.
	served := []string{
		productionCharacterProposalStageName,
		productionCharacterProposalReadName,
		productionCharacterProposalApproveName,
		productionCharacterProposalCancelName,
	}
	inventory := productionCharacterProposalOperations()
	for _, seam := range seams {
		index := -1
		for position, name := range inventory {
			if name == seam.Operation {
				index = position
			}
		}
		if index < 0 {
			t.Fatalf("%v is not in the family inventory at all", seam.Operation)
		}
		for _, name := range served {
			if name == seam.Operation {
				t.Fatalf("%v is inventoried as served, so it cannot also be refused", seam.Operation)
			}
		}
	}
	// The four served names carry no seam: a seam is only ever a promise that the
	// name is refused.
	for _, operation := range served {
		if _, documented := productionCharacterProposalPendingSeam(operation); documented {
			t.Fatalf("%v is served but carries a seam", operation)
		}
	}
	// Every cited line still carries its marker, so the record cannot outlive the
	// declarations it names.
	for _, seam := range seams {
		if len(seam.Missing) < 5 {
			t.Fatalf("%v cites only %v ports; the record carries the four proof entry points and the gate",
				seam.Operation, len(seam.Missing))
		}
		for _, port := range seam.Missing {
			productionCharacterProposalPendingCitedLine(t, port.Path, port.Line, port.Marker)
		}
	}
	// The family's own table still refuses the same two names, in the same order, so
	// the sharpened record and the published seam cannot disagree.
	pending := productionCharacterProposalPendingSeams()
	if len(pending) != len(seams) {
		t.Fatalf("the family refuses %v names and this record covers %v", len(pending), len(seams))
	}
	for position, seam := range pending {
		if seam.Operation != seams[position].Operation {
			t.Fatalf("the family refuses %v where this record covers %v",
				seam.Operation, seams[position].Operation)
		}
	}
	// The retraction is evidence, not a change of mind: every line it cites still
	// exists, and the handle it retired the claim about is still unexported - the
	// composition lives in the store, which is why package main never needed it.
	retraction := productionCharacterProposalResidualRetraction()
	if len(retraction) < 5 {
		t.Fatalf("the retraction cites %v lines; the record rests on five", len(retraction))
	}
	for _, line := range retraction {
		productionCharacterProposalPendingCitedLine(t, line.Path, line.Line, line.Marker)
	}
	productionCharacterProposalPendingCitedLine(t, "../../internal/store/reviewrevision.go", 41,
		"func (r *ReviewRevisions) immediate(")
}
