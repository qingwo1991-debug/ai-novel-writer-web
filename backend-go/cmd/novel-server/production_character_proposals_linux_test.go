//go:build linux

// This file proves the character-proposal lane through the frozen session and a
// real sqlite file: the staged proposal is parsed model output, approve is the only
// writer, a repeated approve reproduces the reference, and every refusal leaves the
// admitted bytes exactly as they were.
//
// # The reference
//
//	electron/services/character-proposal-service.ts:159  stage
//	electron/services/character-proposal-service.ts:114  read
//	electron/services/character-proposal-service.ts:196  cancel
//	electron/services/character-proposal-service.ts:209  approve
//	electron/controllers/generation-controller.ts:88-101 the wire-code gate
//
// # What it does not cover
//
// The two pending-finalized names are refused, not served, so there is nothing to
// stage for them: the last test proves they stay unbound and that every seam the
// file records still points at the line it names.

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/characterproposal"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

const (
	// productionCharacterProposalEpoch is the epoch the seeded run was frozen under.
	// It is deliberately not the lease of the session that stages the proposal: a run
	// outlives the lease it was admitted under, so the proof compares the handle with
	// the run's own binding and never with the calling lease.
	productionCharacterProposalEpoch = "lease-0000000001-character-architecture"

	// The nonces the three seeded attempts were reserved under, and the identity of
	// the two artifacts they settled.
	productionCharacterProposalRunNonce      = "ci-character-proposal-run"
	productionCharacterProposalManifestNonce = "ci-character-proposal-manifest"
	productionCharacterProposalDetailNonce   = "ci-character-proposal-detail"

	// productionCharacterProposalModelRevision is the model receipt the manifest
	// carries: the revision approve attributes its provenance to.
	productionCharacterProposalModelRevision = "5f0c4bd9c0e24f0d9a37c56e2b1d8a47c39e5f60a1b2c3d4e5f60718293a4b5c"

	// The operation ids the approvals are taken under. Each one names one payload:
	// the replay arm repeats its id with the same body, the conflict arm with another.
	productionCharacterProposalApproveID = "character-proposal-approve-1"
	productionCharacterProposalReplayID  = "character-proposal-approve-1"
	productionCharacterProposalSecondID  = "character-proposal-approve-2"
	productionCharacterProposalKeepID    = "character-proposal-keep-1"
	productionCharacterProposalMapID     = "character-proposal-map-1"

	// The three slots the fixture manifest declares, and the three names its detail
	// document gives them. The parser checks the two agree.
	productionCharacterProposalSlotOne   = "s1"
	productionCharacterProposalSlotTwo   = "s2"
	productionCharacterProposalSlotThree = "s3"
	productionCharacterProposalNameOne   = "沉舟"
	productionCharacterProposalNameTwo   = "白露"
	productionCharacterProposalNameThree = "程守"

	// productionCharacterProposalExistingID is the identity a `map` selection adopts.
	// It is seeded with one alias and no field values, so every proposed field is
	// adopted.
	productionCharacterProposalExistingID        = "ci-character-existing"
	productionCharacterProposalExistingSourceKey = "roster:seed"

	// productionCharacterProposalAppearance is the one field the tests read back: it
	// is the manifest's free text for the protagonist, carried through the parser and
	// the commit unchanged.
	productionCharacterProposalAppearance = "瘦高，左眉有一道旧疤"
	productionCharacterProposalLocation = "门内"
)

// productionCharacterProposalManifest is one complete architecture answer: three
// slots, exactly one protagonist, and one relation pair. It is the shape
// src/shared/character-proposal-parser.ts:60-106 accepts.
const productionCharacterProposalManifest = `{
  "slots": [
    {"slotId": "s1", "name": "沉舟", "role": "protagonist", "narrativeDuty": "守住那扇门",
     "relations": [{"targetSlotId": "s2", "relation": "旧识"}]},
    {"slotId": "s2", "name": "白露", "role": "supporting", "narrativeDuty": "递出钥匙",
     "relations": [{"targetSlotId": "s1", "relation": "旧识"}]},
    {"slotId": "s3", "name": "程守", "role": "antagonist", "narrativeDuty": "封死退路",
     "relations": []}
  ]
}`

// productionCharacterProposalDetails is the detail document for that manifest: one
// entry per slot, all eleven identity fields, and a complete current state.
const productionCharacterProposalDetails = `{
  "entries": [
    {"slotId": "s1", "name": "沉舟", "role": "protagonist", "gender": "男", "age": 27,
     "appearance": "瘦高，左眉有一道旧疤", "personality": "寡言，先做后说",
     "background": "守门人的养子", "abilities": "听风辨位", "motivation": "找回自己的名字",
     "arc": "从旁观到承担", "notes": "左手有旧伤",
     "currentState": {"location": "门内", "powerLevel": "初阶", "physicalState": "轻伤",
      "mentalState": "警惕", "keyItems": ["铜钥匙", "旧信"],
      "recentEvents": ["推开门", "听见风声"]}},
    {"slotId": "s2", "name": "白露", "role": "supporting", "gender": "女", "age": 24,
     "appearance": "总穿白衣", "personality": "爱笑，先给后取", "background": "钥匙铺的女儿",
     "abilities": "配钥匙", "motivation": "离开这条街", "arc": "从给予到索取",
     "notes": "怕黑",
     "currentState": {"location": "钥匙铺", "powerLevel": "无", "physicalState": "康健",
      "mentalState": "期待", "keyItems": ["新配的钥匙"], "recentEvents": ["磨了一夜钥匙"]}},
    {"slotId": "s3", "name": "程守", "role": "antagonist", "gender": "男", "age": 51,
     "appearance": "背有点驼", "personality": "守信而刻板", "background": "门外来的人",
     "abilities": "封门的手艺", "motivation": "把门永远关上", "arc": "从守规到违心",
     "notes": "右耳听不见",
     "currentState": {"location": "门外", "powerLevel": "高阶", "physicalState": "康健",
      "mentalState": "坚定", "keyItems": ["一把旧锁"], "recentEvents": ["锁上了侧门"]}}
  ]
}`

// productionCharacterProposalRun is the settlement chain the fixture wrote: the run
// the source handle names and the two artifacts the source references.
type productionCharacterProposalRun struct {
	RunID        string
	RootActionID string
	Epoch        string
	Manifest    productionCharacterProposalArtifact
	Detail      productionCharacterProposalArtifact
}

// productionCharacterProposalArtifact is one settled artifact as the ledger stored
// it: the id, the revision the snapshot wrote, and the text hash it recorded.
type productionCharacterProposalArtifact struct {
	ArtifactID string
	Revision   int64
	TextHash   string
}

// productionCharacterProposalFixture is one seeded project together with the model
// answers it settled. Run is filled by seed, because the artifact ids only exist
// once the ledger has written them.
type productionCharacterProposalFixture struct {
	Manifest string
	Details  string
	Existing bool
	Run      productionCharacterProposalRun
}

// seed is the fixture hook productionGenerationWriteOpen runs against the unpacked
// project before the lane admits it.
func (f *productionCharacterProposalFixture) seed(t *testing.T, projectPath string) {
	t.Helper()
	f.Run = productionCharacterProposalSeed(t, projectPath, f.Manifest, f.Details, f.Existing)
}

