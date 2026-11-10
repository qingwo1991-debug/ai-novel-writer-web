package store

// LegacyRosterProposals is the one-transaction staging writer for the
// legacy-roster generation kind of CharacterProposalService.stage
// (electron/services/character-proposal-service.ts:161-165, the INSERT at
// :191-192). It writes exactly one character_identity_proposals row and stamps
// the settled attempt's usage receipt with the legacyRosterProposal effect, both
// inside one BEGIN IMMEDIATE on a reserved connection - the reference's
// .immediate() transaction (electron/services/legacy-roster-generation.ts:52-68).
//
// The proof is re-derived from the database, never from the caller: the run's
// frozen modelReceipt.modelRevision is read out of the stored manifest and must
// be a 64-hex digest before any provenance is built, so a fabricated or empty
// model revision is refused by this store's own commit guard rather than staged.
// The candidate is parsed and validated here because the one transaction cannot
// delegate the parse to package main and still prove the proposal on read.

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

// The refusals this writer raises. Every code is the reference's own:
// GENERATION_LEGACY_ARTIFACT_INVALID / GENERATION_LEGACY_CANDIDATE_INVALID /
// GENERATION_LEGACY_PROOF_INVALID (legacy-roster-generation-proof.ts:22, :28,
// :41), GENERATION_LEGACY_EFFECT_CONFLICT / GENERATION_LEGACY_EFFECT_INVALID
// (legacy-roster-generation.ts:56, :39) and GENERATION_RUN_IDENTITY_MISMATCH
// (legacy-roster-generation.ts:18). The read-back receipt code is
// character-proposal-service.ts:62, already published as
// ErrCharacterProposalPendingReceiptInvalid.
var (
	ErrLegacyRosterProposalArtifactInvalid  = errors.New("GENERATION_LEGACY_ARTIFACT_INVALID")
	ErrLegacyRosterProposalCandidateInvalid = errors.New("GENERATION_LEGACY_CANDIDATE_INVALID")
	ErrLegacyRosterProposalProofInvalid     = errors.New("GENERATION_LEGACY_PROOF_INVALID")
	ErrLegacyRosterProposalEffectConflict   = errors.New("GENERATION_LEGACY_EFFECT_CONFLICT")
	ErrLegacyRosterProposalEffectInvalid    = errors.New("GENERATION_LEGACY_EFFECT_INVALID")
	ErrLegacyRosterProposalIdentityMismatch = errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
)

const (
	// legacyRosterProposalOperation is the one sourceManifest.operation this
	// writer stages (main-generation-owner.ts:1128).
	legacyRosterProposalOperation = "legacy-character-roster-repair"
	// legacyRosterProposalKind is the source kind CharacterProposalService.stage
	// routes to the legacy arm (character-proposal-service.ts:161).
	legacyRosterProposalKind = "legacy-roster-generation"
	// legacyRosterProposalSourceKeyPrefix is every envelope's source_key
	// (character-proposal-service.ts:62, :192).
	legacyRosterProposalSourceKeyPrefix = "character-proposal-v1:"
	// legacyRosterProposalSchemaVersion is CHARACTER_ROSTER_SCHEMA_VERSION
	// (src/shared/character-roster.ts:10).
	legacyRosterProposalSchemaVersion = 1
)

// The two fence patterns parseLegacyRosterJson strips
// (src/shared/legacy-roster-generation-pure.ts:89).
var (
	legacyRosterProposalJSONFence = regexp.MustCompile("(?i)```json?\n?")
	legacyRosterProposalCodeFence = regexp.MustCompile("```\n?")
)

// LegacyRosterStageRequest is the stage request's two closed shapes, already
// validated by the caller; every fact below is re-proved from the database.
type LegacyRosterStageRequest struct {
	ProjectID    string
	Epoch        string
	RootActionID string
	RunID        string
	ArtifactID   string
	Revision     int64
	TextHash     string
}

// LegacyRosterProposals borrows an admitted author database and stages legacy
// roster proposals over one reserved connection.
type LegacyRosterProposals struct{ db *sql.DB }

// NewLegacyRosterProposals is the constructor.
func NewLegacyRosterProposals(db *sql.DB) *LegacyRosterProposals {
	return &LegacyRosterProposals{db: db}
}

