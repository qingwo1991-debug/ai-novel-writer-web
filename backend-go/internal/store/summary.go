package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	ErrSummaryInputUnsupported = errors.New("SUMMARY_INPUT_UNSUPPORTED")
	ErrSummaryTargetChapterInvalid = errors.New("CONTINUITY_TARGET_CHAPTER_INVALID")
	ErrSummarySourceDraftInvalid = errors.New("CONTINUITY_SOURCE_DRAFT_INVALID")
	ErrSummaryReadBackFailed = errors.New("SUMMARY_READ_BACK_FAILED")
	ErrSummaryCharacterSnapshotInvalid = errors.New("FINALIZED_CHARACTER_SNAPSHOT_INVALID")
)

// Summary borrows the parent's admitted project database. It performs no DDL,
// repair, finalization, generation, or formal continuity writes. Legacy saves
// intentionally admit only safe integers (including zero/negative), not every
// scalar/fraction accepted by the original unvalidated SQLite binding. Text is
// native UTF-8, at most 32 MiB. Missing projects retain the original read/no-op
// behavior, including its precedence over argument validation.
type Summary struct { db *sql.DB }

func NewSummary(db *sql.DB) *Summary { return &Summary{db: db} }

func summaryHash(text string) string {
	hash := sha256.Sum256([]byte(text))
	return hex.EncodeToString(hash[:])
}
func summaryHashValid(value any) bool {
	text, ok := value.(string)
	if !ok || len(text) != 64 { return false }
	for _, c := range text { if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') { return false } }
	return true
}
func summaryTrim(text string) string { return strings.TrimFunc(text, narrativeSpace) }
func summaryText(value any) string { text, _ := value.(string); return text }
func summarySupportedText(text string) bool { return utf8.ValidString(text) && len(text) <= 32<<20 }
// encoding/json replaces lone escaped UTF-16 surrogates. Such valid JSON is
// outside the native UTF-8 boundary: fail explicitly rather than mutate a raw
// candidate/evidence value. Invalid JSON syntax still follows original fallbacks.
func summaryJSONSupported(text string) error {
	if !summarySupportedText(text) { return ErrSummaryInputUnsupported }
	if !json.Valid([]byte(text)) { return nil }
	quoted := false
	for i := 0; i < len(text); i++ {
		if text[i] == '"' { quoted = !quoted; continue }
		if !quoted || text[i] != '\\' { continue }
		i++; if text[i] != 'u' { continue }
		unit, _ := strconv.ParseUint(text[i+1:i+5], 16, 16); i += 4
		if unit >= 0xdc00 && unit <= 0xdfff { return ErrSummaryInputUnsupported }
		if unit < 0xd800 || unit > 0xdbff { continue }
		if i+6 >= len(text) || text[i+1:i+3] != `\u` { return ErrSummaryInputUnsupported }
		low, err := strconv.ParseUint(text[i+3:i+7], 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff { return ErrSummaryInputUnsupported }; i += 6
	}
	return nil
}
func summaryParse(text string) (any, bool) {
	var value any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil { return nil, false }
	var tail any
	if decoder.Decode(&tail) != io.EOF { return nil, false }
	return value, true
}
// String(...) is used by the original category/field validators, including
// Array.toString coercion. It is NOT used to authorize SQL/source identities.
func summaryJSString(value any) string {
	switch v := value.(type) {
	case nil: return "null"
	case string: return v
	case bool: if v { return "true" }; return "false"
	case json.Number: f, err := v.Float64(); if err != nil { return v.String() }; return phNumberString(f)
	case []any:
		parts := make([]string, len(v)); for i, item := range v { if item != nil { parts[i] = summaryJSString(item) } }; return strings.Join(parts, ",")
	case map[string]any: return "[object Object]"
	}
	return narrativeJoinText(value)
}
func summaryNumberEqual(a, b any) bool {
	if x, ok := blueprintNumber(a); ok { y, valid := blueprintNumber(b); return valid && x == y }
	return rrEqual(a, b)
}
func summarySource(value any) bool {
	m, ok := value.(map[string]any); if !ok { return false }
	_, idOK := rrInteger(m["draftId"], 1); _, chapterOK := rrInteger(m["chapterNumber"], 1)
	id, textOK := m["finalizationId"].(string)
	return idOK && chapterOK && textOK && id != "" && summaryHashValid(m["contentHash"])
}
func summarySameSource(left, right any) bool {
	a, aOK := left.(map[string]any); b, bOK := right.(map[string]any)
	return aOK && bOK && summaryNumberEqual(a["draftId"], b["draftId"]) && summaryNumberEqual(a["chapterNumber"], b["chapterNumber"]) && rrEqual(a["finalizationId"], b["finalizationId"]) && rrEqual(a["contentHash"], b["contentHash"])
}
func summaryStateField(value any) bool {
	switch summaryJSString(value) { case "location", "powerLevel", "physicalState", "mentalState", "keyItems", "recentEvents": return true }; return false
}

func summaryFacts(text string, chapter any) []map[string]any {
	empty := []map[string]any{}
	parsed, ok := summaryParse(text); if !ok { return empty }
	items, ok := parsed.([]any); if !ok || len(items) > 12 { return empty }
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		fact, ok := item.(map[string]any); if !ok { return empty }
		category := summaryJSString(fact["category"])
		if category != "character-state" && category != "timeline" && category != "open-thread" && category != "plot" { return empty }
		if !summaryNumberEqual(fact["sourceChapter"], chapter) { return empty }
		entities := []string{}
		if values, ok := fact["entities"].([]any); ok {
			if len(values) > 8 { return empty }
			for _, value := range values { entity := summaryTrim(summaryText(value)); if entity == "" || narrativeLength(entity) > 80 { return empty }; entities = append(entities, entity) }
		}
		statement, evidence := summaryTrim(summaryText(fact["statement"])), summaryTrim(summaryText(fact["evidence"]))
		if statement == "" || evidence == "" { return empty }
		distinct := []string{}; seen := map[string]bool{}
		for _, entity := range entities { if !seen[entity] { distinct = append(distinct, entity); seen[entity] = true } }
		result := map[string]any{"category": fact["category"], "entities": distinct, "statement": statement, "sourceChapter": chapter, "evidence": evidence}
		if refs, ok := fact["characterRefs"].([]any); ok {
			clean := []map[string]any{}
			for _, value := range refs {
				ref, ok := value.(map[string]any); if !ok { return empty }
				id, idOK := ref["characterId"].(string); name, nameOK := ref["displayNameSnapshot"].(string)
				if !idOK || summaryTrim(id) == "" || !nameOK || !seen[name] { return empty }
				clean = append(clean, map[string]any{"characterId": id, "displayNameSnapshot": name})
			}
			result["characterRefs"] = clean
		}
		out = append(out, result)
	}
	return out
}
func summaryCandidates(text string) []map[string]any {
	out := []map[string]any{}
	parsed, ok := summaryParse(text); if !ok { return out }
	pending, ok := parsed.([]any)
	if !ok { if store, valid := parsed.(map[string]any); valid && summaryNumberEqual(store["version"], int64(2)) { pending, _ = store["pending"].([]any) } }
	for _, input := range pending {
		candidate, ok := input.(map[string]any); if !ok { continue }
		bound := false; for _, key := range []string{"candidateKey", "source", "expectedFieldRevision", "expectedFieldValueHash"} { if _, present := candidate[key]; present { bound = true } }
		key, keyOK := candidate["candidateKey"].(string)
		_, revisionOK := rrInteger(candidate["expectedFieldRevision"], 0)
		sourceOK := summarySource(candidate["source"])
		if bound && (!keyOK || key == "" || !sourceOK || !revisionOK || !summaryHashValid(candidate["expectedFieldValueHash"])) { continue }
		name, nameOK := candidate["characterName"].(string); value, valueOK := candidate["value"].(string)
		if !nameOK || summaryTrim(name) == "" || !summaryStateField(candidate["field"]) || !valueOK { continue }
		if !sourceOK { value = summaryTrim(value) }
		result := map[string]any{"characterName": summaryTrim(name), "field": candidate["field"], "value": value}
		if id, ok := candidate["characterId"].(string); ok && id != "" { result["characterId"] = id }
		if phTruthy(candidate["evidence"]) { result["evidence"] = candidate["evidence"] }
		for _, field := range []string{"selectionKey", "displayName", "candidateKey", "expectedFieldValueHash"} { if value, ok := candidate[field].(string); ok { result[field] = value } }
		if value, present := candidate["rawValue"]; present { result["rawValue"] = value }
		if candidate["reason"] == "author-protected" || candidate["reason"] == "legacy-protected" { result["reason"] = candidate["reason"] }
		if sourceOK { result["source"] = candidate["source"] }
		if revisionOK { result["expectedFieldRevision"] = candidate["expectedFieldRevision"] }
		out = append(out, result)
	}
	return out
}

func (s *Summary) SaveSnapshot(ctx context.Context, chapterNumber any, characterStates string) error {
	if s == nil || s.db == nil { return nil }
	chapter, ok := rrInteger(chapterNumber, -9007199254740991)
	if !ok || !utf8.ValidString(characterStates) || len(characterStates) > 32<<20 { return ErrSummaryInputUnsupported }
	return NewReviewRevisions(s.db).immediate(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, "INSERT INTO summary_snapshots(chapter_number,character_states) VALUES(?,?)", chapter, characterStates)
		if err != nil { return err }; id, err := result.LastInsertId(); if err != nil { return err }
		rows, err := rrMaps(ctx, conn, "SELECT * FROM summary_snapshots WHERE id=?", id); if err != nil { return err }
		if len(rows) != 1 { return ErrSummaryReadBackFailed }; row := rows[0]
		if row["draft_id"] != nil || !rrEqual(row["chapter_number"], chapter) || row["character_states"] != characterStates || row["chapter_notes"] != "" || row["continuity_facts"] != "[]" || row["character_state_candidates"] != "[]" || row["source_finalization_id"] != "" || row["source_content_hash"] != "" || !rrEqual(row["projection_generation"], int64(0)) { return ErrSummaryReadBackFailed }
		return nil
	})
}
func (s *Summary) LatestSnapshot(ctx context.Context) (map[string]any, error) {
	if s == nil || s.db == nil { return nil, nil }
	var result map[string]any
	err := NewReviewRevisions(s.db).immediate(ctx, func(conn *sql.Conn) error {
		rows, err := rrMaps(ctx, conn, "SELECT character_states AS characterStates,chapter_number AS chapterNumber FROM summary_snapshots WHERE draft_id IS NULL ORDER BY id DESC LIMIT 1")
		if err != nil { return err }; if len(rows) > 0 { result = rows[0]; if text, ok := result["characterStates"].(string); ok && !summarySupportedText(text) { return ErrSummaryInputUnsupported } }; return nil
	})
	if err != nil { return nil, err }; return result, nil
}

