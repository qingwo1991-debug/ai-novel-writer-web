//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the three names production_character_state_candidates_linux.go
// binds, one subtest per aspect:
//
//	finalized-character:list-state-candidates   the pending projection, and only it
//	finalized-character:read-state-candidate    one candidate, and its refusals
//	finalized-character:decide-state-candidate  accept, stale revision, decline, replay
//
// # The fixture
//
// Every subtest drives the real HTTP handler over a disposable admitted project
// (productionGenerationWriteOpen), because that is where the binding, the frozen
// context, the live lease and the wire answer live. The rows are seeded before the
// lane admits the file, through the helpers the neighbouring finalized-character
// and generation-write fixtures already own: the finalized draft and its outbox
// receipt (productionGenerationWriteSeedOutbox,
// production_generation_write_linux_test.go:240), the live character row with its
// field-level provenance, and the freeze receipt that makes a candidate pending
// (production_finalized_character_linux_test.go:611, :621). This file adds only
// what no sibling seeds: the per-draft candidate store in summary_snapshots
// (internal/admission/baseline_v7_ddl.go:630-643).
//
// continuity_projection_meta is written with an INSERT and never with a bare
// UPDATE: the pinned baseline creates the table empty, so an UPDATE alone is a
// silent no-op and the source guard would then answer
// FINALIZED_CHARACTER_STATE_SOURCE_CHANGED for a fixture that is in fact perfect.
//
// # What every subtest proves
//
// An accepted decision is read back over a connection of the test's own
// (productionStoryStateWriteReadback, mode=ro), never from the receipt and never
// through the lane's admitted handle, so what is asserted is the persisted author
// database. A read - served or refused - must leave the file byte for byte alone;
// so must a decision that is refused, replayed or conflicted. No subtest mints an
// operation id, a candidate key or a payload hash the server produced, and nothing
// here weakens a refusal.
//
// The fixtures derive the candidate key the way the store does
// (productionCharacterStateCandidateKey) because the store recomputes it and
// ignores a candidate that disagrees: a derivation that drifted fails here as an
// empty list or a refused read, not as a wrong success.
const (
	// productionCharacterStateCandidateField is the one state field every
	// candidate in this file proposes. It is the author-protected field the seeded
	// provenance records, so an accept overwrites an author value and a decline
	// leaves it exactly as the author wrote it.
	productionCharacterStateCandidateField = "location"
	// productionCharacterStateCandidateValue is the proposed value: a location the
	// live character does not have.
	productionCharacterStateCandidateValue = "北塔"
	// productionCharacterStateCandidateReason is the reference's own preserved
	// reason (summary-repository.ts:162) for a proposal that must not overwrite an
	// author value.
	productionCharacterStateCandidateReason = "author-protected"
	// productionCharacterStateCandidateSecondFinalizationID is the second draft's
	// outbox identity. Its store holds a receipt and no pending candidate, which is
	// what the list has to keep out.
	productionCharacterStateCandidateSecondFinalizationID = "ci-state-candidate-finalization-2"
	productionCharacterStateCandidateSecondOperationID    = "state-candidate-decided-2"
	// The operation ids a decision is taken under. Each one names one payload: the
	// replay arm reuses its id with the same payload, the conflict arm with another.
	productionCharacterStateCandidateAcceptID  = "state-candidate-accept-1"
	productionCharacterStateCandidateDeclineID = "state-candidate-decline-1"
	productionCharacterStateCandidateReplayID  = "state-candidate-replay-1"
	// productionCharacterStateCandidateStaleRevision is the revision the stale
	// candidate is staged at. The live field is at revision 0, so the candidate's
	// expectation can never hold again - which is what a field conflict means (:549).
	productionCharacterStateCandidateStaleRevision = 5
)

// TestProductionCharacterStateCandidatesServeTheFinalizedCandidateFamily is the
// whole of this lane's claim about the three channels: they are served, their
// answers are the store's own values, a decision is persisted and visible over a
// separate read-only connection, and every refusal is the reference's symbolic
// code with the admitted bytes untouched.
func TestProductionCharacterStateCandidatesServeTheFinalizedCandidateFamily(t *testing.T) {
	t.Run("list returns only this project's pending candidates", testProductionCharacterStateCandidateList)
	t.Run("list and read refuse without a live project lease", testProductionCharacterStateCandidateLease)
	t.Run("read returns a stored candidate", testProductionCharacterStateCandidateRead)
	t.Run("read refuses an unknown candidate key", testProductionCharacterStateCandidateReadUnknown)
	t.Run("decide accepts and the change survives", testProductionCharacterStateCandidateAccept)
	t.Run("decide refuses a stale revision", testProductionCharacterStateCandidateStale)
	t.Run("decide declines an author-protected field", testProductionCharacterStateCandidateDecline)
	t.Run("decide replay is idempotent", testProductionCharacterStateCandidateReplay)
}

