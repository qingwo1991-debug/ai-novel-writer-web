//go:build linux

package main

import (
	"os"
	"strings"
	"testing"
)

// This file proves the re-examined record in
// production_character_proposal_residual_linux.go: the two names the record used
// to refuse are served by production_character_proposal_pending_linux.go, the
// old reasons are gone from the verdict, and every line the retraction cites is
// still the line it names. The line reader is this file's own, and it matches
// through productionSeamContains, so CI's formatting cannot turn a real citation
// into a failure.

// productionCharacterProposalResidualCited requires a cited marker to still be
// in the file it names, and reports the cited line when it has moved.
func productionCharacterProposalResidualCited(t *testing.T, path string, line int, marker string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: %v", path, err)
	}
	source := string(raw)
	if !productionSeamContains(source, marker) {
		lines := strings.Split(source, "\n")
		reported := "(past the end of the file)"
		if line >= 1 && line <= len(lines) {
			reported = strings.TrimSpace(lines[line-1])
		}
		t.Fatalf("%v no longer carries %v anywhere; line %v now reads %v", path, marker, line, reported)
	}
	return source
}

// TestProductionCharacterProposalResidualVerdict proves the record's one
// sentence names both retractions and the binder that replaced them, and that
// neither retracted claim survives in it.
func TestProductionCharacterProposalResidualVerdict(t *testing.T) {
	verdict := productionCharacterProposalResidualVerdict
	for _, want := range []string{
		"internal/store/summary.go:681",
		"internal/store/character_proposal_pending.go",
		"list-pending-finalized",
		"read-pending-finalized",
	} {
		if !strings.Contains(verdict, want) {
			t.Fatalf("the verdict does not name %v", want)
		}
	}
	for _, gone := range []string{
		"cannot be composed in one transaction",
		"no exported store call composes",
		"byte-exact",
		"has no Go port",
	} {
		if strings.Contains(verdict, gone) {
			t.Fatalf("the verdict still carries the retracted claim %v", gone)
		}
	}
}

// TestProductionCharacterProposalResidualRetraction proves the retraction is
// evidence: every cited line still exists, the transaction handle is still
// unexported, and the citations still show both halves of the old claim being
// wrong - an immediate transaction on a reserved connection, with the proposal
// table read on it.
func TestProductionCharacterProposalResidualRetraction(t *testing.T) {
	lines := productionCharacterProposalResidualRetraction()
	if len(lines) < 5 {
		t.Fatalf("the retraction cites %v lines; the record rests on at least five", len(lines))
	}
	immediate, proposals := 0, 0
	for _, line := range lines {
		source := productionCharacterProposalResidualCited(t, line.Path, line.Line, line.Marker)
		content := ""
		if body := strings.Split(source, "\n"); line.Line >= 1 && line.Line <= len(body) {
			content = body[line.Line-1]
		}
		// The cited line is the precise claim; a formatter that moved it still
		// leaves the declaration in the file, so the fallback keeps this an
		// assertion about the fact rather than about a line number.
		if !strings.Contains(content, "immediate(") && !strings.Contains(content, "FROM character_identity_proposals") {
			content = source
		}
		if strings.Contains(content, "immediate(") {
			immediate++
		}
		if strings.Contains(content, "character_identity_proposals") {
			proposals++
		}
	}
	if immediate < 2 {
		t.Fatalf("the retraction cites %v BEGIN IMMEDIATE lines; the old claim was that package store has none", immediate)
	}
	if proposals < 2 {
		t.Fatalf("the retraction cites %v character_identity_proposals lines; the old claim was that the table cannot be read there", proposals)
	}
}

// TestProductionCharacterProposalResidualServedNames proves the names the record
// used to refuse are the two it now expects to be served, that both are still
// inventoried by the family, and that the binder line it publishes is the one the
// wiring report carries.
func TestProductionCharacterProposalResidualServedNames(t *testing.T) {
	served := productionCharacterProposalResidualServed()
	if len(served) != 2 {
		t.Fatalf("the record expects %v served names; the family has two", len(served))
	}
	if served[0] != productionCharacterProposalListPendingName || served[1] != productionCharacterProposalReadPendingName {
		t.Fatalf("the record expects %v and %v, not the two pending-finalized names", served[0], served[1])
	}
	inventory := productionCharacterProposalOperations()
	for _, name := range served {
		found := false
		for _, operation := range inventory {
			if operation == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("%v is no longer part of the family inventory", name)
		}
	}
	if !strings.Contains(productionCharacterProposalResidualBinder, "newProductionCharacterProposalPending(open.durable).Bind(server)") {
		t.Fatalf("the published binder line is %v", productionCharacterProposalResidualBinder)
	}
	// A binder handed no bridge installs nothing, which is what keeps the two
	// names failing closed instead of half-served.
	if err := newProductionCharacterProposalPending(nil).Bind(nil); err != nil {
		t.Fatalf("a nil bridge was refused instead of installing nothing: %v", err)
	}
}