func summaryFinalizedSource(ctx context.Context, conn *sql.Conn, id any) (map[string]any, error) {
	rows, err := rrMaps(ctx, conn, `SELECT d.id AS draftId,d.chapter_number AS chapterNumber,d.status,c.body AS content,
	 o.finalization_id AS finalizationId,o.chapter_title AS chapterTitle,o.content_hash AS contentHash,
	 o.content_snapshot AS contentSnapshot,m.generation AS projectionGeneration
	 FROM drafts d JOIN contents c ON c.id=d.content_id JOIN finalization_outbox o ON o.draft_id=d.id
	 JOIN continuity_projection_meta m ON m.id='main' WHERE d.id=?`, id)
	if err != nil || len(rows) == 0 { return nil, err }; row := rows[0]
	for _, key := range []string{"finalizationId", "content", "contentSnapshot", "chapterTitle"} { if text, ok := row[key].(string); ok && !summarySupportedText(text) { return nil, ErrSummaryInputUnsupported } }
	finalization, finalOK := row["finalizationId"].(string); body, bodyOK := row["content"].(string); snapshot, snapOK := row["contentSnapshot"].(string)
	if row["status"] != "finalized" || !finalOK || summaryTrim(finalization) == "" || !summaryHashValid(row["contentHash"]) || !bodyOK || !snapOK || body != snapshot || summaryHash(snapshot) != row["contentHash"] { return nil, nil }
	return map[string]any{"source": map[string]any{"draftId": row["draftId"], "chapterNumber": row["chapterNumber"], "finalizationId": finalization, "contentHash": row["contentHash"]}, "chapterTitle": row["chapterTitle"], "content": snapshot, "projectionGeneration": row["projectionGeneration"]}, nil
}
func (s *Summary) ReadFinalizedSource(ctx context.Context, draftID any) (map[string]any, error) {
	invalid := map[string]any{"status": "invalid"}
	if s == nil || s.db == nil { return invalid, nil }
	id, ok := rrInteger(draftID, 1); if !ok { return nil, ErrSummarySourceDraftInvalid }
	var result map[string]any
	err := NewReviewRevisions(s.db).immediate(ctx, func(conn *sql.Conn) error {
		rows, err := rrMaps(ctx, conn, `SELECT d.id AS draftId,d.chapter_number AS chapterNumber,d.status,c.body AS content,
		 COALESCE(o.chapter_title,b.title,'') AS chapterTitle,o.draft_id AS receiptDraftId
		 FROM drafts d JOIN contents c ON c.id=d.content_id LEFT JOIN finalization_outbox o ON o.draft_id=d.id
		 LEFT JOIN blueprints b ON b.chapter_number=d.chapter_number WHERE d.id=?`, id)
		if err != nil { return err }; result = invalid
		if len(rows) == 0 || rows[0]["status"] != "finalized" { return nil }; row := rows[0]
		for _, key := range []string{"content", "chapterTitle"} { if text, ok := row[key].(string); ok && !summarySupportedText(text) { return ErrSummaryInputUnsupported } }
		if row["receiptDraftId"] == nil { result = map[string]any{"status": "legacy", "draftId": row["draftId"], "chapterNumber": row["chapterNumber"], "chapterTitle": row["chapterTitle"], "content": row["content"]}; return nil }
		snapshot, err := summaryFinalizedSource(ctx, conn, id); if err != nil { return err }
		if snapshot != nil { result = map[string]any{"status": "valid", "snapshot": snapshot} }; return nil
	})
	if err != nil { return nil, err }; return result, nil
}

