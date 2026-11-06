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
	// Every cited line still carries its marker.
	for _, seam := range seams {
		if len(seam.Missing) < 6 {
			t.Fatalf("%v cites only %v ports; the record carries the transaction, the two halves and the three proofs",
				seam.Operation, len(seam.Missing))
		}
		for _, port := range seam.Missing {
			if err := productionResidualSourceClaims(port.Path, port.Line, port.Marker); err != nil {
				t.Fatalf("%v cites it: %v", seam.Operation, err)
			}
		}
	}
	// The transaction handle is unexported: that is the whole reason a package main
	// adapter cannot compose the receipt and the projection itself. Assert the
	// receiver and the method name as the file spells them, so exporting it (or
	// moving it) is a test failure rather than a silent change of facts.
	immediate, err := productionResidualSourceLineOf("../../internal/store/reviewrevision.go", "*ReviewRevisions) immediate(")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if strings.Contains(immediate, "*ReviewRevisions) Immediate(") {
		t.Fatalf("internal/store/reviewrevision.go now exports the immediate handle, so this record's claim needs re-reading")
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
		found, err := productionResidualSourceLineOf(arm.path, arm.marker)
		if err != nil {
			t.Fatalf("%v no longer takes a database, so the record's claim needs re-reading: %v",
				arm.path, err)
		}
		if strings.Contains(found, "*sql.Tx") {
			t.Fatalf("%v now takes a transaction: this name can be composed and served",
				arm.path)
		}
	}
	// The pattern that would fix it already exists next door, which is what makes
	// the gap a missing twin rather than an unknown design.
	if _, err := productionResidualSourceLineOf("../../internal/store/finalized_character_context.go", "tx *sql.Tx"); err != nil {
		t.Fatalf("the *sql.Tx twin the record points at is gone: %v", err)
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
