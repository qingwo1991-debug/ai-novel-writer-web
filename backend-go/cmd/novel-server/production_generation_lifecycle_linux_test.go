//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// errProductionLifecycleDialed fails a lane whose settle-or-advance operation
// reached the executor's dispatch seam. Cancel, pause and resume move ledger rows;
// none of them may stand in for a provider call, so the seam refuses and counts.
var errProductionLifecycleDialed = errors.New("PRODUCTION_LIFECYCLE_DIALED")

// productionLifecycleNoDial is the executor's injectable dispatch seam for the
// lifecycle journey. It replaces the native dispatch - authority checks, credential
// resolution and the egress HTTP call - and counts its calls, so "these names never
// dial" is asserted from the provider side instead of only from the ledger side.
type productionLifecycleNoDial struct {
	mu    sync.Mutex
	calls int
}

func (s *productionLifecycleNoDial) dispatch(context.Context, any, generation.Callbacks) (generation.DispatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return generation.DispatchResult{}, errProductionLifecycleDialed
}

func (s *productionLifecycleNoDial) dialed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// productionLifecycleLedger opens the admitted author database read-only and wraps
// it in the shipped ledger store, so every assertion below reads the rows the
// ledger wrote rather than the view the operation happened to return. It is the
// same reader-only connection the persisted draft body assertion uses; "get" and
// "budget" are SELECT-only methods, so nothing here can write the author's data.
func productionLifecycleLedger(t *testing.T, projectPath string) *store.GenerationLedger {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store.NewGenerationLedger(db, nil, nil)
}

// productionLifecycleRunStatus reads one run row back through the ledger store.
func productionLifecycleRunStatus(t *testing.T, ledger *store.GenerationLedger, runID string) string {
	t.Helper()
	value, err := ledger.Call(context.Background(), "get", []any{runID})
	if err != nil {
		t.Fatalf("ledger get %q: %v", runID, err)
	}
	return syntheticString(syntheticMap(value)["status"])
}

// productionLifecycleRootStatus reads the root row's status back through the same
// store, so a transition recorded only on the run row cannot pass as a stop.
func productionLifecycleRootStatus(t *testing.T, ledger *store.GenerationLedger, rootID string) string {
	t.Helper()
	value, err := ledger.Call(context.Background(), "budget", []any{rootID})
	if err != nil {
		t.Fatalf("ledger budget %q: %v", rootID, err)
	}
	return syntheticString(syntheticMap(syntheticMap(value)["root"])["status"])
}

// productionLifecycleAnswer posts one lifecycle operation and returns its business
// result. A 501 is named explicitly: the lane would fail silently there, because an
// unbound binder installs nothing and startup never complains.
func productionLifecycleAnswer(t *testing.T, author *productionDurableClient, operation string, handle, frozen map[string]any) map[string]any {
	t.Helper()
	response := author.call(t, operation, []any{handle, frozen})
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s is still the lane's fail-closed 501 (the lifecycle binder is not installed): %d %s",
			operation, response.Code, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		t.Fatalf("%s envelope: %v %s", operation, err, response.Body.String())
	}
	if wire.Result["success"] == false {
		t.Fatalf("%s refused instead of transitioning the run: %s", operation, response.Body.String())
	}
	return wire.Result
}

// productionLifecycleRefusal asserts one refused lifecycle operation without
// pinning the lease error: a stale, foreign or absent lease is refused before any
// ledger row, and the family answers that as its business DTO (HTTP 200, success
// false) rather than as an HTTP error. It returns the symbolic code so a caller can
// pin the codes this lane owns.
func productionLifecycleRefusal(t *testing.T, label string, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s: the lifecycle binder is not installed: %d %s", label, response.Code, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d instead of the business envelope: %s", label, response.Code, response.Body.String())
	}
	var body struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	if body.Result.Success || !strings.HasPrefix(body.Result.Error, "Error: ") {
		t.Fatalf("%s was not refused through the lane envelope: %s", label, response.Body.String())
	}
	return strings.TrimPrefix(body.Result.Error, "Error: ")
}

