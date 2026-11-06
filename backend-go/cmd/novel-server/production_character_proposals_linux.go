//go:build linux

// This file serves the four `character-proposal` names the architecture workflow
// actually calls, and installs nothing for the two it cannot honour:
//
//	character-proposal:stage   web/operations.json:56  served here
//	character-proposal:read    web/operations.json:57  served here
//	character-proposal:approve web/operations.json:60  served here
//	character-proposal:cancel  web/operations.json:61  served here
//	character-proposal:list-pending-finalized  web/operations.json:58  refused, see
//	character-proposal:read-pending-finalized  web/operations.json:59  productionCharacterProposalPendingSeams
//
// # The reference
//
//	electron/controllers/generation-controller.ts:208-216
//	  register('character-proposal:stage', 1, (owner, request) => { ... owner.characterProposals.stage(request.source) })
//	  register('character-proposal:approve', 1, (owner, request) => owner.approveCharacterProposal(request))
//	  register('character-proposal:read' | ':cancel' | ':list-pending-finalized' | ':read-pending-finalized', ...)
//	electron/services/character-proposal-service.ts
//	  :159 stage, :114 read, :196 cancel, :209 approve, :60 readEnvelope, :103 resolveItems
//	electron/services/generation-character-proposal-proof.ts:44-171
//	  the source proof: :136-171 is the generation arm this file ports
//	src/shared/character-proposal-parser.ts:16-47
//	  the architecture parser, ported in internal/characterproposal
//	src/shared/character-identity.ts:116-130
//	  resolveScopedCharacterIdentity, ported below
//
// # The store it writes through
//
//	internal/store/character_identity.go:161
//	  (*CharacterIdentities).CommitIdentities(ctx, request, authorize) (map[string]any, error)
//	internal/store/character_roster.go:27, :591
//	  (*Characters).ReadRoster(ctx) (map[string]any, error)
//	  (*Characters).RefreshIdentityProjection(ctx) error
//
// Those three are the whole write path. This file owns no identity SQL, no alias
// SQL, no retirement and no projection rendering: approve builds one
// CharacterIdentityCommitRequest and hands it to the same commit the author roster
// uses, with an authorize callback that re-encodes the candidate and compares it
// with the request this file froze - the reference's
// `commitCharacterIdentities(db, identityRequest, candidate => JSON.stringify(candidate) === frozenRequest)`
// (character-proposal-service.ts:282-283), spelled with this lane's canonical
// encoder because Go structs carry no JSON identity.
//
// # What stage writes, and what it must never write
//
// stage stores ONE row in character_identity_proposals and touches no roster
// table: no characters row, no alias, no identity revision, no projection. The
// staged proposal is model output, so it is parsed into
// internal/characterproposal.Item before it is stored, and the stored envelope is
// this file's own JSON - never the model's bytes passed through. The envelope is
// also the only row approve reads the proposal back from.
//
// # Why approve is the only writer
//
// `character-proposal:approve` is the only name in this file that changes what the
// author sees. It writes through CommitIdentities (characters, aliases, the
// approval receipt and the identity revision), then refreshes the projection, then
// marks the envelope approved. Those are three statements in three transactions,
// because the store exports no connection-scoped seam, so a crash between them
// leaves an approved commit with a pending envelope. That is self-healing and not
// a double write: the retry presents the same operationId with the same payload,
// and CommitIdentities answers with the stored receipt instead of writing a second
// time (character_identity.go:202-213, the reference's `:1178-1182`), so the
// only thing the retry does is finish the refresh and the envelope update.
//
// # Refusals are symbolic, and there is exactly one mapping layer
//
// productionCharacterProposalFailure is the only place a refusal becomes a wire
// code. Its table names every refusal this file and internal/characterproposal can
// raise, in the same two-tier shape the reference's controller uses
// (generation-controller.ts:88-101): the symbolic codes the reference itself
// publishes pass through unchanged, and anything else - a SQLite message, a path,
// a provider's prose - becomes GENERATION_REQUEST_FAILED. The final fallback is
// the controller's own prefix allowlist, so a code this table does not name still
// only passes when it is one of the families the reference names.
//
// # The two names this file refuses
//
// list-pending-finalized and read-pending-finalized exist to carry
// finalized-generation and import proposals into the review UI
// (character-proposal-service.ts:131-154). Both read the provenance source out of
// the envelope and, for import, the run's purpose from import_runs (:126-130), then
// page the pending batches. Neither source kind can be proved in this lane:
//
//   - finalized-generation needs normalizeFinalizedCharacterProposalSource and the
//     finalization outbox/context read (finalized-character-generation-proof.ts:52-65),
//     which no Go package owns yet;
//   - import needs the import global-facts receipt validation
//     (generation-character-proposal-proof.ts:67-75), likewise absent.
//
// A binder that installed those two names could only answer "not this source
// kind" for every row they are defined to page, so it installs nothing and both
// keep failing closed at GO_OPERATION_NOT_MIGRATED (internal/transport/server.go).
// productionCharacterProposalPendingSeams records the exact code that has to land
// first, and a test re-reads each cited line.
//
// # The source kinds this file serves
//
// stage/read/approve/cancel serve `{kind:'generation', inputKind:'architecture'}`
// only - the architecture workflow's own source (architecture.command.ts). The other
// three kinds (directory, import, finalized-generation, and the legacy roster arm
// the controller refuses before the service sees it) answer
// CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE here: the source is well formed but
// the proof port it needs does not exist in this lane. That code is this lane's
// own, documented here so it is never mistaken for a reference code.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/characterproposal"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionCharacterProposalStageName is web/operations.json:56.
	productionCharacterProposalStageName = "character-proposal:stage"

	// productionCharacterProposalReadName is web/operations.json:57.
	productionCharacterProposalReadName = "character-proposal:read"

	// productionCharacterProposalApproveName is web/operations.json:60.
	productionCharacterProposalApproveName = "character-proposal:approve"

	// productionCharacterProposalCancelName is web/operations.json:61.
	productionCharacterProposalCancelName = "character-proposal:cancel"

	// productionCharacterProposalListPendingName is web/operations.json:58. It is
	// refused, never bound.
	productionCharacterProposalListPendingName = "character-proposal:list-pending-finalized"

	// productionCharacterProposalReadPendingName is web/operations.json:59. It is
	// refused, never bound.
	productionCharacterProposalReadPendingName = "character-proposal:read-pending-finalized"

	// productionCharacterProposalSourceKeyPrefix is the source_key every envelope
	// of this project carries (character-proposal-service.ts:62, :192).
	productionCharacterProposalSourceKeyPrefix = "character-proposal-v1:"

	// productionCharacterProposalDerivationVersion is the only derivation version
	// this lane stages (character-proposal-service.ts:182, :67-68).
	productionCharacterProposalDerivationVersion = 2

	// productionCharacterProposalRequestFailed is the answer for a refusal this
	// lane cannot name (generation-controller.ts:101).
	productionCharacterProposalRequestFailed = "GENERATION_REQUEST_FAILED"

	// The three batch states of CharacterProposalBatch
	// (src/shared/character-proposal.ts).
	productionCharacterProposalStatusPending   = "pending-approval"
	productionCharacterProposalStatusApproved  = "approved"
	productionCharacterProposalStatusCancelled = "cancelled"

	// The three selection actions of ApproveCharacterProposalRequest.
	productionCharacterProposalActionCreate = "create"
	productionCharacterProposalActionMap    = "map"
	productionCharacterProposalActionKeep   = "keep-unresolved"

	// The two source fields that decide whether this lane serves a source at all.
	productionCharacterProposalGenerationKind   = "generation"
	productionCharacterProposalArchitectureKind = "architecture"
	productionCharacterProposalLegacyKind       = "legacy-roster-generation"
	productionCharacterProposalFinalizedKind    = "finalized-generation"

	// The provenance kinds approve accepts
	// (character-proposal-service.ts:241) and the action it commits with
	// (:248).
	productionCharacterProposalProvenanceGenerated = "generated"
	productionCharacterProposalProvenanceDerived   = "derived"
	productionCharacterProposalAdoptGenerated      = "adopt-generated"

	// productionCharacterProposalArchitectureOperation is the run manifest
	// operation the architecture generation freezes
	// (generation-character-proposal-proof.ts:155).
	productionCharacterProposalArchitectureOperation = "character-architecture"

	// productionCharacterProposalSelectionKey and the keys below are the body keys
	// of this family. They are spelled once so a guard and its reader cannot drift.
	productionCharacterProposalBatchKey        = "proposalBatchId"
	productionCharacterProposalRevisionKey     = "expectedRevision"
	productionCharacterProposalOperationKey    = "operationId"
	productionCharacterProposalSelectionsKey   = "selections"
	productionCharacterProposalRelationsKey    = "relationships"
	productionCharacterProposalEditsKey        = "edits"
	productionCharacterProposalSourceKey       = "source"
	productionCharacterProposalSelectionKeyTag = "selectionKey"
	productionCharacterProposalActionKey       = "action"
	productionCharacterProposalCharacterKey    = "characterId"

	// productionCharacterProposalOperationMax is the body guard's cap on the
	// approval operation id (character-proposal-service.ts:210).
	productionCharacterProposalOperationMax = 256
)

// The refusals this file raises. Each one is the reference's own code where the
// reference has one, and a lane-only code where the reference has no answer
// because the shape cannot reach it.
var (
	// productionCharacterProposalReceiptInvalid is :62-68: the stored envelope is
	// absent, belongs to another project, or no longer hashes to its recorded hash.
	productionCharacterProposalReceiptInvalid = errors.New("CHARACTER_PROPOSAL_RECEIPT_INVALID")

	// productionCharacterProposalSourceChanged is :70-78: the source no longer
	// proves to what the envelope recorded.
	productionCharacterProposalSourceChanged = errors.New("CHARACTER_PROPOSAL_SOURCE_CHANGED")

	// productionCharacterProposalSourceInvalid is :46 and :136-142: the source
	// object itself is not the shape this lane reads, or names another project.
	productionCharacterProposalSourceInvalid = errors.New("CHARACTER_PROPOSAL_SOURCE_INVALID")

	// productionCharacterProposalSourceProofUnavailable is this lane's own code for
	// a source that is well formed but whose proof port is absent (see the header).
	productionCharacterProposalSourceProofUnavailable = errors.New("CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE")

	// productionCharacterProposalArtifactInvalid is :146-149: the artifact
	// reference is unknown, discarded, in another run, or disagrees with its own
	// settled receipt.
	productionCharacterProposalArtifactInvalid = errors.New("CHARACTER_PROPOSAL_ARTIFACT_INVALID")

	// productionCharacterProposalManifestInvalid is :155: the manifest artifact is
	// not among the artifacts, or the run is not an architecture run.
	productionCharacterProposalManifestInvalid = errors.New("CHARACTER_PROPOSAL_MANIFEST_INVALID")

	// productionCharacterProposalProvenanceRequired is :241: the source proved to
	// nothing that may be adopted as a generated identity.
	productionCharacterProposalProvenanceRequired = errors.New("CHARACTER_PROPOSAL_PROVENANCE_REQUIRED")

	// productionCharacterProposalApprovalInvalid is :84 and :92: an approved
	// envelope whose approval receipt no longer matches the batch revision.
	productionCharacterProposalApprovalInvalid = errors.New("CHARACTER_PROPOSAL_APPROVAL_INVALID")

	// productionCharacterProposalSelectionInvalid is :242, :253-254: the selection
	// list is not one closed choice per item.
	productionCharacterProposalSelectionInvalid = errors.New("CHARACTER_APPROVAL_SELECTION_INVALID")

	// productionCharacterProposalRelationshipInvalid is :272-278: a proposed
	// relationship the parser never produced, or one naming both endpoints.
	productionCharacterProposalRelationshipInvalid = errors.New("CHARACTER_RELATIONSHIP_PROPOSAL_INVALID")

	// productionCharacterProposalEditInvalid is :215-216: an edit on a batch whose
	// source is not planning-material. Architecture batches can never carry one.
	productionCharacterProposalEditInvalid = errors.New("CHARACTER_PROPOSAL_EDIT_INVALID")

	// productionCharacterProposalCancelled is :236: the batch was cancelled, so it
	// can no longer be approved.
	productionCharacterProposalCancelled = errors.New("CHARACTER_PROPOSAL_CANCELLED")

	// productionCharacterProposalAlreadyApproved is :203: cancel of an approved
	// batch. Cancelling cannot undo a commit that already landed.
	productionCharacterProposalAlreadyApproved = errors.New("CHARACTER_PROPOSAL_ALREADY_APPROVED")

	// productionCharacterProposalLegacyAdmission is generation-controller.ts:209:
	// the controller refuses a legacy roster source before the service sees it.
	productionCharacterProposalLegacyAdmission = errors.New("GENERATION_LEGACY_ADMISSION_REQUIRED")

	// productionCharacterProposalRunMissing is the ledger's own answer for a source
	// handle naming a run this project never opened (generation-run-repository.ts).
	productionCharacterProposalRunMissing = errors.New("GENERATION_RUN_MISSING")

	// productionCharacterProposalRunIdentityMismatch is
	// generation-character-proposal-proof.ts:141-142 and :159: the handle is not the
	// run's own handle.
	productionCharacterProposalRunIdentityMismatch = errors.New("GENERATION_RUN_IDENTITY_MISMATCH")

	// productionCharacterProposalLookupInvalid is resolveScopedCharacterIdentity's
	// own refusal (character-identity.ts:119-123). The reference publishes it as an
	// unsymbolic crash, which its controller answers with the unmapped-failure code.
	productionCharacterProposalLookupInvalid = errors.New("INVALID_CHARACTER_LOOKUP")
)

