package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The identity commit's own refusal vocabulary. Every code below exists verbatim
// in electron/repositories/character-roster-repository.ts:commitCharacterIdentities;
// none is invented and none is a transport code.
var (
	ErrCharacterApprovalRequired       = errors.New("CHARACTER_APPROVAL_REQUIRED")
	ErrCharacterApprovalMutated        = errors.New("CHARACTER_APPROVAL_MUTATED")
	ErrCharacterApprovalNonceConflict  = errors.New("CHARACTER_APPROVAL_NONCE_CONFLICT")
	ErrCharacterApprovalInvalid        = errors.New("CHARACTER_APPROVAL_INVALID")
	ErrCharacterProvenanceInvalid      = errors.New("CHARACTER_PROVENANCE_INVALID")
	ErrCharacterIDRevisionConflict     = errors.New("CHARACTER_ID_REVISION_CONFLICT")
	ErrCharacterFieldsInvalid          = errors.New("CHARACTER_FIELDS_INVALID")
	ErrCharacterIDDuplicate            = errors.New("CHARACTER_ID_DUPLICATE")
	ErrCharacterIDConflict             = errors.New("CHARACTER_ID_CONFLICT")
	ErrCharacterIDUnknown              = errors.New("CHARACTER_ID_UNKNOWN")
	ErrCharacterIDConfirmationRequired = errors.New("CHARACTER_ID_CONFIRMATION_REQUIRED")
)

// CharacterIdentityField is one ordered static field assignment. Field order is
// the caller's column order in the INSERT/UPDATE and the order inside the hashed
// payload, so a Go map (unordered) cannot carry it.
type CharacterIdentityField struct {
	Key   string
	Value string
}

// CharacterIdentitySourceRef mirrors SourceRef in src/shared/source-ref.ts.
type CharacterIdentitySourceRef struct {
	ProjectID   string
	Epoch       string
	SourceID    string
	Revision    int64
	ContentHash string
}

// CharacterIdentityProvenance mirrors CharacterStaticProvenance except that the
// legacy arm is unreachable: every action refuses it (repository :1192-1193).
type CharacterIdentityProvenance struct {
	Kind          string // author | generated | derived
	Source        CharacterIdentitySourceRef
	ModelRevision string
}

// CharacterIdentityApproval mirrors CharacterApproval in
// src/shared/character-identity.ts:135-141. Source is a pointer because the
// original distinguishes an absent source (:1185 APPROVAL_INVALID) from a present
// one whose kind is refused (:1192-1193 PROVENANCE_INVALID).
type CharacterIdentityApproval struct {
	OperationID      string
	ExpectedRevision int64
	Source           *CharacterIdentityProvenance
	Action           string
}

type CharacterIdentityChange struct {
	CharacterID string
	Fields      []CharacterIdentityField
}

type CharacterIdentityCreation struct {
	SelectionKey string
	Fields       []CharacterIdentityField
}

// CharacterIdentityCommitRequest is the ported slice of
// CharacterIdentityCommitRequest (repository :1150-1159). The remaining two
// original fields, relationships and resolutions, are deliberately absent rather
// than silently dropped: a request that carries them cannot be expressed here, so
// this type can never be used to report a commit that skipped :1240-1271. Any
// future binder must add both stages before serving db:character-roster-commit.
type CharacterIdentityCommitRequest struct {
	Approval  CharacterIdentityApproval
	Changes   []CharacterIdentityChange
	Creations []CharacterIdentityCreation
	RetireIDs []string
}

// CharacterIdentities is the identity write owner for one borrowed, already
// admitted project database. The stage map of commitCharacterIdentities:
//
//	:1173 schema gate        ported (outside any transaction)
//	:1174 payload digest     ported (implementation-local canonical encoding)
//	:1176 authorize          ported (nil callback keeps the always-deny default)
//	:1177 mutation guard     ported
//	:1178-1182 idempotency   ported
//	:1183-1184 revision CAS  ported
//	:1185-1197 approval/provenance shape ported
//	:1201 resolutions gate   unreachable (no resolutions field in the slice)
//	:1203-1209 field/ID checks ported
//	:1210-1237 creations, changes, retirements and aliases ported
//	:1238-1239 receipt + approval row ported
//	:1240-1253 relationships NOT ported
//	:1254-1271 proposal resolutions NOT ported
//	:1272 identity meta bump ported
//
// The author service's projection refresh, roster-meta bump and readback
// (electron/services/character-roster-author.ts:160-175) are a separate owner and
// are NOT ported, which is why db:character-roster-commit stays 501.
type CharacterIdentities struct{ db *sql.DB }

