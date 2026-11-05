package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode/utf16"
)

// The author roster commit's own refusal vocabulary, verbatim from
// electron/services/character-roster-author.ts and the roster normalizers it
// calls (electron/repositories/character-roster-repository.ts:60-352).
var (
	ErrCharacterAuthorRequestInvalid = errors.New("CHARACTER_AUTHOR_REQUEST_INVALID")
	ErrCharacterAuthorEntryInvalid   = errors.New("CHARACTER_AUTHOR_ENTRY_INVALID")
	ErrCharacterAuthorScopeRequired  = errors.New("CHARACTER_AUTHOR_SCOPE_REQUIRED")
	ErrCharacterAuthorRosterNotReady = errors.New("CHARACTER_AUTHOR_ROSTER_NOT_READY")
	ErrCharacterAuthorReceiptInvalid = errors.New("CHARACTER_AUTHOR_RECEIPT_INVALID")
	ErrCharacterAuthorReadbackFailed = errors.New("CHARACTER_AUTHOR_READBACK_FAILED")
	ErrCharacterAuthorWriteRequired  = errors.New("CHARACTER_ID_WRITE_REQUIRED")

	ErrCharacterRosterTextInvalid                   = errors.New("CHARACTER_ROSTER_TEXT_INVALID")
	ErrCharacterRosterNameRequired                  = errors.New("CHARACTER_ROSTER_NAME_REQUIRED")
	ErrCharacterRosterRoleInvalid                   = errors.New("CHARACTER_ROSTER_ROLE_INVALID")
	ErrCharacterRosterRelationshipsInvalid          = errors.New("CHARACTER_ROSTER_RELATIONSHIPS_INVALID")
	ErrCharacterRosterRelationshipTargetRequired    = errors.New("CHARACTER_ROSTER_RELATIONSHIP_TARGET_REQUIRED")
	ErrCharacterRosterRelationshipDescriptionNeeded = errors.New("CHARACTER_ROSTER_RELATIONSHIP_DESCRIPTION_REQUIRED")
	ErrCharacterRosterRelationshipSelf              = errors.New("CHARACTER_ROSTER_RELATIONSHIP_SELF")
	ErrCharacterRosterRelationshipDuplicate         = errors.New("CHARACTER_ROSTER_RELATIONSHIP_DUPLICATE")
	ErrCharacterRosterStateInvalid                  = errors.New("CHARACTER_ROSTER_STATE_INVALID")
	ErrCharacterRosterStateChapterInvalid           = errors.New("CHARACTER_ROSTER_STATE_CHAPTER_INVALID")
	ErrCharacterRosterStateProvenanceInvalid        = errors.New("CHARACTER_ROSTER_STATE_PROVENANCE_INVALID")
	ErrCharacterRosterFinalizedSourceInvalid        = errors.New("CHARACTER_ROSTER_FINALIZED_SOURCE_INVALID")
)

// RosterAuthorScope is the frozen author session the original controller hands to
// commitAuthorCharacterRoster: the captured project and its lease epoch.
type RosterAuthorScope struct{ ProjectID, Epoch string }

// RosterAuthor is the identity-schema author roster commit
// (electron/services/character-roster-author.ts:63-155): the entries a human
// edited are translated into one identity commit, then the projection, the roster
// meta row and the readback are all resolved in the same transaction. It borrows an
// already admitted project database and never opens, migrates or switches one.
type RosterAuthor struct{ db *sql.DB }

func NewRosterAuthor(db *sql.DB) *RosterAuthor { return &RosterAuthor{db: db} }

// The eleven static fields in the original's declared order (author.ts:11). The
// same order drives the diff, the identity write and the canonical request hash.
const (
	rosterAuthorName = iota
	rosterAuthorRole
	rosterAuthorGender
	rosterAuthorAge
	rosterAppearance
	rosterPersonality
	rosterBackground
	rosterAbilities
	rosterMotivation
	rosterArc
	rosterNotes
)

var rosterAuthorFields = [rosterNotes + 1]string{"name", "role", "gender", "age", "appearance", "personality", "background", "abilities", "motivation", "arc", "notes"}

type rosterAuthorRelationship struct {
	targetID string
	target   string
	relation string
}

type rosterAuthorState struct {
	text       [6]string
	updatedAt  int64
	provenance plotObject
}

type rosterAuthorEntry struct {
	characterID   string
	static        [rosterNotes + 1]string
	relationships []rosterAuthorRelationship
	currentState  *rosterAuthorState
	// legacyNotes is the entry's own legacyRelationshipNotes when the request
	// carried the key at all (even as an empty string), and nil when it did not.
	legacyNotes *string
}

type rosterAuthorRequest struct {
	operationID            string
	expectedRevision       int64
	expectedIdentityRevision int64
	entries                []rosterAuthorEntry
}

// rosterAuthorEnvelope is the saved idempotency envelope (:151-153) and this is
// its decode shape only: it is written through the canonical encoder so the
// embedded receipt keeps exactly the bytes its own hash was taken over instead of
// a re-escaping by encoding/json (which would escape <, > and & inside it and
// break the replay hash for a name containing one).
type rosterAuthorEnvelope struct {
	Version             int    `json:"version"`
	Kind                string `json:"kind"`
	RequestHash         string `json:"requestHash"`
	Receipt             string `json:"receipt"`
	ReceiptHash         string `json:"receiptHash"`
	ApprovalID          string `json:"approvalId"`
	ApprovalPayloadHash string `json:"approvalPayloadHash"`
	ApprovalReceiptHash string `json:"approvalReceiptHash"`
}