// TestProductionCharacterProposalsServeTheArchitectureSource is the whole of this
// lane's claim: stage parses and stores, approve is the only writer, a repeated
// approve repeats its reference, and every refusal is symbolic and leaves the file
// alone.
func TestProductionCharacterProposalsServeTheArchitectureSource(t *testing.T) {
	t.Run("stage then approve lands the characters", testProductionCharacterProposalStageThenApprove)
	t.Run("approve keeps an unresolved slot out of the roster", testProductionCharacterProposalKeep)
	t.Run("approve maps onto an existing identity", testProductionCharacterProposalMap)
	t.Run("approve refuses an unknown staged id", testProductionCharacterProposalUnknownBatch)
	t.Run("stage refuses a malformed answer and writes nothing", testProductionCharacterProposalMalformed)
	t.Run("approve replay repeats its reference", testProductionCharacterProposalReplay)
	t.Run("a cancelled batch can never be approved", testProductionCharacterProposalCancel)
	t.Run("every source kind this lane cannot prove is refused", testProductionCharacterProposalSourceKinds)
	t.Run("no name answers without a live project lease", testProductionCharacterProposalStaleLease)
}

// TestProductionCharacterProposalsRefusePendingFinalized proves the two names this
// file does not bind: they stay unbound over the real server, and every seam the
// file records still points at the declaration it names.
func TestProductionCharacterProposalsRefusePendingFinalized(t *testing.T) {
	fixture := &productionCharacterProposalFixture{
		Manifest: productionCharacterProposalManifest,
		Details:  productionCharacterProposalDetails,
	}
	lane := productionGenerationWriteOpen(t, nil, fixture.seed)
	for _, operation := range []string{
		productionCharacterProposalListPendingName,
		productionCharacterProposalReadPendingName,
	} {
		response := lane.author.call(t, operation, []any{map[string]any{}, lane.frozen})
		if response.Code != 501 {
			t.Fatalf("%s answered %v instead of staying unbound: %s", operation, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s did not refuse with the fail-closed code: %s", operation, response.Body.String())
		}
		if _, documented := productionCharacterProposalPendingSeam(operation); !documented {
			t.Fatalf("%s is unbound but carries no seam", operation)
		}
	}
	// The four served names carry no seam: a seam is only ever a promise that the
	// name is refused.
	for _, operation := range []string{
		productionCharacterProposalStageName,
		productionCharacterProposalReadName,
		productionCharacterProposalApproveName,
		productionCharacterProposalCancelName,
	} {
		if _, documented := productionCharacterProposalPendingSeam(operation); documented {
			t.Fatalf("%s is served but carries a seam", operation)
		}
	}
	seams := productionCharacterProposalPendingSeams()
	if len(seams) != 2 {
		t.Fatalf("the lane documents %v refused names, want the two pending-finalized ones", len(seams))
	}
	for _, seam := range seams {
		if len(seam.Missing) == 0 {
			t.Fatalf("%s carries no missing port", seam.Operation)
		}
		for _, line := range seam.Missing {
			content, err := productionCharacterProposalReadSourceLine(line.Path, line.Line)
			if err != nil {
				t.Fatalf("%s: %v", seam.Operation, err)
			}
			if !strings.Contains(content, line.Marker) {
				t.Fatalf("%s cites %s:%v for %v, but that line is %v", seam.Operation,
					line.Path, line.Line, line.Marker, content)
			}
		}
	}
	// The inventory the file answers for is web/operations.json's own order, and the
	// two refused names are in it: a family that dropped them would read as served.
	inventory := productionCharacterProposalOperations()
	if len(inventory) != 6 {
		t.Fatalf("the family declares %v names, want the six of web/operations.json:56-61", len(inventory))
	}
	if inventory[2] != productionCharacterProposalListPendingName || inventory[3] != productionCharacterProposalReadPendingName {
		t.Fatalf("the family order is %v, want operations.json's own", inventory)
	}
}

// TestProductionCharacterProposalsMapEveryRefusal proves the one mapping layer: every
// refusal the parser and this lane can raise has a wire code, and every code is
// either one the reference publishes or the controller's fallback.
func TestProductionCharacterProposalsMapEveryRefusal(t *testing.T) {
	for _, refusal := range characterproposal.SortedRefusals() {
		if code := productionCharacterProposalRefusalCode(refusal); code == "" {
			t.Fatalf("the parser refusal %v has no wire code", refusal)
		}
	}
	for _, entry := range productionCharacterProposalRefusals() {
		if entry.code == "" {
			t.Fatalf("the refusal table carries an entry with no code: %v", entry.refusal)
		}
		if !productionCharacterProposalWireCode(entry.code) {
			t.Fatalf("the refusal table publishes %v, which the controller's gate would reword", entry.code)
		}
		if got := productionCharacterProposalRefusalCode(entry.refusal); got != entry.code {
			t.Fatalf("the table maps %v to %v but the lookup answers %v", entry.refusal, entry.code, got)
		}
	}
	// Anything the table does not name is the controller's own fallback, never the
	// text of the failure.
	if got := productionCharacterProposalFailure(errors.New("disk I/O error: near \"SELECT\": syntax error")); got.Error() != productionCharacterProposalRequestFailed {
		t.Fatalf("an unnamed failure answers %v, want %v", got, productionCharacterProposalRequestFailed)
	}
	// A code the reference publishes passes through unchanged.
	if got := productionCharacterProposalFailure(store.ErrCharacterAuthorRosterNotReady); got.Error() != "CHARACTER_AUTHOR_ROSTER_NOT_READY" {
		t.Fatalf("a named store refusal answers %v, want its own code", got)
	}
}

// testProductionCharacterProposalStageThenApprove is the lane's main claim: a staged
// batch is one proposal row and no roster write, and the approval is what lands the
// characters and their aliases. The eleven static fields are the whole of a creation:
// the current state is the chapter-summary lane's to write.
func testProductionCharacterProposalStageThenApprove(t *testing.T) {
	fixture := &productionCharacterProposalFixture{
		Manifest: productionCharacterProposalManifest,
		Details:  productionCharacterProposalDetails,
	}
	lane := productionGenerationWriteOpen(t, nil, fixture.seed)
	before := lane.bytes(t)
	batch := productionCharacterProposalStage(t, lane, fixture.Run)
	productionCharacterProposalAssertStaged(t, batch, fixture.Run)

	// :191-192. One row, and it is a staging store: no characters row, no alias, no
	// identity revision, and no projection. The resolver found nothing to resolve
	// against an empty roster, so every item is unresolved.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var proposals, characters, aliases, revision int64
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM character_identity_proposals", nil, &proposals)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM characters", nil, &characters)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM character_aliases", nil, &aliases)
	productionStoryStateWriteScan(t, readback, "SELECT revision FROM character_identity_meta WHERE id='main'", nil, &revision)
	if proposals != 1 || characters != 0 || aliases != 0 || revision != 0 {
		t.Fatalf("a stage wrote %v proposals, %v characters, %v aliases at revision %v, want one proposal and nothing else",
			proposals, characters, aliases, revision)
	}
	var sourceKey string
	var approval, resolved sql.NullString
	productionStoryStateWriteScan(t, readback,
		"SELECT source_key,approval_id,resolved_character_id FROM character_identity_proposals WHERE proposal_id=?",
		[]any{batch.ProposalBatchID}, &sourceKey, &approval, &resolved)
	if !strings.HasPrefix(sourceKey, "character-proposal-v1:") || approval.Valid || resolved.Valid {
		t.Fatalf("the staged row carries %v/%v/%v, want this project's source key and no decision", sourceKey, approval, resolved)
	}
	// A stage never reached the model: the committed origin was never dialed.
	lane.assertNoDial(t)
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a stage changed the admitted database")
	}

	// :251-283. Three creations, one per slot, in the manifest's own order.
	approved := productionCharacterProposalApprove(t, lane, batch.ProposalBatchID, 0, productionCharacterProposalApproveID,
		[]any{
			productionCharacterProposalCreate(productionCharacterProposalSlotOne),
			productionCharacterProposalCreate(productionCharacterProposalSlotTwo),
			productionCharacterProposalCreate(productionCharacterProposalSlotThree),
		})
	if approved.Batch.Status != productionCharacterProposalStatusApproved ||
		approved.Batch.ApprovalOperationID != productionCharacterProposalApproveID ||
		approved.Batch.Revision != 1 {
		t.Fatalf("the approval answered %v/%v at revision %v, want an approved batch under its operation id at revision 1: %v",
			approved.Batch.Status, approved.Batch.ApprovalOperationID, approved.Batch.Revision, approved.Batch)
	}
	if len(approved.Created) != 3 {
		t.Fatalf("the approval created %v identities, want one per slot: %v", len(approved.Created), approved.Created)
	}
	for index, slot := range []string{productionCharacterProposalSlotOne, productionCharacterProposalSlotTwo,
		productionCharacterProposalSlotThree} {
		if approved.Created[index].SelectionKey != slot || approved.Created[index].CharacterID == "" {
			t.Fatalf("created[%v] = %v, want the identity of %v", index, approved.Created[index], slot)
		}
	}

	// The write is on disk, read over a connection that could not have made it. A
	// creation carries the eleven static fields and nothing else: the current state is
	// the chapter-summary lane's to write, so it stays blank here.
	readback = productionStoryStateWriteReadback(t, lane.projectPath)
	var appearance, location, name, role string
	productionStoryStateWriteScan(t, readback,
		"SELECT appearance,cs_location,name,role FROM characters WHERE character_id=?",
		[]any{approved.Created[0].CharacterID}, &appearance, &location, &name, &role)
	if appearance != productionCharacterProposalAppearance || name != productionCharacterProposalNameOne || role != "protagonist" {
		t.Fatalf("the created identity is %v/%v/%v, want the parser's own name, role and appearance",
			name, role, appearance)
	}
	if location != "" {
		t.Fatalf("the created identity carries the state %v, want a creation of static fields only", location)
	}
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM characters WHERE retired=0", nil, &characters)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM character_aliases", nil, &aliases)
	productionStoryStateWriteScan(t, readback, "SELECT revision FROM character_identity_meta WHERE id='main'", nil, &revision)
	if characters != 3 || aliases != 3 || revision != 1 {
		t.Fatalf("the approval left %v characters, %v aliases at revision %v, want three of each at revision 1",
			characters, aliases, revision)
	}
	// :311-320. The envelope records the decision and its created identities, and the
	// projection was refreshed, so the roster is no longer empty.
	var stored, state string
	var proposalApproval sql.NullString
	productionStoryStateWriteScan(t, readback,
		"SELECT raw_value,approval_id FROM character_identity_proposals WHERE proposal_id=?",
		[]any{batch.ProposalBatchID}, &stored, &proposalApproval)
	// The reference's writer only ever updates the envelope and its hash (:41-44), so
	// the decision is recorded inside raw_value and the legacy approval_id column is
	// left alone; the receipt that backs the envelope lives in
	// character_identity_approvals, keyed by the operation id.
	if proposalApproval.Valid {
		t.Fatalf("the proposal row carries approval_id %v, want the envelope's own record", proposalApproval)
	}
	var envelope struct {
		Batch struct {
			Status              string `json:"status"`
			ApprovalOperationID string `json:"approvalOperationId"`
		} `json:"batch"`
		Decision struct {
			OperationID string `json:"operationId"`
		} `json:"decision"`
		Created []productionCharacterProposalCreated `json:"created"`
	}
	if err := json.Unmarshal([]byte(stored), &envelope); err != nil {
		t.Fatalf("the stored envelope: %v", err)
	}
	if envelope.Batch.Status != productionCharacterProposalStatusApproved ||
		envelope.Batch.ApprovalOperationID != productionCharacterProposalApproveID ||
		envelope.Decision.OperationID != productionCharacterProposalApproveID ||
		len(envelope.Created) != 3 {
		t.Fatalf("the stored envelope is %v, want the decision and the three creations", envelope)
	}
	productionStoryStateWriteScan(t, readback,
		"SELECT migration_state FROM character_roster_meta WHERE id='main'", nil, &state)
	if state != "ready" {
		t.Fatalf("the roster is %v after the approval, want a refreshed projection", state)
	}
	// The family agrees with itself: the read reports what the approval wrote.
	var reread productionCharacterProposalBatch
	lane.author.decode(t, productionCharacterProposalReadName,
		[]any{map[string]any{productionCharacterProposalBatchKey: batch.ProposalBatchID}, lane.frozen}, &reread)
	if reread.Status != productionCharacterProposalStatusApproved ||
		reread.ApprovalOperationID != productionCharacterProposalApproveID ||
		reread.Revision != 1 {
		t.Fatalf("the read reports %v/%v at revision %v, want the approved batch", reread.Status,
			reread.ApprovalOperationID, reread.Revision)
	}
}