func NewCharacterIdentities(db *sql.DB) *CharacterIdentities { return &CharacterIdentities{db: db} }

var characterIdentityAllowedFields = map[string]bool{
	"name": true, "role": true, "gender": true, "age": true, "appearance": true,
	"personality": true, "background": true, "abilities": true, "motivation": true,
	"arc": true, "notes": true,
}

var characterIdentityAllowedSources = map[string][]string{
	"author-edit":      {"author"},
	"adopt-generated":  {"generated"},
	"adopt-import":     {"generated", "derived"},
	"confirm-identity": {"author"},
}

// CommitIdentities commits character identity rows, their aliases, retirements,
// the approval row and the identity revision. It writes nothing else: no
// projection, no roster meta, no relationships, no proposals.
func (c *CharacterIdentities) CommitIdentities(ctx context.Context, request CharacterIdentityCommitRequest,
	authorize func(CharacterIdentityCommitRequest) bool) (map[string]any, error) {
	if c == nil || c.db == nil {
		return nil, ErrProjectNotOpen
	}
	// :1173 hasCharacterIdentitySchema — table presence only, no DDL, no repair.
	existing, err := rrMaps(ctx, c.db, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='character_identity_meta'")
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 {
		return nil, ErrCharacterIDSchemaRequired
	}
	// :1174 the payload is fixed into a digest before the transaction opens. The
	// canonical order below is this port's own; it is compared only against
	// another commit produced here, never against a stored TS payload hash.
	normalized := characterIdentityNormalize(request)
	encoded, err := characterIdentityEncode(normalized)
	if err != nil {
		return nil, err
	}
	var receipt map[string]any
	err = NewReviewRevisions(c.db).immediate(ctx, func(conn *sql.Conn) error {
		var err error
		receipt, err = characterIdentityCommit(ctx, conn, normalized, encoded, rosterHash(encoded), authorize)
		return err
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

func characterIdentityCommit(ctx context.Context, conn *sql.Conn, request CharacterIdentityCommitRequest,
	encoded, digest string, authorize func(CharacterIdentityCommitRequest) bool) (map[string]any, error) {
	// :1176. The original's default callback denies everything; a nil callback is
	// the same always-deny default, never an implicit approval.
	if authorize == nil || !authorize(request) {
		return nil, ErrCharacterApprovalRequired
	}
	// :1177 re-serializes the very object being committed. Mutating the payload
	// (or its shared slices) from the authorizing callback therefore refuses.
	authorized, err := characterIdentityEncode(request)
	if err != nil {
		return nil, err
	}
	if authorized != encoded {
		return nil, ErrCharacterApprovalMutated
	}
	// :1178-1182. A replayed operation ID is either the same payload (idempotent
	// replay of the stored receipt) or a nonce conflict — never a second write.
	previous, err := rrMaps(ctx, conn, "SELECT payload_hash,receipt_json FROM character_identity_approvals WHERE operation_id=?", request.Approval.OperationID)
	if err != nil {
		return nil, err
	}
	if len(previous) > 0 {
		if stored, ok := previous[0]["payload_hash"].(string); !ok || stored != digest {
			return nil, ErrCharacterApprovalNonceConflict
		}
		return characterIdentityReceipt(previous[0]["receipt_json"], true)
	}
	// :1183-1184. A missing meta row is a TypeError in the original (an unsymbolic
	// crash), so the port reports its own meta-missing code instead of a success.
	metas, err := rrMaps(ctx, conn, "SELECT revision FROM character_identity_meta WHERE id='main'")
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, ErrCharacterIDMetaMissing
	}
	current, ok := rrInteger(metas[0]["revision"], -9007199254740991)
	if !ok || current != request.Approval.ExpectedRevision {
		return nil, ErrCharacterIDRevisionConflict
	}
	// :1185 operation ID, source presence and action membership, in that order.
	action := request.Approval.Action
	if request.Approval.OperationID == "" || request.Approval.Source == nil {
		return nil, ErrCharacterApprovalInvalid
	}
	if _, known := characterIdentityAllowedSources[action]; !known {
		return nil, ErrCharacterApprovalInvalid
	}
	// :1192-1193. 'legacy' can never be attributed by a commit, and every other
	// kind must be allowed for this action.
	if !characterIdentitySourceAllowed(action, request.Approval.Source.Kind) {
		return nil, ErrCharacterProvenanceInvalid
	}
	// :1194-1197 the source reference and, for generated/derived, its model hash.
	provenance := *request.Approval.Source
	if !characterIdentityRefValid(provenance.Source) ||
		(provenance.Kind == "generated" || provenance.Kind == "derived") && !summaryHashValid(provenance.ModelRevision) {
		return nil, ErrCharacterProvenanceInvalid
	}
	commandRevision := current + 1
	attributed := characterIdentityAttributedObject(provenance, request.Approval.OperationID)
	source, err := plotJSON(attributed)
	if err != nil {
		return nil, err
	}
	// :1201 `resolutions.length && action !== confirm-identity` is unreachable
	// here: the slice request cannot carry a resolution, so no resolution can be
	// dropped and CHARACTER_ID_CONFIRMATION_REQUIRED can never be earned. That code
	// stays declared so a future port cannot quietly forget the gate.
	// :1207-1208 uniqueness of change IDs, then creation keys, then retire IDs.
	if err := characterIdentityUnique(characterIdentityChangeIDs(request.Changes)); err != nil {
		return nil, err
	}
	if err := characterIdentityUnique(characterIdentityCreationKeys(request.Creations)); err != nil {
		return nil, err
	}
	if err := characterIdentityUnique(request.RetireIDs); err != nil {
		return nil, err
	}
	// :1209 a character cannot be changed and retired in one commit.
	retiring := make(map[string]bool, len(request.RetireIDs))
	for _, id := range request.RetireIDs {
		retiring[id] = true
	}
	for _, change := range request.Changes {
		if retiring[change.CharacterID] {
			return nil, ErrCharacterIDConflict
		}
	}
	// :1215-1222 creations insert the identity row and its first alias.
	created := make([]any, 0, len(request.Creations))
	for _, creation := range request.Creations {
		if err := characterIdentityValidateFields(creation.Fields); err != nil {
			return nil, err
		}
		// :1217 the name is required, not merely present-and-legal.
		name, named := characterIdentityFieldValue(creation.Fields, "name")
		if !named {
			return nil, ErrCharacterFieldsInvalid
		}
		id, err := characterIdentityUUID()
		if err != nil {
			return nil, err
		}
		if err := characterIdentityInsert(ctx, conn, id, source, commandRevision, creation.Fields); err != nil {
			return nil, err
		}
		if err := characterIdentityOpenAlias(ctx, conn, id, name, request.Approval.OperationID, commandRevision); err != nil {
			return nil, err
		}
		created = append(created, plotObject{{"selectionKey", creation.SelectionKey}, {"characterId", id}})
	}
	// :1223-1236 changes: the active lookup precedes field validation, so an
	// unknown or retired ID wins over an illegal field for the same change.
	for _, change := range request.Changes {
		row, err := characterIdentityActive(ctx, conn, change.CharacterID)
		if err != nil {
			return nil, err
		}
		if err := characterIdentityValidateFields(change.Fields); err != nil {
			return nil, err
		}
		if len(change.Fields) > 0 {
			// Field provenance is merged so adopting generated text never relabels
			// untouched author fields (:1227-1229).
			merged, err := characterIdentityMergeProvenance(row["static_provenance"], change.Fields, attributed)
			if err != nil {
				return nil, err
			}
			if err := characterIdentityUpdate(ctx, conn, change.CharacterID, change.Fields, merged, commandRevision); err != nil {
				return nil, err
			}
		}
		// :1232-1235 a genuine rename closes the old alias at the previous
		// revision and opens the new one at the committed revision.
		if name, renamed := characterIdentityFieldValue(change.Fields, "name"); renamed && name != rosterText(row["name"]) {
			if err := characterIdentityCloseAliases(ctx, conn, change.CharacterID, current); err != nil {
				return nil, err
			}
			if err := characterIdentityOpenAlias(ctx, conn, change.CharacterID, name, request.Approval.OperationID, commandRevision); err != nil {
				return nil, err
			}
		}
	}
	// :1237 retirements close the identity and every open alias, in ID order.
	for _, id := range request.RetireIDs {
		if _, err := characterIdentityActive(ctx, conn, id); err != nil {
			return nil, err
		}
		if _, err := conn.ExecContext(ctx, "UPDATE characters SET retired=1,identity_revision=? WHERE character_id=?", commandRevision, id); err != nil {
			return nil, err
		}
		if err := characterIdentityCloseAliases(ctx, conn, id, current); err != nil {
			return nil, err
		}
	}
	// :1238-1239. The receipt is written first and the meta bump last, exactly as
	// in the original, so a failed meta bump can never leave a silent success.
	receiptJSON, err := plotJSON(plotObject{
		{"approval", characterIdentityApprovalObject(request.Approval)},
		{"operationId", request.Approval.OperationID},
		{"revision", commandRevision},
		{"created", created},
		{"idempotent", false},
	})
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "INSERT INTO character_identity_approvals VALUES(?,?,?)", request.Approval.OperationID, digest, receiptJSON); err != nil {
		return nil, err
	}
	// The original writes relationships and proposal resolutions here. They are
	// not ported, and nothing in this file pretends otherwise.
	// :1272
	if _, err := conn.ExecContext(ctx, "UPDATE character_identity_meta SET revision=? WHERE id='main'", commandRevision); err != nil {
		return nil, err
	}
	return characterIdentityReceipt(receiptJSON, false)
}

// Normalization stands in for building the JS request object before the commit is
// called: a repeated field key keeps its first position and its last value, which
// is exactly what `{a:1,b:2,a:3}` becomes once parsed. It also rebuilds the slices
// so a caller keeping a handle on its own request cannot edit the payload that was
// hashed; the authorizing callback is handed this value, whose slices it does
// share, so :1177 still sees anything that callback changes.
func characterIdentityNormalize(request CharacterIdentityCommitRequest) CharacterIdentityCommitRequest {
	out := CharacterIdentityCommitRequest{Approval: request.Approval}
	out.Changes = make([]CharacterIdentityChange, 0, len(request.Changes))
	for _, change := range request.Changes {
		out.Changes = append(out.Changes, CharacterIdentityChange{CharacterID: change.CharacterID, Fields: characterIdentityDedupe(change.Fields)})
	}
	out.Creations = make([]CharacterIdentityCreation, 0, len(request.Creations))
	for _, creation := range request.Creations {
		out.Creations = append(out.Creations, CharacterIdentityCreation{SelectionKey: creation.SelectionKey, Fields: characterIdentityDedupe(creation.Fields)})
	}
	out.RetireIDs = append([]string{}, request.RetireIDs...)
	return out
}

func characterIdentityDedupe(fields []CharacterIdentityField) []CharacterIdentityField {
	out := make([]CharacterIdentityField, 0, len(fields))
	positions := make(map[string]int, len(fields))
	for _, field := range fields {
		if position, seen := positions[field.Key]; seen {
			out[position].Value = field.Value
			continue
		}
		positions[field.Key] = len(out)
		out = append(out, field)
	}
	return out
}

// characterIdentityEncode is the authorized payload: the original request shape in
// a fixed order (declared order of the TS interfaces, with the unported remainder
// pinned to empty). Same request, same bytes; any edit, different bytes.
func characterIdentityEncode(request CharacterIdentityCommitRequest) (string, error) {
	changes := make([]any, 0, len(request.Changes))
	for _, change := range request.Changes {
		changes = append(changes, plotObject{{"characterId", change.CharacterID}, {"fields", characterIdentityFieldsObject(change.Fields)}})
	}
	creations := make([]any, 0, len(request.Creations))
	for _, creation := range request.Creations {
		creations = append(creations, plotObject{{"selectionKey", creation.SelectionKey}, {"fields", characterIdentityFieldsObject(creation.Fields)}})
	}
	retired := make([]any, 0, len(request.RetireIDs))
	for _, id := range request.RetireIDs {
		retired = append(retired, id)
	}
	return plotJSON(plotObject{
		{"approval", characterIdentityApprovalObject(request.Approval)},
		{"changes", changes},
		{"creations", creations},
		{"retireIds", retired},
		{"relationships", []any{}},
		{"resolutions", []any{}},
	})
}

func characterIdentityApprovalObject(approval CharacterIdentityApproval) plotObject {
	out := plotObject{{"operationId", approval.OperationID}, {"expectedRevision", approval.ExpectedRevision}}
	if approval.Source != nil {
		out = append(out, plotField{"source", characterIdentityAttributedObject(*approval.Source, "")})
	}
	return append(out, plotField{"action", approval.Action})
}

func characterIdentityAttributedObject(provenance CharacterIdentityProvenance, operationID string) plotObject {
	out := plotObject{
		{"kind", provenance.Kind},
		{"source", plotObject{
			{"projectId", provenance.Source.ProjectID},
			{"epoch", provenance.Source.Epoch},
			{"sourceId", provenance.Source.SourceID},
			{"revision", provenance.Source.Revision},
			{"contentHash", provenance.Source.ContentHash},
		}},
	}
	if provenance.ModelRevision != "" {
		out = append(out, plotField{"modelRevision", provenance.ModelRevision})
	}
	if operationID != "" {
		out = append(out, plotField{"approvalId", operationID})
	}
	return out
}

func characterIdentityFieldsObject(fields []CharacterIdentityField) plotObject {
	out := make(plotObject, 0, len(fields))
	for _, field := range fields {
		out = append(out, plotField{field.Key, field.Value})
	}
	return out
}

func characterIdentityFieldValue(fields []CharacterIdentityField, key string) (string, bool) {
	for _, field := range fields {
		if field.Key == key {
			return field.Value, true
		}
	}
	return "", false
}

func characterIdentitySourceAllowed(action, kind string) bool {
	for _, allowed := range characterIdentityAllowedSources[action] {
		if allowed == kind {
			return true
		}
	}
	return false
}

func characterIdentityRefValid(ref CharacterIdentitySourceRef) bool {
	return strings.TrimFunc(ref.ProjectID, narrativeSpace) != "" &&
		strings.TrimFunc(ref.Epoch, narrativeSpace) != "" &&
		strings.TrimFunc(ref.SourceID, narrativeSpace) != "" &&
		ref.Revision >= 0 && ref.Revision <= 9007199254740991 &&
		summaryHashValid(ref.ContentHash)
}

func characterIdentityValidateFields(fields []CharacterIdentityField) error {
	for _, field := range fields {
		if !characterIdentityAllowedFields[field.Key] {
			return ErrCharacterFieldsInvalid
		}
		// ECMAScript trim, so a name of only FEFF is empty and one of only NEL is not.
		if field.Key == "name" && strings.TrimFunc(field.Value, narrativeSpace) == "" {
			return ErrCharacterFieldsInvalid
		}
	}
	return nil
}

func characterIdentityUnique(values []string) error {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			return ErrCharacterIDDuplicate
		}
		seen[value] = true
	}
	return nil
}

