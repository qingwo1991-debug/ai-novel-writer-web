package admission

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
)

// verifyCharacterIdentity ports verifyM02CharacterIdentity, the domain verifier
// used by the installed desktop V7 migration. It never repairs missing metadata
// or changes author rows. Custom test contracts without this table do not invoke
// desktop business invariants; production contracts come only from the registry.
func verifyCharacterIdentity(ctx context.Context, tx *sql.Tx) error {
	invalid := errors.New("character identity invariant")
	var foreignKeys int
	if err := tx.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil { return err }
	if foreignKeys != 1 { return invalid }
	meta, err := queryObjects(ctx, tx, "SELECT revision,legacy_count FROM character_identity_meta WHERE id='main'")
	if err != nil { return err }
	if len(meta) != 1 { return invalid }
	revision, validRevision := safeRevision(meta[0]["revision"])
	legacyCount, validCount := safeRevision(meta[0]["legacy_count"])
	if !validRevision || !validCount { return invalid }
	origins, err := queryObjects(ctx, tx, "SELECT o.original_row_json,o.original_hash,c.legacy_key FROM character_identity_origins o JOIN characters c ON c.character_id=o.character_id")
	if err != nil { return err }
	if int64(len(origins)) != legacyCount { return invalid }
	for _, origin := range origins {
		original, ok := origin["original_row_json"].(string)
		if !ok { return invalid }
		digest := sha256.Sum256([]byte(original))
		if origin["original_hash"] != hex.EncodeToString(digest[:]) { return invalid }
		var parsed map[string]json.RawMessage
		if err := json.Unmarshal([]byte(original), &parsed); err != nil { return invalid }
		name, exists := parsed["name"]
		if !exists { return invalid }
		var value any
		if err := json.Unmarshal(name, &value); err != nil { return invalid }
		switch value := value.(type) {
		case nil:
			if origin["legacy_key"] != nil { return invalid }
		case string:
			if origin["legacy_key"] != value { return invalid }
		default: return invalid
		}
	}
	characters, err := queryObjects(ctx, tx, "SELECT character_id,identity_revision FROM characters")
	if err != nil { return err }
	ids := make(map[string]bool, len(characters))
	for _, character := range characters {
		id, valid := nonemptyString(character["character_id"])
		ownRevision, validOwn := safeRevision(character["identity_revision"])
		if !valid || !validOwn || ownRevision > revision { return invalid }
		ids[id] = true
	}
	proposals, err := queryObjects(ctx, tx, "SELECT candidate_ids_json,resolved_character_id,approval_id FROM character_identity_proposals")
	if err != nil { return err }
	for _, proposal := range proposals {
		encoded, ok := proposal["candidate_ids_json"].(string)
		if !ok { return invalid }
		var candidates []any
		if err := json.Unmarshal([]byte(encoded), &candidates); err != nil || strings.TrimSpace(encoded) == "null" { return invalid }
		seen := make(map[string]bool)
		for _, candidate := range candidates {
			id, ok := candidate.(string)
			if !ok || !ids[id] || seen[id] { return invalid }
			seen[id] = true
		}
		if jsTruthy(proposal["resolved_character_id"]) != jsTruthy(proposal["approval_id"]) { return invalid }
	}
	aliases, err := queryObjects(ctx, tx, "SELECT character_id,name,source_key,valid_from,valid_through FROM character_aliases")
	if err != nil { return err }
	for _, alias := range aliases {
		id, validID := nonemptyString(alias["character_id"])
		_, validName := nonemptyString(alias["name"])
		_, validSource := nonemptyString(alias["source_key"])
		from, validFrom := safeRevision(alias["valid_from"])
		if !validID || !ids[id] || !validName || !validSource || !validFrom || from > revision { return invalid }
		if alias["valid_through"] != nil {
			through, valid := safeRevision(alias["valid_through"])
			if !valid || through < from || through > revision { return invalid }
		}
	}
	return nil
}

func queryObjects(ctx context.Context, tx *sql.Tx, query string) ([]map[string]any, error) {
	rows, err := tx.QueryContext(ctx, query)
	if err != nil { return nil, err }
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil { return nil, err }
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range pointers { pointers[i] = &values[i] }
		if err := rows.Scan(pointers...); err != nil { return nil, err }
		object := make(map[string]any, len(columns))
		for i, name := range columns { object[name] = values[i] }
		result = append(result, object)
	}
	return result, rows.Err()
}

func safeRevision(value any) (int64, bool) {
	const maxSafe = int64(9007199254740991)
	switch value := value.(type) {
	case int64: return value, value >= 0 && value <= maxSafe
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > float64(maxSafe) || math.Trunc(value) != value { return 0, false }
		return int64(value), true
	default: return 0, false
	}
}

func nonemptyString(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok && len(strings.TrimFunc(text, jsWhitespace)) > 0
}

func jsWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff': return true
	default: return r >= '\u2000' && r <= '\u200a'
	}
}

func jsTruthy(value any) bool {
	switch value := value.(type) {
	case nil: return false
	case string: return value != ""
	case int64: return value != 0
	case float64: return value != 0 && !math.IsNaN(value)
	case bool: return value
	default: return true
	}
}