// testProductionCharacterStateCandidateList proves the projection and its
// boundary: one pending candidate, its six summary fields, and nothing from the
// draft whose store holds only a decision.
func testProductionCharacterStateCandidateList(t *testing.T) {
	source := productionCharacterStateCandidateSource()
	lane := productionGenerationWriteOpen(t, nil, productionCharacterStateCandidateSeed)
	before := lane.bytes(t)
	var listed []map[string]any
	lane.author.decode(t, productionCharacterStateCandidatesList, []any{lane.frozen}, &listed)
	// The second draft's row is selected by the outer query (:480-481) and carries a
	// receipt only, so it must contribute nothing at all.
	if len(listed) != 1 {
		t.Fatalf("the list carries %v summaries, want the one pending candidate: %v", len(listed), listed)
	}
	want := map[string]any{
		"draftId":        float64(source.DraftID),
		"candidateKey":   productionCharacterStateCandidateKey(source, productionCharacterStateCandidateField, productionCharacterStateCandidateValue),
		"finalizationId": source.FinalizationID,
		"characterId":    productionFinalizedCharacterCharacterID,
		"characterName":  productionFinalizedCharacterFrozenName,
		"field":          productionCharacterStateCandidateField,
	}
	for _, key := range productionFinalizedCharacterSortedKeys(want) {
		if got := listed[0][key]; got != want[key] {
			t.Fatalf("summary.%v = %v, want %v: %v", key, got, want[key], listed[0])
		}
	}
	// The summary is the reference's six-key projection (:487-488) and carries no
	// value, no expectation and no stored source object.
	keys := productionFinalizedCharacterSortedKeys(listed[0])
	if len(keys) != len(want) {
		t.Fatalf("summary carries %v keys %v, want %v", len(keys), keys, len(want))
	}
	for _, key := range keys {
		if _, known := want[key]; !known {
			t.Fatalf("summary carries the extra key %v: %v", key, listed[0])
		}
	}
	// The same projection through the store over a connection that could not have
	// written it: the list is the database's, not the response envelope's.
	stored, err := store.NewFinalizedCharacters(productionStoryStateWriteReadback(t, lane.projectPath)).
		ListPendingStateCandidates(context.Background())
	if err != nil {
		t.Fatalf("mode=ro list read-back: %v", err)
	}
	if len(stored) != 1 || stored[0].CandidateKey != want["candidateKey"] {
		t.Fatalf("mode=ro list read-back holds %+v, want the one pending candidate", stored)
	}
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a served list changed the admitted database")
	}
}

// testProductionCharacterStateCandidateLease proves all three names are refused
// without a live project lease, and that the fixture itself is not what refused.
func testProductionCharacterStateCandidateLease(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionCharacterStateCandidateSeed)
	before := lane.bytes(t)
	decision := productionCharacterStateCandidateDecision(productionCharacterStateCandidateAcceptID,
		"accept", 0, sha256Hex(productionFinalizedCharacterLocation))
	read := []any{map[string]any{"draftId": float64(1),
		"candidateKey": productionCharacterStateCandidateKey(productionCharacterStateCandidateSource(),
			productionCharacterStateCandidateField, productionCharacterStateCandidateValue)}}
	// A lease this session never held, and a context with no lease at all: both are
	// refused by the admission (production_durable_linux.go:313-364) before any row
	// is read or written, and neither may answer 200.
	productionEditorRefused(t, lane.author, "stale lease", productionCharacterStateCandidatesList,
		[]any{lane.stale()})
	productionEditorRefused(t, lane.author, "no lease", productionCharacterStateCandidatesList,
		[]any{map[string]any{"projectId": lane.frozen["projectId"], "projectPath": lane.projectPath}})
	productionEditorRefused(t, lane.author, "read under a stale lease", productionCharacterStateCandidatesRead,
		[]any{read[0], lane.stale()})
	// A refused write must refuse symbolically too, never panic on a lease it does
	// not hold.
	productionEditorRefused(t, lane.author, "decide under a stale lease", productionCharacterStateCandidatesDecide,
		[]any{decision, lane.stale()})
	productionEditorRefused(t, lane.author, "decide with no lease", productionCharacterStateCandidatesDecide,
		[]any{decision, map[string]any{"projectId": lane.frozen["projectId"], "projectPath": lane.projectPath}})
	// The same fixture under its live lease still answers, so every refusal above
	// is the lease's and not a fixture that never held a readable candidate.
	var listed []map[string]any
	lane.author.decode(t, productionCharacterStateCandidatesList, []any{lane.frozen}, &listed)
	if len(listed) != 1 {
		t.Fatalf("the live lease answered %v summaries, want the one pending candidate", len(listed))
	}
	// A request arity that is not the reference's is the transport's own refusal,
	// and a frozen context that is not the three keys is the session's.
	productionFinalizedCharacterRefusal(t, "no frozen context",
		lane.author.call(t, productionCharacterStateCandidatesList, []any{}), "INVALID_ARGUMENTS")
	productionFinalizedCharacterRefusal(t, "a context that is not the three keys",
		lane.author.call(t, productionCharacterStateCandidatesList,
			[]any{map[string]any{"projectId": lane.frozen["projectId"]}}), "PROJECT_SESSION_REQUIRED")
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused read or write changed the admitted database")
	}
}

