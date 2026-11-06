//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the one name production_finalized_character_commit_linux.go
// binds, `finalized-character:commit`, one subtest per aspect:
//
//	the applied field        one derived field is written, its revision advances and
//	                         the roster projection is refreshed
//	the staged candidate     an author-protected field is proposed, not overwritten,
//	                         and the receipt names the staged candidate
//	the fenced answer        a ```json body parses, which only the fence strip can
//	                         make true
//	the parser's refusal     a refused answer names the parser's code and writes
//	                         nothing at all
//	the unresolved identity  an unknown id is kept unresolved, not refused, and
//	                         writes nothing at all
//	the superseded source    a store refusal on the served path, byte-stable
//	the missing lease        a write that refuses symbolically instead of writing
//	the unreadable body      the commit's own allowlist, and the read's draft guard
//
// # The fixture
//
// Every subtest drives the real HTTP handler over a disposable admitted project
// (productionGenerationWriteOpen), because that is where the binding, the frozen
// three-key context, the live lease and the wire answer live. The rows are seeded
// through the helpers the neighbouring finalized-character and generation-write
// fixtures already own: the finalized draft and its outbox receipt
// (productionGenerationWriteSeedOutbox), the live character row with its
// field-level provenance (productionFinalizedCharacterInsertCharacter plus
// productionCharacterStateCandidateProvenance) and the freeze receipt that makes
// the context `bound` (productionFinalizedCharacterInsertFreeze). This file adds
// only what no sibling seeds for this operation: the per-draft candidate store in
// summary_snapshots (internal/admission/baseline_v7_ddl.go:630-643) and the
// character_roster_meta singleton the projection refresh bumps
// (baseline_v7_ddl.go:142-159), which the pinned baseline does not create - the
// store's own commit test seeds it for the same reason
// (internal/store/finalized_character_commit_test.go:137-138).
//
// continuity_projection_meta is written with INSERT ... ON CONFLICT(id) DO UPDATE
// and read back: the pinned baseline creates the table empty, so a bare UPDATE is
// a silent no-op and OR IGNORE alone keeps a stale generation - in either case the
// source order the commit derives would disagree with the seeded provenance and
// every applied field would refuse as FINALIZED_CHARACTER_SOURCE_CONFLICT for a
// fixture that is in fact perfect.
//
// # What every subtest proves
//
// An applied write is read back over a connection of the test's own
// (productionStoryStateWriteReadback, mode=ro), never from the receipt and never
// through the lane's admitted handle, so what is asserted is the persisted author
// database: the field's value, its provenance revision and source order, and the
// roster singleton's revision and fact hash. A staged proposal is read back the
// same way, and then through the family's own candidate read
// (store.FinalizedCharacters.ReadPendingStateCandidate) - the receipt has to name a
// candidate the sibling operations can actually act on.
//
// A refused commit must leave the file byte for byte alone, and so must every
// refusal in the guard subtests. No subtest mints a candidate key or a context id
// the server produced, and nothing here weakens a refusal.
const (
	// productionFinalizedCharacterCommitPowerField is the derived field the applied
	// subtests write. The seeded provenance carries it at revision 2 with the frozen
	// source and the live source order, which is exactly what decideDerivedPatch
	// needs to answer apply-derived
	// (internal/store/finalized_character_commit.go:289-308).
	productionFinalizedCharacterCommitPowerField = "powerLevel"
	// productionFinalizedCharacterCommitAppliedValue is a level the live row does
	// not have, so an echoed request can never pass for a write.
	productionFinalizedCharacterCommitAppliedValue = "金丹"
	// productionFinalizedCharacterCommitStagedValue is the location a model
	// proposes for the author's own field: the receipt must stage it and the live
	// row must keep productionFinalizedCharacterLocation.
	productionFinalizedCharacterCommitStagedValue = "旧塔废墟"
	// productionFinalizedCharacterCommitEvidenceText is a substring of the seeded
	// chapter (productionEditorSeed) that occurs exactly once, so the parser accepts
	// it as unique evidence without the answer naming offsets.
	productionFinalizedCharacterCommitEvidenceText = "推开门"
	// productionFinalizedCharacterCommitRosterRevision is the roster singleton's
	// revision before a commit. Seeded explicitly rather than read from a default,
	// so "+1" is a fact about this write and not about the baseline.
	productionFinalizedCharacterCommitRosterRevision = 4
	// productionFinalizedCharacterCommitSecondFinalizationID is the identity of the
	// newer finalized draft that supersedes draft 1 in the source-changed subtest.
	productionFinalizedCharacterCommitSecondFinalizationID = "pfc-commit-finalization-2"
)