func characterIdentityChangeIDs(changes []CharacterIdentityChange) []string {
	out := make([]string, 0, len(changes))
	for _, change := range changes {
		out = append(out, change.CharacterID)
	}
	return out
}

func characterIdentityCreationKeys(creations []CharacterIdentityCreation) []string {
	out := make([]string, 0, len(creations))
	for _, creation := range creations {
		out = append(out, creation.SelectionKey)
	}
	return out
}

func characterIdentityActive(ctx context.Context, conn *sql.Conn, id string) (map[string]any, error) {
	rows, err := rrMaps(ctx, conn, "SELECT character_id,name,static_provenance FROM characters WHERE character_id=? AND retired=0", id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrCharacterIDUnknown
	}
	return rows[0], nil
}

func characterIdentityInsert(ctx context.Context, conn *sql.Conn, id, source string, revision int64, fields []CharacterIdentityField) error {
	columns := make([]string, 0, len(fields)+3)
	marks := make([]string, 0, len(fields)+3)
	args := make([]any, 0, len(fields)+3)
	columns = append(columns, "character_id", "static_provenance", "identity_revision")
	marks = append(marks, "?", "?", "?")
	args = append(args, id, source, revision)
	for _, field := range fields {
		columns = append(columns, field.Key)
		marks = append(marks, "?")
		args = append(args, field.Value)
	}
	_, err := conn.ExecContext(ctx, "INSERT INTO characters("+strings.Join(columns, ",")+") VALUES("+strings.Join(marks, ",")+")", args...)
	return err
}