// testProductionCharacterStateCandidateRead proves the read is the store's own
// value: field for field against the same read over a separate mode=ro
// connection, and byte for byte unchanged (this port's read never performs the
// reference's :507-511 upgrade).
func testProductionCharacterStateCandidateRead(t *testing.T) {
	source := productionCharacterStateCandidateSource()
	key := productionCharacterStateCandidateKey(source, productionCharacterStateCandidateField,
		productionCharacterStateCandidateValue)
	lane := productionGenerationWriteOpen(t, nil, productionCharacterStateCandidateSeed)
	before := lane.bytes(t)
	var served map[string]any
	lane.author.decode(t, productionCharacterStateCandidatesRead,
		[]any{map[string]any{"draftId": float64(1), "candidateKey": key}, lane.frozen}, &served)
	// The candidate's binding, as pendingCharacterStateCandidate (:354-375) derives
	// it for a preserved legacy locator: the key, the frozen source and the live
	// field's expectation.
	for field, want := range map[string]any{
		"characterId":            productionFinalizedCharacterCharacterID,
		"characterName":          productionFinalizedCharacterFrozenName,
		"field":                  productionCharacterStateCandidateField,
		"value":                  productionCharacterStateCandidateValue,
		"candidateKey":           key,
		"expectedFieldRevision":  float64(0),
		"expectedFieldValueHash": sha256Hex(productionFinalizedCharacterLocation),
	} {
		if got := served[field]; got != want {
			t.Fatalf("candidate.%v = %v, want %v: %v", field, got, want, served)
		}
	}
	sourceObject := productionFinalizedCharacterObject(t, "candidate.source", served["source"])
	for field, want := range map[string]any{
		"draftId":        float64(source.DraftID),
		"finalizationId": source.FinalizationID,
		"chapterNumber":  float64(source.ChapterNumber),
		"contentHash":    source.ContentHash,
	} {
		if got := sourceObject[field]; got != want {
			t.Fatalf("candidate.source.%v = %v, want %v", field, got, want)
		}
	}
	// The answer is the store value, and the store value survives on disk.
	stored, err := store.NewFinalizedCharacters(productionStoryStateWriteReadback(t, lane.projectPath)).
		ReadPendingStateCandidate(context.Background(), 1, key)
	if err != nil {
		t.Fatalf("mode=ro read-back: %v", err)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var expected map[string]any
	if err := json.Unmarshal(encoded, &expected); err != nil {
		t.Fatal(err)
	}
	productionFinalizedCharacterCompare(t, "candidate", served, expected)
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("the served read changed the admitted database")
	}
}

// testProductionCharacterStateCandidateReadUnknown covers every refusal the read
// can answer: the unknown key (:505), the argument guard (:498-500) and a draft no
// snapshot row can be read for (:349). Each leaves the bytes alone.
func testProductionCharacterStateCandidateReadUnknown(t *testing.T) {
	source := productionCharacterStateCandidateSource()
	lane := productionGenerationWriteOpen(t, nil, productionCharacterStateCandidateSeed)
	before := lane.bytes(t)
	unknown := productionCharacterStateCandidateKey(source, productionCharacterStateCandidateField, "无此候选")
	for _, refusal := range []struct {
		label string
		args  []any
		want  string
	}{
		{"a key that derives from no stored candidate",
			[]any{map[string]any{"draftId": float64(1), "candidateKey": unknown}, lane.frozen},
			"FINALIZED_CHARACTER_STATE_CANDIDATE_NOT_FOUND"},
		// :498-500 !Number.isSafeInteger(draftId) || draftId < 1 || !candidateKey?.trim()
		{"a blank candidate key",
			[]any{map[string]any{"draftId": float64(1), "candidateKey": "   "}, lane.frozen},
			"FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID"},
		{"no candidate key",
			[]any{map[string]any{"draftId": float64(1)}, lane.frozen},
			"FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID"},
		{"a fractional draft id",
			[]any{map[string]any{"draftId": float64(1.5), "candidateKey": unknown}, lane.frozen},
			"FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID"},
		{"a zero draft id",
			[]any{map[string]any{"draftId": float64(0), "candidateKey": unknown}, lane.frozen},
			"FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID"},
		{"a textual draft id",
			[]any{map[string]any{"draftId": "1", "candidateKey": unknown}, lane.frozen},
			"FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID"},
		{"a request that is not an object",
			[]any{"1", lane.frozen},
			"FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID"},
		// :349 a draft whose snapshot row does not exist is a changed source, not
		// an empty candidate.
		{"a draft with no snapshot row",
			[]any{map[string]any{"draftId": float64(9), "candidateKey": unknown}, lane.frozen},
			"FINALIZED_CHARACTER_STATE_SOURCE_CHANGED"},
	} {
		productionFinalizedCharacterRefusal(t, refusal.label,
			lane.author.call(t, productionCharacterStateCandidatesRead, refusal.args), refusal.want)
	}
	// The stored candidate is still there after every refusal, and still readable.
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused read changed the admitted database")
	}
}