// TestProductionFinalizedCharacterCommitServesTheRosterWrite is the whole of this
// lane's claim about the channel.
func TestProductionFinalizedCharacterCommitServesTheRosterWrite(t *testing.T) {
	t.Run("finalized-character:commit applies one field", testProductionFinalizedCharacterCommitApplied)
	t.Run("stages an author-protected field", testProductionFinalizedCharacterCommitStaged)
	t.Run("parses a fenced json answer", testProductionFinalizedCharacterCommitFenced)
	t.Run("refuses an answer the parser refuses", testProductionFinalizedCharacterCommitParseRefusal)
	t.Run("keeps an unknown id unresolved without writing", testProductionFinalizedCharacterCommitUnresolved)
	t.Run("refuses a superseded frozen source", testProductionFinalizedCharacterCommitSourceRefusal)
	t.Run("refuses without a live project lease", testProductionFinalizedCharacterCommitLease)
	t.Run("refuses a body it cannot read", testProductionFinalizedCharacterCommitBodyRefusal)
}

// testProductionFinalizedCharacterCommitApplied is the applied arm
// (finalized_character_commit.go:356-380): one derived field is written, its
// provenance revision advances, and the roster projection is refreshed in the same
// transaction (:407-412).
func testProductionFinalizedCharacterCommitApplied(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterCommitSeed)
	source := productionCharacterStateCandidateSource()
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var rosterRevision int64
	var rosterHashBefore string
	productionStoryStateWriteScan(t, readback, "SELECT revision,fact_hash FROM character_roster_meta WHERE id='main'",
		nil, &rosterRevision, &rosterHashBefore)
	if rosterRevision != productionFinalizedCharacterCommitRosterRevision {
		t.Fatalf("the seeded roster revision is %v, want %v", rosterRevision,
			productionFinalizedCharacterCommitRosterRevision)
	}
	answer := productionFinalizedCharacterCommitAnswer(t, productionFinalizedCharacterCommitPowerField,
		productionFinalizedCharacterCommitAppliedValue)
	receipt := productionFinalizedCharacterCommitServe(t, lane, productionFinalizedCharacterCommitBody(1, answer))
	// :230 the receipt's three counts, kept separate: one field applied, nothing
	// staged and nothing unchanged, and an empty list rather than a null one.
	if receipt.Applied != 1 || receipt.Unchanged != 0 {
		t.Fatalf("receipt = %+v, want applied 1 and unchanged 0", receipt)
	}
	if receipt.Candidates == nil || len(receipt.Candidates) != 0 {
		t.Fatalf("receipt candidates = %v, want the empty list the store initialises", receipt.Candidates)
	}
	// The answer always carries the parse's occurrences; a resolved answer has none,
	// as a list rather than a null (finalized-character-generation.ts:79-81).
	if receipt.Unresolved == nil || len(receipt.Unresolved) != 0 {
		t.Fatalf("receipt unresolved = %v, want the empty list", receipt.Unresolved)
	}
	power, provenance := productionFinalizedCharacterCommitProvenance(t, readback)
	if power != productionFinalizedCharacterCommitAppliedValue {
		t.Fatalf("cs_power_level = %v, want the answer's %v", power,
			productionFinalizedCharacterCommitAppliedValue)
	}
	// The write is the store's own: the derived field's revision is base+1, its
	// source is the frozen identity and its order is the one the live watermark
	// derives (finalized_character_context.go:290-300, character-identity.ts:33-35).
	stored := productionFinalizedCharacterCommitProvenanceFields(t, provenance)
	location := productionFinalizedCharacterObject(t, "provenance.location", stored["location"])
	if location["kind"] != "author" || location["chapterNumber"] != float64(1) {
		t.Fatalf("the author's location provenance = %v, want it untouched", location)
	}
	derived := productionFinalizedCharacterObject(t, "provenance.powerLevel", stored["powerLevel"])
	if derived["kind"] != "derived" || derived["revision"] != float64(3) {
		t.Fatalf("the applied field's provenance = %v, want a derived field at revision 3", derived)
	}
	frozen := productionFinalizedCharacterObject(t, "provenance.powerLevel.source", derived["source"])
	expectedSource := productionFinalizedCharacterCommitJSON(t, productionCharacterStateCandidateSourceObject(source))
	for key, want := range expectedSource {
		if frozen[key] != want {
			t.Fatalf("the applied field's source.%v = %v, want %v", key, frozen[key], want)
		}
	}
	order := productionFinalizedCharacterObject(t, "provenance.powerLevel.sourceOrder", derived["sourceOrder"])
	for key, want := range map[string]any{
		"continuityEpoch":                   productionFinalizedCharacterContinuityEpoch(productionEditorProjectID),
		"chapterNumber":                     float64(1),
		"authoritativeFinalizationRevision": float64(1),
	} {
		if order[key] != want {
			t.Fatalf("the applied field's sourceOrder.%v = %v, want %v", key, order[key], want)
		}
	}
	// The roster projection was refreshed: the singleton advanced by exactly one,
	// its fact hash was recomputed and it is a digest, and the author's own
	// compatibility prose is untouched (character_state_candidates.go:630-644).
	var rosterAfter int64
	var rosterHashAfter string
	productionStoryStateWriteScan(t, readback, "SELECT revision,fact_hash FROM character_roster_meta WHERE id='main'",
		nil, &rosterAfter, &rosterHashAfter)
	if rosterAfter != rosterRevision+1 {
		t.Fatalf("the roster revision is %v, want %v", rosterAfter, rosterRevision+1)
	}
	if !productionFinalizedCharacterCommitDigest(rosterHashAfter) {
		t.Fatalf("the roster fact hash %v is not a digest", rosterHashAfter)
	}
	if rosterHashAfter == rosterHashBefore {
		t.Fatalf("the roster fact hash did not move from %v", rosterHashBefore)
	}
}

