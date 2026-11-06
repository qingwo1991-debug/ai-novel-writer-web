//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the three candidate operations of the finalized-character
// family (web/operations.json:201-203):
//
//	finalized-character:list-state-candidates   arity 0  store.ListPendingStateCandidates
//	finalized-character:read-state-candidate    arity 1  store.ReadPendingStateCandidate
//	finalized-character:decide-state-candidate  arity 1  store.DecideStateCandidate
//
// It binds those three names and nothing else: `finalized-character:read-context`
// stays with production_finalized_character_linux.go, and
// `finalized-character:commit` stays unbound at the fail-closed 501
// GO_OPERATION_NOT_MIGRATED because the state-response parser it needs has no Go
// port (production_finalization_generation_linux.go:36-64 records the same
// judgement for its own commit).
//
// # The reference
//
//	electron/controllers/generation-controller.ts:220
//	  register('finalized-character:list-state-candidates', 0, owner => owner.listPendingFinalizedCharacterStateCandidates())
//	electron/controllers/generation-controller.ts:221
//	  register('finalized-character:read-state-candidate', 1, (owner, request) => owner.readPendingFinalizedCharacterStateCandidate(request))
//	electron/controllers/generation-controller.ts:222
//	  register('finalized-character:decide-state-candidate', 1, (owner, request) => owner.decideFinalizedCharacterStateCandidate(request))
//	electron/services/finalized-character-generation.ts:30-33
//	  listPendingStateCandidates() { this.assertCurrent(); return SummaryRepository.listPendingFinalizedCharacterStateCandidates(this.db) }
//	electron/services/finalized-character-generation.ts:35-38
//	  readPendingStateCandidate(request) { this.assertCurrent(); return SummaryRepository.readPendingFinalizedCharacterStateCandidate(request.draftId, request.candidateKey, this.db) }
//	electron/services/finalized-character-generation.ts:40-43
//	  decideStateCandidate(request) { this.assertCurrent(); return SummaryRepository.decideFinalizedCharacterStateCandidate(request, this.db) }
//
// The wire declarations are src/shared/finalized-character-generation.ts:23-25:
//
//	'finalized-character:list-state-candidates': { args: []; return: PendingFinalizedCharacterStateCandidateSummary[] }
//	'finalized-character:read-state-candidate': { args: [{ draftId: number; candidateKey: string }];
//	  return: PendingFinalizedCharacterStateCandidate }
//	'finalized-character:decide-state-candidate': { args: [FinalizedCharacterStateDecisionRequest];
//	  return: FinalizedCharacterStateDecisionReceipt }
//
// The renderer is the only consumer (src/components/characters/use-pending-state-candidates.ts:31,
// :47-49, :62-68): it lists the summaries, reads one by the draft id and
// candidate key the summary carries, and decides with exactly the eight fields
// the read returned plus a decision and an operation id it composes itself.
//
// # The admission
//
// All three go through productionDurableOpen.borrowAuthorDatabase
// (production_durable_linux.go:313-364), the identical fence the sibling read
// (production_finalized_character_linux.go:233) and every write in this lane
// already use: it parses the frozen three-key context, names the caller's own
// cookie/tab owner, refuses a project this lane never opened, re-proves the live
// lease and the admitted handle identity through the runtime, and hands over one
// (*sql.DB, release) pair whose release re-verifies the permit. A stale, foreign,
// closed or absent lease is refused there before any SQL runs, which is the Go
// side of the reference's assertCurrent (:30, :35, :40) and of
// projectAccess.assertCurrentProjectContext (generation-controller.ts:186).
//
// A decide is a write, and it takes that same borrow rather than the read-only
// admission: the borrow's release re-verifies the handle, so the permit adopts the
// content identity the decision committed instead of leaving the very next
// operation on this project refused with AUTHOR_WRITE_STALE. A list or a read
// commits nothing, so its release is only the re-verification.
//
// # The scope, and why nothing here takes one
//
// The two sibling families that take a scope (FinalizedCharacterScope,
// FinalizationScope) need it because their store statements carry a project and an
// epoch as *values*. These three do not: ListPendingStateCandidates,
// ReadPendingStateCandidate and DecideStateCandidate read and write the borrowed
// database itself (internal/store/character_state_candidates.go:209, :308, :374),
// and every row they touch is already confined to the one admitted project. The
// admitted project and lease therefore determine the scope by *being* the borrow,
// and they can only come from the frozen context: the request bodies carry a draft
// id, a candidate key and the candidate's own binding, and there is no field in
// either of them that could name a project, an epoch or a database. Nothing in
// this file ever reads a project id out of a body, so a body cannot forge one.
//
// # The store calls
//
//	ListPendingStateCandidates  opens its own read-only transaction, issues SELECTs
//	                            and commits without a write (:216-229).
//	ReadPendingStateCandidate   opens its own read-only transaction (:313-326); the
//	                            reference upgrades a preserved legacy locator inside
//	                            its own immediate transaction (:507-511), this port's
//	                            read does not write, so the derived key is returned
//	                            without being persisted.
//	DecideStateCandidate        is exactly one BEGIN IMMEDIATE transaction whose
//	                            body is one function (:391-403), so the receipt, the
//	                            candidate store and the character field cannot be
//	                            split by another writer.
//
// Because each call owns its transaction, the adapter wraps none of them in a
// second one: a nested transaction would add a lock without adding an invariant,
// and it would break the decide's "one body, one transaction" shape.
//
// The decide's receipt is returned exactly as the store produced it
// (FinalizedCharacterStateDecisionReceipt, character_state_candidates.go:187-196):
// candidateKey, operationId, payloadHash, decision, source and idempotent, in the
// reference's key order and with the reference's two idempotent values (false on
// the decision this call took, true on a replayed one, summary-repository.ts:566
// and :534). The adapter renames nothing, recomputes
// nothing and never mints an operation id or a payload hash of its own.
//
// # The request guard, per operation
//
// list takes no request argument at all (arity 0), so the only wire argument after
// it is the frozen context.
//
// read takes `{ draftId, candidateKey }` (:24) and the reference validates both
// fields inside the repository (summary-repository.ts:498-500):
//
//	!database || !Number.isSafeInteger(draftId) || draftId < 1 || !candidateKey?.trim()
//	  -> FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID
//
// This file reproduces the two shape questions the store's typed signature cannot
// ask - is draftId a safe integer, is candidateKey text at all - and hands the
// values on. The remaining checks (draftId < 1, a blank key) run again inside
// store.ReadPendingStateCandidate (:310) and refuse with the same value, so the
// guard exists once as a rule and the code is published once.
//
// A shape refusal here returns the store's own value rather than a
// security.HTTPError with a 400, because the value questions are refused by the
// store under the identical value: one code answered with two HTTP statuses
// depending on which half of the same guard caught the body would be a wire
// contract that contradicts itself. The status the transport derives is the
// lane's usual one for a symbolic refusal; the code is what the renderer reads.
//
// decide takes the eight keys of FinalizedCharacterStateDecisionRequest
// (src/shared/finalized-continuity.ts:91-100) and the reference validates all of
// them (summary-repository.ts:518-523):
//
//	!database || !request || !Number.isSafeInteger(request.draftId) || request.draftId < 1
//	  || !request.candidateKey?.trim() || !request.characterId?.trim()
//	  || !CHARACTER_STATE_FIELDS.has(request.field)
//	  || !Number.isSafeInteger(request.expectedFieldRevision) || request.expectedFieldRevision < 0
//	  || !/^[a-f0-9]{64}$/u.test(request.expectedFieldValueHash) || !request.operationId?.trim()
//	  || request.decision !== 'accept' && request.decision !== 'decline'
//	  -> FINALIZED_CHARACTER_STATE_DECISION_INVALID
//
// The reproduction splits the same way: this file answers whether each field is
// present and of the declared Go type, and store.DecideStateCandidate (:382-389)
// answers the value questions (the range, the field list, the hash shape and the
// two decision arms) with its own identical guard.
//
// Neither channel has a key allowlist. The reference's request projections read
// named fields and ignore anything else - unlike its commit, which refuses an
// unknown key (finalized-character-generation.ts:66) - so an extra key in either
// body is not refused here either. Inventing that refusal would be stricter than
// the reference and would refuse a body the reference accepts.
//
// A non-text candidateKey, characterId, field, expectedFieldValueHash, operationId
// or decision is refused with the operation's own invalid code rather than being
// coerced: the store's signatures are typed text, and a port that coerced
// `candidateKey: 1` into "1" would accept a body the reference throws on.
//
// # The refusals
//
// The reference throws; this lane answers the same symbolic code. Every store
// value already carries its own string, and the table below is the one place that
// says so, so a wire code and the store value cannot drift apart unnoticed.
//
// The mapping runs exactly once, in the handler, through one errors.Is chain.
// productionCharacterStateCandidateList/Read/Decide hand their errors back
// untouched, so errors.Is still names the refusal to a test - and mapping twice
// would map the mapped value (a plain error carrying only the code) and publish
// the unmapped-failure code for a refusal that has a name.
//
// The borrow's own refusals are NOT run through that mapper: they are already
// symbolic (PRODUCTION_CLOSED, PROJECT_NOT_OPEN, PROJECT_SESSION_STALE,
// AUTHOR_WRITE_STALE) and are returned unchanged, exactly as the sibling read
// returns them. So is a malformed frozen context (PROJECT_SESSION_REQUIRED, 400),
// and so is the fail-closed half-borrow arm below.
//
// Three refusals on these paths belong to shared helpers rather than to the
// candidate family: a freeze receipt that does not hash to its bytes
// (summary-repository.ts:75), a stored field revision that is not a safe integer
// (:84) and a character row whose text this store cannot represent
// (CHARACTER_INPUT_UNSUPPORTED). They are mapped under their own strings instead
// of being published as GENERATION_REQUEST_FAILED - the reference's
// generationIpcErrorCode passes every `CHARACTER_`/`FINALIZED_CHARACTER_` message
// through verbatim (generation-controller.ts:83-84) - and the read family's table
// (production_finalized_character_linux.go:169-190) publishes the first two under
// the same two strings, so one refusal never has two codes.
//
// What is left - a SQLite diagnostic, a path, a provider sentence, and the
// port-only ErrSummaryInputUnsupported that refuses a candidate store this port
// cannot re-encode byte for byte (internal/store/summary.go:58-60) - is replaced
// by the reference's unmapped-failure code, generationIpcErrorCode's last line
// (generation-controller.ts:101). Never a SQLite message and never a draft body.
type productionCharacterStateCandidates struct {
	durable *productionDurableOpen
}