// productionCharacterProposalScope is the borrowed project and lease every handler
// receives with its database. The lease is used for ownership, never as the run's
// epoch: a run outlives the lease it was frozen under
// (production_composition_linux.go:437-441).
type productionCharacterProposalScope struct {
	ProjectID string
	Epoch     string
}

// productionCharacterProposalHandle is MainGenerationRunHandle
// (src/services/generation/generation-runtime.ts:77-82) in its declared key order,
// which is also the order the source hash is taken over.
type productionCharacterProposalHandle struct {
	ProjectID    string `json:"projectId"`
	Epoch        string `json:"epoch"`
	RootActionID string `json:"rootActionId"`
	RunID        string `json:"runId"`
}

// productionCharacterProposalArtifactRef is one artifact reference of the source
// (generation-character-proposal-proof.ts:146).
type productionCharacterProposalArtifactRef struct {
	ArtifactID string `json:"artifactId"`
	Revision   int64  `json:"revision"`
	TextHash   string `json:"textHash"`
}

// productionCharacterProposalSource is the one CharacterProposalSource arm this
// lane proves. The key order is the reference's own
// (generation-character-proposal-proof.ts:137), because the source hash and the
// batch id are taken over it.
type productionCharacterProposalSource struct {
	Kind               string                                   `json:"kind"`
	InputKind          string                                   `json:"inputKind,omitempty"`
	Handle             *productionCharacterProposalHandle        `json:"handle,omitempty"`
	Artifacts          []productionCharacterProposalArtifactRef  `json:"artifacts,omitempty"`
	ManifestArtifactID string                                    `json:"manifestArtifactId,omitempty"`
}

// productionCharacterProposalRef is SourceRef (src/shared/source-ref.ts).
type productionCharacterProposalRef struct {
	ProjectID   string `json:"projectId"`
	Epoch       string `json:"epoch"`
	SourceID    string `json:"sourceId"`
	Revision    int64  `json:"revision"`
	ContentHash string `json:"contentHash"`
}

// productionCharacterProposalProvenance is CharacterStaticProvenance's
// generated/derived arm (src/shared/character-identity.ts:134). modelRevision is
// omitted when the run's model receipt carries none, exactly the way an absent
// JavaScript property is dropped.
type productionCharacterProposalProvenance struct {
	Kind          string                        `json:"kind"`
	ModelRevision string                        `json:"modelRevision,omitempty"`
	Source        productionCharacterProposalRef `json:"source"`
}

// productionCharacterProposalProof is CharacterProposalProof
// (character-proposal-service.ts:14-19). items carries no resolution: the
// resolution is recomputed against the live roster on every read.
type productionCharacterProposalProof struct {
	Items      []characterproposal.Item               `json:"items"`
	SourceHash string                                 `json:"sourceHash"`
	Provenance *productionCharacterProposalProvenance `json:"provenance"`
}

// productionCharacterProposalResolution is ScopedCharacterResolution
// (src/shared/character-identity.ts:110-113). The two answering shapes carry
// different keys, so the wire form is emitted by MarshalJSON rather than by struct
// tags that would have to keep an empty candidateIds on a resolved answer.
type productionCharacterProposalResolution struct {
	Status       string
	CharacterID  string
	CandidateIDs []string
}

// MarshalJSON answers exactly one of the reference's two shapes.
func (r productionCharacterProposalResolution) MarshalJSON() ([]byte, error) {
	type resolved struct {
		Status      string `json:"status"`
		CharacterID string `json:"characterId"`
	}
	type candidates struct {
		Status       string   `json:"status"`
		CandidateIDs []string `json:"candidateIds"`
	}
	if r.Status == "resolved" {
		return productionCharacterProposalMarshal(resolved{Status: r.Status, CharacterID: r.CharacterID})
	}
	ids := r.CandidateIDs
	if ids == nil {
		ids = []string{}
	}
	return productionCharacterProposalMarshal(candidates{Status: r.Status, CandidateIDs: ids})
}

// productionCharacterProposalItem is CharacterProposalItem
// (src/shared/character-proposal.ts:19-27): the parsed item plus the resolution the
// current roster gives its name.
type productionCharacterProposalItem struct {
	characterproposal.Item
	Resolution productionCharacterProposalResolution `json:"resolution"`
}

// productionCharacterProposalBatch is CharacterProposalBatch
// (src/shared/character-proposal.ts): the only body shape stage/read/cancel answer,
// and the `batch` half of approve's answer.
type productionCharacterProposalBatch struct {
	ProposalBatchID     string                           `json:"proposalBatchId"`
	Revision            int64                            `json:"revision"`
	Status              string                           `json:"status"`
	Items               []productionCharacterProposalItem `json:"items"`
	Source              *productionCharacterProposalSource `json:"source"`
	ApprovalOperationID string                           `json:"approvalOperationId,omitempty"`
}

// productionCharacterProposalSelection is one ApproveCharacterProposalRequest
// selection. characterId is absent unless the action maps onto an existing
// identity, which is exactly the reference's per-action key allowlist (:253-254).
type productionCharacterProposalSelection struct {
	SelectionKey string  `json:"selectionKey"`
	Action       string  `json:"action"`
	CharacterID  *string `json:"characterId,omitempty"`
}

// productionCharacterProposalRelationship is one proposed relationship
// (ApproveCharacterProposalRequest.relationships).
type productionCharacterProposalRelationship struct {
	SourceSelectionKey string `json:"sourceSelectionKey,omitempty"`
	TargetSelectionKey string `json:"targetSelectionKey,omitempty"`
	TargetCharacterID  string `json:"targetCharacterId,omitempty"`
	Relation           string `json:"relation,omitempty"`
}

// productionCharacterProposalDecision is the request approve stores as the
// decision it made (character-proposal-service.ts:231, :313). It is also what a
// repeated approve is compared against (:232-235), so every key the reference
// stores has to survive here with the same presence rule.
type productionCharacterProposalDecision struct {
	ProposalBatchID  string                                       `json:"proposalBatchId"`
	ExpectedRevision *int64                                       `json:"expectedRevision,omitempty"`
	OperationID      string                                       `json:"operationId"`
	Selections       []productionCharacterProposalSelection       `json:"selections"`
	Relationships    *[]productionCharacterProposalRelationship   `json:"relationships,omitempty"`
	Edits            json.RawMessage                              `json:"edits,omitempty"`
}

// productionCharacterProposalApproveRequest is the request as the body carries it,
// before anything in it is validated. The reference guards only the operation id,
// the selections array and the six-key allowlist here (:210-211) and reads every
// other field inside the transaction, so a malformed selection still loses to a
// stale revision. Raw keeps the body itself, because a repeated approve is compared
// with the stored decision as values (:233).
type productionCharacterProposalApproveRequest struct {
	ProposalBatchID string
	ExpectedRevision *int64
	OperationID     string
	Selections      []any
	Edits           json.RawMessage
	Raw             map[string]any
}

// productionCharacterProposalCreated is one created identity of the approval
// receipt (:27, :320).
type productionCharacterProposalCreated struct {
	SelectionKey string `json:"selectionKey"`
	CharacterID  string `json:"characterId"`
}

// productionCharacterProposalEnvelope is the stored row (character-proposal-service.ts:20-30).
// approvalProof and authorEditProof are carried so an envelope written by the
// reference still decodes; this lane never writes either, and readEnvelope refuses
// one it finds.
type productionCharacterProposalEnvelope struct {
	Version                       int64                              `json:"version"`
	ArchitectureDerivationVersion *int64                             `json:"architectureDerivationVersion,omitempty"`
	ProjectID                     string                             `json:"projectId"`
	Proof                         *productionCharacterProposalProof `json:"proof"`
	Batch                         *productionCharacterProposalBatch `json:"batch"`
	Decision                      json.RawMessage                    `json:"decision,omitempty"`
	Created                       *[]productionCharacterProposalCreated `json:"created,omitempty"`
	AuthorEditProof               json.RawMessage                    `json:"authorEditProof,omitempty"`
	ApprovalProof                 json.RawMessage                    `json:"approvalProof,omitempty"`
}

// productionCharacterProposalAlias is ScopedCharacterAlias
// (src/shared/character-identity.ts:102-109) as this lane reads it.
type productionCharacterProposalAlias struct {
	CharacterID  string
	Name         string
	SourceKey    string
	ValidFrom    int64
	ValidThrough *int64
}

// productionCharacterProposalReceipt is the part of the approval receipt the caller
// receives (character-proposal-service.ts:284, :312-320).
type productionCharacterProposalReceipt struct {
	Revision int64                            `json:"revision"`
	Created  []productionCharacterProposalCreated `json:"created"`
}

// productionCharacterProposalFieldOrigin is one field's entry inside
// static_provenance.fields (src/shared/character-identity.ts:131-134).
type productionCharacterProposalFieldOrigin struct {
	Kind string `json:"kind"`
}

// productionCharacterProposalStaticProvenance is the stored static_provenance
// object: a row-level kind and, for an edited row, a per-field kind that wins.
type productionCharacterProposalStaticProvenance struct {
	Kind   string                                        `json:"kind"`
	Fields map[string]productionCharacterProposalFieldOrigin `json:"fields"`
}

// productionCharacterProposalExisting is one characters row as approve reads it for
// a `map` selection: the eleven identity fields, whether the row is retired, and the
// provenance that decides whether the proposal may overwrite a field.
type productionCharacterProposalExisting struct {
	CharacterID string
	Fields      characterproposal.Fields
	Retired     bool
	Provenance  productionCharacterProposalStaticProvenance
}

// productionCharacterProposals is the borrowed lane. It holds no database: every
// handler borrows the project the frozen context names, so a handler can never
// write a database the admission did not prove.
type productionCharacterProposals struct {
	durable *productionDurableOpen
}

// newProductionCharacterProposals is the constructor production_open_linux.go wires.
func newProductionCharacterProposals(durable *productionDurableOpen) *productionCharacterProposals {
	return &productionCharacterProposals{durable: durable}
}