// testProductionFinalizedCharacterCommitStaged is the proposal-required arm
// (finalized_character_commit.go:314-352): the author's own field is never
// overwritten, and the receipt names the candidate that was staged for it.
func testProductionFinalizedCharacterCommitStaged(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterCommitSeed)
	source := productionCharacterStateCandidateSource()
	key := productionCharacterStateCandidateKey(source, productionCharacterStateCandidateField,
		productionFinalizedCharacterCommitStagedValue)
	answer := productionFinalizedCharacterCommitAnswer(t, productionCharacterStateCandidateField,
		productionFinalizedCharacterCommitStagedValue)
	receipt := productionFinalizedCharacterCommitServe(t, lane, productionFinalizedCharacterCommitBody(1, answer))
	if receipt.Applied != 0 || receipt.Unchanged != 0 || len(receipt.Candidates) != 1 {
		t.Fatalf("receipt = %+v, want one staged candidate and nothing applied", receipt)
	}
	candidate := receipt.Candidates[0]
	for label, want := range map[string]any{
		"characterId":            productionFinalizedCharacterCharacterID,
		"characterName":          productionFinalizedCharacterFrozenName,
		"field":                  productionCharacterStateCandidateField,
		"value":                  productionFinalizedCharacterCommitStagedValue,
		"candidateKey":           key,
		"reason":                 productionCharacterStateCandidateReason,
		"expectedFieldRevision":  float64(0),
		"expectedFieldValueHash": sha256Hex(productionFinalizedCharacterLocation),
	} {
		if candidate[label] != want {
			t.Fatalf("the staged candidate's %v = %v, want %v", label, candidate[label], want)
		}
	}
	candidateSource := productionFinalizedCharacterObject(t, "candidate.source", candidate["source"])
	expectedSource := productionFinalizedCharacterCommitJSON(t, productionCharacterStateCandidateSourceObject(source))
	for label, want := range expectedSource {
		if candidateSource[label] != want {
			t.Fatalf("the staged candidate's source.%v = %v, want %v", label, candidateSource[label], want)
		}
	}
	// The author's value and provenance are exactly what the fixture wrote.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var location string
	productionStoryStateWriteScan(t, readback,
		"SELECT cs_location FROM characters WHERE character_id=?", []any{productionFinalizedCharacterCharacterID},
		&location)
	if location != productionFinalizedCharacterLocation {
		t.Fatalf("cs_location = %v, want the author's untouched %v", location,
			productionFinalizedCharacterLocation)
	}
	// The proposal is in the draft's own store, and nothing was decided.
	staged := productionCharacterStateCandidateStored(t, readback, source.DraftID)
	if len(staged.Pending) != 1 || len(staged.Receipts) != 0 {
		t.Fatalf("the store holds %v pending and %v receipts, want one pending and none",
			len(staged.Pending), len(staged.Receipts))
	}
	if staged.Pending[0]["candidateKey"] != key {
		t.Fatalf("the pending candidate = %v, want the receipt's %v", staged.Pending[0], key)
	}
	// And the family's own read finds it, which is what makes the receipt name a
	// candidate the read-state-candidate and decide-state-candidate names can act on.
	served, err := store.NewFinalizedCharacters(readback).ReadPendingStateCandidate(context.Background(),
		source.DraftID, key)
	if err != nil {
		t.Fatalf("mode=ro candidate read-back: %v", err)
	}
	if served == nil || served.CandidateKey != key || served.Value != productionFinalizedCharacterCommitStagedValue {
		t.Fatalf("the staged candidate reads back as %+v, want %v", served, key)
	}
}