const summaryListSQL = `SELECT s.draft_id AS draftId,s.chapter_number AS chapterNumber,
 COALESCE(o.chapter_title,'') AS chapterTitle,s.chapter_notes AS chapterNotes,
 s.continuity_facts AS continuityFacts,s.character_state_candidates AS characterStateCandidates,
 s.source_finalization_id AS sourceFinalizationId,s.source_content_hash AS sourceContentHash,
 s.projection_generation AS projectionGeneration,cur.id AS currentDraftId,cur.status AS currentDraftStatus,
 o.finalization_id AS currentFinalizationId,o.content_hash AS currentContentHash,
 o.content_snapshot AS contentSnapshot,cc.body AS currentContent,m.generation AS currentGeneration,
 m.stale_from_chapter AS staleFromChapter
 FROM summary_snapshots s JOIN drafts d ON d.id=s.draft_id JOIN contents c ON c.id=d.content_id
 LEFT JOIN drafts cur ON cur.chapter_number=d.chapter_number AND cur.status='finalized'
 AND NOT EXISTS(SELECT 1 FROM drafts newer WHERE newer.chapter_number=cur.chapter_number AND newer.status='finalized'
 AND (newer.version>cur.version OR (newer.version=cur.version AND newer.id>cur.id)))
 LEFT JOIN contents cc ON cc.id=cur.content_id LEFT JOIN finalization_outbox o ON o.draft_id=cur.id
 JOIN continuity_projection_meta m ON m.id='main'
 WHERE s.draft_id IS NOT NULL AND s.chapter_number<? AND s.chapter_notes<>''
 AND d.status IN ('finalized','superseded')
 AND NOT EXISTS(SELECT 1 FROM summary_snapshots ns WHERE ns.chapter_number=s.chapter_number AND ns.chapter_notes<>'' AND ns.id>s.id)
 ORDER BY s.chapter_number ASC,s.draft_id ASC`

