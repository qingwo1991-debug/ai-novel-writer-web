package store

// Native port of the missing finalized-character read path:
// SummaryRepository.readFinalizedCharacterContext
// (electron/repositories/summary-repository.ts:401-424) over
// readFinalizedSourceFromDb (:276-322), readFrozenCharacterSnapshot (:63-77) and
// stateSnapshot (:78-88).
//
// Every function below is a read. It borrows the caller's read transaction, or
// opens one with BeginTx(ReadOnly: true) exactly like Drafts.list (drafts.go:123)
// and Narrative.List (narrative.go:239). It issues SELECTs only, never opens an
// immediate/write transaction, and never mutates the prose, the finalization
// receipt, the freeze receipt or the character row it projects. A later bind and
// the state commit re-read this same context and compare it
// (electron/services/finalized-character-generation.ts:58 and
// summary-repository.ts:429), so the returned shape is a contract.

import (
	"context"
	"database/sql"
	"errors"
)

var (
	// summary-repository.ts:402
	//   if (!database || !scope.projectId?.trim() || !scope.epoch?.trim())
	//     throw new Error('FINALIZED_CHARACTER_SCOPE_REQUIRED')
	ErrFinalizedCharacterScopeRequired = errors.New("FINALIZED_CHARACTER_SCOPE_REQUIRED")
	// summary-repository.ts:406
	//   if (!source) throw new Error('FINALIZED_CHARACTER_SOURCE_CHANGED')
	// summary-repository.ts:409
	//   if (!latest || latest.id !== draftId) throw new Error('FINALIZED_CHARACTER_SOURCE_CHANGED')
	ErrFinalizedCharacterSourceChanged = errors.New("FINALIZED_CHARACTER_SOURCE_CHANGED")
	// summary-repository.ts:413
	//   if (!row) throw new Error('FINALIZED_CHARACTER_ID_RETIRED')
	ErrFinalizedCharacterIDRetired = errors.New("FINALIZED_CHARACTER_ID_RETIRED")
	// summary-repository.ts:84
	//   if (!Number.isSafeInteger(revision) || revision < 0)
	//     throw new Error('FINALIZED_CHARACTER_FIELD_REVISION_INVALID')
	ErrFinalizedCharacterFieldRevisionInvalid = errors.New("FINALIZED_CHARACTER_FIELD_REVISION_INVALID")
	// Port-only precondition, not a reference refusal: the reference is always
	// handed a database handle, so summary-repository.ts has no code for a missing
	// transaction. This is the same boundary shape already published as
	// DRAFT_READ_TRANSACTION_REQUIRED (drafts_read_tx.go:14).
	ErrFinalizedCharacterReadTransactionRequired = errors.New("FINALIZED_CHARACTER_READ_TRANSACTION_REQUIRED")
)

// The frozen snapshot's own refusal (summary-repository.ts:75,
// 'FINALIZED_CHARACTER_SNAPSHOT_INVALID') is already published as
// ErrSummaryCharacterSnapshotInvalid (summary.go:22) with exactly that string;
// this port reuses that value instead of defining a second, non-equal error.

// identityStatus is 'bound' only when a freeze receipt was found (:417).
const (
	FinalizedCharacterIdentityBound  = "bound"
	FinalizedCharacterIdentityLegacy = "legacy"
)

// FinalizedCharacterScope is ProjectEpoch (src/shared/source-ref.ts:5). The
// reference only trims it to refuse a blank scope (:402); the raw, untrimmed
// values are what the context and its continuity epoch carry (:416-419).
type FinalizedCharacterScope struct {
	ProjectID string
	Epoch     string
}

// FinalizedCharacterSourceIdentity mirrors FinalizedSourceIdentity
// (src/shared/finalized-continuity.ts:12-17) in the reference's key order, so a
// later digest of the context sees draftId, finalizationId, chapterNumber,
// contentHash in that order.
type FinalizedCharacterSourceIdentity struct {
	DraftID        int64  `json:"draftId"`
	FinalizationID string `json:"finalizationId"`
	ChapterNumber  int64  `json:"chapterNumber"`
	ContentHash    string `json:"contentHash"`
}