// testProductionFinalizedCharacterCommitFenced proves the parser is called on the
// model's real answer: a ```json fence only parses because the parser strips it
// (state_response.go:316, and stripLeadingFence/stripTrailingFence at :662-689), so
// an applied receipt here cannot come from a body whose fence this adapter removed.
func testProductionFinalizedCharacterCommitFenced(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterCommitSeed)
	answer := "```json\n" + productionFinalizedCharacterCommitAnswer(t,
		productionFinalizedCharacterCommitPowerField, productionFinalizedCharacterCommitAppliedValue) + "\n```"
	receipt := productionFinalizedCharacterCommitServe(t, lane, productionFinalizedCharacterCommitBody(1, answer))
	if receipt.Applied != 1 || len(receipt.Candidates) != 0 {
		t.Fatalf("receipt = %+v, want the fenced answer applied once", receipt)
	}
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	power, _ := productionFinalizedCharacterCommitProvenance(t, readback)
	if power != productionFinalizedCharacterCommitAppliedValue {
		t.Fatalf("cs_power_level = %v, want the fenced answer's %v", power,
			productionFinalizedCharacterCommitAppliedValue)
	}
}

// testProductionFinalizedCharacterCommitParseRefusal proves the parser's layer: its
// own code reaches the browser, unchanged, and the database keeps every byte.
func testProductionFinalizedCharacterCommitParseRefusal(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterCommitSeed)
	before := lane.bytes(t)
	// A body JSON.parse refuses (state_response.go:311-318) - this lane's port-only
	// body code.
	productionFinalizedCharacterRefusal(t, "an answer that is not JSON",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{productionFinalizedCharacterCommitBody(1, "不是 JSON"), lane.frozen}),
		"FINALIZED_CHARACTER_RESPONSE_BODY_INVALID")
	// A currentState with no field of the closed list (state_response.go:206-208).
	productionFinalizedCharacterRefusal(t, "an answer with no state field",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{productionFinalizedCharacterCommitBody(1,
				`{"updates":[{"characterId":"`+productionFinalizedCharacterCharacterID+
					`","currentState":{},"evidence":{"text":"`+
					productionFinalizedCharacterCommitEvidenceText+`"}}]}`), lane.frozen}),
		"FINALIZED_CHARACTER_FIELD_INVALID")
	// Evidence that occurs nowhere in the frozen prose (state_response.go:191).
	productionFinalizedCharacterRefusal(t, "an answer whose evidence is not in the chapter",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{productionFinalizedCharacterCommitBody(1,
				`{"updates":[{"characterId":"`+productionFinalizedCharacterCharacterID+
					`","currentState":{"`+productionFinalizedCharacterCommitPowerField+
					`":"`+productionFinalizedCharacterCommitAppliedValue+
					`"},"evidence":{"text":"这段原稿里没有这句话"}}]}`), lane.frozen}),
		"FINALIZED_CHARACTER_EVIDENCE_NOT_UNIQUE")
	// An occurrence with neither an id nor a name. This is the only shape that
	// throws at the parse layer: finalized-continuity.ts:216 refuses when
	// (!originalText.trim() && !characterId). An id the context does not hold is
	// NOT this arm - it is retained in `unresolved` and never becomes an update
	// (:215-223), which the next subtest asserts.
	productionFinalizedCharacterRefusal(t, "an occurrence with no id and no name",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{productionFinalizedCharacterCommitBody(1,
				`{"updates":[{"currentState":{"`+productionFinalizedCharacterCommitPowerField+
					`":"`+productionFinalizedCharacterCommitAppliedValue+
					`"},"evidence":{"text":"`+productionFinalizedCharacterCommitEvidenceText+
					`"}}]}`), lane.frozen}),
		"FINALIZED_CHARACTER_IDENTITY_REQUIRED")
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused answer changed the admitted database")
	}
}

