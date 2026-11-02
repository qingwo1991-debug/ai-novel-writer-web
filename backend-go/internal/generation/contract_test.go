package generation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This suite consumes only the GitHub-produced actual, recursively loaded TS
// service oracle. It never creates schema or manufactures expected receipts.
// Per-case private copies preserve the centrally migrated baseline byte-for-byte.
type originalServiceContract struct {
	Format int
	UsageCases []struct {
		Name string
		Usage, Policy, Expected map[string]any
	}
	Cases []originalServiceCase
	UnsupportedBoundaries []struct {
		Name, Method, OriginalErrorName, OriginalError, Classification string
	}
	OriginalModules []string
	Normalization, Authority, ExcludedNativeDomain string
}

type originalServiceCase struct {
	Name string
	Request, Execute json.RawMessage
	Events []struct {
		Type, Kind, Text, EventID, Action string
		Value map[string]any
	}
	ProviderResult struct {
		Usage map[string]any
		FinishReason string
	}
	ProviderError string
	ThrowSnapshot, ThrowReasoning, Checkpoints, Replay bool
	ReplayHash string
	Expected struct {
		Receipt, ReplayReceipt, ReadAttempt, Run any
		Error, ReplayError string
		Snapshots, Reasoning, Checkpoints, Controls []any
		Dispatches, GeneratedIDs int
		ProviderAborted, ArgsUnchanged bool
		UnsavedTails []any
		WholeSql any
	}
}

func serviceContractDecode(t *testing.T, encoded []byte, target any) {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil { t.Fatal(err) }
}

func serviceContractWire(t *testing.T, value any) any {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil { t.Fatal(err) }
	var result any
	serviceContractDecode(t, encoded, &result)
	return result
}

var serviceContractUUID = regexp.MustCompile(`^00000000-0000-4000-8000-[0-9]{12}$`)

func serviceContractNormalize(value any, sequence int) any {
	switch value := value.(type) {
	case string:
		if serviceContractUUID.MatchString(value) {
			n, err := strconv.Atoi(value[len(value)-12:])
			if err == nil && n > 0 && n <= sequence { return fmt.Sprintf("generated-id:%d", n) }
		}
		return value
	case []any:
		out := make([]any, len(value))
		for i, item := range value { out[i] = serviceContractNormalize(item, sequence) }
		return out
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value { out[key] = serviceContractNormalize(item, sequence) }
		return out
	default:
		return value
	}
}

func serviceContractRows(t *testing.T, db *sql.DB, query string) []map[string]any {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query)
	if err != nil { t.Fatal(err) }
	columns, err := rows.Columns()
	if err != nil { rows.Close(); t.Fatal(err) }
	result := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values { pointers[i] = &values[i] }
		if err := rows.Scan(pointers...); err != nil { rows.Close(); t.Fatal(err) }
		row := map[string]any{}
		for i, column := range columns {
			if blob, ok := values[i].([]byte); ok {
				// Match better-sqlite3 Buffer JSON without erasing storage class.
				data := make([]int, len(blob))
				for j, b := range blob { data[j] = int(b) }
				row[column] = map[string]any{"type": "Buffer", "data": data}
			} else { row[column] = values[i] }
		}
		result = append(result, row)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil { t.Fatal(err, closeErr) }
	return result
}

func serviceContractSQL(t *testing.T, db *sql.DB, sequence int) any {
	t.Helper()
	jsonColumns := map[string]bool{"action_json": true, "budget_json": true, "binding_json": true, "attempt_json": true, "usage_receipt_json": true, "artifact_json": true, "idempotency_key": true, "open_key": true}
	result := map[string][]map[string]any{}
	for _, table := range serviceContractRows(t, db, "SELECT name FROM sqlite_master WHERE type='table' ORDER BY name") {
		name, ok := table["name"].(string)
		if !ok { t.Fatal("invalid actual table name", table) }
		quoted := "\"" + strings.ReplaceAll(name, "\"", "\"\"") + "\""
		rows := serviceContractRows(t, db, "SELECT * FROM " + quoted + " ORDER BY rowid")
		if strings.HasPrefix(name, "generation_") {
			for _, row := range rows {
				for column, value := range row {
					if text, ok := value.(string); ok && jsonColumns[column] {
						var parsed any
						serviceContractDecode(t, []byte(text), &parsed)
						row[column] = parsed
					}
				}
			}
		}
		result[name] = rows
	}
	return serviceContractNormalize(serviceContractWire(t, result), sequence)
}

