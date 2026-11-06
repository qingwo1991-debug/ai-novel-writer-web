//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/agentgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The nonce this test spends once, the identity agentgen.SessionKey turns into
// the session the seeded round froze, and the lease this session never held.
const (
	productionAgentGenerationNonce      = "agent-generation-nonce-1"
	productionAgentGenerationStaleLease = "agent-generation-stale-lease"
	productionAgentGenerationRoot       = "agent-generation-root-0"
	productionAgentGenerationToolCall   = "agent-generation-tool-call-0"
)

// TestProductionLaneRefusesAgentGeneration pins the eight agent-generation:*
// names where this lane leaves them: unbound, answering the fail-closed 501
// GO_OPERATION_NOT_MIGRATED. One subtest per operation, so a failure names the
// channel that moved.
//
// The fixture is a disposable data root whose admitted project carries one real
// round-0 run, written through the shipped ledger before the admission permit
// hashed the file. That row is the positive half of this test: operation
// agent-round with a frozen agentSession key is admissible TODAY
// (internal/store/generation_ledger.go:428-429), so what this family is missing
// is the round protocol, not the ledger. The handle every request below names is
// read back out of that file over its own connection, so nothing is asserted
// against a constant.
//
// Under every request - the reference payload, a stale lease and an absent one -
// the answer is the same 501, because the answer is decided before the envelope,
// the lease or the run is looked at. The database then still holds exactly the
// run the fixture wrote, no attempt row, and the same bytes.
func TestProductionLaneRefusesAgentGeneration(t *testing.T) {
	root, projectPath, database, contract := productionGraphReadsRoot(t)
	frozenEpoch, rootActionID, runID, sessionKey := productionAgentGenerationSeed(t, database)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "agent-generation-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	projectID := syntheticString(opened.Project["id"])
	leaseID := syntheticString(opened.Project["sessionLease"])
	if projectID != productionGraphReadsProjectID || leaseID == "" {
		t.Fatalf("project:open returned %v/%v, want the seeded project with a live lease", projectID, leaseID)
	}

	// The seeded run is what a handle names: its frozen epoch, its root and its
	// run id. The live lease is what the browser would send in the envelope, and
	// the stale one is a lease no session holds.
	frozenHandle := map[string]any{"projectId": projectID, "epoch": frozenEpoch,
		"rootActionId": rootActionID, "runId": runID}
	liveHandle := map[string]any{"projectId": projectID, "epoch": leaseID,
		"rootActionId": rootActionID, "runId": runID}
	ref := map[string]any{"handle": frozenHandle, "attemptId": "agent-generation-attempt-0",
		"toolCallId": productionAgentGenerationToolCall}
	frozen := map[string]any{"projectId": projectID, "projectPath": projectPath, "leaseId": leaseID}
	stale := map[string]any{"projectId": projectID, "projectPath": projectPath,
		"leaseId": productionAgentGenerationStaleLease}
	unleased := map[string]any{"projectId": projectID, "projectPath": projectPath}

	// The round run the ledger admitted: operation agent-round, round index 0, and
	// the session key derived from [project, nonce]. This is the assertion that
	// keeps the report honest about where the family is blocked.
	manifest := productionAgentGenerationFrozen(t, database, runID)
	if syntheticString(manifest["operation"]) != agentgen.Operation {
		t.Fatalf("the seeded run froze operation %v, want %v", manifest["operation"], agentgen.Operation)
	}
	session := syntheticMap(manifest["agentSession"])
	if syntheticString(session["key"]) != sessionKey || syntheticNumber(session["roundIndex"]) != 0 {
		t.Fatalf("the seeded run froze agentSession %+v, want the derived key at round 0", session)
	}

	before := productionGraphReadsBytes(t, database)
	for _, operation := range productionAgentGenerationOperations() {
		operation := operation
		t.Run(operation, func(t *testing.T) {
			productionAgentGenerationSeamProof(t, operation)
			// Every request shape the reference sends, under the live lease, the
			// stale lease and no lease at all: one answer, and no write.
			for _, envelope := range []map[string]any{frozen, stale, unleased} {
				for _, payload := range productionAgentGenerationRequests(operation, frozenHandle, liveHandle, ref) {
					productionAgentGenerationRefused(t, author, operation, []any{payload, envelope})
				}
			}
		})
	}

	// Nothing was reserved, dispatched, written or claimed: the file still holds
	// one run, no attempt, and the same bytes.
	if runs := productionAgentGenerationCount(t, database,
		"SELECT COUNT(*) FROM generation_runs WHERE root_action_id=?", rootActionID); runs != 1 {
		t.Fatalf("the refused family left %d runs under the slot, want the one the fixture wrote", runs)
	}
	if attempts := productionAgentGenerationCount(t, database,
		"SELECT COUNT(*) FROM generation_attempts WHERE root_action_id=?", rootActionID); attempts != 0 {
		t.Fatalf("the refused family left %d attempt rows", attempts)
	}
	productionGraphReadsUnchanged(t, database, before, "agent-generation:*")

	t.Run("binder", func(t *testing.T) {
		// The binder this lane would wire installs nothing over a real server, so
		// inserting it cannot collide with any served name.
		server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := bindProductionAgentGeneration(server); err != nil {
			t.Fatalf("the refusing binder failed: %v", err)
		}
		if names := server.BoundOperations(); len(names) != 0 {
			t.Fatalf("the binder installed %v, want nothing", names)
		}
		if err := bindProductionAgentGeneration(nil); err == nil {
			t.Fatal("a nil server was accepted")
		}
		// Each name is inventoried, so an absent handler is exactly 501
		// GO_OPERATION_NOT_MIGRATED and never 404 OPERATION_NOT_EXPOSED.
		for _, operation := range productionAgentGenerationOperations() {
			fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
			if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
				return nil, nil
			}); err != nil {
				t.Fatalf("%v is not inventoried, so its unbound answer would not be 501: %v", operation, err)
			}
			// Every one of the eight freezes the same ledger operation, which is the
			// one mapping this family needs.
			ledgerOperation, found := productionAgentGenerationLedgerOperation(operation)
			if !found || ledgerOperation != agentgen.Operation {
				t.Fatalf("%v maps to %v/%v, want agentgen.Operation", operation, ledgerOperation, found)
			}
		}
		if _, found := productionAgentGenerationLedgerOperation("agent-generation:nonexistent"); found {
			t.Fatal("a name the family does not list still maps to a ledger operation")
		}
		// None of the eight is bound by the lane this test just drove.
		for _, bound := range productionLaneBoundOperations(t, handler) {
			for _, operation := range productionAgentGenerationOperations() {
				if bound == operation {
					t.Fatalf("%v is bound by the production lane", operation)
				}
			}
		}
	})
}