// Stage is CharacterProposalService.stage's legacy arm in one transaction: prove
// the settled attempt and its artifact, parse and validate the candidate, read
// the frozen model revision, write the proposal row and stamp the effect. The
// answer is the staged batch as a JSON-shaped map.
func (p *LegacyRosterProposals) Stage(ctx context.Context, request LegacyRosterStageRequest) (map[string]any, error) {
	if p == nil || p.db == nil {
		return nil, ErrProjectNotOpen
	}
	if summaryTrim(request.ProjectID) == "" || summaryTrim(request.RunID) == "" {
		return nil, ErrLegacyRosterProposalIdentityMismatch
	}
	var batch map[string]any
	err := NewReviewRevisions(p.db).immediate(ctx, func(conn *sql.Conn) error {
		var err error
		batch, err = legacyRosterProposalStage(ctx, conn, request)
		return err
	})
	if err != nil {
		return nil, err
	}
	return batch, nil
}

// legacyRosterProposalProofResult is what the proof computes before the write.
type legacyRosterProposalProofResult struct {
	items      []any
	sourceHash string
	provenance plotObject
	canonical  plotObject
}

// legacyRosterProposalEffect is the parsed legacyRosterProposal effect of a
// previously staged attempt, together with the attempt row that carried it and
// the source hash the recovery re-proves it against
// (electron/services/legacy-roster-generation.ts:44-53).
type legacyRosterProposalEffect struct {
	attemptID       string
	artifact        plotObject
	proposalBatchID string
	sourceHash      string
}

// LegacyRosterRecoveryRequest names the run whose recovery projection is read. It
// carries no artifact: the reference proves the artifact out of the run's own
// latest attempt instead of trusting a caller.
type LegacyRosterRecoveryRequest struct {
	ProjectID string
	RunID     string
}

// LegacyRosterRecoveryProjection is LegacyRosterGenerationRecovery's two optional
// members (src/shared/legacy-roster-generation.ts:13-21). A nil member is a key
// the wire object omits, exactly like the reference's conditional spread
// (electron/services/main-generation-owner.ts:1109-1116).
type LegacyRosterRecoveryProjection struct {
	Artifact map[string]any
	Proposal map[string]any
}

// Recovery is artifactFor + proposal as the owner's recovery read uses them
// (electron/services/legacy-roster-generation.ts:20-22, :44): the artifact the
// run's latest attempt proved, and the batch a settled legacyRosterProposal
// effect reads back. Both are derived from the database, never from a caller, so
// a browser that executed or staged a run outside this process still gets the
// keys its stage() reads (src/services/legacy-roster-generation.ts:74-83).
//
// It reads on one reserved connection and opens no transaction: the reference's
// recovery is a plain read too, and this is the read half of the family, so a
// legacy-roster:read never takes a write lock.
func (p *LegacyRosterProposals) Recovery(ctx context.Context, request LegacyRosterRecoveryRequest) (*LegacyRosterRecoveryProjection, error) {
	if p == nil || p.db == nil {
		return nil, ErrProjectNotOpen
	}
	if summaryTrim(request.ProjectID) == "" || summaryTrim(request.RunID) == "" {
		return nil, ErrLegacyRosterProposalIdentityMismatch
	}
	conn, err := p.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	projection := &LegacyRosterRecoveryProjection{}
	if err := legacyRosterProposalRecovery(ctx, conn, request, projection); err != nil {
		return nil, err
	}
	return projection, nil
}

