package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// This file ports the two halves db:blueprint-character-sync-complete was
// refused for: the authoritative completion proof and the writer that records
// its receipt.
//
// The reference is one method and one function
// (electron/repositories/blueprint-repository.ts):
//
//	completeCharacterSyncOperation(:764-796)
//	  ensureBlueprintCommitSchema(db)                       :770
//	  tx.immediate():
//	    readCharacterSyncOperation(db, operationId)         :771
//	    if (operation.status === 'completed') {             :773
//	      assertAuthoritativeCharacterSyncCompletion(...)   :774
//	      return operation                                  :775
//	    }
//	    authoritativeCharacterSyncCompletionReceipt(...)    :777
//	    UPDATE ... SET status='completed',                  :778-786
//	      completion_receipt=?, completed_at=datetime('now'),
//	      updated_at=datetime('now')
//	      WHERE operation_id=? AND status='pending'         :786
//	    read-back + receipt assertion                       :787-793
//	assertAuthoritativeCharacterSyncCompletion(:420-431)
//	  the stored receipt must equal the freshly computed one :423-430
//
// # What the roster proof actually checks
//
// authoritativeCharacterSyncCompletionReceipt (:366-418) is a gate followed by
// three checks, and each one is a fact this file re-derives rather than trusts:
//
//  1. the roster is authoritative at all: CharacterRosterRepository.read()
//     (:976 -> readSnapshot :894) must answer status 'ready' or 'empty'
//     (:377-379). Every other derived status - 'legacy_repair_required',
//     'inconsistent' - and every roster store refusal is a refusal here.
//  2. the frozen blueprint facts still hold in that roster
//     (blueprintCharacterSyncFactError, src/shared/
//     blueprint-character-sync-evidence.ts:63-91, called at :379): every
//     `newCharacterCandidates[].name` this sync input declared must exist in
//     the roster by identity key (:69-73), and every structured
//     `relationshipHints` edge whose two ends both exist must be recorded in
//     BOTH directions with the same relation text (:74-88), unless one end
//     carries legacy relationship prose, which stays read-only evidence
//     (:78-81).
//  3. the roster commit that owns this operation left hash/revision evidence
//     (:383-407): exactly one character_roster_operations row for this
//     operation id, committed_revision an integer in [1, roster.revision],
//     payload_hash and projection_hash both 64 lowercase hex characters, and -
//     when the evidence names the CURRENT roster revision - a projection_hash
//     equal to the roster's own. Anything else is
//     BLUEPRINT_CHARACTER_SYNC_EVIDENCE_MISMATCH.
//
// The receipt is the reference's own object and nothing more (:408-416):
//
//	{ blueprintCommitOperationId, operationId, status: 'committed',
//	  rosterReceipt: { operationId, payloadHash, revision, idempotent: false } }
//
// and a sync operation with no roster evidence answers the reference's
// 'already-satisfied' object, which carries no rosterReceipt at all (:387-393).
//
// # What was already in Go
//
// Almost all of it. charactersRoster (character_roster.go:466) is the roster
// read: status derivation, revision, projectionHash and the same entries shape
// (relationships, legacyRelationshipNotes) the reference's readSnapshot answers,
// and RosterAuthor.CommitAuthorRoster already proved it at admit time
// (character_roster_author.go:176-189). Missing were only the fact proof
// (blueprintCharacterSyncFactError), the character_roster_operations evidence
// check, and a writer: no store method wrote a completion receipt at all. This
// file adds those three and reuses everything else.
//
// # Deliberate difference: the proposal branch is NOT this proof
//
// The reference tests one branch before the roster one:
//
//	if (hasCharacterIdentitySchema(db)) { ... 'proposal-staged' ... }  :371-376
//
// In a project with character_identity_meta it demands directory proposal
// evidence (findCharacterProposalEvidence, character-proposal-service.ts:337-346,
// which throws CHARACTER_PROPOSAL_EVIDENCE_REQUIRED when none matches) and never
// reads the roster. That service has no port in this module, so binding it as the
// proof would mean every real request refused, including the ones this lane can
// prove. This port therefore asserts branch two - the roster proof the row's own
// seam names, BLUEPRINT_CHARACTER_SYNC_ROSTER_NOT_READY (:377) - for every
// project. The consequences are exact and none of them is silent: a completed row
// whose receipt says 'proposal-staged' is refused by the receipt comparison below
// with BLUEPRINT_CHARACTER_SYNC_RECEIPT_MISMATCH instead of being accepted as
// authoritative, and db:blueprint-character-sync-get keeps its existing
// completed-row refusal (blueprints.go:178), so nothing in this lane reads a
// completed row back as if the proposal owner had approved it.
//
// # Identity keys
//
// characterRosterIdentityKey (src/shared/character-roster.ts:17-19) is
// trim().toLocaleLowerCase('en-US'). Trim is exactly the store's narrativeSpace
// set (narrative.go:31), and the case fold is Go's Unicode simple mapping. The
// two differ only for the handful of characters with a SpecialCasing full
// mapping (U+0130 is the one that can change a key), which no roster name in this
// lane's data reaches; the divergence is recorded here rather than hidden behind
// a normalization this lane cannot reproduce without ICU.
var (
	ErrBlueprintCharacterSyncRosterNotReady   = errors.New("BLUEPRINT_CHARACTER_SYNC_ROSTER_NOT_READY")
	ErrBlueprintCharacterSyncFactsChanged     = errors.New("BLUEPRINT_CHARACTER_SYNC_FACTS_CHANGED")
	ErrBlueprintCharacterSyncEvidenceMismatch = errors.New("BLUEPRINT_CHARACTER_SYNC_EVIDENCE_MISMATCH")
	ErrBlueprintCharacterSyncReceiptMismatch  = errors.New("BLUEPRINT_CHARACTER_SYNC_RECEIPT_MISMATCH")
	ErrBlueprintCharacterSyncReadBackFailed   = errors.New("BLUEPRINT_CHARACTER_SYNC_READ_BACK_FAILED")
)