// The three names this file binds, spelled once so a test and the binder cannot
// drift from each other.
const (
	productionCharacterStateCandidatesList   = "finalized-character:list-state-candidates"
	productionCharacterStateCandidatesRead   = "finalized-character:read-state-candidate"
	productionCharacterStateCandidatesDecide = "finalized-character:decide-state-candidate"
)

// productionCharacterStateCandidateRefusals is the store's refusal set with the
// code each one is published under. Every code is the value's own string; the
// table exists so the mapping is stated once, next to the line that raises it.
var productionCharacterStateCandidateRefusals = [...]struct {
	refusal error
	code    string
}{
	// summary-repository.ts:498-500 (the read's request guard)
	{store.ErrFinalizedCharacterStateCandidateInvalid, "FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID"},
	// summary-repository.ts:505 (the key derives from no stored candidate),
	// :543 (the candidate is gone, or no longer matches the request) and :547
	// (the character row is absent or retired). One value covers the three.
	{store.ErrFinalizedCharacterStateCandidateNotFound, "FINALIZED_CHARACTER_STATE_CANDIDATE_NOT_FOUND"},
	// summary-repository.ts:349: the snapshot row, the freeze receipt and the
	// latest finalized draft of the chapter no longer describe one source.
	{store.ErrFinalizedCharacterStateSourceChanged, "FINALIZED_CHARACTER_STATE_SOURCE_CHANGED"},
	// summary-repository.ts:518-523 (the decide's request guard)
	{store.ErrFinalizedCharacterStateDecisionInvalid, "FINALIZED_CHARACTER_STATE_DECISION_INVALID"},
	// summary-repository.ts:533: the operation id is reused for another payload.
	{store.ErrFinalizedCharacterStateOperationConflict, "FINALIZED_CHARACTER_STATE_OPERATION_CONFLICT"},
	// summary-repository.ts:550 (the field moved since the candidate was staged)
	// and :557 (the UPDATE matched no live row).
	{store.ErrFinalizedCharacterStateFieldConflict, "FINALIZED_CHARACTER_STATE_FIELD_CONFLICT"},
	// summary-repository.ts:75: a freeze receipt that does not hash to its own
	// bytes. Published by the read family under this same string
	// (production_finalized_character_linux.go:186).
	{store.ErrSummaryCharacterSnapshotInvalid, "FINALIZED_CHARACTER_SNAPSHOT_INVALID"},
	// summary-repository.ts:84: a stored provenance revision that is not a safe
	// integer, raised while the candidate's expectation is derived.
	{store.ErrFinalizedCharacterFieldRevisionInvalid, "FINALIZED_CHARACTER_FIELD_REVISION_INVALID"},
	// The projection refresh of an accepted decision (character_roster.go:405,
	// :440) refuses a character row whose text has no UTF-8 representation.
	{store.ErrCharacterInputUnsupported, "CHARACTER_INPUT_UNSUPPORTED"},
}

