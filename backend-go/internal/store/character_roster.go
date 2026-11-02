package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

var (
	ErrCharacterRosterSchemaUnsupported = errors.New("CHARACTER_ROSTER_SCHEMA_UNSUPPORTED")
	ErrCharacterRosterMetaMissing       = errors.New("CHARACTER_ROSTER_META_MISSING")
)

// ReadRoster implements only the centrally migrated M02 (admitted v7) read lane.
// It never creates a legacy schema, initializes metadata, repairs a receipt, or
// writes a projection. The immediate transaction reserves one connection so
// characters, identities, relationships and receipts form one coherent snapshot.
func (r *Characters) ReadRoster(ctx context.Context) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var result map[string]any
	err := NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		var err error
		result, err = charactersRoster(ctx, conn)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func rosterHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
func rosterLess(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}
func rosterText(value any) string { text, _ := value.(string); return text }
func rosterRole(value any) string { return charactersRole(value) }
func rosterRoleOrder(role any) int {
	switch role {
	case "protagonist":
		return 0
	case "supporting":
		return 1
	case "antagonist":
		return 2
	default:
		return 3
	}
}
func rosterSort(entries []map[string]any) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if rosterRoleOrder(a["role"]) != rosterRoleOrder(b["role"]) {
			return rosterRoleOrder(a["role"]) < rosterRoleOrder(b["role"])
		}
		return rosterLess(rosterText(a["name"]), rosterText(b["name"]))
	})
}

// SQLite's loose typing is retained for finite JS scalars (charactersAll has
// already applied better-sqlite3's JS Number rounding). Buffers, invalid UTF-8,
// unsafe raw receipt integers and nonfinite reals are explicitly unsupported:
// silently stringifying them would change both the wire and the facts hash.
func rosterScalar(value any) error {
	switch v := value.(type) {
	case nil, bool:
		return nil
	case string:
		if utf8.ValidString(v) && len(v) <= 32<<20 {
			return nil
		}
	case int64:
		if v >= -9007199254740991 && v <= 9007199254740991 {
			return nil
		}
	case float64:
		if !math.IsInf(v, 0) && !math.IsNaN(v) {
			return nil
		}
	}
	return ErrCharacterInputUnsupported
}
func rosterSchema(ctx context.Context, conn *sql.Conn) error {
	required := []struct {
		table   string
		columns []string
	}{
		{"character_identity_meta", []string{"id", "revision"}},
		{"characters", []string{"character_id", "retired", "name", "role", "gender", "age", "appearance", "personality", "background", "abilities", "motivation", "relationships", "arc", "notes", "cs_location", "cs_power_level", "cs_physical_state", "cs_mental_state", "cs_key_items", "cs_recent_events", "cs_updated_at_chapter", "cs_provenance"}},
		{"character_relationships", []string{"relationship_id", "source_character_id", "target_character_id", "relation"}},
		{"character_aliases", []string{"character_id", "name", "source_key", "valid_from", "valid_through"}},
		{"character_roster_meta", []string{"id", "schema_version", "revision", "migration_state", "legacy_markdown", "projection_hash", "fact_hash"}},
		{"project_core", []string{"id", "writing_language", "characters_arch"}},
	}
	for _, table := range required {
		rows, err := rrMaps(ctx, conn, "PRAGMA table_info("+table.table+")")
		if err != nil {
			return err
		}
		columns := map[string]bool{}
		for _, row := range rows {
			columns[rosterText(row["name"])] = true
		}
		for _, column := range table.columns {
			if !columns[column] {
				if table.table == "character_roster_meta" || table.table == "project_core" {
					return ErrCharacterRosterSchemaUnsupported
				}
				return ErrCharacterIDSchemaRequired
			}
		}
	}
	return nil
}