// blueprintSyncRelation is one relationship fact the sync input declared. The
// reference calls the same triple a RelationshipFact
// (blueprint-character-sync-evidence.ts:18-22).
type blueprintSyncRelation struct{ from, to, relation string }

// blueprintSyncHexHash reports whether text is one lowercase sha256 digest, the
// reference's SHA256_HEX (/^[a-f0-9]{64}$/u, blueprint-repository.ts:142).
func blueprintSyncHexHash(text string) bool {
	if len(text) != 64 {
		return false
	}
	for index := 0; index < len(text); index++ {
		digit := text[index]
		if (digit >= '0' && digit <= '9') || (digit >= 'a' && digit <= 'f') {
			continue
		}
		return false
	}
	return true
}

// blueprintSyncIdentityKey ports characterRosterIdentityKey
// (src/shared/character-roster.ts:17-19). See the identity-key note above for the
// one character where Go's simple case fold and toLocaleLowerCase('en-US')
// disagree.
func blueprintSyncIdentityKey(name string) string {
	return strings.ToLower(strings.TrimFunc(name, narrativeSpace))
}

// blueprintSyncRelationshipFacts ports relationshipFacts
// (blueprint-character-sync-evidence.ts:28-45). A hint list that is not an array
// declares nothing (:29), an element that is not an object declares nothing
// (:30-31), either end may arrive as from/to or as source/target (:32-33, `??`),
// both ends must be non-blank strings (:34-39) and a blank or non-string relation
// defaults to the reference's own literal (:40-42). A hint with a malformed end is
// dropped, exactly as the reference's flatMap drops it, so dropping can never
// invent a fact.
func blueprintSyncRelationshipFacts(hints any) []blueprintSyncRelation {
	items, ok := hints.([]any)
	if !ok {
		return nil
	}
	facts := []blueprintSyncRelation{}
	for _, item := range items {
		candidate, ok := item.(map[string]any)
		if !ok {
			continue
		}
		rawFrom, present := candidate["from"]
		if !present || rawFrom == nil {
			rawFrom = candidate["source"]
		}
		rawTo, present := candidate["to"]
		if !present || rawTo == nil {
			rawTo = candidate["target"]
		}
		from, validFrom := rawFrom.(string)
		from = strings.TrimFunc(from, narrativeSpace)
		to, validTo := rawTo.(string)
		to = strings.TrimFunc(to, narrativeSpace)
		if !validFrom || from == "" || !validTo || to == "" {
			continue
		}
		relation := "相关"
		if text, ok := candidate["relation"].(string); ok && strings.TrimFunc(text, narrativeSpace) != "" {
			relation = strings.TrimFunc(text, narrativeSpace)
		}
		facts = append(facts, blueprintSyncRelation{from: from, to: to, relation: relation})
	}
	return facts
}