func legacyRosterProposalRecovery(ctx context.Context, conn *sql.Conn, request LegacyRosterRecoveryRequest, projection *LegacyRosterRecoveryProjection) error {
	run, binding, manifest, err := legacyRosterProposalRun(ctx, conn, LegacyRosterStageRequest{
		ProjectID: request.ProjectID, RunID: request.RunID,
	})
	if err != nil {
		return err
	}
	runID := rosterText(run["run_id"])
	rootID := rosterText(run["root_action_id"])
	projectID := pfText(binding, "projectId")
	originEpoch := pfText(manifest, "legacyRosterOriginEpoch")
	if request.ProjectID != projectID {
		return ErrLegacyRosterProposalIdentityMismatch
	}
	// artifactFor: the latest attempt's artifact, and only when the full proof
	// accepts it. The reference swallows that proof's refusal here, so a candidate
	// that could never be staged answers no artifact instead of an error.
	attemptID, reference, err := legacyRosterProposalLatestArtifact(ctx, conn, runID)
	if err != nil {
		return err
	}
	if attemptID != "" {
		if _, err := legacyRosterProposalProof(ctx, conn, runID, rootID, projectID, originEpoch, manifest, attemptID, reference); err == nil {
			if plain, ok := glPlain(reference).(map[string]any); ok {
				projection.Artifact = plain
			}
		}
	}
	// proposal: the run's own stored effect, re-proved key by key exactly like
	// LegacyRosterGeneration.saved (:44-53) before its batch is answered.
	contextHash, err := legacyRosterProposalContextHash(manifest)
	if err != nil {
		return err
	}
	effect, err := legacyRosterProposalSaved(ctx, conn, runID, contextHash)
	if err != nil {
		return err
	}
	if effect == nil {
		return nil
	}
	proof, err := legacyRosterProposalProof(ctx, conn, runID, rootID, projectID, originEpoch, manifest, effect.attemptID, effect.artifact)
	if err != nil {
		// The reference's saved does not wrap the proof: a reference that no longer
		// resolves answers the proof's own code, not the effect code.
		return err
	}
	envelope, err := legacyRosterProposalEnvelopeRead(ctx, conn, projectID, effect.proposalBatchID)
	if err != nil {
		// Likewise the receipt: proposals.read throws its own code for a corrupt
		// envelope and saved lets it through.
		return err
	}
	// The three equalities the reference's saved does turn into
	// GENERATION_LEGACY_EFFECT_INVALID after the proof: the effect's own source
	// hash is the proof's, the stored envelope's evidence hash is that same one,
	// and the batch's source is the proof's source.
	if effect.sourceHash != proof.sourceHash || envelope.proofSourceHash != effect.sourceHash ||
		!legacyRosterProposalSourceSame(envelope.batchSource, proof.canonical) {
		return ErrLegacyRosterProposalEffectInvalid
	}
	projection.Proposal = envelope.batch
	return nil
}

// legacyRosterProposalLatestArtifact is artifactFor (legacy-roster-generation.ts:
// 20-22): the run's latest attempt with the three-key reference of its receipt
// artifact, or no attempt at all. Whether the reference is provable is the
// caller's proof, not this function's decision.
func legacyRosterProposalLatestArtifact(ctx context.Context, conn *sql.Conn, runID string) (string, plotObject, error) {
	row, err := glOne(ctx, conn, "SELECT attempt_id FROM generation_attempts WHERE run_id=? ORDER BY rowid DESC LIMIT 1", "", runID)
	if err != nil {
		return "", nil, err
	}
	if row == nil {
		return "", nil, nil
	}
	attemptID := rosterText(row["attempt_id"])
	if attemptID == "" {
		return "", nil, nil
	}
	receipt, err := NewGenerationLedger(nil, nil, nil).receipt(ctx, conn, attemptID, nil)
	if err != nil {
		return "", nil, nil
	}
	artifact := pfObject(pfValue(receipt, "artifact"))
	if artifact == nil {
		return "", nil, nil
	}
	artifactID := pfText(artifact, "artifactId")
	revision, revisionOK := pfInteger(artifact, "revision")
	textHash := pfText(artifact, "textHash")
	if artifactID == "" || !revisionOK || textHash == "" {
		return "", nil, nil
	}
	return attemptID, plotObject{
		{"artifactId", artifactID},
		{"revision", revision},
		{"textHash", textHash},
	}, nil
}

// legacyRosterProposalSourceSame is isDeepStrictEqual(batch.source, proof.source)
// (legacy-roster-generation.ts:51) without Go's struct identity rules: both sides
// are decoded JSON, so key order and number representation cannot decide a
// comparison that the reference makes over values.
func legacyRosterProposalSourceSame(stored any, canonical plotObject) bool {
	if stored == nil {
		return false
	}
	expected, err := plotJSON(canonical)
	if err != nil {
		return false
	}
	parsed, err := glParse(expected)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(stored, parsed)
}