// Bind installs the four served names and nothing else. A nil or closed bridge
// installs nothing, so every one of them keeps failing closed at
// GO_OPERATION_NOT_MIGRATED instead of being published as a handler that can only
// refuse. The two pending-finalized names are deliberately absent: see the header
// and productionCharacterProposalPendingSeams.
//
// The body guard runs after the borrow, which is the reference's own order: the
// controller proves the project context before it dispatches
// (generation-controller.ts:184-186), and only then does the service read its
// request.
func (c *productionCharacterProposals) Bind(server *transport.Server) error {
	if c == nil || c.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	bindings := []struct {
		Name  string
		Serve func(context.Context, *sql.DB, productionCharacterProposalScope, any) (any, error)
	}{
		{Name: productionCharacterProposalStageName, Serve: func(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
			return c.stage(ctx, db, scope, body)
		}},
		{Name: productionCharacterProposalReadName, Serve: func(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
			return c.read(ctx, db, scope, body)
		}},
		{Name: productionCharacterProposalApproveName, Serve: func(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
			return c.approve(ctx, db, scope, body)
		}},
		{Name: productionCharacterProposalCancelName, Serve: func(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
			return c.cancel(ctx, db, scope, body)
		}},
	}
	for _, binding := range bindings {
		serve := binding.Serve
		if err := server.Bind(binding.Name, func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
			// Wire arity is the request arity (one, generation-controller.ts:208-216)
			// plus the frozen context.
			if len(args) != 2 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			admitted, err := projectsession.ParseContext(args[1])
			if err != nil {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
			}
			if ctx == nil {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			db, release, err := c.durable.borrowAuthorDatabase(ctx, caller, args[1])
			if err != nil {
				// Already symbolic: a lease, ownership or closed-lane refusal.
				return nil, err
			}
			if db == nil || release == nil {
				return nil, errors.New(productionCharacterProposalRequestFailed)
			}
			defer release()
			result, err := serve(ctx, db, productionCharacterProposalScope{ProjectID: admitted.ProjectID, Epoch: admitted.LeaseID}, args[0])
			if err != nil {
				return nil, productionCharacterProposalFailure(err)
			}
			return result, nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// productionCharacterProposalMarshal is the canonical body encoder: no HTML
// escaping (so a model's `<` stays the byte it was) and no trailing newline, which
// is what JSON.stringify produces.
func productionCharacterProposalMarshal(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// productionCharacterProposalEncode is productionCharacterProposalMarshal as text,
// for hashing.
func productionCharacterProposalEncode(value any) (string, error) {
	encoded, err := productionCharacterProposalMarshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// productionCharacterProposalTextHash is the reference's textHash: sha256 hex over
// the UTF-8 bytes (electron/repositories/generation-run-repository.ts).
func productionCharacterProposalTextHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// productionCharacterProposalBatchID is :175: `cpb:` and the hash of the project
// and the canonical source.
func productionCharacterProposalBatchID(projectID string, source *productionCharacterProposalSource) (string, error) {
	encoded, err := productionCharacterProposalEncode([]any{projectID, source})
	if err != nil {
		return "", err
	}
	return "cpb:" + productionCharacterProposalTextHash(encoded), nil
}

// productionCharacterProposalStage is :159-195.
func (c *productionCharacterProposals) stage(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
	source, err := productionCharacterProposalStageBody(body)
	if err != nil {
		return nil, err
	}
	batchID, err := productionCharacterProposalBatchID(scope.ProjectID, source)
	if err != nil {
		return nil, err
	}
	exists, err := productionCharacterProposalExists(ctx, db, batchID)
	if err != nil {
		return nil, err
	}
	if exists {
		// :177-181: the stored envelope, its own re-proof, then the shared read that
		// may refresh the resolution against the live roster.
		envelope, err := productionCharacterProposalReadEnvelope(ctx, db, scope, batchID)
		if err != nil {
			return nil, err
		}
		if _, err := productionCharacterProposalSourceProof(ctx, db, scope, envelope.Batch.Source, true); err != nil {
			return nil, err
		}
		return productionCharacterProposalBatchRead(ctx, db, scope, envelope)
	}
	proof, err := productionCharacterProposalSourceProof(ctx, db, scope, source, true)
	if err != nil {
		return nil, err
	}
	revision, aliases, err := productionCharacterProposalIdentity(ctx, db)
	if err != nil {
		return nil, err
	}
	items, err := productionCharacterProposalResolve(proof.Items, source, scope.ProjectID, revision, aliases)
	if err != nil {
		return nil, err
	}
	version := int64(productionCharacterProposalDerivationVersion)
	envelope := productionCharacterProposalEnvelope{
		Version:                       int64(1),
		ArchitectureDerivationVersion: &version,
		ProjectID:                     scope.ProjectID,
		Proof:                         proof,
		Batch: &productionCharacterProposalBatch{
			ProposalBatchID: batchID,
			Revision:        revision,
			Status:          productionCharacterProposalStatusPending,
			Items:           items,
			Source:          source,
		},
	}
	encoded, err := productionCharacterProposalEncode(&envelope)
	if err != nil {
		return nil, err
	}
	// :191-192. One row, no roster write: this is a staging store, not an author.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO character_identity_proposals(proposal_id,owner_character_id,source_key,source_hash,raw_value,candidate_ids_json) VALUES(?,NULL,?,?,?,?)`,
		batchID, productionCharacterProposalSourceKeyPrefix+scope.ProjectID,
		productionCharacterProposalTextHash(encoded), encoded, "[]"); err != nil {
		return nil, err
	}
	return envelope.Batch, nil
}

// productionCharacterProposalRead is :114-124.
func (c *productionCharacterProposals) read(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
	batchID, err := productionCharacterProposalReadBody(body)
	if err != nil {
		return nil, err
	}
	envelope, err := productionCharacterProposalReadEnvelope(ctx, db, scope, batchID)
	if err != nil {
		return nil, err
	}
	return productionCharacterProposalBatchRead(ctx, db, scope, envelope)
}

// productionCharacterProposalBatchRead is the shared tail of stage and read: a
// pending batch whose stored revision is behind the identity revision is
// re-resolved against the live roster and written back (:117-121).
func productionCharacterProposalBatchRead(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, envelope *productionCharacterProposalEnvelope) (any, error) {
	if envelope.Batch.Status != productionCharacterProposalStatusPending {
		return envelope.Batch, nil
	}
	revision, aliases, err := productionCharacterProposalIdentity(ctx, db)
	if err != nil {
		return nil, err
	}
	if revision == envelope.Batch.Revision {
		return envelope.Batch, nil
	}
	items, err := productionCharacterProposalResolve(envelope.Proof.Items, envelope.Batch.Source, scope.ProjectID, revision, aliases)
	if err != nil {
		return nil, err
	}
	envelope.Batch.Revision = revision
	envelope.Batch.Items = items
	if err := productionCharacterProposalWrite(ctx, db, envelope); err != nil {
		return nil, err
	}
	return envelope.Batch, nil
}

// productionCharacterProposalCancel is :196-208.
func (c *productionCharacterProposals) cancel(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
	batchID, expected, err := productionCharacterProposalCancelBody(body)
	if err != nil {
		return nil, err
	}
	envelope, err := productionCharacterProposalReadEnvelope(ctx, db, scope, batchID)
	if err != nil {
		return nil, err
	}
	if expected != envelope.Batch.Revision {
		return nil, store.ErrCharacterIDRevisionConflict
	}
	if envelope.Batch.Status == productionCharacterProposalStatusApproved {
		return nil, productionCharacterProposalAlreadyApproved
	}
	if envelope.Batch.Status == productionCharacterProposalStatusCancelled {
		return envelope.Batch, nil
	}
	envelope.Batch.Status = productionCharacterProposalStatusCancelled
	if err := productionCharacterProposalWrite(ctx, db, envelope); err != nil {
		return nil, err
	}
	return envelope.Batch, nil
}

// productionCharacterProposalApprove is :209-321, in the reference's own order: the
// body guard, the stored envelope, the edits refusal, the replay arm, the roster and
// revision gates, a fresh proof, the selection count, then the per-selection work.
func (c *productionCharacterProposals) approve(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, body any) (any, error) {
	submitted, err := productionCharacterProposalApproveBody(body)
	if err != nil {
		return nil, err
	}
	envelope, err := productionCharacterProposalReadEnvelope(ctx, db, scope, submitted.ProposalBatchID)
	if err != nil {
		return nil, err
	}
	batch := envelope.Batch
	byKey := make(map[string]*productionCharacterProposalItem, len(batch.Items))
	for index := range batch.Items {
		byKey[batch.Items[index].SelectionKey] = &batch.Items[index]
	}
	// :215-225. An edit is the planning-material author's own draft. The architecture
	// step has no editable source, so a request carrying edits is refused before
	// anything else is decided - even an edit this lane could otherwise parse.
	if submitted.Edits != nil {
		return nil, productionCharacterProposalEditInvalid
	}
	// :232-235. A repeated approve of an already approved batch is a pure no-op when
	// it repeats the same decision, and a nonce conflict when it does not. The
	// comparison is over values, so a body that merely orders its keys differently is
	// the same decision.
	if batch.Status == productionCharacterProposalStatusApproved {
		same, err := productionCharacterProposalSameDecision(envelope.Decision, submitted.Raw)
		if err != nil {
			return nil, err
		}
		if !same {
			return nil, store.ErrCharacterApprovalNonceConflict
		}
		created := []productionCharacterProposalCreated{}
		if envelope.Created != nil {
			created = append(created, (*envelope.Created)...)
		}
		return map[string]any{"batch": batch, "created": created}, nil
	}
	if batch.Status != productionCharacterProposalStatusPending {
		return nil, productionCharacterProposalCancelled
	}
	// :237-238. Only a roster that is ready or empty can receive an adoption: a
	// legacy-repair or inconsistent roster would have to be repaired first.
	roster, err := store.NewCharacters(db).ReadRoster(ctx)
	if err != nil {
		return nil, err
	}
	status, _ := roster["status"].(string)
	if status != "ready" && status != "empty" {
		return nil, store.ErrCharacterAuthorRosterNotReady
	}
	// :239. The revision the browser saw must still be the batch's and still be live.
	revision, _, err := productionCharacterProposalIdentity(ctx, db)
	if err != nil {
		return nil, err
	}
	if submitted.ExpectedRevision == nil || *submitted.ExpectedRevision != batch.Revision || revision != batch.Revision {
		return nil, store.ErrCharacterIDRevisionConflict
	}
	// :240-241. A fresh proof, run for write, is the provenance the commit attributes.
	proof, err := productionCharacterProposalSourceProof(ctx, db, scope, batch.Source, true)
	if err != nil {
		return nil, err
	}
	if proof.Provenance == nil || (proof.Provenance.Kind != productionCharacterProposalProvenanceGenerated &&
		proof.Provenance.Kind != productionCharacterProposalProvenanceDerived) {
		return nil, productionCharacterProposalProvenanceRequired
	}
	// :242. One closed choice per item, each selection key named once.
	if len(submitted.Selections) != len(batch.Items) ||
		!productionCharacterProposalClosedSelections(submitted.Selections, len(batch.Items)) {
		return nil, productionCharacterProposalSelectionInvalid
	}
	characters, err := productionCharacterProposalCharacters(ctx, db)
	if err != nil {
		return nil, err
	}
	commit := store.CharacterIdentityCommitRequest{
		Approval: store.CharacterIdentityApproval{
			OperationID:      submitted.OperationID,
			ExpectedRevision: *submitted.ExpectedRevision,
			Source: &store.CharacterIdentityProvenance{
				Kind: proof.Provenance.Kind,
				Source: store.CharacterIdentitySourceRef{
					ProjectID:   proof.Provenance.Source.ProjectID,
					Epoch:       proof.Provenance.Source.Epoch,
					SourceID:    proof.Provenance.Source.SourceID,
					Revision:    proof.Provenance.Source.Revision,
					ContentHash: proof.Provenance.Source.ContentHash,
				},
				ModelRevision: proof.Provenance.ModelRevision,
			},
			// :249. Both arms this lane serves are the architecture generation, which
			// the reference attributes as generated.
			Action: productionCharacterProposalAdoptGenerated,
		},
		Changes:       []store.CharacterIdentityChange{},
		Creations:     []store.CharacterIdentityCreation{},
		RetireIDs:     []string{},
		Relationships: []store.CharacterIdentityRelationship{},
		Resolutions:   []store.CharacterIdentityResolution{},
	}
	decision := &productionCharacterProposalDecision{
		ProposalBatchID:  submitted.ProposalBatchID,
		ExpectedRevision: submitted.ExpectedRevision,
		OperationID:      submitted.OperationID,
		Selections:       make([]productionCharacterProposalSelection, 0, len(submitted.Selections)),
	}
	// :251-271. Each selection is read and applied in one pass, so a selection the
	// request may not carry is refused by the same code as a selection that does not
	// belong to this batch.
	for _, entry := range submitted.Selections {
		selection, err := productionCharacterProposalSelectionValue(entry)
		if err != nil {
			return nil, err
		}
		item := byKey[selection.SelectionKey]
		if item == nil || !productionCharacterProposalActionAllowed(selection.Action) {
			return nil, productionCharacterProposalSelectionInvalid
		}
		decision.Selections = append(decision.Selections, selection)
		switch selection.Action {
		case productionCharacterProposalActionCreate:
			commit.Creations = append(commit.Creations, store.CharacterIdentityCreation{
				SelectionKey: item.SelectionKey,
				Fields:       productionCharacterProposalFieldList(item.Fields),
			})
		case productionCharacterProposalActionMap:
			existing := productionCharacterProposalLookupExisting(characters, selection.CharacterID)
			if existing == nil {
				return nil, store.ErrCharacterIDUnknown
			}
			adopted := productionCharacterProposalAdoptedFields(item.Fields, *existing)
			if len(adopted) > 0 {
				commit.Changes = append(commit.Changes, store.CharacterIdentityChange{
					CharacterID: existing.CharacterID,
					Fields:      adopted,
				})
			}
		case productionCharacterProposalActionKeep:
		}
	}
	// :272-281. A proposed relationship addresses two selected identities or one
	// selected identity and one existing character, and it has to name a relationship
	// the parser produced for its source item.
	if raw, present := submitted.Raw[productionCharacterProposalRelationsKey]; present {
		entries, ok := raw.([]any)
		if !ok {
			return nil, productionCharacterProposalRelationshipInvalid
		}
		relationships := make([]productionCharacterProposalRelationship, 0, len(entries))
		for _, entry := range entries {
			relationship, err := productionCharacterProposalRelationshipValue(entry)
			if err != nil {
				return nil, err
			}
			row, err := productionCharacterProposalRelationshipRow(decision.Selections, byKey, relationship)
			if err != nil {
				return nil, err
			}
			relationships = append(relationships, relationship)
			commit.Relationships = append(commit.Relationships, row)
		}
		decision.Relationships = &relationships
	}
	// :282-283. The commit re-proves the permit, the payload digest and the nonce;
	// this callback is the request it was given, re-encoded and compared with the one
	// this lane froze, so a candidate that differs by one byte is refused.
	frozen, err := productionCharacterProposalCommitEncoding(commit)
	if err != nil {
		return nil, err
	}
	receipt, err := store.NewCharacterIdentities(db).CommitIdentities(ctx, commit,
		func(candidate store.CharacterIdentityCommitRequest) bool {
			encoded, encodeErr := productionCharacterProposalCommitEncoding(candidate)
			return encodeErr == nil && encoded == frozen
		})
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, productionCharacterProposalReceiptInvalid
	}
	stored := productionCharacterProposalReceipt{Created: []productionCharacterProposalCreated{}}
	encoded, err := productionCharacterProposalEncode(receipt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(encoded), &stored); err != nil {
		return nil, productionCharacterProposalReceiptInvalid
	}
	created := stored.Created
	if created == nil {
		created = []productionCharacterProposalCreated{}
	}
	// :311. The projection is refreshed after the commit and before the envelope, so
	// a reader never sees an approved batch whose roster rendering is behind.
	if err := store.NewCharacters(db).RefreshIdentityProjection(ctx); err != nil {
		return nil, err
	}
	// :312-313, :319-320.
	batch.Status = productionCharacterProposalStatusApproved
	batch.Revision = stored.Revision
	batch.ApprovalOperationID = submitted.OperationID
	// :319-320. The decision the envelope records is the request the browser sent, so a
	// replay compares like with like rather than against a normalized struct.
	decisionJSON, err := productionCharacterProposalMarshal(submitted.Raw)
	if err != nil {
		return nil, productionCharacterProposalReceiptInvalid
	}
	envelope.Decision = decisionJSON
	envelope.Created = &created
	if err := productionCharacterProposalWrite(ctx, db, envelope); err != nil {
		return nil, err
	}
	return map[string]any{"batch": batch, "created": created}, nil
}

// productionCharacterProposalStageBody is the `{ source }` unwrap of
// generation-controller.ts:208-211. The controller refuses a legacy roster source
// itself, before the service sees the request, so that refusal belongs here too.
func productionCharacterProposalStageBody(value any) (*productionCharacterProposalSource, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, productionCharacterProposalSourceInvalid
	}
	raw, present := object[productionCharacterProposalSourceKey]
	if !present {
		return nil, productionCharacterProposalSourceInvalid
	}
	return productionCharacterProposalSourceValue(raw)
}

// productionCharacterProposalSourceValue is the source guard of
// generation-character-proposal-proof.ts:46 and :136-139 followed by this lane's
// capability check. Every key of a served source is read; a source this lane cannot
// prove is refused by name rather than by silently dropping the keys it does not
// understand.
func productionCharacterProposalSourceValue(value any) (*productionCharacterProposalSource, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, productionCharacterProposalSourceInvalid
	}
	kind, _ := object["kind"].(string)
	if kind == productionCharacterProposalLegacyKind {
		return nil, productionCharacterProposalLegacyAdmission
	}
	if kind != productionCharacterProposalGenerationKind {
		return nil, productionCharacterProposalSourceProofUnavailable
	}
	for key := range object {
		switch key {
		case "kind", "inputKind", "handle", "artifacts", "manifestArtifactId":
		default:
			return nil, productionCharacterProposalSourceInvalid
		}
	}
	inputKind, ok := object["inputKind"].(string)
	if !ok || inputKind != productionCharacterProposalArchitectureKind {
		return nil, productionCharacterProposalSourceProofUnavailable
	}
	handleObject, ok := object["handle"].(map[string]any)
	if !ok {
		return nil, productionCharacterProposalSourceInvalid
	}
	handle := &productionCharacterProposalHandle{}
	for key := range handleObject {
		switch key {
		case "projectId":
			handle.ProjectID, _ = handleObject[key].(string)
		case "epoch":
			handle.Epoch, _ = handleObject[key].(string)
		case "rootActionId":
			handle.RootActionID, _ = handleObject[key].(string)
		case "runId":
			handle.RunID, _ = handleObject[key].(string)
		default:
			// The reference never allowlists the handle's keys; it compares the whole
			// object with the run's own handle instead (:159), so an extra key can
			// never match. This lane answers the same refusal, spelled as the
			// source's own invalid code.
			return nil, productionCharacterProposalSourceInvalid
		}
	}
	if handle.ProjectID == "" || handle.Epoch == "" || handle.RootActionID == "" || handle.RunID == "" {
		return nil, productionCharacterProposalSourceInvalid
	}
	values, ok := object["artifacts"].([]any)
	if !ok || len(values) == 0 || len(values) > 64 {
		return nil, productionCharacterProposalSourceInvalid
	}
	artifacts := make([]productionCharacterProposalArtifactRef, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, entry := range values {
		reference, ok := entry.(map[string]any)
		if !ok {
			return nil, productionCharacterProposalArtifactInvalid
		}
		for key := range reference {
			switch key {
			case "artifactId", "revision", "textHash":
			default:
				return nil, productionCharacterProposalArtifactInvalid
			}
		}
		artifactID, _ := reference["artifactId"].(string)
		textHash, _ := reference["textHash"].(string)
		revision, ok := productionCharacterProposalInteger(reference["revision"])
		if !ok || strings.TrimSpace(artifactID) == "" || seen[artifactID] {
			return nil, productionCharacterProposalArtifactInvalid
		}
		seen[artifactID] = true
		artifacts = append(artifacts, productionCharacterProposalArtifactRef{
			ArtifactID: artifactID,
			Revision:   revision,
			TextHash:   textHash,
		})
	}
	source := &productionCharacterProposalSource{
		Kind:      kind,
		InputKind: inputKind,
		Handle:    handle,
		Artifacts: artifacts,
	}
	if manifest, present := object["manifestArtifactId"]; present {
		text, ok := manifest.(string)
		if !ok {
			return nil, productionCharacterProposalSourceInvalid
		}
		source.ManifestArtifactID = text
	}
	return source, nil
}

// productionCharacterProposalReadBody is read's single key. The reference reads it
// without a guard (:114-116), so a body that is not `{ proposalBatchId }` is
// answered by the row lookup - which is exactly what this lane's receipt check
// answers.
func productionCharacterProposalReadBody(value any) (string, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return "", productionCharacterProposalReceiptInvalid
	}
	batchID, ok := object[productionCharacterProposalBatchKey].(string)
	if !ok || strings.TrimSpace(batchID) == "" {
		return "", productionCharacterProposalReceiptInvalid
	}
	return batchID, nil
}

// productionCharacterProposalCancelBody is :197-199: a non-empty batch id, a safe
// integer revision, and no other key.
func productionCharacterProposalCancelBody(value any) (string, int64, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return "", 0, productionCharacterProposalApprovalInvalid
	}
	for key := range object {
		if key != productionCharacterProposalBatchKey && key != productionCharacterProposalRevisionKey {
			return "", 0, productionCharacterProposalApprovalInvalid
		}
	}
	batchID, ok := object[productionCharacterProposalBatchKey].(string)
	if !ok || strings.TrimSpace(batchID) == "" {
		return "", 0, productionCharacterProposalApprovalInvalid
	}
	revision, ok := productionCharacterProposalInteger(object[productionCharacterProposalRevisionKey])
	if !ok || revision < 0 {
		return "", 0, productionCharacterProposalApprovalInvalid
	}
	return batchID, revision, nil
}

// productionCharacterProposalApproveBody is :210-211, the guard that runs before
// anything is read: a non-empty operation id of at most 256 characters, a
// selections array, and no key outside the six the request declares. Every other
// rule - the selection's own keys, its action, its character id - belongs to the
// loop that has the batch in hand (:251-254), so a stale revision still wins over a
// malformed selection.
func productionCharacterProposalApproveBody(value any) (*productionCharacterProposalApproveRequest, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, productionCharacterProposalApprovalInvalid
	}
	for key := range object {
		switch key {
		case productionCharacterProposalBatchKey, productionCharacterProposalRevisionKey,
			productionCharacterProposalOperationKey, productionCharacterProposalSelectionsKey,
			productionCharacterProposalRelationsKey, productionCharacterProposalEditsKey:
		default:
			return nil, productionCharacterProposalApprovalInvalid
		}
	}
	operationID, ok := object[productionCharacterProposalOperationKey].(string)
	if !ok || strings.TrimSpace(operationID) == "" || utf8.RuneCountInString(operationID) > productionCharacterProposalOperationMax {
		return nil, productionCharacterProposalApprovalInvalid
	}
	selections, ok := object[productionCharacterProposalSelectionsKey].([]any)
	if !ok {
		return nil, productionCharacterProposalApprovalInvalid
	}
	// The reference reads proposalBatchId without a guard and lets the row lookup
	// answer for a body that has none. This lane needs a string to look up, so an
	// absent or non-text id is answered by the same receipt refusal the lookup
	// would have given it.
	batchID, ok := object[productionCharacterProposalBatchKey].(string)
	if !ok || strings.TrimSpace(batchID) == "" {
		return nil, productionCharacterProposalReceiptInvalid
	}
	request := &productionCharacterProposalApproveRequest{
		ProposalBatchID: batchID,
		OperationID:     operationID,
		Selections:      selections,
		Raw:             object,
	}
	if revision, present := object[productionCharacterProposalRevisionKey]; present {
		expected, ok := productionCharacterProposalInteger(revision)
		if ok {
			request.ExpectedRevision = &expected
		}
		// A revision that is not a safe integer stays absent, which the revision gate
		// answers with the reference's own conflict (:239).
	}
	if raw, present := object[productionCharacterProposalEditsKey]; present {
		encoded, err := productionCharacterProposalMarshal(raw)
		if err != nil {
			return nil, productionCharacterProposalEditInvalid
		}
		request.Edits = json.RawMessage(encoded)
	}
	return request, nil
}

