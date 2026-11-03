package admission

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

// This is byte-for-byte M02_CHARACTER_AUXILIARY_SQL from the installed desktop
// migration. SQLite includes the original whitespace in its schema identity.
const m02CharacterAuxiliarySQL = `
CREATE TABLE character_identity_origins (
 character_id TEXT PRIMARY KEY REFERENCES characters(character_id), source_key TEXT NOT NULL UNIQUE,
 original_row_json TEXT NOT NULL, original_hash TEXT NOT NULL
);
CREATE TABLE character_aliases (
 character_id TEXT NOT NULL REFERENCES characters(character_id), name TEXT NOT NULL,
 source_key TEXT NOT NULL, valid_from INTEGER NOT NULL, valid_through INTEGER,
 PRIMARY KEY(character_id,name,source_key,valid_from)
);
CREATE TABLE character_identity_proposals (
 proposal_id TEXT PRIMARY KEY, owner_character_id TEXT REFERENCES characters(character_id),
 source_key TEXT NOT NULL, source_hash TEXT NOT NULL, raw_value TEXT NOT NULL, candidate_ids_json TEXT NOT NULL,
 resolved_character_id TEXT REFERENCES characters(character_id), approval_id TEXT REFERENCES character_identity_approvals(operation_id)
);
CREATE TABLE character_identity_approvals (
 operation_id TEXT PRIMARY KEY, payload_hash TEXT NOT NULL, receipt_json TEXT NOT NULL
);
CREATE TABLE character_relationships (
 relationship_id TEXT PRIMARY KEY,
 source_character_id TEXT NOT NULL REFERENCES characters(character_id),
 target_character_id TEXT NOT NULL REFERENCES characters(character_id),
 relation TEXT NOT NULL, source_display_snapshot TEXT NOT NULL, target_display_snapshot TEXT NOT NULL,
 provenance_json TEXT NOT NULL, approval_id TEXT NOT NULL REFERENCES character_identity_approvals(operation_id)
);
CREATE TABLE character_identity_meta (
 id TEXT PRIMARY KEY CHECK(id='main'), revision INTEGER NOT NULL, legacy_count INTEGER NOT NULL
);
INSERT INTO character_identity_meta VALUES('main',0,0);
`

var m02CreateCharacters = regexp.MustCompile(`CREATE TABLE(?: IF NOT EXISTS)? characters`)

type m02Row struct {
	values                        []any
	id, name, sourceKey, original string
}

