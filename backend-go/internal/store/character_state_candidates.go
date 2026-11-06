package store

// Native port of the three finalized-character state-candidate operations:
//
//	SummaryRepository.listPendingFinalizedCharacterStateCandidates  (electron/repositories/summary-repository.ts:478-494)
//	SummaryRepository.readPendingFinalizedCharacterStateCandidate   (:496-514)
//	SummaryRepository.decideFinalizedCharacterStateCandidate        (:516-568)
//
// plus the shared pieces they are built from: characterStateCandidateKey
// (:272-274), stateDecisionPayloadHash (:377-380), the candidate row read
// readCurrentCharacterStateCandidateRow (:334-352), the per-candidate gate
// pendingCharacterStateCandidate (:354-375) and the store parser
// parseCharacterStateCandidateStore (:179-203) over
// parseCharacterStateCandidates (:132-174).
//
// The family lives in the finalized-character state tables, not in the
// character_identity_* receipts: characters keeps the current value and its
// field-level provenance (internal/admission/baseline_v7_ddl.go:166-189,
// cs_location..cs_recent_events plus cs_provenance), and summary_snapshots keeps
// the per-draft candidate store (baseline_v7_ddl.go:630-643), one JSON text
// `{"version":2,"pending":[...],"receipts":[...]}` per row, addressed by the
// partial UNIQUE index on draft_id (baseline_v7_ddl.go:689-691).
//
// Ownership: list and read are reads. They borrow one read-only transaction, the
// same way FinalizedCharacters.ReadContext does (finalized_character_context.go:
// 191-203), issue SELECTs only and never write. A decide is exactly one
// BEGIN IMMEDIATE transaction (NewReviewRevisions.immediate, reviewrevision.go:
// 38-68) whose body is one function, finalizedCharacterStateDecide, so no two
// writes of one decision can be split by another writer. The reference's read
// upgrade (:507-511) persists a derived candidate key on a preserved legacy row;
// this port's reads never write, so the derived key is returned without being
// stored. The decide removes the decided candidate by its *derived* key
// (finalizedCharacterStateDecide) rather than by the stored one, which is what
// the reference's filter (:562) observes once the read's upgrade is not there.

import (
	"context"
	"database/sql"
	"errors"
	"sort"
)

var (
	// summary-repository.ts:498-500
	//   if (!database || !Number.isSafeInteger(draftId) || draftId < 1 || !candidateKey?.trim())
	//     throw new Error('FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID')
	ErrFinalizedCharacterStateCandidateInvalid = errors.New("FINALIZED_CHARACTER_STATE_CANDIDATE_INVALID")
	// summary-repository.ts:349
	//   throw new Error('FINALIZED_CHARACTER_STATE_SOURCE_CHANGED')
	ErrFinalizedCharacterStateSourceChanged = errors.New("FINALIZED_CHARACTER_STATE_SOURCE_CHANGED")
	// summary-repository.ts:505
	//   if (index < 0) throw new Error('FINALIZED_CHARACTER_STATE_CANDIDATE_NOT_FOUND')
	// summary-repository.ts:543 (the candidate is gone, or no longer matches the request)
	// summary-repository.ts:547 (the character row is retired or absent)
	ErrFinalizedCharacterStateCandidateNotFound = errors.New("FINALIZED_CHARACTER_STATE_CANDIDATE_NOT_FOUND")
	// summary-repository.ts:518-523
	//   if (!database || !request || ... || request.decision !== 'accept' && request.decision !== 'decline')
	//     throw new Error('FINALIZED_CHARACTER_STATE_DECISION_INVALID')
	ErrFinalizedCharacterStateDecisionInvalid = errors.New("FINALIZED_CHARACTER_STATE_DECISION_INVALID")
	// summary-repository.ts:533
	//   if (replay.payloadHash !== payloadHash) throw new Error('FINALIZED_CHARACTER_STATE_OPERATION_CONFLICT')
	ErrFinalizedCharacterStateOperationConflict = errors.New("FINALIZED_CHARACTER_STATE_OPERATION_CONFLICT")
	// summary-repository.ts:550
	//   if (current.revision !== request.expectedFieldRevision || current.valueHash !== ...)
	//     throw new Error('FINALIZED_CHARACTER_STATE_FIELD_CONFLICT')
	// summary-repository.ts:557
	//   if (updated.changes !== 1) throw new Error('FINALIZED_CHARACTER_STATE_FIELD_CONFLICT')
	ErrFinalizedCharacterStateFieldConflict = errors.New("FINALIZED_CHARACTER_STATE_FIELD_CONFLICT")
)

// The two decisions of a FinalizedCharacterStateDecisionRequest
// (src/shared/finalized-continuity.ts:99).
const (
	FinalizedCharacterStateDecisionAccept  = "accept"
	FinalizedCharacterStateDecisionDecline = "decline"
)

// finalizedCharacterStateKeyPrefix is the literal prefix of
// characterStateCandidateKey (:272-274). The candidate key is not an opaque
// handle: a stored candidate is looked up by recomputing it, so the derivation
// below must stay byte-for-byte the reference's.
const finalizedCharacterStateKeyPrefix = "fcs-state:"

// The two throwaway scopes the reference inlines into stateSnapshot (:372, :548).
// stateSnapshot only reads a field's value, revision and provenance, never the
// scope it is handed, so the scope text is not observable.
const (
	finalizedCharacterStateUpgradeScope  = "candidate-upgrade"
	finalizedCharacterStateDecisionScope = "candidate-decision"
)

// FinalizedCharacterStateCandidateSummary is
// PendingFinalizedCharacterStateCandidateSummary (:82-89) in the reference's key
// order. The list projection carries no value and no expectation, only the
// identity a later read or decide is addressed by.
type FinalizedCharacterStateCandidateSummary struct {
	DraftID        int64  `json:"draftId"`
	CandidateKey   string `json:"candidateKey"`
	FinalizationID string `json:"finalizationId"`
	CharacterID    string `json:"characterId"`
	CharacterName  string `json:"characterName"`
	Field          string `json:"field"`
}

// FinalizedCharacterStateCandidate is PendingFinalizedCharacterStateCandidate
// (:74-80): the stored candidate plus the bound metadata a decide must echo back,
// in the reference's key order. The pointer fields keep its two distinct
// absences: omitted on a preserved legacy locator (:57-58) versus stored as an
// empty string. CharacterID, Source, ExpectedFieldRevision and
// ExpectedFieldValueHash are always set here, because
// pendingCharacterStateCandidate (:354-375) derives all four for a legacy row.
type FinalizedCharacterStateCandidate struct {
	CharacterID            string                           `json:"characterId"`
	CharacterName          string                           `json:"characterName"`
	Field                  string                           `json:"field"`
	Value                  string                           `json:"value"`
	Evidence               any                              `json:"evidence,omitempty"`
	SelectionKey           *string                          `json:"selectionKey,omitempty"`
	DisplayName            *string                          `json:"displayName,omitempty"`
	RawValue               *any                             `json:"rawValue,omitempty"`
	Reason                 *string                          `json:"reason,omitempty"`
	CandidateKey           string                           `json:"candidateKey"`
	Source                 FinalizedCharacterSourceIdentity `json:"source"`
	ExpectedFieldRevision  int64                            `json:"expectedFieldRevision"`
	ExpectedFieldValueHash string                           `json:"expectedFieldValueHash"`
}

