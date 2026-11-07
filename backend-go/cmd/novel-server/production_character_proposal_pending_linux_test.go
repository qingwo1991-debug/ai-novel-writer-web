//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// This file proves the record in production_character_proposal_pending_linux.go and
// production_character_proposal_residual_linux.go in the only two ways a comment
// cannot be wrong: every cited line is read back, and both names still fail closed
// over the real HTTP handler and a frozen session.
//
// # What each test is for
//
//	TestProductionCharacterProposalPendingProofSeams      the unported proof entry points, line by line
//	TestProductionCharacterProposalPendingRetraction      the retracted transaction claim, line by line
//	TestProductionCharacterProposalPendingNoListableWriter the writer fact that empties a served list
//	TestProductionCharacterProposalPendingFamilyAgrees    the family's own table and this record, one answer
//	TestProductionCharacterProposalPendingStillRefused    real HTTP: neither name is bound, nothing is written
//
// The file reader is deliberately not the sibling residuals' helper: a record about
// two names should fail on its own citation even if another family's helper changes
// its mind about what a marker means. It reads the cited line and nothing else, so a
// declaration that moves one line down fails here instead of passing on a file-wide
// search.

// productionCharacterProposalPendingCitedLine requires a cited marker to still be in
// the file it names, and reports the cited line when it has moved.
//
// The line number is a hint, not the assertion. Two things move it: the workflow
// formats this source in place before the tests run, and any edit above the
// declaration shifts every citation below it. A record that demanded the exact line
// would fail on a file that still says precisely what it claims, which is the same
// false alarm this package's seam markers already learned to avoid
// (production_seam_marker_test.go:20).
func productionCharacterProposalPendingCitedLine(t *testing.T, path string, line int, marker string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v: %v", path, err)
	}
	if productionSeamContains(string(raw), marker) {
		return
	}
	lines := strings.Split(string(raw), "\n")
	reported := "(past the end of the file)"
	if line >= 1 && line <= len(lines) {
		reported = strings.TrimSpace(lines[line-1])
	}
	t.Fatalf("%v no longer carries %v anywhere; line %v now reads %v", path, marker, line, reported)
}

// TestProductionCharacterProposalPendingProofSeams proves the surviving half of the
// refusal: the two names are the ones the family refuses, each cites at least the
// four unported proof entry points and the gate that refuses both kinds today, and
// only the list also cites the writer fact (a read is not emptied by a writer).
func TestProductionCharacterProposalPendingProofSeams(t *testing.T) {
	seams := productionCharacterProposalPendingProofSeams()
	if len(seams) != 2 {
		t.Fatalf("the record covers %v names; the family refuses two", len(seams))
	}
	if seams[0].Operation != productionCharacterProposalListPendingName ||
		seams[1].Operation != productionCharacterProposalReadPendingName {
		t.Fatalf("the record covers %v and %v, not the two pending-finalized names",
			seams[0].Operation, seams[1].Operation)
	}
	for position, seam := range seams {
		if len(seam.Missing) < 5 {
			t.Fatalf("%v cites only %v ports; the record carries the four proof entry points and the gate",
				seam.Operation, len(seam.Missing))
		}
		writer := false
		for _, port := range seam.Missing {
			productionCharacterProposalPendingCitedLine(t, port.Path, port.Line, port.Marker)
			if strings.Contains(port.Path, "dbmaintenance.go") {
				writer = true
			}
		}
		// The list is the one that needs a writer to exist; the read would answer a
		// row it can prove even if nothing here could write one.
		if position == 0 && !writer {
			t.Fatalf("%v cites no writer; a served list has nothing to page", seam.Operation)
		}
		if position == 1 && writer {
			t.Fatalf("%v cites a writer; the read's blocker is the proof alone", seam.Operation)
		}
	}
	// The four names the family serves must not be in this table: a seam is only
	// ever a promise that a name is refused.
	served := []string{
		productionCharacterProposalStageName,
		productionCharacterProposalReadName,
		productionCharacterProposalApproveName,
		productionCharacterProposalCancelName,
	}
	for _, seam := range seams {
		for _, name := range served {
			if name == seam.Operation {
				t.Fatalf("%v is served, so it cannot also carry a refusal", seam.Operation)
			}
		}
	}
	// The gate the record cites is still the architecture-only refusal: if it ever
	// admits a second source kind, this record has to be re-read rather than trusted.
	productionCharacterProposalPendingCitedLine(t, "production_character_proposals_linux.go", 1800,
		"return nil, productionCharacterProposalSourceProofUnavailable")
}

