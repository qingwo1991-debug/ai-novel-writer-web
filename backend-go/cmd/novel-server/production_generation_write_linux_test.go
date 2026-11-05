//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the eight write names production_generation_write_linux.go
// binds: one subtest per operation, each driven over the real HTTP handler and
// then read back through a connection of the test's own - the ledger store over a
// read-only handle, never the lane's admitted handle and never the receipt - so
// every assertion is on persisted state.
//
// The lane under test is a real production lane: the synthetic dispatch seam is
// nil for every name except the one subtest that needs an artifact to discard, and
// the committed model endpoint is a loopback listener this test owns. Nothing may
// dial, so that listener is inspected after every subtest the way
// production_model_egress_linux_test.go:255-270 inspects its own: a dial would
// already be queued for Accept.
const (
	productionGenerationWriteOpenNonce    = "generation-write-open-"
	productionGenerationWriteBeginNonce   = "generation-write-begin-1"
	productionGenerationWriteReviewNonce  = "generation-write-review-1"
	productionGenerationWriteRestartNonce = "generation-write-restart-1"
	productionGenerationWriteInvokeNonce  = "generation-write-invoke-1"
	// productionGenerationWriteReviewOperation is the one operation class a closed
	// material decision can be frozen into (synthetic_generation_linux.go:1571).
	productionGenerationWriteReviewOperation = "review-chapter"
	// productionGenerationWriteGenerated is the text the dispatch seam answers with
	// in the discard subtest, so the artifact it discards is a real one.
	productionGenerationWriteGenerated = "生产写入：门后有人。"
	// The frozen prose the finalization subtests work on. The body differs from the
	// fixture's so a commit that echoed the request instead of writing it is
	// detectable in the stored content.
	productionGenerationWriteFinalTitle = "第一章 门"
	productionGenerationWriteFinalBody  = "定稿正文：他推开门。\n第二行"
	// productionGenerationWriteFinalizationID is the seeded outbox row the retry and
	// knowledge-link subtests act on, minted by this fixture rather than by a commit
	// in a sibling subtest.
	productionGenerationWriteFinalizationID = "ci-generation-write-finalization-1"
	productionGenerationWriteDocumentID     = "kb:合成文档"
	// productionGenerationWriteSeedTargetFile is written by the fixture itself, not
	// derived: the commit path's own target-name derivation is asserted by the
	// durable lane's tests, which own that projection.
	productionGenerationWriteSeedTargetFile = "第一章 门.txt"
)

// productionGenerationWriteLane is one fresh production fixture, one loopback
// origin committed as the only model endpoint, and one open lane over it.
type productionGenerationWriteLane struct {
	projectPath string
	database    string
	listener    net.Listener
	origin      string
	author      *productionDurableClient
	frozen      map[string]any
}

// productionGenerationWriteListener is the no-dial witness: a loopback listener
// whose origin is the model endpoint the lane was opened with. It is closed at the
// end of the subtest and only ever Accepts after the operations under test ran.
func productionGenerationWriteListener(t *testing.T) (net.Listener, string) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener, "http://" + listener.Addr().String()
}

// productionGenerationWriteModel commits the fixture model profile with a
// generation purpose and the loopback endpoint, so the lane's committed
// model-egress policy names exactly the listener above. A dispatch that reached the
// provider would therefore connect to it instead of a documented placeholder host.
func productionGenerationWriteModel(t *testing.T, root, origin string) {
	t.Helper()
	path := productionCommittedModelsPath(root)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		if id, _ := record["id"].(string); id == productionTestModelID {
			record["purposes"] = []any{"generation"}
			record["baseUrl"] = origin + "/v1"
			found = true
		}
	}
	if !found {
		t.Fatalf("the fixture commits no model profile %q", productionTestModelID)
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

// productionGenerationWriteOpen builds the fixture, seeds the rows a subtest needs
// before the lane takes its writer lease, opens the production lane and one
// author session over it. dispatch is nil unless the subtest cannot create its
// subject without a dispatch (generation:discard-candidate needs an artifact).
func productionGenerationWriteOpen(t *testing.T, dispatch generation.Dispatch, seed func(t *testing.T, projectPath string)) *productionGenerationWriteLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	listener, origin := productionGenerationWriteListener(t)
	productionGenerationWriteModel(t, root, origin)
	if seed != nil {
		seed(t, projectPath)
	}
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes, Dispatch: dispatch,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	lane := &productionGenerationWriteLane{
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		listener:    listener,
		origin:      origin,
	}
	lane.author, lane.frozen = productionExecuteSession(t, handler, projectPath, productionGenerationWriteOpenNonce)
	return lane
}

// stale reports the three-key context a server that never issued this lease would
// have: the same project, a lease it cannot prove.
func (l *productionGenerationWriteLane) stale() map[string]any {
	return map[string]any{"projectId": l.frozen["projectId"], "projectPath": l.projectPath, "leaseId": "stale-lease"}
}