// blueprintSyncLegacyNotes reports whether one roster entry still carries legacy
// relationship prose. The reference tests the entry's own field (:81) after
// normalizeEntry trimmed it (character-roster-repository.ts:206-207), so a
// whitespace-only note is absent here too.
func blueprintSyncLegacyNotes(entry map[string]any) bool {
	text, ok := entry["legacyRelationshipNotes"].(string)
	return ok && strings.TrimFunc(text, narrativeSpace) != ""
}

// blueprintSyncEdge ports relationshipSatisfied (:47-56): one entry records the
// edge to target under relation, compared by identity key of the target name and
// by the exact relation text. The roster read builds edges as []map[string]any
// (character_roster.go:434-446), and any other shape is a missing edge rather
// than a satisfied one.
func blueprintSyncEdge(entry map[string]any, target, relation string) bool {
	edges, ok := entry["relationships"].([]map[string]any)
	if !ok {
		return false
	}
	key := blueprintSyncIdentityKey(target)
	for _, edge := range edges {
		name, ok := edge["target"].(string)
		if !ok || blueprintSyncIdentityKey(name) != key {
			continue
		}
		text, ok := edge["relation"].(string)
		if ok && strings.TrimFunc(text, narrativeSpace) == relation {
			return true
		}
	}
	return false
}

// blueprintSyncFactsHold ports blueprintCharacterSyncFactError
// (blueprint-character-sync-evidence.ts:63-91) as the boolean the reference's
// caller really uses: :379-380 discards the message and only tests whether one
// was produced.
func blueprintSyncFactsHold(inputs []any, entries []map[string]any) (bool, error) {
	byName := make(map[string]map[string]any, len(entries))
	for _, entry := range entries {
		name, ok := entry["name"].(string)
		if !ok {
			return false, ErrCharacterInputUnsupported
		}
		byName[blueprintSyncIdentityKey(name)] = entry
	}
	for _, raw := range inputs {
		blueprint, ok := raw.(map[string]any)
		if !ok {
			return false, ErrBlueprintCharacterSyncInputInvalid
		}
		// :69-73 - a declared reusable candidate must be a roster fact already. A
		// candidate list that is present but not an array is refused rather than
		// read as empty: the reference throws on it, and an empty read would accept
		// a row whose candidate evidence was never recorded.
		if candidates, present := blueprint["newCharacterCandidates"]; present && candidates != nil {
			items, ok := candidates.([]any)
			if !ok {
				return false, nil
			}
			for _, item := range items {
				candidate, ok := item.(map[string]any)
				if !ok {
					return false, nil
				}
				name, ok := candidate["name"].(string)
				if !ok {
					return false, nil
				}
				if _, found := byName[blueprintSyncIdentityKey(name)]; !found {
					return false, nil
				}
			}
		}
		// :74-88 - a structured hint between two known characters must be mirrored
		// both ways with the same relation, unless either end's relationship data is
		// still legacy prose, which this proof must not force into a lossy rewrite.
		for _, fact := range blueprintSyncRelationshipFacts(blueprint["relationshipHints"]) {
			from, foundFrom := byName[blueprintSyncIdentityKey(fact.from)]
			to, foundTo := byName[blueprintSyncIdentityKey(fact.to)]
			if !foundFrom || !foundTo {
				continue
			}
			if blueprintSyncLegacyNotes(from) || blueprintSyncLegacyNotes(to) {
				continue
			}
			if !blueprintSyncEdge(from, fact.to, fact.relation) || !blueprintSyncEdge(to, fact.from, fact.relation) {
				return false, nil
			}
		}
	}
	return true, nil
}

