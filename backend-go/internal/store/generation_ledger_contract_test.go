package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Generated only in GitHub by the actual recursive original modules. Each case
// starts from its own centrally migrated baseline copy, never a shared seed.
// Operation arguments remain raw JSON so original fingerprint/budget insertion
// order survives the native adapter; it is observable in nonce replay checks.
type generationLedgerOriginalContract struct {
	Format                                         int
	Cases                                          []generationLedgerOriginalCase
	UnsupportedBoundaries                          []generationLedgerUnsupported
	OriginalModules                                []string
	Counts                                         map[string]int
	Normalization, Authority, ExcludedNativeDomain string
}
type generationLedgerOriginalCase struct {
	Name       string
	NilDB      bool
	Operations []generationLedgerOriginalOperation
}
type generationLedgerOriginalOperation struct {
	Method, Save, Error                     string
	Args                                    json.RawMessage
	Pure                                    bool
	Now                                     int64
	Result                                  any
	WholeSql                                map[string][]map[string]any
	Unchanged, ByteUnchanged, ArgsUnchanged bool
	GeneratedIDs                            int
}
type generationLedgerUnsupported struct {
	Name, Method, OriginalError, OriginalErrorName, Classification string
	Args                                                           json.RawMessage
}

func generationLedgerDecode(t *testing.T, input []byte, output any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.UseNumber()
	if err := decoder.Decode(output); err != nil {
		t.Fatal(err)
	}
}
func generationLedgerWire(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(charactersContractValue(value))
	if err != nil {
		t.Fatal(err)
	}
	var result any
	generationLedgerDecode(t, encoded, &result)
	return result
}

var generationLedgerUUIDPattern = regexp.MustCompile(`^00000000-0000-4000-8000-[0-9]{12}$`)
var generationLedgerRefPattern = regexp.MustCompile(`"@ref:([A-Za-z0-9_.]+)"`)

// No blanket field stripping, timestamp replacement, author text rewriting or
// hash normalization. Only UUIDs emitted by the deterministic seam are labeled.
func generationLedgerNormalize(value any, sequence int) any {
	switch value := value.(type) {
	case string:
		if generationLedgerUUIDPattern.MatchString(value) {
			n, err := strconv.Atoi(value[len(value)-12:])
			if err == nil && n > 0 && n <= sequence {
				return fmt.Sprintf("generated-id:%d", n)
			}
		}
		return value
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = generationLedgerNormalize(item, sequence)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = generationLedgerNormalize(item, sequence)
		}
		return out
	default:
		return value
	}
}
func generationLedgerResolve(t *testing.T, raw json.RawMessage, saved map[string]any) json.RawMessage {
	t.Helper()
	// Replace ONLY explicit whole JSON string reference tokens. All other raw
	// bytes, including nested object insertion order, remain exactly as emitted.
	return generationLedgerRefPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
		var text string
		if err := json.Unmarshal(match, &text); err != nil {
			t.Fatal(err)
		}
		var value any = saved
		for _, part := range strings.Split(strings.TrimPrefix(text, "@ref:"), ".") {
			object, ok := value.(map[string]any)
			if !ok {
				t.Fatal("non-object operation reference", text, part)
			}
			var exists bool
			value, exists = object[part]
			if !exists {
				t.Fatal("unresolved operation reference", text, part)
			}
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return encoded
	})
}
func generationLedgerWholeSQL(t *testing.T, db *sql.DB, sequence int) any {
	t.Helper()
	jsonColumns := map[string]bool{"action_json": true, "budget_json": true, "binding_json": true, "attempt_json": true, "usage_receipt_json": true, "artifact_json": true, "idempotency_key": true, "open_key": true}
	names := charactersContractRows(t, db, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
	result := map[string][]map[string]any{}
	for _, table := range names {
		name, ok := table["name"].(string)
		if !ok {
			t.Fatal("invalid table name", table)
		}
		quoted := "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
		rows := charactersContractRows(t, db, "SELECT * FROM "+quoted+" ORDER BY rowid")
		if strings.HasPrefix(name, "generation_") {
			for _, row := range rows {
				for column, value := range row {
					if jsonColumns[column] {
						if text, ok := value.(string); ok {
							var parsed any
							generationLedgerDecode(t, []byte(text), &parsed)
							row[column] = parsed
						}
					}
				}
			}
		}
		result[name] = rows
	}
	return generationLedgerNormalize(generationLedgerWire(t, result), sequence)
}
func generationLedgerPure(t *testing.T, method string, input json.RawMessage) (any, error) {
	t.Helper()
	var args []any
	generationLedgerDecode(t, input, &args)
	before, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		after, err := json.Marshal(args)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("pure function mutated decoded caller inputs", method, err)
		}
	}()
	m := func(index int) map[string]any {
		value, ok := args[index].(map[string]any)
		if !ok {
			t.Fatal("invalid pure fixture object", method, index)
		}
		return value
	}
	switch method {
	case "rootKey":
		return GLRootKey(m(0))
	case "assertReservation":
		items, ok := args[2].([]any)
		if !ok {
			t.Fatal("invalid pure ledger array")
		}
		attempts := make([]map[string]any, len(items))
		for i, item := range items {
			var ok bool
			attempts[i], ok = item.(map[string]any)
			if !ok {
				t.Fatal("invalid pure attempt")
			}
		}
		return nil, GLAssertReservation(m(0), m(1), attempts, m(3), args[4])
	case "assertTransition":
		before, beforeOK := args[0].(string)
		after, afterOK := args[1].(string)
		if !beforeOK || !afterOK {
			t.Fatal("invalid transition fixture")
		}
		return nil, GLAssertAttemptTransition(before, after)
	case "tokenLiability":
		return GLTokenLiability(m(0))
	case "binding":
		return GLBinding(m(0))
	default:
		t.Fatal("unknown actual original pure method", method)
	}
	return nil, nil
}