const summarySnapshotKind = "finalized-character-snapshot:v1"
func summaryHasIdentity(ctx context.Context, conn *sql.Conn) (bool, error) {
	rows, err := rrMaps(ctx, conn, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='character_identity_meta'")
	return len(rows) > 0, err
}
func summaryFrozen(ctx context.Context, conn *sql.Conn, source map[string]any) ([]map[string]any, bool, error) {
	rows, err := rrMaps(ctx, conn, "SELECT source_key,source_hash,raw_value FROM character_identity_proposals WHERE proposal_id=?", "fcs:"+summaryText(source["finalizationId"]))
	if err != nil { return nil, false, err }; if len(rows) == 0 { return nil, false, nil }
	row := rows[0]; raw, ok := row["raw_value"].(string); if !ok { return nil, false, ErrSummaryCharacterSnapshotInvalid }
	if err := summaryJSONSupported(raw); err != nil { return nil, false, err }
	parsed, ok := summaryParse(raw); if !ok { return nil, false, ErrSummaryCharacterSnapshotInvalid }
	snapshot, ok := parsed.(map[string]any); if !ok { return nil, false, ErrSummaryCharacterSnapshotInvalid }
	_, revisionOK := rrInteger(snapshot["identityRevision"], 0); characters, charsOK := snapshot["characters"].([]any)
	if row["source_key"] != summarySnapshotKind || summaryHash(raw) != row["source_hash"] || snapshot["kind"] != summarySnapshotKind || !summarySameSource(snapshot["source"], source) || !revisionOK || !charsOK { return nil, false, ErrSummaryCharacterSnapshotInvalid }
	out := []map[string]any{}; seen := map[string]bool{}
	for _, value := range characters {
		character, ok := value.(map[string]any); if !ok || !phTruthy(character["characterId"]) { return nil, false, ErrSummaryCharacterSnapshotInvalid }
		if _, ok := character["displayNameSnapshot"].(string); !ok { return nil, false, ErrSummaryCharacterSnapshotInvalid }
		aliases, ok := character["aliases"].([]any); if !ok { return nil, false, ErrSummaryCharacterSnapshotInvalid }
		for _, alias := range aliases { if _, ok := alias.(string); !ok { return nil, false, ErrSummaryCharacterSnapshotInvalid } }
		// JS Set uses identity for objects/arrays, value equality for primitives.
		key := ""; switch id := character["characterId"].(type) { case string: key = "s:"+id; case json.Number: key = "n:"+summaryJSString(id); case bool: key = "b:"+strconv.FormatBool(id) }
		if key != "" { if seen[key] { return nil, false, ErrSummaryCharacterSnapshotInvalid }; seen[key] = true }
		out = append(out, character)
	}
	return out, true, nil
}
func summaryFactEntities(fact map[string]any) []string { entities, _ := fact["entities"].([]string); return entities }
func summaryFactRefs(fact map[string]any) []map[string]any { refs, _ := fact["characterRefs"].([]map[string]any); return refs }
func summaryIdentityStatus(frozen []map[string]any, exists bool, err error, facts, candidates []map[string]any) string {
	if err != nil { return "stale" }; if !exists { return "legacy" }
	hasName := func(name string) bool { for _, character := range frozen { if character["displayNameSnapshot"] == name { return true } }; return false }
	for _, fact := range facts { for _, entity := range summaryFactEntities(fact) { if !hasName(entity) { continue }; found := false; for _, ref := range summaryFactRefs(fact) { if ref["displayNameSnapshot"] == entity { found = true } }; if !found { return "legacy" } } }
	for _, candidate := range candidates { if !phTruthy(candidate["characterId"]) { return "legacy" } }
	bound := func(id any, name string) bool { count := 0; match := false; for _, character := range frozen { if character["displayNameSnapshot"] == name { count++; if summaryNumberEqual(character["characterId"], id) { match = true } } }; return count == 1 && match }
	for _, fact := range facts {
		refs := summaryFactRefs(fact)
		for _, ref := range refs { if !bound(ref["characterId"], summaryText(ref["displayNameSnapshot"])) { return "stale" } }
		for _, entity := range summaryFactEntities(fact) { if !hasName(entity) { continue }; matches := []map[string]any{}; for _, ref := range refs { if ref["displayNameSnapshot"] == entity { matches = append(matches, ref) } }; if len(matches) != 1 || !bound(matches[0]["characterId"], entity) { return "stale" } }
	}
	for _, candidate := range candidates { if !bound(candidate["characterId"], summaryText(candidate["characterName"])) { return "stale" } }
	return "current"
}
func (s *Summary) ListBefore(ctx context.Context, chapterNumber any) ([]map[string]any, error) {
	if s == nil || s.db == nil { return []map[string]any{}, nil }
	chapter, ok := rrInteger(chapterNumber, 1); if !ok { return nil, ErrSummaryTargetChapterInvalid }
	out := []map[string]any{}
	err := NewReviewRevisions(s.db).immediate(ctx, func(conn *sql.Conn) error {
		rows, err := rrMaps(ctx, conn, summaryListSQL, chapter); if err != nil { return err }
		hasIdentity, err := summaryHasIdentity(ctx, conn); if err != nil { return err }
		for _, row := range rows {
			hasBound := phTruthy(row["sourceFinalizationId"]) && phTruthy(row["sourceContentHash"])
			body, bodyOK := row["currentContent"].(string); snapshot, snapOK := row["contentSnapshot"].(string)
			sourceCurrent := hasBound && rrEqual(row["currentFinalizationId"], row["sourceFinalizationId"]) && rrEqual(row["currentContentHash"], row["sourceContentHash"]) && bodyOK && snapOK && body == snapshot && summaryHash(body) == row["currentContentHash"]
			_, idOK := rrInteger(row["currentDraftId"], 1)
			fallback := idOK && row["currentDraftStatus"] == "finalized" && bodyOK && snapOK && body == snapshot && summaryHash(body) == row["currentContentHash"]
			invalidated := row["staleFromChapter"] != nil && narrativeNumeric(row["chapterNumber"]) >= narrativeNumeric(row["staleFromChapter"]) && narrativeNumeric(row["projectionGeneration"]) < narrativeNumeric(row["currentGeneration"])
			for _, key := range []string{"continuityFacts", "characterStateCandidates"} { if err := summaryJSONSupported(narrativeJoinText(row[key])); err != nil { return err } }
			for _, key := range []string{"chapterTitle", "chapterNotes", "currentContent", "contentSnapshot", "sourceFinalizationId", "sourceContentHash"} { if text, ok := row[key].(string); ok && !summarySupportedText(text) { return ErrSummaryInputUnsupported } }
			facts := summaryFacts(narrativeJoinText(row["continuityFacts"]), row["chapterNumber"])
			candidates := summaryCandidates(narrativeJoinText(row["characterStateCandidates"]))
			source := map[string]any{"draftId": row["draftId"], "finalizationId": row["sourceFinalizationId"], "chapterNumber": row["chapterNumber"], "contentHash": row["sourceContentHash"]}
			status := "current"
			if hasIdentity && sourceCurrent {
				frozen, exists, frozenErr := summaryFrozen(ctx, conn, source)
				if errors.Is(frozenErr, ErrSummaryInputUnsupported) { return frozenErr }
				status = summaryIdentityStatus(frozen, exists, frozenErr, facts, candidates)
			}
			if !hasBound { status = "legacy" } else if !sourceCurrent || invalidated { status = "stale" }
			projection := map[string]any{"draftId": row["draftId"], "chapterNumber": row["chapterNumber"], "chapterTitle": row["chapterTitle"], "chapterNotes": row["chapterNotes"], "facts": facts, "sourceStatus": status}
			if !sourceCurrent && fallback { projection["currentFinalizedDraftId"] = row["currentDraftId"] }
			if len(candidates) > 0 { projection["characterStateCandidates"] = candidates }; if hasBound { projection["source"] = source }
			out = append(out, projection)
		}
		return nil
	})
	if err != nil { return nil, err }; return out, nil
}
