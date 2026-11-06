package store

// Native port of the last store gap of the finalized-character family:
//
//	SummaryRepository.commitFinalizedCharacterStates        (electron/repositories/summary-repository.ts:423-475)
//	SummaryRepository.saveFinalizedCharacterStateCandidates (:649-704)
//	normalizeCharacterStateCandidates                       (:214-258)
//	decideDerivedPatch                                      (src/shared/character-identity.ts:67-94)
//	advancesProjectContinuity                               (src/shared/character-identity.ts:57-64)
//
// It is the write half of the family whose read (readFinalizedCharacterContext,
// :401-424) is finalized_character_context.go and whose candidate store, decision
// arms and projection refresh are character_state_candidates.go. Everything this
// file shares with those two - FinalizedCharacters, the scope type, the field
// snapshot reader (finalizedCharacterStateSnapshot), the candidate key
// (finalizedCharacterStateCandidateKey), the store parser and its serializer, the
// ordered provenance decoder and the projection refresh
// (finalizedCharacterStateRefreshProjection) - is reused as it is; nothing here
// declares a second copy of any of them.
//
// The commit answers with FinalizedCharacterStateCommitReceipt (:163-167). Its
// three outcomes are counted separately and must stay separate: a field is
// applied (a revision bump and a projection invalidation), staged as a candidate
// (the field is the author's own and is never overwritten), or unchanged (the
// answer repeats what is already stored, which writes nothing at all - collapsing
// it into "applied" would bump a revision and invalidate a candidate for no
// reason).
//
// # The transaction
//
// One BEGIN IMMEDIATE for the whole commit (:428-475), borrowed from
// ReviewRevisions.immediate exactly as the decide operation does
// (character_state_candidates.go:391-398). The reference opens the transaction and
// reads the frozen context *inside* it; the port keeps that order, so a commit can
// never stage candidates or rewrite fields against a source the author replaced
// after the frozen context was handed out. The write sequence is the reference's:
// fields first, then cs_provenance per character, then the candidate store
// (:471-472), then the projection refresh (:473).
//
// # What is not ported
//
// commitFinalizedCharacterStates writes no knowledge-base document and publishes
// no manuscript, so this port omits no arm of it. The reference's outer
// db.transaction(...).immediate() (:428, :475) is this method's own transaction.
// The reference borrows its database handle; this port borrows the admitted
// project database FinalizedCharacters already holds, opens no schema of its own
// and issues no DDL.

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/characterstate"
)

var (
	// summary-repository.ts:433-434
	//   if (!isDeepStrictEqual(identity(current), identity(context)) || context.characters.some(...))
	//     throw new Error('FINALIZED_CHARACTER_CONTEXT_CHANGED')
	// summary-repository.ts:447
	//   if (!base || base.valueHash !== sha256(base.value)) throw new Error('FINALIZED_CHARACTER_CONTEXT_CHANGED')
	ErrFinalizedCharacterContextChanged = errors.New("FINALIZED_CHARACTER_CONTEXT_CHANGED")
	// summary-repository.ts:436
	//   if (verified.unresolved.length) throw new Error('FINALIZED_CHARACTER_IDENTITY_REQUIRED')
	// summary-repository.ts:227
	//   ... throw new Error('FINALIZED_CHARACTER_IDENTITY_REQUIRED')
	ErrFinalizedCharacterIdentityRequired = errors.New("FINALIZED_CHARACTER_IDENTITY_REQUIRED")
	// summary-repository.ts:452
	//   if (decision === 'source-conflict') throw new Error('FINALIZED_CHARACTER_SOURCE_CONFLICT')
	ErrFinalizedCharacterSourceConflict = errors.New("FINALIZED_CHARACTER_SOURCE_CONFLICT")
	// summary-repository.ts:453
	//   if (decision === 'field-conflict') throw new Error('FINALIZED_CHARACTER_FIELD_CONFLICT')
	ErrFinalizedCharacterFieldConflict = errors.New("FINALIZED_CHARACTER_FIELD_CONFLICT")
	// summary-repository.ts:222
	//   if (!frozen) throw new Error('FINALIZED_CHARACTER_SNAPSHOT_REQUIRED')
	ErrFinalizedCharacterSnapshotRequired = errors.New("FINALIZED_CHARACTER_SNAPSHOT_REQUIRED")
	// summary-repository.ts:232
	//   ... throw new Error('FINALIZED_CHARACTER_EVIDENCE_INVALID')
	ErrFinalizedCharacterEvidenceInvalid = errors.New("FINALIZED_CHARACTER_EVIDENCE_INVALID")
	// summary-repository.ts:219, :245, :249
	//   throw new Error('CONTINUITY_CHARACTER_STATE_CANDIDATES_INVALID')
	// summary-repository.ts:658
	//   ) throw new Error('CONTINUITY_CHARACTER_STATE_CANDIDATES_INVALID')
	ErrContinuityCharacterStateCandidatesInvalid = errors.New("CONTINUITY_CHARACTER_STATE_CANDIDATES_INVALID")
	// summary-repository.ts:663
	//   throw new Error('CONTINUITY_SOURCE_STALE')
	ErrContinuitySourceStale = errors.New("CONTINUITY_SOURCE_STALE")
	// summary-repository.ts:666
	//   throw new Error('CONTINUITY_GENERATION_STALE')
	ErrContinuityGenerationStale = errors.New("CONTINUITY_GENERATION_STALE")
	// summary-repository.ts:686
	//   ) throw new Error('CONTINUITY_PROJECTION_MISSING')
	ErrContinuityProjectionMissing = errors.New("CONTINUITY_PROJECTION_MISSING")
	// summary-repository.ts:240
	//   if (!identity || byIdentity.has(identity)) throw new Error('CONTINUITY_ROSTER_NAME_CONFLICT')
	ErrContinuityRosterNameConflict = errors.New("CONTINUITY_ROSTER_NAME_CONFLICT")
)

// The reference's PROJECT_NOT_OPEN arms stay the package's existing value:
// summary-repository.ts:426 and :650 throw exactly the string ErrProjectNotOpen
// carries (projectcore.go:12), and draft_commit_tx.go:159 borrows the same value
// for the same reference code, so there is one symbolic code per literal and not
// a fourth declaration of it.

// The five decisions of decideDerivedPatch (character-identity.ts:65).
const (
	finalizedCharacterDecisionApply            = "apply-derived"
	finalizedCharacterDecisionAlreadyApplied   = "already-applied"
	finalizedCharacterDecisionProposalRequired = "proposal-required"
	finalizedCharacterDecisionSourceConflict   = "source-conflict"
	finalizedCharacterDecisionFieldConflict    = "field-conflict"
)

// finalizedCharacterCommitCandidateLimit is the :219 cap on a candidate list
// (`value.length > 1_000`). 1,000 candidates are accepted, 1,001 are refused.
const finalizedCharacterCommitCandidateLimit = 1000

