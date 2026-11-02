// Package admission verifies already-copied, private SQLite snapshots. It never
// copies or opens the author's original database and never creates or migrates
// a database. The caller must exclude writers and own the snapshot directory.
package admission

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	_ "github.com/mattn/go-sqlite3"
)

const CurrentSchemaVersion = 7

const (
	CodeInvalidContract = "SCHEMA_CONTRACT_INVALID"
	CodeMigrationRequired = "SCHEMA_MIGRATION_REQUIRED"
	CodeNewerSchema = "SCHEMA_NEWER_UNSUPPORTED"
	CodeInvalidVersion = "SCHEMA_VERSION_INVALID"
	CodeUnrecognizedSchema = "SCHEMA_UNRECOGNIZED"
	CodeIntegrity = "SCHEMA_INTEGRITY_FAILED"
	CodeForeignKey = "SCHEMA_FOREIGN_KEY_FAILED"
	CodeDomain = "SCHEMA_DOMAIN_INVARIANT_FAILED"
	CodeUnsafeSnapshot = "SNAPSHOT_UNSAFE_FILE"
	CodeMissingSnapshot = "SNAPSHOT_MISSING"
	CodeSnapshotChanged = "SNAPSHOT_CHANGED"
	CodeSnapshotRead = "SNAPSHOT_READ_FAILED"
)

// Error exposes a stable code without leaking SQLite errors containing author
// values or file paths. Unwrap preserves cancellation and diagnostic causes.
type Error struct {
	Code string
	cause error
}

func (e *Error) Error() string { return e.Code }
func (e *Error) Unwrap() error { return e.cause }

func failure(code string, cause error) error { return &Error{Code: code, cause: cause} }

// VersionContract is supplied by a trusted application contract generated from
// the TypeScript migration registry, never by the project being admitted.
type VersionContract struct {
	Version int `json:"version"`
	Fingerprints []string `json:"fingerprints"`
}

type SchemaContract []VersionContract

// ContentIdentity matches readSqliteContentIdentity: the SHA256 of the main DB
// and committed-WAL container bytes; an absent WAL has the empty-byte digest.
// This is physical preservation evidence, not a logical row/content hash.
type ContentIdentity struct {
	Database string `json:"database"`
	WAL string `json:"wal"`
}

type Evidence struct {
	UserVersion int `json:"userVersion"`
	SchemaFingerprint string `json:"schemaFingerprint"`
	TableCounts map[string]int64 `json:"tableCounts"`
	ContentIdentity ContentIdentity `json:"contentIdentity"`
	Domain *DomainEvidence `json:"domain,omitempty"`
}

func validateContract(expected SchemaContract) error {
	seen := make(map[string]bool)
	current := false
	for _, entry := range expected {
		if entry.Version < 0 || entry.Version > CurrentSchemaVersion || len(entry.Fingerprints) == 0 {
			return failure(CodeInvalidContract, nil)
		}
		for _, fingerprint := range entry.Fingerprints {
			decoded, err := hex.DecodeString(fingerprint)
			if err != nil || len(decoded) != sha256.Size || fingerprint != strings.ToLower(fingerprint) {
				return failure(CodeInvalidContract, nil)
			}
			// The same DDL can legitimately be registered at two versions.
			key := string(rune(entry.Version)) + ":" + fingerprint
			if seen[key] { return failure(CodeInvalidContract, nil) }
			seen[key] = true
			if entry.Version == CurrentSchemaVersion { current = true }
		}
	}
	if !current { return failure(CodeInvalidContract, nil) }
	return nil
}