// UpgradeSnapshotV2 applies only the installed M02 step to an authorized,
// exclusive private copy. It never opens or binds an author's source path.
// The narrow lane accepts original baseline/donor v2 fingerprints and legacy
// rows representable by the original JS migration. Non-character tables are
// preserved exactly. Rich historical relationship objects are deliberately
// refused until their original coercion is independently qualified. Random
// UUIDs make physical hashes and full-domain hashes differ
// from the TypeScript oracle; schema, old values, and identity semantics agree.
func UpgradeSnapshotV2(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
	var zero Evidence
	if err := validateContract(expected); err != nil {
		return zero, err
	}
	if ctx == nil {
		return zero, failure(CodeUpgradeUnsupported, nil)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	absolute, err := snapshotPath(path)
	if err != nil {
		return zero, err
	}
	if err := privateUpgradePath(absolute); err != nil {
		return zero, err
	}
	before, err := snapshotIdentity(ctx, absolute)
	if err != nil {
		return zero, err
	}
	db, err := openEarlySnapshot(absolute, false)
	if err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	defer tx.Rollback()
	tables, ddl, columns, err := checkM02Source(ctx, tx, expected)
	if err != nil {
		return zero, err
	}
	otherTables := make([]string, 0, len(tables)-1)
	for _, table := range tables {
		if table != "characters" {
			otherTables = append(otherTables, table)
		}
	}
	domainBefore, err := logicalDomain(ctx, tx, otherTables)
	if err != nil {
		return zero, failure(CodeDomain, err)
	}
	rows, err := readM02Characters(ctx, tx, columns)
	if err != nil {
		return zero, err
	}
	blueprints, err := readM02Blueprints(ctx, tx)
	if err != nil {
		return zero, err
	}
	if err := privateUpgradePath(absolute); err != nil {
		return zero, err
	}
	still, err := snapshotIdentity(ctx, absolute)
	if err != nil {
		return zero, err
	}
	if still != before {
		return zero, failure(CodeSnapshotChanged, nil)
	}
	if err := applyM02(ctx, tx, ddl, columns, rows, blueprints); err != nil {
		return zero, err
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 3"); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	finalTables, _, err := checkM02Target(ctx, tx, expected)
	if err != nil {
		return zero, err
	}
	if len(finalTables) != len(tables)+6 {
		return zero, failure(CodeUnrecognizedSchema, nil)
	}
	domainAfter, err := logicalDomain(ctx, tx, otherTables)
	if err != nil {
		return zero, failure(CodeDomain, err)
	}
	if !reflect.DeepEqual(domainBefore, domainAfter) {
		return zero, failure(CodeDomain, nil)
	}
	if err := verifyM02OldValues(ctx, tx, columns, rows); err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	if err := db.Close(); err != nil {
		return zero, failure(CodeUpgradeFailed, err)
	}
	return verifyM02SnapshotV3(ctx, absolute, expected)
}

func checkM02Version(ctx context.Context, tx *sql.Tx, stage int) error {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return failure(CodeIntegrity, err)
	}
	if version < 0 {
		return failure(CodeInvalidVersion, nil)
	}
	if version > CurrentSchemaVersion {
		return failure(CodeNewerSchema, nil)
	}
	if version != stage {
		return failure(CodeUpgradeUnsupported, nil)
	}
	if err := checkIntegrity(ctx, tx); err != nil {
		return err
	}
	return checkForeignKeys(ctx, tx)
}

func checkM02Source(ctx context.Context, tx *sql.Tx, expected SchemaContract) ([]string, string, []string, error) {
	if err := checkM02Version(ctx, tx, 2); err != nil {
		return nil, "", nil, err
	}
	fingerprint, tables, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return nil, "", nil, failure(CodeIntegrity, err)
	}
	if !upgradeFingerprintMatches(expected, 2, fingerprint) {
		return nil, "", nil, failure(CodeUnrecognizedSchema, nil)
	}
	var ddl string
	if err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='characters'").Scan(&ddl); err != nil {
		return nil, "", nil, failure(CodeUpgradeUnsupported, err)
	}
	if !strings.Contains(ddl, "name TEXT PRIMARY KEY") || !m02CreateCharacters.MatchString(ddl) {
		return nil, "", nil, failure(CodeUpgradeUnsupported, nil)
	}
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info(characters)")
	if err != nil {
		return nil, "", nil, failure(CodeIntegrity, err)
	}
	defer rows.Close()
	columns := []string{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &defaultValue, &pk); err != nil {
			return nil, "", nil, failure(CodeIntegrity, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, "", nil, failure(CodeIntegrity, err)
	}
	if len(columns) == 0 || columns[0] != "name" {
		return nil, "", nil, failure(CodeUpgradeUnsupported, nil)
	}
	return tables, ddl, columns, nil
}

func checkM02Target(ctx context.Context, tx *sql.Tx, expected SchemaContract) ([]string, string, error) {
	if err := checkM02Version(ctx, tx, 3); err != nil {
		return nil, "", err
	}
	fingerprint, tables, err := schemaFingerprint(ctx, tx)
	if err != nil {
		return nil, "", failure(CodeIntegrity, err)
	}
	if !upgradeFingerprintMatches(expected, 3, fingerprint) {
		return nil, "", failure(CodeUnrecognizedSchema, nil)
	}
	if err := verifyCharacterIdentity(ctx, tx); err != nil {
		return nil, "", failure(CodeDomain, err)
	}
	return tables, fingerprint, nil
}

func readM02Characters(ctx context.Context, tx *sql.Tx, columns []string) ([]m02Row, error) {
	query := "SELECT * FROM characters ORDER BY rowid"
	result, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, failure(CodeDomain, err)
	}
	defer result.Close()
	rows := []m02Row{}
	for result.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range pointers {
			pointers[i] = &values[i]
		}
		if err := result.Scan(pointers...); err != nil {
			return nil, failure(CodeDomain, err)
		}
		name, ok := values[0].(string)
		if !ok {
			return nil, failure(CodeUpgradeUnsupported, nil)
		}
		id, err := m02UUID()
		if err != nil {
			return nil, failure(CodeUpgradeFailed, err)
		}
		original, err := m02OriginalJSON(columns, values)
		if err != nil {
			return nil, failure(CodeUpgradeUnsupported, err)
		}
		rows = append(rows, m02Row{values: values, id: id, name: name, sourceKey: fmt.Sprintf("legacy:characters:%d", len(rows)), original: original})
	}
	if err := result.Err(); err != nil {
		return nil, failure(CodeDomain, err)
	}
	return rows, nil
}

type m02Blueprint struct {
	chapter int64
	raw     string
	names   []string
}