// newProductionCharacterStateCandidates wraps the lane's durable bridge, the only
// object that can name the author database.
func newProductionCharacterStateCandidates(durable *productionDurableOpen) *productionCharacterStateCandidates {
	return &productionCharacterStateCandidates{durable: durable}
}

// Bind installs the three candidate names and nothing else. A nil or closed bridge
// installs nothing, so the names keep failing closed at GO_OPERATION_NOT_MIGRATED
// instead of being published as handlers that can only refuse. Bind refuses an
// uninventoried name (OPERATION_NOT_EXPOSED) and a double bind
// (OPERATION_ALREADY_BOUND), so a collision with another binder is a startup error
// rather than silent dead code - which is why no two binders in this package may
// own one of these three names.
func (c *productionCharacterStateCandidates) Bind(server *transport.Server) error {
	if c == nil || c.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, binding := range [...]struct {
		name  string
		arity int
		call  func(context.Context, *sql.DB, []any) (any, error)
	}{
		// Arity is the wire arity of the reference's register call
		// (generation-controller.ts:220-222); the frozen context follows it.
		{productionCharacterStateCandidatesList, 0, c.list},
		{productionCharacterStateCandidatesRead, 1, c.read},
		{productionCharacterStateCandidatesDecide, 1, c.decide},
	} {
		if err := server.Bind(binding.name, c.operation(binding.arity, binding.call)); err != nil {
			return err
		}
	}
	return nil
}