// CommitAuthorRoster ports commitAuthorCharacterRoster. The returned map is the
// original CharacterRosterCommitReceipt; the browser's own controller wraps it in
// {success:true,receipt} exactly like every other db-controller write.
func (a *RosterAuthor) CommitAuthorRoster(ctx context.Context, candidate any, scope RosterAuthorScope,
	authorize func() error) (map[string]any, error) {
	if a == nil || a.db == nil {
		return nil, ErrProjectNotOpen
	}
	// :65 the identity schema and both frozen scope halves are checked before the
	// transaction opens, so a project without identity rows never reaches a write.
	identity, err := rrMaps(ctx, a.db, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='character_identity_meta'")
	if err != nil {
		return nil, err
	}
	if len(identity) == 0 || summaryTrim(scope.ProjectID) == "" || summaryTrim(scope.Epoch) == "" {
		return nil, ErrCharacterAuthorScopeRequired
	}
	request, err := rosterAuthorNormalize(candidate)
	if err != nil {
		return nil, err
	}
	// :66 the payload hash binds this exact normalized request to the frozen
	// session, and is also the identity approval's content hash.
	requestHash, err := rosterAuthorRequestHash(scope, request)
	if err != nil {
		return nil, err
	}
	operationKey := "author-roster-request:" + request.operationID
	approvalID := "author-roster-effect:" + request.operationID
	var receipt map[string]any
	if err := NewReviewRevisions(a.db).immediate(ctx, func(conn *sql.Conn) error {
		var inner error
		receipt, inner = rosterAuthorCommit(ctx, conn, scope, request, requestHash, operationKey, approvalID, authorize)
		return inner
	}); err != nil {
		return nil, err
	}
	return receipt, nil
}

func rosterAuthorCommit(ctx context.Context, conn *sql.Conn, scope RosterAuthorScope, request rosterAuthorRequest,
	requestHash, operationKey, approvalID string, authorize func() error) (map[string]any, error) {
	// :69 the frozen-context fence is the first statement inside the transaction.
	// The original has no default for it, so a missing callback can only refuse.
	if authorize == nil {
		return nil, ErrCharacterAuthorScopeRequired
	}
	if err := authorize(); err != nil {
		return nil, err
	}
	// :70-81 a replayed author operation is either this exact request or a nonce
	// conflict; it never writes a second time.
	prior, err := rrMaps(ctx, conn, "SELECT payload_hash,receipt_json FROM character_identity_approvals WHERE operation_id=?", operationKey)
	if err != nil {
		return nil, err
	}
	if len(prior) > 0 {
		return rosterAuthorReplay(ctx, conn, request, requestHash, approvalID, prior[0])
	}
	// :82-84 the roster must be exactly the snapshot the author edited.
	current, err := charactersRoster(ctx, conn)
	if err != nil {
		return nil, err
	}
	revision, okRevision := rrInteger(current["revision"], 0)
	identityRevision, okIdentity := rrInteger(current["identityRevision"], 0)
	if !okRevision || !okIdentity {
		return nil, ErrCharacterInputUnsupported
	}
	if revision != request.expectedRevision || identityRevision != request.expectedIdentityRevision {
		return nil, ErrCharacterIDRevisionConflict
	}
	if status := rosterText(current["status"]); status != "ready" && status != "empty" {
		return nil, ErrCharacterAuthorRosterNotReady
	}
	currentEntries, ok := current["entries"].([]map[string]any)
	if !ok {
		return nil, ErrCharacterInputUnsupported
	}
	original := make(map[string]map[string]any, len(currentEntries))
	retireIDs := make([]string, 0, len(currentEntries))
	for _, entry := range currentEntries {
		id := rosterText(entry["characterId"])
		original[id] = entry
		retireIDs = append(retireIDs, id)
	}
	kept := make(map[string]bool, len(request.entries))
	for _, entry := range request.entries {
		if !rosterAuthorDraftID(entry.characterID) {
			kept[entry.characterID] = true
		}
	}
	for _, entry := range request.entries {
		if _, exists := original[entry.characterID]; !exists && !rosterAuthorDraftID(entry.characterID) {
			return nil, ErrCharacterIDUnknown
		}
	}
	// :88-93 one author-edit identity commit. Its source revision and content hash
	// are the roster snapshot and the author payload hash, and every existing
	// character the author dropped from the list is retired here.
	retirements := make([]string, 0, len(retireIDs))
	for _, id := range retireIDs {
		if !kept[id] {
			retirements = append(retirements, id)
		}
	}
	identity := CharacterIdentityCommitRequest{
		Approval: CharacterIdentityApproval{
			OperationID:      approvalID,
			ExpectedRevision: identityRevision,
			Action:           "author-edit",
			Source: &CharacterIdentityProvenance{Kind: "author", Source: CharacterIdentitySourceRef{
				ProjectID: scope.ProjectID, Epoch: scope.Epoch, SourceID: operationKey,
				Revision: identityRevision, ContentHash: requestHash,
			}},
		},
		RetireIDs: retirements,
	}
	// :94-99 only genuinely changed static fields are written, and a creation takes
	// all eleven because no prior row exists to compare against.
	for _, entry := range request.entries {
		before, exists := original[entry.characterID]
		changed := make([]CharacterIdentityField, 0, len(rosterAuthorFields))
		for index, key := range rosterAuthorFields {
			if !exists || rosterAuthorStaticChanged(before[key], entry.static[index]) {
				changed = append(changed, CharacterIdentityField{Key: key, Value: entry.static[index]})
			}
		}
		if !exists {
			// The original spreads `{...changed, name: entry.name}`; a creation marks
			// every field changed, so the name already leads `changed`.
			identity.Creations = append(identity.Creations, CharacterIdentityCreation{SelectionKey: entry.characterID, Fields: changed})
			continue
		}
		if len(changed) > 0 {
			identity.Changes = append(identity.Changes, CharacterIdentityChange{CharacterID: entry.characterID, Fields: changed})
		}
	}
	// :100-112 the relationship diff: rows an author kept from both endpoints and
	// left in the list survive untouched; every other row of a kept endpoint is
	// deleted, and only genuinely new edges are added by the identity commit.
	existing, err := rrMaps(ctx, conn, `SELECT r.relationship_id,r.source_character_id,r.target_character_id,r.relation FROM character_relationships r
		JOIN characters a ON a.character_id=r.source_character_id JOIN characters b ON b.character_id=r.target_character_id WHERE a.retired=0 AND b.retired=0`)
	if err != nil {
		return nil, err
	}
	desired := make(map[string]bool, len(existing))
	for _, entry := range request.entries {
		for _, relation := range entry.relationships {
			key, err := plotJSON([]any{entry.characterID, relation.targetID, relation.relation})
			if err != nil {
				return nil, err
			}
			desired[key] = true
		}
	}
	previous := make(map[string]bool, len(existing))
	for _, row := range existing {
		key, err := plotJSON([]any{row["source_character_id"], row["target_character_id"], row["relation"]})
		if err != nil {
			return nil, err
		}
		previous[key] = true
	}
	for _, row := range existing {
		if !kept[rosterText(row["source_character_id"])] || !kept[rosterText(row["target_character_id"])] {
			continue
		}
		key, err := plotJSON([]any{row["source_character_id"], row["target_character_id"], row["relation"]})
		if err != nil {
			return nil, err
		}
		if desired[key] {
			continue
		}
		if _, err := conn.ExecContext(ctx, "DELETE FROM character_relationships WHERE relationship_id=?", row["relationship_id"]); err != nil {
			return nil, err
		}
	}
	for _, entry := range request.entries {
		for _, relation := range entry.relationships {
			key, err := plotJSON([]any{entry.characterID, relation.targetID, relation.relation})
			if err != nil {
				return nil, err
			}
			if previous[key] {
				continue
			}
			edge := CharacterIdentityRelationship{Relation: relation.relation}
			// draft:<uuid> is a local creation selection key, never a persisted ID.
			if rosterAuthorDraftID(entry.characterID) {
				edge.SourceSelectionKey = entry.characterID
			} else {
				edge.SourceCharacterID = entry.characterID
			}
			if rosterAuthorDraftID(relation.targetID) {
				edge.TargetSelectionKey = relation.targetID
			} else {
				edge.TargetCharacterID = relation.targetID
			}
			identity.Relationships = append(identity.Relationships, edge)
		}
	}
	// :113 the author commit itself: the session fence is asked a second time here,
	// exactly as the original's authorize callback does, and its own error wins over
	// the commit's generic refusal.
	normalized := characterIdentityNormalize(identity)
	encoded, err := characterIdentityEncode(normalized)
	if err != nil {
		return nil, err
	}
	var fence error
	committed, err := characterIdentityCommit(ctx, conn, normalized, encoded, rosterHash(encoded), func(CharacterIdentityCommitRequest) bool {
		fence = authorize()
		return fence == nil
	})
	if err != nil {
		if fence != nil {
			return nil, fence
		}
		return nil, err
	}
	created := make(map[string]string, len(committed))
	if list, ok := committed["created"].([]any); ok {
		for _, item := range list {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			created[rosterText(row["selectionKey"])] = rosterText(row["characterId"])
		}
	}
	// :114-134 the compatibility prose and the dynamic state a roster commit owns.
	// Both are written after the identity rows so a rolled back commit can never
	// leave prose that describes rows it did not write.
	for _, entry := range request.entries {
		id := entry.characterID
		if mapped := created[id]; mapped != "" {
			id = mapped
		}
		before := original[entry.characterID]
		beforeNotes := ""
		hiddenNotes := ""
		if before != nil {
			if notes, ok := before["legacyRelationshipNotes"].(string); ok {
				beforeNotes = notes
			}
			edges, _ := before["relationships"].([]map[string]any)
			// A kept legacy prose survives only when the author left the structured
			// relationships exactly as they were, so a plain save cannot erase text
			// the card does not show.
			if beforeNotes != "" && len(edges) > 0 && rosterAuthorRelationsMatch(edges, entry.relationships) {
				hiddenNotes = beforeNotes
			}
		}
		legacyNotes := hiddenNotes
		if entry.legacyNotes != nil {
			legacyNotes = *entry.legacyNotes
		}
		if legacyNotes != beforeNotes {
			if _, err := conn.ExecContext(ctx, "UPDATE characters SET relationships=? WHERE character_id=?", legacyNotes, id); err != nil {
				return nil, err
			}
		}
		if err := rosterAuthorWriteState(ctx, conn, id, entry.currentState, before); err != nil {
			return nil, err
		}
	}
	// :135 the projection refresh and roster-meta bump.
	if err := characterIdentityRefreshProjection(ctx, conn); err != nil {
		return nil, err
	}
	// :136-147 the readback: the authoritative roster read must agree with the rows
	// this commit just wrote, entry by entry, or the commit is refused.
	snapshot, err := charactersRoster(ctx, conn)
	if err != nil {
		return nil, err
	}
	if err := rosterAuthorReadback(snapshot, committed, revision, request, original, created); err != nil {
		return nil, err
	}
	// :148-149 the receipt the browser receives.
	receipt := map[string]any{
		"operationId": request.operationID,
		"payloadHash": requestHash,
		"revision":    snapshot["revision"],
		"idempotent":  false,
		"snapshot":    snapshot,
		"created":     committed["created"],
	}
	receiptText, err := json.Marshal(receipt)
	if err != nil {
		return nil, ErrCharacterInputUnsupported
	}
	// :150-153 the saved envelope binds this receipt to the identity approval row
	// that the same transaction just wrote.
	approval, err := rrMaps(ctx, conn, "SELECT payload_hash,receipt_json FROM character_identity_approvals WHERE operation_id=?", approvalID)
	if err != nil {
		return nil, err
	}
	if len(approval) == 0 {
		// The original dereferences a missing row (a TypeError, never a success).
		return nil, ErrCharacterAuthorReceiptInvalid
	}
	saved, err := plotJSON(plotObject{
		{"version", int64(1)},
		{"kind", "author-roster"},
		{"requestHash", requestHash},
		{"receipt", string(receiptText)},
		{"receiptHash", rosterHash(string(receiptText))},
		{"approvalId", approvalID},
		{"approvalPayloadHash", rosterText(approval[0]["payload_hash"])},
		{"approvalReceiptHash", rosterHash(rosterText(approval[0]["receipt_json"]))},
	})
	if err != nil {
		return nil, ErrCharacterInputUnsupported
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO character_identity_approvals VALUES(?,?,?)", operationKey, requestHash, saved); err != nil {
		return nil, err
	}
	return receipt, nil
}

// rosterAuthorReplay ports :70-81: the exact verified receipt, the live snapshot
// and idempotent=true, or CHARACTER_AUTHOR_RECEIPT_INVALID.
func rosterAuthorReplay(ctx context.Context, conn *sql.Conn, request rosterAuthorRequest,
	requestHash, approvalID string, prior map[string]any) (map[string]any, error) {
	if rosterText(prior["payload_hash"]) != requestHash {
		return nil, ErrCharacterApprovalNonceConflict
	}
	text, ok := prior["receipt_json"].(string)
	if !ok {
		return nil, ErrCharacterAuthorReceiptInvalid
	}
	var saved rosterAuthorEnvelope
	if err := json.Unmarshal([]byte(text), &saved); err != nil {
		return nil, ErrCharacterAuthorReceiptInvalid
	}
	receipt := map[string]any{}
	if saved.Receipt != "" {
		if err := json.Unmarshal([]byte(saved.Receipt), &receipt); err != nil {
			return nil, ErrCharacterAuthorReceiptInvalid
		}
	}
	approval, err := rrMaps(ctx, conn, "SELECT payload_hash,receipt_json FROM character_identity_approvals WHERE operation_id=?", approvalID)
	if err != nil {
		return nil, err
	}
	approvalPayload, approvalReceipt := "", ""
	if len(approval) > 0 {
		approvalPayload = rosterText(approval[0]["payload_hash"])
		approvalReceipt = rosterText(approval[0]["receipt_json"])
	}
	if saved.Version != 1 || saved.Kind != "author-roster" || saved.RequestHash != requestHash ||
		saved.ReceiptHash != rosterHash(saved.Receipt) || saved.ApprovalID != approvalID ||
		rosterText(receipt["operationId"]) != request.operationID || rosterText(receipt["payloadHash"]) != requestHash ||
		len(approval) == 0 || saved.ApprovalPayloadHash != approvalPayload ||
		saved.ApprovalReceiptHash != rosterHash(approvalReceipt) {
		return nil, ErrCharacterAuthorReceiptInvalid
	}
	snapshot, err := charactersRoster(ctx, conn)
	if err != nil {
		return nil, err
	}
	result := make(map[string]any, len(receipt)+3)
	for key, value := range receipt {
		result[key] = value
	}
	result["idempotent"] = true
	result["revision"] = snapshot["revision"]
	result["snapshot"] = snapshot
	return result, nil
}

// rosterAuthorReadback ports :137-147. Every gate is a refusal, never a repair.
func rosterAuthorReadback(snapshot map[string]any, committed map[string]any, revision int64,
	request rosterAuthorRequest, original map[string]map[string]any, created map[string]string) error {
	committedRevision, ok := rrInteger(committed["revision"], 0)
	if !ok {
		return ErrCharacterAuthorReadbackFailed
	}
	identityRevision, okIdentity := rrInteger(snapshot["identityRevision"], 0)
	snapshotRevision, okSnapshot := rrInteger(snapshot["revision"], 0)
	if !okIdentity || identityRevision != committedRevision || !okSnapshot || snapshotRevision != revision+1 {
		return ErrCharacterAuthorReadbackFailed
	}
	if status := rosterText(snapshot["status"]); status != "ready" && status != "empty" {
		return ErrCharacterAuthorReadbackFailed
	}
	observed, ok := snapshot["entries"].([]map[string]any)
	if !ok || len(observed) != len(request.entries) {
		return ErrCharacterAuthorReadbackFailed
	}
	byID := make(map[string]map[string]any, len(observed))
	for _, row := range observed {
		byID[rosterText(row["characterId"])] = row
	}
	for _, entry := range request.entries {
		id := entry.characterID
		if mapped := created[id]; mapped != "" {
			id = mapped
		}
		row := byID[id]
		if row == nil {
			return ErrCharacterAuthorReadbackFailed
		}
		for index, key := range rosterAuthorFields {
			if rosterAuthorStaticChanged(row[key], entry.static[index]) {
				return ErrCharacterAuthorReadbackFailed
			}
		}
		observedKeys := make(map[string]bool)
		edges, _ := row["relationships"].([]map[string]any)
		for _, edge := range edges {
			key, err := plotJSON([]any{rosterText(row["characterId"]), rosterText(edge["targetCharacterId"]), rosterText(edge["relation"])})
			if err != nil {
				return ErrCharacterAuthorReadbackFailed
			}
			observedKeys[key] = true
		}
		expectedKeys := make(map[string]bool, len(entry.relationships))
		for _, relation := range entry.relationships {
			target := relation.targetID
			if mapped := created[target]; mapped != "" {
				target = mapped
			}
			key, err := plotJSON([]any{id, target, relation.relation})
			if err != nil {
				return ErrCharacterAuthorReadbackFailed
			}
			expectedKeys[key] = true
		}
		if len(observedKeys) != len(expectedKeys) {
			return ErrCharacterAuthorReadbackFailed
		}
		for key := range expectedKeys {
			if !observedKeys[key] {
				return ErrCharacterAuthorReadbackFailed
			}
		}
		// The expected dynamic state is the entry's own, or the state the author left
		// untouched for a character this request did not restate.
		var expected [6]string
		check := false
		if entry.currentState != nil {
			expected = entry.currentState.text
			check = true
		} else if before := original[entry.characterID]; before != nil {
			if state, ok := before["currentState"].(map[string]any); ok {
				for index, field := range characterStateFields {
					expected[index] = rosterText(state[field])
				}
				check = true
			}
		}
		if !check {
			continue
		}
		state, ok := row["currentState"].(map[string]any)
		if !ok {
			return ErrCharacterAuthorReadbackFailed
		}
		for index, field := range characterStateFields {
			if rosterAuthorStaticChanged(state[field], expected[index]) {
				return ErrCharacterAuthorReadbackFailed
			}
		}
	}
	return nil
}

// rosterAuthorWriteState ports :124-134: only the changed state fields are
// relabelled as author facts, the untouched ones keep their stored provenance in
// its stored order, and nothing is written when no fact actually moved.
func rosterAuthorWriteState(ctx context.Context, conn *sql.Conn, id string, state *rosterAuthorState, before map[string]any) error {
	if state == nil {
		return nil
	}
	var old map[string]any
	if before != nil {
		old, _ = before["currentState"].(map[string]any)
	}
	changed := make([]string, 0, len(characterStateFields))
	for index, field := range characterStateFields {
		if old == nil || rosterAuthorStaticChanged(old[field], state.text[index]) {
			changed = append(changed, field)
		}
	}
	if len(changed) == 0 && old != nil && rosterAuthorChapterEqual(old["updatedAtChapter"], state.updatedAt) {
		return nil
	}
	prior := plotObject{}
	if old != nil {
		// The stored provenance is re-read in its own JSON order, because writing a
		// re-ordered copy of untouched fields is a different fact than the original's
		// `{...oldState.provenance}`.
		ordered, err := rosterAuthorStoredProvenance(ctx, conn, id)
		if err != nil {
			return err
		}
		prior = ordered
	}
	merged := make(plotObject, 0, len(prior)+len(changed))
	seen := make(map[string]bool, len(prior))
	for _, field := range prior {
		seen[field.key] = true
		if rosterAuthorContains(changed, field.key) {
			merged = append(merged, plotField{field.key, rosterAuthorAuthorProvenance(state.updatedAt)})
			continue
		}
		merged = append(merged, field)
	}
	for _, field := range changed {
		if seen[field] {
			continue
		}
		merged = append(merged, plotField{field, rosterAuthorAuthorProvenance(state.updatedAt)})
	}
	provenance, err := plotJSON(merged)
	if err != nil {
		return err
	}
	sets := make([]string, 0, len(characterStateColumns)+2)
	args := make([]any, 0, len(characterStateColumns)+3)
	for index, column := range characterStateColumns {
		sets = append(sets, column+"=?")
		args = append(args, state.text[index])
	}
	sets = append(sets, "cs_updated_at_chapter=?", "cs_provenance=?")
	args = append(args, state.updatedAt, provenance, id)
	_, err = conn.ExecContext(ctx, "UPDATE characters SET "+strings.Join(sets, ",")+" WHERE character_id=?", args...)
	return err
}

func rosterAuthorAuthorProvenance(chapter int64) plotObject {
	return plotObject{{"kind", "author"}, {"chapterNumber", chapter}}
}

// rosterAuthorStoredProvenance re-reads cs_provenance with the ordered decoder so
// the merge preserves the stored key order of untouched fields.
func rosterAuthorStoredProvenance(ctx context.Context, conn *sql.Conn, id string) (plotObject, error) {
	rows, err := rrMaps(ctx, conn, "SELECT cs_provenance FROM characters WHERE character_id=?", id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rosterProvenance(rows[0]["cs_provenance"])
}

// rosterAuthorRelationsMatch compares the two relation lists the way the original's
// relationKeys helper does: a sorted, newline-joined list of [targetId, relation]
// pairs, so order alone can never rewrite the preserved legacy prose.
func rosterAuthorRelationsMatch(edges []map[string]any, relationships []rosterAuthorRelationship) bool {
	left := make([]string, 0, len(edges))
	for _, edge := range edges {
		key, err := plotJSON([]any{edge["targetCharacterId"], edge["relation"]})
		if err != nil {
			return false
		}
		left = append(left, key)
	}
	right := make([]string, 0, len(relationships))
	for _, relation := range relationships {
		key, err := plotJSON([]any{relation.targetID, relation.relation})
		if err != nil {
			return false
		}
		right = append(right, key)
	}
	if len(left) != len(right) {
		return false
	}
	sort.SliceStable(left, func(i, j int) bool { return rosterLess(left[i], left[j]) })
	sort.SliceStable(right, func(i, j int) bool { return rosterLess(right[i], right[j]) })
	return strings.Join(left, "\n") == strings.Join(right, "\n")
}

// rosterAuthorStaticChanged is `observed !== expected` for one stored scalar. A
// stored non-string (a number, a Buffer, NULL) is always a change, because the
// original compares the raw column value with a string.
func rosterAuthorStaticChanged(value any, expected string) bool {
	actual, ok := value.(string)
	return !ok || actual != expected
}

func rosterAuthorChapterEqual(value any, expected int64) bool {
	chapter, ok := rrInteger(value, -9007199254740991)
	return ok && chapter == expected
}

func rosterAuthorContains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

// rosterAuthorDraft is the original's draftId regexp (case-insensitive): a local
// creation selection key, never a persisted character ID.
var rosterAuthorDraft = regexp.MustCompile(`(?i)^draft:[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$`)

func rosterAuthorDraftID(id string) bool { return rosterAuthorDraft.MatchString(id) }

// rosterAuthorNormalize ports normalize (author.ts:20-60) plus the roster entry
// normalizers it calls. Every check keeps the original's position: a request that
// is not a manual_edit at schema 1 refuses before any entry is looked at, and each
// entry is fully normalized before its characterId is attached.
func rosterAuthorNormalize(candidate any) (rosterAuthorRequest, error) {
	object, ok := candidate.(map[string]any)
	if !ok {
		return rosterAuthorRequest{}, ErrCharacterAuthorRequestInvalid
	}
	operationID, _ := object["operationId"].(string)
	entries, entriesOK := object["entries"].([]any)
	expectedRevision, revisionOK := rrInteger(object["expectedRevision"], 0)
	expectedIdentity, identityOK := rrInteger(object["expectedIdentityRevision"], 0)
	if object["intent"] != "manual_edit" || !rosterAuthorSchemaVersion(object["schemaVersion"]) ||
		rosterAuthorTrim(operationID) == "" || len(utf16.Encode([]rune(operationID))) > 200 ||
		!revisionOK || !identityOK || !entriesOK ||
		phTruthy(object["generationRunHandle"]) || phTruthy(object["source"]) {
		return rosterAuthorRequest{}, ErrCharacterAuthorRequestInvalid
	}
	// expectedLegacyMarkdown !== undefined: the KEY may not be present at all, even
	// carrying null, because a legacy markdown request belongs to another lane.
	if _, present := object["expectedLegacyMarkdown"]; present {
		return rosterAuthorRequest{}, ErrCharacterAuthorRequestInvalid
	}
	normalized := make([]rosterAuthorEntry, 0, len(entries))
	for _, raw := range entries {
		entry, err := rosterAuthorEntryFrom(raw)
		if err != nil {
			return rosterAuthorRequest{}, err
		}
		normalized = append(normalized, entry)
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		return rosterLess(normalized[i].characterID, normalized[j].characterID)
	})
	for index := 1; index < len(normalized); index++ {
		if normalized[index].characterID == normalized[index-1].characterID {
			return rosterAuthorRequest{}, ErrCharacterIDDuplicate
		}
	}
	byID := make(map[string]*rosterAuthorEntry, len(normalized))
	for index := range normalized {
		byID[normalized[index].characterID] = &normalized[index]
	}
	for index := range normalized {
		entry := &normalized[index]
		seen := make(map[string]bool, len(entry.relationships))
		for _, relation := range entry.relationships {
			key, err := plotJSON([]any{relation.targetID, relation.relation})
			if err != nil {
				return rosterAuthorRequest{}, err
			}
			target, exists := byID[relation.targetID]
			if !exists || target.characterID == entry.characterID || seen[key] {
				return rosterAuthorRequest{}, ErrCharacterRelationshipInvalid
			}
			seen[key] = true
		}
		// The display snapshot of an edge is the target entry's name, resolved only
		// after every target above has been proved to be part of this same request.
		for position := range entry.relationships {
			entry.relationships[position].target = byID[entry.relationships[position].targetID].static[rosterAuthorName]
		}
	}
	if renames, present := object["renames"]; present {
		list, ok := renames.([]any)
		if !ok {
			return rosterAuthorRequest{}, ErrCharacterAuthorWriteRequired
		}
		for _, item := range list {
			rename, ok := item.(map[string]any)
			if !ok {
				return rosterAuthorRequest{}, ErrCharacterAuthorWriteRequired
			}
			characterID, _ := rename["characterId"].(string)
			target, exists := byID[characterID]
			if rosterAuthorTrim(characterID) == "" || !exists || target.static[rosterAuthorName] != rosterText(rename["newName"]) {
				return rosterAuthorRequest{}, ErrCharacterAuthorWriteRequired
			}
		}
	}
	return rosterAuthorRequest{operationID: operationID, expectedRevision: expectedRevision,
		expectedIdentityRevision: expectedIdentity, entries: normalized}, nil
}

func rosterAuthorSchemaVersion(value any) bool {
	switch number := value.(type) {
	case float64:
		return number == 1
	case int:
		return number == 1
	case int64:
		return number == 1
	}
	return false
}

func rosterAuthorTrim(text string) string { return strings.TrimFunc(text, narrativeSpace) }

func rosterAuthorNonempty(value any) bool {
	text, ok := value.(string)
	return ok && rosterAuthorTrim(text) != ""
}

// rosterAuthorRequiredText is the original requiredText: a non-string refuses with
// CHARACTER_ROSTER_TEXT_INVALID, and every accepted value is trimmed.
func rosterAuthorRequiredText(value any) (string, error) {
	text, ok := value.(string)
	if !ok {
		return "", ErrCharacterRosterTextInvalid
	}
	return rosterAuthorTrim(text), nil
}

// rosterAuthorTextOrNumber is requiredTextOrFiniteNumber, which the age field alone
// uses: a finite JS number is stringified, everything else must be a string.
func rosterAuthorTextOrNumber(value any) (string, error) {
	switch number := value.(type) {
	case float64:
		encoded, err := plotJSON(number)
		if err != nil {
			return "", ErrCharacterRosterTextInvalid
		}
		if encoded == "null" {
			// NaN and the infinities are not finite, so they fall through to the text
			// rule and refuse below exactly like the original's typeof check.
			break
		}
		return encoded, nil
	case int64:
		return phNumberString(number), nil
	case int:
		return phNumberString(int64(number)), nil
	}
	return rosterAuthorRequiredText(value)
}

// rosterAuthorEntryFrom ports the per-entry head of normalize (author.ts:36-47) and
// normalizeAuthorRosterEntry (:314-317), including the raw overwrite that restores
// the author's untrimmed strings after validation trimmed them.
func rosterAuthorEntryFrom(raw any) (rosterAuthorEntry, error) {
	object, ok := raw.(map[string]any)
	if !ok {
		// `!raw` is refused before the entry normalizer ever sees it.
		return rosterAuthorEntry{}, ErrCharacterAuthorWriteRequired
	}
	characterID, _ := object["characterId"].(string)
	relationships, relationshipsOK := object["relationships"].([]any)
	if rosterAuthorTrim(characterID) == "" || !relationshipsOK {
		return rosterAuthorEntry{}, ErrCharacterAuthorWriteRequired
	}
	entry, err := rosterAuthorEntryNormalize(object)
	if err != nil {
		return rosterAuthorEntry{}, err
	}
	entry.characterID = characterID
	// Untrimmed author strings win over the validated values for every static field
	// the request expressed as text.
	for index, key := range rosterAuthorFields {
		if text, ok := object[key].(string); ok {
			entry.static[index] = text
		}
	}
	if notes, present := object["legacyRelationshipNotes"]; present {
		text, ok := notes.(string)
		if !ok {
			return rosterAuthorEntry{}, ErrCharacterAuthorEntryInvalid
		}
		entry.legacyNotes = &text
	}
	if entry.currentState != nil {
		if rawState, ok := object["currentState"].(map[string]any); ok {
			for index, field := range characterStateFields {
				if text, ok := rawState[field].(string); ok {
					entry.currentState.text[index] = text
				}
			}
		}
	}
	edges := make([]rosterAuthorRelationship, 0, len(relationships))
	for _, item := range relationships {
		relation, ok := item.(map[string]any)
		if !ok {
			return rosterAuthorEntry{}, ErrCharacterRelationshipIDRequired
		}
		targetID, _ := relation["targetCharacterId"].(string)
		text, _ := relation["relation"].(string)
		if !rosterAuthorNonempty(targetID) || !rosterAuthorNonempty(text) {
			return rosterAuthorEntry{}, ErrCharacterRelationshipIDRequired
		}
		edges = append(edges, rosterAuthorRelationship{targetID: targetID, relation: text})
	}
	sort.SliceStable(edges, func(i, j int) bool {
		left, leftErr := plotJSON([]any{edges[i].targetID, edges[i].relation})
		right, rightErr := plotJSON([]any{edges[j].targetID, edges[j].relation})
		if leftErr != nil || rightErr != nil {
			return false
		}
		return left < right
	})
	entry.relationships = edges
	return entry, nil
}

func rosterAuthorEntryNormalize(object map[string]any) (rosterAuthorEntry, error) {
	var entry rosterAuthorEntry
	name, err := rosterAuthorRequiredText(object["name"])
	if err != nil {
		return entry, err
	}
	if name == "" {
		return entry, ErrCharacterRosterNameRequired
	}
	role, ok := object["role"].(string)
	if !ok || !rosterAuthorRoleValid(role) {
		return entry, ErrCharacterRosterRoleInvalid
	}
	entry.static[rosterAuthorName] = name
	entry.static[rosterAuthorRole] = role
	for index := rosterAuthorGender; index < len(rosterAuthorFields); index++ {
		key := rosterAuthorFields[index]
		var text string
		var err error
		if key == "age" {
			text, err = rosterAuthorTextOrNumber(object[key])
		} else {
			text, err = rosterAuthorRequiredText(object[key])
		}
		if err != nil {
			return entry, err
		}
		entry.static[index] = text
	}
	if value, present := object["currentState"]; present {
		entry.currentState, err = rosterAuthorStateFrom(value)
		if err != nil {
			return entry, err
		}
	}
	return entry, nil
}

func rosterAuthorRoleValid(role string) bool {
	switch role {
	case "protagonist", "supporting", "antagonist", "minor":
		return true
	}
	return false
}

// rosterAuthorStateFrom ports normalizeState (:117-146). An absent provenance key
// is the original's `{}` default, so a present value without its own provenance is
// still labelled legacy rather than trusted as an author fact.
func rosterAuthorStateFrom(value any) (*rosterAuthorState, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrCharacterRosterStateInvalid
	}
	updatedAt, ok := rrInteger(object["updatedAtChapter"], 0)
	if !ok {
		return nil, ErrCharacterRosterStateChapterInvalid
	}
	fields := map[string]any{}
	if raw, present := object["provenance"]; present {
		declared, ok := raw.(map[string]any)
		if !ok {
			return nil, ErrCharacterRosterStateProvenanceInvalid
		}
		fields = declared
	}
	state := &rosterAuthorState{updatedAt: updatedAt}
	state.provenance = make(plotObject, 0, len(characterStateFields))
	for index, field := range characterStateFields {
		text, err := rosterAuthorRequiredText(object[field])
		if err != nil {
			return nil, err
		}
		state.text[index] = text
		if raw, has := fields[field]; has {
			normalized, err := rosterAuthorFieldProvenance(raw)
			if err != nil {
				return nil, err
			}
			state.provenance = append(state.provenance, plotField{field, normalized})
		} else if text != "" {
			state.provenance = append(state.provenance, plotField{field, plotObject{{"kind", "legacy"}}})
		}
	}
	return state, nil
}