// VerifySnapshot uses mode=ro (not immutable=1: immutable can ignore copied WAL
// frames). SQLite may update reader marks in the PRIVATE copied -shm, but cannot
// write the DB or WAL. No journal mode, checkpoint, DDL, or migration is issued.
// All reads share one connection/transaction; pre/post byte identities detect
// drift. Sidecars must be regular files in this same caller-owned directory.
func VerifySnapshot(ctx context.Context, path string, expected SchemaContract) (Evidence, error) {
 return verifySnapshot(ctx,path,expected,false)
}
// VerifySnapshotWithDomain adds the original migration lane's logical row proof.
// It is deliberately opt-in: opening current projects must not sort all author
// text in memory merely to inspect their schema. This temporary implementation
// fails closed above 64MiB encoded rows; external sorting is still required.
func VerifySnapshotWithDomain(ctx context.Context,path string,expected SchemaContract)(Evidence,error){
 return verifySnapshot(ctx,path,expected,true)
}
func verifySnapshot(ctx context.Context, path string, expected SchemaContract, withDomain bool) (Evidence, error) {
	var zero Evidence
	if err := validateContract(expected); err != nil { return zero, err }
	if err := ctx.Err(); err != nil { return zero, err }
	absolute, err := snapshotPath(path)
	if err != nil { return zero, err }
	before, err := snapshotIdentity(ctx, absolute)
	if err != nil { return zero, err }
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := url.Values{}
	query.Set("mode", "ro")
	query.Set("_busy_timeout", "5000")
	query.Set("_foreign_keys", "on")
	query.Set("_query_only", "on")
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil { return zero, failure(CodeSnapshotRead, err) }
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil { return zero, failure(CodeIntegrity, err) }
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return zero, failure(CodeIntegrity, err)
	}
	if err := checkIntegrity(ctx, tx); err != nil { return zero, err }
	if err := checkForeignKeys(ctx, tx); err != nil { return zero, err }
	if version < 0 { return zero, failure(CodeInvalidVersion, nil) }
	if version > CurrentSchemaVersion { return zero, failure(CodeNewerSchema, nil) }
	if version < CurrentSchemaVersion { return zero, failure(CodeMigrationRequired, nil) }
	fingerprint, tables, err := schemaFingerprint(ctx, tx)
	if err != nil { return zero, failure(CodeIntegrity, err) }
	matched := false
	for _, entry := range expected {
		if entry.Version != version { continue }
		for _, accepted := range entry.Fingerprints {
			if fingerprint == accepted { matched = true }
		}
	}
	if !matched || len(tables) == 0 { return zero, failure(CodeUnrecognizedSchema, nil) }
	counts := make(map[string]int64, len(tables))
	for _, table := range tables {
		var count int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdentifier(table)).Scan(&count); err != nil {
			return zero, failure(CodeIntegrity, err)
		}
		counts[table] = count
	}
	if _, desktop := counts["character_identity_meta"]; desktop {
		if err := verifyCharacterIdentity(ctx, tx); err != nil { return zero, failure(CodeDomain, err) }
	}
 var domain *DomainEvidence
 if withDomain{proof,err:=logicalDomain(ctx,tx,tables);if err!=nil{return zero,failure(CodeDomain,err)};domain=&proof}
	// Closing the transaction/connection before hashing also catches any unexpected
	// close-time persistent changes. ROLLBACK is read-only and never checkpoints.
	if err := tx.Rollback(); err != nil { return zero, failure(CodeSnapshotRead, err) }
	if err := db.Close(); err != nil { return zero, failure(CodeSnapshotRead, err) }
	after, err := snapshotIdentity(ctx, absolute)
	if err != nil { return zero, err }
	if before != after { return zero, failure(CodeSnapshotChanged, nil) }
	return Evidence{UserVersion: version, SchemaFingerprint: fingerprint, TableCounts: counts,
		ContentIdentity: ContentIdentity{Database: before.database, WAL: before.wal},Domain:domain}, nil
}

func checkIntegrity(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil { return failure(CodeIntegrity, err) }
	defer rows.Close()
	count := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil { return failure(CodeIntegrity, err) }
		if result != "ok" { return failure(CodeIntegrity, nil) }
		count++
	}
	if err := rows.Err(); err != nil { return failure(CodeIntegrity, err) }
	if count != 1 { return failure(CodeIntegrity, nil) }
	return nil
}

func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil { return failure(CodeForeignKey, err) }
	defer rows.Close()
	if rows.Next() { return failure(CodeForeignKey, nil) }
	if err := rows.Err(); err != nil { return failure(CodeForeignKey, err) }
	return nil
}

func quoteIdentifier(value string) string { return `"` + strings.ReplaceAll(value, `"`, `""`) + `"` }