// FinalizedCharacterSourceOrder mirrors DerivedSourceOrder
// (src/shared/character-identity.ts:33-35) in the reference's key order
// (continuityEpoch, chapterNumber, authoritativeFinalizationRevision, :418-419).
type FinalizedCharacterSourceOrder struct {
	ContinuityEpoch                   string `json:"continuityEpoch"`
	ChapterNumber                     int64  `json:"chapterNumber"`
	AuthoritativeFinalizationRevision int64  `json:"authoritativeFinalizationRevision"`
}

// FinalizedCharacterFieldSnapshot mirrors CharacterFieldSnapshot
// (src/shared/character-identity.ts:36-39) exactly as stateSnapshot (:86-87)
// builds it. Field order is the struct's, which is the reference's literal order
// and therefore the order a JSON digest of the context observes.
type FinalizedCharacterFieldSnapshot struct {
	ProjectID   string `json:"projectId"`
	Epoch       string `json:"epoch"`
	CharacterID string `json:"characterId"`
	Field       string `json:"field"`
	Revision    int64  `json:"revision"`
	Value       string `json:"value"`
	ValueHash   string `json:"valueHash"`
	// Provenance is the :81-82 union: {"kind":"author","chapterNumber":n},
	// {"kind":"derived","source":{...}} or {"kind":"legacy"}. It is copied, never
	// normalized, so a stored value that no writer in this repo produces survives
	// to the commit's own checks. JavaScript key order is kind-then-payload; a Go
	// map is unordered and no consumer reads this by order (the commit compares
	// provenance by kind and by the four identity fields, character-identity.ts:
	// 47-50), so the order is not observable.
	Provenance map[string]any `json:"provenance"`
	// SourceOrder is the stored provenance object's sourceOrder, verbatim
	// (structuredClone, :87), or nil when the stored provenance has no truthy one.
	// Typed as any because the reference clones whatever was stored: a truthy
	// non-object must stay visible so a later decideDerivedPatch port can treat it
	// as a source conflict (character-identity.ts:82-83) instead of seeing an
	// absent order.
	SourceOrder any `json:"sourceOrder,omitempty"`
}

// FinalizedCharacterContextCharacter is one frozen FinalizedCharacterReference
// spread with its six state fields (:414). The sole freeze writer
// (finalization_outbox.go:333-341) emits exactly characterId,
// displayNameSnapshot, aliases, so the spread carries no further key here.
type FinalizedCharacterContextCharacter struct {
	CharacterID         string                            `json:"characterId"`
	DisplayNameSnapshot string                            `json:"displayNameSnapshot"`
	Aliases             []string                          `json:"aliases"`
	Fields              []FinalizedCharacterFieldSnapshot `json:"fields"`
}

// FinalizedCharacterContext mirrors FinalizedCharacterContext
// (src/shared/finalized-continuity.ts:113-124) in the reference's return order
// (:416-419). Characters is always a list; an absent freeze receipt is not an
// error, it is IdentityStatus legacy with IdentityRevision 0.
type FinalizedCharacterContext struct {
	ProjectID            string                               `json:"projectId"`
	Epoch                string                               `json:"epoch"`
	Source               FinalizedCharacterSourceIdentity     `json:"source"`
	Content              string                               `json:"content"`
	ProjectionGeneration int64                                `json:"projectionGeneration"`
	IdentityRevision     int64                                `json:"identityRevision"`
	IdentityStatus       string                               `json:"identityStatus"`
	SourceOrder          FinalizedCharacterSourceOrder        `json:"sourceOrder"`
	Characters           []FinalizedCharacterContextCharacter `json:"characters"`
}