func serviceContractCompare(t *testing.T, name string, got, want any, sequence int) {
	t.Helper()
	got = serviceContractNormalize(serviceContractWire(t, got), sequence)
	want = serviceContractWire(t, want)
	if !reflect.DeepEqual(got, want) { t.Fatalf("%s differs\nGo=%#v\nactual original=%#v", name, got, want) }
}

func serviceContractError(err error) string {
	if err != nil { return err.Error() }
	return ""
}

func TestOriginalGenerationServiceContracts(t *testing.T) {
	filename := os.Getenv("GO_GENERATION_SERVICE_CONTRACT")
	if filename == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" { t.Fatal("actual original generation service oracle mandatory in GitHub") }
		t.Skip("GitHub-only actual original service fixture required")
	}
	input, err := os.ReadFile(filename)
	if err != nil { t.Fatal(err) }
	var oracle originalServiceContract
	serviceContractDecode(t, input, &oracle)
	if oracle.Format != 1 || len(oracle.UsageCases) < 100 || len(oracle.Cases) < 40 || oracle.Normalization == "" || oracle.Authority == "" || oracle.ExcludedNativeDomain == "" { t.Fatal("incomplete actual original generation service fixture") }
	modules := map[string]bool{}
	for _, module := range oracle.OriginalModules { modules[module] = true }
	for _, required := range []string{"electron/services/generation-run-service.ts", "electron/repositories/generation-run-repository.ts", "src/shared/generation-contract.ts", "src/shared/source-ref.ts", "src/shared/agent-response-protocol.ts"} {
		if !modules[required] { t.Fatal("missing actual recursive original module", required) }
	}
	seen := map[string]bool{}
	trusted, conservative := 0, 0
	for _, fixture := range oracle.UsageCases {
		if fixture.Name == "" || seen[fixture.Name] { t.Fatal("duplicate actual usage case", fixture.Name) }
		seen[fixture.Name] = true
		if fixture.Expected["trusted"] == true { trusted++ } else { conservative++ }
		t.Run("usage/"+fixture.Name, func(t *testing.T) {
			before := serviceContractWire(t, []any{fixture.Usage, fixture.Policy})
			got := SettleProviderUsage(fixture.Usage, fixture.Policy)
			serviceContractCompare(t, "whole usage receipt", got, fixture.Expected, 0)
			serviceContractCompare(t, "usage caller inputs", []any{fixture.Usage, fixture.Policy}, before, 0)
		})
	}
	if trusted < 20 || conservative < 20 { t.Fatal("usage arithmetic matrix must cover both trust outcomes", trusted, conservative) }
	for _, boundary := range oracle.UnsupportedBoundaries {
		if boundary.Name == "" || boundary.Method != "settleProviderUsage" || boundary.OriginalErrorName != "TypeError" || boundary.OriginalError == "" || boundary.Classification == "" { t.Fatal("invalid separately unsupported native exception evidence", boundary) }
		// Deliberately no fake Go error-parity assertion for native JS TypeError.
	}
	if len(oracle.UnsupportedBoundaries) == 0 { t.Fatal("missing unsupported native-domain evidence") }
	for _, fixture := range oracle.Cases {
		if fixture.Name == "" || filepath.Base(fixture.Name) != fixture.Name || seen[fixture.Name] { t.Fatal("invalid actual service scenario name", fixture.Name) }
		seen[fixture.Name] = true
		t.Run("service/"+fixture.Name, func(t *testing.T) {
			baseline := filepath.Join(filepath.Dir(filename), fixture.Name+"-before.db")
			seed, err := os.ReadFile(baseline)
			if err != nil { t.Fatal(err) }
			private := filepath.Join(t.TempDir(), "private-generation-service.db")
			if err := os.WriteFile(private, seed, 0600); err != nil { t.Fatal(err) }
			dsn, err := store.ExistingDSN(private)
			if err != nil { t.Fatal(err) }
			db, err := sql.Open("sqlite3", dsn)
			if err != nil { t.Fatal(err) }
			db.SetMaxOpenConns(1)
			defer db.Close()
			sequence := 0
			ledger := store.NewGenerationLedger(db, func() int64 { return 1000 }, func() string { sequence++; return fmt.Sprintf("00000000-0000-4000-8000-%012d", sequence) })
			// A fixture-only watchdog bounds deadlocked or uncooperative controls.
			// Normal events remain immediate and production timers remain native.
			ctx, cancelFixture := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelFixture()
			var openArgs json.RawMessage = append(append([]byte{'['}, fixture.Request...), ']')
			opened, err := ledger.CallJSON(ctx, "open", openArgs)
			if err != nil { t.Fatal("actual open request", err) }
			run := opened.(map[string]any)
			runID, rootID := run["runId"].(string), run["rootActionId"].(string)
			var request Request
			serviceContractDecode(t, fixture.Execute, &request)
			request.RunID = runID
			// Caller immutability compares native request bytes, not fixture UUID
			// labels: the before and after values both contain the real seam IDs.
			beforeRequest, err := json.Marshal(request)
			if err != nil { t.Fatal(err) }
			snapshots, reasoning, checkpoints, controls := []any{}, []any{}, []any{}, []any{}
			dispatches, providerAborted := 0, false
			dispatchStarted, dispatchDone := make(chan struct{}, 1), make(chan struct{})
			var service *Service
			service = New(Dependencies{
				Ledger: ledger,
				Now: func() time.Time { return time.UnixMilli(1000) },
				OnSnapshot: func(value Snapshot) {
					snapshots = append(snapshots, serviceContractWire(t, value))
					if fixture.ThrowSnapshot { panic("synthetic UI-only failure") }
				},
				OnReasoning: func(value ReasoningEvent) {
					reasoning = append(reasoning, serviceContractWire(t, value))
					if fixture.ThrowReasoning { panic("synthetic UI-only failure") }
				},
				Dispatch: func(providerCtx context.Context, providerRequest any, callbacks Callbacks) (DispatchResult, error) {
					dispatchStarted <- struct{}{}
					defer close(dispatchDone)
					dispatches++
					serviceContractCompare(t, "provider request", providerRequest, request.ProviderRequest, sequence)
					for index, event := range fixture.Events {
						switch event.Type {
						case "visible": callbacks.OnVisible(VisibleEvent{Kind: event.Kind, Text: event.Text, EventID: event.EventID})
						case "reasoning": callbacks.OnReasoning(event.Text)
						case "diagnostics": callbacks.OnDiagnostics(event.Value)
						case "control":
							var controlErr error
							if event.Action == "suspend" { _, controlErr = service.SuspendForProjectClose(ctx) } else if event.Action == "pause" { _, controlErr = service.Pause(ctx, rootID) } else if event.Action == "cancel" { _, controlErr = service.Cancel(ctx, rootID) } else { t.Fatal("unknown fixture control", event.Action) }
							controls = append(controls, map[string]any{"action": event.Action, "error": serviceContractError(controlErr)})
						default: t.Fatal("unknown actual provider event", event.Type)
						}
						if fixture.Checkpoints { checkpoints = append(checkpoints, map[string]any{"index": index, "wholeSql": serviceContractSQL(t, db, sequence), "generatedIDs": sequence}) }
					}
					providerAborted = providerCtx.Err() != nil
					if fixture.ProviderError != "" { return DispatchResult{}, errors.New(fixture.ProviderError) }
					return DispatchResult{Usage: fixture.ProviderResult.Usage, FinishReason: fixture.ProviderResult.FinishReason}, nil
				},
			})
			receipt, executeErr := service.Execute(ctx, request)
			// Cancellation intentionally need not await a late provider. Synchronize
			// only the fixture callback script before inspecting its observations;
			// never sleep, replace a timer, or change the service's business logic.
			select {
			case <-dispatchStarted:
				select {
				case <-dispatchDone:
				case <-ctx.Done(): t.Fatal("controlled provider callbacks did not finish", ctx.Err())
				}
			default:
			}
			if got := serviceContractError(executeErr); got != fixture.Expected.Error { t.Fatalf("execute error differs: Go=%q actual=%q", got, fixture.Expected.Error) }
			serviceContractCompare(t, "full receipt", receipt, fixture.Expected.Receipt, sequence)
			var replayReceipt any
			var replayErr error
			if fixture.Replay || fixture.ReplayHash != "" {
				replayRequest := request
				if fixture.ReplayHash != "" { replayRequest.RequestHash = fixture.ReplayHash }
				replayReceipt, replayErr = service.Execute(ctx, replayRequest)
			}
			if got := serviceContractError(replayErr); got != fixture.Expected.ReplayError { t.Fatalf("replay error differs: Go=%q actual=%q", got, fixture.Expected.ReplayError) }
			serviceContractCompare(t, "replay receipt", replayReceipt, fixture.Expected.ReplayReceipt, sequence)
			var readAttempt any
			attempts := serviceContractRows(t, db, "SELECT attempt_id FROM generation_attempts ORDER BY rowid")
			if len(attempts) > 0 {
				readAttempt, err = ledger.Call(ctx, "receipt", []any{attempts[0]["attempt_id"]})
				if err != nil { t.Fatal(err) }
			}
			currentRun, err := ledger.Call(ctx, "get", []any{runID})
			if err != nil { t.Fatal(err) }
			serviceContractCompare(t, "readAttempt whole source receipt", readAttempt, fixture.Expected.ReadAttempt, sequence)
			serviceContractCompare(t, "get whole source run", currentRun, fixture.Expected.Run, sequence)
			serviceContractCompare(t, "all snapshot notifications", snapshots, fixture.Expected.Snapshots, sequence)
			serviceContractCompare(t, "display-only reasoning notifications", reasoning, fixture.Expected.Reasoning, sequence)
			serviceContractCompare(t, "per-event whole SQL checkpoints", checkpoints, fixture.Expected.Checkpoints, sequence)
			serviceContractCompare(t, "pause/cancel/suspend errors", controls, fixture.Expected.Controls, sequence)
			serviceContractCompare(t, "all raw tables exact final SQL", serviceContractSQL(t, db, sequence), fixture.Expected.WholeSql, sequence)
			if sequence != fixture.Expected.GeneratedIDs || dispatches != fixture.Expected.Dispatches || providerAborted != fixture.Expected.ProviderAborted { t.Fatal("UUID/dispatch/abort mismatch", sequence, fixture.Expected.GeneratedIDs, dispatches, fixture.Expected.Dispatches, providerAborted, fixture.Expected.ProviderAborted) }
			if len(service.ReadUnsavedTails()) != len(fixture.Expected.UnsavedTails) { t.Fatal("unsaved tails differ") }
			if !fixture.Expected.ArgsUnchanged { t.Fatal("actual source changed execute input") }
			afterRequest, err := json.Marshal(request)
			if err != nil || !bytes.Equal(beforeRequest, afterRequest) { t.Fatal("execute changed caller request bytes", err) }
			still, err := os.ReadFile(baseline)
			if err != nil || !bytes.Equal(still, seed) { t.Fatal("original baseline fixture mutated", err) }
		})
	}
	for _, required := range []string{"execute-unicode-delta", "execute-duplicate-id-text-conflict", "execute-split-open-think", "execute-byte-flush-4096", "execute-byte-flush-unicode-boundary", "execute-pause-flush-and-ignore-late", "execute-cancel-flush-and-ignore-late", "execute-suspend-flush-and-ignore-late", "execute-replay-does-not-redispatch", "execute-diagnostics-stable-network-category"} {
		if !seen[required] { t.Fatal("missing meaningful actual lifecycle scenario", required) }
	}
}