// The query, row order, property order, and JSON escaping deliberately match
// sqliteSchemaFingerprint in sqlite-schema-adapter.ts. Internal SQLite objects
// use the identical LIKE predicate (including SQLite's '_' wildcard semantics).
func schemaFingerprint(ctx context.Context, tx *sql.Tx) (string, []string, error) {
	rows, err := tx.QueryContext(ctx, "SELECT type, name, tbl_name, sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name")
	if err != nil { return "", nil, err }
	defer rows.Close()
	encoded := []byte{'['}
	var tables []string
	first := true
	for rows.Next() {
		var kind, name, table string
		var ddl sql.NullString
		if err := rows.Scan(&kind, &name, &table, &ddl); err != nil { return "", nil, err }
		if !utf8.ValidString(kind) || !utf8.ValidString(name) || !utf8.ValidString(table) || !utf8.ValidString(ddl.String) {
			return "", nil, errors.New("invalid UTF-8 in schema")
		}
		if !first { encoded = append(encoded, ',') }
		first = false
		encoded = append(encoded, `{"type":`...)
		encoded = appendJSString(encoded, kind)
		encoded = append(encoded, `,"name":`...)
		encoded = appendJSString(encoded, name)
		encoded = append(encoded, `,"tbl_name":`...)
		encoded = appendJSString(encoded, table)
		encoded = append(encoded, `,"sql":`...)
		if ddl.Valid { encoded = appendJSString(encoded, ddl.String) } else { encoded = append(encoded, "null"...) }
		encoded = append(encoded, '}')
		if kind == "table" { tables = append(tables, name) }
	}
	if err := rows.Err(); err != nil { return "", nil, err }
	encoded = append(encoded, ']')
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), tables, nil
}

// JSON.stringify does NOT HTML-escape <>& or escape U+2028/U+2029. Go's
// encoding/json does both, even when HTML escaping is disabled for the latter.
func appendJSString(dst []byte, value string) []byte {
	const digits = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch c {
		case '"', '\\': dst = append(dst, '\\', c)
		case '\b': dst = append(dst, '\\', 'b')
		case '\f': dst = append(dst, '\\', 'f')
		case '\n': dst = append(dst, '\\', 'n')
		case '\r': dst = append(dst, '\\', 'r')
		case '\t': dst = append(dst, '\\', 't')
		default:
			if c < 0x20 { dst = append(dst, '\\', 'u', '0', '0', digits[c>>4], digits[c&15]) } else { dst = append(dst, c) }
		}
	}
	return append(dst, '"')
}

type physicalIdentity struct { database, wal, journal string }

func snapshotPath(path string) (string, error) {
	if path == "" || path == ":memory:" { return "", failure(CodeUnsafeSnapshot, nil) }
	absolute, err := filepath.Abs(path)
	if err != nil { return "", failure(CodeUnsafeSnapshot, err) }
	for dir := filepath.Dir(absolute); ; dir = filepath.Dir(dir) {
		info, err := os.Lstat(dir)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 { return "", failure(CodeUnsafeSnapshot, err) }
		if filepath.Dir(dir) == dir { break }
	}
	return absolute, nil
}

func snapshotIdentity(ctx context.Context, path string) (physicalIdentity, error) {
	var identity physicalIdentity
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		info, err := os.Lstat(path + suffix)
		missing := errors.Is(err, os.ErrNotExist)
		if missing && suffix == "" { return identity, failure(CodeMissingSnapshot, err) }
		if err != nil && !missing { return identity, failure(CodeSnapshotRead, err) }
		if !missing && (!info.Mode().IsRegular() || !singleLink(info)) { return identity, failure(CodeUnsafeSnapshot, nil) }
		if suffix == "-shm" { continue } // Reader marks are private scratch state.
		digest := sha256.New()
		if !missing {
			file, err := os.Open(path + suffix)
			if err != nil { return identity, failure(CodeSnapshotRead, err) }
			opened, statErr := file.Stat()
			if statErr != nil || !os.SameFile(info, opened) { file.Close(); return identity, failure(CodeSnapshotChanged, statErr) }
			if suffix == "" {
				var header [16]byte
				_, headerErr := file.ReadAt(header[:], 0)
				if headerErr != nil || info.Size() < 100 || !bytes.Equal(header[:], []byte("SQLite format 3\x00")) {
					file.Close(); return identity, failure(CodeIntegrity, headerErr)
				}
			}
			_, copyErr := io.Copy(digest, contextReader{ctx: ctx, reader: file})
			closeErr := file.Close()
			if copyErr != nil { return identity, failure(CodeSnapshotRead, copyErr) }
			if closeErr != nil { return identity, failure(CodeSnapshotRead, closeErr) }
			after, statErr := os.Lstat(path + suffix)
			if statErr != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) || !singleLink(after) {
				return identity, failure(CodeSnapshotChanged, statErr)
			}
		}
		value := hex.EncodeToString(digest.Sum(nil))
		switch suffix {
		case "": identity.database = value
		case "-wal": identity.wal = value
		case "-journal": identity.journal = value
		}
	}
	return identity, nil
}

type contextReader struct { ctx context.Context; reader io.Reader }
func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil { return 0, err }
	return r.reader.Read(p)
}