// finalizedCharacterSource is the value of readFinalizedSourceFromDb (:311-321).
// ChapterTitle is produced there (:321) but FinalizedCharacterContext does not
// carry it (:416-419); it is kept so the port of that helper stays complete.
type finalizedCharacterSource struct {
	Identity             FinalizedCharacterSourceIdentity
	ChapterTitle         string
	Content              string
	ProjectionGeneration int64
}

// finalizedCharacterFrozen is the value of readFrozenCharacterSnapshot (:76).
type finalizedCharacterFrozen struct {
	IdentityRevision int64
	Characters       []finalizedCharacterFrozenCharacter
}

// finalizedCharacterFrozenCharacter is one validated entry of the freeze receipt.
type finalizedCharacterFrozenCharacter struct {
	CharacterID         string
	DisplayNameSnapshot string
	Aliases             []string
}

// finalizedCharacterLatest is the :407-409 "latest finalized draft of that
// chapter" row, whose version becomes the authoritative finalization revision.
type finalizedCharacterLatest struct {
	DraftID int64
	Version int64
}

// FinalizedCharacters borrows an admitted project database for the
// finalized-character read path. It owns no schema, opens no write transaction,
// and never re-enters the pool from inside the read.
type FinalizedCharacters struct{ db *sql.DB }

func NewFinalizedCharacters(db *sql.DB) *FinalizedCharacters {
	return &FinalizedCharacters{db: db}
}

// ReadContext is the same read on its own read-only transaction. A missing
// database is the reference's first refusal arm (:402 `!database`), not an empty
// context.
func (f *FinalizedCharacters) ReadContext(ctx context.Context, draftID int64, scope FinalizedCharacterScope) (*FinalizedCharacterContext, error) {
	if f == nil || f.db == nil {
		return nil, ErrFinalizedCharacterScopeRequired
	}
	// All rows below share one SQLite snapshot; a concurrent supersession cannot
	// produce a hybrid context. Never re-enter f.db inside the transaction.
	tx, err := f.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	result, err := ReadFinalizedCharacterContextTx(ctx, tx, draftID, scope)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}