func rosterAuthorFieldProvenance(value any) (plotObject, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrCharacterRosterStateProvenanceInvalid
	}
	switch object["kind"] {
	case "legacy":
		return plotObject{{"kind", "legacy"}}, nil
	case "author":
		chapter, ok := rrInteger(object["chapterNumber"], 0)
		if !ok {
			return nil, ErrCharacterRosterStateChapterInvalid
		}
		return plotObject{{"kind", "author"}, {"chapterNumber", chapter}}, nil
	case "derived":
		source, err := rosterAuthorFinalizedSource(object["source"])
		if err != nil {
			return nil, err
		}
		return plotObject{{"kind", "derived"}, {"source", source}}, nil
	}
	return nil, ErrCharacterRosterStateProvenanceInvalid
}

func rosterAuthorFinalizedSource(value any) (plotObject, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, ErrCharacterRosterFinalizedSourceInvalid
	}
	finalizationID, err := rosterAuthorRequiredText(object["finalizationId"])
	if err != nil {
		return nil, err
	}
	contentHash, err := rosterAuthorRequiredText(object["contentHash"])
	if err != nil {
		return nil, err
	}
	draftID, okDraft := rrInteger(object["draftId"], 1)
	chapter, okChapter := rrInteger(object["chapterNumber"], 1)
	if !okDraft || !okChapter || finalizationID == "" || !summaryHashValid(contentHash) {
		return nil, ErrCharacterRosterFinalizedSourceInvalid
	}
	return plotObject{{"draftId", draftID}, {"finalizationId", finalizationID},
		{"chapterNumber", chapter}, {"contentHash", contentHash}}, nil
}