// blueprintSyncAuthoritativeReceipt ports authoritativeCharacterSyncCompletionReceipt
// (:366-418) for the roster branch, in the reference's own order so a project
// that fails twice answers with the first code the reference would publish.
// It runs on the caller's reserved connection, inside the completion transaction,
// so the roster snapshot, the evidence row and the write describe one state.
func blueprintSyncAuthoritativeReceipt(ctx context.Context, conn *sql.Conn, row map[string]any) (map[string]any, error) {
	operationID, ok := row["operationId"].(string)
	if !ok {
		return nil, ErrBlueprintCharacterSyncInputInvalid
	}
	commitID, ok := row["blueprintCommitOperationId"].(string)
	if !ok {
		return nil, ErrBlueprintCharacterSyncInputInvalid
	}
	// :377-379 - the roster read is the authority, and only 'ready' or 'empty' is
	// authoritative. charactersRoster publishes the reference's own refusals for a
	// project whose roster tables or meta row are missing or inconsistent, and they
	// travel unchanged.
	roster, err := charactersRoster(ctx, conn)
	if err != nil {
		return nil, err
	}
	status, _ := roster["status"].(string)
	if status != "ready" && status != "empty" {
		return nil, ErrBlueprintCharacterSyncRosterNotReady
	}
	entries, ok := roster["entries"].([]map[string]any)
	if !ok {
		return nil, ErrBlueprintCharacterSyncRosterNotReady
	}
	inputs, ok := row["characterSyncInput"].([]any)
	if !ok {
		return nil, ErrBlueprintCharacterSyncInputInvalid
	}
	hold, err := blueprintSyncFactsHold(inputs, entries)
	if err != nil {
		return nil, err
	}
	if !hold {
		return nil, ErrBlueprintCharacterSyncFactsChanged
	}
	evidence, err := rrMaps(ctx, conn, `SELECT operation_id,payload_hash,committed_revision,projection_hash
 FROM character_roster_operations WHERE operation_id=?`, operationID)
	if err != nil {
		return nil, err
	}
	// :387-393 - no committed roster operation for this sync is not a failure: the
	// roster already carries the facts, and the receipt says only that.
	if len(evidence) == 0 {
		return map[string]any{
			"blueprintCommitOperationId": commitID,
			"operationId":                operationID,
			"status":                     "already-satisfied",
		}, nil
	}
	record := evidence[0]
	storedID, _ := record["operation_id"].(string)
	payloadHash, _ := record["payload_hash"].(string)
	projectionHash, _ := record["projection_hash"].(string)
	revision, revisionOK := rrInteger(record["committed_revision"], 1)
	rosterRevision, rosterOK := rrInteger(roster["revision"], 0)
	rosterProjection, _ := roster["projectionHash"].(string)
	// :394-407 - the evidence row must be this operation's, name a revision the
	// roster has actually reached, carry two well-formed digests, and agree with the
	// current projection whenever it claims the current revision.
	if !rosterOK {
		return nil, ErrBlueprintCharacterSyncEvidenceMismatch
	}
	if storedID != operationID ||
		!revisionOK || revision > rosterRevision ||
		!blueprintSyncHexHash(payloadHash) || !blueprintSyncHexHash(projectionHash) ||
		(revision == rosterRevision && projectionHash != rosterProjection) {
		return nil, ErrBlueprintCharacterSyncEvidenceMismatch
	}
	// :408-416 - hash and revision evidence only; the roster snapshot itself stays
	// in its fact tables and is never copied into a receipt.
	return map[string]any{
		"blueprintCommitOperationId": commitID,
		"operationId":                operationID,
		"status":                     "committed",
		"rosterReceipt": map[string]any{
			"operationId": storedID,
			"payloadHash": payloadHash,
			"revision":    revision,
			"idempotent":  false,
		},
	}, nil
}

// blueprintSyncReceiptEqual is the comparison assertAuthoritativeCharacterSyncCompletion
// makes (:423-430): canonicalize both receipts and require the same bytes. Both
// sides here are Go values, so plotJSON's deterministic key order is the
// canonicalization: it sorts keys by UTF-16 code unit, and every receipt key is
// ASCII, where that order coincides with the reference's en-US collation. A
// receipt that is absent or not an object is a mismatch, never an empty match.
func blueprintSyncReceiptEqual(stored any, authoritative map[string]any) error {
	object, ok := stored.(map[string]any)
	if !ok {
		return ErrBlueprintCharacterSyncReceiptMismatch
	}
	left, err := plotJSON(object)
	if err != nil {
		return ErrBlueprintCharacterSyncReceiptMismatch
	}
	right, err := plotJSON(authoritative)
	if err != nil {
		return err
	}
	if left != right {
		return ErrBlueprintCharacterSyncReceiptMismatch
	}
	return nil
}

