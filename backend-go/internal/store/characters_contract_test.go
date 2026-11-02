package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// GitHub generates these fixtures by invoking actual recursively loaded original
// repositories/services against centrally migrated M02 baseline copies. Go reads
// private copies captured after all seeding and BEFORE the original invocation.
type charactersOriginalContract struct {
	Format                int
	OriginalModules       []string
	Counts                map[string]int
	Cases                 []charactersOriginalCase
	UnsupportedBoundaries []charactersUnsupportedBoundary
	Normalization         string
	Authority             string
	ExcludedNativeDomain  string
}

type charactersOriginalCase struct {
	Name, Method, ProjectID, Error string
	NilDB, Unchanged               bool
	Result                         any
	After                          map[string][]map[string]any
}

type charactersUnsupportedBoundary struct {
	Name, Method, ExpectedGoError, OriginalError, OriginalErrorName string
	After                                                           map[string][]map[string]any
}

// No timestamp omission, rewriting, blanket recursive field stripping or JSON
// string reparsing is permitted. SQL clocks are stable stored fixture strings;
// author metadata with the same names remains intact. Only SQLite BLOB gets the
// original Node Buffer JSON representation rather than Go's base64 []byte wire.
func charactersContractValue(value any) any {
	switch value := value.(type) {
	case []byte:
		data := make([]int, len(value))
		for i, b := range value {
			data[i] = int(b)
		}
		return map[string]any{"type": "Buffer", "data": data}
	case map[string]any:
		if value == nil {
			return nil
		}
		out := map[string]any{}
		for key, item := range value {
			out[key] = charactersContractValue(item)
		}
		return out
	case map[string][]map[string]any:
		if value == nil {
			return nil
		}
		out := map[string]any{}
		for key, rows := range value {
			out[key] = charactersContractValue(rows)
		}
		return out
	case []map[string]any:
		if value == nil {
			return nil
		}
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = charactersContractValue(item)
		}
		return out
	case []any:
		if value == nil {
			return nil
		}
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = charactersContractValue(item)
		}
		return out
	default:
		return value
	}
}

// Decode oracle inputs with UseNumber; compare semantically only after marshaling
// the actual return wire. This avoids an artificial int64/json.Number mismatch
// while still comparing every raw source/JSON string and hash byte-for-byte.
func charactersContractWire(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(charactersContractValue(value))
	if err != nil {
		t.Fatal(err)
	}
	var parsed any
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func charactersContractCopy(t *testing.T, directory, name string) (*sql.DB, string, []byte) {
	t.Helper()
	if name == "" || filepath.Base(name) != name {
		t.Fatal("invalid original case name", name)
	}
	input, err := os.ReadFile(filepath.Join(directory, name+"-before.db"))
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "private-characters.db")
	if err := os.WriteFile(filename, input, 0600); err != nil {
		t.Fatal(err)
	}
	dsn, err := ExistingDSN(filename)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db, filename, input
}

// Scan raw driver values: converting []byte to string would silently hide the
// original SQLite storage class and lose Buffer wire parity.
func charactersContractRows(t *testing.T, db *sql.DB, query string) []map[string]any {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	columns, err := rows.Columns()
	if err != nil {
		rows.Close()
		t.Fatal(err)
	}
	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		record := map[string]any{}
		for i, key := range columns {
			if blob, ok := values[i].([]byte); ok {
				copyOfBlob := make([]byte, len(blob))
				copy(copyOfBlob, blob)
				record[key] = copyOfBlob
			} else {
				record[key] = values[i]
			}
		}
		result = append(result, record)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	return result
}

func charactersContractPoststate(t *testing.T, db *sql.DB, expected map[string][]map[string]any) {
	t.Helper()
	// Compare ALL actual tables, not just an assumed subset. Byte invariance below
	// additionally protects indexes, schema, SQL clocks and hidden SQLite state.
	names := charactersContractRows(t, db, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	if len(names) != len(expected) {
		t.Fatal("raw table inventory differs", names, len(expected))
	}
	got := map[string][]map[string]any{}
	for _, row := range names {
		name, ok := row["name"].(string)
		if !ok {
			t.Fatal("unexpected SQLite table name", row)
		}
		if _, ok := expected[name]; !ok {
			t.Fatal("extra raw table", name)
		}
		quoted := "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
		got[name] = charactersContractRows(t, db, "SELECT * FROM "+quoted+" ORDER BY rowid")
	}
	actualWire, expectedWire := charactersContractWire(t, got), charactersContractWire(t, expected)
	if !reflect.DeepEqual(actualWire, expectedWire) {
		t.Fatalf("full exact raw post-state differs\nGo=%#v\noriginal=%#v", actualWire, expectedWire)
	}
}

func charactersContractUnchanged(t *testing.T, db *sql.DB, filename string, before []byte) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("character read changed actual copied pre-invocation database bytes")
	}
}