// FinalizedCharacterStateCommitReceipt is FinalizedCharacterStateCommitReceipt
// (src/shared/finalized-continuity.ts:163-167) in the reference's key order.
// Candidates is always a list (:437), so an empty commit re-encodes as [] rather
// than null.
type FinalizedCharacterStateCommitReceipt struct {
	Applied    int64                              `json:"applied"`
	Unchanged  int64                              `json:"unchanged"`
	Candidates []FinalizedCharacterStateCandidate `json:"candidates"`
}

// finalizedCharacterDerivedPatch is DerivedCharacterPatch
// (character-identity.ts:39-44). It is built by the commit for one field of one
// update; nothing else in this package builds one.
type finalizedCharacterDerivedPatch struct {
	ProjectID         string
	Epoch             string
	CharacterID       string
	Field             string
	Value             string
	ValueHash         string
	BaseFieldRevision int64
	BaseValueHash     string
	BaseProvenance    map[string]any
	Source            FinalizedCharacterSourceIdentity
	SourceOrder       FinalizedCharacterSourceOrder
}

// finalizedCharacterDerivedAuthority is the authority argument of
// decideDerivedPatch (character-identity.ts:67-68): the *live* source and order,
// which the commit read in its own transaction, plus the optional transfer origin
// (character-identity.ts:53-56). An empty OriginProjectID is the reference's
// absent optional argument.
type finalizedCharacterDerivedAuthority struct {
	Source          FinalizedCharacterSourceIdentity
	Order           FinalizedCharacterSourceOrder
	OriginProjectID string
}

// finalizedCharacterStateCandidateInput is SaveFinalizedCharacterStateCandidatesRequest
// (:649-658) as the commit builds it at :471-472.
type finalizedCharacterStateCandidateInput struct {
	DraftID              int64
	ChapterNumber        int64
	ProjectionGeneration int64
	Source               FinalizedCharacterSourceIdentity
	Candidates           []finalizedCharacterStateStoredCandidate
}