// testProductionCharacterStateCandidateAccept is the accept arm (:545-559): the
// proposal becomes the author's field value, its provenance records the author
// chapter and the next revision, the candidate leaves the pending store, and the
// receipt is the store's own.
func testProductionCharacterStateCandidateAccept(t *testing.T) {
	source := productionCharacterStateCandidateSource()
	key := productionCharacterStateCandidateKey(source, productionCharacterStateCandidateField,
		productionCharacterStateCandidateValue)
	lane := productionGenerationWriteOpen(t, nil, productionCharacterStateCandidateSeed)
	var receipt map[string]any
	lane.author.decode(t, productionCharacterStateCandidatesDecide,
		[]any{productionCharacterStateCandidateDecision(productionCharacterStateCandidateAcceptID, "accept", 0,
			sha256Hex(productionFinalizedCharacterLocation)), lane.frozen}, &receipt)
	// :566 the receipt names the decision this call took, and carries the payload
	// hash the store computed, not one the browser supplied.
	if receipt["idempotent"] != false || receipt["decision"] != "accept" {
		t.Fatalf("receipt = %v, want a first accept", receipt)
	}
	if receipt["candidateKey"] != key || receipt["operationId"] != productionCharacterStateCandidateAcceptID {
		t.Fatalf("receipt names %v/%v, want %v/%v", receipt["candidateKey"], receipt["operationId"],
			key, productionCharacterStateCandidateAcceptID)
	}
	payloadHash, ok := receipt["payloadHash"].(string)
	if !ok || len(payloadHash) != 64 {
		t.Fatalf("receipt.payloadHash = %v, want the store's 64-hex digest", receipt["payloadHash"])
	}
	sourceObject := productionFinalizedCharacterObject(t, "receipt.source", receipt["source"])
	if sourceObject["finalizationId"] != source.FinalizationID ||
		sourceObject["contentHash"] != source.ContentHash {
		t.Fatalf("receipt.source = %v, want the frozen identity", sourceObject)
	}

	// The change is on disk: the character row over a connection that could not
	// have written it.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var location, provenance string
	productionStoryStateWriteScan(t, readback,
		"SELECT cs_location,cs_provenance FROM characters WHERE character_id=?",
		[]any{productionFinalizedCharacterCharacterID}, &location, &provenance)
	if location != productionCharacterStateCandidateValue {
		t.Fatalf("cs_location = %v, want the accepted %v", location, productionCharacterStateCandidateValue)
	}
	// :553 provenance[field] = { kind: 'author', chapterNumber: candidate.source.chapterNumber, revision: current.revision + 1 }
	var recorded struct {
		Location map[string]any `json:"location"`
	}
	if err := json.Unmarshal([]byte(provenance), &recorded); err != nil {
		t.Fatalf("stored provenance %v: %v", provenance, err)
	}
	if recorded.Location["kind"] != "author" || recorded.Location["chapterNumber"] != float64(source.ChapterNumber) ||
		recorded.Location["revision"] != float64(1) {
		t.Fatalf("stored location provenance = %v, want the author arm of chapter %v at revision 1",
			recorded.Location, source.ChapterNumber)
	}
	// :562-563 the candidate left the pending store and the decision became a
	// receipt, on disk.
	staged := productionCharacterStateCandidateStored(t, readback, 1)
	if len(staged.Pending) != 0 {
		t.Fatalf("the store still holds %v pending candidates: %v", len(staged.Pending), staged.Pending)
	}
	if len(staged.Receipts) != 1 {
		t.Fatalf("the store holds %v receipts, want exactly the accept: %v", len(staged.Receipts), staged.Receipts)
	}
	if staged.Receipts[0]["operationId"] != productionCharacterStateCandidateAcceptID ||
		staged.Receipts[0]["decision"] != "accept" ||
		staged.Receipts[0]["candidateKey"] != key ||
		staged.Receipts[0]["payloadHash"] != payloadHash {
		t.Fatalf("the staged receipt = %v, want the one this call answered", staged.Receipts[0])
	}
	// The family agrees with itself: the pending list is empty, and the decided
	// candidate can no longer be read.
	var listed []map[string]any
	lane.author.decode(t, productionCharacterStateCandidatesList, []any{lane.frozen}, &listed)
	if len(listed) != 0 {
		t.Fatalf("the list still holds %v candidates after the accept: %v", len(listed), listed)
	}
	productionFinalizedCharacterRefusal(t, "the decided candidate",
		lane.author.call(t, productionCharacterStateCandidatesRead,
			[]any{map[string]any{"draftId": float64(1), "candidateKey": key}, lane.frozen}),
		"FINALIZED_CHARACTER_STATE_CANDIDATE_NOT_FOUND")
}