// testProductionFinalizedCharacterCommitUnresolved is the reference's actual
// contract for an answer that names a character no frozen identity answers:
//
//	finalized-continuity.ts:215-223
//	  if (!character || sharedLabel) {
//	    if (!originalText.trim() && !characterId) throw new Error('FINALIZED_CHARACTER_IDENTITY_REQUIRED')
//	    result.unresolved.push({ selectionKey: `update:${index}`, displayName: ..., originalText,
//	      candidateIds: ..., currentState, evidence: exactEvidence, rawValue: structuredClone(input),
//	      reason: ... characterId ? 'unknown-id' : 'name-only' })
//	    continue
//	  }
//
// So the parse does not throw, the occurrence never joins `updates`, and the store -
// which re-parses exactly `updates` (summary-repository.ts:435,
// finalized_character_commit.go:795-808) - sees an empty answer, writes nothing and
// answers the empty receipt (:437). The occurrence comes back beside it
// (finalized-character-generation.ts:79-81), which is what tells the author that the
// model named a character the freeze does not hold. A 200 here is only honest if the
// database is byte-identical: an unresolved occurrence was never a write.
//
// Do not "fix" this into a refusal: the refusals that do exist are the parse-layer
// one for an occurrence with no id and no name (:216, asserted above) and the
// store's own re-parse guard (:436), which a parsed answer can never trigger.
func testProductionFinalizedCharacterCommitUnresolved(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterCommitSeed)
	before := lane.bytes(t)
	answer := `{"updates":[{"characterId":"not-a-frozen-character","currentState":{"` +
		productionFinalizedCharacterCommitPowerField + `":"` + productionFinalizedCharacterCommitAppliedValue +
		`"},"evidence":{"text":"` + productionFinalizedCharacterCommitEvidenceText + `"}}]}`
	receipt := productionFinalizedCharacterCommitServe(t, lane, productionFinalizedCharacterCommitBody(1, answer))
	if receipt.Applied != 0 || receipt.Unchanged != 0 || len(receipt.Candidates) != 0 {
		t.Fatalf("receipt = %+v, want an empty commit", receipt)
	}
	if len(receipt.Unresolved) != 1 {
		t.Fatalf("receipt unresolved = %v, want the one occurrence the parse retained", receipt.Unresolved)
	}
	occurrence := receipt.Unresolved[0]
	for label, want := range map[string]any{
		"selectionKey": "update:0",
		"reason":       "unknown-id",
		"displayName":  "",
		"originalText": "",
	} {
		if occurrence[label] != want {
			t.Fatalf("the retained occurrence's %v = %v, want %v", label, occurrence[label], want)
		}
	}
	// candidateIds is a list, empty because nothing in the frozen context matches
	// the id, the empty name or an alias (:219-221).
	candidates, ok := occurrence["candidateIds"].([]any)
	if !ok || len(candidates) != 0 {
		t.Fatalf("the retained occurrence's candidateIds = %v, want the empty list", occurrence["candidateIds"])
	}
	state := productionFinalizedCharacterObject(t, "occurrence.currentState", occurrence["currentState"])
	if state[productionFinalizedCharacterCommitPowerField] != productionFinalizedCharacterCommitAppliedValue {
		t.Fatalf("the retained occurrence's currentState = %v, want the model's own", state)
	}
	evidence := productionFinalizedCharacterObject(t, "occurrence.evidence", occurrence["evidence"])
	if evidence["text"] != productionFinalizedCharacterCommitEvidenceText {
		t.Fatalf("the retained occurrence's evidence = %v, want the model's own quote", evidence)
	}
	// rawValue is structuredClone(input) (:221): the update exactly as it was written.
	raw := productionFinalizedCharacterObject(t, "occurrence.rawValue", occurrence["rawValue"])
	if raw["characterId"] != "not-a-frozen-character" {
		t.Fatalf("the retained occurrence's rawValue = %v, want the update as written", raw)
	}
	// Nothing was written, and nothing was staged for the author either.
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("an unresolved occurrence changed the admitted database")
	}
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	power, _ := productionFinalizedCharacterCommitProvenance(t, readback)
	if power != productionFinalizedCharacterPowerLevel {
		t.Fatalf("cs_power_level = %v, want the untouched %v", power, productionFinalizedCharacterPowerLevel)
	}
	staged := productionCharacterStateCandidateStored(t, readback, 1)
	if len(staged.Pending) != 0 || len(staged.Receipts) != 0 {
		t.Fatalf("the store holds %v pending and %v receipts, want none", len(staged.Pending), len(staged.Receipts))
	}
}