func characterIdentityUpdate(ctx context.Context, conn *sql.Conn, id string, fields []CharacterIdentityField, provenance string, revision int64) error {
	sets := make([]string, 0, len(fields)+2)
	args := make([]any, 0, len(fields)+3)
	for _, field := range fields {
		sets = append(sets, field.Key+"=?")
		args = append(args, field.Value)
	}
	sets = append(sets, "static_provenance=?", "identity_revision=?")
	args = append(args, provenance, revision, id)
	_, err := conn.ExecContext(ctx, "UPDATE characters SET "+strings.Join(sets, ",")+" WHERE character_id=?", args...)
	return err
}

func characterIdentityOpenAlias(ctx context.Context, conn *sql.Conn, id, name, operationID string, revision int64) error {
	_, err := conn.ExecContext(ctx, "INSERT INTO character_aliases VALUES(?,?,?,?,NULL)", id, name, operationID, revision)
	return err
}

func characterIdentityCloseAliases(ctx context.Context, conn *sql.Conn, id string, revision int64) error {
	_, err := conn.ExecContext(ctx, "UPDATE character_aliases SET valid_through=? WHERE character_id=? AND valid_through IS NULL", revision, id)
	return err
}

// characterIdentityMergeProvenance reproduces
// `{...prior, fields: {...prior.fields, ...attributed}}` (:1228-1229). Key order is
// the stored JSON's own order with integer-like keys first, as JSON.parse
// enumerates them, so the ordered decoder is mandatory: map iteration would write
// a different static_provenance for identical facts.
//
// Deliberate narrowing: only an object prior (and an object or absent prior.fields)
// is merged. The original would spread an array or a string into index properties;
// no commit here can produce such a row, so the port refuses instead of guessing.
func characterIdentityMergeProvenance(raw any, fields []CharacterIdentityField, attributed plotObject) (string, error) {
	text, ok := raw.(string)
	if !ok {
		return "", ErrCharacterInputUnsupported
	}
	// A lone escaped surrogate is valid JSON but has no UTF-8 form, and Go's
	// decoder would rewrite it to U+FFFD. Refuse rather than mutate the value.
	if err := summaryJSONSupported(text); err != nil {
		return "", ErrCharacterInputUnsupported
	}
	if !json.Valid([]byte(text)) {
		return "", ErrCharacterInputUnsupported
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	value, err := rosterJSONValue(decoder)
	if err != nil {
		return "", ErrCharacterInputUnsupported
	}
	prior, ok := value.(plotObject)
	if !ok {
		return "", ErrCharacterInputUnsupported
	}
	merged := make(plotObject, 0, len(prior)+1)
	placed := false
	for _, entry := range prior {
		if entry.key != "fields" {
			merged = append(merged, entry)
			continue
		}
		existing := plotObject{}
		if entry.value != nil {
			object, ok := entry.value.(plotObject)
			if !ok {
				return "", ErrCharacterInputUnsupported
			}
			existing = object
		}
		merged = append(merged, plotField{"fields", characterIdentityMergedFields(existing, fields, attributed)})
		placed = true
	}
	if !placed {
		merged = append(merged, plotField{"fields", characterIdentityMergedFields(plotObject{}, fields, attributed)})
	}
	return plotJSON(merged)
}

func characterIdentityMergedFields(existing plotObject, fields []CharacterIdentityField, attributed plotObject) plotObject {
	changed := make(map[string]bool, len(fields))
	for _, field := range fields {
		changed[field.Key] = true
	}
	merged := make(plotObject, 0, len(existing)+len(fields))
	seen := make(map[string]bool, len(existing))
	for _, entry := range existing {
		seen[entry.key] = true
		if changed[entry.key] {
			merged = append(merged, plotField{entry.key, attributed})
			continue
		}
		merged = append(merged, entry)
	}
	for _, field := range fields {
		if seen[field.Key] {
			continue
		}
		merged = append(merged, plotField{field.Key, attributed})
	}
	return merged
}

// characterIdentityReceipt decodes the stored receipt so a replay and a fresh
// commit return exactly the same value. `idempotent` is always set, mirroring
// `{...JSON.parse(receipt_json), idempotent:true}`.
func characterIdentityReceipt(raw any, idempotent bool) (map[string]any, error) {
	text, ok := raw.(string)
	if !ok {
		return nil, ErrCharacterInputUnsupported
	}
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return nil, ErrCharacterInputUnsupported
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		// Spreading null or undefined yields an empty object; every other shape
		// (array, string, number) is not a receipt this port ever wrote.
		if decoded != nil {
			return nil, ErrCharacterInputUnsupported
		}
		object = map[string]any{}
	}
	receipt := make(map[string]any, len(object)+1)
	for key, item := range object {
		receipt[key] = item
	}
	receipt["idempotent"] = idempotent
	return receipt, nil
}

// RFC 4122 version 4, the shape node's randomUUID produces for a new identity.
func characterIdentityUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}