// CommitFinalizedCharacterStates is
// SummaryRepository.commitFinalizedCharacterStates (:423-475).
//
// frozen is the context the caller was handed by ReadContext (or the equivalent
// answer of the read operation); it is never trusted: the commit re-reads the
// same context inside its transaction and refuses any difference (:429-434).
// response is the validated model answer produced by
// characterstate.ParseResponse, and is re-parsed against the context exactly as
// the reference re-parses JSON.stringify({ updates }) at :435. originProjectID is
// the optional transfer origin of :424-425; an empty string is the reference's
// absent argument.
func (f *FinalizedCharacters) CommitFinalizedCharacterStates(ctx context.Context, frozen FinalizedCharacterContext,
	response characterstate.Response, originProjectID string) (*FinalizedCharacterStateCommitReceipt, error) {
	// :426 if (!database) throw new Error('PROJECT_NOT_OPEN')
	if f == nil || f.db == nil {
		return nil, ErrProjectNotOpen
	}
	var receipt *FinalizedCharacterStateCommitReceipt
	err := NewReviewRevisions(f.db).immediate(ctx, func(conn *sql.Conn) error {
		result, err := finalizedCharacterCommitBody(ctx, conn, frozen, response, originProjectID)
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

// finalizedCharacterCommitBody is the single transaction body of :429-474.
func finalizedCharacterCommitBody(ctx context.Context, conn *sql.Conn, frozen FinalizedCharacterContext,
	response characterstate.Response, originProjectID string) (*FinalizedCharacterStateCommitReceipt, error) {
	// :429 const current = readFinalizedCharacterContext(context.source.draftId, context, db)
	// The re-read is the first statement of the transaction, so every write below
	// is ordered after the source it was proved against.
	current, err := finalizedCharacterCommitContext(ctx, conn, frozen.Source.DraftID,
		FinalizedCharacterScope{ProjectID: frozen.ProjectID, Epoch: frozen.Epoch})
	if err != nil {
		return nil, err
	}
	// :430-434 the frozen identity must still be exactly what the caller holds,
	// and every one of the caller's field snapshots must carry the caller's own
	// scope.
	if finalizedCharacterCommitIdentityChanged(frozen, *current) {
		return nil, ErrFinalizedCharacterContextChanged
	}
	// :435 const verified = parseFinalizedCharacterStateResponse(JSON.stringify({ updates: response.updates }), context)
	verified, err := finalizedCharacterCommitVerify(frozen, response)
	if err != nil {
		return nil, err
	}
	// :436 if (verified.unresolved.length) throw new Error('FINALIZED_CHARACTER_IDENTITY_REQUIRED')
	if len(verified.Unresolved) > 0 {
		return nil, ErrFinalizedCharacterIdentityRequired
	}
	// :437 const receipt = { applied: 0, unchanged: 0, candidates: [] }
	receipt := &FinalizedCharacterStateCommitReceipt{Candidates: []FinalizedCharacterStateCandidate{}}
	staged := make([]finalizedCharacterStateStoredCandidate, 0, len(verified.Updates))
	// :438 for (const update of verified.updates) {
	for _, update := range verified.Updates {
		// :439-440 captured/currentCharacter. Both lookups are guaranteed: the parse
		// resolved the id against `frozen`, and :433 just proved `current` carries
		// the same character list. A miss is a context that no longer agrees with
		// itself, which is the :434 refusal.
		captured, capturedOK := finalizedCharacterCommitCharacter(frozen.Characters, update.CharacterID)
		actualCharacter, actualOK := finalizedCharacterCommitCharacter(current.Characters, update.CharacterID)
		if !capturedOK || !actualOK {
			return nil, ErrFinalizedCharacterContextChanged
		}
		// :441 SELECT cs_provenance FROM characters WHERE character_id=? AND retired=0
		// A missing row is unreachable: :413 refused the whole read for a retired
		// frozen character, and the identity check above ties `current` to `frozen`.
		// The reference would dereference an undefined row here (a TypeError with no
		// code); this port answers the code that same read uses for the condition.
		rows, err := rrMaps(ctx, conn, "SELECT cs_provenance FROM characters WHERE character_id=? AND retired=0", update.CharacterID)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, ErrFinalizedCharacterIDRetired
		}
		// :442 JSON.parse(row.cs_provenance || '{}'). The ordered decoder is
		// mandatory: the merge below re-serializes every untouched field, and their
		// stored bytes are part of the roster fact hash
		// (character_state_candidates.go:566-573 takes the same care).
		//
		// Divergence: a stored provenance that is not JSON is `null` here, so the
		// write below starts from an empty object, where the reference's bare
		// JSON.parse throws a SyntaxError with no code. The store's readers answer "no
		// stored observations" for the same text (characters.go:146-148), and this
		// port keeps that convention rather than inventing a refusal code for it.
		provenance, err := rosterProvenance(rows[0]["cs_provenance"])
		if err != nil {
			return nil, err
		}
		changed := false
		// :444 for (const field of CHARACTER_STATE_TEXT_FIELDS) {
		for index, field := range characterStateFields {
			// :445 if (!Object.hasOwn(update.currentState, field)) continue
			value, present := update.CurrentState.Get(field)
			if !present {
				continue
			}
			// :446 const base = captured.fields.find(...), actual = currentCharacter.fields.find(...)!
			base, baseOK := finalizedCharacterCommitField(captured.Fields, field)
			// :447 if (!base || base.valueHash !== sha256(base.value)) throw new Error('FINALIZED_CHARACTER_CONTEXT_CHANGED')
			if !baseOK || base.ValueHash != summaryHash(base.Value) {
				return nil, ErrFinalizedCharacterContextChanged
			}
			actual, actualOK := finalizedCharacterCommitField(actualCharacter.Fields, field)
			if !actualOK {
				// Unreachable for the same reason as :439-440: every frozen character
				// carries all six snapshots (:414).
				return nil, ErrFinalizedCharacterContextChanged
			}
			// :449-451 decideDerivedPatch(actual, {...}, { source: current.source, order: current.sourceOrder, originProjectId })
			patch := finalizedCharacterDerivedPatch{
				ProjectID:         frozen.ProjectID,
				Epoch:             frozen.Epoch,
				CharacterID:       update.CharacterID,
				Field:             field,
				Value:             value,
				ValueHash:         summaryHash(value),
				BaseFieldRevision: base.Revision,
				BaseValueHash:     base.ValueHash,
				BaseProvenance:    base.Provenance,
				Source:            frozen.Source,
				SourceOrder:       frozen.SourceOrder,
			}
			authority := finalizedCharacterDerivedAuthority{
				Source:          current.Source,
				Order:           current.SourceOrder,
				OriginProjectID: originProjectID,
			}
			decision := finalizedCharacterDecideDerivedPatch(actual, patch, authority)
			// :452-453
			switch decision {
			case finalizedCharacterDecisionSourceConflict:
				return nil, ErrFinalizedCharacterSourceConflict
			case finalizedCharacterDecisionFieldConflict:
				return nil, ErrFinalizedCharacterFieldConflict
			case finalizedCharacterDecisionAlreadyApplied:
				// :454 `{ receipt.unchanged++; continue }` - nothing is written.
				receipt.Unchanged++
				continue
			case finalizedCharacterDecisionProposalRequired:
				// :456 `if (value === actual.value) { receipt.unchanged++; continue }`
				// The author's field already says what the model says it should say,
				// so there is no proposal to stage and nothing to write.
				if value == actual.Value {
					receipt.Unchanged++
					continue
				}
				// :457-461 the candidate, in the reference's own key order. The key is
				// the sibling's derivation (character_state_candidates.go:646-663), so a
				// later read and a later decide can address this row; the expectation is
				// the *live* field, which is what makes a decide safe.
				key, err := finalizedCharacterStateCandidateKey(frozen.Source, update.CharacterID, field, value)
				if err != nil {
					return nil, err
				}
				reason := "legacy-protected"
				if kind, _ := actual.Provenance["kind"].(string); kind == "author" {
					reason = "author-protected"
				}
				candidate := finalizedCharacterStateStoredCandidate{
					characterID:            &update.CharacterID,
					characterName:          captured.DisplayNameSnapshot,
					field:                  field,
					value:                  value,
					evidence:               finalizedCharacterCommitEvidence(update.Evidence),
					selectionKey:           finalizedCharacterCommitPointer("state:" + update.CharacterID + ":" + field),
					displayName:            finalizedCharacterCommitPointer(captured.DisplayNameSnapshot),
					rawValue:               finalizedCharacterCommitUpdateValue(update),
					rawValuePresent:        true,
					reason:                 &reason,
					candidateKey:           &key,
					source:                 finalizedCharacterStateSourceRecord(frozen.Source),
					expectedFieldRevision:  &actual.Revision,
					expectedFieldValueHash: &actual.ValueHash,
				}
				staged = append(staged, candidate)
				receipt.Candidates = append(receipt.Candidates, *finalizedCharacterStateCandidateValue(candidate, frozen.Source))
				continue
			}
			// :464 UPDATE characters SET <STATE_COLUMNS[field]>=? WHERE character_id=? AND retired=0
			// The reference does not inspect changes here; the field row was proved
			// active by the read, so a zero-row update cannot happen.
			if _, err := conn.ExecContext(ctx,
				"UPDATE characters SET "+characterStateColumns[index]+"=? WHERE character_id=? AND retired=0",
				value, update.CharacterID); err != nil {
				return nil, err
			}
			// :465 provenance[field] = { kind: 'derived', source, revision: actual.revision + 1, sourceOrder }
			provenance = finalizedCharacterCommitProvenance(provenance, field, plotObject{
				{"kind", "derived"},
				{"source", finalizedCharacterStateSourceObject(frozen.Source)},
				{"revision", actual.Revision + 1},
				{"sourceOrder", finalizedCharacterCommitSourceOrder(frozen.SourceOrder)},
			})
			// :466 receipt.applied++; changed = true
			receipt.Applied++
			changed = true
		}
		// :468-469
		//   UPDATE characters SET cs_provenance=?,cs_updated_at_chapter=MAX(COALESCE(cs_updated_at_chapter,0),?),
		//     updated_at=datetime('now') WHERE character_id=?
		if changed {
			text, err := plotJSON(provenance)
			if err != nil {
				return nil, err
			}
			if _, err := conn.ExecContext(ctx, "UPDATE characters SET cs_provenance=?,"+
				"cs_updated_at_chapter=MAX(COALESCE(cs_updated_at_chapter,0),?),updated_at=datetime('now') "+
				"WHERE character_id=?", text, frozen.Source.ChapterNumber, update.CharacterID); err != nil {
				return nil, err
			}
		}
	}
	// :471-472 if (receipt.candidates.length) saveFinalizedCharacterStateCandidates({...}, db)
	// The reference's nested db.transaction() is a savepoint inside this one; the
	// port keeps its body in the same transaction instead of opening a second one.
	if len(staged) > 0 {
		if err := finalizedCharacterCommitSaveCandidates(ctx, conn, finalizedCharacterStateCandidateInput{
			DraftID:              frozen.Source.DraftID,
			ChapterNumber:        frozen.Source.ChapterNumber,
			ProjectionGeneration: frozen.ProjectionGeneration,
			Source:               frozen.Source,
			Candidates:           staged,
		}); err != nil {
			return nil, err
		}
	}
	// :473 if (receipt.applied) refreshCharacterStateProjection(db)
	if receipt.Applied > 0 {
		if err := finalizedCharacterStateRefreshProjection(ctx, conn); err != nil {
			return nil, err
		}
	}
	// :474 return receipt
	return receipt, nil
}

// finalizedCharacterCommitSaveCandidates is
// SummaryRepository.saveFinalizedCharacterStateCandidates (:649-704) as a
// transaction body.
//
// :650 `if (!db) throw new Error('PROJECT_NOT_OPEN')` has no arm here: the caller
// is already inside the commit's transaction on an admitted database, and the
// commit refuses an absent handle at :426 before opening it.
//
// The merged list is written through the one canonical serializer this package
// owns (character_state_candidates.go:1121-1156), so a staged row and the same row
// read back and re-saved are the same bytes. The reference instead serializes the
// objects it merged (:697 `JSON.stringify(store)`), and a candidate staged by the
// commit keeps the key order of the literal at :457-461 while a re-parsed one keeps
// the order of :153-168. Nothing reads that order - every reader addresses a
// candidate by name - so the port prefers the canonical writer.
func finalizedCharacterCommitSaveCandidates(ctx context.Context, conn *sql.Conn, input finalizedCharacterStateCandidateInput) error {
	// :651-658
	//   !Number.isSafeInteger(input.draftId) || input.draftId < 1
	//   || !Number.isSafeInteger(input.chapterNumber) || input.chapterNumber < 1
	//   || !Number.isSafeInteger(input.projectionGeneration) || input.projectionGeneration < 0
	if input.DraftID < 1 || input.DraftID > maxSafeInteger ||
		input.ChapterNumber < 1 || input.ChapterNumber > maxSafeInteger ||
		input.ProjectionGeneration < 0 || input.ProjectionGeneration > maxSafeInteger {
		return ErrContinuityCharacterStateCandidatesInvalid
	}
	// :661 const snapshot = readFinalizedSourceFromDb(db, input.draftId)
	snapshot, err := finalizedCharacterReadSource(ctx, conn, input.DraftID)
	if err != nil {
		return err
	}
	// :662-664
	//   if (!snapshot || !sameSource(snapshot.source, input.source)
	//     || input.chapterNumber !== snapshot.source.chapterNumber) throw new Error('CONTINUITY_SOURCE_STALE')
	if snapshot == nil || snapshot.Identity != input.Source ||
		input.ChapterNumber != input.Source.ChapterNumber {
		return ErrContinuitySourceStale
	}
	// :665-667 if (input.projectionGeneration !== snapshot.projectionGeneration)
	//   throw new Error('CONTINUITY_GENERATION_STALE')
	if input.ProjectionGeneration != snapshot.ProjectionGeneration {
		return ErrContinuityGenerationStale
	}
	// :668-680 one row, addressed by the partial UNIQUE index on draft_id
	// (baseline_v7_ddl.go:689-691).
	rows, err := rrMaps(ctx, conn, `SELECT character_state_candidates AS candidates,
 source_finalization_id AS finalizationId,source_content_hash AS contentHash,
 projection_generation AS projectionGeneration FROM summary_snapshots WHERE draft_id=?`, input.DraftID)
	if err != nil {
		return err
	}
	// :681-686
	//   !row || row.finalizationId !== input.source.finalizationId
	//   || row.contentHash !== input.source.contentHash || row.projectionGeneration !== input.projectionGeneration
	if len(rows) == 0 {
		return ErrContinuityProjectionMissing
	}
	entry := rows[0]
	finalizationID, finalizationOK := entry["finalizationId"].(string)
	contentHash, contentOK := entry["contentHash"].(string)
	generation, generationOK := rrInteger(entry["projectionGeneration"], 0)
	if !finalizationOK || finalizationID != input.Source.FinalizationID ||
		!contentOK || contentHash != input.Source.ContentHash ||
		!generationOK || generation != input.ProjectionGeneration {
		return ErrContinuityProjectionMissing
	}
	stored := summaryText(entry["candidates"])
	// The store is re-serialized below (:697-702). Text this port cannot re-encode
	// byte for byte (a lone UTF-16 surrogate, a non-finite number) is refused
	// instead of being silently normalized - the boundary
	// character_state_candidates.go:451-458 draws for the same column.
	if err := summaryJSONSupported(stored); err != nil {
		return err
	}
	// :688 const store = parseCharacterStateCandidateStore(row.candidates)
	store := finalizedCharacterStateParseStore(stored)
	// :692 normalizeCharacterStateCandidates(db, input.candidates, input.source)
	normalized, err := finalizedCharacterCommitNormalizeCandidates(ctx, conn, input.Candidates, input.Source)
	if err != nil {
		return err
	}
	// :689-696 the merge. A receipt already answers for a key, so a decided
	// candidate is never re-staged; the map is keyed by the character identity and
	// the field, so a later proposal for the same pair replaces the earlier one in
	// the position it first took (Map.set keeps an existing key's position).
	merged := make([]finalizedCharacterStateStoredCandidate, 0, len(store.pending)+len(normalized))
	positions := make(map[string]int, len(store.pending)+len(normalized))
	for _, candidate := range append(append([]finalizedCharacterStateStoredCandidate{}, store.pending...), normalized...) {
		// :694 `if (candidate.candidateKey && store.receipts.some(...)) continue`
		if candidate.candidateKey != nil && *candidate.candidateKey != "" &&
			finalizedCharacterCommitHasReceipt(store.receipts, *candidate.candidateKey) {
			continue
		}
		key := finalizedCharacterCommitMergeKey(candidate)
		if position, seen := positions[key]; seen {
			merged[position] = candidate
			continue
		}
		positions[key] = len(merged)
		merged = append(merged, candidate)
	}
	// :697 store.pending = [...merged.values()]
	store.pending = merged
	text, err := finalizedCharacterStateStoreText(store)
	if err != nil {
		return err
	}
	// :698-702 UPDATE summary_snapshots SET character_state_candidates=?,created_at=datetime('now') WHERE draft_id=?
	if _, err := conn.ExecContext(ctx,
		"UPDATE summary_snapshots SET character_state_candidates=?,created_at=datetime('now') WHERE draft_id=?",
		text, input.DraftID); err != nil {
		return err
	}
	return nil
}

// finalizedCharacterCommitNormalizeCandidates is normalizeCharacterStateCandidates
// (:214-258).
func finalizedCharacterCommitNormalizeCandidates(ctx context.Context, conn *sql.Conn,
	candidates []finalizedCharacterStateStoredCandidate, source FinalizedCharacterSourceIdentity) ([]finalizedCharacterStateStoredCandidate, error) {
	// :219 if (!Array.isArray(value) || value.length > 1_000)
	if len(candidates) > finalizedCharacterCommitCandidateLimit {
		return nil, ErrContinuityCharacterStateCandidatesInvalid
	}
	// :220 if (hasCharacterIdentitySchema(db)) {
	hasSchema, err := finalizedCharacterHasIdentitySchema(ctx, conn)
	if err != nil {
		return nil, err
	}
	if hasSchema {
		// :221 const frozen = source && readFrozenCharacterSnapshot(db, source)
		frozen, err := finalizedCharacterReadFrozen(ctx, conn, source)
		if err != nil {
			return nil, err
		}
		// :222 if (!frozen) throw new Error('FINALIZED_CHARACTER_SNAPSHOT_REQUIRED')
		// source is always present here: the commit passes the context's own.
		if frozen == nil {
			return nil, ErrFinalizedCharacterSnapshotRequired
		}
		// :229 const snapshot = readFinalizedSourceFromDb(db, source!.draftId)
		// The reference re-reads it once per candidate; inside one transaction every
		// read returns the same row, so it is read once and the nil arm stays where
		// the reference evaluates it (the evidence guard below).
		snapshot, err := finalizedCharacterReadSource(ctx, conn, source.DraftID)
		if err != nil {
			return nil, err
		}
		result := make([]finalizedCharacterStateStoredCandidate, 0, len(candidates))
		for _, candidate := range candidates {
			// :224-227
			//   const character = frozen.characters.find(item => item.characterId === candidate.characterId)
			//   if (!character || candidate.characterName !== character.displayNameSnapshot
			//     || !CHARACTER_STATE_FIELDS.has(candidate.field) || typeof candidate.value !== 'string'
			//     || frozen.characters.filter(item => item.displayNameSnapshot === character.displayNameSnapshot).length !== 1)
			//     throw new Error('FINALIZED_CHARACTER_IDENTITY_REQUIRED')
			var character *finalizedCharacterFrozenCharacter
			if candidate.characterID != nil {
				for index := range frozen.Characters {
					if frozen.Characters[index].CharacterID == *candidate.characterID {
						character = &frozen.Characters[index]
						break
					}
				}
			}
			if character == nil || candidate.characterName != character.DisplayNameSnapshot ||
				!summaryStateField(candidate.field) {
				return nil, ErrFinalizedCharacterIdentityRequired
			}
			sharing := 0
			for _, item := range frozen.Characters {
				if item.DisplayNameSnapshot == character.DisplayNameSnapshot {
					sharing++
				}
			}
			if sharing != 1 {
				return nil, ErrFinalizedCharacterIdentityRequired
			}
			// :230-232
			//   if (!evidence || !snapshot || !Number.isSafeInteger(evidence.start) || !Number.isSafeInteger(evidence.end)
			//     || evidence.start < 0 || evidence.end <= evidence.start || evidence.end > snapshot.content.length
			//     || snapshot.content.slice(evidence.start, evidence.end) !== evidence.text)
			//     throw new Error('FINALIZED_CHARACTER_EVIDENCE_INVALID')
			if snapshot == nil || !finalizedCharacterCommitEvidenceMatches(candidate.evidence, snapshot.Content) {
				return nil, ErrFinalizedCharacterEvidenceInvalid
			}
			// :233 if (!db.prepare('SELECT 1 FROM characters WHERE character_id=? AND retired=0').get(...))
			//   throw new Error('FINALIZED_CHARACTER_ID_RETIRED')
			active, err := rrMaps(ctx, conn, "SELECT 1 FROM characters WHERE character_id=? AND retired=0", character.CharacterID)
			if err != nil {
				return nil, err
			}
			if len(active) == 0 {
				return nil, ErrFinalizedCharacterIDRetired
			}
			// :234 return structuredClone(candidate)
			result = append(result, candidate)
		}
		return result, nil
	}
	// :237-242 the legacy roster. Every name, retired or not, takes part in the
	// duplicate check, exactly as `SELECT name FROM characters` does.
	//
	// Unreachable from the commit: :221 needs readFrozenCharacterSnapshot to answer
	// null, which needs the identity schema absent, which in turn makes the context
	// read report IdentityStatus legacy - but a commit also needs a freeze receipt to
	// re-verify its own answer against a character id (:435), and a frozen empty list
	// resolves no id. The arm is ported because it is the function's other half, not
	// because the commit can reach it.
	names, err := rrMaps(ctx, conn, "SELECT name FROM characters")
	if err != nil {
		return nil, err
	}
	byIdentity := make(map[string]string, len(names))
	for _, row := range names {
		name := summaryText(row["name"])
		identity := finalizedCharacterRosterIdentityKey(name)
		if identity == "" {
			return nil, ErrContinuityRosterNameConflict
		}
		if _, duplicate := byIdentity[identity]; duplicate {
			return nil, ErrContinuityRosterNameConflict
		}
		byIdentity[identity] = name
	}
	// :243-257
	normalized := make([]finalizedCharacterStateStoredCandidate, 0, len(candidates))
	positions := make(map[string]int, len(candidates))
	for _, input := range candidates {
		// :246 const characterName = typeof input.characterName === 'string' ? input.characterName.trim() : ''
		name := summaryTrim(input.characterName)
		// :247-250
		storedName, known := byIdentity[finalizedCharacterRosterIdentityKey(name)]
		if !known || !summaryStateField(input.field) {
			return nil, ErrContinuityCharacterStateCandidatesInvalid
		}
		candidate := finalizedCharacterStateStoredCandidate{
			characterName: storedName,
			field:         input.field,
			value:         summaryTrim(input.value),
		}
		// :256 normalized.set(`${characterRosterIdentityKey(candidate.characterName)}\u0000${candidate.field}`, candidate)
		key := finalizedCharacterRosterIdentityKey(candidate.characterName) + "\u0000" + candidate.field
		if position, seen := positions[key]; seen {
			normalized[position] = candidate
			continue
		}
		positions[key] = len(normalized)
		normalized = append(normalized, candidate)
	}
	return normalized, nil
}

// finalizedCharacterDecideDerivedPatch is decideDerivedPatch
// (character-identity.ts:67-94). The guards keep the reference's order and its
// `===` comparisons: a stored value that is not the type the reference compares
// takes the conflict arm it takes there, because strict equality never widens.
func finalizedCharacterDecideDerivedPatch(current FinalizedCharacterFieldSnapshot,
	patch finalizedCharacterDerivedPatch, authority finalizedCharacterDerivedAuthority) string {
	order := patch.SourceOrder
	// :70-75
	if !finalizedCharacterCommitSameProjectEpoch(current.ProjectID, current.Epoch, patch.ProjectID, patch.Epoch) ||
		!finalizedCharacterCommitSourceValid(patch.Source) ||
		!finalizedCharacterCommitSourceValid(authority.Source) ||
		patch.Source != authority.Source ||
		!summaryHashValid(patch.Source.ContentHash) ||
		order.ContinuityEpoch == "" ||
		order.ContinuityEpoch != authority.Order.ContinuityEpoch ||
		order.ChapterNumber != authority.Order.ChapterNumber ||
		order.ChapterNumber != patch.Source.ChapterNumber ||
		order.AuthoritativeFinalizationRevision != authority.Order.AuthoritativeFinalizationRevision ||
		order.ChapterNumber < 1 || order.ChapterNumber > maxSafeInteger ||
		order.AuthoritativeFinalizationRevision < 1 || order.AuthoritativeFinalizationRevision > maxSafeInteger {
		return finalizedCharacterDecisionSourceConflict
	}
	// :76-78
	if current.CharacterID != patch.CharacterID || current.Field != patch.Field || patch.CharacterID == "" ||
		!summaryHashValid(patch.ValueHash) || !summaryHashValid(patch.BaseValueHash) ||
		current.Revision < 0 || current.Revision > maxSafeInteger {
		return finalizedCharacterDecisionFieldConflict
	}
	kind, _ := current.Provenance["kind"].(string)
	previous := current.SourceOrder
	// :80-81
	//   if (current.provenance.kind === 'derived' && (!previous
	//     || previous.chapterNumber !== current.provenance.source.chapterNumber)) return 'source-conflict'
	if kind == "derived" {
		derivedChapter, derivedOK := finalizedCharacterCommitProvenanceChapter(current.Provenance)
		previousChapter, previousOK := finalizedCharacterCommitOrderInteger(previous, "chapterNumber")
		if previous == nil || !derivedOK || !previousOK || previousChapter != derivedChapter {
			return finalizedCharacterDecisionSourceConflict
		}
	}
	// :82-83 a stored order that is not a readable order is a source conflict.
	if previous != nil {
		_, epochOK := finalizedCharacterCommitOrderString(previous, "continuityEpoch")
		chapter, chapterOK := finalizedCharacterCommitOrderInteger(previous, "chapterNumber")
		revision, revisionOK := finalizedCharacterCommitOrderInteger(previous, "authoritativeFinalizationRevision")
		if !epochOK || !chapterOK || chapter < 1 || !revisionOK || revision < 1 {
			return finalizedCharacterDecisionSourceConflict
		}
		// :84-87
		//   (previous.continuityEpoch !== order.continuityEpoch
		//     && !(derived && advancesProjectContinuity(patch.projectId, previous.continuityEpoch, order.continuityEpoch, originProjectId))
		//     || previous.chapterNumber > order.chapterNumber
		//     || previous.chapterNumber === order.chapterNumber
		//       && previous.authoritativeFinalizationRevision > order.authoritativeFinalizationRevision)
		previousEpoch, _ := finalizedCharacterCommitOrderString(previous, "continuityEpoch")
		advanced := false
		if kind == "derived" {
			advanced = finalizedCharacterCommitAdvances(patch.ProjectID, previousEpoch, order.ContinuityEpoch, authority.OriginProjectID)
		}
		if previousEpoch != order.ContinuityEpoch && !advanced {
			return finalizedCharacterDecisionSourceConflict
		}
		if chapter > order.ChapterNumber ||
			(chapter == order.ChapterNumber && revision > order.AuthoritativeFinalizationRevision) {
			return finalizedCharacterDecisionSourceConflict
		}
	}
	// :88-89 a derived field that already holds this exact value from this exact
	// source is the "already applied" arm: the model repeated itself.
	if kind == "derived" {
		if source, present := current.Provenance["source"]; present &&
			finalizedCharacterSourceMatches(source, patch.Source) && current.ValueHash == patch.ValueHash {
			return finalizedCharacterDecisionAlreadyApplied
		}
	}
	// :90-91
	if current.Revision != patch.BaseFieldRevision || current.ValueHash != patch.BaseValueHash ||
		!finalizedCharacterCommitSameProvenance(current.Provenance, patch.BaseProvenance) {
		return finalizedCharacterDecisionFieldConflict
	}
	// :92 the author's own field (and any non-derived field with content) becomes a
	// proposal; only a legacy field that is still empty may be filled in place.
	if kind != "derived" && !(kind == "legacy" && current.Value == "") {
		return finalizedCharacterDecisionProposalRequired
	}
	// :93 return 'apply-derived'
	return finalizedCharacterDecisionApply
}

// finalizedCharacterCommitIdentityChanged is the :430-434 comparison:
// isDeepStrictEqual over the identity projection of the context, which is every
// member but the per-character field snapshots, plus the rule that each of the
// caller's snapshots carries the caller's own scope.
func finalizedCharacterCommitIdentityChanged(frozen, current FinalizedCharacterContext) bool {
	if frozen.Source != current.Source || frozen.Content != current.Content ||
		frozen.ProjectionGeneration != current.ProjectionGeneration ||
		frozen.IdentityRevision != current.IdentityRevision ||
		frozen.IdentityStatus != current.IdentityStatus ||
		frozen.SourceOrder != current.SourceOrder ||
		len(frozen.Characters) != len(current.Characters) {
		return true
	}
	for index := range frozen.Characters {
		captured, actual := frozen.Characters[index], current.Characters[index]
		if captured.CharacterID != actual.CharacterID ||
			captured.DisplayNameSnapshot != actual.DisplayNameSnapshot ||
			!slices.Equal(captured.Aliases, actual.Aliases) {
			return true
		}
	}
	for _, character := range frozen.Characters {
		for _, field := range character.Fields {
			if field.CharacterID != character.CharacterID || field.ProjectID != frozen.ProjectID ||
				field.Epoch != frozen.Epoch {
				return true
			}
		}
	}
	return false
}

// finalizedCharacterCommitVerify is :435: the answer is re-parsed against the
// frozen context instead of being trusted. The reference re-encodes its parsed
// updates and parses them again, so the text below is what JSON.stringify of
// those updates produces: the identity, the field values in
// CHARACTER_STATE_TEXT_FIELDS order and the parsed evidence in its own order.
func finalizedCharacterCommitVerify(frozen FinalizedCharacterContext,
	response characterstate.Response) (characterstate.Response, error) {
	updates := make([]any, 0, len(response.Updates))
	for _, update := range response.Updates {
		// The same object the staging arm clones into rawValue (:460), so the
		// re-parse and the stored proposal can never disagree about the answer.
		updates = append(updates, finalizedCharacterCommitUpdateValue(update))
	}
	encoded, err := plotJSON(plotObject{{"updates", updates}})
	if err != nil {
		return characterstate.Response{}, err
	}
	return characterstate.ParseResponse(encoded, finalizedCharacterParseContext(frozen))
}

// finalizedCharacterParseContext is the adapter's conversion of the store context
// into the parser's input. characterstate.Context is deliberately a leaf mirror
// (state_response.go:57-84), so this is where the two halves meet; it is exported
// behaviour of this file only through CommitFinalizedCharacterStates.
func finalizedCharacterParseContext(frozen FinalizedCharacterContext) characterstate.Context {
	parsed := characterstate.Context{
		Source:         characterstate.ContextSource{ChapterNumber: frozen.Source.ChapterNumber},
		Content:        frozen.Content,
		IdentityStatus: frozen.IdentityStatus,
		Characters:     make([]characterstate.ContextCharacter, 0, len(frozen.Characters)),
	}
	for _, character := range frozen.Characters {
		parsed.Characters = append(parsed.Characters, characterstate.ContextCharacter{
			CharacterID:         character.CharacterID,
			DisplayNameSnapshot: character.DisplayNameSnapshot,
			Aliases:             character.Aliases,
		})
	}
	return parsed
}

// finalizedCharacterCommitContext is readFinalizedCharacterContext (:401-424) over
// the commit's own reserved connection.
//
// The published read (ReadFinalizedCharacterContextTx, finalized_character_context.go:210-302)
// takes a *sql.Tx, and the commit holds a *sql.Conn inside its BEGIN IMMEDIATE.
// The reference reads the context with the same database handle and therefore in
// the same transaction as the writes that follow; opening a second transaction
// here would compare the frozen source against a different snapshot than the one
// the commit writes into, which is the one thing :429 exists to prevent. Every
// statement below is that read's, in that order, over the same shared helpers.
func finalizedCharacterCommitContext(ctx context.Context, q rrQueryer, draftID int64,
	scope FinalizedCharacterScope) (*FinalizedCharacterContext, error) {
	// :402 if (!database || !scope.projectId?.trim() || !scope.epoch?.trim())
	if summaryTrim(scope.ProjectID) == "" || summaryTrim(scope.Epoch) == "" {
		return nil, ErrFinalizedCharacterScopeRequired
	}
	// :405 const source = readFinalizedSourceFromDb(db, draftId)
	source, err := finalizedCharacterReadSource(ctx, q, draftID)
	if err != nil {
		return nil, err
	}
	// :406 if (!source) throw new Error('FINALIZED_CHARACTER_SOURCE_CHANGED')
	if source == nil {
		return nil, ErrFinalizedCharacterSourceChanged
	}
	// :407-409 the latest finalized draft of that chapter must be this one.
	latest, err := finalizedCharacterLatestFinalized(ctx, q, source.Identity.ChapterNumber)
	if err != nil {
		return nil, err
	}
	if latest == nil || latest.DraftID != draftID {
		return nil, ErrFinalizedCharacterSourceChanged
	}
	// :410 const frozen = readFrozenCharacterSnapshot(db, source.source)
	frozen, err := finalizedCharacterReadFrozen(ctx, q, source.Identity)
	if err != nil {
		return nil, err
	}
	// :411-415 every frozen character's active row, and its six snapshots.
	characters := []FinalizedCharacterContextCharacter{}
	if frozen != nil {
		for _, character := range frozen.Characters {
			row, err := finalizedCharacterActiveRow(ctx, q, character.CharacterID)
			if err != nil {
				return nil, err
			}
			// :413 if (!row) throw new Error('FINALIZED_CHARACTER_ID_RETIRED')
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
	// :417 identityRevision: frozen?.identityRevision ?? 0, identityStatus: frozen ? 'bound' : 'legacy'
	identityRevision, identityStatus := int64(0), FinalizedCharacterIdentityLegacy
	if frozen != nil {
		identityRevision, identityStatus = frozen.IdentityRevision, FinalizedCharacterIdentityBound
	}
	// :418-419 sourceOrder, whose continuityEpoch is the scope's project and the
	// live generation.
	sourceOrder := FinalizedCharacterSourceOrder{
		ContinuityEpoch:                   scope.ProjectID + ":" + phNumberString(source.ProjectionGeneration),
		ChapterNumber:                     source.Identity.ChapterNumber,
		AuthoritativeFinalizationRevision: latest.Version,
	}
	return &FinalizedCharacterContext{
		ProjectID:            scope.ProjectID,
		Epoch:                scope.Epoch,
		Source:               source.Identity,
		Content:              source.Content,
		ProjectionGeneration: source.ProjectionGeneration,
		IdentityRevision:     identityRevision,
		IdentityStatus:       identityStatus,
		SourceOrder:          sourceOrder,
		Characters:           characters,
	}, nil
}

// finalizedCharacterCommitAdvances is advancesProjectContinuity
// (character-identity.ts:57-64): a derived field may move to a newer generation
// of this project, or - when a verified transfer origin shares the copied counter
// - to an equal generation of that origin.
func finalizedCharacterCommitAdvances(projectID, previous, current, originProjectID string) bool {
	after, afterOK := finalizedCharacterCommitEpochGeneration(current, projectID)
	before, beforeOK := finalizedCharacterCommitEpochGeneration(previous, projectID)
	fromOrigin, originOK := int64(0), false
	if !beforeOK && originProjectID != "" {
		fromOrigin, originOK = finalizedCharacterCommitEpochGeneration(previous, originProjectID)
	}
	return afterOK && (beforeOK && after > before || originOK && after >= fromOrigin)
}

// finalizedCharacterCommitEpochGeneration is the reference's inner generation():
// the decimal suffix of `${project}:${generation}`, only when it is an unpadded
// safe integer.
func finalizedCharacterCommitEpochGeneration(epoch, projectID string) (int64, bool) {
	if projectID == "" || !strings.HasPrefix(epoch, projectID+":") {
		return 0, false
	}
	value := epoch[len(projectID)+1:]
	// /^(0|[1-9][0-9]*)$/u
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return 0, false
	}
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return 0, false
		}
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed > maxSafeInteger {
		return 0, false
	}
	return parsed, true
}

// finalizedCharacterCommitSameProvenance is sameProvenance
// (character-identity.ts:47-50). A kind mismatch is false; within a kind the
// comparison is the reference's `===` on the payload.
func finalizedCharacterCommitSameProvenance(left, right map[string]any) bool {
	leftKind, _ := left["kind"].(string)
	rightKind, _ := right["kind"].(string)
	if leftKind != rightKind {
		return false
	}
	switch leftKind {
	case "legacy":
		return true
	case "author":
		// a.chapterNumber === b.chapterNumber, including absent === absent.
		return rrEqual(left["chapterNumber"], right["chapterNumber"])
	case "derived":
		leftSource, leftPresent := left["source"]
		rightSource, rightPresent := right["source"]
		if !leftPresent || !rightPresent {
			// The reference dereferences an absent derived source (a TypeError with
			// no code); no writer in this repo stores one
			// (character_roster_author.go:1028-1033 refuses it), so the arm only has
			// to be a refusal and not a pass.
			return false
		}
		return finalizedCharacterCommitSameSource(leftSource, rightSource)
	}
	return false
}

// finalizedCharacterCommitSameSource is the two-value sameSource of
// character-identity.ts:45-46: four strict comparisons of the stored objects.
func finalizedCharacterCommitSameSource(left, right any) bool {
	leftObject, leftOK := left.(map[string]any)
	rightObject, rightOK := right.(map[string]any)
	if !leftOK || !rightOK {
		return false
	}
	return rrEqual(leftObject["draftId"], rightObject["draftId"]) &&
		rrEqual(leftObject["finalizationId"], rightObject["finalizationId"]) &&
		rrEqual(leftObject["chapterNumber"], rightObject["chapterNumber"]) &&
		rrEqual(leftObject["contentHash"], rightObject["contentHash"])
}

// finalizedCharacterCommitSameProjectEpoch is sameProjectEpoch
// (src/shared/source-ref.ts:23-24).
func finalizedCharacterCommitSameProjectEpoch(leftProject, leftEpoch, rightProject, rightEpoch string) bool {
	return leftProject != "" && leftEpoch != "" && leftProject == rightProject && leftEpoch == rightEpoch
}

// finalizedCharacterCommitSourceValid is isFinalizedSourceIdentity
// (src/shared/source-ref.ts:18-21).
func finalizedCharacterCommitSourceValid(source FinalizedCharacterSourceIdentity) bool {
	return summaryTrim(source.FinalizationID) != "" &&
		source.DraftID >= 1 && source.DraftID <= maxSafeInteger &&
		source.ChapterNumber >= 1 && source.ChapterNumber <= maxSafeInteger &&
		summaryHashValid(source.ContentHash)
}

// finalizedCharacterCommitProvenanceChapter reads provenance.source.chapterNumber,
// the value decideDerivedPatch matches against the previous order's chapter
// (character-identity.ts:81).
func finalizedCharacterCommitProvenanceChapter(provenance map[string]any) (int64, bool) {
	source, present := provenance["source"]
	if !present {
		return 0, false
	}
	value, present := finalizedCharacterCommitMember(source, "chapterNumber")
	if !present {
		return 0, false
	}
	return rrInteger(value, 0)
}

// finalizedCharacterCommitOrderString reads a string member of a stored
// DerivedSourceOrder. A missing or non-text member is not readable, which is what
// the reference's `!previous.continuityEpoch` refuses.
func finalizedCharacterCommitOrderString(order any, key string) (string, bool) {
	value, present := finalizedCharacterCommitMember(order, key)
	if !present {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

// finalizedCharacterCommitOrderInteger reads a numeric member of a stored
// DerivedSourceOrder. A negative, fractional or out-of-range value is not
// readable.
func finalizedCharacterCommitOrderInteger(order any, key string) (int64, bool) {
	value, present := finalizedCharacterCommitMember(order, key)
	if !present {
		return 0, false
	}
	return rrInteger(value, 0)
}

// finalizedCharacterCommitMember reads one key of a value that may be an ordered
// object built here (plotObject) or a decoded JSON object (map[string]any). A
// stored sourceOrder is always the latter; a patch built by this file is always
// the former.
func finalizedCharacterCommitMember(object any, key string) (any, bool) {
	switch value := object.(type) {
	case map[string]any:
		field, present := value[key]
		return field, present
	case plotObject:
		for _, field := range value {
			if field.key == key {
				return field.value, true
			}
		}
	}
	return nil, false
}

// finalizedCharacterCommitCharacter is characters.find(item => item.characterId === id).
func finalizedCharacterCommitCharacter(characters []FinalizedCharacterContextCharacter,
	characterID string) (FinalizedCharacterContextCharacter, bool) {
	for _, character := range characters {
		if character.CharacterID == characterID {
			return character, true
		}
	}
	return FinalizedCharacterContextCharacter{}, false
}

// finalizedCharacterCommitField is fields.find(item => item.field === field).
func finalizedCharacterCommitField(fields []FinalizedCharacterFieldSnapshot,
	field string) (FinalizedCharacterFieldSnapshot, bool) {
	for _, snapshot := range fields {
		if snapshot.Field == field {
			return snapshot, true
		}
	}
	return FinalizedCharacterFieldSnapshot{}, false
}

// finalizedCharacterCommitHasReceipt is store.receipts.some(...) (:694).
func finalizedCharacterCommitHasReceipt(receipts []finalizedCharacterStateStoredReceipt, candidateKey string) bool {
	for _, receipt := range receipts {
		if receipt.candidateKey == candidateKey {
			return true
		}
	}
	return false
}

// finalizedCharacterCommitMergeKey is
// `${candidate.characterId ?? `legacy:${characterRosterIdentityKey(candidate.characterName)}`}\u0000${candidate.field}` (:695).
func finalizedCharacterCommitMergeKey(candidate finalizedCharacterStateStoredCandidate) string {
	identity := "legacy:" + finalizedCharacterRosterIdentityKey(candidate.characterName)
	if candidate.characterID != nil {
		identity = *candidate.characterID
	}
	return identity + "\u0000" + candidate.field
}

// finalizedCharacterCommitProvenance is the :465 assignment: an existing key keeps
// its position and a new one is appended, which is what assignment to a parsed
// object does in JavaScript.
func finalizedCharacterCommitProvenance(provenance plotObject, field string, entry plotObject) plotObject {
	for index := range provenance {
		if provenance[index].key == field {
			provenance[index].value = entry
			return provenance
		}
	}
	return append(provenance, plotField{field, entry})
}

// finalizedCharacterCommitSourceOrder is the derived source order as ordered JSON
// in the reference's key order (src/shared/character-identity.ts:33-35).
func finalizedCharacterCommitSourceOrder(order FinalizedCharacterSourceOrder) plotObject {
	return plotObject{
		{"continuityEpoch", order.ContinuityEpoch},
		{"chapterNumber", order.ChapterNumber},
		{"authoritativeFinalizationRevision", order.AuthoritativeFinalizationRevision},
	}
}

// finalizedCharacterCommitEvidence is the parsed evidence as ordered JSON
// (finalized-continuity.ts:212 builds { start, end, text } in that order).
func finalizedCharacterCommitEvidence(evidence characterstate.Evidence) plotObject {
	return plotObject{
		{"start", int64(evidence.Start)},
		{"end", int64(evidence.End)},
		{"text", evidence.Text},
	}
}

// finalizedCharacterCommitUpdateValue is structuredClone(update) (:460): the
// verified update exactly as the parser produced it.
func finalizedCharacterCommitUpdateValue(update characterstate.Update) plotObject {
	state := plotObject{}
	for _, field := range characterStateFields {
		if value, present := update.CurrentState.Get(field); present {
			state = append(state, plotField{field, value})
		}
	}
	return plotObject{
		{"characterId", update.CharacterID},
		{"currentState", state},
		{"evidence", finalizedCharacterCommitEvidence(update.Evidence)},
	}
}

// finalizedCharacterCommitPointer takes the address of its own parameter, so every
// candidate owns its own cell.
func finalizedCharacterCommitPointer(value string) *string { return &value }

// finalizedCharacterCommitEvidenceMatches is the :230-232 evidence guard. Offsets
// count UTF-16 code units in the immutable prose (:231 `snapshot.content.slice`),
// so the comparison is made in code units and never in bytes or runes.
func finalizedCharacterCommitEvidenceMatches(evidence any, content string) bool {
	// `!evidence` is a truthiness test, and an empty object is truthy.
	if !phTruthy(evidence) {
		return false
	}
	startValue, _ := finalizedCharacterCommitMember(evidence, "start")
	endValue, _ := finalizedCharacterCommitMember(evidence, "end")
	textValue, _ := finalizedCharacterCommitMember(evidence, "text")
	start, startOK := rrInteger(startValue, -maxSafeInteger)
	end, endOK := rrInteger(endValue, -maxSafeInteger)
	text, textOK := textValue.(string)
	if !startOK || !endOK || !textOK {
		return false
	}
	units := utf16.Encode([]rune(content))
	if start < 0 || end <= start || end > int64(len(units)) {
		return false
	}
	return slices.Equal(units[start:end], utf16.Encode([]rune(text)))
}

// finalizedCharacterRosterIdentityKey is characterRosterIdentityKey
// (src/shared/character-roster.ts:17-19): name.trim().toLocaleLowerCase('en-US').
// Only ASCII is folded, the way charactersRole already folds it
// (characters.go:34-42): Go's Unicode lowercasing would accept a name JavaScript
// does not, and a merge key that accepts more than the reference's would merge two
// author characters.
func finalizedCharacterRosterIdentityKey(name string) string {
	return strings.Map(func(character rune) rune {
		if character >= 'A' && character <= 'Z' {
			return character + ('a' - 'A')
		}
		return character
	}, summaryTrim(name))
}