// operation is the one adapter all three names share: the arity check, the frozen
// three-key context, the borrow, and the single refusal mapping. Nothing in it can
// take a project, an epoch or a database from the request.
func (c *productionCharacterStateCandidates) operation(arity int,
	call func(context.Context, *sql.DB, []any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		// The wire arity is the request arity (from the reference's register call)
		// plus the frozen context.
		if len(args) != arity+1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		// The reference refuses a missing or malformed session with
		// GENERATION_PROJECT_SESSION_REQUIRED (generation-controller.ts:184). The
		// frozen context is this lane's transport envelope, and every binder in it
		// answers with the transport's own code.
		if _, err := projectsession.ParseContext(args[arity]); err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		if ctx == nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		db, release, err := c.durable.borrowAuthorDatabase(ctx, caller, args[arity])
		if err != nil {
			// Already symbolic: a lease, ownership or closed-lane refusal. It is
			// not a store refusal, so it does not go through the mapper below.
			return nil, err
		}
		if db == nil || release == nil {
			// The bridge never returns a half-open borrow; if it ever does the lane
			// fails closed rather than reading a database nobody admitted.
			return nil, errors.New(productionFinalizedCharacterRequestFailed)
		}
		defer release()
		answer, err := call(ctx, db, args[:arity])
		if err != nil {
			// The one and only mapping layer: the store's refusal is turned into its
			// symbolic code here, and nowhere else.
			return nil, productionCharacterStateCandidateFailure(err)
		}
		return answer, nil
	}
}

