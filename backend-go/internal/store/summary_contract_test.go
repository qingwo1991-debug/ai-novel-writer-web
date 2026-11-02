package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// The original repository is invoked only by GitHub fixture generation. Go
// opens private copies of its actual post-seeding, pre-invocation databases.
type summaryOriginalContract struct {
	Format            int
	OriginalModules   []string
	ReceiptedStatuses []string
	Cases             []summaryOriginalCase
	StrongerAtomic    []summaryAtomicCase
}

type summaryOriginalCase struct {
	Name, Method, Text, Error string
	Chapter, DraftID          any
	NilDB, Unchanged          bool
	Result                    any
	After                     []map[string]any
}

type summaryAtomicCase struct {
	Name, Text, Error string
	Chapter           any
	After             []map[string]any
}

// Preserve every returned field, including author metadata named created_at.
// Only summaryContractPoststate excludes a top-level SQL row clock. Source
// hashes, IDs, legacy text and revisions stay exact; typed nil maps are null.
func summaryContractNormalize(value any) any {
	switch value := value.(type) {
	case map[string]any:
		if value == nil {
			return nil
		}
		out := map[string]any{}
		for key, item := range value {
			out[key] = summaryContractNormalize(item)
		}
		return out
	case []map[string]any:
		if value == nil {
			return nil
		}
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = summaryContractNormalize(item)
		}
		return out
	case []any:
		if value == nil {
			return nil
		}
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = summaryContractNormalize(item)
		}
		return out
	default:
		return value
	}
}

// UseNumber retains exact request-domain integers while decoding the oracle.
// Results are compared semantically AFTER marshaling to the actual JSON wire;
// otherwise json.Number versus int64/float64 would cause artificial failures.
func summaryContractWire(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(summaryContractNormalize(value))
	if err != nil {
		t.Fatal(err)
	}
	var parsed any
	if err := json.Unmarshal(encoded, &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed
}

func summaryContractCopy(t *testing.T, directory, name string) (*sql.DB, string, []byte) {
	t.Helper()
	if name == "" || filepath.Base(name) != name {
		t.Fatal("invalid oracle case name", name)
	}
	input, err := os.ReadFile(filepath.Join(directory, name+"-before.db"))
	if err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(t.TempDir(), "private-summary.db")
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

func summaryContractPoststate(t *testing.T, db *sql.DB, expected []map[string]any) {
	t.Helper()
	rows, err := rrMaps(context.Background(), db, "SELECT * FROM summary_snapshots ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	clean := make([]map[string]any, len(rows))
	for i, row := range rows {
		clean[i] = map[string]any{}
		for key, value := range row {
			if key != "created_at" {
				clean[i][key] = value
			}
		}
	}
	got, want := summaryContractWire(t, clean), summaryContractWire(t, expected)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full raw summary_snapshots post-state differs\nGo=%#v\noriginal=%#v", got, want)
	}
}

func summaryContractUnchanged(t *testing.T, db *sql.DB, filename string, before []byte) {
	t.Helper()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("read/error changed actual copied baseline bytes")
	}
}

func TestOriginalSummaryRepositoryContract(t *testing.T) {
	filename := os.Getenv("GO_SUMMARY_CONTRACT")
	if filename == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original SummaryRepository contract mandatory in GitHub")
		}
		t.Skip("GitHub-only actual SummaryRepository fixture required")
	}
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(file)
	decoder.UseNumber()
	var oracle summaryOriginalContract
	err = decoder.Decode(&oracle)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if oracle.Format != 1 || len(oracle.Cases) < 60 || len(oracle.OriginalModules) < 3 || len(oracle.StrongerAtomic) == 0 {
		t.Fatal("incomplete actual summary oracle", oracle.Format, len(oracle.Cases), oracle.OriginalModules, len(oracle.StrongerAtomic))
	}
	if !reflect.DeepEqual(oracle.ReceiptedStatuses, []string{"finalized", "superseded"}) {
		t.Fatal("shared actual receipted statuses changed", oracle.ReceiptedStatuses)
	}
	modules := map[string]bool{}
	for _, module := range oracle.OriginalModules {
		modules[module] = true
	}
	for _, required := range []string{"electron/repositories/summary-repository.ts", "electron/repositories/character-repository.ts", "electron/repositories/character-roster-repository.ts", "src/shared/finalized-continuity.ts", "src/shared/draft-status.ts"} {
		if !modules[required] {
			t.Fatal("actual recursive business module missing", required)
		}
	}
	seen := map[string]bool{}
	methods := map[string]int{}
	for _, fixture := range oracle.Cases {
		if seen[fixture.Name] {
			t.Fatal("duplicate original case", fixture.Name)
		}
		seen[fixture.Name] = true
		methods[fixture.Method]++
		t.Run(fixture.Name, func(t *testing.T) {
			db, private, before := summaryContractCopy(t, filepath.Dir(filename), fixture.Name)
			repositoryDB := db
			if fixture.NilDB {
				repositoryDB = nil
			}
			repository := NewSummary(repositoryDB)
			ctx := context.Background()
			var result any
			var invocationErr error
			switch fixture.Method {
			case "SaveSnapshot":
				invocationErr = repository.SaveSnapshot(ctx, fixture.Chapter, fixture.Text)
			case "LatestSnapshot":
				result, invocationErr = repository.LatestSnapshot(ctx)
			case "ReadFinalizedSource":
				result, invocationErr = repository.ReadFinalizedSource(ctx, fixture.DraftID)
			case "ListBefore":
				result, invocationErr = repository.ListBefore(ctx, fixture.Chapter)
			default:
				t.Fatal("unknown original method", fixture.Method)
			}
			code := ""
			if invocationErr != nil {
				code = invocationErr.Error()
			}
			if code != fixture.Error {
				t.Fatalf("original error differs: Go=%q original=%q", code, fixture.Error)
			}
			// Error returns have no wire payload. Do not compare typed-nil map
			// artifacts with the oracle's unassigned result placeholder.
			if invocationErr == nil {
				got, want := summaryContractWire(t, result), summaryContractWire(t, fixture.Result)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("full return differs\nGo=%#v\noriginal=%#v", got, want)
				}
			}
			summaryContractPoststate(t, db, fixture.After)
			if fixture.Unchanged {
				summaryContractUnchanged(t, db, private, before)
			}
		})
	}
	for _, method := range []string{"SaveSnapshot", "LatestSnapshot", "ReadFinalizedSource", "ListBefore"} {
		if methods[method] == 0 {
			t.Fatal("missing actual original API coverage", method)
		}
	}
	// Separate, stronger Go atomic guarantee: the original nontransactional
	// saveSnapshot is deliberately NOT invoked for these RAISE(FAIL) fixtures.
	for _, fixture := range oracle.StrongerAtomic {
		t.Run(fixture.Name, func(t *testing.T) {
			db, private, before := summaryContractCopy(t, filepath.Dir(filename), fixture.Name)
			err := NewSummary(db).SaveSnapshot(context.Background(), fixture.Chapter, fixture.Text)
			if err == nil || err.Error() != fixture.Error {
				t.Fatal("stronger Go-only atomic trigger error differs", err, fixture.Error)
			}
			summaryContractPoststate(t, db, fixture.After)
			summaryContractUnchanged(t, db, private, before)
		})
	}
}