func readM02Blueprints(ctx context.Context, tx *sql.Tx) ([]m02Blueprint, error) {
	rows, err := tx.QueryContext(ctx, "SELECT chapter_number,characters FROM blueprints")
	if err != nil {
		return nil, failure(CodeDomain, err)
	}
	defer rows.Close()
	var blueprints []m02Blueprint
	for rows.Next() {
		var chapter int64
		var raw any
		if err := rows.Scan(&chapter, &raw); err != nil {
			return nil, failure(CodeDomain, err)
		}
		if chapter > 9007199254740991 || chapter < -9007199254740991 {
			return nil, failure(CodeUpgradeUnsupported, nil)
		}
		if raw == nil || raw == "" {
			continue
		}
		value, ok := raw.(string)
		if !ok {
			return nil, failure(CodeUpgradeUnsupported, nil)
		}
		names, err := m02StringValues(value)
		if err != nil {
			return nil, failure(CodeUpgradeUnsupported, err)
		}
		blueprints = append(blueprints, m02Blueprint{chapter, value, names})
	}
	return blueprints, rows.Err()
}

// The installed migration accepts arbitrary JSON, but the supported private
// lane limits legacy references to plain strings. Object coercion and numeric
// display names require a separate exact JS semantics proof.
func m02StringValues(raw string) ([]string, error) {
	// JSON.parse keeps escaped lone UTF-16 surrogates, while encoding/json
	// substitutes U+FFFD. Refuse that input before decoding or writing DDL.
	if m03LoneSurrogateEscape(raw) {
		return nil, errors.New("unsupported legacy unicode reference")
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return []string{raw}, nil
	}
	array, ok := value.([]any)
	if !ok {
		return []string{raw}, nil
	}
	names := make([]string, len(array))
	for i, item := range array {
		name, ok := item.(string)
		if !ok {
			return nil, errors.New("complex legacy reference")
		}
		names[i] = name
	}
	return names, nil
}

func m02UUID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	data[6] = data[6]&0x0f | 0x40
	data[8] = data[8]&0x3f | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", data[0:4], data[4:6], data[6:8], data[8:10], data[10:16]), nil
}

func m02OriginalJSON(columns []string, values []any) (string, error) {
	encoded := []byte{'{'}
	for i, column := range columns {
		if i > 0 {
			encoded = append(encoded, ',')
		}
		encoded = appendJSString(encoded, column)
		encoded = append(encoded, ':')
		var err error
		encoded, err = appendSQLiteValue(encoded, values[i])
		if err != nil {
			return "", err
		}
	}
	encoded = append(encoded, '}')
	return string(encoded), nil
}