// ReadFinalizedCharacterContextTx is SummaryRepository.readFinalizedCharacterContext
// (:401-424) over a caller-owned, already-admitted read transaction. It never
// opens a transaction and never writes, so it also runs on a connection that is
// already inside a read transaction (for example a mode=ro project handle).
func ReadFinalizedCharacterContextTx(ctx context.Context, tx *sql.Tx, draftID int64, scope FinalizedCharacterScope) (*FinalizedCharacterContext, error) {
	if ctx == nil || tx == nil {
		return nil, ErrFinalizedCharacterReadTransactionRequired
	}
	// summary-repository.ts:402 if (!database || !scope.projectId?.trim() ||
	// scope.epoch?.trim()) throw new Error('FINALIZED_CHARACTER_SCOPE_REQUIRED');
	// scope is answered before anything is read.
	if summaryTrim(scope.ProjectID) == "" || summaryTrim(scope.Epoch) == "" {
		return nil, ErrFinalizedCharacterScopeRequired
	}
	// summary-repository.ts:405 const source = readFinalizedSourceFromDb(db, draftId)
	source, err := finalizedCharacterReadSource(ctx, tx, draftID)
	if err != nil {
		return nil, err
	}
	// summary-repository.ts:406 if (!source) throw new Error('FINALIZED_CHARACTER_SOURCE_CHANGED')
	if source == nil {
		return nil, ErrFinalizedCharacterSourceChanged
	}
	// summary-repository.ts:407-409 the latest finalized draft of that chapter
	// must be this exact draftId.
	latest, err := finalizedCharacterLatestFinalized(ctx, tx, source.Identity.ChapterNumber)
	if err != nil {
		return nil, err
	}
	if latest == nil || latest.DraftID != draftID {
		return nil, ErrFinalizedCharacterSourceChanged
	}
	// summary-repository.ts:410 const frozen = readFrozenCharacterSnapshot(db, source.source)
	frozen, err := finalizedCharacterReadFrozen(ctx, tx, source.Identity)
	if err != nil {
		return nil, err
	}
	// summary-repository.ts:411-415 the active row of every frozen character; a
	// missing (retired) row refuses the whole read.
	characters := []FinalizedCharacterContextCharacter{}
	if frozen != nil {
		for _, character := range frozen.Characters {
			row, err := finalizedCharacterActiveRow(ctx, tx, character.CharacterID)
			if err != nil {
				return nil, err
			}
			if row == nil {
				return nil, ErrFinalizedCharacterIDRetired
			}
			// CHARACTER_STATE_TEXT_FIELDS (src/shared/character-roster.ts:44), the
			// same six names and order the store already encodes as
			// characterStateFields/characterStateColumns (characters.go:31-32).
			// character-proposal-parser.ts:94 repeats the identical set; the roster
			// one is authoritative here.
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
	// summary-repository.ts:417 identityRevision: frozen?.identityRevision ?? 0,
	// identityStatus: frozen ? 'bound' : 'legacy'
	identityRevision, identityStatus := int64(0), FinalizedCharacterIdentityLegacy
	if frozen != nil {
		identityRevision, identityStatus = frozen.IdentityRevision, FinalizedCharacterIdentityBound
	}
	// summary-repository.ts:418-419 sourceOrder: continuityEpoch is
	// `${scope.projectId}:${source.projectionGeneration}`, a JavaScript template
	// literal, so the generation is String(number).
	order := FinalizedCharacterSourceOrder{
		ContinuityEpoch:                   scope.ProjectID + ":" + phNumberString(source.ProjectionGeneration),
		ChapterNumber:                     source.Identity.ChapterNumber,
		AuthoritativeFinalizationRevision: latest.Version,
	}
	// summary-repository.ts:416-419
	return &FinalizedCharacterContext{
		ProjectID:            scope.ProjectID,
		Epoch:                scope.Epoch,
		Source:               source.Identity,
		Content:              source.Content,
		ProjectionGeneration: source.ProjectionGeneration,
		IdentityRevision:     identityRevision,
		IdentityStatus:       identityStatus,
		SourceOrder:          order,
		Characters:           characters,
	}, nil
}

// finalizedCharacterReadSource is readFinalizedSourceFromDb (:276-322).
func finalizedCharacterReadSource(ctx context.Context, q rrQueryer, draftID int64) (*finalizedCharacterSource, error) {
	// :280-292 one statement, one snapshot.
	rows, err := rrMaps(ctx, q, `SELECT d.id AS draftId,d.chapter_number AS chapterNumber,d.status,
 c.body AS content,o.finalization_id AS finalizationId,o.chapter_title AS chapterTitle,
 o.content_hash AS contentHash,o.content_snapshot AS contentSnapshot,
 m.generation AS projectionGeneration
 FROM drafts d JOIN contents c ON c.id=d.content_id JOIN finalization_outbox o ON o.draft_id=d.id
 JOIN continuity_projection_meta m ON m.id='main' WHERE d.id=?`, draftID)
	if err != nil {
		return nil, err
	}
	// The reference is `.get(...)`, i.e. the first row and no ORDER BY.
	// finalization_outbox.draft_id is UNIQUE (baseline_v7_ddl.go:220), so at most
	// one row can match and the unordered first row is the only row.
	// :304 !row
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	// :305 row.status !== 'finalized'
	if status, ok := row["status"].(string); !ok || status != "finalized" {
		return nil, nil
	}
	// :306 !row.finalizationId.trim() (a non-text finalizationId would throw a
	// TypeError in the reference; it is the same null here).
	finalizationID, finalizationOK := row["finalizationId"].(string)
	if !finalizationOK || summaryTrim(finalizationID) == "" {
		return nil, nil
	}
	// :307 !/^[a-f0-9]{64}$/u.test(row.contentHash)
	if !summaryHashValid(row["contentHash"]) {
		return nil, nil
	}
	// :308 row.content !== row.contentSnapshot
	content, contentOK := row["content"].(string)
	snapshot, snapshotOK := row["contentSnapshot"].(string)
	if !contentOK || !snapshotOK || content != snapshot {
		return nil, nil
	}
	// :309 sha256(row.contentSnapshot) !== row.contentHash
	if summaryHash(snapshot) != row["contentHash"] {
		return nil, nil
	}
	draft, draftOK := rrInteger(row["draftId"], 0)
	chapter, chapterOK := rrInteger(row["chapterNumber"], 0)
	generation, generationOK := rrInteger(row["projectionGeneration"], 0)
	if !draftOK || !chapterOK || !generationOK {
		return nil, nil
	}
	chapterTitle, _ := row["chapterTitle"].(string)
	// :311-321 content is the frozen snapshot, not the live contents row; the
	// source identity is the receipt's own text.
	identity := FinalizedCharacterSourceIdentity{
		DraftID:        draft,
		FinalizationID: finalizationID,
		ChapterNumber:  chapter,
		ContentHash:    summaryText(row["contentHash"]),
	}
	return &finalizedCharacterSource{
		Identity:             identity,
		ChapterTitle:         chapterTitle,
		Content:              snapshot,
		ProjectionGeneration: generation,
	}, nil
}

// finalizedCharacterHasIdentitySchema is hasCharacterIdentitySchema
// (electron/repositories/character-repository.ts:8-10); summary.go:514 publishes
// the same probe bound to *sql.Conn. A database without the M02 tables has no
// freeze receipts at all, so every read is the legacy arm.
func finalizedCharacterHasIdentitySchema(ctx context.Context, q rrQueryer) (bool, error) {
	rows, err := rrMaps(ctx, q, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='character_identity_meta'")
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}

// finalizedCharacterSourceMatches is sameSource (:265-270): all four identity
// fields with the reference's strict equality, so a JSON number and the stored
// INTEGER compare equal while text never matches a number.
func finalizedCharacterSourceMatches(value any, source FinalizedCharacterSourceIdentity) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return rrEqual(object["draftId"], source.DraftID) &&
		rrEqual(object["finalizationId"], source.FinalizationID) &&
		rrEqual(object["chapterNumber"], source.ChapterNumber) &&
		rrEqual(object["contentHash"], source.ContentHash)
}

// finalizedCharacterReadFrozen is readFrozenCharacterSnapshot (:63-77). A nil
// result is the reference's null: no identity schema, no receipt, or no receipt
// for this finalization. Everything else is FINALIZED_CHARACTER_SNAPSHOT_INVALID.
func finalizedCharacterReadFrozen(ctx context.Context, q rrQueryer, source FinalizedCharacterSourceIdentity) (*finalizedCharacterFrozen, error) {
	// :64 if (!hasCharacterIdentitySchema(db)) return null
	hasSchema, err := finalizedCharacterHasIdentitySchema(ctx, q)
	if err != nil {
		return nil, err
	}
	if !hasSchema {
		return nil, nil
	}
	// :65-67 the receipt is keyed by the finalization id it froze.
	rows, err := rrMaps(ctx, q, "SELECT source_key,source_hash,raw_value FROM character_identity_proposals WHERE proposal_id=?",
		"fcs:"+source.FinalizationID)
	if err != nil {
		return nil, err
	}
	// :68 if (!row) return null
	if len(rows) == 0 {
		return nil, nil
	}
	row := rows[0]
	// :69 const snapshot = JSON.parse(row.raw_value) — summaryParse is this
	// store's JSON.parse (summary.go:164), so numbers land as float64 exactly as
	// JavaScript sees them.
	raw, rawOK := row["raw_value"].(string)
	if !rawOK {
		return nil, ErrSummaryCharacterSnapshotInvalid
	}
	parsed, parsedOK := summaryParse(raw)
	if !parsedOK {
		return nil, ErrSummaryCharacterSnapshotInvalid
	}
	snapshot, objectOK := parsed.(map[string]any)
	if !objectOK {
		return nil, ErrSummaryCharacterSnapshotInvalid
	}
	// :70-75 one refusal string covers the whole clause; the reference's guards
	// are kept in its order even though they share the code.
	if row["source_key"] != summarySnapshotKind { // row.source_key !== KIND
		return nil, ErrSummaryCharacterSnapshotInvalid
	}
	if summaryHash(raw) != row["source_hash"] { // sha256(row.raw_value) !== row.source_hash
		return nil, ErrSummaryCharacterSnapshotInvalid
	}
	if snapshot["kind"] != summarySnapshotKind { // snapshot.kind !== KIND
		return nil, ErrSummaryCharacterSnapshotInvalid
	}
	if !finalizedCharacterSourceMatches(snapshot["source"], source) { // !sameSource(snapshot.source, source)
		return nil, ErrSummaryCharacterSnapshotInvalid
	}
	// !Number.isSafeInteger(snapshot.identityRevision) || snapshot.identityRevision < 0
	revision, revisionOK := rrInteger(snapshot["identityRevision"], 0)
	// !Array.isArray(snapshot.characters)
	characters, charactersOK := snapshot["characters"].([]any)
	if !revisionOK || !charactersOK {
		return nil, ErrSummaryCharacterSnapshotInvalid
	}
	frozen := &finalizedCharacterFrozen{IdentityRevision: revision, Characters: make([]finalizedCharacterFrozenCharacter, 0, len(characters))}
	// :73 new Set(characters.map(item => item.characterId)).size !==
	// characters.length — a JavaScript Set dedupes primitive ids by value. The
	// characterId is required to be text here (see the port note in the report):
	// every writer in this repo freezes characters.character_id, a TEXT column.
	seen := make(map[string]bool, len(characters))
	for _, value := range characters {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, ErrSummaryCharacterSnapshotInvalid
		}
		id, idOK := item["characterId"].(string)
		if !idOK || id == "" { // !item.characterId
			return nil, ErrSummaryCharacterSnapshotInvalid
		}
		if seen[id] {
			return nil, ErrSummaryCharacterSnapshotInvalid
		}
		seen[id] = true
		name, nameOK := item["displayNameSnapshot"].(string)  // typeof ... === 'string'
		aliases, aliasesOK := item["aliases"].([]any)         // !Array.isArray(item.aliases)
		if !nameOK || !aliasesOK {
			return nil, ErrSummaryCharacterSnapshotInvalid
		}
		names := make([]string, 0, len(aliases))
		for _, alias := range aliases {
			text, textOK := alias.(string) // item.aliases.some(alias => typeof alias !== 'string')
			if !textOK {
				return nil, ErrSummaryCharacterSnapshotInvalid
			}
			names = append(names, text)
		}
		frozen.Characters = append(frozen.Characters, finalizedCharacterFrozenCharacter{
			CharacterID:         id,
			DisplayNameSnapshot: name,
			Aliases:             names,
		})
	}
	return frozen, nil
}

// finalizedCharacterLatestFinalized is the :407 latest-finalized-draft probe.
// A row whose id/version cannot be read as integers is the reference's
// `latest.id !== draftId` arm, so it is reported as no row at all.
func finalizedCharacterLatestFinalized(ctx context.Context, q rrQueryer, chapterNumber int64) (*finalizedCharacterLatest, error) {
	rows, err := rrMaps(ctx, q, `SELECT id,version FROM drafts WHERE chapter_number=? AND status='finalized'
 ORDER BY version DESC,id DESC LIMIT 1`, chapterNumber)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	id, idOK := rrInteger(rows[0]["id"], 0)
	version, versionOK := rrInteger(rows[0]["version"], 0)
	if !idOK || !versionOK {
		return nil, nil
	}
	return &finalizedCharacterLatest{DraftID: id, Version: version}, nil
}

// finalizedCharacterActiveRow is :412 SELECT * FROM characters WHERE
// character_id=? AND retired=0. No row is the retired refusal, decided by the
// caller.
func finalizedCharacterActiveRow(ctx context.Context, q rrQueryer, characterID string) (map[string]any, error) {
	rows, err := rrMaps(ctx, q, "SELECT * FROM characters WHERE character_id=? AND retired=0", characterID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows[0], nil
}

// finalizedCharacterFieldProvenance is the :81-82 union, rebuilt as a fresh
// object the way the reference's literals are. A missing stored payload key stays
// absent: the reference would carry an explicit undefined, which Go cannot hold,
// and no writer in this repo stores a derived provenance without its source
// (character_roster_author.go:1028-1033 refuses one), nor an author provenance
// without its chapter (character_roster_author.go:1022-1027).
func finalizedCharacterFieldProvenance(stored map[string]any) map[string]any {
	kind, _ := stored["kind"].(string)
	switch kind {
	case "derived":
		provenance := map[string]any{"kind": "derived"}
		if source, present := stored["source"]; present {
			provenance["source"] = source
		}
		return provenance
	case "author":
		provenance := map[string]any{"kind": "author"}
		if chapter, present := stored["chapterNumber"]; present {
			provenance["chapterNumber"] = chapter
		}
		return provenance
	}
	return map[string]any{"kind": "legacy"}
}

// finalizedCharacterStateSnapshot is stateSnapshot (:78-88) for one field.
func finalizedCharacterStateSnapshot(row map[string]any, field, column string, scope FinalizedCharacterScope, characterID string) (FinalizedCharacterFieldSnapshot, error) {
	snapshot := FinalizedCharacterFieldSnapshot{
		ProjectID:   scope.ProjectID,
		Epoch:       scope.Epoch,
		CharacterID: characterID,
		Field:       field,
	}
	// :79 const provenance = JSON.parse(String(row.cs_provenance || '{}')).
	// charactersProvenance is this store's provenance read (characters.go:127-154):
	// it is the same String(...||'{}') coercion, including the Buffer/binary arm and
	// the falsy-default arm.
	// Port note: the reference's bare JSON.parse throws a SyntaxError on malformed
	// text; this store's provenance helpers map malformed text to "no stored
	// observations" instead (characters.go:146-148, character_roster.go:216-218).
	// There is no refusal code for it, and a second convention is what the port
	// must not invent.
	provenance, err := charactersProvenance(row["cs_provenance"], true)
	if err != nil {
		return snapshot, err
	}
	// :80 const stored = provenance[field]
	stored, _ := provenance[field].(map[string]any)
	// :81-82
	snapshot.Provenance = finalizedCharacterFieldProvenance(stored)
	// :83 const revision = stored?.revision ?? 0
	revision := int64(0)
	if raw, present := stored["revision"]; present && raw != nil {
		// :84 !Number.isSafeInteger(revision) || revision < 0
		value, ok := rrInteger(raw, 0)
		if !ok {
			return snapshot, ErrFinalizedCharacterFieldRevisionInvalid
		}
		revision = value
	}
	snapshot.Revision = revision
	// :85 const value = String(row[STATE_COLUMNS[field]] ?? '') — narrativeJoinText
	// is this store's read-side String() for raw SQLite scalars (narrative.go:185).
	snapshot.Value = narrativeJoinText(row[column])
	// :86 valueHash: sha256(value)
	snapshot.ValueHash = summaryHash(snapshot.Value)
	// :87 ...(stored?.sourceOrder ? { sourceOrder: structuredClone(...) } : {}) —
	// a JavaScript object, even an empty one, is truthy, so phTruthy is the test.
	if order, present := stored["sourceOrder"]; present && phTruthy(order) {
		snapshot.SourceOrder = order
	}
	return snapshot, nil
}