// testProductionCharacterProposalKeep proves a selection that keeps its slot
// unresolved commits nothing for it, and that the mixed request still commits the
// ones that create.
func testProductionCharacterProposalKeep(t *testing.T) {
	fixture := &productionCharacterProposalFixture{
		Manifest: productionCharacterProposalManifest,
		Details:  productionCharacterProposalDetails,
	}
	lane := productionGenerationWriteOpen(t, nil, fixture.seed)
	batch := productionCharacterProposalStage(t, lane, fixture.Run)
	approved := productionCharacterProposalApprove(t, lane, batch.ProposalBatchID, 0, productionCharacterProposalKeepID,
		[]any{
			productionCharacterProposalCreate(productionCharacterProposalSlotOne),
			map[string]any{"selectionKey": productionCharacterProposalSlotTwo, "action": productionCharacterProposalActionKeep},
			productionCharacterProposalCreate(productionCharacterProposalSlotThree),
		})
	if len(approved.Created) != 2 {
		t.Fatalf("the approval created %v identities, want the two slots it was asked to create: %v",
			len(approved.Created), approved.Created)
	}
	for _, created := range approved.Created {
		if created.SelectionKey == productionCharacterProposalSlotTwo {
			t.Fatalf("a kept slot was committed: %v", created)
		}
	}
	var characters int64
	productionStoryStateWriteScan(t, productionStoryStateWriteReadback(t, lane.projectPath),
		"SELECT COUNT(*) FROM characters WHERE retired=0", nil, &characters)
	if characters != 2 {
		t.Fatalf("the roster holds %v identities, want only the two created", characters)
	}
}