// productionCharacterProposalSelectionValue is one selection as the body carries it.
// The action decides which keys are read, and an unknown key is refused here only
// because the reference's allowlist is per selection (:253-254); the selection's
// membership in the batch is the loop's business.
func productionCharacterProposalSelectionValue(value any) (productionCharacterProposalSelection, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return productionCharacterProposalSelection{}, productionCharacterProposalSelectionInvalid
	}
	key, ok := object[productionCharacterProposalSelectionKeyTag].(string)
	if !ok || strings.TrimSpace(key) == "" {
		return productionCharacterProposalSelection{}, productionCharacterProposalSelectionInvalid
	}
	action, ok := object[productionCharacterProposalActionKey].(string)
	if !ok {
		return productionCharacterProposalSelection{}, productionCharacterProposalSelectionInvalid
	}
	for field := range object {
		switch field {
		case productionCharacterProposalSelectionKeyTag, productionCharacterProposalActionKey:
		case productionCharacterProposalCharacterKey:
			if action != productionCharacterProposalActionMap {
				return productionCharacterProposalSelection{}, productionCharacterProposalSelectionInvalid
			}
		default:
			return productionCharacterProposalSelection{}, productionCharacterProposalSelectionInvalid
		}
	}
	selection := productionCharacterProposalSelection{SelectionKey: key, Action: action}
	if raw, present := object[productionCharacterProposalCharacterKey]; present {
		text, ok := raw.(string)
		if !ok {
			return productionCharacterProposalSelection{}, productionCharacterProposalSelectionInvalid
		}
		characterID := text
		selection.CharacterID = &characterID
	}
	return selection, nil
}