// JSON.parse preserves string-key insertion order, but enumerates array-index
// keys first. Duplicate keys replace their value without moving their position.
// Do not replace this with map decoding: even nested opaque provenance fields
// participate in the historical JSON.stringify facts hash.
func rosterJSONValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := token.(json.Delim); ok {
		switch delimiter {
		case '{':
			out := plotObject{}
			positions := map[string]int{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, err
				}
				key := keyToken.(string)
				value, err := rosterJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				if i, ok := positions[key]; ok {
					out[i].value = value
				} else {
					positions[key] = len(out)
					out = append(out, plotField{key, value})
				}
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			sort.SliceStable(out, func(i, j int) bool {
				a, ai := rosterIndex(out[i].key)
				b, bi := rosterIndex(out[j].key)
				if ai != bi {
					return ai
				}
				return ai && a < b
			})
			return out, nil
		case '[':
			out := []any{}
			for decoder.More() {
				value, err := rosterJSONValue(decoder)
				if err != nil {
					return nil, err
				}
				out = append(out, value)
			}
			if _, err := decoder.Token(); err != nil {
				return nil, err
			}
			return out, nil
		}
	}
	if number, ok := token.(json.Number); ok {
		f, err := number.Float64()
		if math.IsInf(f, 0) || math.IsNaN(f) || (err != nil && f != 0) {
			return nil, ErrCharacterInputUnsupported
		}
		return f, nil
	}
	return token, nil
}
func rosterIndex(key string) (uint64, bool) {
	n, err := strconv.ParseUint(key, 10, 32)
	return n, err == nil && n < 4294967295 && strconv.FormatUint(n, 10) == key
}
func rosterProvenance(raw any) (plotObject, error) {
	text, ok := raw.(string)
	if !ok || strings.TrimFunc(text, narrativeSpace) == "" {
		return nil, nil
	}
	if err := summaryJSONSupported(text); err != nil {
		return nil, ErrCharacterInputUnsupported
	}
	if !json.Valid([]byte(text)) {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	value, err := rosterJSONValue(decoder)
	if err != nil {
		return nil, ErrCharacterInputUnsupported
	}
	object, _ := value.(plotObject)
	return object, nil
}
func rosterStructured(raw string) bool {
	var parsed any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if !json.Valid([]byte(raw)) || decoder.Decode(&parsed) != nil {
		return false
	}
	values, ok := parsed.([]any)
	if !ok {
		return false
	}
	for _, value := range values {
		row, ok := value.(map[string]any)
		if !ok {
			return false
		}
		if _, ok := row["target"].(string); !ok {
			return false
		}
		if _, ok := row["relation"].(string); !ok {
			return false
		}
	}
	return true
}

func rosterEntry(character map[string]any) (map[string]any, error) {
	entry := map[string]any{}
	for _, key := range []string{"name", "gender", "age", "appearance", "personality", "background", "abilities", "motivation", "arc", "notes"} {
		if err := rosterScalar(character[key]); err != nil {
			return nil, err
		}
		entry[key] = character[key]
	}
	if _, ok := entry["name"].(string); !ok {
		return nil, ErrCharacterInputUnsupported
	}
	entry["role"] = rosterRole(character["role"])
	if state, exists := character["currentState"]; exists {
		row, ok := state.(map[string]any)
		if !ok {
			return nil, ErrCharacterInputUnsupported
		}
		for _, key := range []string{"location", "powerLevel", "physicalState", "mentalState", "keyItems", "recentEvents", "updatedAtChapter"} {
			if err := rosterScalar(row[key]); err != nil {
				return nil, err
			}
		}
		entry["currentState"] = state
	}
	raw, ok := character["relationships"].(string)
	if !ok {
		return nil, ErrCharacterInputUnsupported
	}
	if !rosterStructured(raw) && raw != "" {
		entry["legacyRelationshipNotes"] = raw
	}
	entry["relationships"] = []map[string]any{}
	return entry, nil
}

func rosterEntriesJSON(entries []map[string]any, provenance map[string]plotObject, identity bool) (string, error) {
	canonical := append([]map[string]any{}, entries...)
	sort.SliceStable(canonical, func(i, j int) bool {
		if identity && canonical[i]["characterId"] != canonical[j]["characterId"] {
			return rosterLess(rosterText(canonical[i]["characterId"]), rosterText(canonical[j]["characterId"]))
		}
		return rosterLess(rosterText(canonical[i]["name"]), rosterText(canonical[j]["name"]))
	})
	ordered := []any{}
	for _, entry := range canonical {
		relations := append([]map[string]any{}, entry["relationships"].([]map[string]any)...)
		sort.SliceStable(relations, func(i, j int) bool {
			for _, key := range []string{"targetCharacterId", "target", "relation"} {
				if key == "targetCharacterId" && !identity {
					continue
				}
				a, b := rosterText(relations[i][key]), rosterText(relations[j][key])
				if a != b {
					return rosterLess(a, b)
				}
			}
			return false
		})
		edges := []any{}
		for _, relation := range relations {
			edge := plotOrdered(relation, "target", "relation")
			if identity {
				edge = append(edge, plotField{"targetCharacterId", relation["targetCharacterId"]})
			}
			edges = append(edges, edge)
		}
		object := plotOrdered(entry, "name", "role", "gender", "age", "appearance", "personality", "background", "abilities", "motivation")
		object = append(object, plotField{"relationships", edges})
		object = append(object, plotOrdered(entry, "arc", "notes")...)
		if state, ok := entry["currentState"].(map[string]any); ok {
			current := plotOrdered(state, "location", "powerLevel", "physicalState", "mentalState", "keyItems", "recentEvents", "updatedAtChapter")
			if raw := provenance[rosterText(entry["characterId"])]; len(raw) > 0 {
				current = append(current, plotField{"provenance", raw})
			}
			object = append(object, plotField{"currentState", current})
		}
		object = append(object, plotOrdered(entry, "legacyRelationshipNotes")...)
		if identity {
			object = append(object, plotField{"characterId", entry["characterId"]})
		}
		ordered = append(ordered, object)
	}
	return plotJSON(ordered)
}
func rosterMarkdown(entries []map[string]any, english bool) string {
	if len(entries) == 0 {
		return ""
	}
	sorted := append([]map[string]any{}, entries...)
	rosterSort(sorted)
	zhRoles := map[string]string{"protagonist": "主角", "supporting": "配角", "antagonist": "反派", "minor": "龙套"}
	enRoles := map[string]string{"protagonist": "Protagonist", "supporting": "Supporting character", "antagonist": "Antagonist", "minor": "Minor character"}
	heading, colon := "# 角色图谱", "："
	if english {
		heading, colon = "# Character graph", ": "
	}
	blocks := []string{heading}
	fields := []struct{ key, zh, en string }{{"gender", "性别", "Gender"}, {"age", "年龄", "Age"}, {"appearance", "外貌", "Appearance"}, {"personality", "性格", "Personality"}, {"background", "背景", "Background"}, {"abilities", "能力", "Abilities"}, {"motivation", "动机", "Motivation"}, {"arc", "弧光", "Arc"}, {"notes", "备注", "Notes"}}
	for _, entry := range sorted {
		role := zhRoles[rosterText(entry["role"])]
		if english {
			role = enRoles[rosterText(entry["role"])]
		}
		lines := []string{"## " + role + colon + rosterText(entry["name"])}
		for _, field := range fields {
			if phTruthy(entry[field.key]) {
				label := field.zh
				if english {
					label = field.en
				}
				lines = append(lines, "- "+label+colon+summaryJSString(entry[field.key]))
			}
		}
		for _, edge := range entry["relationships"].([]map[string]any) {
			line := "- 关系：" + rosterText(edge["target"]) + "（" + rosterText(edge["relation"]) + "）"
			if english {
				line = "- Relationship: " + rosterText(edge["target"]) + " (" + rosterText(edge["relation"]) + ")"
			}
			lines = append(lines, line)
		}
		if note, ok := entry["legacyRelationshipNotes"].(string); ok && note != "" {
			label := "关系备注"
			if english {
				label = "Relationship notes"
			}
			lines = append(lines, "- "+label+colon+note)
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}

func charactersRoster(ctx context.Context, conn *sql.Conn) (map[string]any, error) {
	if err := rosterSchema(ctx, conn); err != nil {
		return nil, err
	}
	metas, err := rrMaps(ctx, conn, "SELECT schema_version,revision,migration_state,legacy_markdown,projection_hash,fact_hash FROM character_roster_meta WHERE id='main'")
	if err != nil {
		return nil, err
	}
	if len(metas) == 0 {
		return nil, ErrCharacterRosterMetaMissing
	}
	meta := metas[0]
	for _, value := range meta {
		if err := rosterScalar(value); err != nil {
			return nil, err
		}
	}
	state := rosterText(meta["migration_state"])
	switch state {
	case "empty", "legacy_cards_preserved", "legacy_markdown_pending", "ready":
	default:
		return nil, ErrCharacterInputUnsupported
	}
	for _, key := range []string{"legacy_markdown", "projection_hash", "fact_hash"} {
		if _, ok := meta[key].(string); !ok {
			return nil, ErrCharacterInputUnsupported
		}
	}
	characters, err := charactersAll(ctx, conn)
	if err != nil {
		return nil, err
	}
	rawRows, err := rrMaps(ctx, conn, "SELECT character_id,cs_provenance,cs_updated_at_chapter FROM characters WHERE retired=0")
	if err != nil {
		return nil, err
	}
	provenance := map[string]plotObject{}
	active := map[string]bool{}
	for _, row := range rawRows {
		id, ok := row["character_id"].(string)
		if !ok || rosterScalar(id) != nil {
			return nil, ErrCharacterInputUnsupported
		}
		active[id] = true
		if row["cs_updated_at_chapter"] != nil {
			raw, err := rosterProvenance(row["cs_provenance"])
			if err != nil {
				return nil, err
			}
			provenance[id] = raw
		}
	}
	relationships, err := rrMaps(ctx, conn, `SELECT r.source_character_id,r.target_character_id,c.name,r.relation FROM character_relationships r JOIN characters c ON c.character_id=r.target_character_id WHERE c.retired=0 ORDER BY r.relationship_id`)
	if err != nil {
		return nil, err
	}
	entries := []map[string]any{}
	for _, character := range characters {
		id, ok := character["characterId"].(string)
		if !ok {
			return nil, ErrCharacterInputUnsupported
		}
		if !active[id] {
			continue
		}
		entry, err := rosterEntry(character)
		if err != nil {
			return nil, err
		}
		entry["characterId"] = id
		edges := []map[string]any{}
		for _, row := range relationships {
			if row["source_character_id"] == id {
				for _, key := range []string{"target_character_id", "name", "relation"} {
					text, ok := row[key].(string)
					if !ok || rosterScalar(text) != nil {
						return nil, ErrCharacterInputUnsupported
					}
				}
				edges = append(edges, map[string]any{"target": row["name"], "relation": row["relation"], "targetCharacterId": row["target_character_id"]})
			}
		}
		entry["relationships"] = edges
		entries = append(entries, entry)
	}
	rosterSort(entries)
	core, err := rrMaps(ctx, conn, "SELECT writing_language,COALESCE(characters_arch,'') AS characters_arch FROM project_core WHERE id='main'")
	if err != nil {
		return nil, err
	}
	english, projection := false, ""
	if len(core) > 0 {
		english = core[0]["writing_language"] == "en-US"
		text, ok := core[0]["characters_arch"].(string)
		if !ok || rosterScalar(text) != nil {
			return nil, ErrCharacterInputUnsupported
		}
		projection = text
	}
	rendered, previous := rosterMarkdown(entries, english), rosterMarkdown(entries, !english)
	if state == "ready" && projection == previous && meta["projection_hash"] == rosterHash(previous) {
		rendered = previous
	}
	nameJSON, err := rosterEntriesJSON(entries, provenance, false)
	if err != nil {
		return nil, err
	}
	identityJSON, err := rosterEntriesJSON(entries, provenance, true)
	if err != nil {
		return nil, err
	}
	status := "inconsistent"
	switch state {
	case "empty":
		if len(entries) == 0 && strings.TrimFunc(rosterText(meta["legacy_markdown"]), narrativeSpace) == "" && strings.TrimFunc(projection, narrativeSpace) == "" {
			status = "empty"
		}
	case "legacy_markdown_pending":
		if len(entries) == 0 && strings.TrimFunc(rosterText(meta["legacy_markdown"]), narrativeSpace) != "" {
			status = "legacy_repair_required"
		}
	case "ready":
		if meta["projection_hash"] == rosterHash(rendered) && meta["fact_hash"] == rosterHash(nameJSON) && projection == rendered {
			status = "empty"
			if len(entries) > 0 {
				status = "ready"
			}
		}
	}
	revisions, err := rrMaps(ctx, conn, "SELECT revision FROM character_identity_meta WHERE id='main'")
	if err != nil {
		return nil, err
	}
	if len(revisions) == 0 {
		return nil, ErrCharacterIDSchemaRequired
	}
	if err := rosterScalar(revisions[0]["revision"]); err != nil {
		return nil, err
	}
	aliases, err := rrMaps(ctx, conn, `SELECT a.character_id AS characterId,a.name,a.source_key AS sourceKey,a.valid_from AS validFrom,a.valid_through AS validThrough FROM character_aliases a JOIN characters c ON c.character_id=a.character_id WHERE c.retired=0 ORDER BY a.character_id,a.name,a.source_key,a.valid_from`)
	if err != nil {
		return nil, err
	}
	for _, alias := range aliases {
		for _, value := range alias {
			if err := rosterScalar(value); err != nil {
				return nil, err
			}
		}
	}
	result := map[string]any{"schemaVersion": int64(1), "revision": meta["revision"], "migrationState": state, "status": status, "entries": entries, "identityRevision": revisions[0]["revision"], "aliases": aliases, "renderedMarkdown": rendered, "projectionHash": rosterHash(rendered), "factHash": rosterHash(identityJSON), "nameOnlyFactHash": meta["fact_hash"]}
	if meta["legacy_markdown"] != "" {
		result["legacyMarkdown"] = meta["legacy_markdown"]
	}
	return result, nil
}