// FinalizedCharacterStateDecisionRequest is
// FinalizedCharacterStateDecisionRequest (src/shared/finalized-continuity.ts:
// 91-100) in the reference's key order.
type FinalizedCharacterStateDecisionRequest struct {
	DraftID                int64  `json:"draftId"`
	CandidateKey           string `json:"candidateKey"`
	CharacterID            string `json:"characterId"`
	Field                  string `json:"field"`
	ExpectedFieldRevision  int64  `json:"expectedFieldRevision"`
	ExpectedFieldValueHash string `json:"expectedFieldValueHash"`
	OperationID            string `json:"operationId"`
	Decision               string `json:"decision"`
}

// FinalizedCharacterStateDecisionReceipt is
// FinalizedCharacterStateDecisionReceipt (:102-109). Idempotent is false on the
// decision this call took and true on a replayed one (:534, :566).
type FinalizedCharacterStateDecisionReceipt struct {
	CandidateKey string                           `json:"candidateKey"`
	OperationID  string                           `json:"operationId"`
	PayloadHash  string                           `json:"payloadHash"`
	Decision     string                           `json:"decision"`
	Source       FinalizedCharacterSourceIdentity `json:"source"`
	Idempotent   bool                             `json:"idempotent"`
}

// finalizedCharacterStateStoredCandidate is one entry of
// parseCharacterStateCandidates (:139-170) after the reference has normalized it:
// a nil pointer means the key is absent from the object. Every bound field is
// either absent or present-and-valid, because :144-148 drops a candidate whose
// bound metadata is only partly readable instead of reinterpreting it as legacy.
type finalizedCharacterStateStoredCandidate struct {
	characterID            *string
	characterName          string
	field                  string
	value                  string
	evidence               any
	selectionKey           *string
	displayName            *string
	rawValue               any
	rawValuePresent        bool
	reason                 *string
	candidateKey           *string
	source                 map[string]any
	expectedFieldRevision  *int64
	expectedFieldValueHash *string
}

// finalizedCharacterStateStoredReceipt is one entry of the store's receipts
// (:187-198).
type finalizedCharacterStateStoredReceipt struct {
	candidateKey string
	operationID  string
	payloadHash  string
	decision     string
	source       map[string]any
}

// finalizedCharacterStateStore is CharacterStateCandidateStore (:177) as this
// port keeps it. Version is not a field: every path that writes a store writes
// version 2 (:182, :199), so the literal 2 is emitted unconditionally.
type finalizedCharacterStateStore struct {
	pending  []finalizedCharacterStateStoredCandidate
	receipts []finalizedCharacterStateStoredReceipt
}

// finalizedCharacterStateRow is CharacterStateCandidateRow (:324-332): the store
// text plus the finalized source that snapshot row was staged against.
type finalizedCharacterStateRow struct {
	ID         int64
	Candidates string
	Source     FinalizedCharacterSourceIdentity
}

// ListPendingStateCandidates is
// SummaryRepository.listPendingFinalizedCharacterStateCandidates (:478-494).
//
// A missing database is the :479 `if (!database) return []` arm, and stays an
// empty list rather than an error: a project that is not open has no candidates.
// One broken draft is skipped (:490-491 catch), so a single unreadable store can
// never hide the rest.
func (f *FinalizedCharacters) ListPendingStateCandidates(ctx context.Context) ([]FinalizedCharacterStateCandidateSummary, error) {
	if f == nil || f.db == nil {
		return []FinalizedCharacterStateCandidateSummary{}, nil
	}
	// Every row below shares one SQLite snapshot, so a concurrent decision cannot
	// produce a list that mixes two stores. Never re-enter f.db inside the
	// transaction.
	tx, err := f.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := finalizedCharacterStateListPending(ctx, tx)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// finalizedCharacterStateListPending is the read half of :478-494.
func finalizedCharacterStateListPending(ctx context.Context, q rrQueryer) ([]FinalizedCharacterStateCandidateSummary, error) {
	// :480-481
	//   SELECT draft_id AS draftId FROM summary_snapshots WHERE character_state_candidates<>'[]'
	// The prepare/all sits *outside* the reference's try: a database that cannot
	// answer this query refuses the whole list, while a per-draft failure below is
	// swallowed exactly like :490-491.
	rows, err := rrMaps(ctx, q, "SELECT draft_id AS draftId FROM summary_snapshots WHERE character_state_candidates<>'[]'")
	if err != nil {
		return nil, err
	}
	result := []FinalizedCharacterStateCandidateSummary{}
	for _, entry := range rows {
		// A snapshot row without a draft is addressed as draft_id=NULL, which the
		// :338 lookup can never match: the reference's own
		// FINALIZED_CHARACTER_STATE_SOURCE_CHANGED, caught by :490-491.
		draftID, ok := rrInteger(entry["draftId"], 1)
		if !ok {
			continue
		}
		pending, err := finalizedCharacterStateListDraft(ctx, q, draftID)
		if err != nil {
			continue
		}
		result = append(result, pending...)
	}
	// :493 .sort((left, right) => left.draftId - right.draftId || left.candidateKey.localeCompare(right.candidateKey))
	// A candidate key is the ASCII prefix plus 64 lowercase hex digits, so the
	// locale's collation only ever orders ASCII: byte order and localeCompare
	// order agree. Array.prototype.sort is stable, hence SliceStable.
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].DraftID != result[j].DraftID {
			return result[i].DraftID < result[j].DraftID
		}
		return result[i].CandidateKey < result[j].CandidateKey
	})
	return result, nil
}

// finalizedCharacterStateListDraft is the per-draft body of :484-489.
func finalizedCharacterStateListDraft(ctx context.Context, q rrQueryer, draftID int64) ([]FinalizedCharacterStateCandidateSummary, error) {
	row, err := finalizedCharacterStateReadRow(ctx, q, draftID)
	if err != nil {
		return nil, err
	}
	store := finalizedCharacterStateParseStore(row.Candidates)
	result := make([]FinalizedCharacterStateCandidateSummary, 0, len(store.pending))
	for _, candidate := range store.pending {
		pending, err := finalizedCharacterStatePendingCandidate(ctx, q, row, candidate)
		if err != nil {
			return nil, err
		}
		if pending == nil {
			continue
		}
		// :487-488 the summary is keyed by the outer row's draftId, and carries the
		// candidate's own finalization id.
		result = append(result, FinalizedCharacterStateCandidateSummary{
			DraftID:        draftID,
			CandidateKey:   pending.CandidateKey,
			FinalizationID: pending.Source.FinalizationID,
			CharacterID:    pending.CharacterID,
			CharacterName:  pending.CharacterName,
			Field:          pending.Field,
		})
	}
	return result, nil
}