// testProductionCharacterStateCandidateStale is the :549-551 refusal. The
// candidate is bound to a revision the field no longer has, while the request
// echoes the candidate exactly, so the decision reaches the field comparison
// instead of being refused as an unknown candidate.
func testProductionCharacterStateCandidateStale(t *testing.T) {
	source := productionCharacterStateCandidateSource()
	lane := productionGenerationWriteOpen(t, nil, productionCharacterStateCandidateStaleSeed)
	before := lane.bytes(t)
	productionFinalizedCharacterRefusal(t, "a stale field revision",
		lane.author.call(t, productionCharacterStateCandidatesDecide,
			[]any{productionCharacterStateCandidateDecision(productionCharacterStateCandidateAcceptID, "accept",
				productionCharacterStateCandidateStaleRevision,
				sha256Hex(productionFinalizedCharacterLocation)), lane.frozen}),
		"FINALIZED_CHARACTER_STATE_FIELD_CONFLICT")
	// :549-551 a request that does not echo the candidate is the other refusal:
	// the candidate is gone or no longer matches (:540-544).
	productionFinalizedCharacterRefusal(t, "a request that does not echo the candidate",
		lane.author.call(t, productionCharacterStateCandidatesDecide,
			[]any{productionCharacterStateCandidateDecision(productionCharacterStateCandidateAcceptID, "accept", 0,
				sha256Hex(productionFinalizedCharacterLocation)), lane.frozen}),
		"FINALIZED_CHARACTER_STATE_CANDIDATE_NOT_FOUND")
	// The refused write changed nothing at all: the character keeps its author
	// value and the candidate is still pending.
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("the refused decision changed the admitted database")
	}
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var location string
	productionStoryStateWriteScan(t, readback, "SELECT cs_location FROM characters WHERE character_id=?",
		[]any{productionFinalizedCharacterCharacterID}, &location)
	if location != productionFinalizedCharacterLocation {
		t.Fatalf("cs_location = %v, want the author's %v", location, productionFinalizedCharacterLocation)
	}
	staged := productionCharacterStateCandidateStored(t, readback, 1)
	if len(staged.Pending) != 1 || len(staged.Receipts) != 0 {
		t.Fatalf("the refused decision left %v pending and %v receipts, want one pending and none",
			len(staged.Pending), len(staged.Receipts))
	}
	if staged.Pending[0]["candidateKey"] != productionCharacterStateCandidateKey(source,
		productionCharacterStateCandidateField, productionCharacterStateCandidateValue) {
		t.Fatalf("the pending candidate = %v, want the bound one", staged.Pending[0])
	}
}

// testProductionCharacterStateCandidateDecline is the :560-566 arm: nothing is
// applied and the decision is staged, which is how an author-protected field keeps
// its author value while the proposal is consumed.
func testProductionCharacterStateCandidateDecline(t *testing.T) {
	source := productionCharacterStateCandidateSource()
	key := productionCharacterStateCandidateKey(source, productionCharacterStateCandidateField,
		productionCharacterStateCandidateValue)
	lane := productionGenerationWriteOpen(t, nil, productionCharacterStateCandidateDeclineSeed)
	var receipt map[string]any
	lane.author.decode(t, productionCharacterStateCandidatesDecide,
		[]any{productionCharacterStateCandidateDecision(productionCharacterStateCandidateDeclineID, "decline", 0,
			sha256Hex(productionFinalizedCharacterLocation)), lane.frozen}, &receipt)
	if receipt["idempotent"] != false || receipt["decision"] != "decline" || receipt["candidateKey"] != key {
		t.Fatalf("receipt = %v, want a first decline of %v", receipt, key)
	}
	// The character was not touched: the author's value and the author's provenance
	// are the ones the fixture wrote.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var location, provenance string
	productionStoryStateWriteScan(t, readback,
		"SELECT cs_location,cs_provenance FROM characters WHERE character_id=?",
		[]any{productionFinalizedCharacterCharacterID}, &location, &provenance)
	if location != productionFinalizedCharacterLocation {
		t.Fatalf("cs_location = %v, want the author's untouched %v", location,
			productionFinalizedCharacterLocation)
	}
	if provenance != productionCharacterStateCandidateProvenance(source) {
		t.Fatalf("cs_provenance = %v, want it unchanged", provenance)
	}
	// The proposal is consumed, and the decision is staged as a receipt.
	staged := productionCharacterStateCandidateStored(t, readback, 1)
	if len(staged.Pending) != 0 {
		t.Fatalf("the store still holds %v pending candidates: %v", len(staged.Pending), staged.Pending)
	}
	if len(staged.Receipts) != 1 || staged.Receipts[0]["decision"] != "decline" ||
		staged.Receipts[0]["operationId"] != productionCharacterStateCandidateDeclineID {
		t.Fatalf("the staged receipts = %v, want the one decline", staged.Receipts)
	}
	var listed []map[string]any
	lane.author.decode(t, productionCharacterStateCandidatesList, []any{lane.frozen}, &listed)
	if len(listed) != 0 {
		t.Fatalf("the list still holds %v candidates after the decline: %v", len(listed), listed)
	}
}