// testProductionCharacterProposalMap proves the adoption arm: a `map` selection onto
// an identity the roster already knows adopts the fields the author has not written,
// and leaves the author's own values alone.
func testProductionCharacterProposalMap(t *testing.T) {
	fixture := &productionCharacterProposalFixture{
		Manifest: productionCharacterProposalManifest,
		Details:  productionCharacterProposalDetails,
		Existing: true,
	}
	lane := productionGenerationWriteOpen(t, nil, fixture.seed)
	batch := productionCharacterProposalStage(t, lane, fixture.Run)
	// The alias names the protagonist, but not under the source key this batch is
	// staged from, so the resolution carries the candidate and stays unresolved - the
	// reference's own shape for a single name match (:127-130).
	if len(batch.Items) != 3 {
		t.Fatalf("the batch carries %v items, want three", len(batch.Items))
	}
	if batch.Items[0].Resolution.Status != "unresolved" ||
		len(batch.Items[0].Resolution.CandidateIDs) != 1 ||
		batch.Items[0].Resolution.CandidateIDs[0] != productionCharacterProposalExistingID {
		t.Fatalf("the protagonist resolved to %v, want the one unexpected candidate", batch.Items[0].Resolution)
	}
	approved := productionCharacterProposalApprove(t, lane, batch.ProposalBatchID, 0, productionCharacterProposalMapID,
		[]any{
			map[string]any{"selectionKey": productionCharacterProposalSlotOne, "action": productionCharacterProposalActionMap,
				"characterId": productionCharacterProposalExistingID},
			productionCharacterProposalCreate(productionCharacterProposalSlotTwo),
			map[string]any{"selectionKey": productionCharacterProposalSlotThree, "action": productionCharacterProposalActionKeep},
		})
	if len(approved.Created) != 1 || approved.Created[0].SelectionKey != productionCharacterProposalSlotTwo {
		t.Fatalf("the approval created %v, want only the second slot: %v", len(approved.Created), approved.Created)
	}
	// :259-267. The adopted fields are the proposed ones the author never wrote, and
	// the identity keeps its id: the row is updated, not created.
	var appearance, location, name string
	productionStoryStateWriteScan(t, productionStoryStateWriteReadback(t, lane.projectPath),
		"SELECT appearance,cs_location,name FROM characters WHERE character_id=?",
		[]any{productionCharacterProposalExistingID}, &appearance, &location, &name)
	if name != productionCharacterProposalNameOne || appearance != productionCharacterProposalAppearance {
		t.Fatalf("the mapped identity is %v/%v, want the adopted appearance on the seeded name", name, appearance)
	}
	// The current state belongs to the detail document, not to the eleven static
	// fields, so the commit does not carry it.
	if location != "" {
		t.Fatalf("the mapped identity holds the state %v, want it untouched: a change carries static fields only", location)
	}
	var characters int64
	productionStoryStateWriteScan(t, productionStoryStateWriteReadback(t, lane.projectPath),
		"SELECT COUNT(*) FROM characters WHERE retired=0", nil, &characters)
	if characters != 2 {
		t.Fatalf("the roster holds %v identities, want the seeded one and the one created", characters)
	}
}

// testProductionCharacterProposalUnknownBatch proves an approval of a batch that was
// never staged is refused by name and writes nothing.
func testProductionCharacterProposalUnknownBatch(t *testing.T) {
	fixture := &productionCharacterProposalFixture{
		Manifest: productionCharacterProposalManifest,
		Details:  productionCharacterProposalDetails,
	}
	lane := productionGenerationWriteOpen(t, nil, fixture.seed)
	before := lane.bytes(t)
	unknown := "cpb:" + strings.Repeat("0", 64)
	response := lane.author.call(t, productionCharacterProposalApproveName,
		[]any{map[string]any{
			productionCharacterProposalBatchKey:    unknown,
			productionCharacterProposalRevisionKey: 0,
			productionCharacterProposalOperationKey: productionCharacterProposalApproveID,
			productionCharacterProposalSelectionsKey: []any{
				productionCharacterProposalCreate(productionCharacterProposalSlotOne),
			},
		}, lane.frozen})
	productionFinalizedCharacterRefusal(t, "approve of an unknown batch", response, "CHARACTER_PROPOSAL_RECEIPT_INVALID")
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused approval changed the admitted database")
	}
	// The same batch is refused by read and cancel too: an unstaged id is not a batch
	// this project can be told about.
	response = lane.author.call(t, productionCharacterProposalReadName,
		[]any{map[string]any{productionCharacterProposalBatchKey: unknown}, lane.frozen})
	productionFinalizedCharacterRefusal(t, "read of an unknown batch", response, "CHARACTER_PROPOSAL_RECEIPT_INVALID")
	response = lane.author.call(t, productionCharacterProposalCancelName,
		[]any{map[string]any{productionCharacterProposalBatchKey: unknown,
			productionCharacterProposalRevisionKey: 0}, lane.frozen})
	productionFinalizedCharacterRefusal(t, "cancel of an unknown batch", response, "CHARACTER_PROPOSAL_RECEIPT_INVALID")
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused family call changed the admitted database")
	}
}