// ReadPendingStateCandidate is
// SummaryRepository.readPendingFinalizedCharacterStateCandidate (:496-514).
//
// It never writes: the reference's :507-511 upgrade arm stores the derived key on
// a preserved legacy row inside its own immediate transaction, and this port's
// reads do not write, so the derived key is returned without being persisted.
// The derivation is deterministic (:272-274), so a later list, read or decide
// recomputes the same key.
func (f *FinalizedCharacters) ReadPendingStateCandidate(ctx context.Context, draftID int64, candidateKey string) (*FinalizedCharacterStateCandidate, error) {
	// :498-500 `!database || !Number.isSafeInteger(draftId) || draftId < 1 || !candidateKey?.trim()`
	if f == nil || f.db == nil || draftID < 1 || draftID > 9007199254740991 || summaryTrim(candidateKey) == "" {
		return nil, ErrFinalizedCharacterStateCandidateInvalid
	}
	tx, err := f.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := finalizedCharacterStateReadCandidate(ctx, tx, draftID, candidateKey)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// finalizedCharacterStateReadCandidate is the transaction body of :501-513.
func finalizedCharacterStateReadCandidate(ctx context.Context, q rrQueryer, draftID int64, candidateKey string) (*FinalizedCharacterStateCandidate, error) {
	// :502
	row, err := finalizedCharacterStateReadRow(ctx, q, draftID)
	if err != nil {
		return nil, err
	}
	store := finalizedCharacterStateParseStore(row.Candidates)
	// :504 store.pending.findIndex(item => pendingCharacterStateCandidate(...)?.candidateKey === candidateKey)
	// The key is recomputed from the stored value rather than trusted, so a
	// stored key that no longer derives is simply not found.
	for _, candidate := range store.pending {
		pending, err := finalizedCharacterStatePendingCandidate(ctx, q, row, candidate)
		if err != nil {
			return nil, err
		}
		if pending == nil || pending.CandidateKey != candidateKey {
			continue
		}
		return pending, nil
	}
	// :505 `if (index < 0) throw new Error('FINALIZED_CHARACTER_STATE_CANDIDATE_NOT_FOUND')`
	return nil, ErrFinalizedCharacterStateCandidateNotFound
}

// DecideStateCandidate is
// SummaryRepository.decideFinalizedCharacterStateCandidate (:516-568): one
// BEGIN IMMEDIATE transaction, one body function, so the receipt, the candidate
// store and the character field can never be split by another writer.
//
// The reference has exactly two arms, `accept` and `decline`, and three outcomes:
//
//	accept   + the field still holds the expected value and revision -> the value
//	         is applied as an author edit and the roster projection is refreshed
//	         (:545-559);
//	accept   + anything else -> FINALIZED_CHARACTER_STATE_FIELD_CONFLICT (:549-551,
//	         :557) with the candidate left pending;
//	decline  -> nothing is applied and the decision is staged in the store's
//	         receipts (:560-566), which is how an author-protected field keeps its
//	         author value while the proposal is consumed.
//
// A decision that was already taken is replayed from its receipt (:528-535) and
// answers before the source guard below, so it survives a superseded source.
// This retires a pending candidate; it never creates one — candidate creation
// belongs to commitFinalizedCharacterStates (:455-462) through
// saveFinalizedCharacterStateCandidates (:649-704), which are outside this port.
func (f *FinalizedCharacters) DecideStateCandidate(ctx context.Context, request FinalizedCharacterStateDecisionRequest) (*FinalizedCharacterStateDecisionReceipt, error) {
	// :518-523
	//   if (!database || !request || !Number.isSafeInteger(request.draftId) || request.draftId < 1
	//     || !request.candidateKey?.trim() || !request.characterId?.trim() || !CHARACTER_STATE_FIELDS.has(request.field)
	//     || !Number.isSafeInteger(request.expectedFieldRevision) || request.expectedFieldRevision < 0
	//     || !/^[a-f0-9]{64}$/u.test(request.expectedFieldValueHash) || !request.operationId?.trim()
	//     || request.decision !== 'accept' && request.decision !== 'decline')
	//     throw new Error('FINALIZED_CHARACTER_STATE_DECISION_INVALID')
	if f == nil || f.db == nil || request.DraftID < 1 || request.DraftID > 9007199254740991 ||
		summaryTrim(request.CandidateKey) == "" || summaryTrim(request.CharacterID) == "" ||
		!summaryStateField(request.Field) || request.ExpectedFieldRevision < 0 ||
		request.ExpectedFieldRevision > 9007199254740991 ||
		!summaryHashValid(request.ExpectedFieldValueHash) || summaryTrim(request.OperationID) == "" ||
		(request.Decision != FinalizedCharacterStateDecisionAccept && request.Decision != FinalizedCharacterStateDecisionDecline) {
		return nil, ErrFinalizedCharacterStateDecisionInvalid
	}
	var receipt *FinalizedCharacterStateDecisionReceipt
	err := NewReviewRevisions(f.db).immediate(ctx, func(conn *sql.Conn) error {
		result, err := finalizedCharacterStateDecide(ctx, conn, request)
		if err != nil {
			return err
		}
		receipt = result
		return nil
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

// finalizedCharacterStateDecide is the single transaction body of :526-567.
func finalizedCharacterStateDecide(ctx context.Context, conn *sql.Conn, request FinalizedCharacterStateDecisionRequest) (*FinalizedCharacterStateDecisionReceipt, error) {
	// :527
	payloadHash, err := finalizedCharacterStateDecisionPayloadHash(request)
	if err != nil {
		return nil, err
	}
	// :528-535 the replay scan. It reads every snapshot row that still holds a
	// store, not just this request's draft, and it answers before the source guard
	// below: a decision already taken keeps returning its receipt.
	receiptRows, err := rrMaps(ctx, conn, "SELECT character_state_candidates AS candidates FROM summary_snapshots WHERE character_state_candidates<>'[]'")
	if err != nil {
		return nil, err
	}
	for _, entry := range receiptRows {
		text, ok := entry["candidates"].(string)
		if !ok {
			continue
		}
		for _, receipt := range finalizedCharacterStateParseStore(text).receipts {
			if receipt.operationID != request.OperationID {
				continue
			}
			// :533 `if (replay.payloadHash !== payloadHash) throw new Error('FINALIZED_CHARACTER_STATE_OPERATION_CONFLICT')`
			if receipt.payloadHash != payloadHash {
				return nil, ErrFinalizedCharacterStateOperationConflict
			}
			// :534 `return { ...structuredClone(replay), idempotent: true }` — a
			// replay writes nothing at all. The receipt filter already validated
			// these four identity fields against summarySource (:194), so the source
			// decodes without a second refusal.
			return &FinalizedCharacterStateDecisionReceipt{
				CandidateKey: receipt.candidateKey,
				OperationID:  receipt.operationID,
				PayloadHash:  receipt.payloadHash,
				Decision:     receipt.decision,
				Source:       finalizedCharacterStateSourceFromMap(receipt.source),
				Idempotent:   true,
			}, nil
		}
	}
	// :536
	row, err := finalizedCharacterStateReadRow(ctx, conn, request.DraftID)
	if err != nil {
		return nil, err
	}
	// The store is re-serialized below (:564-565). Text this port cannot re-encode
	// byte for byte (a lone UTF-16 surrogate, a non-finite number) is refused
	// instead of being silently normalized, the boundary summary.go:55-57 already
	// draws for the same column. There is no reference code for it; a silent
	// rewrite is the one outcome that is worse than the refusal.
	if err := summaryJSONSupported(row.Candidates); err != nil {
		return nil, err
	}
	store := finalizedCharacterStateParseStore(row.Candidates)
	// :538-539 store.pending.map(item => pendingCharacterStateCandidate(db, row, item))
	// each pending entry is gated once, so the write-back below can identify the
	// decided candidate by the key it derives rather than by a stored key the read
	// upgrade would have added.
	derived := make([]*FinalizedCharacterStateCandidate, len(store.pending))
	for index, candidate := range store.pending {
		pending, err := finalizedCharacterStatePendingCandidate(ctx, conn, row, candidate)
		if err != nil {
			return nil, err
		}
		derived[index] = pending
	}
	decided := (*FinalizedCharacterStateCandidate)(nil)
	for _, pending := range derived {
		if pending != nil && pending.CandidateKey == request.CandidateKey {
			decided = pending
			break
		}
	}
	// :540-544
	//   if (!candidate || candidate.characterId !== request.characterId || candidate.field !== request.field
	//     || candidate.expectedFieldRevision !== request.expectedFieldRevision
	//     || candidate.expectedFieldValueHash !== request.expectedFieldValueHash)
	//     throw new Error('FINALIZED_CHARACTER_STATE_CANDIDATE_NOT_FOUND')
	if decided == nil || decided.CharacterID != request.CharacterID || decided.Field != request.Field ||
		decided.ExpectedFieldRevision != request.ExpectedFieldRevision ||
		decided.ExpectedFieldValueHash != request.ExpectedFieldValueHash {
		return nil, ErrFinalizedCharacterStateCandidateNotFound
	}
	// :545 `if (request.decision === 'accept')` — the decline arm deliberately
	// reads no character row at all.
	if request.Decision == FinalizedCharacterStateDecisionAccept {
		if err := finalizedCharacterStateApplyAccept(ctx, conn, decided); err != nil {
			return nil, err
		}
	}
	// :560-561
	receipt := finalizedCharacterStateStoredReceipt{
		candidateKey: decided.CandidateKey,
		operationID:  request.OperationID,
		payloadHash:  payloadHash,
		decision:     request.Decision,
		source:       finalizedCharacterStateSourceRecord(decided.Source),
	}
	// :562 `store.pending = store.pending.filter(item => item.candidateKey !== candidate.candidateKey)`
	remaining := make([]finalizedCharacterStateStoredCandidate, 0, len(store.pending))
	for index, candidate := range store.pending {
		if derived[index] != nil && derived[index].CandidateKey == decided.CandidateKey {
			continue
		}
		remaining = append(remaining, candidate)
	}
	store.pending = remaining
	// :563 `store.receipts.push(receipt)`
	store.receipts = append(store.receipts, receipt)
	text, err := finalizedCharacterStateStoreText(store)
	if err != nil {
		return nil, err
	}
	// :564-565 UPDATE summary_snapshots SET character_state_candidates=?,created_at=datetime('now') WHERE id=?
	if _, err := conn.ExecContext(ctx, "UPDATE summary_snapshots SET character_state_candidates=?,created_at=datetime('now') WHERE id=?", text, row.ID); err != nil {
		return nil, err
	}
	// :566 `return { ...structuredClone(receipt), idempotent: false }`
	return &FinalizedCharacterStateDecisionReceipt{
		CandidateKey: receipt.candidateKey,
		OperationID:  receipt.operationID,
		PayloadHash:  receipt.payloadHash,
		Decision:     receipt.decision,
		Source:       decided.Source,
		Idempotent:   false,
	}, nil
}

// finalizedCharacterStateApplyAccept is the accept arm (:545-559) for a candidate
// whose expectation still holds: the proposal becomes an author field value, the
// field's provenance records the author chapter and the next revision, and the
// roster projection is invalidated.
func finalizedCharacterStateApplyAccept(ctx context.Context, conn *sql.Conn, candidate *FinalizedCharacterStateCandidate) error {
	// :546-547 SELECT * FROM characters WHERE character_id=? AND retired=0 — a
	// retired or absent row refuses the decision, it does not apply blind.
	character, err := finalizedCharacterActiveRow(ctx, conn, candidate.CharacterID)
	if err != nil {
		return err
	}
	if character == nil {
		return ErrFinalizedCharacterStateCandidateNotFound
	}
	column, ok := finalizedCharacterStateColumn(candidate.Field)
	if !ok {
		// Unreachable: parseCharacterStateCandidates (:151) keeps only the six
		// names, so the field always has a column.
		return ErrFinalizedCharacterStateCandidateNotFound
	}
	// :548 const current = stateSnapshot(character, candidate.field, { projectId: 'candidate-decision', epoch: 'candidate-decision' })
	current, err := finalizedCharacterStateSnapshot(character, candidate.Field, column,
		FinalizedCharacterScope{ProjectID: finalizedCharacterStateDecisionScope, Epoch: finalizedCharacterStateDecisionScope},
		candidate.CharacterID)
	if err != nil {
		return err
	}
	// :549-551 the stale-revision refusal: the field moved since the candidate was
	// staged, so accepting it would overwrite an author edit.
	if current.Revision != candidate.ExpectedFieldRevision || current.ValueHash != candidate.ExpectedFieldValueHash {
		return ErrFinalizedCharacterStateFieldConflict
	}
	// :552 JSON.parse(String(character.cs_provenance || '{}')). The ordered decoder
	// is mandatory here: the merge below re-serializes untouched fields, and their
	// stored key order is part of the roster fact hash
	// (character_roster.go:654-665 takes the same care for the author path).
	stored, err := rosterProvenance(character["cs_provenance"])
	if err != nil {
		return err
	}
	// :553 provenance[field] = { kind: 'author', chapterNumber: candidate.source.chapterNumber,
	//   revision: current.revision + 1 }
	// Assignment keeps an existing key's position and appends a new one, which is
	// what this merge does too.
	authored := plotObject{{"kind", "author"}, {"chapterNumber", candidate.Source.ChapterNumber},
		{"revision", current.Revision + 1}}
	merged := make(plotObject, 0, len(stored)+1)
	replaced := false
	for _, field := range stored {
		if field.key == candidate.Field {
			merged = append(merged, plotField{candidate.Field, authored})
			replaced = true
			continue
		}
		merged = append(merged, field)
	}
	if !replaced {
		merged = append(merged, plotField{candidate.Field, authored})
	}
	provenance, err := plotJSON(merged)
	if err != nil {
		return err
	}
	// :554-557
	//   UPDATE characters SET <STATE_COLUMNS[field]>=?,cs_provenance=?,cs_updated_at_chapter=MAX(COALESCE(cs_updated_at_chapter,0),?),
	//     updated_at=datetime('now') WHERE character_id=? AND retired=0
	//   if (updated.changes !== 1) throw new Error('FINALIZED_CHARACTER_STATE_FIELD_CONFLICT')
	update, err := conn.ExecContext(ctx, "UPDATE characters SET "+column+"=?,cs_provenance=?,"+
		"cs_updated_at_chapter=MAX(COALESCE(cs_updated_at_chapter,0),?),updated_at=datetime('now') "+
		"WHERE character_id=? AND retired=0",
		candidate.Value, provenance, candidate.Source.ChapterNumber, candidate.CharacterID)
	if err := rrAffected(update, err, ErrFinalizedCharacterStateFieldConflict); err != nil {
		return err
	}
	// :558 refreshCharacterStateProjection(db)
	return finalizedCharacterStateRefreshProjection(ctx, conn)
}

// finalizedCharacterStateRefreshProjection ports refreshCharacterStateProjection
// (electron/repositories/character-roster-repository.ts:888-892):
//
//	UPDATE character_roster_meta SET revision=revision+1,fact_hash=?,updated_at=datetime('now') WHERE id='main'
//
// with fullFactHash(identityProjectionEntries(db)) (:750-752). A dynamic state
// decision invalidates roster readers and must never rewrite the author's
// compatibility prose, which is exactly why characterIdentityRefreshProjection
// (character_roster.go:562-582) cannot stand in for it: that one also rewrites
// project_core.characters_arch (:574-576) and forces migration_state 'ready'
// (:577). The fact hash is the name-keyed canonical digest
// characterizationEntries(…, identity=false) produces, the same digest the
// identity refresh persists (character_roster.go:568-570).
//
// The reference's own guard (:889 `!db.inTransaction || !hasCharacterIdentitySchema(db)`)
// cannot fire here: this runs inside the decide's immediate transaction, and the
// frozen snapshot pendingCharacterStateCandidate demanded proves the identity
// schema is present.
func finalizedCharacterStateRefreshProjection(ctx context.Context, conn *sql.Conn) error {
	entries, provenance, _, _, err := rosterFacts(ctx, conn)
	if err != nil {
		return err
	}
	facts, err := rosterEntriesJSON(entries, provenance, false)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE character_roster_meta SET revision=revision+1,fact_hash=?,updated_at=datetime('now') WHERE id='main'",
		rosterHash(facts)); err != nil {
		return err
	}
	return nil
}

// finalizedCharacterStateCandidateKey is characterStateCandidateKey (:272-274):
//
//	`fcs-state:${sha256(JSON.stringify([source.draftId, source.finalizationId, source.contentHash,
//	  characterId, field, sha256(value)]))}`
//
// The digest covers exactly six array elements in that order, with the field value
// itself digested first, and it is a JSON array, not a join: the separators,
// the JSON string escaping and the number text are all part of the input. plotJSON
// is this package's JavaScript JSON.stringify (plottree.go:31-33 explains why the
// encoder and not encoding/json produces the bytes), so the key is reproducible
// byte for byte.
func finalizedCharacterStateCandidateKey(source FinalizedCharacterSourceIdentity, characterID, field, value string) (string, error) {
	encoded, err := plotJSON([]any{source.DraftID, source.FinalizationID, source.ContentHash, characterID, field, summaryHash(value)})
	if err != nil {
		return "", err
	}
	return finalizedCharacterStateKeyPrefix + summaryHash(encoded), nil
}

// finalizedCharacterStateDecisionPayloadHash is stateDecisionPayloadHash (:377-380):
//
//	sha256(JSON.stringify([request.draftId, request.candidateKey, request.characterId, request.field,
//	  request.expectedFieldRevision, request.expectedFieldValueHash, request.operationId, request.decision]))
//
// Eight elements in that order, so the operation id is bound to the decision it
// names: the same operation id with a different arm is an operation conflict.
func finalizedCharacterStateDecisionPayloadHash(request FinalizedCharacterStateDecisionRequest) (string, error) {
	encoded, err := plotJSON([]any{request.DraftID, request.CandidateKey, request.CharacterID, request.Field,
		request.ExpectedFieldRevision, request.ExpectedFieldValueHash, request.OperationID, request.Decision})
	if err != nil {
		return "", err
	}
	return summaryHash(encoded), nil
}

// finalizedCharacterStateReadRow is readCurrentCharacterStateCandidateRow
// (:334-352): the candidate store of one draft together with the finalized source
// it was staged against. Every disagreement is
// FINALIZED_CHARACTER_STATE_SOURCE_CHANGED (:349).
func finalizedCharacterStateReadRow(ctx context.Context, q rrQueryer, draftID int64) (*finalizedCharacterStateRow, error) {
	// :335-338 one statement. summary_snapshots carries a partial UNIQUE index on
	// draft_id (baseline_v7_ddl.go:689-691), so at most one row matches, and
	// WHERE draft_id=? never matches a NULL draft_id; the unordered single row is
	// the only row.
	rows, err := rrMaps(ctx, q, `SELECT id,draft_id AS draftId,chapter_number AS chapterNumber,
 character_state_candidates AS candidates,source_finalization_id AS finalizationId,
 source_content_hash AS contentHash,projection_generation AS projectionGeneration
 FROM summary_snapshots WHERE draft_id=?`, draftID)
	if err != nil {
		return nil, err
	}
	// :339 const snapshot = readFinalizedSourceFromDb(db, draftId)
	snapshot, err := finalizedCharacterReadSource(ctx, q, draftID)
	if err != nil {
		return nil, err
	}
	// :342-343 the latest finalized draft of the snapshot's chapter.
	var latest *finalizedCharacterLatest
	if snapshot != nil {
		latest, err = finalizedCharacterLatestFinalized(ctx, q, snapshot.Identity.ChapterNumber)
		if err != nil {
			return nil, err
		}
	}
	// :344 const staleFromChapter = … continuity_projection_meta.stale_from_chapter
	stale, err := finalizedCharacterStateStaleFrom(ctx, q)
	if err != nil {
		return nil, err
	}
	// :345 !row
	if len(rows) == 0 {
		return nil, ErrFinalizedCharacterStateSourceChanged
	}
	entry := rows[0]
	id, idOK := rrInteger(entry["id"], 0)
	draft, draftOK := rrInteger(entry["draftId"], 0)
	chapter, chapterOK := rrInteger(entry["chapterNumber"], 0)
	generation, generationOK := rrInteger(entry["projectionGeneration"], 0)
	finalizationID, finalizationOK := entry["finalizationId"].(string)
	// A column that is not text leaves the empty store, which is what the
	// reference's JSON.parse catch (:200-202) does with an unreadable value.
	candidates := summaryText(entry["candidates"])
	source := FinalizedCharacterSourceIdentity{
		DraftID:        draft,
		FinalizationID: finalizationID,
		ChapterNumber:  chapter,
		ContentHash:    summaryText(entry["contentHash"]),
	}
	// :345-346 `!snapshot || !source || !sameSource(source, snapshot.source)`: the
	// four identity fields must equal the frozen receipt's, with the reference's
	// strict equality (a text column can never match a number).
	if snapshot == nil || !draftOK || !chapterOK || !finalizationOK ||
		source != snapshot.Identity {
		return nil, ErrFinalizedCharacterStateSourceChanged
	}
	// :346 row.projectionGeneration > snapshot.projectionGeneration
	if !generationOK || generation > snapshot.ProjectionGeneration {
		return nil, ErrFinalizedCharacterStateSourceChanged
	}
	// :347 row.projectionGeneration < snapshot.projectionGeneration && staleFromChapter !== null
	//   && row.chapterNumber >= staleFromChapter
	// (&& binds tighter than ||.) A NULL watermark and a missing 'main' row both
	// fail this conjunction, which is why finalizedCharacterStateStaleFrom answers
	// nil for each.
	if generation < snapshot.ProjectionGeneration && stale != nil && chapter >= *stale {
		return nil, ErrFinalizedCharacterStateSourceChanged
	}
	// :348 latest !== draftId
	if latest == nil || latest.DraftID != draftID {
		return nil, ErrFinalizedCharacterStateSourceChanged
	}
	// The row id addresses the store UPDATE (:565). It cannot be missing on a row
	// that just matched, but a decision that cannot name its row refuses instead of
	// returning a success that wrote nothing.
	if !idOK {
		return nil, ErrFinalizedCharacterStateSourceChanged
	}
	return &finalizedCharacterStateRow{ID: id, Candidates: candidates, Source: source}, nil
}

// finalizedCharacterStateStaleFrom is the :344 pluck of
// continuity_projection_meta.stale_from_chapter. nil is "no watermark": a NULL
// column and an absent 'main' row both fail the :347 conjunction.
func finalizedCharacterStateStaleFrom(ctx context.Context, q rrQueryer) (*int64, error) {
	rows, err := rrMaps(ctx, q, "SELECT stale_from_chapter AS staleFromChapter FROM continuity_projection_meta WHERE id='main'")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0]["staleFromChapter"] == nil {
		return nil, nil
	}
	value, ok := rrInteger(rows[0]["staleFromChapter"], 0)
	if !ok {
		return nil, nil
	}
	return &value, nil
}

// finalizedCharacterStatePendingCandidate is pendingCharacterStateCandidate
// (:354-375): the candidate a caller may act on, or nil when it is not one. It is
// the only place a legacy locator is upgraded into the bound shape.
func finalizedCharacterStatePendingCandidate(ctx context.Context, q rrQueryer, row *finalizedCharacterStateRow, candidate finalizedCharacterStateStoredCandidate) (*FinalizedCharacterStateCandidate, error) {
	// :356-357 the row's own source, rebuilt rather than stored twice.
	source := row.Source
	// :358 `if (!candidate.characterId) return null` — a locator with no character
	// id can never be bound to a frozen identity.
	if candidate.characterID == nil || *candidate.characterID == "" {
		return nil, nil
	}
	characterID := *candidate.characterID
	// :359
	expectedKey, err := finalizedCharacterStateCandidateKey(source, characterID, candidate.field, candidate.value)
	if err != nil {
		return nil, err
	}
	// :360-361 `candidate.candidateKey !== undefined || candidate.source !== undefined || …`
	// parseCharacterStateCandidates has already dropped a partially bound candidate
	// (:148), so this is all-four-or-none.
	hasBoundMetadata := candidate.candidateKey != nil || candidate.source != nil ||
		candidate.expectedFieldRevision != nil || candidate.expectedFieldValueHash != nil
	// :362-365 a bound candidate must still describe this exact source, this exact
	// field value and this exact derivation; otherwise it is not pending at all.
	if hasBoundMetadata && (candidate.candidateKey == nil || *candidate.candidateKey == "" ||
		candidate.source == nil || !finalizedCharacterSourceMatches(candidate.source, source) ||
		candidate.expectedFieldRevision == nil || *candidate.expectedFieldRevision < 0 ||
		candidate.expectedFieldValueHash == nil || !summaryHashValid(*candidate.expectedFieldValueHash) ||
		*candidate.candidateKey != expectedKey) {
		return nil, nil
	}
	// :366-368 the frozen identity must still name this character, under the
	// display name the candidate was captured with.
	frozen, err := finalizedCharacterReadFrozen(ctx, q, source)
	if err != nil {
		return nil, err
	}
	if frozen == nil {
		return nil, nil
	}
	named := false
	for _, character := range frozen.Characters {
		if character.CharacterID == characterID && character.DisplayNameSnapshot == candidate.characterName {
			named = true
			break
		}
	}
	if !named {
		return nil, nil
	}
	// :369 a bound candidate is handed back verbatim.
	if hasBoundMetadata {
		return finalizedCharacterStateCandidateValue(candidate, source), nil
	}
	// :370-371 the legacy upgrade needs the live row; a retired one is not pending.
	character, err := finalizedCharacterActiveRow(ctx, q, characterID)
	if err != nil {
		return nil, err
	}
	if character == nil {
		return nil, nil
	}
	column, ok := finalizedCharacterStateColumn(candidate.field)
	if !ok {
		// Unreachable, as in finalizedCharacterStateApplyAccept.
		return nil, nil
	}
	// :372 const current = stateSnapshot(character, candidate.field, { projectId: 'candidate-upgrade', … })
	current, err := finalizedCharacterStateSnapshot(character, candidate.field, column,
		FinalizedCharacterScope{ProjectID: finalizedCharacterStateUpgradeScope, Epoch: finalizedCharacterStateUpgradeScope},
		characterID)
	if err != nil {
		return nil, err
	}
	// :373-374 the upgrade carries the derived key and the expectation of the live
	// field, which is what makes a later decide safe.
	pending := finalizedCharacterStateCandidateValue(candidate, source)
	pending.CandidateKey = expectedKey
	pending.ExpectedFieldRevision = current.Revision
	pending.ExpectedFieldValueHash = current.ValueHash
	return pending, nil
}

// finalizedCharacterStateCandidateValue flattens one stored candidate into the
// shape a caller acts on. source is the row's finalized identity: on the bound
// path the reference clones the candidate's own source object, which
// finalizedCharacterSourceMatches has just proved equal field by field, and every
// writer in this repo stores exactly those four fields in this order.
func finalizedCharacterStateCandidateValue(stored finalizedCharacterStateStoredCandidate, source FinalizedCharacterSourceIdentity) *FinalizedCharacterStateCandidate {
	candidate := &FinalizedCharacterStateCandidate{
		CharacterName: stored.characterName,
		Field:         stored.field,
		Value:         stored.value,
		Evidence:      stored.evidence,
		SelectionKey:  stored.selectionKey,
		DisplayName:   stored.displayName,
		Reason:        stored.reason,
		Source:        source,
	}
	if stored.characterID != nil {
		candidate.CharacterID = *stored.characterID
	}
	if stored.candidateKey != nil {
		candidate.CandidateKey = *stored.candidateKey
	}
	if stored.expectedFieldRevision != nil {
		candidate.ExpectedFieldRevision = *stored.expectedFieldRevision
	}
	if stored.expectedFieldValueHash != nil {
		candidate.ExpectedFieldValueHash = *stored.expectedFieldValueHash
	}
	if stored.rawValuePresent {
		raw := stored.rawValue
		candidate.RawValue = &raw
	}
	return candidate
}

// finalizedCharacterStateColumn is STATE_COLUMNS (summary-repository.ts:38-41),
// read here out of the store's own authoritative pair
// (characters.go:31-32) so the field list and its columns can never drift apart.
func finalizedCharacterStateColumn(field string) (string, bool) {
	for index, name := range characterStateFields {
		if name == field {
			return characterStateColumns[index], true
		}
	}
	return "", false
}

// finalizedCharacterStateParseStore is parseCharacterStateCandidateStore
// (:179-203). Parsing never refuses: the reference's catch (:200-202) turns an
// unreadable store into the empty version-2 store, so a corrupt row is preserved
// untouched rather than reinterpreted, and a legacy top-level array (:182) is the
// pending list with no receipts.
func finalizedCharacterStateParseStore(text string) finalizedCharacterStateStore {
	parsed, ok := summaryParse(text)
	if !ok {
		return finalizedCharacterStateStore{}
	}
	if _, legacy := parsed.([]any); legacy {
		return finalizedCharacterStateStore{pending: finalizedCharacterStateParseCandidates(parsed)}
	}
	object, ok := parsed.(map[string]any)
	if !ok || !summaryNumberEqual(object["version"], int64(2)) {
		return finalizedCharacterStateStore{}
	}
	// :184-185 both arrays must be present; :186 re-parses the pending list from
	// the same text, which is the same value as object["pending"].
	if _, ok := object["pending"].([]any); !ok {
		return finalizedCharacterStateStore{}
	}
	receipts, ok := object["receipts"].([]any)
	if !ok {
		return finalizedCharacterStateStore{}
	}
	store := finalizedCharacterStateStore{pending: finalizedCharacterStateParseCandidates(object["pending"])}
	for _, value := range receipts {
		stored, ok := finalizedCharacterStateParseReceipt(value)
		if !ok {
			continue
		}
		store.receipts = append(store.receipts, stored)
	}
	return store
}

// finalizedCharacterStateParseCandidates is parseCharacterStateCandidates
// (:132-174): the normalization every read and every write-back goes through.
func finalizedCharacterStateParseCandidates(parsed any) []finalizedCharacterStateStoredCandidate {
	items, ok := parsed.([]any)
	if !ok {
		return nil
	}
	result := make([]finalizedCharacterStateStoredCandidate, 0, len(items))
	for _, input := range items {
		candidate, ok := input.(map[string]any)
		if !ok {
			continue
		}
		// :142-143 every present bound key, valid or not.
		bound := false
		for _, key := range []string{"candidateKey", "source", "expectedFieldRevision", "expectedFieldValueHash"} {
			if _, present := candidate[key]; present {
				bound = true
				break
			}
		}
		// :144-147 all four, valid. A candidate with only some of them is dropped
		// (:148) and never reinterpreted as a legacy locator.
		keyText, keyIsText := candidate["candidateKey"].(string)
		_, revisionOK := rrInteger(candidate["expectedFieldRevision"], 0)
		validBound := keyIsText && keyText != "" && summarySource(candidate["source"]) && revisionOK &&
			summaryHashValid(candidate["expectedFieldValueHash"])
		if bound && !validBound {
			continue
		}
		// :149-152 a named candidate of one of the six state fields.
		characterName, nameOK := candidate["characterName"].(string)
		field, fieldOK := candidate["field"].(string)
		value, valueOK := candidate["value"].(string)
		if !nameOK || summaryTrim(characterName) == "" || !fieldOK || !summaryStateField(field) || !valueOK {
			continue
		}
		stored := finalizedCharacterStateStoredCandidate{characterName: summaryTrim(characterName), field: field, value: value}
		// :155-157 an unbound value is trimmed; a source-bound one is kept verbatim,
		// because that is the prose the candidate was derived from.
		if !summarySource(candidate["source"]) {
			stored.value = summaryTrim(value)
		}
		// :154 `typeof candidate.characterId === 'string' && candidate.characterId`
		if id, ok := candidate["characterId"].(string); ok && id != "" {
			stored.characterID = &id
		}
		// :158 `candidate.evidence ? { evidence: … } : {}` — a JavaScript truthiness
		// test, so an empty object is kept and an empty string is dropped.
		if evidence, present := candidate["evidence"]; present && phTruthy(evidence) {
			stored.evidence = evidence
		}
		if text, ok := candidate["selectionKey"].(string); ok {
			stored.selectionKey = &text
		}
		if text, ok := candidate["displayName"].(string); ok {
			stored.displayName = &text
		}
		// :161 `candidate.rawValue !== undefined` — a stored JSON null is a value.
		if raw, present := candidate["rawValue"]; present {
			stored.rawValue = raw
			stored.rawValuePresent = true
		}
		// :162 only the two reasons the decided-candidate path understands.
		if text, ok := candidate["reason"].(string); ok &&
			(text == "author-protected" || text == "legacy-protected") {
			stored.reason = &text
		}
		if text, ok := candidate["candidateKey"].(string); ok {
			stored.candidateKey = &text
		}
		// :164 `isFinalizedSource(candidate.source) ? { source: … } : {}`
		if summarySource(candidate["source"]) {
			source, _ := candidate["source"].(map[string]any)
			stored.source = source
		}
		if revision, ok := rrInteger(candidate["expectedFieldRevision"], 0); ok {
			stored.expectedFieldRevision = &revision
		}
		if text, ok := candidate["expectedFieldValueHash"].(string); ok {
			stored.expectedFieldValueHash = &text
		}
		result = append(result, stored)
	}
	return result
}

// finalizedCharacterStateParseReceipt is the :187-198 receipt filter.
func finalizedCharacterStateParseReceipt(value any) (finalizedCharacterStateStoredReceipt, bool) {
	receipt, ok := value.(map[string]any)
	if !ok {
		return finalizedCharacterStateStoredReceipt{}, false
	}
	key, keyOK := receipt["candidateKey"].(string)
	operation, operationOK := receipt["operationId"].(string)
	hash, hashOK := receipt["payloadHash"].(string)
	decision, decisionOK := receipt["decision"].(string)
	if !keyOK || key == "" || !operationOK || operation == "" || !hashOK || !summaryHashValid(hash) ||
		!decisionOK ||
		(decision != FinalizedCharacterStateDecisionAccept && decision != FinalizedCharacterStateDecisionDecline) ||
		!summarySource(receipt["source"]) {
		return finalizedCharacterStateStoredReceipt{}, false
	}
	source, _ := receipt["source"].(map[string]any)
	return finalizedCharacterStateStoredReceipt{
		candidateKey: key,
		operationID:  operation,
		payloadHash:  hash,
		decision:     decision,
		source:       source,
	}, true
}

// finalizedCharacterStateSourceObject is a stored source as ordered JSON: the four
// FinalizedSourceIdentity fields in their reference order (:12-17). The reference
// clones the stored object, extra keys and all; no writer in this repo stores any
// (finalization_outbox.go:333-341 and commitFinalizedCharacterStates :457 both
// build the object from that same identity), so the canonical order is the stored
// order.
func finalizedCharacterStateSourceObject(source FinalizedCharacterSourceIdentity) plotObject {
	return plotObject{
		{"draftId", source.DraftID},
		{"finalizationId", source.FinalizationID},
		{"chapterNumber", source.ChapterNumber},
		{"contentHash", source.ContentHash},
	}
}

// finalizedCharacterStateSourceRecord is the same source as the map the store keeps
// in memory (a parsed JSON object, so numbers are already JavaScript numbers).
func finalizedCharacterStateSourceRecord(source FinalizedCharacterSourceIdentity) map[string]any {
	return map[string]any{
		"draftId":        source.DraftID,
		"finalizationId": source.FinalizationID,
		"chapterNumber":  source.ChapterNumber,
		"contentHash":    source.ContentHash,
	}
}

// finalizedCharacterStateSourceFromMap reads the four identity fields back out of a
// stored source object that summarySource has already validated.
func finalizedCharacterStateSourceFromMap(source map[string]any) FinalizedCharacterSourceIdentity {
	draftID, _ := rrInteger(source["draftId"], 0)
	chapter, _ := rrInteger(source["chapterNumber"], 0)
	return FinalizedCharacterSourceIdentity{
		DraftID:        draftID,
		FinalizationID: summaryText(source["finalizationId"]),
		ChapterNumber:  chapter,
		ContentHash:    summaryText(source["contentHash"]),
	}
}

// finalizedCharacterStateStoreText is JSON.stringify(store) (:510, :565):
// `{"version":2,"pending":[…],"receipts":[…]}` in that key order, with each entry
// in the key order parseCharacterStateCandidates and the receipt filter build.
func finalizedCharacterStateStoreText(store finalizedCharacterStateStore) (string, error) {
	pending := make([]any, 0, len(store.pending))
	for _, candidate := range store.pending {
		pending = append(pending, finalizedCharacterStateCandidateObject(candidate))
	}
	receipts := make([]any, 0, len(store.receipts))
	for _, receipt := range store.receipts {
		receipts = append(receipts, finalizedCharacterStateReceiptObject(receipt))
	}
	return plotJSON(plotObject{{"version", int64(2)}, {"pending", pending}, {"receipts", receipts}})
}

// finalizedCharacterStateCandidateObject is one candidate in the :153-168 literal
// key order. A key is emitted only in the arm that produced it, exactly like the
// reference's conditional spreads.
func finalizedCharacterStateCandidateObject(candidate finalizedCharacterStateStoredCandidate) plotObject {
	object := plotObject{}
	if candidate.characterID != nil {
		object = append(object, plotField{"characterId", *candidate.characterID})
	}
	object = append(object, plotField{"characterName", candidate.characterName},
		plotField{"field", candidate.field}, plotField{"value", candidate.value})
	if candidate.evidence != nil {
		object = append(object, plotField{"evidence", candidate.evidence})
	}
	if candidate.selectionKey != nil {
		object = append(object, plotField{"selectionKey", *candidate.selectionKey})
	}
	if candidate.displayName != nil {
		object = append(object, plotField{"displayName", *candidate.displayName})
	}
	if candidate.rawValuePresent {
		object = append(object, plotField{"rawValue", candidate.rawValue})
	}
	if candidate.reason != nil {
		object = append(object, plotField{"reason", *candidate.reason})
	}
	if candidate.candidateKey != nil {
		object = append(object, plotField{"candidateKey", *candidate.candidateKey})
	}
	if candidate.source != nil {
		object = append(object, plotField{"source", finalizedCharacterStateSourceObject(finalizedCharacterStateSourceFromMap(candidate.source))})
	}
	if candidate.expectedFieldRevision != nil {
		object = append(object, plotField{"expectedFieldRevision", *candidate.expectedFieldRevision})
	}
	if candidate.expectedFieldValueHash != nil {
		object = append(object, plotField{"expectedFieldValueHash", *candidate.expectedFieldValueHash})
	}
	return object
}

// finalizedCharacterStateReceiptObject is one receipt in the :187-196 literal key
// order.
func finalizedCharacterStateReceiptObject(receipt finalizedCharacterStateStoredReceipt) plotObject {
	return plotObject{
		{"candidateKey", receipt.candidateKey},
		{"operationId", receipt.operationID},
		{"payloadHash", receipt.payloadHash},
		{"decision", receipt.decision},
		{"source", finalizedCharacterStateSourceObject(finalizedCharacterStateSourceFromMap(receipt.source))},
	}
}