// productionCharacterProposalRelationshipValue is one proposed relationship. The
// reference reads four keys off it without an allowlist (:272-281), so unknown keys
// are ignored and a value that is not an object is the relationship refusal.
func productionCharacterProposalRelationshipValue(value any) (productionCharacterProposalRelationship, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return productionCharacterProposalRelationship{}, productionCharacterProposalRelationshipInvalid
	}
	relationship := productionCharacterProposalRelationship{}
	for _, key := range []string{"sourceSelectionKey", "targetSelectionKey", "targetCharacterId", "relation"} {
		raw, present := object[key]
		if !present {
			continue
		}
		text, ok := raw.(string)
		if !ok {
			return productionCharacterProposalRelationship{}, productionCharacterProposalRelationshipInvalid
		}
		switch key {
		case "sourceSelectionKey":
			relationship.SourceSelectionKey = text
		case "targetSelectionKey":
			relationship.TargetSelectionKey = text
		case "targetCharacterId":
			relationship.TargetCharacterID = text
		case "relation":
			relationship.Relation = text
		}
	}
	return relationship, nil
}

// productionCharacterProposalInteger is Number.isSafeInteger for a JSON value.
func productionCharacterProposalInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		if integer, err := number.Int64(); err == nil {
			return integer, true
		}
		return 0, false
	case float64:
		if number != float64(int64(number)) || number > 9007199254740991 || number < -9007199254740991 {
			return 0, false
		}
		return int64(number), true
	case int64:
		return number, true
	case int:
		return int64(number), true
	}
	return 0, false
}

// productionCharacterProposalClosedSelections is :242: one selection per item, and
// no selection key named twice.
func productionCharacterProposalClosedSelections(selections []any, total int) bool {
	if len(selections) != total {
		return false
	}
	seen := make(map[string]bool, len(selections))
	for _, entry := range selections {
		key := ""
		if object, ok := entry.(map[string]any); ok {
			key, _ = object[productionCharacterProposalSelectionKeyTag].(string)
		}
		if key == "" || seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

// productionCharacterProposalActionAllowed is the action membership of :253.
func productionCharacterProposalActionAllowed(action string) bool {
	switch action {
	case productionCharacterProposalActionCreate, productionCharacterProposalActionMap, productionCharacterProposalActionKeep:
		return true
	}
	return false
}

// productionCharacterProposalSameDecision is :233: the stored decision and the
// request are compared as values, the way the reference's deep-equality helper does.
// Both sides are canonicalized through one encoder with object keys sorted, so key
// order, a nil-versus-empty slice and a number written as 0 or 0.0 are not
// differences; a different selection, action or operation id is.
// productionCharacterProposalSameDecision compares the request a batch was approved
// with the one a repeated approval submits, as values rather than as bytes. Both
// sides are decoded with json.Number and re-encoded by the same encoder, so a body
// that merely orders its keys differently - or spells the same integer the same way -
// is the same decision. The stored side is the request itself (:233), never a
// normalized struct: a struct would drop a key the request carried and turn an honest
// replay into a nonce conflict.
func productionCharacterProposalSameDecision(stored json.RawMessage, raw map[string]any) (bool, error) {
	if len(stored) == 0 || raw == nil {
		return false, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(stored)))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return false, nil
	}
	left, err := productionCharacterProposalEncode(decoded)
	if err != nil {
		return false, err
	}
	right, err := productionCharacterProposalEncode(raw)
	if err != nil {
		return false, err
	}
	return left == right, nil
}

// productionCharacterProposalRelationshipRow is one committed relationship from a
// proposed one (:272-281): both endpoints are resolved against this request's own
// selections, and the proposal has to name the relationship the parser produced.
func productionCharacterProposalRelationshipRow(selections []productionCharacterProposalSelection,
	byKey map[string]*productionCharacterProposalItem, relationship productionCharacterProposalRelationship) (store.CharacterIdentityRelationship, error) {
	refusal := store.CharacterIdentityRelationship{}
	from := productionCharacterProposalSelectionOf(selections, relationship.SourceSelectionKey)
	to := productionCharacterProposalSelectionOf(selections, relationship.TargetSelectionKey)
	// A relationship is proposed between two selected characters, and it addresses
	// either a selected slot or an existing id - never both, never neither.
	if from == nil || from.Action == productionCharacterProposalActionKeep ||
		(to != nil && to.Action == productionCharacterProposalActionKeep) ||
		(to != nil) == (relationship.TargetCharacterID != "") {
		return refusal, productionCharacterProposalRelationshipInvalid
	}
	item := byKey[from.SelectionKey]
	if item == nil || !productionCharacterProposalHasRelationship(item.Relationships, relationship) {
		return refusal, productionCharacterProposalRelationshipInvalid
	}
	row := store.CharacterIdentityRelationship{Relation: relationship.Relation}
	if from.Action == productionCharacterProposalActionCreate {
		row.SourceSelectionKey = from.SelectionKey
	} else if from.CharacterID != nil {
		row.SourceCharacterID = *from.CharacterID
	}
	if to != nil {
		if to.Action == productionCharacterProposalActionCreate {
			row.TargetSelectionKey = to.SelectionKey
		} else if to.CharacterID != nil {
			row.TargetCharacterID = *to.CharacterID
		}
	} else {
		row.TargetCharacterID = relationship.TargetCharacterID
	}
	return row, nil
}

// productionCharacterProposalSelectionOf is the reference's
// `request.selections.find(...)`.
func productionCharacterProposalSelectionOf(selections []productionCharacterProposalSelection, selectionKey string) *productionCharacterProposalSelection {
	for index := range selections {
		if selections[index].SelectionKey == selectionKey {
			return &selections[index]
		}
	}
	return nil
}

// productionCharacterProposalHasRelationship is :277-278: the proposal must name a
// relation the parser produced for the source item, addressed the same way.
func productionCharacterProposalHasRelationship(relations []characterproposal.Relationship, relationship productionCharacterProposalRelationship) bool {
	for _, relation := range relations {
		if relation.Relation != relationship.Relation {
			continue
		}
		if relation.TargetSelectionKey != "" {
			if relation.TargetSelectionKey == relationship.TargetSelectionKey {
				return true
			}
			continue
		}
		if relation.TargetName != "" {
			return true
		}
	}
	return false
}

// productionCharacterProposalFieldList is one creation's fields in the roster's
// declared order (character-proposal-service.ts:255).
func productionCharacterProposalFieldList(fields characterproposal.Fields) []store.CharacterIdentityField {
	out := make([]store.CharacterIdentityField, 0, len(characterproposal.StaticFields))
	for _, key := range characterproposal.StaticFields {
		out = append(out, store.CharacterIdentityField{Key: key, Value: productionCharacterProposalFieldValue(fields, key)})
	}
	return out
}

// productionCharacterProposalFieldValue is one field of a typed static-field set.
func productionCharacterProposalFieldValue(fields characterproposal.Fields, key string) string {
	switch key {
	case "name":
		return fields.Name
	case "role":
		return fields.Role
	case "gender":
		return fields.Gender
	case "age":
		return fields.Age
	case "appearance":
		return fields.Appearance
	case "personality":
		return fields.Personality
	case "background":
		return fields.Background
	case "abilities":
		return fields.Abilities
	case "motivation":
		return fields.Motivation
	case "arc":
		return fields.Arc
	case "notes":
		return fields.Notes
	}
	return ""
}

// productionCharacterProposalAdoptedFields is :261-265: a mapped identity adopts
// only the fields that differ and that no author or legacy origin protects.
func productionCharacterProposalAdoptedFields(item characterproposal.Fields, existing productionCharacterProposalExisting) []store.CharacterIdentityField {
	adopted := make([]store.CharacterIdentityField, 0, len(characterproposal.StaticFields))
	for _, key := range characterproposal.StaticFields {
		value := productionCharacterProposalFieldValue(item, key)
		current := productionCharacterProposalFieldValue(existing.Fields, key)
		origin := existing.Provenance.Kind
		if field, ok := existing.Provenance.Fields[key]; ok && field.Kind != "" {
			origin = field.Kind
		}
		if current == value {
			continue
		}
		if strings.TrimSpace(current) != "" && (origin == "author" || origin == "legacy") {
			continue
		}
		adopted = append(adopted, store.CharacterIdentityField{Key: key, Value: value})
	}
	return adopted
}

// productionCharacterProposalLookupExisting is the map selection's lookup (:258): the
// id must name a live character, and a missing id can never match one. The entries are
// the roster this approval read a moment earlier, and a retired row is never adoptable
// however well its id matches.
func productionCharacterProposalLookupExisting(characters []productionCharacterProposalExisting, characterID *string) *productionCharacterProposalExisting {
	if characterID == nil {
		return nil
	}
	for index := range characters {
		if characters[index].CharacterID != *characterID || characters[index].Retired {
			continue
		}
		return &characters[index]
	}
	return nil
}