// begin opens one admitted run from the given selection and returns its handle.
func (l *productionGenerationWriteLane) begin(t *testing.T, selection map[string]any) map[string]any {
	t.Helper()
	result := productionStoryStateWriteCall(t, l.author, "generation:begin", []any{selection, l.frozen})
	handle, ok := result["handle"].(map[string]any)
	if !ok || handle["runId"] == "" || handle["rootActionId"] == "" {
		t.Fatalf("generation:begin returned no run handle: %v", result)
	}
	return handle
}

// bytes reads the committed author database file, the baseline a refused write has
// to leave untouched.
func (l *productionGenerationWriteLane) bytes(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(l.database)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// assertNoDial proves nothing under test reached the committed endpoint: a dial
// would already be queued on the listener, which never Accepted before this point.
func (l *productionGenerationWriteLane) assertNoDial(t *testing.T) {
	t.Helper()
	if tcp, ok := l.listener.(*net.TCPListener); ok {
		if err := tcp.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
	}
	if connection, acceptErr := l.listener.Accept(); acceptErr == nil {
		_ = connection.Close()
		t.Fatalf("a write dialed the committed origin %s", l.origin)
	}
}

// attempts snapshots one root's attempt rows through the ledger store, so a
// refusal can be asserted to leave reserved or settled usage exactly as it was.
func productionGenerationWriteAttempts(t *testing.T, ledger *store.GenerationLedger, rootID string) string {
	t.Helper()
	value, err := ledger.Call(context.Background(), "budget", []any{rootID})
	if err != nil {
		t.Fatalf("ledger budget %q: %v", rootID, err)
	}
	encoded, err := json.Marshal(syntheticSlice(syntheticMap(value)["attempts"]))
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// productionGenerationWriteDecision is a closed material decision of the shape the
// reduced admission accepts: version 1, admitted, a content hash and the capacity
// and coverage the renderer froze, with one included and no omitted source.
func productionGenerationWriteDecision() map[string]any {
	return map[string]any{
		"version":    float64(1),
		"verdict":    "admitted",
		"promptHash": strings.Repeat("a1", 32),
		"capacity":   map[string]any{"maxInputUnits": float64(4096), "methodVersion": "ci-material-1"},
		"coverage":   map[string]any{"complete": true},
		"included": []any{map[string]any{
			"sourceId": "draft:1", "category": "draft", "contentHash": strings.Repeat("b2", 32),
		}},
		"omitted": []any{},
	}
}

// productionGenerationWriteHash is the content hash the finalization outbox stores
// next to a frozen snapshot.
func productionGenerationWriteHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// productionGenerationWriteSeedOutbox freezes one outbox row before the lane opens,
// so finalization:retry and db:finalization-link-knowledge-document act on a
// genuinely committed row instead of one a sibling subtest minted.
func productionGenerationWriteSeedOutbox(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"UPDATE drafts SET status='finalized' WHERE id=1", nil},
		{`INSERT INTO finalization_outbox (finalization_id, draft_id, chapter_number, chapter_title,
 content_hash, content_revision, content_snapshot, target_file_name)
 VALUES (?, 1, 1, ?, ?, 1, ?, ?)`,
			[]any{productionGenerationWriteFinalizationID, productionGenerationWriteFinalTitle,
				productionGenerationWriteHash(productionEditorSeed), productionEditorSeed,
				productionGenerationWriteSeedTargetFile}},
	} {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			_ = db.Close()
			t.Fatalf("seed %q: %v", statement.query, err)
		}
	}
	productionEditorCheckpoint(t, db, database)
}

// TestProductionGenerationWritesServeTheLedgerMutationsThisLaneOwns drives the
// five generation names production_generation_write_linux.go binds. Each subtest
// works on its own fixture, its own loopback endpoint and its own admitted run, so
// a failure names one operation.
func TestProductionGenerationWritesServeTheLedgerMutationsThisLaneOwns(t *testing.T) {
	t.Run("generation:bind-material-decision", testProductionGenerationWriteMaterialDecision)
	t.Run("generation:discard-candidate", testProductionGenerationWriteDiscardCandidate)
	t.Run("generation:restart", testProductionGenerationWriteRestart)
	t.Run("graph-generation:cancel", func(t *testing.T) {
		testProductionGenerationWriteFamilyCancel(t, "graph-generation:cancel", "GENERATION_GRAPH_REQUEST_INVALID")
	})
	t.Run("finalization-generation:cancel", func(t *testing.T) {
		testProductionGenerationWriteFamilyCancel(t, "finalization-generation:cancel", "GENERATION_FINALIZATION_REQUEST_INVALID")
	})
	// The refused sibling is asserted last, so the same test states both halves of
	// the decision: the family is live and this name is still not bound.
	t.Run("generation:retry-draft-short-outline", testProductionGenerationWriteRefusesShortOutlineRetry)
}