// testProductionCharacterProposalMalformed proves the model's answer is parsed, not
// stored: a manifest outside the parser's shape, a detail document with no entries
// and a detail that disagrees with its slot are all refused by name, and none of
// them writes a row.
func testProductionCharacterProposalMalformed(t *testing.T) {
	for _, arm := range []struct {
		label    string
		manifest string
		details  string
		want     string
	}{
		{
			// :65. One slot is fewer than the three the manifest must declare, and the
			// parser's own refusal is prose, which the controller answers by falling
			// back to the unmapped-failure code.
			label:    "a manifest with too few slots",
			manifest: `{"slots": [{"slotId": "s1", "name": "沉舟", "role": "protagonist", "narrativeDuty": "守门", "relations": []}]}`,
			details:  productionCharacterProposalDetails,
			want:     productionCharacterProposalRequestFailed,
		},
		{
			// :26. The symbolic code the reference publishes itself passes through.
			label:    "a detail document with no entries",
			manifest: productionCharacterProposalManifest,
			details:  `{"entries": []}`,
			want:     "CHARACTER_PROPOSAL_EMPTY_DETAILS",
		},
		{
			// :31. The detail's role disagrees with the slot it claims, which is the
			// parser's own mismatch refusal.
			label:    "a detail that renames its slot's role",
			manifest: productionCharacterProposalManifest,
			details: strings.Replace(productionCharacterProposalDetails,
				`{"slotId": "s3", "name": "程守", "role": "antagonist"`,
				`{"slotId": "s3", "name": "程守", "role": "supporting"`, 1),
			want: "CHARACTER_PROPOSAL_SLOT_MISMATCH",
		},
		{
			// :35. A key the detail's closed set does not carry is refused, which is what
			// keeps the model from widening the stored row.
			label:    "a detail with an extra key",
			manifest: productionCharacterProposalManifest,
			details: strings.Replace(productionCharacterProposalDetails,
				`{"slotId": "s2", "name": "白露"`,
				`{"slotId": "s2", "secret": "x", "name": "白露"`, 1),
			want: "CHARACTER_PROPOSAL_DETAIL_FIELD_INVALID",
		},
	} {
		t.Run(arm.label, func(t *testing.T) {
			fixture := &productionCharacterProposalFixture{Manifest: arm.manifest, Details: arm.details}
			lane := productionGenerationWriteOpen(t, nil, fixture.seed)
			before := lane.bytes(t)
			response := lane.author.call(t, productionCharacterProposalStageName,
				[]any{map[string]any{productionCharacterProposalSourceKey: productionCharacterProposalSourceBody(fixture.Run)}, lane.frozen})
			productionFinalizedCharacterRefusal(t, "stage of "+arm.label, response, arm.want)
			if after := lane.bytes(t); !bytes.Equal(before, after) {
				t.Fatal("a refused stage changed the admitted database")
			}
			var proposals, characters int64
			readback := productionStoryStateWriteReadback(t, lane.projectPath)
			productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM character_identity_proposals", nil, &proposals)
			productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM characters", nil, &characters)
			if proposals != 0 || characters != 0 {
				t.Fatalf("a refused stage wrote %v proposals and %v characters, want nothing", proposals, characters)
			}
		})
	}
}

// testProductionCharacterProposalReplay proves the answer a repeated approval gives:
// the same decision reproduces the stored reference without a second write, and a
// different operation id is a nonce conflict rather than a second commit.
func testProductionCharacterProposalReplay(t *testing.T) {
	fixture := &productionCharacterProposalFixture{
		Manifest: productionCharacterProposalManifest,
		Details:  productionCharacterProposalDetails,
	}
	lane := productionGenerationWriteOpen(t, nil, fixture.seed)
	batch := productionCharacterProposalStage(t, lane, fixture.Run)
	selections := []any{
		productionCharacterProposalCreate(productionCharacterProposalSlotOne),
		productionCharacterProposalCreate(productionCharacterProposalSlotTwo),
		productionCharacterProposalCreate(productionCharacterProposalSlotThree),
	}
	first := productionCharacterProposalApprove(t, lane, batch.ProposalBatchID, 0, productionCharacterProposalApproveID, selections)
	settled := lane.bytes(t)
	// :232-235. The same request body answers the stored created list, byte for byte,
	// and writes nothing: the envelope, the revision and the roster are already there.
	replay := productionCharacterProposalApprove(t, lane, batch.ProposalBatchID, 0, productionCharacterProposalReplayID, selections)
	if len(replay.Created) != len(first.Created) {
		t.Fatalf("the replay created %v identities, want the first approval's %v: %v",
			len(replay.Created), len(first.Created), replay.Created)
	}
	for index := range first.Created {
		if replay.Created[index] != first.Created[index] {
			t.Fatalf("replay created[%v] = %v, want %v", index, replay.Created[index], first.Created[index])
		}
	}
	if replay.Batch.Revision != first.Batch.Revision || replay.Batch.ApprovalOperationID != first.Batch.ApprovalOperationID {
		t.Fatalf("the replay answered %v at revision %v, want the stored envelope %v at %v",
			replay.Batch.ApprovalOperationID, replay.Batch.Revision, first.Batch.ApprovalOperationID, first.Batch.Revision)
	}
	if after := lane.bytes(t); !bytes.Equal(settled, after) {
		t.Fatal("a replayed approval changed the admitted database")
	}
	// :233. Another operation id is another decision, so it can never be the same
	// approval: the reference refuses it and the commit would refuse it too.
	response := lane.author.call(t, productionCharacterProposalApproveName,
		[]any{map[string]any{
			productionCharacterProposalBatchKey:      batch.ProposalBatchID,
			productionCharacterProposalRevisionKey:   0,
			productionCharacterProposalOperationKey:  productionCharacterProposalSecondID,
			productionCharacterProposalSelectionsKey: selections,
		}, lane.frozen})
	productionFinalizedCharacterRefusal(t, "approve under a second operation id", response, "CHARACTER_APPROVAL_NONCE_CONFLICT")
	if after := lane.bytes(t); !bytes.Equal(settled, after) {
		t.Fatal("a conflicting approval changed the admitted database")
	}
	// :203. Cancelling cannot undo a commit that already landed, so an approved batch
	// is refused rather than reverted.
	response = lane.author.call(t, productionCharacterProposalCancelName,
		[]any{map[string]any{
			productionCharacterProposalBatchKey:    batch.ProposalBatchID,
			productionCharacterProposalRevisionKey: 1,
		}, lane.frozen})
	productionFinalizedCharacterRefusal(t, "cancel of an approved batch", response, "CHARACTER_PROPOSAL_ALREADY_APPROVED")
	if after := lane.bytes(t); !bytes.Equal(settled, after) {
		t.Fatal("a refused cancel changed the admitted database")
	}
}

// testProductionCharacterProposalCancel proves the cancel arm: a cancelled batch can
// no longer be approved, and cancelling it again is the reference's own no-op.
func testProductionCharacterProposalCancel(t *testing.T) {
	fixture := &productionCharacterProposalFixture{
		Manifest: productionCharacterProposalManifest,
		Details:  productionCharacterProposalDetails,
	}
	lane := productionGenerationWriteOpen(t, nil, fixture.seed)
	batch := productionCharacterProposalStage(t, lane, fixture.Run)
	cancelled := productionCharacterProposalCancel(t, lane, batch.ProposalBatchID, 0)
	if cancelled.Status != productionCharacterProposalStatusCancelled || cancelled.ApprovalOperationID != "" {
		t.Fatalf("cancel answered %v/%v, want a cancelled batch with no approval", cancelled.Status,
			cancelled.ApprovalOperationID)
	}
	settled := lane.bytes(t)
	// :204. Cancelling a cancelled batch answers the same batch and writes nothing.
	if again := productionCharacterProposalCancel(t, lane, batch.ProposalBatchID, 0); again.Status != productionCharacterProposalStatusCancelled {
		t.Fatalf("a repeated cancel answered %v, want the same cancelled batch", again.Status)
	}
	if after := lane.bytes(t); !bytes.Equal(settled, after) {
		t.Fatal("a replayed cancel changed the admitted database")
	}
	// :236. A cancelled batch can never be approved: the decision is final, and the
	// refusal lands before the provenance or the selections are read.
	response := lane.author.call(t, productionCharacterProposalApproveName,
		[]any{map[string]any{
			productionCharacterProposalBatchKey:      batch.ProposalBatchID,
			productionCharacterProposalRevisionKey:   0,
			productionCharacterProposalOperationKey:  productionCharacterProposalSecondID,
			productionCharacterProposalSelectionsKey: []any{productionCharacterProposalCreate(productionCharacterProposalSlotOne)},
		}, lane.frozen})
	productionFinalizedCharacterRefusal(t, "approve of a cancelled batch", response, "CHARACTER_PROPOSAL_CANCELLED")
	if after := lane.bytes(t); !bytes.Equal(settled, after) {
		t.Fatal("a refused approval of a cancelled batch changed the admitted database")
	}
	var characters, proposals int64
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM characters", nil, &characters)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM character_identity_proposals", nil, &proposals)
	if characters != 0 || proposals != 1 {
		t.Fatalf("a cancelled batch left %v characters beside %v proposals, want none of the former", characters, proposals)
	}
}

