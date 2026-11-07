package store

// Native port of the two `character-proposal` pending-finalized reads. They are
// the only names in that family whose envelope is not a staged architecture
// answer, so they are also the only ones whose source proof runs over kinds this
// module does not stage itself:
//
//	character-proposal:list-pending-finalized  CharacterProposalService.listPendingFinalized (:131-147)
//	character-proposal:read-pending-finalized  CharacterProposalService.readPendingFinalized (:149-154)
//
// Reference chain:
//
//	electron/services/character-proposal-service.ts
//	  :60-102  readEnvelope: the row receipt, the source proof, the provenance
//	           comparison, assertFinalizedSourceCurrent and the approval checks
//	  :103-112 resolveItems: the resolution refreshed against the live roster
//	  :131-147 listPendingFinalized: page by source_key, prove every row, keep a
//	           pending row only when its provenance carries a source
//	  :149-154 readPendingFinalized: read() plus the pending-finalized guard
//	electron/services/generation-character-proposal-proof.ts:66-105
//	  the `import` arm of proveCharacterProposal
//	electron/services/finalized-character-generation-proof.ts:13-64
//	  proveFinalizedCharacterGeneration
//	generation-character-proposal-proof.ts:52-61
//	  the `finalized-generation` arm built on it
//
// # Why this lives in the store
//
// Both names re-prove the source of every row they answer, and a row that does
// not re-prove must not be served. The proof reads the generation ledger, the
// import global-facts ledger and the finalization receipt; the projection and
// the receipt are the same two facts, so they are read on one reserved
// connection inside one BEGIN IMMEDIATE - exactly the shape
// summary.go:681 and finalized_draft_import.go:900 already use. The alternative,
// a package-main prover over *sql.DB, would take a second pool connection while
// this transaction holds the only one (projectfs opens the author database with
// SetMaxOpenConns(1)), which is a deadlock rather than a proof.
//
// # The comparison
//
// The reference compares the stored proof with a freshly computed one by value
// (isDeepStrictEqual), and stores the source hash as a string. Both halves are
// reproducible here without a TypeScript encoder:
//
//   - the hash is sha256 over JSON.stringify, and this package already emits
//     JSON.stringify's bytes for a value that was parsed with rosterJSONValue
//     (plottree.go:30-49). Hashing the ordered re-encoding of a *stored* JSON
//     document therefore reproduces the desktop's bytes even where a typed Go
//     struct would not, because the stored document already has the desktop's
//     key order.
//   - the comparison itself is order-insensitive; glSame (:212) is the
//     package's isDeepStrictEqual.
//
// # What is still refused, and why
//
// The `generated` arm of the import proof decodes the settled import-inference
// artifact (decodeImportInferenceJson, import-inference-contract.ts:257) and
// compares the decoded cards with the receipt's entries
// (generation-character-proposal-proof.ts:83-88). No Go package owns that
// decoder, so a row whose import_effect receipt carries a generationRunHandle is
// refused with CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE instead of being
// served with a guessed provenance. The lane's own import writer
// (dbmaintenance.go:671) never records that effect, so every proposal it stages
// is proven by the `provenance: null` arm and the read serves it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/characterstate"
)

// The refusal values main maps to wire codes. Every string is a code the
// reference's own service raises or this package's published port uses; none of
// them is a SQLite message, a path or a provider sentence.
var (
	// character-proposal-service.ts:62-68, :70-78 and :46/:136-142.
	ErrCharacterProposalPendingReceiptInvalid = errors.New("CHARACTER_PROPOSAL_RECEIPT_INVALID")
	ErrCharacterProposalPendingSourceChanged  = errors.New("CHARACTER_PROPOSAL_SOURCE_CHANGED")
	ErrCharacterProposalPendingSourceInvalid  = errors.New("CHARACTER_PROPOSAL_SOURCE_INVALID")
	// generation-character-proposal-proof.ts:69
	ErrCharacterProposalPendingSourceMissing = errors.New("CHARACTER_PROPOSAL_SOURCE_MISSING")
	// finalized-character-generation-proof.ts:32, :38, :52
	ErrCharacterProposalPendingContextUnproven = errors.New("CHARACTER_PROPOSAL_CONTEXT_UNPROVEN")
	// finalized-character-generation-proof.ts:41 and generation-character-proposal-proof.ts:149
	ErrCharacterProposalPendingArtifactInvalid = errors.New("CHARACTER_PROPOSAL_ARTIFACT_INVALID")
	// generation-character-proposal-proof.ts:100
	ErrCharacterProposalPendingProvenanceRequired = errors.New("CHARACTER_PROPOSAL_PROVENANCE_REQUIRED")
	// Port-only code: the proof entry point has no Go port (see the header).
	ErrCharacterProposalPendingProofUnavailable = errors.New("CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE")
	// character-proposal-service.ts:85-92
	ErrCharacterProposalPendingApprovalInvalid = errors.New("CHARACTER_PROPOSAL_APPROVAL_INVALID")
	// character-proposal-service.ts:151-153
	ErrCharacterProposalNotPendingFinalized = errors.New("CHARACTER_PROPOSAL_NOT_PENDING_FINALIZED")
	// Port-only: the reference always carries a project. A blank one is refused
	// before any row is read instead of matching a source key that cannot exist.
	ErrCharacterProposalPendingProjectRequired = errors.New("CHARACTER_PROPOSAL_PROJECT_REQUIRED")
)

const (
	// character-proposal-service.ts:175 and every writer: the source key prefix.
	pendingFinalizedSourceKeyPrefix = "character-proposal-v1:"
	// The two kinds readPendingFinalized is defined to answer (:151-153).
	pendingFinalizedFinalizedKind = "finalized-generation"
	pendingFinalizedImportKind    = "import"
	// generation-character-proposal-proof.ts:52 and :137: the only operations an
	// import or finalized-generation proposal can come from.
	pendingFinalizedStateOperation  = "finalized-character-state"
	pendingFinalizedImportOperation = "import-global-facts"
)

// PendingFinalizedProposal is one envelope that re-proved, with the projection
// the two names need. Envelope is the proven raw row, still byte for byte what
// the source hash covers; the caller decodes it and answers the batch.
type PendingFinalizedProposal struct {
	ProposalBatchID string
	Revision        int64
	Kind            string
	OperationID     string
	FinalizationID  string
	ItemCount       int
	ImportPurpose   string
	Envelope        string

	status string
	source plotObject
}