// list serves `finalized-character:list-state-candidates`: the pending candidates
// of the borrowed project, projected as summaries (:82-89) in the reference's key
// order. It reads no request argument.
func (c *productionCharacterStateCandidates) list(ctx context.Context, db *sql.DB, _ []any) (any, error) {
	return productionCharacterStateCandidateList(ctx, db)
}

// read serves `finalized-character:read-state-candidate`: one stored candidate,
// addressed by the draft id and the candidate key its summary carried.
func (c *productionCharacterStateCandidates) read(ctx context.Context, db *sql.DB, args []any) (any, error) {
	draftID, candidateKey, err := productionCharacterStateCandidateReadRequest(args[0])
	if err != nil {
		return nil, err
	}
	return productionCharacterStateCandidateRead(ctx, db, draftID, candidateKey)
}

// decide serves `finalized-character:decide-state-candidate`: one decision, taken
// in one transaction, answered with the receipt the store produced.
func (c *productionCharacterStateCandidates) decide(ctx context.Context, db *sql.DB, args []any) (any, error) {
	request, err := productionCharacterStateCandidateDecisionRequest(args[0])
	if err != nil {
		return nil, err
	}
	return productionCharacterStateCandidateDecide(ctx, db, request)
}

// productionCharacterStateCandidateList is the served list over an already
// admitted database. Its errors are the store's own values, unmapped: the handler
// is the one place that turns them into wire codes, so a caller can still ask
// errors.Is which refusal it was.
func productionCharacterStateCandidateList(ctx context.Context, db *sql.DB) ([]store.FinalizedCharacterStateCandidateSummary, error) {
	// One read-only transaction, opened and committed by the store itself
	// (character_state_candidates.go:216-229).
	return store.NewFinalizedCharacters(db).ListPendingStateCandidates(ctx)
}

// productionCharacterStateCandidateRead is the same shape for the single read
// (:313-326). It writes nothing: a legacy locator's derived key is returned
// without being persisted.
func productionCharacterStateCandidateRead(ctx context.Context, db *sql.DB, draftID int64,
	candidateKey string) (*store.FinalizedCharacterStateCandidate, error) {
	return store.NewFinalizedCharacters(db).ReadPendingStateCandidate(ctx, draftID, candidateKey)
}

// productionCharacterStateCandidateDecide is the decision over an already admitted
// database: the store opens the one BEGIN IMMEDIATE transaction (:391-403) and the
// receipt comes back untouched.
func productionCharacterStateCandidateDecide(ctx context.Context, db *sql.DB,
	request store.FinalizedCharacterStateDecisionRequest) (*store.FinalizedCharacterStateDecisionReceipt, error) {
	return store.NewFinalizedCharacters(db).DecideStateCandidate(ctx, request)
}

// productionCharacterStateCandidateReadRequest is the `{ draftId, candidateKey }`
// projection of src/shared/finalized-character-generation.ts:24. A body that is not
// an object, a draft id that is not a safe integer and a candidate key that is not
// text are the reference's own FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID
// (summary-repository.ts:498-500); the value questions (draftId < 1, a blank key)
// are left to store.ReadPendingStateCandidate (:310), which raises the same value.
func productionCharacterStateCandidateReadRequest(value any) (int64, string, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return 0, "", store.ErrFinalizedCharacterStateCandidateInvalid
	}
	draftID, ok := productionCharacterStateCandidateInteger(object["draftId"])
	if !ok {
		return 0, "", store.ErrFinalizedCharacterStateCandidateInvalid
	}
	candidateKey, ok := object["candidateKey"].(string)
	if !ok {
		return 0, "", store.ErrFinalizedCharacterStateCandidateInvalid
	}
	return draftID, candidateKey, nil
}