func TestOriginalCharactersReadContracts(t *testing.T) {
	filename := os.Getenv("GO_CHARACTERS_CONTRACT")
	if filename == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("actual original character read oracle mandatory in GitHub")
		}
		t.Skip("GitHub-only actual original character fixture required")
	}
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	var oracle charactersOriginalContract
	err = decoder.Decode(&oracle)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if oracle.Format != 1 || len(oracle.Cases) < 60 || len(oracle.UnsupportedBoundaries) == 0 || oracle.Normalization == "" || oracle.Authority == "" || oracle.ExcludedNativeDomain == "" {
		t.Fatal("incomplete actual original characters oracle", oracle.Format, len(oracle.Cases))
	}
	modules := map[string]bool{}
	for _, module := range oracle.OriginalModules {
		modules[module] = true
	}
	for _, required := range []string{
		"electron/repositories/character-repository.ts",
		"electron/repositories/character-roster-repository.ts",
		"electron/repositories/character-roster-schema.ts",
		"electron/repositories/project-core-repository.ts",
		"electron/services/current-character-projection.ts",
		"electron/services/character-proposal-service.ts",
		"src/shared/character-role.ts",
		"src/shared/character-roster.ts",
		"src/shared/source-ref.ts",
	} {
		if !modules[required] {
			t.Fatal("actual recursive module missing", required)
		}
	}
	seen, methods := map[string]bool{}, map[string]int{}
	for _, fixture := range oracle.Cases {
		if seen[fixture.Name] {
			t.Fatal("duplicate original case", fixture.Name)
		}
		seen[fixture.Name] = true
		methods[fixture.Method]++
		if !fixture.Unchanged {
			t.Fatal("original character read mutated fixture", fixture.Name)
		}
		t.Run(fixture.Name, func(t *testing.T) {
			db, private, before := charactersContractCopy(t, filepath.Dir(filename), fixture.Name)
			repositoryDB := db
			if fixture.NilDB {
				repositoryDB = nil
			}
			repository := NewCharacters(repositoryDB)
			ctx := context.Background()
			var result any
			var invocationErr error
			switch fixture.Method {
			case "All":
				result, invocationErr = repository.All(ctx)
			case "ReadRoster":
				result, invocationErr = repository.ReadRoster(ctx)
			case "IdentitySnapshot":
				result, invocationErr = repository.IdentitySnapshot(ctx)
			case "IdentityView":
				result, invocationErr = repository.IdentityView(ctx)
			case "CurrentDerivedFields":
				result, invocationErr = repository.CurrentDerivedFields(ctx, fixture.ProjectID)
			default:
				t.Fatal("unknown actual original method", fixture.Method)
			}
			code := ""
			if invocationErr != nil {
				code = invocationErr.Error()
			}
			if code != fixture.Error {
				t.Fatalf("original domain error differs: Go=%q original=%q", code, fixture.Error)
			}
			if invocationErr == nil {
				got, want := charactersContractWire(t, result), charactersContractWire(t, fixture.Result)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("full character return differs\nGo=%#v\noriginal=%#v", got, want)
				}
			}
			charactersContractPoststate(t, db, fixture.After)
			charactersContractUnchanged(t, db, private, before)
			// Even an accidentally reused input must never change the oracle file.
			still, err := os.ReadFile(filepath.Join(filepath.Dir(filename), fixture.Name+"-before.db"))
			if err != nil || !bytes.Equal(still, before) {
				t.Fatal("original input fixture changed", err)
			}
		})
	}
	if !reflect.DeepEqual(methods, oracle.Counts) {
		t.Fatal("oracle runtime counts do not match actual cases", methods, oracle.Counts)
	}
	for _, method := range []string{"All", "ReadRoster", "IdentitySnapshot", "IdentityView", "CurrentDerivedFields"} {
		if methods[method] == 0 {
			t.Fatal("missing actual original read coverage", method)
		}
	}
	if methods["IdentityView"] < 10 {
		t.Fatal("public original identity DTO coverage incomplete", methods["IdentityView"])
	}

	// Deliberately NOT original error parity: native missing-row TypeErrors are
	// outside the API domain. Go supplies an explicit deterministic boundary.
	for _, fixture := range oracle.UnsupportedBoundaries {
		if seen[fixture.Name] || fixture.OriginalErrorName != "TypeError" || fixture.OriginalError == "" || fixture.ExpectedGoError != "CHARACTER_ID_META_MISSING" {
			t.Fatal("invalid separately labeled unsupported boundary", fixture)
		}
		seen[fixture.Name] = true
		t.Run(fixture.Name, func(t *testing.T) {
			db, private, before := charactersContractCopy(t, filepath.Dir(filename), fixture.Name)
			repository := NewCharacters(db)
			var invocationErr error
			switch fixture.Method {
			case "IdentitySnapshot":
				_, invocationErr = repository.IdentitySnapshot(context.Background())
			case "IdentityView":
				_, invocationErr = repository.IdentityView(context.Background())
			default:
				t.Fatal("unknown Go-only unsupported boundary", fixture.Method)
			}
			if invocationErr == nil || invocationErr.Error() != fixture.ExpectedGoError {
				t.Fatal("Go-only explicit unsupported boundary differs", invocationErr, fixture.ExpectedGoError)
			}
			charactersContractPoststate(t, db, fixture.After)
			charactersContractUnchanged(t, db, private, before)
		})
	}
}