// TestProductionCharacterProposalPendingRetraction proves the claim this family used
// to refuse with is really retracted: the record's verdict no longer says a single
// transaction is impossible, and the store lines that do it are still there.
func TestProductionCharacterProposalPendingRetraction(t *testing.T) {
	verdict := productionCharacterProposalResidualVerdict
	for _, gone := range []string{
		"cannot be composed in one transaction",
		"no exported store call composes",
	} {
		if strings.Contains(verdict, gone) {
			t.Fatalf("the verdict still claims %v; the re-test refuted it", gone)
		}
	}
	for _, want := range []string{
		"internal/store/summary.go:681",
		"finalized-character-generation-proof.ts:13",
		"generation-character-proposal-proof.ts:16",
	} {
		if !strings.Contains(verdict, want) {
			t.Fatalf("the verdict does not name %v, so it rests on prose again", want)
		}
	}
	lines := productionCharacterProposalResidualRetraction()
	if len(lines) < 5 {
		t.Fatalf("the retraction cites %v lines; the record rests on five", len(lines))
	}
	immediate, proposals := 0, 0
	for _, line := range lines {
		productionCharacterProposalPendingCitedLine(t, line.Path, line.Line, line.Marker)
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%v: %v", line.Path, err)
		}
		content := strings.Split(string(raw), "\n")[line.Line-1]
		if strings.Contains(content, "immediate(") {
			immediate++
		}
		if strings.Contains(content, "character_identity_proposals") {
			proposals++
		}
	}
	// The retraction has to show both halves it was accused of: an immediate
	// transaction being opened, and the proposal table being read on its connection.
	// A citation set that lost either half would be a different claim.
	if immediate < 2 {
		t.Fatalf("the retraction cites %v BEGIN IMMEDIATE lines; the claim was that package store has none", immediate)
	}
	if proposals < 2 {
		t.Fatalf("the retraction cites %v character_identity_proposals lines; the claim was that the table cannot be read there", proposals)
	}
	// The handle itself is still unexported. That is why the composition belongs to a
	// store method, and it is no longer a reason to refuse: the record says so.
	productionCharacterProposalPendingCitedLine(t, "../../internal/store/reviewrevision.go", 41,
		"func (r *ReviewRevisions) immediate(")
}

// TestProductionCharacterProposalPendingNoListableWriter proves the writer fact the
// list's refusal now rests on: every INSERT into character_identity_proposals is
// accounted for, none of them writes a finalized-generation proposal, and the only
// import writer writes the null provenance the reference's list skips
// (character-proposal-service.ts:136-138).
func TestProductionCharacterProposalPendingNoListableWriter(t *testing.T) {
	writers := productionCharacterProposalPendingEnvelopeWriters()
	if len(writers) != 4 {
		t.Fatalf("this module writes character_identity_proposals in %v places; the record knows four, so a fifth needs a re-read", len(writers))
	}
	imports := 0
	for _, writer := range writers {
		productionCharacterProposalPendingCitedLine(t, writer.Path, writer.Line, writer.Marker)
		if writer.Kind == productionCharacterProposalFinalizedKind {
			t.Fatalf("%v writes a %v envelope now, so a served list could page something: %v:%v",
				writer.Path, writer.Kind, writer.Path, writer.Line)
		}
		if writer.Kind == "import" {
			imports++
			if writer.Provenance != "nil" {
				t.Fatalf("%v writes an import envelope with provenance %v, so the list's guard would keep it: %v:%v",
					writer.Path, writer.Provenance, writer.Path, writer.Line)
			}
			// The null provenance is a stored fact of this writer's own function, not
			// this table's opinion: it is cited, it sits above the INSERT, and no
			// other function starts between the two lines.
			productionCharacterProposalPendingCitedLine(t, writer.Path, writer.ProvenanceLine, `"provenance": nil`)
			raw, err := os.ReadFile(writer.Path)
			if err != nil {
				t.Fatalf("%v: %v", writer.Path, err)
			}
			lines := strings.Split(string(raw), "\n")
			if writer.ProvenanceLine < 1 || writer.ProvenanceLine >= writer.Line || writer.Line > len(lines) {
				t.Fatalf("%v cites its provenance at %v and its writer at %v, so the two cannot be one function",
					writer.Path, writer.ProvenanceLine, writer.Line)
			}
			for index := writer.ProvenanceLine; index < writer.Line; index++ {
				if strings.HasPrefix(lines[index], "func ") {
					t.Fatalf("%v:%v belongs to another function, not the writer at %v",
						writer.Path, writer.ProvenanceLine, writer.Line)
				}
			}
		}
	}
	if imports != 1 {
		t.Fatalf("this module has %v import writers; the record knows one", imports)
	}
	// The architecture arm this lane does serve is still the one writer with a
	// generated provenance, which is what makes the four served names work.
	generated := 0
	for _, writer := range writers {
		if writer.Provenance == "generated" {
			generated++
		}
	}
	if generated != 1 {
		t.Fatalf("%v writers claim a generated provenance; the architecture arm is the only one", generated)
	}
}