// rosterAuthorRequestHash is the original's
// sha256(JSON.stringify({projectId, request})) over the normalized request in its
// declared field order. A Go map cannot carry that order, so the encoder is
// explicit; it is an implementation-local canonical encoding, never a TS digest.
func rosterAuthorRequestHash(scope RosterAuthorScope, request rosterAuthorRequest) (string, error) {
	entries := make([]any, 0, len(request.entries))
	for _, entry := range request.entries {
		object := plotObject{
			{"name", entry.static[rosterAuthorName]},
			{"role", entry.static[rosterAuthorRole]},
			{"gender", entry.static[rosterAuthorGender]},
			{"age", entry.static[rosterAuthorAge]},
			{"appearance", entry.static[rosterAppearance]},
			{"personality", entry.static[rosterPersonality]},
			{"background", entry.static[rosterBackground]},
			{"abilities", entry.static[rosterAbilities]},
			{"motivation", entry.static[rosterMotivation]},
		}
		relationships := make([]any, 0, len(entry.relationships))
		for _, relation := range entry.relationships {
			relationships = append(relationships, plotObject{{"targetCharacterId", relation.targetID},
				{"target", relation.target}, {"relation", relation.relation}})
		}
		object = append(object, plotField{"relationships", relationships})
		object = append(object, plotField{"arc", entry.static[rosterArc]}, plotField{"notes", entry.static[rosterNotes]})
		if entry.currentState != nil {
			state := make(plotObject, 0, len(characterStateFields)+2)
			for index, field := range characterStateFields {
				state = append(state, plotField{field, entry.currentState.text[index]})
			}
			state = append(state, plotField{"updatedAtChapter", entry.currentState.updatedAt})
			if len(entry.currentState.provenance) > 0 {
				state = append(state, plotField{"provenance", entry.currentState.provenance})
			}
			object = append(object, plotField{"currentState", state})
		}
		if entry.legacyNotes != nil {
			object = append(object, plotField{"legacyRelationshipNotes", *entry.legacyNotes})
		}
		object = append(object, plotField{"characterId", entry.characterID})
		entries = append(entries, object)
	}
	encoded, err := plotJSON(plotObject{
		{"projectId", scope.ProjectID},
		{"request", plotObject{
			{"operationId", request.operationID},
			{"expectedRevision", request.expectedRevision},
			{"expectedIdentityRevision", request.expectedIdentityRevision},
			{"entries", entries},
		}},
	})
	if err != nil {
		return "", err
	}
	return rosterHash(encoded), nil
}