// productionAgentGenerationRequests is the reference payload set for one name
// (electron/controllers/generation-controller.ts:244-257, arity 1 plus the
// project-session envelope). It covers the request the desktop sends, a replay
// and an out-of-range round index, the two claim shapes, and the empty object,
// so a handler that only tolerated one shape could not pass.
func productionAgentGenerationRequests(operation string, handle, live, ref map[string]any) []map[string]any {
	switch operation {
	case productionAgentGenerationBegin:
		input := map[string]any{"mode": "fast", "uiLocale": "zh-CN", "historyMessages": []any{},
			"userMessage": "续写下一章的开头", "tools": []any{}}
		return []map[string]any{
			{"uiActionNonce": productionAgentGenerationNonce, "modelId": productionTestModelID, "input": input},
			{"uiActionNonce": productionAgentGenerationNonce, "modelId": productionTestModelID},
			{},
		}
	case productionAgentGenerationRead, productionAgentGenerationResume:
		return []map[string]any{{"handle": handle}, {"handle": live}, {}}
	case productionAgentGenerationRound:
		return []map[string]any{
			{"handle": handle, "index": 0},
			{"handle": live, "index": 0},
			{"handle": handle, "index": 1},
			{"handle": handle, "index": agentgen.MaxRounds},
			{"handle": handle, "index": -1},
			{},
		}
	case productionAgentGenerationClaimTool:
		return []map[string]any{
			{"ref": ref, "confirmed": true},
			{"ref": ref, "confirmed": false, "authorBlueprintProposals": []any{}},
			{},
		}
	case productionAgentGenerationFinishTool:
		return []map[string]any{
			{"ref": ref, "status": "completed", "observation": "草稿工作流已登记"},
			{"ref": ref, "status": "unknown", "observation": ""},
			{},
		}
	case productionAgentGenerationRegister, productionAgentGenerationCommitDomain:
		return []map[string]any{{"ref": ref}, {}}
	}
	return nil
}