// productionGenerationWriteDecisionOf reads one run's frozen manifest back through
// the ledger store on a connection of the test's own.
func productionGenerationWriteDecisionOf(t *testing.T, ledger *store.GenerationLedger, runID string) map[string]any {
	t.Helper()
	value, err := ledger.Call(context.Background(), "get", []any{runID})
	if err != nil {
		t.Fatalf("ledger get %q: %v", runID, err)
	}
	manifest := syntheticMap(syntheticMap(syntheticMap(value)["binding"])["sourceManifest"])
	if manifest == nil {
		t.Fatalf("the committed run %q holds no source manifest", runID)
	}
	return syntheticMap(manifest["materialDecision"])
}

// testProductionGenerationWriteMaterialDecision proves the closed decision is
// frozen into the run the ledger holds, that a different second decision is the
// run's own conflict, and that a lease this server never issued is refused before
// any row moves.
func testProductionGenerationWriteMaterialDecision(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	ledger := productionLifecycleLedger(t, lane.projectPath)
	selection := productionEditorBeginRequest(productionGenerationWriteReviewNonce)
	selection["operation"] = productionGenerationWriteReviewOperation
	selection["promptKeys"] = []any{"review"}
	handle := lane.begin(t, selection)
	runID, _ := handle["runId"].(string)

	decision := productionGenerationWriteDecision()
	result := productionStoryStateWriteCall(t, lane.author, "generation:bind-material-decision", []any{
		map[string]any{"handle": handle, "materialDecision": decision}, lane.frozen,
	})
	if answered, ok := result["handle"].(map[string]any); !ok || answered["runId"] != runID {
		t.Fatalf("generation:bind-material-decision answered for another run: %v", result["handle"])
	}
	// Write-then-readback: the manifest the ledger holds carries the decision, not
	// only the answer.
	bound := productionGenerationWriteDecisionOf(t, ledger, runID)
	if bound == nil || bound["verdict"] != "admitted" || bound["promptHash"] != decision["promptHash"] {
		t.Fatalf("the frozen manifest does not hold the decision: %v", bound)
	}
	if got := productionLifecycleRunStatus(t, ledger, runID); got != "running" {
		t.Fatalf("binding a decision moved the run to %q; want running", got)
	}
	// A different decision for the same run is the lane's own conflict, and the
	// frozen one stays.
	other := productionGenerationWriteDecision()
	other["promptHash"] = strings.Repeat("c3", 32)
	if code := productionLifecycleRefusal(t, "conflicting decision", lane.author.call(t, "generation:bind-material-decision",
		[]any{map[string]any{"handle": handle, "materialDecision": other}, lane.frozen})); code != "GENERATION_MATERIAL_DECISION_CONFLICT" {
		t.Fatalf("a conflicting decision refused with %q; want GENERATION_MATERIAL_DECISION_CONFLICT", code)
	}
	if again := productionGenerationWriteDecisionOf(t, ledger, runID); again["promptHash"] != decision["promptHash"] {
		t.Fatalf("a refused decision rewrote the frozen manifest: %v", again)
	}
	// The stale lease is refused by the reused admission, before a row is read.
	baseline := lane.bytes(t)
	if code := productionLifecycleRefusal(t, "stale-lease generation:bind-material-decision",
		lane.author.call(t, "generation:bind-material-decision",
			[]any{map[string]any{"handle": handle, "materialDecision": decision}, lane.stale()})); code == "" {
		t.Fatal("the stale-lease decision was refused without a symbolic code")
	}
	if again := productionGenerationWriteDecisionOf(t, ledger, runID); again["promptHash"] != decision["promptHash"] {
		t.Fatalf("a refused stale-lease decision rewrote the frozen manifest: %v", again)
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused material decision changed the committed database file")
	}
	lane.assertNoDial(t)
}

// productionGenerationWriteCandidate returns the one candidate the executed run
// reports, failing when the dispatch settled no artifact to discard.
func productionGenerationWriteCandidate(t *testing.T, executed map[string]any) map[string]any {
	t.Helper()
	run := syntheticMap(executed["run"])
	candidates := syntheticSlice(run["candidates"])
	if len(candidates) != 1 {
		t.Fatalf("the executed run reports %d candidates; want the one settled artifact", len(candidates))
	}
	candidate := syntheticMap(candidates[0])
	if candidate == nil || candidate["artifactId"] == "" {
		t.Fatalf("the executed run reports no artifact identity: %v", candidates[0])
	}
	return candidate
}