// PendingFinalized borrows an admitted author database. It opens one
// BEGIN IMMEDIATE per call on a connection it reserves, and never re-enters the
// pool from inside that transaction.
type PendingFinalized struct{ db *sql.DB }

func NewPendingFinalized(db *sql.DB) *PendingFinalized { return &PendingFinalized{db: db} }

// List is listPendingFinalized (:131-147). A row whose envelope does not
// re-prove is skipped, not listed; a row whose provenance carries no source is
// skipped too. The answer is ordered by proposal_id, the reference's ORDER BY.
func (p *PendingFinalized) List(ctx context.Context, projectID string) ([]PendingFinalizedProposal, error) {
	if p == nil || p.db == nil {
		return nil, ErrProjectNotOpen
	}
	if summaryTrim(projectID) == "" {
		return nil, ErrCharacterProposalPendingProjectRequired
	}
	proposals := []PendingFinalizedProposal{}
	err := NewReviewRevisions(p.db).immediate(ctx, func(conn *sql.Conn) error {
		rows, err := rrMaps(ctx, conn, `SELECT proposal_id FROM character_identity_proposals WHERE source_key=? ORDER BY proposal_id`,
			pendingFinalizedSourceKeyPrefix+projectID)
		if err != nil {
			return err
		}
		for _, row := range rows {
			batchID := rosterText(row["proposal_id"])
			if batchID == "" {
				continue
			}
			proposal, err := pendingFinalizedProve(ctx, conn, projectID, batchID)
			if err != nil {
				// :146 `catch { return [] }`: an unprovable row is invisible.
				continue
			}
			if proposal.status != "pending-approval" || proposal.provenanceSource() == nil {
				continue
			}
			switch proposal.Kind {
			case pendingFinalizedFinalizedKind:
				source := proposal.provenanceSource()
				proposal.FinalizationID = pfText(source, "sourceId")
			case pendingFinalizedImportKind:
				proposal.OperationID = pfText(proposal.source, "operationId")
				proposal.ImportPurpose = pendingFinalizedImportPurpose(ctx, conn, proposal.OperationID)
			default:
				// :144 the only two kinds the summary exists for.
				continue
			}
			proposals = append(proposals, proposal)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return proposals, nil
}

// Read is readPendingFinalized (:149-154): the ordinary read - envelope receipt,
// source proof, the finalized current-source assertion, and the resolution
// refresh - followed by the two-field pending-finalized guard. The refresh runs
// in this same transaction, so the batch answered is the batch written back.
func (p *PendingFinalized) Read(ctx context.Context, projectID, proposalBatchID string) (PendingFinalizedProposal, error) {
	if p == nil || p.db == nil {
		return PendingFinalizedProposal{}, ErrProjectNotOpen
	}
	if summaryTrim(projectID) == "" || summaryTrim(proposalBatchID) == "" {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	var answer PendingFinalizedProposal
	err := NewReviewRevisions(p.db).immediate(ctx, func(conn *sql.Conn) error {
		proposal, err := pendingFinalizedProve(ctx, conn, projectID, proposalBatchID)
		if err != nil {
			return err
		}
		// :151-153 status pending-approval and one of the two kinds.
		if proposal.status != "pending-approval" ||
			(proposal.Kind != pendingFinalizedFinalizedKind && proposal.Kind != pendingFinalizedImportKind) {
			return ErrCharacterProposalNotPendingFinalized
		}
		proposal, err = pendingFinalizedRefresh(ctx, conn, projectID, proposal)
		if err != nil {
			return err
		}
		answer = proposal
		return nil
	})
	if err != nil {
		return PendingFinalizedProposal{}, err
	}
	return answer, nil
}

// Prove is the writer half of these two reads: it recomputes the
// CharacterProposalProof for one source and answers it as JSON-shaped data. It
// exists so a caller that already holds an admitted database can ask the store
// what the proof of a source is, instead of re-implementing the hash. The read
// path below uses the same function, so a row staged from this answer re-proves
// byte for byte.
func (p *PendingFinalized) Prove(ctx context.Context, projectID string, source any) (map[string]any, error) {
	if p == nil || p.db == nil {
		return nil, ErrProjectNotOpen
	}
	if summaryTrim(projectID) == "" {
		return nil, ErrCharacterProposalPendingProjectRequired
	}
	encoded, err := json.Marshal(source)
	if err != nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	value, err := glParse(string(encoded))
	if err != nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	object := pfObject(value)
	if object == nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	var proof plotObject
	err = NewReviewRevisions(p.db).immediate(ctx, func(conn *sql.Conn) error {
		computed, err := pendingFinalizedProof(ctx, conn, projectID, object)
		if err != nil {
			return err
		}
		proof = computed
		return nil
	})
	if err != nil {
		return nil, err
	}
	plain, ok := glPlain(proof).(map[string]any)
	if !ok {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	return plain, nil
}

// provenanceSource is readEnvelope's `provenance && 'source' in provenance`.
func (p PendingFinalizedProposal) provenanceSource() plotObject {
	value, present := pfField(p.proof(), "provenance")
	if !present || value == nil {
		return nil
	}
	provenance := pfObject(value)
	if provenance == nil {
		return nil
	}
	source, present := pfField(provenance, "source")
	if !present || source == nil {
		return nil
	}
	return pfObject(source)
}

// proof keeps the proof beside the projection without a second SQL read: the
// envelope is what the row already carries.
func (p PendingFinalizedProposal) proof() any {
	value, err := glParse(p.Envelope)
	if err != nil {
		return nil
	}
	proof, _ := pfField(value, "proof")
	return proof
}

// pendingFinalizedProve is readEnvelope (:60-102) for the envelope-level
// receipt and the source proof. The approval arms (:81-92) are not run: both
// names refuse an approved or cancelled batch at the guard (:151-153) before the
// answer exists, and this lane writes neither authorEditProof nor approvalProof.
func pendingFinalizedProve(ctx context.Context, conn *sql.Conn, projectID, proposalBatchID string) (PendingFinalizedProposal, error) {
	rows, err := rrMaps(ctx, conn, `SELECT source_key,source_hash,raw_value FROM character_identity_proposals WHERE proposal_id=?`,
		proposalBatchID)
	if err != nil {
		return PendingFinalizedProposal{}, err
	}
	if len(rows) == 0 {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	sourceKey := rosterText(rows[0]["source_key"])
	storedHash := rosterText(rows[0]["source_hash"])
	raw := rosterText(rows[0]["raw_value"])
	// :62 the row belongs to this project and its bytes still hash to the hash
	// recorded beside them.
	if sourceKey != pendingFinalizedSourceKeyPrefix+projectID || storedHash == "" || summaryHash(raw) != storedHash {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	value, err := glParse(raw)
	if err != nil {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	envelope := pfObject(value)
	if envelope == nil {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	// :63-66 version, project and batch identity.
	version, versionOK := pfInteger(envelope, "version")
	if !versionOK || version != 1 || pfText(envelope, "projectId") != projectID {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	batchValue, present := pfField(envelope, "batch")
	batch := pfObject(batchValue)
	if !present || batch == nil || pfText(batch, "proposalBatchId") != proposalBatchID {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	sourceValue, present := pfField(batch, "source")
	source := pfObject(sourceValue)
	if !present || source == nil {
		// :60-67 proveCharacterProposal refuses an absent source.
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingSourceInvalid
	}
	// :67-68 a version-2 derivation only describes an architecture generation.
	if derivation, present := pfField(envelope, "architectureDerivationVersion"); present {
		derivationValue, derivationOK := rrInteger(derivation, -pendingFinalizedMaxSafe)
		if !derivationOK || derivationValue != 2 ||
			pfText(source, "kind") != "generation" || pfText(source, "inputKind") != "architecture" {
			return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
		}
	}
	proof, err := pendingFinalizedProof(ctx, conn, projectID, source)
	if err != nil {
		return PendingFinalizedProposal{}, err
	}
	storedProof, present := pfField(envelope, "proof")
	if !present || !glSame(storedProof, proof) {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingSourceChanged
	}
	// :87-90 this lane writes neither proof, so an envelope carrying one is not
	// served. An explicit null is the reference's falsy and is accepted.
	for _, key := range []string{"authorEditProof", "approvalProof"} {
		if value, present := pfField(envelope, key); present && phTruthy(value) {
			return PendingFinalizedProposal{}, ErrCharacterProposalPendingApprovalInvalid
		}
	}
	if pfText(source, "kind") == pendingFinalizedFinalizedKind {
		if err := pendingFinalizedCurrent(ctx, conn, projectID, source, proof); err != nil {
			return PendingFinalizedProposal{}, ErrCharacterProposalPendingSourceChanged
		}
	}
	revision, revisionOK := pfInteger(batch, "revision")
	items, itemsOK := pfArray(batch, "items")
	if !revisionOK || !itemsOK {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	return PendingFinalizedProposal{
		ProposalBatchID: proposalBatchID,
		Revision:        revision,
		Kind:            pfText(source, "kind"),
		ItemCount:       len(items),
		Envelope:        raw,
		status:          pfText(batch, "status"),
		source:          source,
	}, nil
}

// pendingFinalizedProof dispatches on the source kind. Kinds these two names are
// not defined to page answer the reference's own CHARACTER_PROPOSAL_NOT_PENDING_FINALIZED:
// a valid row of such a kind reaches exactly that code after readEnvelope, and
// the list skips it either way.
func pendingFinalizedProof(ctx context.Context, conn *sql.Conn, projectID string, source plotObject) (plotObject, error) {
	switch pfText(source, "kind") {
	case pendingFinalizedImportKind:
		return pendingFinalizedImportProof(ctx, conn, projectID, source)
	case pendingFinalizedFinalizedKind:
		return pendingFinalizedStateProof(ctx, conn, projectID, source)
	}
	return nil, ErrCharacterProposalNotPendingFinalized
}

// pendingFinalizedImportProof is generation-character-proposal-proof.ts:66-105.
// The item list, the source hash and the null-provenance arm are complete. The
// `generated` arm is refused at the decoder it needs (see the header).
func pendingFinalizedImportProof(ctx context.Context, conn *sql.Conn, projectID string, source plotObject) (plotObject, error) {
	// :67 only these two keys.
	if !pfAllow(source, "kind", "operationId") {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	operationID := pfText(source, "operationId")
	if operationID == "" {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	// :68-70 the ledger row.
	rows, err := rrMaps(ctx, conn, `SELECT payload_hash,receipt_json FROM import_global_fact_operations WHERE operation_id=?`,
		operationID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrCharacterProposalPendingSourceMissing
	}
	payloadHash := rosterText(rows[0]["payload_hash"])
	receiptValue, err := glParse(rosterText(rows[0]["receipt_json"]))
	if err != nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	receipt := pfObject(receiptValue)
	if receipt == nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	// :71-73 the receipt names its own operation and payload, and the source it
	// carries hashes back to that payload.
	proposalValue, present := pfField(receipt, "proposalSource")
	proposalSource := pfObject(proposalValue)
	if !present || proposalSource == nil ||
		pfText(receipt, "operationId") != operationID || pfText(receipt, "payloadHash") != payloadHash {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	encodedSource, err := plotJSON(proposalSource)
	if err != nil || summaryHash(encodedSource) != payloadHash {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	// :74-80 the items, in the reference's destructuring order.
	entries, entriesOK := pfArray(proposalSource, "characterEntries")
	if !entriesOK {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	items := make([]any, 0, len(entries))
	for index, value := range entries {
		entry := pfObject(value)
		if entry == nil {
			return nil, ErrCharacterProposalPendingSourceInvalid
		}
		fields := plotObject{}
		var relationshipsValue any
		relationshipsPresent := false
		for _, field := range entry {
			switch field.key {
			case "relationships":
				relationshipsValue, relationshipsPresent = field.value, true
			case "currentState", "legacyRelationshipNotes":
				// Destructured out by the reference.
			default:
				fields = append(fields, field)
			}
		}
		if !relationshipsPresent {
			return nil, ErrCharacterProposalPendingSourceInvalid
		}
		relationshipsIn, ok := relationshipsValue.([]any)
		if !ok {
			return nil, ErrCharacterProposalPendingSourceInvalid
		}
		relationships := make([]any, 0, len(relationshipsIn))
		for _, rawRelation := range relationshipsIn {
			relation := pfObject(rawRelation)
			if relation == nil {
				return nil, ErrCharacterProposalPendingSourceInvalid
			}
			built := plotObject{}
			// An absent target/relation is undefined in JavaScript and
			// JSON.stringify drops it, so a missing key stays missing here.
			if target, present := pfField(relation, "target"); present {
				built = append(built, plotField{"targetName", target})
			}
			if relationText, present := pfField(relation, "relation"); present {
				built = append(built, plotField{"relation", relationText})
			}
			built = append(built, plotField{"raw", relation})
			relationships = append(relationships, built)
		}
		items = append(items, plotObject{
			{"selectionKey", "import:" + strconv.Itoa(index)},
			{"sourceId", operationID + ":" + strconv.Itoa(index)},
			{"fields", fields},
			{"relationships", relationships},
			{"rawValue", entry},
		})
	}
	// :81 sourceHash = textHash(JSON.stringify([payload_hash, characterEntries])).
	hashInput, err := plotJSON([]any{payloadHash, entries})
	if err != nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	proof := plotObject{
		{"items", items},
		{"sourceHash", summaryHash(hashInput)},
	}
	// :82-86 exactly one effect row carries the run handle, or there is none.
	effects, err := rrMaps(ctx, conn, `SELECT run_id,batch_id,state,payload_json,payload_hash FROM import_run_receipts
 WHERE stage='global' AND kind='project-global-facts' AND json_extract(payload_json,'$.operationId')=?`, operationID)
	if err != nil {
		return nil, err
	}
	if len(effects) != 1 {
		return append(proof, plotField{"provenance", nil}), nil
	}
	effect := effects[0]
	payloadText := rosterText(effect["payload_json"])
	payloadValue, err := glParse(payloadText)
	if err != nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	payload := pfObject(payloadValue)
	if payload == nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	// :87-89 the stored payload is its own hash, and it describes the same
	// entries the receipt froze.
	effectEntries, _ := pfField(payload, "characterEntries")
	if summaryHash(payloadText) != rosterText(effect["payload_hash"]) || !glSame(effectEntries, entries) {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	// :90 no handle is a proven null provenance.
	if handleValue, present := pfField(payload, "generationRunHandle"); !present || handleValue == nil {
		return append(proof, plotField{"provenance", nil}), nil
	}
	// :91-104 the generated arm. Its run binding and effect slot are checked, so
	// the only missing piece is named precisely: the artifact decode.
	handle := pfObject(pfValue(payload, "generationRunHandle"))
	if handle == nil || pfText(handle, "projectId") != projectID {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	runRows, err := rrMaps(ctx, conn, `SELECT run_id,root_action_id,binding_json FROM generation_runs WHERE run_id=?`,
		pfText(handle, "runId"))
	if err != nil {
		return nil, err
	}
	if len(runRows) != 1 {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	bindingValue, err := glParse(rosterText(runRows[0]["binding_json"]))
	if err != nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	binding := pfObject(bindingValue)
	if binding == nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	manifest := pfObject(pfValue(binding, "sourceManifest"))
	slot := plotObject{
		{"runId", rosterText(effect["run_id"])},
		{"stage", "global"},
		{"batchId", rosterText(effect["batch_id"])},
	}
	if pfText(binding, "projectId") != projectID ||
		pfText(handle, "rootActionId") != rosterText(runRows[0]["root_action_id"]) ||
		manifest == nil || pfText(manifest, "operation") != pendingFinalizedImportOperation ||
		!glSame(pfValue(manifest, "importSlot"), slot) {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	return nil, ErrCharacterProposalPendingProofUnavailable
}

// pendingFinalizedStateProof is
// generation-character-proposal-proof.ts:52-61 on top of
// proveFinalizedCharacterGeneration (finalized-character-generation-proof.ts:13-64).
func pendingFinalizedStateProof(ctx context.Context, conn *sql.Conn, projectID string, source plotObject) (plotObject, error) {
	// :53 only kind, handle and artifact.
	if !pfAllow(source, "kind", "handle", "artifact") {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	handle := pfObject(pfValue(source, "handle"))
	if handle == nil || !pfAllow(handle, "projectId", "epoch", "rootActionId", "runId") {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	handleProject := pfText(handle, "projectId")
	handleEpoch := pfText(handle, "epoch")
	handleRoot := pfText(handle, "rootActionId")
	handleRun := pfText(handle, "runId")
	if handleProject == "" || handleEpoch == "" || handleRoot == "" || handleRun == "" {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	// :14 runs.get, and :15-16 the current handle.
	runRows, err := rrMaps(ctx, conn, `SELECT run_id,root_action_id,binding_json FROM generation_runs WHERE run_id=?`, handleRun)
	if err != nil {
		return nil, err
	}
	if len(runRows) == 0 {
		return nil, glError("GENERATION_RUN_MISSING")
	}
	bindingValue, err := glParse(rosterText(runRows[0]["binding_json"]))
	if err != nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	binding := pfObject(bindingValue)
	if binding == nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	runID := rosterText(runRows[0]["run_id"])
	rootID := rosterText(runRows[0]["root_action_id"])
	bindingProject := pfText(binding, "projectId")
	bindingEpoch := pfText(binding, "epoch")
	manifest := pfObject(pfValue(binding, "sourceManifest"))
	if manifest == nil {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	// :17-18 the dedicated arm is keyed by any one of the three manifest keys.
	dedicated := false
	for _, key := range []string{"finalizationGenerationContext", "finalizationGenerationContextHash",
		"finalizationGenerationOriginEpoch"} {
		if _, present := pfField(manifest, key); present {
			dedicated = true
		}
	}
	// :19-21 the handle belongs to this project and this run, the run is a
	// finalized-character-state run, and an old (non-dedicated) source must be
	// exactly the run's current handle.
	if handleProject != projectID || handleProject != bindingProject || handleRoot != rootID ||
		pfText(manifest, "operation") != pendingFinalizedStateOperation {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	if !dedicated && (handleEpoch != bindingEpoch || handleRoot != rootID || handleRun != runID) {
		return nil, ErrCharacterProposalPendingSourceInvalid
	}
	// :22-24 the frozen context is an author input, hashed by the manifest.
	serialized := ""
	inputs, _ := pfArray(manifest, "authorInputs")
	for _, value := range inputs {
		input := pfObject(value)
		if input != nil && pfText(input, "id") == "finalized-character-context" {
			serialized = pfText(input, "text")
			break
		}
	}
	if serialized == "" || pfText(manifest, "finalizedCharacterContextHash") != summaryHash(serialized) {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	contextValue, err := glParse(serialized)
	if err != nil {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	contextObject := pfObject(contextValue)
	if contextObject == nil {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	contextSource := pfObject(pfValue(contextObject, "source"))
	if contextSource == nil {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	// :26-37 the dedicated arm's own hash, identity and slot.
	if dedicated {
		finalization := pfObject(pfValue(manifest, "finalizationGenerationContext"))
		originEpoch := pfText(manifest, "finalizationGenerationOriginEpoch")
		if finalization == nil || summaryTrim(originEpoch) == "" {
			return nil, ErrCharacterProposalPendingContextUnproven
		}
		encodedFinalization, err := plotJSON(finalization)
		if err != nil || pfText(manifest, "finalizationGenerationContextHash") != summaryHash(encodedFinalization) {
			return nil, ErrCharacterProposalPendingContextUnproven
		}
		if originEpoch != pfText(contextObject, "epoch") ||
			!glSame(pfValue(finalization, "identity"), contextObject) {
			return nil, ErrCharacterProposalPendingContextUnproven
		}
		slot := pfObject(pfValue(finalization, "slot"))
		if slot == nil || pfText(slot, "stepKey") != "character_cards" ||
			!glSame(pfValue(slot, "source"), pfValue(contextObject, "source")) {
			return nil, ErrCharacterProposalPendingContextUnproven
		}
		characters, _ := pfArray(contextObject, "characters")
		for _, value := range characters {
			character := pfObject(value)
			if character == nil {
				return nil, ErrCharacterProposalPendingContextUnproven
			}
			fields, _ := pfArray(character, "fields")
			for _, rawField := range fields {
				field := pfObject(rawField)
				if field == nil ||
					pfText(field, "projectId") != pfText(contextObject, "projectId") ||
					pfText(field, "epoch") != pfText(contextObject, "epoch") ||
					pfText(field, "characterId") != pfText(character, "characterId") {
					return nil, ErrCharacterProposalPendingContextUnproven
				}
			}
		}
		if handleEpoch != bindingEpoch && handleEpoch != originEpoch {
			return nil, ErrCharacterProposalPendingSourceInvalid
		}
	}
	// :38-44 the context is this project's, its prose still hashes to its own
	// source, and the run froze exactly this draft and source reference.
	contextProject := pfText(contextObject, "projectId")
	if contextProject != projectID {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	if !dedicated && pfText(contextObject, "epoch") != handleEpoch {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	contextContent := pfText(contextObject, "content")
	if summaryHash(contextContent) != pfText(contextSource, "contentHash") {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	draftID, draftOK := pfInteger(contextSource, "draftId")
	if !draftOK {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	selected, _ := pfArray(manifest, "selectedFinalizedDraftIds")
	if len(selected) != 1 {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	selectedID, selectedOK := rrInteger(selected[0], 0)
	if !selectedOK || selectedID != draftID {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	finalizationID := pfText(contextSource, "finalizationId")
	contentHash := pfText(contextSource, "contentHash")
	wantRef := "finalized:" + strconv.FormatInt(draftID, 10) + ":" + finalizationID
	refs, _ := pfArray(binding, "sourceRefs")
	refOK := false
	for _, value := range refs {
		ref := pfObject(value)
		if ref != nil && pfText(ref, "sourceId") == wantRef && pfText(ref, "contentHash") == contentHash {
			refOK = true
			break
		}
	}
	if !refOK {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	// :39-48 the artifact reference and the ledger receipt.
	artifact := pfObject(pfValue(source, "artifact"))
	if artifact == nil || !pfAllow(artifact, "artifactId", "revision", "textHash") {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	artifactID := pfText(artifact, "artifactId")
	artifactRevision, artifactRevisionOK := pfInteger(artifact, "revision")
	artifactHash := pfText(artifact, "textHash")
	if artifactID == "" || !artifactRevisionOK || artifactHash == "" {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	artifactRows, err := rrMaps(ctx, conn, `SELECT run_id,attempt_id,status FROM generation_artifacts WHERE artifact_id=?`, artifactID)
	if err != nil {
		return nil, err
	}
	if len(artifactRows) == 0 || rosterText(artifactRows[0]["run_id"]) != runID ||
		rosterText(artifactRows[0]["status"]) == "discarded" {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	receiptValue, err := NewGenerationLedger(nil, nil, nil).receipt(ctx, conn, rosterText(artifactRows[0]["attempt_id"]), nil)
	if err != nil {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	receiptArtifact := pfObject(pfValue(receiptValue, "artifact"))
	receiptAttempt := pfObject(pfValue(receiptValue, "attempt"))
	receiptResult := pfObject(pfValue(receiptValue, "result"))
	if receiptArtifact == nil ||
		pfText(receiptArtifact, "artifactId") != artifactID ||
		!pfNumberIs(pfValue(receiptArtifact, "revision"), artifactRevision) ||
		pfText(receiptArtifact, "textHash") != artifactHash {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	// `receipt.failureCode` is truthy: the ledger omits a falsy one, so a
	// present key is a failed attempt and no artifact may supply the source.
	if _, failed := pfField(receiptValue, "failureCode"); failed {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	attemptStatus := pfText(receiptAttempt, "status")
	if attemptStatus != "settled" && attemptStatus != "unknown" {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	if pfText(receiptResult, "finishReason") != "stop" {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	artifactText := pfText(receiptArtifact, "text")
	if summaryHash(artifactText) != artifactHash {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	// :49-51 the response, re-parsed against the frozen context.
	office, err := pendingFinalizedCharacterState(contextObject)
	if err != nil {
		return nil, err
	}
	response, err := characterstate.ParseResponse(artifactText, office)
	if err != nil {
		return nil, err
	}
	// :53-66 the model receipt the provenance names.
	model := pfObject(pfValue(manifest, "modelReceipt"))
	if model == nil {
		return nil, ErrCharacterProposalPendingProvenanceRequired
	}
	modelRevision := pfText(model, "modelRevision")
	if !pfDigest(modelRevision) {
		return nil, ErrCharacterProposalPendingProvenanceRequired
	}
	// :56-61 the items: one per named unresolved occurrence, the raw model
	// update preserved with its own key order.
	orderedUpdates := pendingFinalizedUpdates(artifactText)
	items := make([]any, 0, len(response.Unresolved))
	for _, occurrence := range response.Unresolved {
		if pfTrim(occurrence.DisplayName) == "" {
			continue
		}
		raw := occurrence.RawValue
		if index, ok := pendingFinalizedOccurrenceIndex(occurrence.SelectionKey); ok &&
			index >= 0 && index < len(orderedUpdates) {
			raw = orderedUpdates[index]
		}
		occurrenceObject := plotObject{
			{"selectionKey", occurrence.SelectionKey},
			{"displayName", occurrence.DisplayName},
			{"originalText", occurrence.OriginalText},
			{"candidateIds", pfTexts(occurrence.CandidateIDs)},
			{"currentState", pendingFinalizedStateValues(occurrence.CurrentState)},
			{"evidence", plotObject{
				{"start", int64(occurrence.Evidence.Start)},
				{"end", int64(occurrence.Evidence.End)},
				{"text", occurrence.Evidence.Text},
			}},
			{"rawValue", raw},
			{"reason", occurrence.Reason},
		}
		items = append(items, plotObject{
			{"selectionKey", occurrence.SelectionKey},
			{"sourceId", finalizationID + ":" + artifactID + ":" + occurrence.SelectionKey},
			{"fields", plotObject{{"name", occurrence.DisplayName}}},
			{"relationships", []any{}},
			{"rawValue", occurrenceObject},
		})
	}
	// :58-61 the canonical source and the hash over it and the items.
	sourceEpoch := bindingEpoch
	canonical := source
	if dedicated {
		sourceEpoch = pfText(contextObject, "epoch")
		canonical = plotObject{
			{"kind", pendingFinalizedFinalizedKind},
			{"handle", plotObject{
				{"projectId", bindingProject},
				{"epoch", sourceEpoch},
				{"rootActionId", rootID},
				{"runId", runID},
			}},
			{"artifact", plotObject{
				{"artifactId", artifactID},
				{"revision", artifactRevision},
				{"textHash", artifactHash},
			}},
		}
	}
	hashInput, err := plotJSON([]any{canonical, items})
	if err != nil {
		return nil, ErrCharacterProposalPendingArtifactInvalid
	}
	// :62-64 the provenance names the finalization the context froze, at the
	// authoritative finalization revision the context's source order records.
	sourceOrder := pfObject(pfValue(contextObject, "sourceOrder"))
	authoritativeRevision, sourceOrderOK := int64(0), false
	if sourceOrder != nil {
		authoritativeRevision, sourceOrderOK = pfInteger(sourceOrder, "authoritativeFinalizationRevision")
	}
	if !sourceOrderOK {
		return nil, ErrCharacterProposalPendingContextUnproven
	}
	return plotObject{
		{"items", items},
		{"sourceHash", summaryHash(hashInput)},
		{"provenance", plotObject{
			{"kind", "derived"},
			{"modelRevision", modelRevision},
			{"source", plotObject{
				{"projectId", projectID},
				{"epoch", sourceEpoch},
				{"sourceId", finalizationID},
				{"revision", authoritativeRevision},
				{"contentHash", contentHash},
			}},
		}},
	}, nil
}

// pendingFinalizedCurrent is assertFinalizedSourceCurrent
// (character-proposal-service.ts:44-58): the finalization is still the frozen
// one, its prose still hashes to the recorded content hash, and the
// authoritative finalization revision is still the one the proof recorded. Any
// failure is the reference's own CHARACTER_PROPOSAL_SOURCE_CHANGED.
func pendingFinalizedCurrent(ctx context.Context, conn *sql.Conn, projectID string, source, proof plotObject) error {
	provenance := pfObject(pfValue(proof, "provenance"))
	if provenance == nil {
		return ErrCharacterProposalPendingSourceChanged
	}
	ref := pfObject(pfValue(provenance, "source"))
	if ref == nil {
		return ErrCharacterProposalPendingSourceChanged
	}
	sourceID := pfText(ref, "sourceId")
	contentHash := pfText(ref, "contentHash")
	revision, revisionOK := pfInteger(ref, "revision")
	if sourceID == "" || contentHash == "" || !revisionOK {
		return ErrCharacterProposalPendingSourceChanged
	}
	rows, err := rrMaps(ctx, conn, `SELECT draft_id FROM finalization_outbox WHERE finalization_id=?`, sourceID)
	if err != nil || len(rows) == 0 {
		return ErrCharacterProposalPendingSourceChanged
	}
	draftID, ok := rrInteger(rows[0]["draft_id"], 0)
	if !ok {
		return ErrCharacterProposalPendingSourceChanged
	}
	handle := pfObject(pfValue(source, "handle"))
	scope := FinalizedCharacterScope{ProjectID: projectID, Epoch: pfText(handle, "epoch")}
	current, err := pendingFinalizedCharacterContext(ctx, conn, draftID, scope)
	if err != nil || current == nil {
		return ErrCharacterProposalPendingSourceChanged
	}
	if current.Source.FinalizationID != sourceID ||
		current.Source.ContentHash != contentHash ||
		current.SourceOrder.AuthoritativeFinalizationRevision != revision {
		return ErrCharacterProposalPendingSourceChanged
	}
	return nil
}

// pendingFinalizedCharacterContext is readFinalizedCharacterContext
// (summary-repository.ts:401-424) on the caller's reserved connection. The
// store's exported twin takes a *sql.Tx (ReadFinalizedCharacterContextTx), and
// opening a transaction here would need a second pool connection while this one
// is held, so the same body is run through rrQueryer instead.
func pendingFinalizedCharacterContext(ctx context.Context, q rrQueryer, draftID int64,
	scope FinalizedCharacterScope) (*FinalizedCharacterContext, error) {
	if summaryTrim(scope.ProjectID) == "" || summaryTrim(scope.Epoch) == "" {
		return nil, ErrFinalizedCharacterScopeRequired
	}
	source, err := finalizedCharacterReadSource(ctx, q, draftID)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return nil, ErrFinalizedCharacterSourceChanged
	}
	latest, err := finalizedCharacterLatestFinalized(ctx, q, source.Identity.ChapterNumber)
	if err != nil {
		return nil, err
	}
	if latest == nil || latest.DraftID != draftID {
		return nil, ErrFinalizedCharacterSourceChanged
	}
	frozen, err := finalizedCharacterReadFrozen(ctx, q, source.Identity)
	if err != nil {
		return nil, err
	}
	characters := []FinalizedCharacterContextCharacter{}
	if frozen != nil {
		for _, character := range frozen.Characters {
			row, err := finalizedCharacterActiveRow(ctx, q, character.CharacterID)
			if err != nil {
				return nil, err
			}
			if row == nil {
				return nil, ErrFinalizedCharacterIDRetired
			}
			fields := make([]FinalizedCharacterFieldSnapshot, 0, len(characterStateFields))
			for index, field := range characterStateFields {
				snapshot, err := finalizedCharacterStateSnapshot(row, field, characterStateColumns[index], scope, character.CharacterID)
				if err != nil {
					return nil, err
				}
				fields = append(fields, snapshot)
			}
			characters = append(characters, FinalizedCharacterContextCharacter{
				CharacterID:         character.CharacterID,
				DisplayNameSnapshot: character.DisplayNameSnapshot,
				Aliases:             character.Aliases,
				Fields:              fields,
			})
		}
	}
	identityRevision, identityStatus := int64(0), FinalizedCharacterIdentityLegacy
	if frozen != nil {
		identityRevision, identityStatus = frozen.IdentityRevision, FinalizedCharacterIdentityBound
	}
	return &FinalizedCharacterContext{
		ProjectID:            scope.ProjectID,
		Epoch:                scope.Epoch,
		Source:               source.Identity,
		Content:              source.Content,
		ProjectionGeneration: source.ProjectionGeneration,
		IdentityRevision:     identityRevision,
		IdentityStatus:       identityStatus,
		SourceOrder: FinalizedCharacterSourceOrder{
			ContinuityEpoch:                   scope.ProjectID + ":" + phNumberString(source.ProjectionGeneration),
			ChapterNumber:                     source.Identity.ChapterNumber,
			AuthoritativeFinalizationRevision: latest.Version,
		},
		Characters: characters,
	}, nil
}

// pendingFinalizedCharacterState converts the frozen context object into the
// parser's minimal input. Only the members characterstate reads are copied.
func pendingFinalizedCharacterState(contextObject plotObject) (characterstate.Context, error) {
	source := pfObject(pfValue(contextObject, "source"))
	if source == nil {
		return characterstate.Context{}, ErrCharacterProposalPendingContextUnproven
	}
	chapter, ok := pfInteger(source, "chapterNumber")
	if !ok {
		return characterstate.Context{}, ErrCharacterProposalPendingContextUnproven
	}
	state := characterstate.Context{
		Source:         characterstate.ContextSource{ChapterNumber: chapter},
		Content:        pfText(contextObject, "content"),
		IdentityStatus: pfText(contextObject, "identityStatus"),
		Characters:     []characterstate.ContextCharacter{},
	}
	characters, _ := pfArray(contextObject, "characters")
	for _, value := range characters {
		character := pfObject(value)
		if character == nil {
			continue
		}
		aliases := []string{}
		rawAliases, _ := pfArray(character, "aliases")
		for _, alias := range rawAliases {
			aliases = append(aliases, rosterText(alias))
		}
		state.Characters = append(state.Characters, characterstate.ContextCharacter{
			CharacterID:         pfText(character, "characterId"),
			DisplayNameSnapshot: pfText(character, "displayNameSnapshot"),
			Aliases:             aliases,
		})
	}
	return state, nil
}

// pendingFinalizedRefresh is resolveItems (:103-112) plus the write-back of
// :117-121: a pending batch whose stored revision trails the identity revision
// is re-resolved against the live roster and stored again in this transaction.
func pendingFinalizedRefresh(ctx context.Context, conn *sql.Conn, projectID string,
	proposal PendingFinalizedProposal) (PendingFinalizedProposal, error) {
	revision, aliases, err := importGlobalFactsIdentity(ctx, conn)
	if err != nil {
		return PendingFinalizedProposal{}, err
	}
	if revision == proposal.Revision {
		return proposal, nil
	}
	envelopeValue, err := glParse(proposal.Envelope)
	if err != nil {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	envelope := pfObject(envelopeValue)
	batch := pfObject(pfValue(envelope, "batch"))
	proof := pfObject(pfValue(envelope, "proof"))
	if batch == nil || proof == nil {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	proofItems, _ := pfArray(proof, "items")
	items := make([]any, 0, len(proofItems))
	for _, value := range proofItems {
		item := pfObject(value)
		if item == nil {
			return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
		}
		fields := pfObject(pfValue(item, "fields"))
		resolution := importGlobalFactsResolveScoped(aliases, pfText(fields, "name"), projectID,
			pfText(item, "sourceId"), revision)
		if proposal.Kind != pendingFinalizedFinalizedKind && rosterText(resolution["status"]) == "unresolved" {
			resolution = importGlobalFactsResolveScoped(aliases, pfText(fields, "name"), projectID, "", revision)
		}
		updated := append(plotObject{}, item...)
		updated = append(updated, plotField{"resolution", pendingFinalizedResolution(resolution)})
		items = append(items, updated)
	}
	batch = pfReplace(batch, "revision", revision)
	batch = pfReplace(batch, "items", items)
	envelope = pfReplace(envelope, "batch", batch)
	encoded, err := plotJSON(envelope)
	if err != nil {
		return PendingFinalizedProposal{}, ErrCharacterProposalPendingReceiptInvalid
	}
	if _, err := conn.ExecContext(ctx,
		`UPDATE character_identity_proposals SET source_hash=?,raw_value=? WHERE proposal_id=?`,
		summaryHash(encoded), encoded, proposal.ProposalBatchID); err != nil {
		return PendingFinalizedProposal{}, err
	}
	proposal.Revision = revision
	proposal.Envelope = encoded
	proposal.ItemCount = len(items)
	return proposal, nil
}

// pendingFinalizedResolution is ScopedCharacterResolution in the reference's
// literal key order (character-identity.ts:110-113).
func pendingFinalizedResolution(resolution map[string]any) plotObject {
	status := rosterText(resolution["status"])
	if status == "resolved" {
		return plotObject{{"status", status}, {"characterId", rosterText(resolution["characterId"])}}
	}
	ids := []any{}
	if candidates, ok := resolution["candidateIds"].([]string); ok {
		for _, candidate := range candidates {
			ids = append(ids, candidate)
		}
	}
	return plotObject{{"status", status}, {"candidateIds", ids}}
}

// pendingFinalizedImportPurpose is importPurpose (:125-130): the purpose of the
// import run the operation id names, absent when it cannot be read.
func pendingFinalizedImportPurpose(ctx context.Context, conn *sql.Conn, operationID string) string {
	const prefix = "novel-import-global-"
	if !strings.HasPrefix(operationID, prefix) || len(operationID) == len(prefix) {
		return ""
	}
	rows, err := rrMaps(ctx, conn, `SELECT purpose FROM import_runs WHERE id=?`, strings.TrimPrefix(operationID, prefix))
	if err != nil || len(rows) == 0 {
		return ""
	}
	return rosterText(rows[0]["purpose"])
}

// pendingFinalizedUpdates re-reads the artifact body with the reference's own
// trim and fence rules so each occurrence's rawValue keeps the model's key
// order, which the proof's source hash covers.
func pendingFinalizedUpdates(text string) []any {
	trimmed := pfTrim(text)
	trimmed = pfLeadingFence(trimmed)
	trimmed = pfTrailingFence(trimmed)
	value, err := glParse(trimmed)
	if err != nil {
		return nil
	}
	body := pfObject(value)
	if body == nil {
		return nil
	}
	updates, _ := pfArray(body, "updates")
	return updates
}

// pendingFinalizedOccurrenceIndex is the `update:<index>` selection key
// (finalized-continuity.ts:219).
func pendingFinalizedOccurrenceIndex(selectionKey string) (int, bool) {
	const prefix = "update:"
	if !strings.HasPrefix(selectionKey, prefix) {
		return 0, false
	}
	index, err := strconv.Atoi(strings.TrimPrefix(selectionKey, prefix))
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

// pendingFinalizedStateValues is the occurrence's currentState in
// CHARACTER_STATE_TEXT_FIELDS order (character-roster.ts:44).
func pendingFinalizedStateValues(values characterstate.Values) plotObject {
	state := plotObject{}
	for _, field := range characterStateFields {
		if value, present := values.Get(field); present {
			state = append(state, plotField{field, value})
		}
	}
	return state
}

// pfTexts is a []string as a JSON array of strings.
func pfTexts(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

// The JSON-object helpers below accept both the order-preserving plotObject this
// package parses and a plain map, because a caller can hand Prove a Go map.
func pfObject(value any) plotObject {
	switch object := value.(type) {
	case plotObject:
		return object
	case map[string]any:
		keys := make([]string, 0, len(object))
		for key := range object {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return plotOrdered(object, keys...)
	}
	return nil
}

func pfField(value any, key string) (any, bool) {
	switch object := value.(type) {
	case plotObject:
		for _, field := range object {
			if field.key == key {
				return field.value, true
			}
		}
	case map[string]any:
		field, present := object[key]
		return field, present
	}
	return nil, false
}

func pfValue(value any, key string) any {
	field, _ := pfField(value, key)
	return field
}

func pfText(value any, key string) string { return rosterText(pfValue(value, key)) }

func pfInteger(value any, key string) (int64, bool) {
	field, present := pfField(value, key)
	if !present {
		return 0, false
	}
	return rrInteger(field, -pendingFinalizedMaxSafe)
}

func pfArray(value any, key string) ([]any, bool) {
	field, present := pfField(value, key)
	if !present {
		return nil, false
	}
	list, ok := field.([]any)
	return list, ok
}

// pfAllow is Object.keys(object).every(key => allowed.includes(key)).
func pfAllow(value any, keys ...string) bool {
	allowed := make(map[string]bool, len(keys))
	for _, key := range keys {
		allowed[key] = true
	}
	switch object := value.(type) {
	case plotObject:
		for _, field := range object {
			if !allowed[field.key] {
				return false
			}
		}
		return true
	case map[string]any:
		for key := range object {
			if !allowed[key] {
				return false
			}
		}
		return true
	}
	return false
}

// pfReplace sets key in an ordered object, keeping its position or appending it.
func pfReplace(object plotObject, key string, value any) plotObject {
	replaced := false
	out := make(plotObject, 0, len(object)+1)
	for _, field := range object {
		if field.key == key {
			out = append(out, plotField{key, value})
			replaced = true
			continue
		}
		out = append(out, field)
	}
	if !replaced {
		out = append(out, plotField{key, value})
	}
	return out
}

func pfNumberIs(value any, want int64) bool {
	number, ok := rrInteger(value, -pendingFinalizedMaxSafe)
	return ok && number == want
}

// pfDigest is /^[a-f0-9]{64}$/.
func pfDigest(text string) bool {
	if len(text) != 64 {
		return false
	}
	for _, character := range text {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

const pendingFinalizedMaxSafe = int64(9007199254740991)

// pfSpace is the JavaScript WhiteSpace + LineTerminator set (state_response.go:652).
func pfSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' ||
		r == ' ' || r == '\u00a0' || r == '\u1680' ||
		(r >= '\u2000' && r <= '\u200a') || r == '\u2028' || r == '\u2029' ||
		r == '\u202f' || r == '\u205f' || r == '\u3000' || r == '\ufeff'
}

func pfTrim(value string) string { return strings.TrimFunc(value, pfSpace) }

// pfLeadingFence is replace(/^```(?:json)?\s*\n?/i, '').
func pfLeadingFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	rest := text[3:]
	if len(rest) >= 4 && strings.EqualFold(rest[:4], "json") {
		rest = rest[4:]
	}
	return strings.TrimLeftFunc(rest, pfSpace)
}

// pfTrailingFence is replace(/\n?```\s*$/, '').
func pfTrailingFence(text string) string {
	fence := strings.LastIndex(text, "```")
	if fence < 0 || pfTrim(text[fence+3:]) != "" {
		return text
	}
	if fence > 0 && text[fence-1] == '\n' {
		fence--
	}
	return text[:fence]
}