// testProductionFinalizedCharacterCommitSourceRefusal is the store's layer: a newer
// finalized draft of the same chapter supersedes the frozen one, which the read this
// commit performs first refuses with FINALIZED_CHARACTER_SOURCE_CHANGED
// (internal/store/finalized_character_context.go:29-33) before a single field is
// written.
func testProductionFinalizedCharacterCommitSourceRefusal(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterCommitSupersededSeed)
	before := lane.bytes(t)
	answer := productionFinalizedCharacterCommitAnswer(t, productionFinalizedCharacterCommitPowerField,
		productionFinalizedCharacterCommitAppliedValue)
	productionFinalizedCharacterRefusal(t, "a superseded frozen source",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{productionFinalizedCharacterCommitBody(1, answer), lane.frozen}),
		"FINALIZED_CHARACTER_SOURCE_CHANGED")
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("the refused commit changed the admitted database")
	}
	// Nothing moved: the live field still carries the fixture's value.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	power, _ := productionFinalizedCharacterCommitProvenance(t, readback)
	if power != productionFinalizedCharacterPowerLevel {
		t.Fatalf("cs_power_level = %v, want the untouched %v", power, productionFinalizedCharacterPowerLevel)
	}
}

// testProductionFinalizedCharacterCommitLease proves the write refuses
// symbolically when the lane is closed to this caller, and that the fixture itself
// was not what refused.
func testProductionFinalizedCharacterCommitLease(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterCommitSeed)
	before := lane.bytes(t)
	answer := productionFinalizedCharacterCommitAnswer(t, productionFinalizedCharacterCommitPowerField,
		productionFinalizedCharacterCommitAppliedValue)
	body := productionFinalizedCharacterCommitBody(1, answer)
	// A lease this session never held, and a context with no lease at all: both are
	// refused by the admission (production_durable_linux.go:313-364) before any row
	// is read or written, and neither may answer 200.
	productionEditorRefused(t, lane.author, "stale lease", productionFinalizedCharacterCommitName,
		[]any{body, lane.stale()})
	productionEditorRefused(t, lane.author, "no lease", productionFinalizedCharacterCommitName,
		[]any{body, map[string]any{"projectId": lane.frozen["projectId"], "projectPath": lane.projectPath}})
	// A request arity that is not the reference's is the transport's own refusal,
	// and a frozen context that is not the three keys is the session's.
	productionFinalizedCharacterRefusal(t, "no frozen context",
		lane.author.call(t, productionFinalizedCharacterCommitName, []any{}), "INVALID_ARGUMENTS")
	productionFinalizedCharacterRefusal(t, "a context that is not the three keys",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{body, map[string]any{"projectId": lane.frozen["projectId"]}}), "PROJECT_SESSION_REQUIRED")
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused commit changed the admitted database")
	}
	// The same fixture under its live lease still writes, so every refusal above is
	// the lease's and not a fixture that could never commit.
	receipt := productionFinalizedCharacterCommitServe(t, lane, body)
	if receipt.Applied != 1 {
		t.Fatalf("the live lease applied %v fields, want 1", receipt.Applied)
	}
}

// testProductionFinalizedCharacterCommitBodyRefusal proves the commit's own
// allowlist (finalized-character-generation.ts:66-67) and the draft guard it reuses
// from the read, and that a blank answer is the parser's refusal and not the
// guard's.
func testProductionFinalizedCharacterCommitBodyRefusal(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionFinalizedCharacterCommitSeed)
	before := lane.bytes(t)
	answer := productionFinalizedCharacterCommitAnswer(t, productionFinalizedCharacterCommitPowerField,
		productionFinalizedCharacterCommitAppliedValue)
	// A key the reference's commit refuses - including the contextId this lane
	// deliberately does not accept from a browser.
	productionFinalizedCharacterRefusal(t, "a body with an unknown key",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{map[string]any{"draftId": float64(1), "text": answer, "contextId": "browser-chosen"},
				lane.frozen}), productionFinalizedCharacterCommitInvalid)
	// A body that is not an object at all.
	productionFinalizedCharacterRefusal(t, "a body that is not an object",
		lane.author.call(t, productionFinalizedCharacterCommitName, []any{answer, lane.frozen}),
		productionFinalizedCharacterCommitInvalid)
	// An answer that is not text.
	productionFinalizedCharacterRefusal(t, "a body whose answer is not text",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{map[string]any{"draftId": float64(1), "text": float64(1)}, lane.frozen}),
		productionFinalizedCharacterCommitInvalid)
	// The draft guard the read owns (finalized-character-generation.ts:22).
	productionFinalizedCharacterRefusal(t, "a body with no draft id",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{map[string]any{"text": answer}, lane.frozen}), productionFinalizedCharacterDraftInvalid)
	productionFinalizedCharacterRefusal(t, "a body with a draft id below one",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{map[string]any{"draftId": float64(0), "text": answer}, lane.frozen}),
		productionFinalizedCharacterDraftInvalid)
	// A blank answer is not a shape refusal: the parser owns it (state_response.go:311-318),
	// exactly as the reference's JSON.parse does.
	productionFinalizedCharacterRefusal(t, "a blank answer",
		lane.author.call(t, productionFinalizedCharacterCommitName,
			[]any{productionFinalizedCharacterCommitBody(1, ""), lane.frozen}),
		"FINALIZED_CHARACTER_RESPONSE_BODY_INVALID")
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused body changed the admitted database")
	}
}