// productionCharacterProposalResolve is :103-113. Names alone are never proof of
// identity, and the scoped lookup is retried without the source key so a proposal
// can still point at a character this project already knows by name.
func productionCharacterProposalResolve(items []characterproposal.Item, source *productionCharacterProposalSource,
	projectID string, revision int64, aliases []productionCharacterProposalAlias) ([]productionCharacterProposalItem, error) {
	resolved := make([]productionCharacterProposalItem, 0, len(items))
	for _, item := range items {
		resolution, err := productionCharacterProposalResolveScoped(aliases, item.Fields.Name, projectID, item.SourceID, revision)
		if err != nil {
			return nil, err
		}
		if source != nil && source.Kind != productionCharacterProposalFinalizedKind && resolution.Status == "unresolved" {
			resolution, err = productionCharacterProposalResolveScoped(aliases, item.Fields.Name, projectID, "", revision)
			if err != nil {
				return nil, err
			}
		}
		resolved = append(resolved, productionCharacterProposalItem{Item: item, Resolution: resolution})
	}
	return resolved, nil
}

// productionCharacterProposalResolveScoped is resolveScopedCharacterIdentity
// (src/shared/character-identity.ts:116-130): exact name, project and version
// scoped, with a source key narrowing the match to one proposal source.
func productionCharacterProposalResolveScoped(aliases []productionCharacterProposalAlias, name, projectID, sourceKey string, revision int64) (productionCharacterProposalResolution, error) {
	if strings.TrimSpace(name) == "" || strings.TrimSpace(projectID) == "" || revision < 0 || revision > 9007199254740991 {
		return productionCharacterProposalResolution{}, productionCharacterProposalLookupInvalid
	}
	seen := make(map[string]bool, len(aliases))
	candidates := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if strings.TrimSpace(alias.CharacterID) == "" || strings.TrimSpace(alias.Name) == "" ||
			strings.TrimSpace(alias.SourceKey) == "" || alias.ValidFrom < 0 ||
			(alias.ValidThrough != nil && (*alias.ValidThrough < alias.ValidFrom || *alias.ValidThrough < 0)) {
			return productionCharacterProposalResolution{}, productionCharacterProposalLookupInvalid
		}
		if alias.Name != name || alias.ValidFrom > revision {
			continue
		}
		if alias.ValidThrough != nil && revision > *alias.ValidThrough {
			continue
		}
		if sourceKey != "" && alias.SourceKey != sourceKey {
			continue
		}
		if seen[alias.CharacterID] {
			continue
		}
		seen[alias.CharacterID] = true
		candidates = append(candidates, alias.CharacterID)
	}
	sort.Strings(candidates)
	if len(candidates) > 1 {
		return productionCharacterProposalResolution{Status: "ambiguous", CandidateIDs: candidates}, nil
	}
	if len(candidates) == 1 && sourceKey != "" {
		return productionCharacterProposalResolution{Status: "resolved", CharacterID: candidates[0]}, nil
	}
	return productionCharacterProposalResolution{Status: "unresolved", CandidateIDs: candidates}, nil
}

// productionCharacterProposalIdentity is the reference's identitySnapshot
// (character-proposal-service.ts:323-346): the identity revision and the aliases of
// every live character, in one consistent read.
func productionCharacterProposalIdentity(ctx context.Context, db *sql.DB) (int64, []productionCharacterProposalAlias, error) {
	var revision int64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM character_identity_meta WHERE id='main'`).Scan(&revision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil, store.ErrCharacterIDMetaMissing
		}
		return 0, nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT a.character_id,a.name,a.source_key,a.valid_from,a.valid_through
		   FROM character_aliases a JOIN characters c ON c.character_id=a.character_id
		  WHERE c.retired=0 ORDER BY a.character_id,a.name,a.valid_from`)
	if err != nil {
		return 0, nil, err
	}
	defer rows.Close()
	aliases := make([]productionCharacterProposalAlias, 0, 32)
	for rows.Next() {
		var alias productionCharacterProposalAlias
		var through sql.NullInt64
		if err := rows.Scan(&alias.CharacterID, &alias.Name, &alias.SourceKey, &alias.ValidFrom, &through); err != nil {
			return 0, nil, err
		}
		if through.Valid {
			value := through.Int64
			alias.ValidThrough = &value
		}
		aliases = append(aliases, alias)
	}
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return revision, aliases, nil
}

// productionCharacterProposalCharacters reads the live roster rows a map selection
// needs. It is a read: approve never writes a characters row itself.
func productionCharacterProposalCharacters(ctx context.Context, db *sql.DB) ([]productionCharacterProposalExisting, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT character_id,name,role,gender,age,appearance,personality,background,abilities,
		        motivation,arc,notes,retired,static_provenance
		   FROM characters ORDER BY character_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	characters := make([]productionCharacterProposalExisting, 0, 32)
	for rows.Next() {
		var characterID string
		var fields [11]sql.NullString
		var retired int64
		var provenance string
		if err := rows.Scan(&characterID, &fields[0], &fields[1], &fields[2], &fields[3],
			&fields[4], &fields[5], &fields[6], &fields[7], &fields[8], &fields[9], &fields[10],
			&retired, &provenance); err != nil {
			return nil, err
		}
		existing := productionCharacterProposalExisting{
			CharacterID: characterID,
			Retired:     retired != 0,
			Fields: characterproposal.Fields{
				Name:        fields[0].String,
				Role:        fields[1].String,
				Gender:      fields[2].String,
				Age:         fields[3].String,
				Appearance:  fields[4].String,
				Personality: fields[5].String,
				Background:  fields[6].String,
				Abilities:   fields[7].String,
				Motivation:  fields[8].String,
				Arc:         fields[9].String,
				Notes:       fields[10].String,
			},
		}
		if strings.TrimSpace(provenance) != "" {
			if err := json.Unmarshal([]byte(provenance), &existing.Provenance); err != nil {
				return nil, productionCharacterProposalLookupInvalid
			}
		}
		characters = append(characters, existing)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return characters, nil
}

// productionCharacterProposalExists is :176.
func productionCharacterProposalExists(ctx context.Context, db *sql.DB, proposalBatchID string) (bool, error) {
	var found int64
	err := db.QueryRowContext(ctx, `SELECT 1 FROM character_identity_proposals WHERE proposal_id=?`, proposalBatchID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// productionCharacterProposalReadEnvelope is :60-102.
func productionCharacterProposalReadEnvelope(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope, proposalBatchID string) (*productionCharacterProposalEnvelope, error) {
	var sourceKey, sourceHash, raw string
	err := db.QueryRowContext(ctx,
		`SELECT source_key,source_hash,raw_value FROM character_identity_proposals WHERE proposal_id=?`, proposalBatchID).
		Scan(&sourceKey, &sourceHash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, productionCharacterProposalReceiptInvalid
	}
	if err != nil {
		return nil, err
	}
	// :62. The row is trusted only when it belongs to this project and its bytes still
	// hash to the hash recorded beside them, so a hand-edited envelope is refused
	// rather than served.
	if sourceKey != productionCharacterProposalSourceKeyPrefix+scope.ProjectID ||
		productionCharacterProposalTextHash(raw) != sourceHash {
		return nil, productionCharacterProposalReceiptInvalid
	}
	var envelope productionCharacterProposalEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, productionCharacterProposalReceiptInvalid
	}
	if envelope.Version != 1 || envelope.ProjectID != scope.ProjectID ||
		envelope.Batch == nil || envelope.Batch.ProposalBatchID != proposalBatchID {
		return nil, productionCharacterProposalReceiptInvalid
	}
	// :67-68. Version 2 is the only derivation this lane stages, and it only ever
	// describes an architecture generation.
	if envelope.ArchitectureDerivationVersion != nil &&
		(*envelope.ArchitectureDerivationVersion != productionCharacterProposalDerivationVersion ||
			envelope.Batch.Source == nil ||
			envelope.Batch.Source.Kind != productionCharacterProposalGenerationKind ||
			envelope.Batch.Source.InputKind != productionCharacterProposalArchitectureKind) {
		return nil, productionCharacterProposalReceiptInvalid
	}
	// :69-79. Re-proving the source is what makes a staged proposal evidence: the
	// artifacts it was parsed from must still be the ones it was parsed from.
	current, err := productionCharacterProposalSourceProof(ctx, db, scope, envelope.Batch.Source, false)
	if err != nil {
		return nil, err
	}
	stored, err := productionCharacterProposalEncode(envelope.Proof)
	if err != nil {
		return nil, err
	}
	fresh, err := productionCharacterProposalEncode(current)
	if err != nil {
		return nil, err
	}
	if stored != fresh {
		return nil, productionCharacterProposalSourceChanged
	}
	// :85-92. This lane writes no authorEditProof and no approvalProof, so an
	// envelope carrying one was not written by this lane and is not served.
	if len(envelope.AuthorEditProof) > 0 || len(envelope.ApprovalProof) > 0 {
		return nil, productionCharacterProposalApprovalInvalid
	}
	if envelope.Batch.Status == productionCharacterProposalStatusApproved {
		if err := productionCharacterProposalVerifyApproval(ctx, db, &envelope); err != nil {
			return nil, err
		}
	}
	return &envelope, nil
}

// productionCharacterProposalVerifyApproval is :81-92 for an adoption this lane
// wrote: the approval receipt must still exist and must still be the revision the
// batch records.
func productionCharacterProposalVerifyApproval(ctx context.Context, db *sql.DB, envelope *productionCharacterProposalEnvelope) error {
	var receiptJSON string
	err := db.QueryRowContext(ctx,
		`SELECT receipt_json FROM character_identity_approvals WHERE operation_id=?`, envelope.Batch.ApprovalOperationID).Scan(&receiptJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return productionCharacterProposalApprovalInvalid
	}
	if err != nil {
		return err
	}
	if len(envelope.Decision) == 0 || string(envelope.Decision) == "null" {
		return productionCharacterProposalApprovalInvalid
	}
	var receipt productionCharacterProposalReceipt
	if err := json.Unmarshal([]byte(receiptJSON), &receipt); err != nil {
		return productionCharacterProposalApprovalInvalid
	}
	if receipt.Revision != envelope.Batch.Revision {
		return productionCharacterProposalApprovalInvalid
	}
	return nil
}

// productionCharacterProposalWrite is :41-44. A row that is no longer there is
// refused rather than silently counted as a write, which is a lane-side
// tightening: the reference ignores the changed-row count because its own
// transaction has just read the row.
func productionCharacterProposalWrite(ctx context.Context, db *sql.DB, envelope *productionCharacterProposalEnvelope) error {
	encoded, err := productionCharacterProposalEncode(envelope)
	if err != nil {
		return err
	}
	result, err := db.ExecContext(ctx,
		`UPDATE character_identity_proposals SET source_hash=?,raw_value=? WHERE proposal_id=?`,
		productionCharacterProposalTextHash(encoded), encoded, envelope.Batch.ProposalBatchID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return productionCharacterProposalReceiptInvalid
	}
	return nil
}

// productionCharacterProposalSourceProof is
// generation-character-proposal-proof.ts:44-171. Only the generation/architecture
// arm is served; every other source kind is refused with this lane's own code
// because the proof port it needs does not exist here (see the header).
//
// forWrite is the reference's flag for the assertions that only make sense before a
// write. Both arms here compute the same proof: the two assertions it guards
// (assertSources and assertBlueprintRecoveryWritable) prove blueprint and draft
// writability this commit does not touch, and the lease check the reference makes
// (:164, run epoch against the project epoch) cannot be made here because this
// lane's project epoch is a per-save lease (production_composition_linux.go:437-441).
func productionCharacterProposalSourceProof(ctx context.Context, db *sql.DB, scope productionCharacterProposalScope,
	source *productionCharacterProposalSource, forWrite bool) (*productionCharacterProposalProof, error) {
	if source == nil {
		return nil, productionCharacterProposalSourceInvalid
	}
	if source.Kind != productionCharacterProposalGenerationKind || source.InputKind != productionCharacterProposalArchitectureKind {
		return nil, productionCharacterProposalSourceProofUnavailable
	}
	if source.Handle == nil || len(source.Artifacts) == 0 || len(source.Artifacts) > 64 {
		return nil, productionCharacterProposalSourceInvalid
	}
	ledger := store.NewGenerationLedger(db, nil, nil)
	value, err := syntheticCall(ctx, ledger, "get", source.Handle.RunID)
	if err != nil {
		return nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, productionCharacterProposalRunMissing
	}
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return nil, productionCharacterProposalSourceInvalid
	}
	// :141 and :159. The handle is the run's own handle, or this source names a run
	// it does not own.
	if syntheticString(binding["projectId"]) != scope.ProjectID || source.Handle.ProjectID != scope.ProjectID ||
		syntheticString(run["rootActionId"]) != source.Handle.RootActionID ||
		syntheticString(run["runId"]) != source.Handle.RunID ||
		syntheticString(binding["epoch"]) != source.Handle.Epoch {
		return nil, productionCharacterProposalRunIdentityMismatch
	}
	manifest := syntheticMap(binding["sourceManifest"])
	if manifest == nil {
		return nil, productionCharacterProposalManifestInvalid
	}
	// :144-151. Every referenced artifact has to be a settled answer of this very
	// run; the receipt read re-hashes the text and re-checks the identity the ledger
	// froze when the artifact was written.
	artifacts := make([]characterproposal.Artifact, 0, len(source.Artifacts))
	for _, reference := range source.Artifacts {
		var runID, attemptID, status string
		err := db.QueryRowContext(ctx,
			`SELECT run_id,attempt_id,status FROM generation_artifacts WHERE artifact_id=?`, reference.ArtifactID).
			Scan(&runID, &attemptID, &status)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, productionCharacterProposalArtifactInvalid
		}
		if err != nil {
			return nil, err
		}
		if runID != source.Handle.RunID || status == "discarded" {
			return nil, productionCharacterProposalArtifactInvalid
		}
		value, err := syntheticCall(ctx, ledger, "receipt", attemptID)
		if err != nil {
			// A ledger refusal is already symbolic (ARTIFACT_INTEGRITY_FAILED and
			// friends); it is forwarded by the refusal mapper, never reworded.
			return nil, err
		}
		receipt := syntheticMap(value)
		artifact := syntheticMap(receipt["artifact"])
		attempt := syntheticMap(receipt["attempt"])
		result := syntheticMap(receipt["result"])
		if artifact == nil ||
			reference.Revision != syntheticNumber(artifact["revision"]) ||
			reference.TextHash != syntheticString(artifact["textHash"]) ||
			!productionCharacterProposalAttemptStatus(syntheticString(attempt["status"])) ||
			syntheticString(result["finishReason"]) != "stop" {
			return nil, productionCharacterProposalArtifactInvalid
		}
		artifacts = append(artifacts, characterproposal.Artifact{
			ArtifactID: syntheticString(artifact["artifactId"]),
			Text:       syntheticString(artifact["text"]),
		})
	}
	// :153-156. The manifest is one of the artifacts, and the run is an architecture
	// run; the parser then has to find exactly one detail per slot.
	index := -1
	for position, artifact := range artifacts {
		if artifact.ArtifactID == source.ManifestArtifactID {
			index = position
			break
		}
	}
	if index < 0 || syntheticString(manifest["operation"]) != productionCharacterProposalArchitectureOperation {
		return nil, productionCharacterProposalManifestInvalid
	}
	details := make([]characterproposal.Artifact, 0, len(artifacts))
	for position, artifact := range artifacts {
		if position != index {
			details = append(details, artifact)
		}
	}
	items, err := characterproposal.ParseArchitecture(artifacts[index], details)
	if err != nil {
		return nil, err
	}
	// :168-171. The hash covers the source and the parsed items, and the provenance
	// names the run the artifacts came from.
	sourceHash, err := productionCharacterProposalSourceHash(source, items)
	if err != nil {
		return nil, err
	}
	modelRevision := ""
	if receipt := syntheticMap(manifest["modelReceipt"]); receipt != nil {
		modelRevision = syntheticString(receipt["modelRevision"])
	}
	return &productionCharacterProposalProof{
		Items:      items,
		SourceHash: sourceHash,
		Provenance: &productionCharacterProposalProvenance{
			Kind:          productionCharacterProposalProvenanceGenerated,
			ModelRevision: modelRevision,
			Source: productionCharacterProposalRef{
				ProjectID:   scope.ProjectID,
				Epoch:       source.Handle.Epoch,
				SourceID:    source.Handle.RunID,
				Revision:    0,
				ContentHash: sourceHash,
			},
		},
	}, nil
}