// testProductionGenerationWriteDiscardCandidate proves the discard is the
// artifact's status transition and nothing else: the attempt rows keep their
// settled usage, and a refused discard writes neither.
func testProductionGenerationWriteDiscardCandidate(t *testing.T) {
	// The artifact only exists once one dispatch settled. The seam answers that
	// dispatch inside the process, which is why this is the one subtest that opens
	// the lane with a dispatch at all - the endpoint below is still never contacted.
	seam := &productionGenerationSeam{text: productionGenerationWriteGenerated}
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)
	ledger := productionLifecycleLedger(t, lane.projectPath)
	handle := lane.begin(t, productionEditorBeginRequest(productionGenerationWriteBeginNonce))
	rootID, _ := handle["rootActionId"].(string)

	executed := productionExecuteAnswer(t, lane.author, productionExecuteRequest(handle, productionGenerationWriteInvokeNonce), lane.frozen)
	if seam.dispatched() != 1 {
		t.Fatalf("the admitted run dispatched %d times through the seam; want exactly 1", seam.dispatched())
	}
	artifactID, _ := productionGenerationWriteCandidate(t, executed)["artifactId"].(string)
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var artifactStatus string
	productionStoryStateWriteScan(t, readback,
		`SELECT status FROM generation_artifacts WHERE artifact_id=?`, []any{artifactID}, &artifactStatus)
	if artifactStatus != "partial" {
		t.Fatalf("the reserved artifact starts at %q; want partial", artifactStatus)
	}
	usage := productionGenerationWriteAttempts(t, ledger, rootID)

	discarded := productionStoryStateWriteCall(t, lane.author, "generation:discard-candidate",
		[]any{handle, artifactID, lane.frozen})
	if candidates := syntheticSlice(discarded["candidates"]); len(candidates) != 0 {
		t.Fatalf("the discarded candidate is still listed: %v", discarded["candidates"])
	}
	productionStoryStateWriteScan(t, readback,
		`SELECT status FROM generation_artifacts WHERE artifact_id=?`, []any{artifactID}, &artifactStatus)
	if artifactStatus != "discarded" {
		t.Fatalf("the committed artifact row is %q after the discard; want discarded", artifactStatus)
	}
	// A discarded candidate is not a settled attempt: the usage rows are untouched.
	if now := productionGenerationWriteAttempts(t, ledger, rootID); now != usage {
		t.Fatalf("the discard rewrote the attempt rows: %s -> %s", usage, now)
	}
	// The stale lease is refused before the status transition.
	baseline := lane.bytes(t)
	if code := productionLifecycleRefusal(t, "stale-lease generation:discard-candidate",
		lane.author.call(t, "generation:discard-candidate",
			[]any{handle, artifactID, lane.stale()})); code == "" {
		t.Fatal("the stale-lease discard was refused without a symbolic code")
	}
	productionStoryStateWriteScan(t, readback,
		`SELECT status FROM generation_artifacts WHERE artifact_id=?`, []any{artifactID}, &artifactStatus)
	if artifactStatus != "discarded" {
		t.Fatalf("a refused discard moved the artifact row to %q", artifactStatus)
	}
	if now := productionGenerationWriteAttempts(t, ledger, rootID); now != usage {
		t.Fatalf("a refused discard rewrote the attempt rows: %s -> %s", usage, now)
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused discard changed the committed database file")
	}
	lane.assertNoDial(t)
}