// productionAgentGenerationRefused asserts one request gets the fail-closed 501.
func productionAgentGenerationRefused(t *testing.T, client *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%v = %d %s, want 501 GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
	}
}

// productionAgentGenerationSeamProof re-reads the code every seam row points at.
// The marker is searched for across the whole file rather than only at the
// recorded line: a seam that is still documented but has been shifted by an
// unrelated edit is not a regression, while a seam that was deleted is. The
// recorded line is still reported so a reader can find it quickly.
func productionAgentGenerationSeamProof(t *testing.T, operation string) {
	t.Helper()
	seam, found := productionAgentGenerationSeamFor(operation)
	if !found {
		t.Fatalf("%v has no documented seam", operation)
	}
	if len(seam.Missing) == 0 || seam.Ledger != agentgen.Operation {
		t.Fatalf("%v documents no missing piece or no ledger operation", operation)
	}
	for _, line := range seam.Missing {
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%v: %v: %v", operation, line.Path, err)
		}
		if !productionSeamContains(string(raw), line.Marker) {
			t.Fatalf("%v no longer documents %v anywhere in %v (recorded at line %d)",
				operation, line.Marker, line.Path, line.Line)
		}
	}
}

// productionAgentGenerationSeed writes one round-0 run of this project through
// the shipped ledger, before the lane opens the file, and returns the epoch,
// root, run id and session key it froze. A ledger that refused the agentSession
// manifest would fail here instead of silently leaving the test without a run.
func productionAgentGenerationSeed(t *testing.T, database string) (epoch, rootActionID, runID, sessionKey string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database for seeding: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ordinal := 0
	ledger := store.NewGenerationLedger(db,
		func() int64 { return 1700000000000 },
		func() string {
			ordinal++
			return "agent-generation-ledger-" + strconv.Itoa(ordinal)
		})
	epoch = productionGraphReadsFrozenEpoch
	sessionKey, err = agentgen.SessionKey(productionGraphReadsProjectID, productionAgentGenerationNonce)
	if err != nil {
		t.Fatalf("derive the session key: %v", err)
	}
	manifest := map[string]any{
		"operation":    agentgen.Operation,
		"modelId":      productionTestModelID,
		"output":       "visible-text",
		"promptKeys":   []any{"assistant_writing_identity"},
		"agentSession": map[string]any{"key": sessionKey, "roundIndex": 0},
	}
	run := productionGraphReadsSeedRun(t, context.Background(), ledger, productionGraphReadsSeedRequest(t,
		agentgen.Operation, productionGraphReadsProjectID, epoch, manifest))
	rootActionID = syntheticString(run["rootActionId"])
	runID = syntheticString(run["runId"])
	if rootActionID == "" || runID == "" {
		t.Fatalf("the ledger open returned no run identity: %v", run)
	}
	// The permit hashes the file at project:open, so the row has to be on disk
	// before it runs.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return epoch, rootActionID, runID, sessionKey
}