// testProductionCharacterStateCandidateReplay is the :528-535 arm: an operation id
// already decided answers its own receipt and writes nothing, and the same id with
// another payload is an operation conflict.
func testProductionCharacterStateCandidateReplay(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionCharacterStateCandidateSeed)
	request := productionCharacterStateCandidateDecision(productionCharacterStateCandidateReplayID, "accept", 0,
		sha256Hex(productionFinalizedCharacterLocation))
	var first map[string]any
	lane.author.decode(t, productionCharacterStateCandidatesDecide, []any{request, lane.frozen}, &first)
	if first["idempotent"] != false {
		t.Fatalf("the first decision reports idempotent = %v", first["idempotent"])
	}
	before := lane.bytes(t)
	var replay map[string]any
	lane.author.decode(t, productionCharacterStateCandidatesDecide, []any{request, lane.frozen}, &replay)
	if replay["idempotent"] != true {
		t.Fatalf("the replayed decision reports idempotent = %v, want true", replay["idempotent"])
	}
	// :534 the replay is the stored receipt, field for field; only idempotent moves.
	replay["idempotent"] = false
	productionFinalizedCharacterCompare(t, "receipt", replay, first)
	// A replay writes nothing at all.
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("the replayed decision changed the admitted database")
	}
	// :533 the same operation id with a different payload is a conflict, and it is
	// answered before the source guard, so a repeated conflict still refuses.
	conflict := productionCharacterStateCandidateDecision(productionCharacterStateCandidateReplayID, "decline", 0,
		sha256Hex(productionFinalizedCharacterLocation))
	productionFinalizedCharacterRefusal(t, "the same operation id with another payload",
		lane.author.call(t, productionCharacterStateCandidatesDecide, []any{conflict, lane.frozen}),
		"FINALIZED_CHARACTER_STATE_OPERATION_CONFLICT")
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("the conflicted decision changed the admitted database")
	}
	// The accepted value survived both refusals.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var location string
	productionStoryStateWriteScan(t, readback, "SELECT cs_location FROM characters WHERE character_id=?",
		[]any{productionFinalizedCharacterCharacterID}, &location)
	if location != productionCharacterStateCandidateValue {
		t.Fatalf("cs_location = %v, want the accepted %v", location, productionCharacterStateCandidateValue)
	}
}

// productionCharacterStateCandidateSource is the frozen source identity every
// fixture writes: chapter 1's finalized draft, whose outbox receipt is the
// neighbouring generation-write fixture's own row.
func productionCharacterStateCandidateSource() store.FinalizedCharacterSourceIdentity {
	return store.FinalizedCharacterSourceIdentity{DraftID: 1, ChapterNumber: 1,
		FinalizationID: productionGenerationWriteFinalizationID, ContentHash: sha256Hex(productionEditorSeed)}
}

// productionCharacterStateCandidateProvenance is the field-level provenance the
// character row carries (production_finalized_character_linux_test.go:652), so a
// decline can prove it left those bytes alone.
func productionCharacterStateCandidateProvenance(source store.FinalizedCharacterSourceIdentity) string {
	return productionFinalizedCharacterProvenanceJSON(source, 0,
		productionFinalizedCharacterContinuityEpoch(productionEditorProjectID))
}

// productionCharacterStateCandidateKey is characterStateCandidateKey
// (summary-repository.ts:272-274):
//
//	"fcs-state:" + sha256(JSON.stringify([draftId, finalizationId, contentHash, characterId, field, sha256(value)]))
//
// Every element of this fixture's array is ASCII, so the %v inside literal quotes
// is byte for byte what the store's own JSON encoder produces. That is a fixture
// convenience and not a second derivation: the store recomputes the key and keeps only a candidate
// whose key it can reproduce, so a drift here fails as an empty list or a refused
// read.
func productionCharacterStateCandidateKey(source store.FinalizedCharacterSourceIdentity, field, value string) string {
	digest := fmt.Sprintf(`[%v,"%v","%v","%v","%v","%v"]`, source.DraftID, source.FinalizationID,
		source.ContentHash, productionFinalizedCharacterCharacterID, field, sha256Hex(value))
	return "fcs-state:" + sha256Hex(digest)
}