// TestProductionCharacterProposalPendingFamilyAgrees proves the two records cannot
// drift: this file's table and the family's own pending seam table cover the same two
// names in the same order, so neither can describe a refusal the other does not.
func TestProductionCharacterProposalPendingFamilyAgrees(t *testing.T) {
	family := productionCharacterProposalPendingSeams()
	mine := productionCharacterProposalPendingProofSeams()
	if len(family) != len(mine) {
		t.Fatalf("the family refuses %v names and this record covers %v", len(family), len(mine))
	}
	for position := range family {
		if family[position].Operation != mine[position].Operation {
			t.Fatalf("the family refuses %v where this record covers %v",
				family[position].Operation, mine[position].Operation)
		}
	}
	inventory := productionCharacterProposalOperations()
	if len(inventory) != 6 {
		t.Fatalf("the family declares %v names, want the six of web/operations.json:56-61", len(inventory))
	}
	if inventory[2] != productionCharacterProposalListPendingName ||
		inventory[3] != productionCharacterProposalReadPendingName {
		t.Fatalf("the family order is %v, want operations.json's own", inventory)
	}
}

// TestProductionCharacterProposalPendingStillRefused is the binding half: over the
// real HTTP handler and a frozen session, both names answer the fail-closed
// GO_OPERATION_NOT_MIGRATED - with a live lease and with a lease the server cannot
// prove - and the admitted database file is byte for byte unchanged.
//
// The list's wire arity is zero (src/shared/character-proposal.ts:77) and the read's
// is one (:78), so the list is also called with the frozen context alone. An
// unbound name must not care: the transport refuses before any arity, body or lease
// proof runs, which is exactly what "installs nothing" buys.
func TestProductionCharacterProposalPendingStillRefused(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	before := lane.bytes(t)
	for _, operation := range []string{
		productionCharacterProposalListPendingName,
		productionCharacterProposalReadPendingName,
	} {
		for label, args := range map[string][]any{
			"with a live lease":  []any{map[string]any{}, lane.frozen},
			"with a stale lease": []any{map[string]any{}, lane.stale()},
		} {
			response := lane.author.call(t, operation, args)
			if response.Code != 501 {
				t.Fatalf("%v %v answered %v instead of staying unbound: %s",
					operation, label, response.Code, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
				t.Fatalf("%v %v did not refuse with the fail-closed code: %s",
					operation, label, response.Body.String())
			}
		}
		// The list takes no request at all, so the frozen context is its only
		// argument on the wire.
		response := lane.author.call(t, operation, []any{lane.frozen})
		if response.Code != 501 || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%v answered %v for a context-only call instead of staying unbound: %s",
				operation, response.Code, response.Body.String())
		}
	}
	// The lane is live and this session reaches it: the family's own read answers a
	// symbolic refusal for an id no project staged, so the 501 above is about binding
	// and not about a dead fixture.
	control := lane.author.call(t, productionCharacterProposalReadName,
		[]any{map[string]any{productionCharacterProposalBatchKey: "cpb:" + strings.Repeat("0", 64)}, lane.frozen})
	if control.Code == 501 {
		t.Fatalf("the family's read is unbound, so this lane proves nothing: %s", control.Body.String())
	}
	var refusal struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(control.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("the control answer is not the wire refusal: %v %s", err, control.Body.String())
	}
	if refusal.Error.Code != "CHARACTER_PROPOSAL_RECEIPT_INVALID" {
		t.Fatalf("the control answered %v, want the receipt refusal of an absent batch", refusal.Error.Code)
	}
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a call to an unbound name or the control read changed the admitted database")
	}
}