// productionFinalizedCharacterCommitServed is the answer on the wire: the store's
// three counts in the reference's key order (src/shared/finalized-continuity.ts:
// 163-167) and the parse's occurrences beside them
// (src/shared/finalized-character-generation.ts:16-19, :79-81). Every list is
// decoded as a slice so a null is visible as nil instead of an empty list.
type productionFinalizedCharacterCommitServed struct {
	Applied    int64            `json:"applied"`
	Unchanged  int64            `json:"unchanged"`
	Candidates []map[string]any `json:"candidates"`
	Unresolved []map[string]any `json:"unresolved"`
}

// productionFinalizedCharacterCommitBody is the served body: exactly the two keys
// the binder's allowlist admits.
func productionFinalizedCharacterCommitBody(draftID float64, answer string) map[string]any {
	return map[string]any{
		productionFinalizedCharacterCommitDraftKey: draftID,
		productionFinalizedCharacterCommitTextKey:  answer,
	}
}

// productionFinalizedCharacterCommitAnswer is one model answer: a single update for
// the fixture character, one state field and a unique verbatim quote from the frozen
// chapter, which is the shape the parser turns into verified offsets.
func productionFinalizedCharacterCommitAnswer(t *testing.T, field, value string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"updates": []any{map[string]any{
			"characterId":  productionFinalizedCharacterCharacterID,
			"currentState": map[string]any{field: value},
			"evidence":     map[string]any{"text": productionFinalizedCharacterCommitEvidenceText},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// productionFinalizedCharacterCommitServe performs one commit under the lane's live
// lease and decodes the receipt.
func productionFinalizedCharacterCommitServe(t *testing.T, lane *productionGenerationWriteLane,
	body map[string]any) productionFinalizedCharacterCommitServed {
	t.Helper()
	var receipt productionFinalizedCharacterCommitServed
	lane.author.decode(t, productionFinalizedCharacterCommitName, []any{body, lane.frozen}, &receipt)
	return receipt
}

// productionFinalizedCharacterCommitJSON decodes one fixture value through JSON, so a
// comparison against a served receipt compares JSON values to JSON values - an int64
// and a float64 are never equal in Go even when they are one number on the wire.
func productionFinalizedCharacterCommitJSON(t *testing.T, value any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode fixture %v: %v", string(encoded), err)
	}
	return decoded
}

// productionFinalizedCharacterCommitProvenance reads the live character's level and
// its field-level provenance through a connection of the test's own.
func productionFinalizedCharacterCommitProvenance(t *testing.T, db *sql.DB) (string, string) {
	t.Helper()
	var power, provenance string
	productionStoryStateWriteScan(t, db,
		"SELECT cs_power_level,cs_provenance FROM characters WHERE character_id=?",
		[]any{productionFinalizedCharacterCharacterID}, &power, &provenance)
	return power, provenance
}

// productionFinalizedCharacterCommitProvenanceFields decodes one stored provenance
// object, so a write is asserted member by member instead of byte for byte: the
// reference's serialization order is the store's own contract and this test asserts
// what a reader sees, not how it was spelled.
func productionFinalizedCharacterCommitProvenanceFields(t *testing.T, provenance string) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal([]byte(provenance), &fields); err != nil {
		t.Fatalf("stored provenance %v: %v", provenance, err)
	}
	return fields
}

// productionFinalizedCharacterCommitDigest is summaryHashValid's read-side check
// (internal/store/summary.go:36-48): lowercase hex, 64 characters.
func productionFinalizedCharacterCommitDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= 'a' && character <= 'f' || character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

// productionFinalizedCharacterCommitSeed writes the commit fixture's rows into an
// admitted project, before the lane admits it. The finalized draft and its outbox
// receipt are the neighbouring generation-write fixture's rows; the character, its
// provenance and the freeze receipt are the read fixture's helpers.
func productionFinalizedCharacterCommitSeed(t *testing.T, projectPath string) {
	t.Helper()
	productionFinalizedCharacterCommitSeedWith(t, projectPath, false)
}

// productionFinalizedCharacterCommitSupersededSeed is the same fixture plus a newer
// finalized draft of chapter 1, which is what makes the frozen source stale.
func productionFinalizedCharacterCommitSupersededSeed(t *testing.T, projectPath string) {
	t.Helper()
	productionFinalizedCharacterCommitSeedWith(t, projectPath, true)
}

// productionFinalizedCharacterCommitSeedWith writes both fixtures. superseded adds
// one more finalized draft of the frozen chapter; nothing else differs, so a
// refusal in the source-changed subtest can only be that row's doing.
func productionFinalizedCharacterCommitSeedWith(t *testing.T, projectPath string, superseded bool) {
	t.Helper()
	productionGenerationWriteSeedOutbox(t, projectPath)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	db := productionFinalizedCharacterOpen(t, database, "mode=rw")
	source := productionCharacterStateCandidateSource()
	// The source read INNER JOINs this singleton (finalized_character_context.go:307-313),
	// so it is upserted and read back before anything depends on it.
	productionFinalizedCharacterExec(t, db, `INSERT INTO continuity_projection_meta(id,generation) VALUES('main',?)
 ON CONFLICT(id) DO UPDATE SET generation=excluded.generation`, productionFinalizedCharacterGeneration)
	var generation int64
	productionStoryStateWriteScan(t, db, "SELECT generation FROM continuity_projection_meta WHERE id='main'",
		nil, &generation)
	if generation != productionFinalizedCharacterGeneration {
		t.Fatalf("continuity_projection_meta.generation = %v, want %v", generation,
			productionFinalizedCharacterGeneration)
	}
	// The baseline creates no roster singleton, and the projection refresh can only
	// bump one that exists (character_state_candidates.go:639-641).
	// An upsert, not an INSERT: another fixture in this package may already own the
	// roster singleton, and a plain INSERT would then fail the seed on a UNIQUE
	// constraint before the code under test ran at all.
	productionFinalizedCharacterExec(t, db, `INSERT INTO character_roster_meta(id,schema_version,revision,
 migration_state,legacy_markdown,projection_hash,fact_hash) VALUES('main',1,?,'ready','','','')
 ON CONFLICT(id) DO UPDATE SET revision=excluded.revision, migration_state=excluded.migration_state`,
		productionFinalizedCharacterCommitRosterRevision)
	var revision int64
	productionStoryStateWriteScan(t, db, "SELECT revision FROM character_roster_meta WHERE id='main'", nil, &revision)
	if revision != productionFinalizedCharacterCommitRosterRevision {
		t.Fatalf("character_roster_meta.revision = %v, want %v", revision,
			productionFinalizedCharacterCommitRosterRevision)
	}
	productionFinalizedCharacterInsertCharacter(t, db, productionFinalizedCharacterLiveName, 0,
		productionCharacterStateCandidateProvenance(source))
	productionFinalizedCharacterInsertFreeze(t, db, source, 1)
	// The draft's candidate store, at the frozen watermark, so a staged proposal has
	// the row the reference stages into (baseline_v7_ddl.go:630-643).
	productionFinalizedCharacterExec(t, db, `INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,
 character_state_candidates,source_finalization_id,source_content_hash,projection_generation)
 VALUES(?,?,'',?,?,?,?)`, source.DraftID, source.ChapterNumber,
		productionCharacterStateCandidateStoreText(t, nil, nil), source.FinalizationID, source.ContentHash,
		productionFinalizedCharacterGeneration)
	if superseded {
		productionFinalizedCharacterInsertDraft(t, db, 2, source.ChapterNumber, 2, "finalized",
			productionFinalizedCharacterCommitSecondFinalizationID)
	}
	// The lane admits the file itself, so the seeded rows must be checkpointed into
	// it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
}