// productionCharacterProposalSourceHash is :169: the hash of the source and the
// items it parsed to.
func productionCharacterProposalSourceHash(source *productionCharacterProposalSource, items []characterproposal.Item) (string, error) {
	encoded, err := productionCharacterProposalEncode([]any{source, items})
	if err != nil {
		return "", err
	}
	return productionCharacterProposalTextHash(encoded), nil
}

// productionCharacterProposalAttemptStatus is :149: only a settled or unknown
// attempt may supply an artifact.
func productionCharacterProposalAttemptStatus(status string) bool {
	return status == "settled" || status == "unknown"
}

// productionCharacterProposalCommitField is one field of this lane's canonical
// commit encoding.
type productionCharacterProposalCommitField struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// productionCharacterProposalCommitChange is one change of the canonical encoding.
type productionCharacterProposalCommitChange struct {
	CharacterID string                                `json:"characterId"`
	Fields      []productionCharacterProposalCommitField `json:"fields"`
}

// productionCharacterProposalCommitCreation is one creation of the canonical encoding.
type productionCharacterProposalCommitCreation struct {
	SelectionKey string                                `json:"selectionKey"`
	Fields      []productionCharacterProposalCommitField `json:"fields"`
}

// productionCharacterProposalCommitRelationship is one relationship of the
// canonical encoding. All five keys are always present, which is what makes the
// frozen bytes comparable.
type productionCharacterProposalCommitRelationship struct {
	SourceCharacterID  string `json:"sourceCharacterId"`
	SourceSelectionKey string `json:"sourceSelectionKey"`
	TargetCharacterID  string `json:"targetCharacterId"`
	TargetSelectionKey string `json:"targetSelectionKey"`
	Relation           string `json:"relation"`
}

// productionCharacterProposalCommitResolution is one resolution of the canonical
// encoding.
type productionCharacterProposalCommitResolution struct {
	ProposalID  string `json:"proposalId"`
	CharacterID string `json:"characterId"`
}

// productionCharacterProposalCommitApproval is the approval of the canonical
// encoding.
type productionCharacterProposalCommitApproval struct {
	OperationID      string                                       `json:"operationId"`
	ExpectedRevision int64                                        `json:"expectedRevision"`
	Source           *productionCharacterProposalCommitProvenance `json:"source,omitempty"`
	Action           string                                       `json:"action"`
}

// productionCharacterProposalCommitProvenance is the provenance of the canonical
// encoding.
type productionCharacterProposalCommitProvenance struct {
	Kind          string                        `json:"kind"`
	Source        productionCharacterProposalRef `json:"source"`
	ModelRevision string                        `json:"modelRevision"`
}

// productionCharacterProposalCommitRequest is the canonical encoding of one
// CharacterIdentityCommitRequest, in the reference's declared field order
// (character_identity.go:512-515). It exists only so `authorize` can compare a
// candidate with the request this lane froze; the store's own encoder remains the
// payload digest owner.
type productionCharacterProposalCommitRequest struct {
	Approval      productionCharacterProposalCommitApproval       `json:"approval"`
	Changes       []productionCharacterProposalCommitChange       `json:"changes"`
	Creations     []productionCharacterProposalCommitCreation     `json:"creations"`
	RetireIDs     []string                                       `json:"retireIds"`
	Relationships []productionCharacterProposalCommitRelationship `json:"relationships"`
	Resolutions   []productionCharacterProposalCommitResolution   `json:"resolutions"`
}

// productionCharacterProposalCommitEncoding canonicalizes one commit request.
func productionCharacterProposalCommitEncoding(request store.CharacterIdentityCommitRequest) (string, error) {
	changes := make([]productionCharacterProposalCommitChange, 0, len(request.Changes))
	for _, change := range request.Changes {
		changes = append(changes, productionCharacterProposalCommitChange{
			CharacterID: change.CharacterID,
			Fields:      productionCharacterProposalCommitFields(change.Fields),
		})
	}
	creations := make([]productionCharacterProposalCommitCreation, 0, len(request.Creations))
	for _, creation := range request.Creations {
		creations = append(creations, productionCharacterProposalCommitCreation{
			SelectionKey: creation.SelectionKey,
			Fields:       productionCharacterProposalCommitFields(creation.Fields),
		})
	}
	retired := make([]string, 0, len(request.RetireIDs))
	retired = append(retired, request.RetireIDs...)
	relationships := make([]productionCharacterProposalCommitRelationship, 0, len(request.Relationships))
	for _, relationship := range request.Relationships {
		relationships = append(relationships, productionCharacterProposalCommitRelationship{
			SourceCharacterID:  relationship.SourceCharacterID,
			SourceSelectionKey: relationship.SourceSelectionKey,
			TargetCharacterID:  relationship.TargetCharacterID,
			TargetSelectionKey: relationship.TargetSelectionKey,
			Relation:           relationship.Relation,
		})
	}
	resolutions := make([]productionCharacterProposalCommitResolution, 0, len(request.Resolutions))
	for _, resolution := range request.Resolutions {
		resolutions = append(resolutions, productionCharacterProposalCommitResolution{
			ProposalID:  resolution.ProposalID,
			CharacterID: resolution.CharacterID,
		})
	}
	var provenance *productionCharacterProposalCommitProvenance
	if request.Approval.Source != nil {
		provenance = &productionCharacterProposalCommitProvenance{
			Kind: request.Approval.Source.Kind,
			Source: productionCharacterProposalRef{
				ProjectID:   request.Approval.Source.Source.ProjectID,
				Epoch:       request.Approval.Source.Source.Epoch,
				SourceID:    request.Approval.Source.Source.SourceID,
				Revision:    request.Approval.Source.Source.Revision,
				ContentHash: request.Approval.Source.Source.ContentHash,
			},
			ModelRevision: request.Approval.Source.ModelRevision,
		}
	}
	return productionCharacterProposalEncode(productionCharacterProposalCommitRequest{
		Approval: productionCharacterProposalCommitApproval{
			OperationID:      request.Approval.OperationID,
			ExpectedRevision: request.Approval.ExpectedRevision,
			Source:           provenance,
			Action:           request.Approval.Action,
		},
		Changes:       changes,
		Creations:     creations,
		RetireIDs:     retired,
		Relationships: relationships,
		Resolutions:   resolutions,
	})
}

// productionCharacterProposalCommitFields canonicalizes one field list.
func productionCharacterProposalCommitFields(fields []store.CharacterIdentityField) []productionCharacterProposalCommitField {
	out := make([]productionCharacterProposalCommitField, 0, len(fields))
	for _, field := range fields {
		out = append(out, productionCharacterProposalCommitField{Key: field.Key, Value: field.Value})
	}
	return out
}

// productionCharacterProposalRefusal is one named refusal and the code it answers.
type productionCharacterProposalRefusal struct {
	refusal error
	code    string
}