// testProductionGenerationWriteRestart proves restart is not a second admission
// path: it refuses the two requests that are not a new nonce, admits the
// replacement run through the same entry point generation:begin uses, settles the
// old root with the ordinary stop, and reserves nothing for dispatch.
func testProductionGenerationWriteRestart(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	ledger := productionLifecycleLedger(t, lane.projectPath)
	handle := lane.begin(t, productionEditorBeginRequest(productionGenerationWriteBeginNonce))
	oldRun, _ := handle["runId"].(string)
	oldRoot, _ := handle["rootActionId"].(string)
	if got := productionLifecycleRunStatus(t, ledger, oldRun); got != "running" {
		t.Fatalf("the admitted run starts at %q; want running", got)
	}
	oldUsage := productionGenerationWriteAttempts(t, ledger, oldRoot)

	// The same nonce is not a restart: the refusal happens before the replacement
	// run is admitted, so the old run keeps running.
	repeated := productionEditorBeginRequest(productionGenerationWriteBeginNonce)
	if code := productionLifecycleRefusal(t, "same-nonce restart",
		lane.author.call(t, "generation:restart", []any{handle, repeated, lane.frozen})); code != "GENERATION_RESTART_NEW_NONCE_REQUIRED" {
		t.Fatalf("a same-nonce restart refused with %q; want GENERATION_RESTART_NEW_NONCE_REQUIRED", code)
	}
	// Restart mints a new root, never a child of the old one.
	parented := productionEditorBeginRequest(productionGenerationWriteRestartNonce)
	parented["parentRootActionId"] = oldRoot
	if code := productionLifecycleRefusal(t, "parented restart",
		lane.author.call(t, "generation:restart", []any{handle, parented, lane.frozen})); code != "GENERATION_RESTART_NEW_NONCE_REQUIRED" {
		t.Fatalf("a parented restart refused with %q; want GENERATION_RESTART_NEW_NONCE_REQUIRED", code)
	}
	if got := productionLifecycleRunStatus(t, ledger, oldRun); got != "running" {
		t.Fatalf("a refused restart moved the run to %q; want running", got)
	}

	restarted := productionStoryStateWriteCall(t, lane.author, "generation:restart", []any{
		handle, productionEditorBeginRequest(productionGenerationWriteRestartNonce), lane.frozen,
	})
	next, ok := restarted["handle"].(map[string]any)
	if !ok || next["runId"] == "" || next["rootActionId"] == "" {
		t.Fatalf("generation:restart returned no replacement handle: %v", restarted)
	}
	newRun, _ := next["runId"].(string)
	newRoot, _ := next["rootActionId"].(string)
	if newRun == oldRun || newRoot == oldRoot {
		t.Fatalf("generation:restart reused the old identity: %v -> %v", handle, next)
	}
	// Write-then-readback: the replacement run is open under its own active root,
	// and the old root was settled by the ordinary stop.
	if got := productionLifecycleRunStatus(t, ledger, newRun); got != "running" {
		t.Fatalf("the replacement run is %q; want running", got)
	}
	if got := productionLifecycleRootStatus(t, ledger, newRoot); got != "active" {
		t.Fatalf("the replacement root is %q; want active", got)
	}
	if got := productionLifecycleRunStatus(t, ledger, oldRun); got != "cancelled" {
		t.Fatalf("the replaced run is %q; want cancelled", got)
	}
	if got := productionLifecycleRootStatus(t, ledger, oldRoot); got != "cancelled" {
		t.Fatalf("the replaced root is %q; want cancelled", got)
	}
	// Restart settles no attempt usage and reserves none for the replacement: the
	// replacement can reach a provider only through generation:execute.
	if now := productionGenerationWriteAttempts(t, ledger, oldRoot); now != oldUsage {
		t.Fatalf("the restart rewrote the replaced root's attempts: %s -> %s", oldUsage, now)
	}
	if reserved := productionGenerationWriteAttempts(t, ledger, newRoot); reserved != "null" && reserved != "[]" {
		t.Fatalf("the replacement run reserved attempts before any dispatch: %s", reserved)
	}
	baseline := lane.bytes(t)
	if code := productionLifecycleRefusal(t, "stale-lease generation:restart",
		lane.author.call(t, "generation:restart",
			[]any{handle, productionEditorBeginRequest("generation-write-restart-2"), lane.stale()})); code == "" {
		t.Fatal("the stale-lease restart was refused without a symbolic code")
	}
	if got := productionLifecycleRunStatus(t, ledger, newRun); got != "running" {
		t.Fatalf("a refused restart moved the replacement run to %q", got)
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused restart changed the committed database file")
	}
	lane.assertNoDial(t)
}

// testProductionGenerationWriteFamilyCancel proves the two channel stops are the
// same stop as generation:cancel behind the `{handle}` envelope: a bare handle and
// any other key are the channel's own refusal code, and the wrapped handle
// transitions the run and its root.
func testProductionGenerationWriteFamilyCancel(t *testing.T, operation, invalidCode string) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	ledger := productionLifecycleLedger(t, lane.projectPath)
	handle := lane.begin(t, productionEditorBeginRequest(productionGenerationWriteBeginNonce))
	runID, _ := handle["runId"].(string)
	rootID, _ := handle["rootActionId"].(string)

	// The envelope is load-bearing: the channel sends `{ handle }`, and the owner
	// refuses any request that is not exactly that object.
	if code := productionLifecycleRefusal(t, operation+" bare handle",
		lane.author.call(t, operation, []any{handle, lane.frozen})); code != invalidCode {
		t.Fatalf("%s accepted a bare handle, refusing with %q instead of %q", operation, code, invalidCode)
	}
	if code := productionLifecycleRefusal(t, operation+" extra key",
		lane.author.call(t, operation, []any{map[string]any{"handle": handle, "cancelledAt": float64(1)}, lane.frozen})); code != invalidCode {
		t.Fatalf("%s accepted an extra key, refusing with %q instead of %q", operation, code, invalidCode)
	}
	if got := productionLifecycleRunStatus(t, ledger, runID); got != "running" {
		t.Fatalf("a refused %s moved the run to %q; want running", operation, got)
	}

	cancelled := productionStoryStateWriteCall(t, lane.author, operation,
		[]any{map[string]any{"handle": handle}, lane.frozen})
	if cancelled["status"] != "cancelled" {
		t.Fatalf("%s answered %v; want cancelled", operation, cancelled["status"])
	}
	answered, ok := cancelled["handle"].(map[string]any)
	if !ok || answered["runId"] != runID || answered["rootActionId"] != rootID {
		t.Fatalf("%s answered for another run: %v", operation, cancelled["handle"])
	}
	if got := productionLifecycleRunStatus(t, ledger, runID); got != "cancelled" {
		t.Fatalf("%s left the ledger run row at %q; want cancelled", operation, got)
	}
	if got := productionLifecycleRootStatus(t, ledger, rootID); got != "cancelled" {
		t.Fatalf("%s left the ledger root row at %q; want cancelled", operation, got)
	}

	baseline := lane.bytes(t)
	if code := productionLifecycleRefusal(t, "stale-lease "+operation,
		lane.author.call(t, operation, []any{map[string]any{"handle": handle}, lane.stale()})); code == "" {
		t.Fatalf("the stale-lease %s was refused without a symbolic code", operation)
	}
	if got := productionLifecycleRootStatus(t, ledger, rootID); got != "cancelled" {
		t.Fatalf("a refused %s moved the root row to %q", operation, got)
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatalf("a refused %s changed the committed database file", operation)
	}
	lane.assertNoDial(t)
}