// productionAgentGenerationReadOnly opens the admitted author database without
// write authority, so nothing this test reads can repair what it is checking.
func productionAgentGenerationReadOnly(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database read-only: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// productionAgentGenerationFrozen reads one run's frozen source manifest back
// over its own read-only connection.
func productionAgentGenerationFrozen(t *testing.T, database, runID string) map[string]any {
	t.Helper()
	db := productionAgentGenerationReadOnly(t, database)
	defer func() { _ = db.Close() }()
	var binding string
	if err := db.QueryRow("SELECT binding_json FROM generation_runs WHERE run_id=?", runID).Scan(&binding); err != nil {
		t.Fatalf("read the seeded binding: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(binding), &decoded); err != nil {
		t.Fatalf("decode the seeded binding: %v", err)
	}
	manifest := syntheticMap(decoded["sourceManifest"])
	if manifest == nil {
		t.Fatalf("the seeded run froze no source manifest: %v", binding)
	}
	return manifest
}

// productionAgentGenerationCount reads one integer back over a read-only
// connection: what a write would have added is only absent when the file says so.
func productionAgentGenerationCount(t *testing.T, database, query string, args ...any) int {
	t.Helper()
	db := productionAgentGenerationReadOnly(t, database)
	defer func() { _ = db.Close() }()
	var found int
	if err := db.QueryRow(query, args...).Scan(&found); err != nil {
		t.Fatalf("read %v: %v", query, err)
	}
	return found
}

// TestProductionAgentGenerationRoundProtocol pins the rules the family's eight
// names are built on while no handler may serve them. The rules are the whole
// substance of internal/agentgen, so they are tested against the reference's
// order rather than against a running lane: there is no port to run them on yet.
//
// The two cases that a naive implementation would pass and the reference would
// not are called out where they are asserted: a session key that ignores the
// project, and a round admission that checks order before replay.
func TestProductionAgentGenerationRoundProtocol(t *testing.T) {
	key, err := agentgen.SessionKey(productionGraphReadsProjectID, productionAgentGenerationNonce)
	if err != nil || len(key) != 64 {
		t.Fatalf("session key = %v (%v), want 64 hex", key, err)
	}
	session := agentgen.Session{Key: key, RoundIndex: 0}
	root := productionAgentGenerationRoot

	t.Run("register then run one round", func(t *testing.T) {
		// A begin freezes round 0 before anything dispatches it, so the recovery
		// has one not-started round and the only index it may execute is 0 - not
		// one past the number of rounds.
		recovery := agentgen.Recovery{Session: session, RootActionID: root, SourceCurrent: true,
			Rounds: []agentgen.Round{{Index: 0, Status: agentgen.RoundNotStarted}}}
		next, err := agentgen.NextRound(recovery.Rounds)
		if err != nil || next == nil || *next != 0 {
			t.Fatalf("next round is not 0 for a frozen round 0: %v", err)
		}
		decision, err := agentgen.DecideRound(recovery, 0)
		if err != nil || decision.Kind != agentgen.KindExecute {
			t.Fatalf("round 0 answered %v (%v), want execute", decision.Kind, err)
		}

		// The round's own model call is the run's one attempt. A stop whose artifact
		// text is the response's visible text completes the round and projects its
		// actions, pending, in call order.
		attempt := &agentgen.Attempt{Status: agentgen.AttemptSettled, FinishReason: "stop",
			HasResponse: true, ResponseVersion: 1, ResponseText: "我先登记一个草稿工作流。",
			ArtifactText: "我先登记一个草稿工作流。",
			ToolCalls: []agentgen.ToolCall{{Name: "start_workflow",
				Arguments: map[string]any{"workflow": agentgen.WorkflowDraft, "chapter_number": 3}}}}
		round := agentgen.ClassifyRound(0, attempt)
		if round.Status != agentgen.RoundCompleted || len(round.Actions) != 1 ||
			round.Actions[0].Status != agentgen.ActionPending {
			t.Fatalf("the completed attempt projected %v with %d actions", round.Status, len(round.Actions))
		}

		// A registration is metadata: the child begin must re-prove every field.
		action := round.Actions[0]
		action.Status = agentgen.ActionRunning
		registration, created, err := agentgen.Register(action, productionAgentGenerationToolCall,
			productionTestModelID, root, true, true, true)
		if err != nil || !created {
			t.Fatalf("register refused a running start_workflow: %v", err)
		}
		if registration.Workflow != agentgen.WorkflowDraft || registration.ChapterNumber != 3 ||
			registration.ParentRootActionID != root || registration.ModelID != productionTestModelID {
			t.Fatalf("the registration froze %+v, not the action's own workflow", registration)
		}
		// Registering the same action twice returns the same metadata and stamps
		// nothing a second time.
		againAction := action
		againAction.Registration = &registration
		again, created, err := agentgen.Register(againAction, productionAgentGenerationToolCall,
			productionTestModelID, root, true, true, true)
		if err != nil || created || again.ID != registration.ID {
			t.Fatalf("the second registration created=%v id=%v, want the same metadata", created, again.ID)
		}
		admission, err := agentgen.AdmitRegistration([]agentgen.Registration{registration}, agentgen.ChildRequest{
			RegistrationID: registration.ID, Operation: "chapter-draft", ModelID: productionTestModelID,
			ParentRootActionID: root, ParentOperation: agentgen.Operation, OwnerStatus: agentgen.ActionRunning,
			ChapterNumber: 3, HasChapterNumber: true, RootCurrent: true, SourcesCurrent: true})
		if err != nil || admission.Kind != agentgen.AdmitNew {
			t.Fatalf("the registered child was not admitted: %v", err)
		}

		// The settled action is what the next round's input carries, and the next
		// index is 1 only once every action reached a durable outcome.
		round.Actions[0].Status = agentgen.ActionCompleted
		round.Actions[0].Observation = "草稿工作流已登记"
		messages, err := agentgen.Protocol(round)
		if err != nil || len(messages) != 2 {
			t.Fatalf("the round protocol produced %d messages (%v), want 2", len(messages), err)
		}
		if messages[0].Role != "assistant" || !strings.Contains(messages[0].Content, round.VisibleText) ||
			!strings.Contains(messages[0].Content, "<tool_call>") {
			t.Fatal("the assistant message does not carry the reply and its tool call")
		}
		if messages[1].Role != "user" ||
			!strings.Contains(messages[1].Content, `<tool_result name="start_workflow" status="completed">`) {
			t.Fatal("the tool results message does not carry the settled action")
		}
		completed := agentgen.Recovery{Session: session, RootActionID: root, SourceCurrent: true,
			Rounds: []agentgen.Round{round}}
		next, err = agentgen.NextRound(completed.Rounds)
		if err != nil || next == nil || *next != 1 {
			t.Fatalf("next round is not 1 after a settled completed round: %v", err)
		}
		decision, err = agentgen.DecideRound(completed, 1)
		if err != nil || decision.Kind != agentgen.KindExecute {
			t.Fatalf("round 1 answered %v (%v), want execute", decision.Kind, err)
		}
	})

	t.Run("a replay of the same round index is idempotent", func(t *testing.T) {
		// The reference checks replay before order, so a round that already ran is
		// answered from the stored recovery even though it is no longer the next
		// round. It is idempotent, not refused: the replay writes no second attempt.
		rounds := []agentgen.Round{{Index: 0, Status: agentgen.RoundCompleted, VisibleText: "done",
			Actions: []agentgen.Action{{Name: "start_workflow", Status: agentgen.ActionCompleted}}}}
		recovery := agentgen.Recovery{Session: session, RootActionID: root, SourceCurrent: true, Rounds: rounds}
		decision, err := agentgen.DecideRound(recovery, 0)
		if err != nil {
			t.Fatalf("the replay of round 0 refused: %v", err)
		}
		if decision.Kind != agentgen.KindReplay {
			t.Fatalf("round 0 answered %v, want a replay", decision.Kind)
		}
		// A round whose invocation may have reached the provider is not replayed
		// and not advanced either: it is unknown, and it stays round 0.
		unknown := agentgen.Recovery{Session: session, RootActionID: root, SourceCurrent: true,
			Rounds: []agentgen.Round{{Index: 0, Status: agentgen.RoundUnknown}}}
		decision, err = agentgen.DecideRound(unknown, 0)
		if err != nil || decision.Kind != agentgen.KindReplay {
			t.Fatalf("the unknown round answered %v (%v), want a replay", decision.Kind, err)
		}
		if next, err := agentgen.NextRound(unknown.Rounds); err != nil || next != nil {
			t.Fatalf("the unknown round promoted the session: %v", err)
		}
	})

	t.Run("an out-of-order round refuses", func(t *testing.T) {
		rounds := []agentgen.Round{{Index: 0, Status: agentgen.RoundCompleted,
			Actions: []agentgen.Action{{Name: "start_workflow", Status: agentgen.ActionCompleted}}}}
		recovery := agentgen.Recovery{Session: session, RootActionID: root, SourceCurrent: true, Rounds: rounds}
		for _, index := range []int64{2, 3, agentgen.MaxRounds - 1} {
			_, err := agentgen.DecideRound(recovery, index)
			if code := productionAgentGenerationCode(t, err); code != agentgen.CodeRoundOrder {
				t.Fatalf("round %d answered %v, want GENERATION_AGENT_ROUND_ORDER", index, code)
			}
		}
		// An index outside the bound is its own refusal, not an ordering one.
		for _, index := range []int64{-1, agentgen.MaxRounds} {
			_, err := agentgen.DecideRound(recovery, index)
			if code := productionAgentGenerationCode(t, err); code != agentgen.CodeRoundInvalid {
				t.Fatalf("round %d answered %v, want GENERATION_AGENT_ROUND_INVALID", index, code)
			}
		}
		// A gap in the recovery is a gap: no index is computed over it.
		gap := []agentgen.Round{{Index: 0, Status: agentgen.RoundCompleted}, {Index: 2, Status: agentgen.RoundCompleted}}
		_, err := agentgen.NextRound(gap)
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeRoundGap {
			t.Fatalf("the gapped recovery answered %v, want GENERATION_AGENT_ROUND_GAP", code)
		}
	})

	t.Run("an unregistered workflow refuses", func(t *testing.T) {
		// No registration at all under an agent-round parent is a refusal, not an
		// ordinary begin; the same request outside that parent is ordinary.
		_, err := agentgen.AdmitRegistration(nil, agentgen.ChildRequest{Operation: "chapter-draft",
			ParentOperation: agentgen.Operation})
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeRegistrationRequired {
			t.Fatalf("an unregistered child answered %v", code)
		}
		admission, err := agentgen.AdmitRegistration(nil, agentgen.ChildRequest{Operation: "chapter-draft"})
		if err != nil || admission.Kind != agentgen.AdmitOrdinary {
			t.Fatalf("an ordinary begin answered %v (%v)", admission.Kind, err)
		}
		// A malformed id is not a lookup miss, and a well-formed id nobody
		// registered is.
		_, err = agentgen.AdmitRegistration(nil, agentgen.ChildRequest{Operation: "chapter-draft",
			RegistrationID: "agent-workflow:not-a-digest"})
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeRegistrationInvalid {
			t.Fatalf("a malformed registration id answered %v", code)
		}
		_, err = agentgen.AdmitRegistration(nil, agentgen.ChildRequest{Operation: "chapter-draft",
			RegistrationID: "agent-workflow:" + strings.Repeat("a", 64)})
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeRegistrationMissing {
			t.Fatalf("an unknown registration id answered %v", code)
		}

		registration := agentgen.Registration{ID: "agent-workflow:" + strings.Repeat("b", 64),
			Workflow: agentgen.WorkflowDraft, ChapterNumber: 3, HasChapterNumber: true,
			ParentRootActionID: root, ModelID: productionTestModelID, State: agentgen.RegistrationRegistered}
		registrations := []agentgen.Registration{registration}
		base := agentgen.ChildRequest{RegistrationID: registration.ID, Operation: "chapter-draft",
			ModelID: productionTestModelID, ParentRootActionID: root, ParentOperation: agentgen.Operation,
			OwnerStatus: agentgen.ActionRunning, ChapterNumber: 3, HasChapterNumber: true,
			RootCurrent: true, SourcesCurrent: true}
		if admission, err := agentgen.AdmitRegistration(registrations, base); err != nil || admission.Kind != agentgen.AdmitNew {
			t.Fatalf("the registered child was not admitted: %v", err)
		}
		for _, mutate := range []struct {
			label string
			edit  func(*agentgen.ChildRequest)
		}{
			{"another operation", func(request *agentgen.ChildRequest) { request.Operation = "generate-plot-outline" }},
			{"another model", func(request *agentgen.ChildRequest) { request.ModelID = "another-model" }},
			{"another parent root", func(request *agentgen.ChildRequest) { request.ParentRootActionID = "another-root" }},
			{"another chapter", func(request *agentgen.ChildRequest) { request.ChapterNumber = 4 }},
			{"a batch child", func(request *agentgen.ChildRequest) { request.Batch = true }},
			{"a settled owner", func(request *agentgen.ChildRequest) { request.OwnerStatus = agentgen.ActionDeclined }},
		} {
			request := base
			mutate.edit(&request)
			_, err := agentgen.AdmitRegistration(registrations, request)
			if code := productionAgentGenerationCode(t, err); code != agentgen.CodeChildNotAuthorized {
				t.Fatalf("%v answered %v, want GENERATION_AGENT_CHILD_NOT_AUTHORIZED", mutate.label, code)
			}
		}
		// A planning registration authorizes exactly its own range.
		planning := registration
		planning.ID = "agent-workflow:" + strings.Repeat("c", 64)
		planning.Workflow = agentgen.WorkflowArchitecture
		planning.HasChapterNumber = false
		planning.Planning = &agentgen.Planning{From: 4, To: 9, TargetUnits: 6}
		planned := base
		planned.RegistrationID = planning.ID
		planned.Operation = "generate-plot-outline"
		planned.HasChapterNumber = false
		planned.ChapterNumber = 0
		planned.Planning = &agentgen.Planning{From: 4, To: 9, TargetUnits: 6}
		if admission, err := agentgen.AdmitRegistration([]agentgen.Registration{planning}, planned); err != nil || admission.Kind != agentgen.AdmitNew {
			t.Fatalf("the planned child was not admitted: %v", err)
		}
		planned.Planning = &agentgen.Planning{From: 4, To: 10, TargetUnits: 6}
		_, err = agentgen.AdmitRegistration([]agentgen.Registration{planning}, planned)
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeChildNotAuthorized {
			t.Fatalf("a widened planning range answered %v", code)
		}
	})

	t.Run("a stale lease refuses", func(t *testing.T) {
		pending := agentgen.Action{Name: "start_workflow", Status: agentgen.ActionPending}
		staleClaim := agentgen.ClaimRequest{Confirmed: true, EpochCurrent: false, SourcesCurrent: true,
			RootActive: true, ToolKnown: true}
		if _, err := agentgen.DecideClaim(pending, staleClaim); err == nil {
			t.Fatal("a claim under a stale epoch was admitted")
		} else if err.Error() != agentgen.CodeToolStale {
			t.Fatalf("a stale claim answered %v, want GENERATION_AGENT_TOOL_STALE", err)
		}
		for _, request := range []agentgen.ClaimRequest{
			{Confirmed: true, EpochCurrent: true, SourcesCurrent: false, RootActive: true, ToolKnown: true},
			{Confirmed: true, EpochCurrent: true, SourcesCurrent: true, RootActive: false, ToolKnown: true},
		} {
			_, err := agentgen.DecideClaim(pending, request)
			if code := productionAgentGenerationCode(t, err); code != agentgen.CodeToolStale {
				t.Fatalf("a drifting lease answered %v", code)
			}
		}
		// A finish under a stale epoch is its own code, and a round whose frozen
		// sources drifted is a source refusal.
		_, err := agentgen.DecideFinish(agentgen.Action{Name: "start_workflow", Status: agentgen.ActionRunning},
			agentgen.FinishRequest{Status: agentgen.ActionCompleted, Observation: "done", EpochCurrent: false})
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeEpochStale {
			t.Fatalf("a stale finish answered %v", code)
		}
		_, err = agentgen.DecideRound(agentgen.Recovery{Session: session, RootActionID: root, SourceCurrent: false,
			Rounds: []agentgen.Round{{Index: 0, Status: agentgen.RoundNotStarted}}}, 0)
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeSourceChanged {
			t.Fatalf("a drifting source answered %v", code)
		}
		// An action that already ran answers before the lease is read at all, so a
		// replayed claim over a closed session is a noop and not a stale refusal.
		kind, err := agentgen.DecideClaim(agentgen.Action{Name: "start_workflow", Status: agentgen.ActionRunning}, staleClaim)
		if err != nil || kind != agentgen.ClaimNoop {
			t.Fatalf("the replayed claim answered %v (%v), want a noop", kind, err)
		}
	})

	t.Run("a failed round writes nothing and cannot advance", func(t *testing.T) {
		// A dispatch-marked attempt may have reached the provider, so it is unknown:
		// never redispatched, never promoted, and it projects no action.
		attempt := &agentgen.Attempt{Status: agentgen.AttemptDispatchMarked}
		round := agentgen.ClassifyRound(0, attempt)
		if round.Status != agentgen.RoundUnknown || len(round.Actions) != 0 {
			t.Fatalf("the interrupted attempt projected %v with %d actions", round.Status, len(round.Actions))
		}
		// Every unusable response is incomplete rather than completed, so it can
		// neither settle an action nor open the next round.
		for _, unusable := range []*agentgen.Attempt{
			{Status: agentgen.AttemptSettled, FinishReason: "length", HasResponse: true, ResponseVersion: 1,
				ResponseText: "a", ArtifactText: "a"},
			{Status: agentgen.AttemptSettled, FinishReason: "stop", HasResponse: true, ResponseVersion: 1,
				ResponseText: "a", ArtifactText: "b"},
			{Status: agentgen.AttemptSettled, FinishReason: "stop", HasResponse: true, ResponseVersion: 2,
				ResponseText: "a", ArtifactText: "a"},
			{Status: agentgen.AttemptReserved, FinishReason: "stop", HasResponse: true, ResponseVersion: 1,
				ResponseText: "a", ArtifactText: "a"},
		} {
			projected := agentgen.ClassifyRound(0, unusable)
			if projected.Status == agentgen.RoundCompleted || len(projected.Actions) != 0 {
				t.Fatalf("an unusable response projected %v with %d actions", projected.Status, len(projected.Actions))
			}
			recovery := agentgen.Recovery{Session: session, RootActionID: root, SourceCurrent: true,
				Rounds: []agentgen.Round{projected}}
			if next, err := agentgen.NextRound(recovery.Rounds); err != nil || next != nil {
				t.Fatalf("an unusable response advanced the session: %v", err)
			}
		}
		// A completed round whose actions are still pending cannot advance either:
		// the next round would run on top of a tool result nobody waited for.
		blocked := agentgen.Recovery{Session: session, RootActionID: root, SourceCurrent: true,
			Rounds: []agentgen.Round{{Index: 0, Status: agentgen.RoundCompleted,
				Actions: []agentgen.Action{{Name: "start_workflow", Status: agentgen.ActionRunning}}}}}
		if next, err := agentgen.NextRound(blocked.Rounds); err != nil || next != nil {
			t.Fatalf("a round with an unsettled action advanced the session: %v", err)
		}
		// A finished builtin domain tool without its domain write refuses, and a
		// finish that contradicts the stored action refuses.
		_, err := agentgen.DecideFinish(agentgen.Action{Name: "propose_chapter_blueprint", Status: agentgen.ActionRunning},
			agentgen.FinishRequest{Status: agentgen.ActionCompleted, Observation: "done", EpochCurrent: true})
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeDomainEffectRequired {
			t.Fatalf("a settled domain tool without an effect answered %v", code)
		}
		_, err = agentgen.DecideFinish(agentgen.Action{Name: "start_workflow", Status: agentgen.ActionPending},
			agentgen.FinishRequest{Status: agentgen.ActionCompleted, Observation: "done", EpochCurrent: true})
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeToolResultConflict {
			t.Fatalf("a finish on a pending action answered %v", code)
		}
	})

	t.Run("session scoping and replay order are the reference's, not the obvious ones", func(t *testing.T) {
		// A naive key would hash the nonce alone, and then one project's begin
		// would replay another project's session. The reference hashes the pair.
		other, err := agentgen.SessionKey(productionGraphReadsForeignProjectID, productionAgentGenerationNonce)
		if err != nil {
			t.Fatalf("derive the second session key: %v", err)
		}
		if other == key {
			t.Fatal("the session key ignores the project, so one nonce would collide across projects")
		}
		// A handle that names a run of another project refuses even when the run id
		// matches: a lookup by run id alone would accept it.
		foreign := agentgen.Frozen{ProjectID: productionGraphReadsForeignProjectID, Epoch: productionGraphReadsFrozenEpoch,
			RootActionID: root, RunID: "agent-generation-run-0", Operation: agentgen.Operation, Session: session}
		handle := agentgen.Handle{ProjectID: productionGraphReadsProjectID, Epoch: productionGraphReadsFrozenEpoch,
			RootActionID: root, RunID: "agent-generation-run-0"}
		_, err = agentgen.MatchIdentity(handle, foreign)
		if code := productionAgentGenerationCode(t, err); code != agentgen.CodeIdentityMismatch {
			t.Fatalf("a foreign run answered %v, want GENERATION_AGENT_IDENTITY_MISMATCH", code)
		}
		own := foreign
		own.ProjectID = productionGraphReadsProjectID
		frozen, err := agentgen.MatchIdentity(handle, own)
		if err != nil || frozen.Key != key {
			t.Fatalf("the run's own handle answered %v (%v)", frozen.Key, err)
		}
		// And the replay-before-order rule: round 0 is replayed, not refused, even
		// though the next round is 1. An implementation that checked order first
		// would answer ROUND_ORDER for the same request.
		recovery := agentgen.Recovery{Session: session, RootActionID: root, SourceCurrent: true,
			Rounds: []agentgen.Round{
				{Index: 0, Status: agentgen.RoundCompleted, Actions: []agentgen.Action{{Name: "start_workflow", Status: agentgen.ActionCompleted}}},
				{Index: 1, Status: agentgen.RoundNotStarted},
			}}
		replayed, err := agentgen.DecideRound(recovery, 0)
		if err != nil || replayed.Kind != agentgen.KindReplay {
			t.Fatalf("the replay of a settled round answered %v (%v), want a replay", replayed.Kind, err)
		}
	})
}

// productionAgentGenerationCode returns a refusal's symbolic code, or fails the
// test when the decision was admitted.
func productionAgentGenerationCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("the decision was admitted, want a refusal")
	}
	return err.Error()
}