// productionCharacterStateCandidateDecisionRequest is the eight-key projection of
// FinalizedCharacterStateDecisionRequest (src/shared/finalized-continuity.ts:91-100).
// A missing or mistyped field is the reference's own
// FINALIZED_CHARACTER_STATE_DECISION_INVALID (summary-repository.ts:518-523) rather
// than a Go zero value, because a zero would be a *valid* revision and draft id:
// refusing the shape here is what keeps an absent field from being read as 0. The
// value questions (the ranges, the field list, the hash shape and the two decision
// arms) stay with store.DecideStateCandidate (:382-389), which raises the same
// value. Extra keys are ignored, exactly as the reference's named reads ignore
// them.
func productionCharacterStateCandidateDecisionRequest(value any) (store.FinalizedCharacterStateDecisionRequest, error) {
	invalid := store.FinalizedCharacterStateDecisionRequest{}
	object, ok := value.(map[string]any)
	if !ok {
		return invalid, store.ErrFinalizedCharacterStateDecisionInvalid
	}
	text := func(key string) (string, bool) {
		field, ok := object[key].(string)
		return field, ok
	}
	candidateKey, candidateKeyOK := text("candidateKey")
	characterID, characterIDOK := text("characterId")
	field, fieldOK := text("field")
	valueHash, valueHashOK := text("expectedFieldValueHash")
	operationID, operationIDOK := text("operationId")
	decision, decisionOK := text("decision")
	draftID, draftIDOK := productionCharacterStateCandidateInteger(object["draftId"])
	revision, revisionOK := productionCharacterStateCandidateInteger(object["expectedFieldRevision"])
	if !candidateKeyOK || !characterIDOK || !fieldOK || !valueHashOK || !operationIDOK ||
		!decisionOK || !draftIDOK || !revisionOK {
		return invalid, store.ErrFinalizedCharacterStateDecisionInvalid
	}
	return store.FinalizedCharacterStateDecisionRequest{
		DraftID:                draftID,
		CandidateKey:           candidateKey,
		CharacterID:            characterID,
		Field:                  field,
		ExpectedFieldRevision:  revision,
		ExpectedFieldValueHash: valueHash,
		OperationID:            operationID,
		Decision:               decision,
	}, nil
}

// productionCharacterStateCandidateInteger is the `Number.isSafeInteger` half of
// both request guards. The transport decodes a JSON number as a JavaScript number
// (internal/security/body.go:67-77), so a fractional, absent, textual or
// out-of-range value is not a safe integer; a json.Number is accepted for the same
// reason the sibling read accepts it (production_finalized_character_linux.go:262-272),
// so a value from anywhere but the wire is not silently refused.
func productionCharacterStateCandidateInteger(value any) (int64, bool) {
	var number float64
	switch raw := value.(type) {
	case float64:
		number = raw
	case json.Number:
		parsed, err := raw.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number ||
		number < -productionFinalizedCharacterMaxSafeInteger || number > productionFinalizedCharacterMaxSafeInteger {
		return 0, false
	}
	return int64(number), true
}

// productionCharacterStateCandidateCode is the reference's symbolic code for a
// store refusal, or "" for anything this lane does not name.
func productionCharacterStateCandidateCode(err error) string {
	for _, entry := range productionCharacterStateCandidateRefusals {
		if errors.Is(err, entry.refusal) {
			return entry.code
		}
	}
	return ""
}

// productionCharacterStateCandidateFailure is the refusal the browser receives:
// the transport's own error unchanged, the store refusal's symbolic code, or the
// reference's unmapped-failure code. Never a SQLite message, a path, a draft body
// or a provider sentence.
func productionCharacterStateCandidateFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if code := productionCharacterStateCandidateCode(err); code != "" {
		return errors.New(code)
	}
	return errors.New(productionFinalizedCharacterRequestFailed)
}