// testProductionCharacterProposalSourceKinds proves every source this lane cannot
// prove is refused before anything is read or written, by the code the reference
// publishes for it.
func testProductionCharacterProposalSourceKinds(t *testing.T) {
	fixture := &productionCharacterProposalFixture{
		Manifest: productionCharacterProposalManifest,
		Details:  productionCharacterProposalDetails,
	}
	lane := productionGenerationWriteOpen(t, nil, fixture.seed)
	before := lane.bytes(t)
	served := productionCharacterProposalSourceBody(fixture.Run)
	handle := map[string]any{
		"projectId":    served["handle"].(map[string]any)["projectId"],
		"epoch":        productionCharacterProposalEpoch,
		"rootActionId": fixture.Run.RootActionID,
		"runId":        fixture.Run.RunID,
	}
	for _, arm := range []struct {
		label string
		body  any
		want  string
	}{
		{
			// generation-controller.ts:209. The legacy roster arm is refused by the
			// controller before the service sees it, so the wire code is the
			// controller's own.
			label: "a legacy roster source",
			body:  map[string]any{productionCharacterProposalSourceKey: map[string]any{"kind": productionCharacterProposalLegacyKind}},
			want:  "GENERATION_LEGACY_ADMISSION_REQUIRED",
		},
		{
			// generation-character-proposal-proof.ts:67-75. The import receipt has no Go
			// reader, so the port does not exist and the source is refused by name.
			label: "an import source",
			body:  map[string]any{productionCharacterProposalSourceKey: map[string]any{"kind": "import"}},
			want:  "CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE",
		},
		{
			// finalized-character-generation-proof.ts:52-65, likewise absent.
			label: "a finalized-generation source",
			body:  map[string]any{productionCharacterProposalSourceKey: map[string]any{"kind": productionCharacterProposalFinalizedKind}},
			want:  "CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE",
		},
		{
			// The planning-material arm belongs to a different parser this lane does
			// not port.
			label: "a planning-material source",
			body: map[string]any{productionCharacterProposalSourceKey: map[string]any{
				"kind": productionCharacterProposalGenerationKind, "inputKind": "planning-material", "handle": handle,
			}},
			want: "CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE",
		},
		{
			// :46. A generation source without its handle is not a source at all.
			label: "a generation source without a handle",
			body: map[string]any{productionCharacterProposalSourceKey: map[string]any{
				"kind": productionCharacterProposalGenerationKind, "inputKind": productionCharacterProposalArchitectureKind,
			}},
			want: "CHARACTER_PROPOSAL_SOURCE_INVALID",
		},
		{
			// :159. A handle naming another project is not this run's handle.
			label: "a handle naming another project",
			body: map[string]any{productionCharacterProposalSourceKey: map[string]any{
				"kind": productionCharacterProposalGenerationKind, "inputKind": productionCharacterProposalArchitectureKind,
				"handle": map[string]any{"projectId": "6a1f4b2c-0000-4000-8000-000000000000",
					"epoch": productionCharacterProposalEpoch, "rootActionId": fixture.Run.RootActionID,
					"runId": fixture.Run.RunID},
				"artifacts": served["artifacts"], "manifestArtifactId": fixture.Run.Manifest.ArtifactID,
			}},
			want: "GENERATION_RUN_IDENTITY_MISMATCH",
		},
		{
			// :144-151. An artifact id this run never settled cannot be proved.
			label: "an artifact of another run",
			body: map[string]any{productionCharacterProposalSourceKey: map[string]any{
				"kind": productionCharacterProposalGenerationKind, "inputKind": productionCharacterProposalArchitectureKind,
				"handle": handle, "manifestArtifactId": fixture.Run.Manifest.ArtifactID,
				"artifacts": []any{map[string]any{"artifactId": "ci-artifact-absent",
					"revision": fixture.Run.Manifest.Revision, "textHash": fixture.Run.Manifest.TextHash}},
			}},
			want: "CHARACTER_PROPOSAL_ARTIFACT_INVALID",
		},
		{
			// :208. The body is `{ source }` and nothing else.
			label: "a body without a source",
			body:  map[string]any{},
			want:  "CHARACTER_PROPOSAL_SOURCE_INVALID",
		},
	} {
		t.Run(arm.label, func(t *testing.T) {
			response := lane.author.call(t, productionCharacterProposalStageName, []any{arm.body, lane.frozen})
			productionFinalizedCharacterRefusal(t, "stage of "+arm.label, response, arm.want)
		})
	}
	// The wire arity is the request arity plus the frozen context
	// (generation-controller.ts:208-216).
	response := lane.author.call(t, productionCharacterProposalStageName, []any{served})
	productionFinalizedCharacterRefusal(t, "stage without a frozen context", response, "INVALID_ARGUMENTS")
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused source changed the admitted database")
	}
	// The fixture is not what refused: the same handle with the source kind it serves
	// stages one proposal.
	batch := productionCharacterProposalStage(t, lane, fixture.Run)
	if batch.Status != productionCharacterProposalStatusPending {
		t.Fatalf("the fixture's own source staged %v, want a pending batch", batch.Status)
	}
}

// testProductionCharacterProposalStaleLease proves no name in this family answers
// without a live project lease, and that a context the server cannot prove writes
// nothing. The exact code is left open: the context parses - it names a project, a
// path and a lease - and it is the lease proof, not the shape, that refuses it.
func testProductionCharacterProposalStaleLease(t *testing.T) {
	fixture := &productionCharacterProposalFixture{
		Manifest: productionCharacterProposalManifest,
		Details:  productionCharacterProposalDetails,
	}
	lane := productionGenerationWriteOpen(t, nil, fixture.seed)
	batch := productionCharacterProposalStage(t, lane, fixture.Run)
	before := lane.bytes(t)
	body := map[string]any{
		productionCharacterProposalBatchKey:      batch.ProposalBatchID,
		productionCharacterProposalRevisionKey:   0,
		productionCharacterProposalOperationKey:  productionCharacterProposalApproveID,
		productionCharacterProposalSelectionsKey: []any{productionCharacterProposalCreate(productionCharacterProposalSlotOne)},
	}
	for _, operation := range []string{
		productionCharacterProposalStageName,
		productionCharacterProposalReadName,
		productionCharacterProposalApproveName,
		productionCharacterProposalCancelName,
	} {
		response := lane.author.call(t, operation, []any{body, lane.stale()})
		productionFinalizedCharacterRefusal(t, operation+" without a live lease", response, "")
	}
	if after := lane.bytes(t); !bytes.Equal(before, after) {
		t.Fatal("a refused call without a live lease changed the admitted database")
	}
	// The batch is untouched: still pending at the revision it was staged at.
	var reread productionCharacterProposalBatch
	lane.author.decode(t, productionCharacterProposalReadName,
		[]any{map[string]any{productionCharacterProposalBatchKey: batch.ProposalBatchID}, lane.frozen}, &reread)
	if reread.Status != productionCharacterProposalStatusPending || reread.Revision != 0 {
		t.Fatalf("the batch is %v at revision %v, want it untouched", reread.Status, reread.Revision)
	}
}