// productionCharacterProposalRefusals is the one mapping table of this lane. It
// names, in one place, every refusal internal/characterproposal, this file and the
// store can answer with, so a new refusal without an entry is a review finding
// rather than a silent GENERATION_REQUEST_FAILED.
func productionCharacterProposalRefusals() []productionCharacterProposalRefusal {
	return []productionCharacterProposalRefusal{
		// internal/characterproposal: the six the reference publishes as codes.
		{characterproposal.ErrKeysInvalid, "CHARACTER_PROPOSAL_KEYS_INVALID"},
		{characterproposal.ErrEnvelopeInvalid, "CHARACTER_PROPOSAL_ENVELOPE_INVALID"},
		{characterproposal.ErrEmptyDetails, "CHARACTER_PROPOSAL_EMPTY_DETAILS"},
		{characterproposal.ErrDetailFieldInvalid, "CHARACTER_PROPOSAL_DETAIL_FIELD_INVALID"},
		{characterproposal.ErrSlotMismatch, "CHARACTER_PROPOSAL_SLOT_MISMATCH"},
		{characterproposal.ErrCoverageIncomplete, "CHARACTER_PROPOSAL_COVERAGE_INCOMPLETE"},
		// internal/characterproposal: the refusals the reference writes as Chinese
		// prose, which its controller answers with the unmapped-failure code.
		{characterproposal.ErrDocumentTruncated, productionCharacterProposalRequestFailed},
		{characterproposal.ErrDocumentMultiple, productionCharacterProposalRequestFailed},
		{characterproposal.ErrDocumentMissing, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestSlots, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestFields, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestCount, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestSlotInvalid, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestSlotFields, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestRelations, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestIncomplete, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestRelation, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestDuplicate, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestProtagonist, productionCharacterProposalRequestFailed},
		{characterproposal.ErrManifestEndpoints, productionCharacterProposalRequestFailed},
		{characterproposal.ErrDetailEntries, productionCharacterProposalRequestFailed},
		{characterproposal.ErrDetailField, productionCharacterProposalRequestFailed},
		{characterproposal.ErrDetailRole, productionCharacterProposalRequestFailed},
		{characterproposal.ErrDetailRelationships, productionCharacterProposalRequestFailed},
		{characterproposal.ErrDetailState, productionCharacterProposalRequestFailed},
		// this lane.
		{productionCharacterProposalReceiptInvalid, "CHARACTER_PROPOSAL_RECEIPT_INVALID"},
		{productionCharacterProposalSourceChanged, "CHARACTER_PROPOSAL_SOURCE_CHANGED"},
		{productionCharacterProposalSourceInvalid, "CHARACTER_PROPOSAL_SOURCE_INVALID"},
		{productionCharacterProposalSourceProofUnavailable, "CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE"},
		{productionCharacterProposalArtifactInvalid, "CHARACTER_PROPOSAL_ARTIFACT_INVALID"},
		{productionCharacterProposalManifestInvalid, "CHARACTER_PROPOSAL_MANIFEST_INVALID"},
		{productionCharacterProposalProvenanceRequired, "CHARACTER_PROPOSAL_PROVENANCE_REQUIRED"},
		{productionCharacterProposalApprovalInvalid, "CHARACTER_PROPOSAL_APPROVAL_INVALID"},
		{productionCharacterProposalSelectionInvalid, "CHARACTER_APPROVAL_SELECTION_INVALID"},
		{productionCharacterProposalRelationshipInvalid, "CHARACTER_RELATIONSHIP_PROPOSAL_INVALID"},
		{productionCharacterProposalEditInvalid, "CHARACTER_PROPOSAL_EDIT_INVALID"},
		{productionCharacterProposalCancelled, "CHARACTER_PROPOSAL_CANCELLED"},
		{productionCharacterProposalAlreadyApproved, "CHARACTER_PROPOSAL_ALREADY_APPROVED"},
		{productionCharacterProposalLegacyAdmission, "GENERATION_LEGACY_ADMISSION_REQUIRED"},
		{productionCharacterProposalLookupInvalid, productionCharacterProposalRequestFailed},
		// internal/store: the identity and roster refusals this path can meet.
		{store.ErrCharacterApprovalRequired, "CHARACTER_APPROVAL_REQUIRED"},
		{store.ErrCharacterApprovalMutated, "CHARACTER_APPROVAL_MUTATED"},
		{store.ErrCharacterApprovalNonceConflict, "CHARACTER_APPROVAL_NONCE_CONFLICT"},
		{store.ErrCharacterApprovalInvalid, "CHARACTER_APPROVAL_INVALID"},
		{store.ErrCharacterProvenanceInvalid, "CHARACTER_PROVENANCE_INVALID"},
		{store.ErrCharacterIDRevisionConflict, "CHARACTER_ID_REVISION_CONFLICT"},
		{store.ErrCharacterFieldsInvalid, "CHARACTER_FIELDS_INVALID"},
		{store.ErrCharacterIDDuplicate, "CHARACTER_ID_DUPLICATE"},
		{store.ErrCharacterIDConflict, "CHARACTER_ID_CONFLICT"},
		{store.ErrCharacterIDUnknown, "CHARACTER_ID_UNKNOWN"},
		{store.ErrCharacterIDConfirmationRequired, "CHARACTER_ID_CONFIRMATION_REQUIRED"},
		{store.ErrCharacterRelationshipIDRequired, "CHARACTER_RELATIONSHIP_ID_REQUIRED"},
		{store.ErrCharacterRelationshipInvalid, "CHARACTER_RELATIONSHIP_INVALID"},
		{store.ErrCharacterProposalConflict, "CHARACTER_PROPOSAL_CONFLICT"},
		{store.ErrCharacterProposalKindInvalid, "CHARACTER_PROPOSAL_KIND_INVALID"},
		{store.ErrCharacterAuthorRosterNotReady, "CHARACTER_AUTHOR_ROSTER_NOT_READY"},
		{store.ErrCharacterRosterMetaMissing, "CHARACTER_ROSTER_META_MISSING"},
		{store.ErrCharacterRosterSchemaUnsupported, "CHARACTER_ROSTER_SCHEMA_UNSUPPORTED"},
		{store.ErrCharacterIDSchemaRequired, "CHARACTER_ID_SCHEMA_REQUIRED"},
		{store.ErrCharacterIDMetaMissing, "CHARACTER_ID_META_MISSING"},
		{store.ErrCharacterInputUnsupported, "CHARACTER_INPUT_UNSUPPORTED"},
		// store.ErrProjectNotOpen is deliberately absent: it says this lane's database
		// is closed, not that the proposal is wrong, and its name is outside every
		// family the controller publishes. The fallback below answers the controller's
		// own unmapped-failure code for it, which is what a browser would rewrite it to
		// anyway.
		// the ledger: the two codes the proof raises itself, before it reads a receipt.
		{productionCharacterProposalRunMissing, "GENERATION_RUN_MISSING"},
		{productionCharacterProposalRunIdentityMismatch, "GENERATION_RUN_IDENTITY_MISMATCH"},
	}
}

// productionCharacterProposalWirePrefixes is the family allowlist of
// generationIpcErrorCode (generation-controller.ts:88-101). A code outside it is
// never published, whatever its shape.
var productionCharacterProposalWirePrefixes = []string{"GENERATION_", "ROOT_BUDGET_",
	"ARTIFACT_", "MAIN_", "CHARACTER_", "FINALIZED_CHARACTER_", "LEGACY_ROSTER_"}

// productionCharacterProposalWireCode reports whether text is a code this family's
// controller would pass through.
func productionCharacterProposalWireCode(text string) bool {
	if text == "SOURCE_DRAFT_CHANGED" {
		return true
	}
	for _, prefix := range productionCharacterProposalWirePrefixes {
		if !strings.HasPrefix(text, prefix) {
			continue
		}
		rest := text[len(prefix):]
		if rest == "" {
			return false
		}
		for _, character := range rest {
			if character != '_' && (character < 'A' || character > 'Z') {
				return false
			}
		}
		return true
	}
	return false
}

// productionCharacterProposalRefusalCode is the table lookup.
func productionCharacterProposalRefusalCode(err error) string {
	for _, entry := range productionCharacterProposalRefusals() {
		if errors.Is(err, entry.refusal) {
			return entry.code
		}
	}
	return ""
}

// productionCharacterProposalFailure is the one and only mapping layer: a symbolic
// code for a refusal this lane names, the controller's own family allowlist for a
// ledger refusal it forwards, and the unmapped-failure code for everything else -
// never a SQLite message, a path or a provider's prose.
func productionCharacterProposalFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if code := productionCharacterProposalRefusalCode(err); code != "" {
		return errors.New(code)
	}
	if text := err.Error(); productionCharacterProposalWireCode(text) {
		return errors.New(text)
	}
	return errors.New(productionCharacterProposalRequestFailed)
}

// productionCharacterProposalSeamLine is one line that has to exist before a
// refused name can be served: the path of the file, the line inside it, and a
// marker that must appear on that line.
// TestProductionCharacterProposalsRefusesPendingFinalized re-reads every one of
// them, so a moved declaration fails this lane's test instead of leaving a note
// that points at nothing.
type productionCharacterProposalSeamLine struct {
	Path   string
	Line   int
	Marker string
}

// productionCharacterProposalSeam is one refused name together with the code that
// keeps it unbound.
type productionCharacterProposalSeam struct {
	Operation string
	Missing   []productionCharacterProposalSeamLine
}

// productionCharacterProposalOperations is the whole family in the order
// web/operations.json lists it (:56-61). The first four are the ones Bind serves;
// the last two are the ones it refuses.
func productionCharacterProposalOperations() []string {
	return []string{
		productionCharacterProposalStageName,
		productionCharacterProposalReadName,
		productionCharacterProposalListPendingName,
		productionCharacterProposalReadPendingName,
		productionCharacterProposalApproveName,
		productionCharacterProposalCancelName,
	}
}

// productionCharacterProposalPendingSeams is this file's whole answer for the two
// names it does not bind. Both exist to page the batches of the other three source
// kinds, and neither can be proved in this lane:
//
//   - `finalized-generation` needs proveFinalizedCharacterGeneration
//     (electron/services/finalized-character-generation-proof.ts): the proposal
//     response parsed off a settled artifact, the stable-finalization source and
//     the source handle it derives. The Go read it would build on exists
//     (internal/store/finalized_character_context.go), but nothing turns that
//     context into proposal items, so a listing could only ever return the
//     architecture batches this lane stages and never the finalized ones it is
//     named for.
//   - `import` needs the import_global_fact_operations receipt that
//     generation-character-proposal-proof.ts:67-75 reads (payload_hash and
//     receipt_json, cross-checked against the global-facts effect and the
//     artifacts the run settled). internal/admission owns the table; no Go package
//     reads it.
//
// A binder installed for either name would therefore answer
// CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE for every row it is defined to page.
// Installing nothing keeps both failing closed at GO_OPERATION_NOT_MIGRATED, which
// is what the desktop controller's own inventory check reports for a name it does
// not serve.
func productionCharacterProposalPendingSeams() []productionCharacterProposalSeam {
	proof := productionCharacterProposalSeamLine{
		Path: "production_character_proposals_linux.go", Line: 1800,
		Marker: "productionCharacterProposalSourceProofUnavailable",
	}
	stage := productionCharacterProposalSeamLine{
		Path: "production_character_proposals_linux.go", Line: 691,
		Marker: "INSERT INTO character_identity_proposals",
	}
	context := productionCharacterProposalSeamLine{
		Path: "../../internal/store/finalized_character_context.go", Line: 185,
		Marker: "func (f *FinalizedCharacters) ReadContext(",
	}
	receipt := productionCharacterProposalSeamLine{
		Path: "../../internal/admission/baseline_v7_ddl.go", Line: 277,
		Marker: "import_global_fact_operations",
	}
	read := productionCharacterProposalSeamLine{
		Path: "production_character_proposals_linux.go", Line: 1667,
		Marker: "func productionCharacterProposalReadEnvelope(",
	}
	return []productionCharacterProposalSeam{
		{
			Operation: productionCharacterProposalListPendingName,
			Missing:   []productionCharacterProposalSeamLine{proof, stage, context, receipt},
		},
		{
			Operation: productionCharacterProposalReadPendingName,
			Missing:   []productionCharacterProposalSeamLine{proof, read, stage, context, receipt},
		},
	}
}

// productionCharacterProposalPendingSeam returns the documented seam for one name,
// or false when the name is not one of the two.
func productionCharacterProposalPendingSeam(operation string) (productionCharacterProposalSeam, bool) {
	for _, seam := range productionCharacterProposalPendingSeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionCharacterProposalSeam{}, false
}