// testProductionGenerationWriteRefusesShortOutlineRetry states the refusal this
// file derives: the family is bound and answers, and the one name that would make
// this lane a prompt author stays fail-closed.
func testProductionGenerationWriteRefusesShortOutlineRetry(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	handle := lane.begin(t, productionEditorBeginRequest(productionGenerationWriteBeginNonce))
	response := lane.author.call(t, "generation:retry-draft-short-outline", []any{
		map[string]any{"handle": handle, "failedAttemptId": "ci-not-an-attempt"}, lane.frozen,
	})
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("generation:retry-draft-short-outline is bound in the production lane: %d %s",
			response.Code, response.Body.String())
	}
	lane.assertNoDial(t)
}

// TestProductionFinalizationWritesServeTheOutboxThisLaneOwns drives the three
// names BindFinalization installs over the author's own admitted database.
func TestProductionFinalizationWritesServeTheOutboxThisLaneOwns(t *testing.T) {
	t.Run("finalization:commit", testProductionFinalizationWriteCommit)
	t.Run("finalization:retry", testProductionFinalizationWriteRetry)
	t.Run("db:finalization-link-knowledge-document", testProductionFinalizationWriteKnowledgeLink)
}

// productionFinalizationWriteSnapshot is the frozen commit request: the draft, the
// prose the author confirmed, the baseline it replaced and the snapshot's own copy
// of the session, which must equal the wire context.
func productionFinalizationWriteSnapshot(lane *productionGenerationWriteLane, content, baseline string) map[string]any {
	return map[string]any{
		"tabId":           productionTestTab,
		"projectPath":     lane.projectPath,
		"projectSession":  lane.frozen,
		"draftId":         float64(1),
		"chapterNumber":   float64(1),
		"chapterTitle":    productionGenerationWriteFinalTitle,
		"content":         content,
		"expectedContent": baseline,
		"contentRevision": float64(1),
	}
}