// productionCharacterStateCandidateLegacy is a preserved locator: it carries no
// bound metadata at all (:142-146), so only a read may upgrade it and a decide must
// use the expectation that read derived.
func productionCharacterStateCandidateLegacy(reason string) map[string]any {
	candidate := map[string]any{
		"characterId":   productionFinalizedCharacterCharacterID,
		"characterName": productionFinalizedCharacterFrozenName,
		"field":         productionCharacterStateCandidateField,
		"value":         productionCharacterStateCandidateValue,
	}
	if reason != "" {
		candidate["reason"] = reason
	}
	return candidate
}

// productionCharacterStateCandidateBound is a source-bound candidate in the
// reference's key order (:153-168), staged at an explicit revision and value hash.
func productionCharacterStateCandidateBound(source store.FinalizedCharacterSourceIdentity,
	revision int64, valueHash string) map[string]any {
	return map[string]any{
		"characterId":            productionFinalizedCharacterCharacterID,
		"characterName":          productionFinalizedCharacterFrozenName,
		"field":                  productionCharacterStateCandidateField,
		"value":                  productionCharacterStateCandidateValue,
		"candidateKey":           productionCharacterStateCandidateKey(source, productionCharacterStateCandidateField, productionCharacterStateCandidateValue),
		"source":                 productionCharacterStateCandidateSourceObject(source),
		"expectedFieldRevision":  revision,
		"expectedFieldValueHash": valueHash,
	}
}

// productionCharacterStateCandidateSourceObject is the four-field source object a
// stored candidate and a stored receipt carry (:1064-1077).
func productionCharacterStateCandidateSourceObject(source store.FinalizedCharacterSourceIdentity) map[string]any {
	return map[string]any{"draftId": source.DraftID, "finalizationId": source.FinalizationID,
		"chapterNumber": source.ChapterNumber, "contentHash": source.ContentHash}
}

