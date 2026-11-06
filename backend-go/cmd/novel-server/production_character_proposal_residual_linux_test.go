//go:build linux

package main

import (
	"strings"
	"testing"
)

// TestProductionCharacterProposalResidualVerdict proves the re-examination recorded
// in production_character_proposal_residual_linux.go is still true, in the only way
// a comment cannot: the transaction handle that record cites is still unexported,
// the two halves of the family still take a database rather than a transaction, the
// twin that would fix it still exists for a neighbouring read but not for this one,
// and the family's own seam table still names the same two operations. The line
// reader is the one the sibling residual record defines, so both records are read
// the same way.
func TestProductionCharacterProposalResidualVerdict(t *testing.T) {
	seams := productionCharacterProposalResidualSeams()
	if len(seams) != 2 {
		t.Fatalf("the record covers %v names; the family refuses two", len(seams))
	}
	if seams[0].Operation != productionCharacterProposalListPendingName ||
		seams[1].Operation != productionCharacterProposalReadPendingName {
		t.Fatalf("the record covers %v and %v, not the two pending-finalized names",
			seams[0].Operation, seams[1].Operation)
	}
	// Both names are inventoried by this family and both sit past the served four,
	// which is what makes "refused" a statement about binding rather than about the
	// inventory.
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
		if index < 4 {
			t.Fatalf("%v sits at %v in the family inventory, which is the served half", seam.Operation, index)
		}
	}
	// Every cited line still carries its marker.
	for _, seam := range seams {
		if len(seam.Missing) < 6 {
			t.Fatalf("%v cites only %v ports; the record carries the transaction, the two halves and the three proofs",
				seam.Operation, len(seam.Missing))
		}
		for _, port := range seam.Missing {
			found, err := productionResidualSourceLine(port.Path, port.Line)
			if err != nil {
				t.Fatalf("%v cites %v: %v", seam.Operation, port.Path, err)
			}
			if !strings.Contains(found, port.Marker) {
				t.Fatalf("%v cites %v line %v for %v, but that line no longer carries it",
					seam.Operation, port.Path, port.Line, port.Marker)
			}
		}
	}
	// The transaction handle is unexported: that is the whole reason a package main
	// adapter cannot compose the receipt and the projection itself. Assert the
	// receiver and the method name as the file spells them, so exporting it (or
	// moving it) is a test failure rather than a silent change of facts.
	immediate, err := productionResidualSourceLine("../../internal/store/reviewrevision.go", 41)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(immediate, "*ReviewRevisions) immediate(") ||
		strings.Contains(immediate, "*ReviewRevisions) Immediate(") {
		t.Fatalf("internal/store/reviewrevision.go:41 no longer declares the unexported immediate handle")
	}
	// The two halves of this family take a database, never a transaction: this is
	// what "cannot compose in one transaction" means concretely.
	for _, arm := range []struct {
		path   string
		line   int
		marker string
	}{
		{"production_character_proposals_linux.go", 1667, "db *sql.DB"},
		{"production_character_proposals_linux.go", 1762, "db *sql.DB"},
	} {
		found, err := productionResidualSourceLine(arm.path, arm.line)
		if err != nil {
			t.Fatalf("%v", err)
		}
		if !strings.Contains(found, arm.marker) {
			t.Fatalf("%v line %v no longer takes a database, so the record's claim needs re-reading",
				arm.path, arm.line)
		}
		if strings.Contains(found, "*sql.Tx") {
			t.Fatalf("%v line %v now takes a transaction: this name can be composed and served",
				arm.path, arm.line)
		}
	}
	// The pattern that would fix it already exists next door, which is what makes
	// the gap a missing twin rather than an unknown design.
	twin, err := productionResidualSourceLine("../../internal/store/finalized_character_context.go", 210)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(twin, "tx *sql.Tx") {
		t.Fatalf("internal/store/finalized_character_context.go:210 is no longer the *sql.Tx twin the record points at")
	}
	// The family's own table is untouched by this record and still refuses the same
	// two names, so the sharpened text and the published seam cannot disagree.
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
	// The verdict must name the handle it rests on, otherwise it is the same prose
	// assertion this record was written to replace.
	if !strings.Contains(productionCharacterProposalResidualVerdict, "internal/store/reviewrevision.go:41") {
		t.Fatalf("the verdict no longer names the unexported immediate handle")
	}
}