// testProductionFinalizationWriteCommit proves the commit freezes the author's
// prose and the outbox row in one write and reports the unpublished projection
// this lane can honestly produce, and that a refused commit writes nothing.
func testProductionFinalizationWriteCommit(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	result := productionStoryStateWriteCall(t, lane.author, "finalization:commit",
		[]any{productionFinalizationWriteSnapshot(lane, productionGenerationWriteFinalBody, productionEditorSeed), lane.frozen})
	digest := productionGenerationWriteHash(productionGenerationWriteFinalBody)
	if result["success"] != false || result["committed"] != true || result["publicationStatus"] != "pending" ||
		result["contentHash"] != digest || result["draftId"] != float64(1) ||
		result["error"] != "Error: FINALIZATION_PUBLISHER_UNAVAILABLE" {
		t.Fatalf("finalization:commit did not freeze an unpublished projection: %v", result)
	}
	if id, _ := result["finalizationId"].(string); id == "" {
		t.Fatalf("finalization:commit minted no outbox identity: %v", result)
	}
	// Write-then-readback through a connection of the test's own: the prose, the
	// draft status and the outbox row, none of them from the receipt.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var draftStatus, storedBody, storedID, storedSnapshot, storedHash, publication, lastError, published string
	productionStoryStateWriteScan(t, readback, `SELECT status FROM drafts WHERE id=1`, nil, &draftStatus)
	productionStoryStateWriteScan(t, readback,
		`SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=1`, nil, &storedBody)
	productionStoryStateWriteScan(t, readback,
		`SELECT finalization_id FROM finalization_outbox WHERE draft_id=1`, nil, &storedID)
	productionStoryStateWriteScan(t, readback,
		`SELECT content_snapshot FROM finalization_outbox WHERE draft_id=1`, nil, &storedSnapshot)
	productionStoryStateWriteScan(t, readback,
		`SELECT content_hash FROM finalization_outbox WHERE draft_id=1`, nil, &storedHash)
	productionStoryStateWriteScan(t, readback,
		`SELECT publication_status FROM finalization_outbox WHERE draft_id=1`, nil, &publication)
	productionStoryStateWriteScan(t, readback,
		`SELECT COALESCE(last_error,'') FROM finalization_outbox WHERE draft_id=1`, nil, &lastError)
	productionStoryStateWriteScan(t, readback,
		`SELECT COALESCE(published_at,'') FROM finalization_outbox WHERE draft_id=1`, nil, &published)
	if draftStatus != "finalized" || storedBody != productionGenerationWriteFinalBody {
		t.Fatalf("the commit left the draft at %q holding %q", draftStatus, storedBody)
	}
	if storedID != result["finalizationId"] || storedSnapshot != productionGenerationWriteFinalBody || storedHash != digest {
		t.Fatalf("the outbox row froze different facts: id=%q hash=%q snapshot=%q", storedID, storedHash, storedSnapshot)
	}
	if publication != "pending" || lastError != "Error: FINALIZATION_PUBLISHER_UNAVAILABLE" || published != "" {
		t.Fatalf("the commit claimed a publication state: %q %q %q", publication, lastError, published)
	}
	// A different frozen request for the same draft is the service's own conflict:
	// bare code, no prefix, and the frozen prose stays.
	baseline := lane.bytes(t)
	conflict := productionStoryStateWriteCall(t, lane.author, "finalization:commit",
		[]any{productionFinalizationWriteSnapshot(lane, "另一份正文", productionEditorSeed), lane.frozen})
	if conflict["success"] != false || conflict["committed"] != false || conflict["error"] != "FINALIZATION_COMMIT_CONFLICT" {
		t.Fatalf("a conflicting commit was not refused with the bare conflict code: %v", conflict)
	}
	productionStoryStateWriteScan(t, readback,
		`SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=1`, nil, &storedBody)
	if storedBody != productionGenerationWriteFinalBody {
		t.Fatalf("a refused commit changed the frozen prose: %q", storedBody)
	}
	// The stale lease is refused before the borrow, so nothing is written.
	if code := productionLifecycleRefusal(t, "stale-lease finalization:commit",
		lane.author.call(t, "finalization:commit",
			[]any{productionFinalizationWriteSnapshot(lane, productionGenerationWriteFinalBody, productionEditorSeed), lane.stale()})); code == "" {
		t.Fatal("the stale-lease commit was refused without a symbolic code")
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused commit changed the committed database file")
	}
	lane.assertNoDial(t)
}

// testProductionFinalizationWriteRetry proves a retry replays the frozen outbox row
// and re-records the pending projection, never a prose the request carries, and
// that its own refusals write nothing.
func testProductionFinalizationWriteRetry(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionGenerationWriteSeedOutbox)
	result := productionStoryStateWriteCall(t, lane.author, "finalization:retry",
		[]any{productionGenerationWriteFinalizationID, lane.frozen})
	digest := productionGenerationWriteHash(productionEditorSeed)
	if result["success"] != false || result["committed"] != true || result["publicationStatus"] != "pending" ||
		result["finalizationId"] != productionGenerationWriteFinalizationID || result["contentHash"] != digest ||
		result["draftId"] != float64(1) || result["error"] != "Error: FINALIZATION_PUBLISHER_UNAVAILABLE" {
		t.Fatalf("finalization:retry did not replay the frozen projection: %v", result)
	}
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var publication, lastError, storedSnapshot, published string
	productionStoryStateWriteScan(t, readback,
		`SELECT publication_status FROM finalization_outbox WHERE finalization_id=?`,
		[]any{productionGenerationWriteFinalizationID}, &publication)
	productionStoryStateWriteScan(t, readback,
		`SELECT COALESCE(last_error,'') FROM finalization_outbox WHERE finalization_id=?`,
		[]any{productionGenerationWriteFinalizationID}, &lastError)
	productionStoryStateWriteScan(t, readback,
		`SELECT content_snapshot FROM finalization_outbox WHERE finalization_id=?`,
		[]any{productionGenerationWriteFinalizationID}, &storedSnapshot)
	productionStoryStateWriteScan(t, readback,
		`SELECT COALESCE(published_at,'') FROM finalization_outbox WHERE finalization_id=?`,
		[]any{productionGenerationWriteFinalizationID}, &published)
	if publication != "pending" || lastError != "Error: FINALIZATION_PUBLISHER_UNAVAILABLE" || published != "" {
		t.Fatalf("the retry claimed a publication state: %q %q %q", publication, lastError, published)
	}
	if storedSnapshot != productionEditorSeed {
		t.Fatalf("the retry rewrote the frozen snapshot: %q", storedSnapshot)
	}
	// The two refusals this name owns, both before any write.
	baseline := lane.bytes(t)
	missing := productionStoryStateWriteCall(t, lane.author, "finalization:retry", []any{"", lane.frozen})
	if missing["success"] != false || missing["committed"] != false || missing["error"] != "Error: FINALIZATION_COMMIT_ID_REQUIRED" {
		t.Fatalf("finalization:retry accepted an empty id: %v", missing)
	}
	unknown := productionStoryStateWriteCall(t, lane.author, "finalization:retry", []any{"ci-missing-finalization", lane.frozen})
	if unknown["success"] != false || unknown["committed"] != false || unknown["error"] != "FINALIZATION_COMMIT_NOT_FOUND" {
		t.Fatalf("finalization:retry accepted an unknown id: %v", unknown)
	}
	if code := productionLifecycleRefusal(t, "stale-lease finalization:retry",
		lane.author.call(t, "finalization:retry", []any{productionGenerationWriteFinalizationID, lane.stale()})); code == "" {
		t.Fatal("the stale-lease retry was refused without a symbolic code")
	}
	productionStoryStateWriteScan(t, readback,
		`SELECT COALESCE(last_error,'') FROM finalization_outbox WHERE finalization_id=?`,
		[]any{productionGenerationWriteFinalizationID}, &lastError)
	if lastError != "Error: FINALIZATION_PUBLISHER_UNAVAILABLE" {
		t.Fatalf("a refused retry rewrote the projection: %q", lastError)
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused retry changed the committed database file")
	}
	lane.assertNoDial(t)
}