// TestProductionLaneSettlesAndAdvancesAdmittedRunsWithoutDialing is the lifecycle
// half of this lane: generation:cancel, generation:pause and generation:resume must
// answer 200 over a real admitted run instead of the fail-closed 501, move exactly
// the ledger rows their name promises (root and run), leave generation:read
// reporting the new status, refuse a stale lease before any row moves, treat a
// settled run as documented instead of succeeding silently, and never reach the
// dispatch seam. generation:restart is not asserted here: it is served by the
// generation write batch, which proves it runs the begin body and then stops the
// old root without dialing.
func TestProductionLaneSettlesAndAdvancesAdmittedRunsWithoutDialing(t *testing.T) {
	tests := []struct {
		operation    string
		token        string
		prerequisite string
		startRun     string
		startRoot    string
		wantStatus   string
		wantRun      string
		wantRoot     string
		// repeatCode is the code a second identical call must refuse with. Empty
		// means the repeat is a documented idempotent re-stop that answers the same
		// status.
		repeatCode string
	}{
		{
			operation: "generation:cancel", token: "cancel",
			startRun: "running", startRoot: "active",
			wantStatus: "cancelled", wantRun: "cancelled", wantRoot: "cancelled",
			// A cancel of an already cancelled run is refused with this lane's own
			// code: the shipped ledger answers the same stop with a silent nil, and
			// a 200 for a row set nobody wrote is exactly what must not happen.
			repeatCode: "GENERATION_RUN_ALREADY_SETTLED",
		},
		{
			operation: "generation:pause", token: "pause",
			startRun: "running", startRoot: "active",
			wantStatus: "paused", wantRun: "paused", wantRoot: "paused",
			// Pausing a paused run is a repeated stop, not a settled one: the ledger
			// rewrites the same paused rows and cancels any still-reserved attempt, so
			// it answers the paused view again. Pausing a cancelled run is refused
			// with the same settled code cancel uses (shared stop path).
			repeatCode: "",
		},
		{
			operation: "generation:resume", token: "resume",
			prerequisite: "generation:pause",
			startRun:     "paused", startRoot: "paused",
			wantStatus: "running", wantRun: "running", wantRoot: "active",
			// Resume is default-deny and only advances a parked run; a run that is
			// already running is refused by the reused path's own code.
			repeatCode: "GENERATION_RESUME_UNAVAILABLE",
		},
	}
	for _, tc := range tests {
		t.Run(tc.operation, func(t *testing.T) {
			root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
			productionCommitGenerationPurpose(t, root, productionTestModelID)
			seam := &productionLifecycleNoDial{}
			handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
				productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes, Dispatch: seam.dispatch})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()

			author, frozen := productionExecuteSession(t, handler, projectPath, "generation-lifecycle-open-"+tc.token)
			handle := productionExecuteBegin(t, author, frozen, "generation-lifecycle-begin-"+tc.token)
			runID, _ := handle["runId"].(string)
			rootID, _ := handle["rootActionId"].(string)
			if runID == "" || rootID == "" {
				t.Fatalf("the admitted run carries no identity: %v", handle)
			}

			// The rows generation:begin wrote, read from the ledger rather than
			// assumed: the admission opens one running run under an active root.
			ledger := productionLifecycleLedger(t, projectPath)
			if got := productionLifecycleRunStatus(t, ledger, runID); got != "running" {
				t.Fatalf("the admitted run starts at %q; want running", got)
			}
			if got := productionLifecycleRootStatus(t, ledger, rootID); got != "active" {
				t.Fatalf("the admitted root starts at %q; want active", got)
			}
			if tc.prerequisite != "" {
				parked := productionLifecycleAnswer(t, author, tc.prerequisite, handle, frozen)
				if parked["status"] != "paused" {
					t.Fatalf("%s left the run at %v; want paused", tc.prerequisite, parked["status"])
				}
			}
			// The state the operation under test starts from.
			if got := productionLifecycleRunStatus(t, ledger, runID); got != tc.startRun {
				t.Fatalf("%s starts from a run row of %q; want %q", tc.operation, got, tc.startRun)
			}
			if got := productionLifecycleRootStatus(t, ledger, rootID); got != tc.startRoot {
				t.Fatalf("%s starts from a root row of %q; want %q", tc.operation, got, tc.startRoot)
			}

			// The transition: the business DTO, the ledger rows it wrote, and the
			// same status replayed through generation:read.
			result := productionLifecycleAnswer(t, author, tc.operation, handle, frozen)
			if result["status"] != tc.wantStatus {
				t.Fatalf("%s answered %v; want %s", tc.operation, result["status"], tc.wantStatus)
			}
			answerHandle, ok := result["handle"].(map[string]any)
			if !ok || answerHandle["runId"] != handle["runId"] || answerHandle["rootActionId"] != handle["rootActionId"] {
				t.Fatalf("%s answered for another run: %v", tc.operation, result["handle"])
			}
			if got := productionLifecycleRunStatus(t, ledger, runID); got != tc.wantRun {
				t.Fatalf("%s left the ledger run row at %q; want %q", tc.operation, got, tc.wantRun)
			}
			if got := productionLifecycleRootStatus(t, ledger, rootID); got != tc.wantRoot {
				t.Fatalf("%s left the ledger root row at %q; want %q", tc.operation, got, tc.wantRoot)
			}
			var view map[string]any
			productionEditorDecode(t, author, "generation:read", []any{handle, frozen}, &view)
			if view["status"] != tc.wantStatus {
				t.Fatalf("generation:read reported %v after %s; want %s", view["status"], tc.operation, tc.wantStatus)
			}

			// A lease this server never issued is refused before any ledger write: the
			// rows keep the status the transition above left them at.
			stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}
			productionLifecycleRefusal(t, "stale-lease "+tc.operation, author.call(t, tc.operation, []any{handle, stale}))
			if got := productionLifecycleRunStatus(t, ledger, runID); got != tc.wantRun {
				t.Fatalf("a stale-lease %s moved the run row to %q; want %q", tc.operation, got, tc.wantRun)
			}
			if got := productionLifecycleRootStatus(t, ledger, rootID); got != tc.wantRoot {
				t.Fatalf("a stale-lease %s moved the root row to %q; want %q", tc.operation, got, tc.wantRoot)
			}

			// An already-settled run - and an already-advanced one - behaves as
			// documented instead of reporting a success nobody recorded.
			if tc.repeatCode == "" {
				again := productionLifecycleAnswer(t, author, tc.operation, handle, frozen)
				if again["status"] != tc.wantStatus {
					t.Fatalf("a repeated %s left the run at %v; want %s", tc.operation, again["status"], tc.wantStatus)
				}
			} else {
				repeat := productionLifecycleRefusal(t, "repeated "+tc.operation, author.call(t, tc.operation, []any{handle, frozen}))
				if repeat != tc.repeatCode {
					t.Fatalf("a repeated %s refused with %q; want %q", tc.operation, repeat, tc.repeatCode)
				}
			}
			if got := productionLifecycleRunStatus(t, ledger, runID); got != tc.wantRun {
				t.Fatalf("a repeated %s moved the run row to %q; want %q", tc.operation, got, tc.wantRun)
			}

			// Nothing dialed: the one executor never saw a prepared dispatch.
			if seam.dialed() != 0 {
				t.Fatalf("%s dispatched %d times; the lifecycle names settle or advance state only", tc.operation, seam.dialed())
			}
		})
	}
}