// productionCharacterProposalStage stages one source over the frozen session.
func productionCharacterProposalStage(t *testing.T, lane *productionGenerationWriteLane, run productionCharacterProposalRun) productionCharacterProposalBatch {
	t.Helper()
	var batch productionCharacterProposalBatch
	lane.author.decode(t, productionCharacterProposalStageName,
		[]any{map[string]any{productionCharacterProposalSourceKey: productionCharacterProposalSourceBody(run)}, lane.frozen}, &batch)
	return batch
}

// productionCharacterProposalAssertStaged checks the parsed batch: one item per slot,
// in the manifest's own order, each carrying the detail's fields and the artifact the
// parser attributed it to.
func productionCharacterProposalAssertStaged(t *testing.T, batch productionCharacterProposalBatch, run productionCharacterProposalRun) {
	t.Helper()
	if batch.Status != productionCharacterProposalStatusPending || batch.Revision != 0 || batch.ApprovalOperationID != "" {
		t.Fatalf("the staged batch is %v at revision %v under %v, want a pending batch at revision 0",
			batch.Status, batch.Revision, batch.ApprovalOperationID)
	}
	if batch.ProposalBatchID == "" || !strings.HasPrefix(batch.ProposalBatchID, "cpb:") {
		t.Fatalf("the staged batch is named %v, want the derived cpb: identity", batch.ProposalBatchID)
	}
	if batch.Source == nil || batch.Source.ManifestArtifactID != run.Manifest.ArtifactID ||
		len(batch.Source.Artifacts) != 2 {
		t.Fatalf("the staged source is %v, want the manifest and its one detail document", batch.Source)
	}
	if len(batch.Items) != 3 {
		t.Fatalf("the staged batch carries %v items, want three", len(batch.Items))
	}
	for index, want := range []struct {
		slot string
		name string
		role string
	}{
		{productionCharacterProposalSlotOne, productionCharacterProposalNameOne, "protagonist"},
		{productionCharacterProposalSlotTwo, productionCharacterProposalNameTwo, "supporting"},
		{productionCharacterProposalSlotThree, productionCharacterProposalNameThree, "antagonist"},
	} {
		item := batch.Items[index]
		if item.SelectionKey != want.slot || item.Fields.Name != want.name || item.Fields.Role != want.role {
			t.Fatalf("items[%v] = %v/%v/%v, want %v/%v/%v", index, item.SelectionKey, item.Fields.Name,
				item.Fields.Role, want.slot, want.name, want.role)
		}
		// The source id is the detail artifact and the slot the entry claimed
		// (character-proposal-parser.ts:38-40).
		if item.SourceID != run.Detail.ArtifactID+":"+want.slot {
			t.Fatalf("items[%v].sourceId = %v, want %v", index, item.SourceID, run.Detail.ArtifactID+":"+want.slot)
		}
		if item.Resolution.Status != "unresolved" {
			t.Fatalf("items[%v] resolved to %v against an empty roster, want unresolved", index, item.Resolution)
		}
		// rawValue.detail is the entry the parser read, state included: the eleven
		// static fields are what a commit writes, and this is the evidence behind them.
		if !strings.Contains(string(item.RawValue.Detail), productionCharacterProposalLocation) {
			t.Fatalf("items[%v].rawValue.detail does not carry the state the answer wrote: %s",
				index, item.RawValue.Detail)
		}
	}
}

// productionCharacterProposalApprove approves one staged batch over the frozen
// session and answers the batch with its created identities.
func productionCharacterProposalApprove(t *testing.T, lane *productionGenerationWriteLane, batchID string,
	revision int64, operationID string, selections []any) productionCharacterProposalApproval {
	t.Helper()
	var approval productionCharacterProposalApproval
	lane.author.decode(t, productionCharacterProposalApproveName, []any{map[string]any{
		productionCharacterProposalBatchKey:      batchID,
		productionCharacterProposalRevisionKey:   revision,
		productionCharacterProposalOperationKey:  operationID,
		productionCharacterProposalSelectionsKey: selections,
	}, lane.frozen}, &approval)
	return approval
}

// productionCharacterProposalCancel cancels one staged batch over the frozen session.
func productionCharacterProposalCancel(t *testing.T, lane *productionGenerationWriteLane, batchID string, revision int64) productionCharacterProposalBatch {
	t.Helper()
	var batch productionCharacterProposalBatch
	lane.author.decode(t, productionCharacterProposalCancelName, []any{map[string]any{
		productionCharacterProposalBatchKey:    batchID,
		productionCharacterProposalRevisionKey: revision,
	}, lane.frozen}, &batch)
	return batch
}

// productionCharacterProposalApproval is approve's answer: the batch and the
// identities the commit created.
type productionCharacterProposalApproval struct {
	Batch   productionCharacterProposalBatch     `json:"batch"`
	Created []productionCharacterProposalCreated `json:"created"`
}

// productionCharacterProposalCreate is one create selection.
func productionCharacterProposalCreate(selectionKey string) map[string]any {
	return map[string]any{"selectionKey": selectionKey, "action": productionCharacterProposalActionCreate}
}

// productionCharacterProposalSourceBody builds the source a staged batch is read from:
// the frozen handle, the two settled artifacts, and the manifest among them.
func productionCharacterProposalSourceBody(run productionCharacterProposalRun) map[string]any {
	return map[string]any{
		"kind":      productionCharacterProposalGenerationKind,
		"inputKind": productionCharacterProposalArchitectureKind,
		"handle": map[string]any{
			"projectId":    productionEditorProjectID,
			"epoch":        run.Epoch,
			"rootActionId": run.RootActionID,
			"runId":        run.RunID,
		},
		"artifacts": []any{
			map[string]any{"artifactId": run.Manifest.ArtifactID, "revision": run.Manifest.Revision,
				"textHash": run.Manifest.TextHash},
			map[string]any{"artifactId": run.Detail.ArtifactID, "revision": run.Detail.Revision,
				"textHash": run.Detail.TextHash},
		},
		"manifestArtifactId": run.Manifest.ArtifactID,
	}
}