// testProductionFinalizationWriteKnowledgeLink proves the link records the trimmed
// managed document identity on the frozen row and mints nothing else.
func testProductionFinalizationWriteKnowledgeLink(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, productionGenerationWriteSeedOutbox)
	result := productionStoryStateWriteCall(t, lane.author, "db:finalization-link-knowledge-document",
		[]any{float64(1), "  " + productionGenerationWriteDocumentID + "  ", lane.projectPath, lane.frozen})
	if result["success"] != true || result["error"] != nil {
		t.Fatalf("db:finalization-link-knowledge-document refused a managed document: %v", result)
	}
	record := syntheticMap(result["finalization"])
	if record == nil || record["knowledgeDocumentId"] != productionGenerationWriteDocumentID ||
		record["finalizationId"] != productionGenerationWriteFinalizationID ||
		record["contentSnapshot"] != productionEditorSeed || record["publicationStatus"] != "pending" {
		t.Fatalf("the link returned a malformed record: %v", result["finalization"])
	}
	// Write-then-readback: the trimmed identity is on the frozen row.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var storedDocument, storedSnapshot string
	productionStoryStateWriteScan(t, readback,
		`SELECT COALESCE(knowledge_document_id,'') FROM finalization_outbox WHERE finalization_id=?`,
		[]any{productionGenerationWriteFinalizationID}, &storedDocument)
	productionStoryStateWriteScan(t, readback,
		`SELECT content_snapshot FROM finalization_outbox WHERE finalization_id=?`,
		[]any{productionGenerationWriteFinalizationID}, &storedSnapshot)
	if storedDocument != productionGenerationWriteDocumentID || storedSnapshot != productionEditorSeed {
		t.Fatalf("the linked row holds %q over %q", storedDocument, storedSnapshot)
	}
	// A blank identity and an unknown draft are this name's own refusals.
	baseline := lane.bytes(t)
	blank := productionStoryStateWriteCall(t, lane.author, "db:finalization-link-knowledge-document",
		[]any{float64(1), "   ", lane.projectPath, lane.frozen})
	if blank["success"] != false || blank["error"] != "Error: FINALIZATION_COMMIT_KNOWLEDGE_DOCUMENT_REQUIRED" {
		t.Fatalf("the link accepted a blank document: %v", blank)
	}
	unknown := productionStoryStateWriteCall(t, lane.author, "db:finalization-link-knowledge-document",
		[]any{float64(999999), productionGenerationWriteDocumentID, lane.projectPath, lane.frozen})
	if unknown["success"] != false || unknown["error"] != "Error: FINALIZATION_COMMIT_NOT_FOUND" {
		t.Fatalf("the link accepted an unknown draft: %v", unknown)
	}
	// The stale lease is refused before the store is reached.
	if code := productionLifecycleRefusal(t, "stale-lease db:finalization-link-knowledge-document",
		lane.author.call(t, "db:finalization-link-knowledge-document",
			[]any{float64(1), "kb:伪造", lane.projectPath, lane.stale()})); code == "" {
		t.Fatal("the stale-lease link was refused without a symbolic code")
	}
	productionStoryStateWriteScan(t, readback,
		`SELECT COALESCE(knowledge_document_id,'') FROM finalization_outbox WHERE finalization_id=?`,
		[]any{productionGenerationWriteFinalizationID}, &storedDocument)
	if storedDocument != productionGenerationWriteDocumentID {
		t.Fatalf("a refused link rewrote the document identity: %q", storedDocument)
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused link changed the committed database file")
	}
	lane.assertNoDial(t)
}