func legacyRosterProposalStage(ctx context.Context, conn *sql.Conn, request LegacyRosterStageRequest) (map[string]any, error) {
	run, binding, manifest, err := legacyRosterProposalRun(ctx, conn, request)
	if err != nil {
		return nil, err
	}
	runID := rosterText(run["run_id"])
	rootID := rosterText(run["root_action_id"])
	projectID := pfText(binding, "projectId")
	bindingEpoch := pfText(binding, "epoch")
	originEpoch := pfText(manifest, "legacyRosterOriginEpoch")
	if request.ProjectID != projectID || request.RunID != runID || request.RootActionID != rootID ||
		(request.Epoch != bindingEpoch && request.Epoch != originEpoch) {
		return nil, ErrLegacyRosterProposalIdentityMismatch
	}
	contextHash, err := legacyRosterProposalContextHash(manifest)
	if err != nil {
		return nil, err
	}
	// saved: a run already staged re-plays its batch, or refuses a conflicting
	// artifact, exactly like LegacyRosterGeneration.stage (:54-58).
	effect, err := legacyRosterProposalSaved(ctx, conn, runID, contextHash)
	if err != nil {
		return nil, err
	}
	if effect != nil {
		if !legacyRosterProposalArtifactSame(effect.artifact, request) {
			return nil, ErrLegacyRosterProposalEffectConflict
		}
		return legacyRosterProposalRead(ctx, conn, projectID, effect.proposalBatchID)
	}
	// artifactFor: the latest attempt's artifact must equal the request's (:59).
	attemptID, reference, err := legacyRosterProposalArtifactFor(ctx, conn, request, runID)
	if err != nil {
		return nil, err
	}
	proof, err := legacyRosterProposalProof(ctx, conn, runID, rootID, projectID, originEpoch, manifest, attemptID, reference)
	if err != nil {
		return nil, err
	}
	batchID, err := legacyRosterProposalBatchID(projectID, proof.canonical)
	if err != nil {
		return nil, err
	}
	batch, err := legacyRosterProposalBatch(ctx, conn, projectID, batchID, proof.items, proof.canonical)
	if err != nil {
		return nil, err
	}
	envelope := legacyRosterProposalEnvelope(projectID, plotObject{
		{"items", proof.items},
		{"sourceHash", proof.sourceHash},
		{"provenance", proof.provenance},
	}, batch)
	encoded, err := plotJSON(envelope)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO character_identity_proposals(proposal_id,owner_character_id,source_key,source_hash,raw_value,candidate_ids_json) VALUES(?,NULL,?,?,?,?)`,
		batchID, legacyRosterProposalSourceKeyPrefix+projectID, summaryHash(encoded), encoded, "[]"); err != nil {
		return nil, err
	}
	// The effect and the proposal row are written in this one transaction, so a
	// run can never be re-dispatched after its roster was staged (:63-65).
	if err := legacyRosterProposalStamp(ctx, conn, attemptID, legacyRosterProposalEffectObject(reference, contextHash, batchID, proof.sourceHash)); err != nil {
		return nil, err
	}
	plain, ok := glPlain(batch).(map[string]any)
	if !ok {
		return nil, ErrLegacyRosterProposalProofInvalid
	}
	return plain, nil
}

func legacyRosterProposalRun(ctx context.Context, conn *sql.Conn, request LegacyRosterStageRequest) (map[string]any, plotObject, plotObject, error) {
	rows, err := rrMaps(ctx, conn, "SELECT run_id,root_action_id,binding_json FROM generation_runs WHERE run_id=?", request.RunID)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(rows) == 0 {
		return nil, nil, nil, glError("GENERATION_RUN_MISSING")
	}
	bindingValue, err := glParse(rosterText(rows[0]["binding_json"]))
	if err != nil {
		return nil, nil, nil, ErrLegacyRosterProposalIdentityMismatch
	}
	binding := pfObject(bindingValue)
	if binding == nil {
		return nil, nil, nil, ErrLegacyRosterProposalIdentityMismatch
	}
	manifest := pfObject(pfValue(binding, "sourceManifest"))
	if manifest == nil || pfText(manifest, "operation") != legacyRosterProposalOperation {
		return nil, nil, nil, ErrLegacyRosterProposalIdentityMismatch
	}
	return rows[0], binding, manifest, nil
}

func legacyRosterProposalContextHash(manifest plotObject) (string, error) {
	contextValue := pfValue(manifest, "legacyRosterContext")
	if contextValue == nil {
		return "", ErrLegacyRosterProposalProofInvalid
	}
	encoded, err := plotJSON(contextValue)
	if err != nil {
		return "", err
	}
	return summaryHash(encoded), nil
}

func legacyRosterProposalSaved(ctx context.Context, conn *sql.Conn, runID, contextHash string) (*legacyRosterProposalEffect, error) {
	rows, err := rrMaps(ctx, conn, "SELECT attempt_id,usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid", runID)
	if err != nil {
		return nil, err
	}
	var found *legacyRosterProposalEffect
	for _, row := range rows {
		usage, err := glStored(row["usage_receipt_json"])
		if err != nil {
			return nil, err
		}
		effectValue := pfValue(usage, "legacyRosterProposal")
		if effectValue == nil {
			continue
		}
		effect := pfObject(effectValue)
		if effect == nil {
			return nil, ErrLegacyRosterProposalEffectInvalid
		}
		version, versionOK := pfInteger(effect, "version")
		if !versionOK || version != 1 || pfText(effect, "contextHash") != contextHash {
			return nil, ErrLegacyRosterProposalEffectInvalid
		}
		artifact := pfObject(pfValue(effect, "artifact"))
		proposalBatchID := pfText(effect, "proposalBatchId")
		if artifact == nil || proposalBatchID == "" {
			return nil, ErrLegacyRosterProposalEffectInvalid
		}
		if found != nil {
			return nil, ErrLegacyRosterProposalEffectInvalid
		}
		// The attempt and the source hash are not part of this function's refusal
		// boundary - the stage replay only needs the artifact and the batch - but
		// the recovery's proof reads them back, so they are captured here rather
		// than queried again.
		found = &legacyRosterProposalEffect{
			attemptID:       rosterText(row["attempt_id"]),
			artifact:        artifact,
			proposalBatchID: proposalBatchID,
			sourceHash:      pfText(effect, "sourceHash"),
		}
	}
	return found, nil
}

func legacyRosterProposalArtifactSame(artifact plotObject, request LegacyRosterStageRequest) bool {
	return pfText(artifact, "artifactId") == request.ArtifactID &&
		legacyRosterProposalRevisionSame(artifact, request.Revision) &&
		pfText(artifact, "textHash") == request.TextHash
}

func legacyRosterProposalRevisionSame(artifact plotObject, revision int64) bool {
	value, ok := pfInteger(artifact, "revision")
	return ok && value == revision
}

func legacyRosterProposalArtifactFor(ctx context.Context, conn *sql.Conn, request LegacyRosterStageRequest, runID string) (string, plotObject, error) {
	row, err := glOne(ctx, conn, "SELECT attempt_id FROM generation_attempts WHERE run_id=? ORDER BY rowid DESC LIMIT 1", "", runID)
	if err != nil {
		return "", nil, err
	}
	if row == nil {
		return "", nil, ErrLegacyRosterProposalArtifactInvalid
	}
	attemptID := rosterText(row["attempt_id"])
	receipt, err := NewGenerationLedger(nil, nil, nil).receipt(ctx, conn, attemptID, nil)
	if err != nil {
		return "", nil, ErrLegacyRosterProposalArtifactInvalid
	}
	artifact := pfObject(pfValue(receipt, "artifact"))
	if artifact == nil {
		return "", nil, ErrLegacyRosterProposalArtifactInvalid
	}
	artifactID := pfText(artifact, "artifactId")
	revision, revisionOK := pfInteger(artifact, "revision")
	textHash := pfText(artifact, "textHash")
	if artifactID == "" || !revisionOK || textHash == "" {
		return "", nil, ErrLegacyRosterProposalArtifactInvalid
	}
	if artifactID != request.ArtifactID || revision != request.Revision || textHash != request.TextHash {
		return "", nil, ErrLegacyRosterProposalArtifactInvalid
	}
	return attemptID, plotObject{{"artifactId", artifactID}, {"revision", revision}, {"textHash", textHash}}, nil
}

func legacyRosterProposalProof(ctx context.Context, conn *sql.Conn, runID, rootID, projectID, originEpoch string, manifest plotObject, attemptID string, reference plotObject) (legacyRosterProposalProofResult, error) {
	artifactID := pfText(reference, "artifactId")
	rows, err := rrMaps(ctx, conn, "SELECT run_id,attempt_id,status FROM generation_artifacts WHERE artifact_id=?", artifactID)
	if err != nil {
		return legacyRosterProposalProofResult{}, err
	}
	if len(rows) == 0 || rosterText(rows[0]["run_id"]) != runID || rosterText(rows[0]["status"]) == "discarded" ||
		rosterText(rows[0]["attempt_id"]) != attemptID {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalArtifactInvalid
	}
	receipt, err := NewGenerationLedger(nil, nil, nil).receipt(ctx, conn, attemptID, nil)
	if err != nil {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalArtifactInvalid
	}
	receiptArtifact := pfObject(pfValue(receipt, "artifact"))
	receiptAttempt := pfObject(pfValue(receipt, "attempt"))
	receiptResult := pfObject(pfValue(receipt, "result"))
	if receiptArtifact == nil || receiptAttempt == nil {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalArtifactInvalid
	}
	revision, revisionOK := pfInteger(reference, "revision")
	if !revisionOK || pfText(receiptArtifact, "artifactId") != artifactID ||
		!pfNumberIs(pfValue(receiptArtifact, "revision"), revision) ||
		pfText(receiptArtifact, "textHash") != pfText(reference, "textHash") {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalArtifactInvalid
	}
	if _, failed := pfField(receipt, "failureCode"); failed {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalArtifactInvalid
	}
	status := pfText(receiptAttempt, "status")
	if status != "settled" && status != "unknown" {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalArtifactInvalid
	}
	if pfText(receiptResult, "finishReason") != "stop" {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalArtifactInvalid
	}
	text := pfText(receiptArtifact, "text")
	if summaryHash(text) != pfText(reference, "textHash") {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalArtifactInvalid
	}
	candidate, err := legacyRosterProposalParseJSON(text)
	if err != nil {
		return legacyRosterProposalProofResult{}, err
	}
	if err := legacyRosterProposalCandidate(candidate); err != nil {
		return legacyRosterProposalProofResult{}, err
	}
	entries, entriesOK := pfArray(candidate, "entries")
	if !entriesOK {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalCandidateInvalid
	}
	legacyHash := legacyRosterProposalContextLegacyHash(manifest)
	items, err := legacyRosterProposalItems(artifactID, legacyHash, entries)
	if err != nil {
		return legacyRosterProposalProofResult{}, err
	}
	modelReceipt := pfObject(pfValue(manifest, "modelReceipt"))
	modelRevision := pfText(modelReceipt, "modelRevision")
	if !pfDigest(modelRevision) {
		return legacyRosterProposalProofResult{}, ErrLegacyRosterProposalProofInvalid
	}
	canonical := legacyRosterProposalCanonical(projectID, originEpoch, rootID, runID, reference)
	sourceHash, err := legacyRosterProposalSourceHash(canonical, pfText(manifest, "legacyRosterContextHash"), items)
	if err != nil {
		return legacyRosterProposalProofResult{}, err
	}
	provenance := legacyRosterProposalProvenance(projectID, originEpoch, runID, modelRevision, sourceHash)
	return legacyRosterProposalProofResult{items: items, sourceHash: sourceHash, provenance: provenance, canonical: canonical}, nil
}

func legacyRosterProposalParseJSON(text string) (plotObject, error) {
	clean := legacyRosterProposalJSONFence.ReplaceAllString(text, "")
	clean = legacyRosterProposalCodeFence.ReplaceAllString(clean, "")
	clean = strings.TrimSpace(clean)
	firstBrace := strings.Index(clean, "{")
	firstBracket := strings.Index(clean, "[")
	lastBrace := strings.LastIndex(clean, "}")
	lastBracket := strings.LastIndex(clean, "]")
	switch {
	case firstBrace != -1 && lastBrace != -1 && (firstBracket == -1 || firstBrace < firstBracket):
		clean = clean[firstBrace : lastBrace+1]
	case firstBracket != -1 && lastBracket != -1:
		clean = clean[firstBracket : lastBracket+1]
	}
	value, err := glParse(clean)
	if err != nil {
		return nil, ErrLegacyRosterProposalCandidateInvalid
	}
	object := pfObject(value)
	if object == nil {
		return nil, ErrLegacyRosterProposalCandidateInvalid
	}
	return object, nil
}

func legacyRosterProposalCandidate(object plotObject) error {
	invalid := ErrLegacyRosterProposalCandidateInvalid
	if object == nil {
		return invalid
	}
	version, versionOK := pfInteger(object, "schemaVersion")
	if !versionOK || version != legacyRosterProposalSchemaVersion {
		return invalid
	}
	entries, entriesOK := pfArray(object, "entries")
	if !entriesOK || len(entries) == 0 {
		return invalid
	}
	names := make(map[string]bool, len(entries))
	targets := make([][]string, 0, len(entries))
	for _, rawEntry := range entries {
		entry := pfObject(rawEntry)
		if entry == nil {
			return invalid
		}
		name := strings.TrimSpace(pfText(entry, "name"))
		if name == "" {
			return invalid
		}
		key := strings.ToLower(name)
		if names[key] {
			return invalid
		}
		names[key] = true
		if !legacyRosterProposalRoleAllowed(pfText(entry, "role")) {
			return invalid
		}
		relationships, relationshipsOK := pfArray(entry, "relationships")
		if !relationshipsOK {
			return invalid
		}
		seen := make(map[string]bool, len(relationships))
		entryTargets := make([]string, 0, len(relationships))
		for _, rawRelationship := range relationships {
			relationship := pfObject(rawRelationship)
			if relationship == nil {
				return invalid
			}
			target := strings.TrimSpace(pfText(relationship, "target"))
			relation := strings.TrimSpace(pfText(relationship, "relation"))
			if target == "" || relation == "" {
				return invalid
			}
			if target == name {
				return invalid
			}
			pair := target + "\x00" + relation
			if seen[pair] {
				return invalid
			}
			seen[pair] = true
			entryTargets = append(entryTargets, target)
		}
		targets = append(targets, entryTargets)
	}
	for _, entryTargets := range targets {
		for _, target := range entryTargets {
			if !names[strings.ToLower(target)] {
				return invalid
			}
		}
	}
	return nil
}

func legacyRosterProposalRoleAllowed(role string) bool {
	switch role {
	case "protagonist", "antagonist", "supporting", "minor":
		return true
	default:
		return false
	}
}

func legacyRosterProposalContextLegacyHash(manifest plotObject) string {
	contextObject := pfObject(pfValue(manifest, "legacyRosterContext"))
	if contextObject == nil {
		return ""
	}
	source := pfObject(pfValue(contextObject, "source"))
	if source == nil {
		return ""
	}
	return pfText(source, "legacyHash")
}

func legacyRosterProposalItems(artifactID, legacyHash string, entries []any) ([]any, error) {
	key := func(index int) string { return artifactID + ":entry:" + strconv.Itoa(index) }
	identityIndex := make(map[string]int, len(entries))
	for index, rawEntry := range entries {
		entry := pfObject(rawEntry)
		if entry == nil {
			return nil, ErrLegacyRosterProposalCandidateInvalid
		}
		identityIndex[strings.ToLower(strings.TrimSpace(pfText(entry, "name")))] = index
	}
	items := make([]any, 0, len(entries))
	for index, rawEntry := range entries {
		entry := pfObject(rawEntry)
		if entry == nil {
			return nil, ErrLegacyRosterProposalCandidateInvalid
		}
		fields := plotObject{}
		for _, field := range entry {
			switch field.key {
			case "currentState", "relationships", "legacyRelationshipNotes":
			default:
				fields = append(fields, field)
			}
		}
		relationships := []any{}
		if rawRelationships, ok := pfArray(entry, "relationships"); ok {
			for _, rawRelationship := range rawRelationships {
				relationship := pfObject(rawRelationship)
				if relationship == nil {
					return nil, ErrLegacyRosterProposalCandidateInvalid
				}
				target := pfText(relationship, "target")
				targetIndex, found := identityIndex[strings.ToLower(strings.TrimSpace(target))]
				if !found {
					targetIndex = -1
				}
				relationships = append(relationships, plotObject{
					{"targetSelectionKey", key(targetIndex)},
					{"targetName", target},
					{"relation", pfText(relationship, "relation")},
					{"raw", relationship},
				})
			}
		}
		items = append(items, plotObject{
			{"selectionKey", key(index)},
			{"sourceId", "legacy:" + legacyHash + ":" + strconv.Itoa(index)},
			{"fields", fields},
			{"relationships", relationships},
			{"rawValue", entry},
		})
	}
	return items, nil
}

func legacyRosterProposalCanonical(projectID, originEpoch, rootID, runID string, reference plotObject) plotObject {
	return plotObject{
		{"kind", legacyRosterProposalKind},
		{"handle", plotObject{
			{"projectId", projectID},
			{"epoch", originEpoch},
			{"rootActionId", rootID},
			{"runId", runID},
		}},
		{"artifact", reference},
	}
}

func legacyRosterProposalSourceHash(canonical plotObject, contextHash string, items []any) (string, error) {
	encoded, err := plotJSON([]any{canonical, contextHash, items})
	if err != nil {
		return "", err
	}
	return summaryHash(encoded), nil
}

func legacyRosterProposalProvenance(projectID, originEpoch, runID, modelRevision, sourceHash string) plotObject {
	return plotObject{
		{"kind", "generated"},
		{"modelRevision", modelRevision},
		{"source", plotObject{
			{"projectId", projectID},
			{"epoch", originEpoch},
			{"sourceId", runID},
			{"revision", int64(0)},
			{"contentHash", sourceHash},
		}},
	}
}

func legacyRosterProposalBatchID(projectID string, canonical plotObject) (string, error) {
	encoded, err := plotJSON([]any{projectID, canonical})
	if err != nil {
		return "", err
	}
	return "cpb:" + summaryHash(encoded), nil
}

func legacyRosterProposalBatch(ctx context.Context, conn *sql.Conn, projectID, batchID string, proofItems []any, canonical plotObject) (plotObject, error) {
	revision, aliases, err := importGlobalFactsIdentity(ctx, conn)
	if err != nil {
		return nil, err
	}
	items := make([]any, 0, len(proofItems))
	for _, rawItem := range proofItems {
		item := pfObject(rawItem)
		if item == nil {
			return nil, ErrLegacyRosterProposalProofInvalid
		}
		fields := pfObject(pfValue(item, "fields"))
		name := ""
		if fields != nil {
			name = pfText(fields, "name")
		}
		sourceID := pfText(item, "sourceId")
		resolution := importGlobalFactsResolve(aliases, name, projectID, sourceID, revision)
		resolved := append(plotObject{}, item...)
		resolved = append(resolved, plotField{"resolution", pendingFinalizedResolution(resolution)})
		items = append(items, resolved)
	}
	return plotObject{
		{"proposalBatchId", batchID},
		{"revision", revision},
		{"status", "pending-approval"},
		{"source", canonical},
		{"items", items},
	}, nil
}

func legacyRosterProposalEnvelope(projectID string, proof, batch plotObject) plotObject {
	return plotObject{
		{"version", int64(1)},
		{"projectId", projectID},
		{"proof", proof},
		{"batch", batch},
	}
}

func legacyRosterProposalEffectObject(reference plotObject, contextHash, proposalBatchID, sourceHash string) plotObject {
	return plotObject{
		{"version", int64(1)},
		{"artifact", reference},
		{"contextHash", contextHash},
		{"proposalBatchId", proposalBatchID},
		{"sourceHash", sourceHash},
	}
}

func legacyRosterProposalStamp(ctx context.Context, conn *sql.Conn, attemptID string, effect plotObject) error {
	row, err := glOne(ctx, conn, "SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?", "GENERATION_ATTEMPT_MISSING", attemptID)
	if err != nil {
		return err
	}
	encoded, err := glPatch(row["usage_receipt_json"], map[string]any{"legacyRosterProposal": effect})
	if err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, "UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=?", encoded, attemptID)
	return err
}

// legacyRosterProposalStoredEnvelope is one stored envelope read back: the batch
// the wire answers, its decoded source, and the proof hash the reference's
// evidence read returns (character-proposal-service.ts:155-157). It is not the
// builder of the same shape above: this one is what the database holds.
type legacyRosterProposalStoredEnvelope struct {
	proofSourceHash string
	batch           map[string]any
	batchSource     any
}

func legacyRosterProposalEnvelopeRead(ctx context.Context, conn *sql.Conn, projectID, proposalBatchID string) (*legacyRosterProposalStoredEnvelope, error) {
	rows, err := rrMaps(ctx, conn, "SELECT source_key,source_hash,raw_value FROM character_identity_proposals WHERE proposal_id=?", proposalBatchID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrCharacterProposalPendingReceiptInvalid
	}
	sourceKey := rosterText(rows[0]["source_key"])
	storedHash := rosterText(rows[0]["source_hash"])
	raw := rosterText(rows[0]["raw_value"])
	if sourceKey != legacyRosterProposalSourceKeyPrefix+projectID || storedHash == "" || summaryHash(raw) != storedHash {
		return nil, ErrCharacterProposalPendingReceiptInvalid
	}
	value, err := glParse(raw)
	if err != nil {
		return nil, ErrCharacterProposalPendingReceiptInvalid
	}
	envelope := pfObject(value)
	if envelope == nil {
		return nil, ErrCharacterProposalPendingReceiptInvalid
	}
	version, versionOK := pfInteger(envelope, "version")
	if !versionOK || version != 1 || pfText(envelope, "projectId") != projectID {
		return nil, ErrCharacterProposalPendingReceiptInvalid
	}
	batch := pfObject(pfValue(envelope, "batch"))
	if batch == nil || pfText(batch, "proposalBatchId") != proposalBatchID {
		return nil, ErrCharacterProposalPendingReceiptInvalid
	}
	plain, ok := glPlain(batch).(map[string]any)
	if !ok {
		return nil, ErrCharacterProposalPendingReceiptInvalid
	}
	return &legacyRosterProposalStoredEnvelope{
		proofSourceHash: pfText(pfObject(pfValue(envelope, "proof")), "sourceHash"),
		batch:           plain,
		batchSource:     pfValue(batch, "source"),
	}, nil
}

// legacyRosterProposalRead is the batch alone, for the stage replay path.
func legacyRosterProposalRead(ctx context.Context, conn *sql.Conn, projectID, proposalBatchID string) (map[string]any, error) {
	envelope, err := legacyRosterProposalEnvelopeRead(ctx, conn, projectID, proposalBatchID)
	if err != nil {
		return nil, err
	}
	return envelope.batch, nil
}