// productionCharacterProposalSeed writes the fixture's project state and its two
// settled model answers through the shipped ledger, then folds the write-ahead log
// into the file the lane is about to admit.
func productionCharacterProposalSeed(t *testing.T, projectPath, manifest, details string, existing bool) productionCharacterProposalRun {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	// The baseline leaves the empty roster row for the lane's own migration, so the
	// fixture writes it: an empty roster is one that holds no character, no legacy
	// markdown and no projection.
	statements := []string{
		"DELETE FROM character_identity_proposals",
		"DELETE FROM character_relationships",
		"DELETE FROM character_identity_approvals",
		"DELETE FROM character_identity_origins",
		"DELETE FROM character_aliases",
		"DELETE FROM characters",
		"DELETE FROM character_roster_operations",
		"UPDATE project_core SET characters_arch='', character_states='{}' WHERE id='main'",
		`INSERT OR REPLACE INTO character_roster_meta
 (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'empty', '', '', '')`,
		`INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)`,
	}
	if existing {
		// The identity a `map` selection adopts: a name the roster already knows under
		// an alias, and no field values at all, so the proposal is allowed to fill them
		// in.
		statements = append(statements,
			`INSERT INTO characters (character_id, name, role, retired, static_provenance)
 VALUES ('`+productionCharacterProposalExistingID+`', '`+productionCharacterProposalNameOne+`', 'protagonist', 0, '{}')`,
			`INSERT INTO character_aliases (character_id, name, source_key, valid_from)
 VALUES ('`+productionCharacterProposalExistingID+`', '`+productionCharacterProposalNameOne+`', '`+
				productionCharacterProposalExistingSourceKey+`', 0)`)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed character proposals: %v", err)
		}
	}
	if existing {
		// The projection is the roster's own rendering: without it the seeded row makes
		// the roster inconsistent, and approve would have to refuse it before the
		// selection it is testing.
		if err := store.NewCharacters(db).RefreshIdentityProjection(context.Background()); err != nil {
			_ = db.Close()
			t.Fatalf("seed projection: %v", err)
		}
	}
	run := productionCharacterProposalSeedRun(t, db, manifest, details)
	productionEditorCheckpoint(t, db, database)
	return run
}

// productionCharacterProposalSeedRun settles the fixture's run through the real
// ledger: one architecture run with a manifest answer and one detail answer, both
// settled and both read back from the receipt the ledger wrote.
func productionCharacterProposalSeedRun(t *testing.T, db *sql.DB, manifest, details string) productionCharacterProposalRun {
	t.Helper()
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return fmt.Sprintf("production-character-proposal-%02d", ordinal)
	})
	ctx := context.Background()
	value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+productionCharacterProposalRunRequest()+"]"))
	if err != nil {
		t.Fatalf("seed run open: %v", err)
	}
	run, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("seed run open returned %T", value)
	}
	runID, _ := run["runId"].(string)
	rootID, _ := run["rootActionId"].(string)
	if runID == "" || rootID == "" {
		t.Fatalf("seed run open returned no identity: %v", run)
	}
	return productionCharacterProposalRun{
		RunID:        runID,
		RootActionID: rootID,
		Epoch:        productionCharacterProposalEpoch,
		Manifest:     productionCharacterProposalSeedArtifact(t, ledger, runID, productionCharacterProposalManifestNonce, manifest),
		Detail:       productionCharacterProposalSeedArtifact(t, ledger, runID, productionCharacterProposalDetailNonce, details),
	}
}

// productionCharacterProposalSeedArtifact settles one answer and reads back the
// artifact the ledger stored for it, so the source references what the ledger wrote
// rather than what the test expected.
func productionCharacterProposalSeedArtifact(t *testing.T, ledger *store.GenerationLedger, runID, nonce, text string) productionCharacterProposalArtifact {
	t.Helper()
	ctx := context.Background()
	reserved, err := ledger.Call(ctx, "reserve", []any{runID, nonce, strings.Repeat("b", 64), 200, 100})
	if err != nil {
		t.Fatalf("seed artifact reserve: %v", err)
	}
	receipt, _ := reserved.(map[string]any)
	attempt, _ := receipt["attempt"].(map[string]any)
	attemptID, _ := attempt["attemptId"].(string)
	if attemptID == "" {
		t.Fatalf("seed artifact reserve returned no attempt: %v", reserved)
	}
	if _, err := ledger.Call(ctx, "markDispatched", []any{attemptID}); err != nil {
		t.Fatalf("seed artifact markDispatched: %v", err)
	}
	if _, err := ledger.Call(ctx, "snapshot", []any{attemptID, 0, text}); err != nil {
		t.Fatalf("seed artifact snapshot: %v", err)
	}
	if _, err := ledger.Call(ctx, "settle", []any{attemptID, map[string]any{"trusted": true, "actualTokens": 120}, "stop"}); err != nil {
		t.Fatalf("seed artifact settle: %v", err)
	}
	value, err := ledger.Call(ctx, "receipt", []any{attemptID})
	if err != nil {
		t.Fatalf("seed artifact receipt: %v", err)
	}
	frozen, _ := value.(map[string]any)
	artifact, _ := frozen["artifact"].(map[string]any)
	artifactID, _ := artifact["artifactId"].(string)
	textHash, _ := artifact["textHash"].(string)
	revision, ok := productionCharacterProposalInteger(artifact["revision"])
	if artifactID == "" || textHash == "" || !ok {
		t.Fatalf("seed artifact receipt carries %v, want an identity, a hash and a revision", artifact)
	}
	return productionCharacterProposalArtifact{ArtifactID: artifactID, Revision: revision, TextHash: textHash}
}

// productionCharacterProposalRunRequest is the frozen run the fixture settles: the
// editor's own binding, with the architecture step as its source manifest so the
// proposal proof reads the operation it requires
// (generation-character-proposal-proof.ts:155).
func productionCharacterProposalRunRequest() string {
	hash := strings.Repeat("a", 64)
	return `{"projectId":"` + productionEditorProjectID + `","epoch":"` + productionCharacterProposalEpoch +
		`","operation":"` + productionEditorGenerationOperation + `","uiActionNonce":"` +
		productionCharacterProposalRunNonce + `","frozenInputHash":"` + hash + `","fingerprint":{"chapterBriefHash":"` +
		hash + `","authorGuidanceHash":"` + hash + `","dependencyHash":"` + hash + `","contextSnapshotHash":"` + hash +
		`","templateHash":"` + hash + `","skillSnapshotHash":"` + hash + `","modelLeaseRevision":"` + hash +
		`","policyHash":"` + hash + `","outputContractHash":"` + hash + `"},"contextSnapshotId":` +
		`"ci-character-proposal-snapshot","sourceManifest":{"operation":"` +
		productionCharacterProposalArchitectureOperation + `","modelReceipt":{"modelRevision":"` +
		productionCharacterProposalModelRevision + `"}},"sourceRefs":[],"budget":{"maxPhysicalRequests":3,` +
		`"maxTokenLiability":4000,"maxOutputPerRequest":1000,"maxActiveElapsedMs":600000}}`
}

// productionCharacterProposalReadSourceLine reads one line of one source file, so a
// seam can be checked against the line it cites instead of against a note.
func productionCharacterProposalReadSourceLine(path string, line int) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines := strings.Split(string(raw), "\n")
	if line < 1 || line > len(lines) {
		return "", fmt.Errorf("%v has %v lines, so it cannot carry line %v", path, len(lines), line)
	}
	return strings.TrimSpace(lines[line-1]), nil
}