// productionCharacterStateCandidateStoreText is JSON.stringify of the version-2
// store (:1103-1116): `{"version":2,"pending":[…],"receipts":[…]}`.
func productionCharacterStateCandidateStoreText(t *testing.T, pending, receipts []any) string {
	t.Helper()
	if pending == nil {
		pending = []any{}
	}
	if receipts == nil {
		receipts = []any{}
	}
	encoded, err := json.Marshal(map[string]any{"version": 2, "pending": pending, "receipts": receipts})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// productionCharacterStateCandidateDecision is the eight-key decision request of
// src/shared/finalized-continuity.ts:91-100, with the draft and field this file's
// fixture seeds.
func productionCharacterStateCandidateDecision(operationID, decision string, revision int64,
	valueHash string) map[string]any {
	return map[string]any{
		"draftId":                float64(1),
		"candidateKey":           productionCharacterStateCandidateKey(productionCharacterStateCandidateSource(),
			productionCharacterStateCandidateField, productionCharacterStateCandidateValue),
		"characterId":            productionFinalizedCharacterCharacterID,
		"field":                  productionCharacterStateCandidateField,
		"expectedFieldRevision":  float64(revision),
		"expectedFieldValueHash": valueHash,
		"operationId":            operationID,
		"decision":               decision,
	}
}

// productionCharacterStateCandidateStoredValue is one draft's candidate store as
// it sits on disk: the version-2 envelope with its pending candidates and its
// accumulated decisions.
type productionCharacterStateCandidateStoredValue struct {
	Version  int64            `json:"version"`
	Pending  []map[string]any `json:"pending"`
	Receipts []map[string]any `json:"receipts"`
}

// productionCharacterStateCandidateStored scans that store for one draft through a
// connection of the test's own, never through the lane's admitted handle.
func productionCharacterStateCandidateStored(t *testing.T, db *sql.DB, draftID int64) productionCharacterStateCandidateStoredValue {
	t.Helper()
	var text string
	productionStoryStateWriteScan(t, db,
		"SELECT character_state_candidates FROM summary_snapshots WHERE draft_id=?", []any{draftID}, &text)
	var stored productionCharacterStateCandidateStoredValue
	if err := json.Unmarshal([]byte(text), &stored); err != nil {
		t.Fatalf("stored candidate store %v: %v", text, err)
	}
	if stored.Version != 2 {
		t.Fatalf("the stored candidate store is version %v, want 2: %v", stored.Version, text)
	}
	return stored
}

// productionCharacterStateCandidateSeed is the shared fixture: one pending legacy
// candidate for draft 1 and one receipt-only store for draft 2.
func productionCharacterStateCandidateSeed(t *testing.T, projectPath string) {
	productionCharacterStateCandidateSeedWith(t, projectPath, func(store.FinalizedCharacterSourceIdentity) []any {
		return []any{productionCharacterStateCandidateLegacy("")}
	})
}

// productionCharacterStateCandidateDeclineSeed stages a candidate that must not
// overwrite an author value (:162 preserves the reference's own reason).
func productionCharacterStateCandidateDeclineSeed(t *testing.T, projectPath string) {
	productionCharacterStateCandidateSeedWith(t, projectPath, func(store.FinalizedCharacterSourceIdentity) []any {
		return []any{productionCharacterStateCandidateLegacy(productionCharacterStateCandidateReason)}
	})
}

// productionCharacterStateCandidateStaleSeed stages a bound candidate whose
// expectation the live field no longer meets.
func productionCharacterStateCandidateStaleSeed(t *testing.T, projectPath string) {
	productionCharacterStateCandidateSeedWith(t, projectPath, func(source store.FinalizedCharacterSourceIdentity) []any {
		return []any{productionCharacterStateCandidateBound(source, productionCharacterStateCandidateStaleRevision,
			sha256Hex(productionFinalizedCharacterLocation))}
	})
}

// productionCharacterStateCandidateSeedWith writes the rows every subtest reads:
// the finalized draft and its outbox receipt (the neighbouring fixture's own), the
// live character with its provenance, the freeze receipt that binds a candidate to
// a frozen name, and the two per-draft candidate stores.
func productionCharacterStateCandidateSeedWith(t *testing.T, projectPath string,
	pending func(source store.FinalizedCharacterSourceIdentity) []any) {
	t.Helper()
	productionGenerationWriteSeedOutbox(t, projectPath)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	db := productionFinalizedCharacterOpen(t, database, "mode=rw")
	source := productionCharacterStateCandidateSource()
	// The pinned baseline creates this table empty, so the row has to be an INSERT:
	// a bare UPDATE is a silent no-op, and every source guard below would then
	// answer FINALIZED_CHARACTER_STATE_SOURCE_CHANGED for a fixture that is in fact
	// perfect. OR IGNORE keeps a baseline that already carries the singleton row
	// from failing the whole fixture on a UNIQUE constraint, and the readback makes
	// the generation this fixture depends on a checked fact rather than an
	// assumption.
	productionFinalizedCharacterExec(t, db,
		"INSERT OR IGNORE INTO continuity_projection_meta(id,generation) VALUES('main',?)",
		productionFinalizedCharacterGeneration)
	var generation int64
	productionStoryStateWriteScan(t, db,
		"SELECT generation FROM continuity_projection_meta WHERE id='main'", nil, &generation)
	if generation != productionFinalizedCharacterGeneration {
		t.Fatalf("continuity_projection_meta.generation = %v, want %v", generation,
			productionFinalizedCharacterGeneration)
	}
	productionFinalizedCharacterInsertCharacter(t, db, productionFinalizedCharacterLiveName, 0,
		productionCharacterStateCandidateProvenance(source))
	productionFinalizedCharacterInsertFreeze(t, db, source, 1)
	// The candidate store lives in one row per draft, addressed by draft_id.
	productionFinalizedCharacterExec(t, db, "DELETE FROM summary_snapshots")
	productionFinalizedCharacterExec(t, db, `INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,
 character_state_candidates,source_finalization_id,source_content_hash,projection_generation)
 VALUES(?,?,'',?,?,?,?)`, source.DraftID, source.ChapterNumber,
		productionCharacterStateCandidateStoreText(t, pending(source), nil),
		source.FinalizationID, source.ContentHash, productionFinalizedCharacterGeneration)
	// A second finalized draft whose store holds a receipt and no pending
	// candidate: the outer list query still selects it (:480-481) and it contributes
	// nothing.
	second := productionFinalizedCharacterInsertDraft(t, db, 2, 2, 1, "finalized",
		productionCharacterStateCandidateSecondFinalizationID)
	productionFinalizedCharacterExec(t, db, `INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,
 character_state_candidates,source_finalization_id,source_content_hash,projection_generation)
 VALUES(?,?,'',?,?,?,?)`, second.DraftID, second.ChapterNumber,
		productionCharacterStateCandidateStoreText(t, nil, []any{map[string]any{
			"candidateKey": productionCharacterStateCandidateKey(second, productionCharacterStateCandidateField,
				"上一次已决定的候选"),
			"operationId": productionCharacterStateCandidateSecondOperationID,
			"payloadHash": sha256Hex(productionCharacterStateCandidateSecondOperationID),
			"decision":    "accept",
			"source":      productionCharacterStateCandidateSourceObject(second),
		}}),
		second.FinalizationID, second.ContentHash, productionFinalizedCharacterGeneration)
	// The lane admits the file itself, so the seeded rows must be checkpointed into
	// it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
}