// CompleteSync serves db:blueprint-character-sync-complete: one transaction that
// proves the roster is authoritative, writes the completion receipt the reference
// writes and marks the row complete.
//
// A refusal before the UPDATE leaves the file byte-identical, because the only
// statement that can come before it is the reference's own insert-only lazy
// repair, which inserts nothing when the commit ledger is already represented.
// The already-completed branch issues no write at all (:773-775), so a second
// completion of the same row is idempotent - or, when the stored receipt no
// longer matches the proof, BLUEPRINT_CHARACTER_SYNC_RECEIPT_MISMATCH (:429).
func (b *Blueprints) CompleteSync(ctx context.Context, id string) (map[string]any, error) {
	// The reference checks the id before it reaches for the database (:768).
	if strings.TrimFunc(id, narrativeSpace) == "" {
		return nil, ErrBlueprintCharacterSyncOperationIDRequired
	}
	if b == nil || b.db == nil {
		return nil, ErrProjectNotOpen
	}
	// The reference repairs as it reads and as it completes: ensureBlueprintCommitSchema
	// is INSERT OR IGNORE over the commit ledger (:176-194, called at :770) and this
	// lane's two sync reads already mirror it (blueprints.go:167, :174). It runs
	// before the transaction, exactly where the reference runs it.
	if _, err := b.db.ExecContext(ctx, blueprintBackfillSQL); err != nil {
		return nil, err
	}
	var completed map[string]any
	err := NewReviewRevisions(b.db).immediate(ctx, func(conn *sql.Conn) error {
		row, err := blueprintSync(ctx, conn, id)
		if err != nil {
			return err
		}
		if row == nil {
			return ErrBlueprintCharacterSyncOperationMissing
		}
		// :773-775 - the deciding lines: a completed row is never updated, it is
		// PROVED, and it is returned unchanged when the proof agrees with the receipt
		// already stored. That is the idempotent answer, and the mismatch refusal is
		// the one assertAuthoritativeCharacterSyncCompletion raises inside it (:429).
		if row["status"] == "completed" {
			authoritative, err := blueprintSyncAuthoritativeReceipt(ctx, conn, row)
			if err != nil {
				return err
			}
			if err := blueprintSyncReceiptEqual(row["completionReceipt"], authoritative); err != nil {
				return err
			}
			completed = row
			return nil
		}
		receipt, err := blueprintSyncAuthoritativeReceipt(ctx, conn, row)
		if err != nil {
			return err
		}
		encoded, err := plotJSON(receipt)
		if err != nil {
			return err
		}
		// :778-786 - one UPDATE, conditional on the row still being pending, so a
		// concurrent completion is a lost CAS rather than a second receipt.
		update, execErr := conn.ExecContext(ctx, `UPDATE blueprint_character_sync_operations
 SET status='completed',completion_receipt=?,completed_at=datetime('now'),updated_at=datetime('now')
 WHERE operation_id=? AND status='pending'`, encoded, id)
		if err := rrAffected(update, execErr, ErrBlueprintCharacterSyncReadBackFailed); err != nil {
			return err
		}
		// :787-793 - read the row back and prove the receipt that is now in the file,
		// not the one this call built.
		stored, err := blueprintSync(ctx, conn, id)
		if err != nil {
			return err
		}
		if stored == nil || stored["status"] != "completed" {
			return ErrBlueprintCharacterSyncReadBackFailed
		}
		readBack, present := stored["completionReceipt"]
		if !present || readBack == nil {
			return ErrBlueprintCharacterSyncReadBackFailed
		}
		if err := blueprintSyncReceiptEqual(readBack, receipt); err != nil {
			return err
		}
		completed = stored
		return nil
	})
	if err != nil {
		return nil, err
	}
	return completed, nil
}
