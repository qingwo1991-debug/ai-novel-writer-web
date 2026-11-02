package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

var (
	ErrCharacterInputUnsupported = errors.New("CHARACTER_INPUT_UNSUPPORTED")
	ErrCharacterIDSchemaRequired = errors.New("CHARACTER_ID_SCHEMA_REQUIRED")
	ErrCharacterIDMetaMissing    = errors.New("CHARACTER_ID_META_MISSING")
)

// Characters borrows an admitted project database. These are read primitives,
// not identity/schema migration, author, generation, or portable-origin owners.
// Each public multi-query read reserves one BEGIN IMMEDIATE connection. The
// connection helpers deliberately never acquire another pool connection.
// Native text is UTF-8, at most 32 MiB per field; parsed JSON additionally rejects
// lone surrogates, nonfinite numbers, and nesting beyond 64. Unparsed identity
// evidence stays raw. JSON object insertion order is not a map/wire guarantee:
// hash owners must reread cs_provenance and use their ordered JSON decoder.
type Characters struct{ db *sql.DB }

func NewCharacters(db *sql.DB) *Characters { return &Characters{db: db} }

var characterStateFields = []string{"location", "powerLevel", "physicalState", "mentalState", "keyItems", "recentEvents"}
var characterStateColumns = []string{"cs_location", "cs_power_level", "cs_physical_state", "cs_mental_state", "cs_key_items", "cs_recent_events"}

func charactersRole(value any) string {
	text, ok := value.(string)
	if !ok {
		return "supporting"
	}
	// Only ASCII case folding can reach an alias. Go's Unicode ToLower would
	// incorrectly accept e.g. MİNOR (JS lowercases İ to i + combining dot).
	text = strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, summaryTrim(text))
	switch text {
	case "protagonist", "main", "主角", "男主", "女主", "核心主角":
		return "protagonist"
	case "antagonist", "villain", "反派", "对手", "敌人":
		return "antagonist"
	case "supporting", "support", "配角", "重要配角", "核心配角":
		return "supporting"
	case "minor", "龙套", "次要角色":
		return "minor"
	}
	return "supporting"
}

// Keep SQLite storage classes here so a caller can still distinguish string
// proof fields from BLOB and numeric fields. No JSON coercion before validation.
func charactersRows(ctx context.Context, conn *sql.Conn, suffix string, args ...any) ([]map[string]any, error) {
	return charactersQuery(ctx, conn, "SELECT * FROM characters "+suffix, args...)
}
func charactersQuery(ctx context.Context, conn *sql.Conn, query string, args ...any) ([]map[string]any, error) {
	rows, err := rrMaps(ctx, conn, query, args...)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		for _, value := range row {
			switch v := value.(type) {
			case string:
				if !summarySupportedText(v) {
					return nil, ErrCharacterInputUnsupported
				}
			case []byte:
				if len(v) > 32<<20 {
					return nil, ErrCharacterInputUnsupported
				}
			case float64:
				if math.IsInf(v, 0) || math.IsNaN(v) {
					return nil, ErrCharacterInputUnsupported
				}
			}
		}
	}
	return rows, nil
}