// TestProductionLaneRefusesCancelOfASettledRunWithItsOwnCode pins the settled
// refusal on the one case the ledger would otherwise answer with a silent nil: a
// root the lane already cancelled. It reads the run back through the ledger store
// and through generation:read afterwards, so neither a second cancel nor a pause of
// the same settled run may claim a transition.
func TestProductionLaneRefusesCancelOfASettledRunWithItsOwnCode(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	productionCommitGenerationPurpose(t, root, productionTestModelID)
	seam := &productionLifecycleNoDial{}
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes, Dispatch: seam.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author, frozen := productionExecuteSession(t, handler, projectPath, "generation-settled-open-1")
	handle := productionExecuteBegin(t, author, frozen, "generation-settled-begin-1")
	runID, _ := handle["runId"].(string)
	rootID, _ := handle["rootActionId"].(string)
	ledger := productionLifecycleLedger(t, projectPath)

	settled := productionLifecycleAnswer(t, author, "generation:cancel", handle, frozen)
	if settled["status"] != "cancelled" {
		t.Fatalf("generation:cancel answered %v; want cancelled", settled["status"])
	}
	// Both names stop through the same guard, so both must refuse the settled run
	// with the same code, and neither may move a row.
	for _, operation := range []string{"generation:cancel", "generation:pause"} {
		if code := productionLifecycleRefusal(t, "settled "+operation, author.call(t, operation, []any{handle, frozen})); code != "GENERATION_RUN_ALREADY_SETTLED" {
			t.Fatalf("a settled %s refused with %q; want GENERATION_RUN_ALREADY_SETTLED", operation, code)
		}
		if got := productionLifecycleRunStatus(t, ledger, runID); got != "cancelled" {
			t.Fatalf("a settled %s moved the run row to %q", operation, got)
		}
		if got := productionLifecycleRootStatus(t, ledger, rootID); got != "cancelled" {
			t.Fatalf("a settled %s moved the root row to %q", operation, got)
		}
	}
	// generation:read still reports the settled status the ledger holds.
	var view map[string]any
	productionEditorDecode(t, author, "generation:read", []any{handle, frozen}, &view)
	if view["status"] != "cancelled" {
		t.Fatalf("generation:read reported %v after the settled refusals; want cancelled", view["status"])
	}
	if seam.dialed() != 0 {
		t.Fatalf("the settled refusals dispatched %d times", seam.dialed())
	}
}