func m02Digest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func applyM02(ctx context.Context, tx *sql.Tx, ddl string, columns []string, rows []m02Row, blueprints []m02Blueprint) error {
	rebuilt := m02CreateCharacters.ReplaceAllString(ddl, "CREATE TABLE characters_m02")
	rebuilt = strings.Replace(rebuilt, "name TEXT PRIMARY KEY", "character_id TEXT PRIMARY KEY, legacy_key TEXT, static_provenance TEXT NOT NULL DEFAULT '{}', identity_revision INTEGER NOT NULL DEFAULT 0, retired INTEGER NOT NULL DEFAULT 0 CHECK(retired IN (0,1)), name TEXT", 1)
	if _, err := tx.ExecContext(ctx, rebuilt); err != nil {
		return failure(CodeUpgradeFailed, err)
	}
	quoted := make([]string, len(columns))
	placeholders := make([]string, len(columns)+3)
	for i, name := range columns {
		quoted[i] = quoteIdentifier(name)
	}
	for i := range placeholders {
		placeholders[i] = "?"
	}
	statement := "INSERT INTO characters_m02(character_id,legacy_key,static_provenance," + strings.Join(quoted, ",") + ") VALUES(" + strings.Join(placeholders, ",") + ")"
	for _, row := range rows {
		provenance := `{"kind":"legacy","sourceKey":"` + row.sourceKey + `"}`
		args := append([]any{row.id, row.name, provenance}, row.values...)
		if _, err := tx.ExecContext(ctx, statement, args...); err != nil {
			return failure(CodeUpgradeFailed, err)
		}
	}
	if _, err := tx.ExecContext(ctx, "DROP TABLE characters; ALTER TABLE characters_m02 RENAME TO characters;"); err != nil {
		return failure(CodeUpgradeFailed, err)
	}
	if _, err := tx.ExecContext(ctx, m02CharacterAuxiliarySQL); err != nil {
		return failure(CodeUpgradeFailed, err)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE character_identity_meta SET legacy_count=? WHERE id='main'", len(rows)); err != nil {
		return failure(CodeUpgradeFailed, err)
	}
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, "INSERT INTO character_identity_origins VALUES(?,?,?,?)", row.id, row.sourceKey, row.original, m02Digest(row.original)); err != nil {
			return failure(CodeUpgradeFailed, err)
		}
		if strings.TrimFunc(row.name, jsWhitespace) != "" {
			if _, err := tx.ExecContext(ctx, "INSERT INTO character_aliases VALUES(?,?,?,0,NULL)", row.id, row.name, row.sourceKey); err != nil {
				return failure(CodeUpgradeFailed, err)
			}
		}
		for i, column := range columns {
			if column != "relationships" {
				continue
			}
			raw, ok := row.values[i].(string)
			if !ok || raw == "" {
				break
			}
			names, err := m02StringValues(raw)
			if err != nil {
				return failure(CodeUpgradeUnsupported, err)
			}
			for index, name := range names {
				// The original migration only derives relationship candidates
				// from an object target field. Plain strings remain unbound.
				if err := insertM02Proposal(ctx, tx, nil, row.id, fmt.Sprintf("%s:relationships:%d", row.sourceKey, index), raw, name); err != nil {
					return err
				}
			}
			break
		}
	}
	for _, blueprint := range blueprints {
		for index, name := range blueprint.names {
			if err := insertM02Proposal(ctx, tx, rows, nil, fmt.Sprintf("legacy:blueprints:%d:characters:%d", blueprint.chapter, index), blueprint.raw, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func insertM02Proposal(ctx context.Context, tx *sql.Tx, rows []m02Row, owner any, key, source, raw string) error {
	id, err := m02UUID()
	if err != nil {
		return failure(CodeUpgradeFailed, err)
	}
	candidates := make([]string, 0)
	for _, row := range rows {
		if row.name == raw {
			candidates = append(candidates, row.id)
		}
	}
	encoded, err := json.Marshal(candidates)
	if err != nil {
		return failure(CodeUpgradeFailed, err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO character_identity_proposals VALUES(?,?,?,?,?,?,NULL,NULL)", id, owner, key, m02Digest(source), raw, string(encoded)); err != nil {
		return failure(CodeUpgradeFailed, err)
	}
	return nil
}

func verifyM02OldValues(ctx context.Context, tx *sql.Tx, columns []string, rows []m02Row) error {
	query := "SELECT " + strings.Join(func() []string {
		quoted := make([]string, len(columns))
		for i, v := range columns {
			quoted[i] = quoteIdentifier(v)
		}
		return quoted
	}(), ",") + " FROM characters WHERE character_id=?"
	for _, row := range rows {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range pointers {
			pointers[i] = &values[i]
		}
		if err := tx.QueryRowContext(ctx, query, row.id).Scan(pointers...); err != nil {
			return failure(CodeDomain, err)
		}
		if !reflect.DeepEqual(values, row.values) {
			return failure(CodeDomain, nil)
		}
	}
	return nil
}

func verifyM02SnapshotV3(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
	var zero Evidence
	if err := privateUpgradePath(path); err != nil {
		return zero, err
	}
	before, err := snapshotIdentity(ctx, path)
	if err != nil {
		return zero, err
	}
	db, err := openEarlySnapshot(path, true)
	if err != nil {
		return zero, failure(CodeSnapshotRead, err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return zero, failure(CodeIntegrity, err)
	}
	defer tx.Rollback()
	tables, fingerprint, err := checkM02Target(ctx, tx, expected)
	if err != nil {
		return zero, err
	}
	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
			return zero, failure(CodeIntegrity, err)
		}
		counts[table] = count
	}
	domain, err := logicalDomain(ctx, tx, tables)
	if err != nil {
		return zero, failure(CodeDomain, err)
	}
	if err := tx.Rollback(); err != nil {
		return zero, failure(CodeSnapshotRead, err)
	}
	if err := db.Close(); err != nil {
		return zero, failure(CodeSnapshotRead, err)
	}
	if err := privateUpgradePath(path); err != nil {
		return zero, err
	}
	after, err := snapshotIdentity(ctx, path)
	if err != nil {
		return zero, err
	}
	if before != after {
		return zero, failure(CodeSnapshotChanged, nil)
	}
	return Evidence{UserVersion: 3, SchemaFingerprint: fingerprint, TableCounts: counts, ContentIdentity: ContentIdentity{Database: before.database, WAL: before.wal}, Domain: &domain}, nil
}