// better-sqlite3's default INTEGER reads are JS Number (including rounding above
// MAX_SAFE_INTEGER), and JSON.stringify(Buffer) is not Go's base64 []byte wire.
func charactersWire(value any) any {
	switch v := value.(type) {
	case int64:
		if v > 9007199254740991 || v < -9007199254740991 {
			return float64(v)
		}
		return v
	case []byte:
		data := make([]int, len(v))
		for i, b := range v {
			data[i] = int(b)
		}
		return map[string]any{"type": "Buffer", "data": data}
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = charactersWire(item)
		}
		return out
	case []map[string]any:
		out := make([]map[string]any, len(v))
		for i, item := range v {
			out[i] = charactersWire(item).(map[string]any)
		}
		return out
	}
	return value
}
func charactersOrEmpty(value any) any {
	if !phTruthy(value) {
		return ""
	}
	return charactersWire(value)
}
func charactersProvenance(value any, coerce bool) (map[string]any, error) {
	text, ok := value.(string)
	if !ok {
		if !coerce {
			return map[string]any{}, nil
		}
		if bytes, ok := value.([]byte); ok && !utf8.Valid(bytes) {
			return nil, ErrCharacterInputUnsupported
		}
		if !phTruthy(value) {
			text = "{}"
		} else {
			text = narrativeJoinText(value)
		}
	}
	if err := summaryJSONSupported(text); err != nil {
		return nil, ErrCharacterInputUnsupported
	}
	parsed, valid := summaryParse(text)
	if !valid {
		return map[string]any{}, nil
	}
	object, ok := parsed.(map[string]any)
	if !ok {
		return map[string]any{}, nil
	}
	return object, nil
}
func charactersRowToData(row map[string]any) (map[string]any, error) {
	// The TS alias table inherits Object.prototype. Those non-role objects or
	// functions have no supported native role wire; never fabricate a fallback.
	if role, ok := row["role"].(string); ok {
		trimmed := summaryTrim(role)
		if strings.EqualFold(trimmed, "__proto__") || strings.EqualFold(trimmed, "constructor") {
			return nil, ErrCharacterInputUnsupported
		}
		switch trimmed {
		case "__proto__", "constructor", "toString", "toLocaleString", "valueOf", "hasOwnProperty", "isPrototypeOf", "propertyIsEnumerable", "__defineGetter__", "__defineSetter__", "__lookupGetter__", "__lookupSetter__":
			return nil, ErrCharacterInputUnsupported
		}
	}
	data := map[string]any{"name": charactersWire(row["name"]), "role": charactersRole(row["role"])}
	if id, ok := row["character_id"].(string); ok {
		data["characterId"] = id
	}
	for _, key := range []string{"gender", "age", "appearance", "personality", "background", "abilities", "motivation", "relationships", "arc", "notes"} {
		data[key] = charactersOrEmpty(row[key])
	}
	if chapter := row["cs_updated_at_chapter"]; chapter != nil {
		provenance, err := charactersProvenance(row["cs_provenance"], false)
		if err != nil {
			return nil, err
		}
		state := map[string]any{"updatedAtChapter": charactersWire(chapter)}
		for i, field := range characterStateFields {
			state[field] = charactersOrEmpty(row[characterStateColumns[i]])
		}
		if len(provenance) > 0 {
			state["provenance"] = provenance
		}
		data["currentState"] = state
	}
	return data, nil
}
func charactersAll(ctx context.Context, conn *sql.Conn) ([]map[string]any, error) {
	hasIdentity, err := summaryHasIdentity(ctx, conn)
	if err != nil {
		return nil, err
	}
	suffix := ""
	// Exact original gate: table presence, not a guessed retired column check.
	if hasIdentity {
		suffix = "WHERE retired=0 "
	}
	rows, err := charactersRows(ctx, conn, suffix+`ORDER BY CASE role
	 WHEN 'protagonist' THEN 0 WHEN 'supporting' THEN 1
	 WHEN 'antagonist' THEN 2 WHEN 'minor' THEN 3 ELSE 9 END ASC`)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		data, err := charactersRowToData(row)
		if err != nil {
			return nil, err
		}
		out = append(out, data)
	}
	return out, nil
}
func (c *Characters) All(ctx context.Context) ([]map[string]any, error) {
	if c == nil || c.db == nil {
		return []map[string]any{}, nil
	}
	var result []map[string]any
	err := NewReviewRevisions(c.db).immediate(ctx, func(conn *sql.Conn) error {
		var err error
		result, err = charactersAll(ctx, conn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func charactersIdentity(ctx context.Context, conn *sql.Conn) (map[string]any, error) {
	hasIdentity, err := summaryHasIdentity(ctx, conn)
	if err != nil {
		return nil, err
	}
	if !hasIdentity {
		return nil, ErrCharacterIDSchemaRequired
	}
	meta, err := charactersQuery(ctx, conn, "SELECT revision FROM character_identity_meta WHERE id='main'")
	if err != nil {
		return nil, err
	}
	if len(meta) == 0 {
		return nil, ErrCharacterIDMetaMissing
	}
	result := map[string]any{"revision": charactersWire(meta[0]["revision"])}
	for _, item := range []struct{ key, query string }{
		{"characters", "SELECT * FROM characters ORDER BY character_id"},
		{"aliases", "SELECT * FROM character_aliases ORDER BY character_id,name,valid_from"},
		{"relationships", "SELECT * FROM character_relationships ORDER BY relationship_id"},
		{"proposals", "SELECT * FROM character_identity_proposals ORDER BY proposal_id"},
	} {
		rows, err := charactersQuery(ctx, conn, item.query)
		if err != nil {
			return nil, err
		}
		result[item.key] = charactersWire(rows)
	}
	return result, nil
}
func (c *Characters) IdentitySnapshot(ctx context.Context) (map[string]any, error) {
	if c == nil || c.db == nil {
		return nil, ErrProjectNotOpen
	}
	var result map[string]any
	err := NewReviewRevisions(c.db).immediate(ctx, func(conn *sql.Conn) error {
		var err error
		result, err = charactersIdentity(ctx, conn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// IdentityView is the proposal service's display identitySnapshot, not the raw
// repository evidence snapshot above. It does not expose proposals or source
// provenance envelopes from relationships, and does not normalize author roles.
func charactersIdentityView(ctx context.Context, conn *sql.Conn) (map[string]any, error) {
	hasIdentity, err := summaryHasIdentity(ctx, conn)
	if err != nil {
		return nil, err
	}
	if !hasIdentity {
		return nil, ErrCharacterIDSchemaRequired
	}
	meta, err := charactersQuery(ctx, conn, "SELECT revision FROM character_identity_meta WHERE id='main'")
	if err != nil {
		return nil, err
	}
	if len(meta) == 0 {
		return nil, ErrCharacterIDMetaMissing
	}
	rows, err := charactersRows(ctx, conn, "ORDER BY character_id")
	if err != nil {
		return nil, err
	}
	characters := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		fields := map[string]any{}
		for _, key := range []string{"name", "role", "gender", "age", "appearance", "personality", "background", "abilities", "motivation", "arc", "notes"} {
			text := ""
			// Node Buffer UTF-8 replacement groups malformed byte sequences
			// differently from Go. Keep those display coercions out of-domain;
			// raw repository reads still preserve arbitrary Buffer bytes.
			if bytes, ok := row[key].([]byte); ok && !utf8.Valid(bytes) {
				return nil, ErrCharacterInputUnsupported
			}
			if row[key] != nil {
				text = narrativeJoinText(row[key])
			}
			fields[key] = text
		}
		raw := "null"
		if bytes, ok := row["static_provenance"].([]byte); ok && !utf8.Valid(bytes) {
			return nil, ErrCharacterInputUnsupported
		}
		if row["static_provenance"] != nil {
			raw = narrativeJoinText(row["static_provenance"])
		}
		if err := summaryJSONSupported(raw); err != nil {
			return nil, ErrCharacterInputUnsupported
		}
		provenance, valid := summaryParse(raw)
		if !valid {
			return nil, ErrCharacterInputUnsupported
		}
		characters = append(characters, map[string]any{"characterId": charactersWire(row["character_id"]), "fields": fields,
			"retired": summaryNumberEqual(row["retired"], int64(1)), "revision": charactersWire(row["identity_revision"]), "provenance": provenance})
	}
	aliases, err := charactersQuery(ctx, conn, "SELECT character_id AS characterId,name,source_key AS sourceKey,valid_from AS validFrom,valid_through AS validThrough FROM character_aliases ORDER BY character_id,name,valid_from")
	if err != nil {
		return nil, err
	}
	relationships, err := charactersQuery(ctx, conn, "SELECT relationship_id AS relationshipId,source_character_id AS sourceCharacterId,target_character_id AS targetCharacterId,relation FROM character_relationships ORDER BY relationship_id")
	if err != nil {
		return nil, err
	}
	return map[string]any{"revision": charactersWire(meta[0]["revision"]), "characters": characters, "aliases": charactersWire(aliases), "relationships": charactersWire(relationships)}, nil
}
func (c *Characters) IdentityView(ctx context.Context) (map[string]any, error) {
	if c == nil || c.db == nil {
		return nil, ErrProjectNotOpen
	}
	var result map[string]any
	err := NewReviewRevisions(c.db).immediate(ctx, func(conn *sql.Conn) error {
		var err error
		result, err = charactersIdentityView(ctx, conn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

const charactersCurrentSourcesSQL = `SELECT d.id AS draftId,d.chapter_number AS chapterNumber,d.version AS draftVersion,c.body,
 o.finalization_id AS finalizationId,o.content_hash AS contentHash,o.content_snapshot AS contentSnapshot,
 s.projection_generation AS projectionGeneration
 FROM drafts d JOIN contents c ON c.id=d.content_id JOIN finalization_outbox o ON o.draft_id=d.id
 JOIN summary_snapshots s ON s.draft_id=d.id AND s.chapter_number=d.chapter_number
 AND s.source_finalization_id=o.finalization_id AND s.source_content_hash=o.content_hash
 WHERE d.status='finalized' AND NOT EXISTS (
 SELECT 1 FROM drafts newer WHERE newer.chapter_number=d.chapter_number AND newer.status='finalized'
 AND (newer.version>d.version OR (newer.version=d.version AND newer.id>d.id)))`

func charactersKeys(object map[string]any, expected string) bool {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",") == expected
}
func charactersSourceKey(source map[string]any) string {
	// Fixed array serialization, no input-object enumeration dependency.
	text, _ := plotJSON([]any{source["draftId"], source["finalizationId"], source["chapterNumber"], source["contentHash"]})
	return text
}
func charactersDerivedTrusted(entry any, proofs map[string]map[string]any, watermark map[string]any, projectID string) bool {
	value, ok := entry.(map[string]any)
	if !ok || value["kind"] != "derived" || !charactersKeys(value, "kind,revision,source,sourceOrder") {
		return false
	}
	if _, ok := rrInteger(value["revision"], 1); !ok {
		return false
	}
	source, ok := value["source"].(map[string]any)
	if !ok || !summarySource(source) || summaryTrim(summaryText(source["finalizationId"])) == "" {
		return false
	}
	proof := proofs[charactersSourceKey(source)]
	if proof == nil {
		return false
	}
	order, ok := value["sourceOrder"].(map[string]any)
	if !ok || !charactersKeys(order, "authoritativeFinalizationRevision,chapterNumber,continuityEpoch") {
		return false
	}
	if !summaryNumberEqual(order["chapterNumber"], source["chapterNumber"]) || !summaryNumberEqual(order["authoritativeFinalizationRevision"], proof["draftVersion"]) {
		return false
	}
	if projectID == "" || order["continuityEpoch"] != projectID+":"+summaryJSString(proof["projectionGeneration"]) {
		return false
	}
	if watermark["staleFromChapter"] != nil {
		chapter, chapterOK := blueprintNumber(source["chapterNumber"])
		stale, staleOK := blueprintNumber(watermark["staleFromChapter"])
		generation, genOK := blueprintNumber(watermark["generation"])
		projected, projOK := blueprintNumber(proof["projectionGeneration"])
		if chapterOK && staleOK && genOK && projOK && chapter >= stale && projected < generation {
			return false
		}
	}
	return true
}

// Display-only grants from currentCharacterProjection's proof. No portable origin
// is admitted here. Author/legacy fields cannot grant derived capabilities, so
// the remaining projection's serialization need not be reconstructed to derive
// exactly its six ordered field names (including empty string values).
func currentDerivedFields(ctx context.Context, conn *sql.Conn, projectID string) (map[string]any, error) {
	if !summarySupportedText(projectID) {
		return nil, ErrCharacterInputUnsupported
	}
	meta, err := charactersQuery(ctx, conn, "SELECT generation,stale_from_chapter AS staleFromChapter FROM continuity_projection_meta WHERE id='main'")
	if err != nil {
		return nil, err
	}
	if len(meta) == 0 {
		return nil, sql.ErrNoRows
	}
	watermark := meta[0]
	sources, err := charactersQuery(ctx, conn, charactersCurrentSourcesSQL)
	if err != nil {
		return nil, err
	}
	proofs := map[string]map[string]any{}
	for _, row := range sources {
		body, bodyOK := row["body"].(string)
		snapshot, snapshotOK := row["contentSnapshot"].(string)
		_, finalOK := row["finalizationId"].(string)
		_, hashOK := row["contentHash"].(string)
		_, versionOK := rrInteger(row["draftVersion"], 1)
		projected, projectionOK := rrInteger(row["projectionGeneration"], 0)
		generation, generationOK := blueprintNumber(watermark["generation"])
		if !bodyOK || !snapshotOK || !finalOK || !hashOK || body != snapshot || summaryHash(body) != row["contentHash"] || !versionOK || !projectionOK || !generationOK || float64(projected) > generation {
			continue
		}
		proofs[charactersSourceKey(row)] = row
	}
	rows, err := charactersRows(ctx, conn, "ORDER BY character_id")
	if err != nil {
		return nil, err
	}
	result := make(map[string]any, len(rows))
	for _, row := range rows {
		provenance, err := charactersProvenance(row["cs_provenance"], true)
		if err != nil {
			return nil, err
		}
		fields := []string{}
		for i, field := range characterStateFields {
			if _, stringValue := row[characterStateColumns[i]].(string); stringValue && charactersDerivedTrusted(provenance[field], proofs, watermark, projectID) {
				fields = append(fields, field)
			}
		}
		id, exists := row["character_id"]
		key := "undefined"
		if exists {
			key = narrativeJoinText(id)
			if id == nil {
				key = "null"
			}
		}
		result[key] = fields
	}
	return result, nil
}
func (c *Characters) CurrentDerivedFields(ctx context.Context, projectID string) (map[string]any, error) {
	if c == nil || c.db == nil {
		return nil, ErrProjectNotOpen
	}
	var result map[string]any
	err := NewReviewRevisions(c.db).immediate(ctx, func(conn *sql.Conn) error {
		var err error
		result, err = currentDerivedFields(ctx, conn, projectID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