func TestOriginalGenerationLedgerContracts(t *testing.T) {
	filename := os.Getenv("GO_GENERATION_LEDGER_CONTRACT")
	if filename == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("actual original generation ledger oracle mandatory in GitHub")
		}
		t.Skip("GitHub-only actual original generation ledger fixture required")
	}
	input, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var oracle generationLedgerOriginalContract
	generationLedgerDecode(t, input, &oracle)
	if oracle.Format != 1 || len(oracle.Cases) < 100 || len(oracle.UnsupportedBoundaries) == 0 || oracle.Normalization == "" || oracle.Authority == "" || oracle.ExcludedNativeDomain == "" {
		t.Fatal("incomplete original generation ledger oracle")
	}
	modules := map[string]bool{}
	for _, module := range oracle.OriginalModules {
		modules[module] = true
	}
	for _, required := range []string{"electron/repositories/generation-run-repository.ts", "src/shared/generation-contract.ts", "src/shared/source-ref.ts", "src/shared/plot-outline-contract.ts", "src/shared/visible-continuation.ts", "src/shared/draft-visible-text.ts"} {
		if !modules[required] {
			t.Fatal("actual recursive original module missing", required)
		}
	}
	seen, counts := map[string]bool{}, map[string]int{}
	for _, fixture := range oracle.Cases {
		if fixture.Name == "" || seen[fixture.Name] || len(fixture.Operations) == 0 {
			t.Fatal("invalid original trace", fixture.Name)
		}
		seen[fixture.Name] = true
		for _, operation := range fixture.Operations {
			prefix := "ledger:"
			if operation.Pure {
				prefix = "pure:"
			}
			counts[prefix+operation.Method]++
		}
		t.Run(fixture.Name, func(t *testing.T) {
			db, private, originalBytes := charactersContractCopy(t, filepath.Dir(filename), fixture.Name)
			repositoryDB := db
			if fixture.NilDB {
				repositoryDB = nil
			}
			now, sequence := int64(1000), 0
			ledger := NewGenerationLedger(repositoryDB, func() int64 { return now }, func() string { sequence++; return fmt.Sprintf("00000000-0000-4000-8000-%012d", sequence) })
			saved := map[string]any{}
			for index, operation := range fixture.Operations {
				t.Run(fmt.Sprintf("%02d-%s", index, operation.Method), func(t *testing.T) {
					now = operation.Now
					args := generationLedgerResolve(t, operation.Args, saved)
					beforeArgs := append([]byte(nil), args...)
					beforeSQL := generationLedgerWholeSQL(t, db, sequence)
					beforeFile, err := os.ReadFile(private)
					if err != nil {
						t.Fatal(err)
					}
					var result any
					var invocationErr error
					if operation.Pure {
						result, invocationErr = generationLedgerPure(t, operation.Method, args)
					} else {
						result, invocationErr = ledger.CallJSON(context.Background(), operation.Method, args)
					}
					code := ""
					if invocationErr != nil {
						code = invocationErr.Error()
					}
					if code != operation.Error {
						t.Fatalf("domain error differs: Go=%q original=%q", code, operation.Error)
					}
					if sequence != operation.GeneratedIDs {
						t.Fatalf("actual UUID seam call count differs: Go=%d original=%d", sequence, operation.GeneratedIDs)
					}
					if !operation.ArgsUnchanged || !bytes.Equal(beforeArgs, args) {
						t.Fatal("operation changed caller argument bytes")
					}
					if invocationErr == nil {
						wire := generationLedgerWire(t, result)
						if operation.Save != "" {
							saved[operation.Save] = wire
						}
						got := generationLedgerNormalize(wire, sequence)
						if !reflect.DeepEqual(got, operation.Result) {
							t.Fatalf("full return differs\nGo=%#v\noriginal=%#v", got, operation.Result)
						}
					}
					afterSQL := generationLedgerWholeSQL(t, db, sequence)
					wantSQL := generationLedgerWire(t, operation.WholeSql)
					if !reflect.DeepEqual(afterSQL, wantSQL) {
						t.Fatalf("exact whole SQL poststate differs\nGo=%#v\noriginal=%#v", afterSQL, wantSQL)
					}
					unchanged := reflect.DeepEqual(beforeSQL, afterSQL)
					if unchanged != operation.Unchanged {
						t.Fatalf("whole SQL mutation differs: Go unchanged=%v original=%v", unchanged, operation.Unchanged)
					}
					if (operation.Pure || operation.Error != "") && !unchanged {
						t.Fatal("failed/pure operation mutated SQL")
					}
					if operation.ByteUnchanged {
						afterFile, err := os.ReadFile(private)
						if err != nil || !bytes.Equal(beforeFile, afterFile) {
							t.Fatal("original byte-invariant operation mutated private database bytes", err)
						}
					}
				})
				if t.Failed() {
					break
				} // Later references require successful prior outputs.
			}
			still, err := os.ReadFile(filepath.Join(filepath.Dir(filename), fixture.Name+"-before.db"))
			if err != nil || !bytes.Equal(still, originalBytes) {
				t.Fatal("original seed fixture changed", err)
			}
		})
	}
	if !reflect.DeepEqual(counts, oracle.Counts) {
		t.Fatal("actual trace operation counts differ", counts, oracle.Counts)
	}
	for _, method := range []string{"pure:rootKey", "pure:assertReservation", "pure:assertTransition", "pure:tokenLiability", "pure:binding", "ledger:open", "ledger:get", "ledger:budget", "ledger:reserve", "ledger:findInvocation", "ledger:receipt", "ledger:markDispatched", "ledger:snapshot", "ledger:settle", "ledger:block", "ledger:pause", "ledger:recoverInterrupted", "ledger:discardCandidate"} {
		if counts[method] == 0 {
			t.Fatal("missing actual original coverage", method)
		}
	}
	// These are NOT original error parity. Actual JS throws native TypeError;
	// Go deliberately supplies a deterministic unsupported-input boundary.
	for _, boundary := range oracle.UnsupportedBoundaries {
		if seen[boundary.Name] || boundary.Method != "binding" || boundary.OriginalErrorName != "TypeError" || boundary.OriginalError == "" || boundary.Classification == "" {
			t.Fatal("invalid unsupported evidence", boundary.Name)
		}
		seen[boundary.Name] = true
		t.Run("unsupported-"+boundary.Name, func(t *testing.T) {
			_, err := generationLedgerPure(t, boundary.Method, boundary.Args)
			if err != ErrGLUnsupportedInput {
				t.Fatal("separate Go-only unsupported boundary differs", err)
			}
		})
	}
}
