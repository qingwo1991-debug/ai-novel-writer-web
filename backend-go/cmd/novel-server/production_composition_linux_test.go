//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/composition"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the two composition names: one subtest per operation over the
// real HTTP handler, plus the pure algorithm and one subtest per refusal, because
// every refusal in the reference is one symbolic code and a code nobody asserts is
// a code nobody notices changing.
//
// The end-to-end half drives generation:begin, generation:execute and
// generation:compose-visible with the in-process dispatch seam, so the artifact it
// composes was written by this lane's own ledger and the run is frozen under the
// live lease. It then re-reads the receipt through a separate mode=ro connection -
// never the lane's admitted handle - and asserts the served text hashes to the
// served hash, which is the one parity claim the desktop and this lane share.
const (
	productionCompositionBeginNonce   = "production-composition-begin-1"
	productionCompositionExecuteNonce = "production-composition-execute-1"
	// productionCompositionText is the dispatched candidate: two lines with a
	// trailing newline, so the trim the reference applies is observable in the
	// served text instead of assumed.
	productionCompositionText = "草稿候选：门后有人。\n第二行。\n"
	// productionCompositionTamperNonce freezes the fixture run whose stored receipt
	// is rewritten before the lane opens.
	productionCompositionTamperNonce        = "production-composition-tamper-1"
	productionCompositionTamperReserveNonce = "production-composition-tamper-reserve-1"
	productionCompositionTamperText         = "篡改候选：门后无人。"
)

// productionCompositionDigest is textHash (generation-run-repository.ts:92) as this
// test computes it, independently of the package under test.
func productionCompositionDigest(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// productionCompositionPayload reads one committed artifact's own revision and
// payload hash over the test's own mode=ro connection, so a served source is
// checked against the ledger instead of against the value under test.
func productionCompositionPayload(t *testing.T, db *sql.DB, artifactID string) (int64, string) {
	t.Helper()
	var revision int64
	var hash string
	productionStoryStateWriteScan(t, db,
		`SELECT revision, json_extract(artifact_json,'$.textHash') FROM generation_artifacts WHERE artifact_id=?`,
		[]any{artifactID}, &revision, &hash)
	if hash == "" {
		t.Fatalf("the committed artifact %s records no textHash", artifactID)
	}
	return revision, hash
}

// productionCompositionAnswer decodes one answer of the family's read envelope. A
// null result is reported as absent, which is exactly what
// generation:read-visible-composition answers for a run that stores no receipt.
func productionCompositionAnswer(t *testing.T, client *productionDurableClient, operation string, args []any) (map[string]any, bool) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s is still GO_OPERATION_NOT_MIGRATED in the production lane: %s", operation, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if len(wire.Result) == 0 || string(wire.Result) == "null" {
		return nil, false
	}
	var receipt map[string]any
	if err := json.Unmarshal(wire.Result, &receipt); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	return receipt, true
}

// productionCompositionRefusal asserts one symbolic refusal of the family: the
// envelope's error with an all-caps code, never a 200 and never the fail-closed 501.
func productionCompositionRefusal(t *testing.T, label string, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s: the composition binder is not installed: %d %s", label, response.Code, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%s answered 200 instead of a refusal: %s", label, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	if body.Error.Code == "" {
		t.Fatalf("%s: a refusal surfaced no code: %s", label, response.Body.String())
	}
	return body.Error.Code
}

// TestProductionCompositionComposesAndReadsBack is the served journey: compose a
// committed candidate, answer its receipt, store it on the attempt the reference
// stores it on, and re-read it after re-composing it from the ledger instead of
// trusting the stored bytes.
func TestProductionCompositionComposesAndReadsBack(t *testing.T) {
	seam := &productionGenerationSeam{text: productionCompositionText}
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)
	handle := lane.begin(t, productionEditorBeginRequest(productionCompositionBeginNonce))

	// A run with no receipt answers null, not an empty receipt.
	if receipt, ok := productionCompositionAnswer(t, lane.author, "generation:read-visible-composition", []any{handle, lane.frozen}); ok {
		t.Fatalf("a run that stores no composition answered %v", receipt)
	}

	executed := productionExecuteAnswer(t, lane.author, productionExecuteRequest(handle, productionCompositionExecuteNonce), lane.frozen)
	candidate := productionGenerationWriteCandidate(t, executed)
	artifactID, _ := candidate["artifactId"].(string)
	if artifactID == "" {
		t.Fatal("the executed run reports no artifact to compose")
	}
	// The reference composes a single artifact as text.trim().
	expected := strings.TrimSpace(productionCompositionText)
	expectedHash := productionCompositionDigest(expected)

	// compose-visible with three wire arguments: no algorithm, so the version is the
	// default one.
	receipt, ok := productionCompositionAnswer(t, lane.author, "generation:compose-visible",
		[]any{handle, []any{artifactID}, expectedHash, lane.frozen})
	if !ok {
		t.Fatal("generation:compose-visible answered null")
	}
	if receipt["algorithm"] != composition.VisibleContinuationVersion {
		t.Fatalf("compose answered algorithm %v", receipt["algorithm"])
	}
	text, isText := receipt["text"].(string)
	if !isText || text != expected {
		t.Fatalf("compose text %v; want the trimmed candidate %q", receipt["text"], expected)
	}
	if receipt["textHash"] != expectedHash || receipt["textHash"] != productionCompositionDigest(text) {
		t.Fatalf("compose answered hash %v; want %s", receipt["textHash"], expectedHash)
	}
	artifactIDs, isList := receipt["artifactIds"].([]any)
	if !isList || len(artifactIDs) != 1 || artifactIDs[0] != artifactID {
		t.Fatalf("compose answered artifactIds %v", receipt["artifactIds"])
	}
	sources, isList := receipt["sources"].([]any)
	if !isList || len(sources) != 1 {
		t.Fatalf("compose answered sources %v", receipt["sources"])
	}
	// The source names the payload, not the composition: its revision and its hash
	// are the artifact's own (generation-run-repository.ts:511) - the hash of the
	// raw snapshot text, trailing newline included - while the receipt hashes the
	// composed, trimmed text this call produced. Reading both from a connection of
	// the test's own is what makes that split observable instead of assumed.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	payloadRevision, payloadHash := productionCompositionPayload(t, readback, artifactID)
	if payloadHash != productionCompositionDigest(productionCompositionText) {
		t.Fatalf("the committed payload hashes %q; want the raw candidate text", payloadHash)
	}
	if payloadHash == expectedHash {
		t.Fatal("the payload and the composition hash the same text, so this fixture no longer pins the trim")
	}
	source, isMap := sources[0].(map[string]any)
	if !isMap {
		t.Fatalf("compose answered source %v", sources[0])
	}
	if source["artifactId"] != artifactID {
		t.Fatalf("compose answered source artifactId %v; want %s", source["artifactId"], artifactID)
	}
	if source["revision"] != float64(payloadRevision) {
		t.Fatalf("compose answered source revision %v; want the payload revision %d", source["revision"], payloadRevision)
	}
	if source["textHash"] != payloadHash {
		t.Fatalf("compose answered source textHash %v; want the payload hash %s", source["textHash"], payloadHash)
	}

	// The receipt was written on the attempt that owns the last composed artifact,
	// as generation-run-repository.ts:691-693 does, and it was written as one added
	// key rather than a replacement of the attempt's usage.
	var storedHash, storedIDs, usageKeys string
	productionStoryStateWriteScan(t, readback,
		`SELECT json_extract(usage_receipt_json,'$.visibleComposition.textHash'),
 json_extract(usage_receipt_json,'$.visibleComposition.artifactIds'),
 (SELECT group_concat(key) FROM json_each(usage_receipt_json))`,
		nil, &storedHash, &storedIDs, &usageKeys)
	if storedHash != expectedHash {
		t.Fatalf("the stored receipt hashes %q; want %s", storedHash, expectedHash)
	}
	if !strings.Contains(storedIDs, artifactID) {
		t.Fatalf("the stored receipt names %q; want %s", storedIDs, artifactID)
	}
	// The keys the attempt already carried survive the write, and visibleComposition
	// is appended last, exactly like the reference's spread-then-add.
	for _, key := range []string{"requestHash", "usagePolicy", "artifactIdentity", "visibleComposition"} {
		if !strings.Contains(usageKeys, key) {
			t.Fatalf("the stored usage lost %q: %s", key, usageKeys)
		}
	}
	if !strings.HasSuffix(usageKeys, "visibleComposition") {
		t.Fatalf("visibleComposition is not the last stored key: %s", usageKeys)
	}

	// read-visible-composition re-composes the stored receipt from the ledger and
	// answers it with the run's frozen author inputs.
	stored, ok := productionCompositionAnswer(t, lane.author, "generation:read-visible-composition", []any{handle, lane.frozen})
	if !ok {
		t.Fatal("generation:read-visible-composition answered null for a run that stores a receipt")
	}
	if stored["text"] != receipt["text"] || stored["textHash"] != receipt["textHash"] {
		t.Fatalf("the re-read receipt %v does not reproduce the composed one %v", stored, receipt)
	}
	if _, present := stored["authorInputs"]; !present {
		t.Fatalf("the re-read receipt dropped the frozen author inputs: %v", stored)
	}
	reReadText, isText := stored["text"].(string)
	if !isText || stored["textHash"] != productionCompositionDigest(reReadText) {
		t.Fatal("the re-read text does not hash to the re-read hash")
	}

	// A wrong expected hash is refused with the algorithm named explicitly, which is
	// the family's four-argument wire.
	baseline := lane.bytes(t)
	if code := productionCompositionRefusal(t, "hash mismatch", lane.author.call(t, "generation:compose-visible",
		[]any{handle, []any{artifactID}, strings.Repeat("0", 64), composition.VisibleContinuationVersion, lane.frozen})); code != composition.CodeHashMismatch {
		t.Fatalf("a wrong expected hash was refused with %q; want %s", code, composition.CodeHashMismatch)
	}
	// An empty selection is refused before anything is composed.
	if code := productionCompositionRefusal(t, "empty selection", lane.author.call(t, "generation:compose-visible",
		[]any{handle, []any{}, expectedHash, lane.frozen})); code != composition.CodeInvalid {
		t.Fatalf("an empty selection was refused with %q; want %s", code, composition.CodeInvalid)
	}
	// An algorithm nobody implements is refused by the same membership check.
	if code := productionCompositionRefusal(t, "algorithm", lane.author.call(t, "generation:compose-visible",
		[]any{handle, []any{artifactID}, expectedHash, "visible-append-v9", lane.frozen})); code != composition.CodeAlgorithmInvalid {
		t.Fatalf("an unknown algorithm was refused with %q; want %s", code, composition.CodeAlgorithmInvalid)
	}
	// A wrong wire arity never reaches the handler.
	if code := productionCompositionRefusal(t, "arity", lane.author.call(t, "generation:compose-visible",
		[]any{handle, lane.frozen})); code != "INVALID_ARGUMENTS" {
		t.Fatalf("a malformed wire was refused with %q; want INVALID_ARGUMENTS", code)
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused compose changed the committed database file")
	}

	// A stale lease is refused by the reused admission, before a row is read.
	if code := productionCompositionRefusal(t, "stale lease compose", lane.author.call(t, "generation:compose-visible",
		[]any{handle, []any{artifactID}, expectedHash, lane.stale()})); code == "" {
		t.Fatal("a stale-lease compose was refused without a symbolic code")
	}
	if code := productionCompositionRefusal(t, "stale lease read", lane.author.call(t, "generation:read-visible-composition",
		[]any{handle, lane.stale()})); code == "" {
		t.Fatal("a stale-lease read was refused without a symbolic code")
	}
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused read changed the committed database file")
	}
	lane.assertNoDial(t)
}

// TestProductionCompositionRefusesATamperedReceipt seeds one settled visible-text
// run whose stored receipt still names the right artifact and sources but the wrong
// hash, then proves the read re-composes it, finds the disagreement and refuses with
// the integrity code, that the compose over the same run is stopped one gate earlier
// by the lease, and that neither leaves a byte behind. The seeded run is frozen under
// the fixture's epoch, which no live lease can equal, so the compose is a lease
// refusal here and the compose-side integrity branch is covered by the pure table.
func TestProductionCompositionRefusesATamperedReceipt(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	seed := productionCompositionTamperedRun(t, database, productionCompositionTamperText)
	seeded, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	author, frozen := productionExecuteSession(t, handler, projectPath, "production-composition-tamper-open-1")
	handle := map[string]any{
		"projectId":    productionEditorProjectID,
		"epoch":        seed.Epoch,
		"rootActionId": seed.RootActionID,
		"runId":        seed.RunID,
	}

	if code := productionCompositionRefusal(t, "tampered receipt", author.call(t, "generation:read-visible-composition",
		[]any{handle, frozen})); code != composition.CodeIntegrityFailed {
		t.Fatalf("a stored receipt that no longer re-hashes was refused with %q; want %s", code, composition.CodeIntegrityFailed)
	}
	if after, readErr := os.ReadFile(database); readErr != nil {
		t.Fatal(readErr)
	} else if !bytes.Equal(after, seeded) {
		t.Fatal("a refused read of a tampered receipt changed the committed database file")
	}

	// A compose over the same run never reaches that receipt: the run was frozen
	// under the fixture's own epoch, and a compose re-admits the handle against the
	// live lease first (requireRun(handle, true), main-generation-owner.ts:164), so
	// the lease gate answers before the integrity check can. That is why the read
	// above is the operation that proves the tampered receipt is rejected, and why
	// the pure table below covers the compose-side integrity branch directly.
	if code := productionCompositionRefusal(t, "tampered compose", author.call(t, "generation:compose-visible",
		[]any{handle, []any{seed.ArtifactID}, seed.TextHash, frozen})); code != composition.CodeEpochStale {
		t.Fatalf("a compose over a run frozen under another epoch was refused with %q; want %s", code, composition.CodeEpochStale)
	}
	if after, readErr := os.ReadFile(database); readErr != nil {
		t.Fatal(readErr)
	} else if !bytes.Equal(after, seeded) {
		t.Fatal("a refused compose of a tampered receipt changed the committed database file")
	}
}

// productionCompositionTamperedRun is one settled visible-text run whose stored
// receipt names the ledger's own artifact and source hash, but a hash for text
// nobody can compose, so the recomposition's comparison is the only refusal that can
// fire.
type productionCompositionTamperedRow struct {
	RootActionID string
	RunID        string
	ArtifactID   string
	Epoch        string
	TextHash     string
}

func productionCompositionTamperedRun(t *testing.T, database, text string) productionCompositionTamperedRow {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return fmt.Sprintf("composition-ledger-%02d", ordinal)
	})
	ctx := context.Background()
	request := strings.Replace(productionEditorGenerationRequest(),
		`"uiActionNonce":"`+productionEditorGenerationNonce+`"`,
		`"uiActionNonce":"`+productionCompositionTamperNonce+`"`, 1)
	request = strings.Replace(request, `"sourceManifest":{"operation":"chapter-draft"}`,
		`"sourceManifest":{"operation":"chapter-draft","output":"visible-text"}`, 1)
	value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+request+"]"))
	if err != nil {
		t.Fatalf("tampered-run open: %v", err)
	}
	opened := syntheticMap(value)
	if opened == nil {
		t.Fatalf("tampered-run open returned %T", value)
	}
	runID := syntheticString(opened["runId"])
	rootID := syntheticString(opened["rootActionId"])
	if runID == "" || rootID == "" {
		t.Fatalf("tampered-run open returned no identity: %v", opened)
	}
	reserved, err := ledger.Call(ctx, "reserve", []any{runID, productionCompositionTamperReserveNonce, strings.Repeat("b", 64), 200, 100})
	if err != nil {
		t.Fatalf("tampered-run reserve: %v", err)
	}
	attemptID := syntheticString(syntheticMap(syntheticMap(reserved)["attempt"])["attemptId"])
	if attemptID == "" {
		t.Fatalf("tampered-run reserve returned no attempt: %v", reserved)
	}
	if _, err := ledger.Call(ctx, "markDispatched", []any{attemptID}); err != nil {
		t.Fatalf("tampered-run markDispatched: %v", err)
	}
	if _, err := ledger.Call(ctx, "snapshot", []any{attemptID, 0, text}); err != nil {
		t.Fatalf("tampered-run snapshot: %v", err)
	}
	if _, err := ledger.Call(ctx, "settle", []any{attemptID, map[string]any{"trusted": true, "actualTokens": 120}, "stop"}); err != nil {
		t.Fatalf("tampered-run settle: %v", err)
	}
	var artifact, artifactText, artifactHash string
	var artifactRevision int64
	if err := db.QueryRow(`SELECT artifact_id,json_extract(artifact_json,'$.text'),json_extract(artifact_json,'$.textHash'),revision
 FROM generation_artifacts WHERE attempt_id=?`, attemptID).Scan(&artifact, &artifactText, &artifactHash, &artifactRevision); err != nil {
		t.Fatalf("tampered-run artifact: %v", err)
	}
	if artifactHash != productionCompositionDigest(artifactText) {
		t.Fatalf("the ledger's own artifact does not hash to its text: %q vs %q", artifactHash, artifactText)
	}
	// Only the composed hash lies: the ids, the source and its revision are the ones
	// the ledger holds, so the recomposition's comparison is the single refusal that
	// can fire (generation-run-repository.ts:672).
	tampered := fmt.Sprintf(`{"algorithm":%q,"textHash":%q,"artifactIds":[%q],"sources":[{"artifactId":%q,"revision":%d,"textHash":%q}]}`,
		composition.VisibleContinuationVersion, strings.Repeat("0", 64), artifact, artifact, artifactRevision, artifactHash)
	if _, err := db.Exec(`UPDATE generation_attempts SET usage_receipt_json=json_set(usage_receipt_json,'$.visibleComposition',json(?)) WHERE attempt_id=?`,
		tampered, attemptID); err != nil {
		t.Fatalf("tampered-run receipt: %v", err)
	}
	productionEditorCheckpoint(t, db, database)
	seeded := productionCompositionTamperedRow{}
	seeded.RootActionID = rootID
	seeded.RunID = runID
	seeded.ArtifactID = artifact
	seeded.Epoch = productionEditorGenerationEpoch
	seeded.TextHash = artifactHash
	return seeded
}

// TestProductionCompositionAlgorithm is the pure half: the append algorithm, one
// subtest per refusal, and the null answer a run without a receipt gets.
func TestProductionCompositionAlgorithm(t *testing.T) {
	t.Run("append drops a repeated tail of at least the floor", func(t *testing.T) {
		// MIN_VISIBLE_OVERLAP_CHARS is the floor: a shorter repeat is appended whole,
		// which is why the two cases below use a tail above it.
		tail := strings.Repeat("甲", composition.MinVisibleOverlapChars+12)
		if got := composition.ComposeVisible(tail, tail+"乙"); got != tail+"\n\n乙" {
			t.Fatalf("append composed %q; want the tail stripped once", got)
		}
		// A short repeat is below the floor and stays whole.
		if got := composition.ComposeVisible("他推开门。", "他推开门。风很大。"); got != "他推开门。\n\n他推开门。风很大。" {
			t.Fatalf("a sub-floor repeat was stripped: %q", got)
		}
		if got := composition.ComposeVisible("他推开门。", "风很大。"); got != "他推开门。\n\n风很大。" {
			t.Fatalf("append composed %q", got)
		}
	})
	t.Run("an overlap below the floor is not a repeat", func(t *testing.T) {
		short := strings.Repeat("a", composition.MinVisibleOverlapChars-1)
		if got := composition.ComposeVisible("门"+short, short); got != "门"+short+"\n\n"+short {
			t.Fatalf("a sub-floor overlap was treated as a repeated tail: %q", got)
		}
	})
	t.Run("the hash is sha256 of the utf8 text", func(t *testing.T) {
		if got := composition.TextHash("草稿"); got != productionCompositionDigest("草稿") {
			t.Fatalf("hash %q", got)
		}
	})
	t.Run("a run without a receipt answers null", func(t *testing.T) {
		if receipt, err := composition.Read(composition.Request{}); err != nil || receipt != nil {
			t.Fatalf("read without a stored receipt answered %v, %v", receipt, err)
		}
	})
	t.Run("refusals", func(t *testing.T) {
		text := "草稿候选：门后有人。"
		added := text + "风很大。"
		// One assignment per field: a composite literal here would be one gofmt
		// alignment section, and this file is written by hand.
		var source composition.Artifact
		source.Present = true
		source.ArtifactID = "a1"
		source.AttemptID = "t1"
		source.RunID = "r1"
		source.Status = "partial"
		source.Revision = 0
		source.Ordinal = 1
		source.AttemptStatus = "settled"
		source.FinishReason = "stop"
		source.HasIdentity = true
		source.IdentityArtifactID = "a1"
		source.IdentityEpoch = "e1"
		source.IdentityFingerprint = `{"x":1}`
		source.BindingProjectID = "p1"
		source.BindingEpoch = "e1"
		source.BindingFingerprint = `{"x":1}`
		source.PayloadArtifactID = "a1"
		source.PayloadAttemptID = "t1"
		source.PayloadRevision = 0
		source.ProjectID = "p1"
		source.Epoch = "e1"
		source.RootActionID = "root1"
		source.Fingerprint = `{"x":1}`
		source.Text = text
		source.TextHash = composition.TextHash(text)
		second := source
		second.ArtifactID = "a2"
		second.AttemptID = "t2"
		second.Ordinal = 2
		second.PayloadArtifactID = "a2"
		second.PayloadAttemptID = "t2"
		second.IdentityArtifactID = "a2"
		second.Text = added
		second.TextHash = composition.TextHash(added)
		lookup := map[string]composition.Artifact{"a1": source, "a2": second}
		composed := &composition.Stored{}
		composed.Algorithm = composition.VisibleContinuationVersion
		composed.TextHash = composition.TextHash(text)
		composed.ArtifactIDs = []string{"a1"}
		composed.Sources = []composition.SourceRef{{ArtifactID: "a1", Revision: 0, TextHash: source.TextHash}}
		base := func() composition.Request {
			var request composition.Request
			request.Operation = "chapter-draft"
			request.OutputContract = "visible-text"
			request.RunID = "r1"
			request.RootActionID = "root1"
			request.ArtifactIDs = []string{"a1"}
			request.Lookup = lookup
			return request
		}
		cases := []struct {
			name   string
			mutate func(*composition.Request)
			want   string
		}{
			{"an agent-round run is frozen", func(r *composition.Request) { r.Operation = "agent-round" }, composition.CodeAgentControlImmutable},
			{"a stale epoch", func(r *composition.Request) { r.EpochStale = true }, composition.CodeEpochStale},
			{"a cancelled root", func(r *composition.Request) { r.RootStatus = "cancelled" }, composition.CodeActionCancelled},
			{"a draft algorithm off a chapter draft", func(r *composition.Request) {
				algorithm := "draft-visible-v2"
				r.Algorithm = &algorithm
				r.Operation = "review-chapter"
			}, composition.CodeAlgorithmInvalid},
			{"another output contract", func(r *composition.Request) { r.OutputContract = "structured-data" }, composition.CodeOutputInvalid},
			{"a malformed algorithm", func(r *composition.Request) { r.AlgorithmMalformed = true }, composition.CodeAlgorithmInvalid},
			{"an outline run", func(r *composition.Request) { r.PlotOutline = true }, composition.CodeAlgorithmInvalid},
			{"an unknown algorithm", func(r *composition.Request) {
				algorithm := "visible-append-v9"
				r.Algorithm = &algorithm
			}, composition.CodeAlgorithmInvalid},
			{"a malformed selection", func(r *composition.Request) { r.IDsMalformed = true }, composition.CodeInvalid},
			{"an empty selection", func(r *composition.Request) { r.ArtifactIDs = nil }, composition.CodeInvalid},
			{"a duplicate selection", func(r *composition.Request) { r.ArtifactIDs = []string{"a1", "a1"} }, composition.CodeInvalid},
			{"an unknown artifact", func(r *composition.Request) { r.ArtifactIDs = []string{"missing"} }, composition.CodeSourceInvalid},
			{"a source out of ledger order", func(r *composition.Request) {
				r.ArtifactIDs = []string{"a2", "a1"}
			}, composition.CodeSourceInvalid},
			{"a source detached from the binding", func(r *composition.Request) {
				detached := source
				detached.Fingerprint = `{"x":2}`
				r.Lookup = map[string]composition.Artifact{"a1": detached}
			}, composition.CodeArtifactIntegrity},
			{"a source whose text no longer hashes", func(r *composition.Request) {
				edited := source
				edited.Text = "别处来的正文。"
				r.Lookup = map[string]composition.Artifact{"a1": edited}
			}, composition.CodeArtifactIntegrity},
			{"a reconciled source", func(r *composition.Request) {
				reconciled := source
				reconciled.Purpose = "chapter-draft-reconcile"
				r.Lookup = map[string]composition.Artifact{"a1": reconciled}
			}, composition.CodeSourceInvalid},
			{"a source that is not terminal", func(r *composition.Request) {
				pending := source
				pending.AttemptStatus = "reserved"
				r.Lookup = map[string]composition.Artifact{"a1": pending}
			}, composition.CodeSourceNotTerminal},
			{"a source with no text", func(r *composition.Request) {
				blank := source
				blank.Text = "   \n"
				blank.TextHash = composition.TextHash(blank.Text)
				r.Lookup = map[string]composition.Artifact{"a1": blank}
			}, composition.CodeSourceNotTerminal},
			{"an untrusted source", func(r *composition.Request) {
				untrusted := source
				untrusted.FinishReason = "content_filter"
				r.Lookup = map[string]composition.Artifact{"a1": untrusted}
			}, composition.CodeSourceUntrusted},
			{"a source that adds no letter or digit", func(r *composition.Request) {
				punctuation := second
				punctuation.Text = "。。"
				punctuation.TextHash = composition.TextHash(punctuation.Text)
				r.ArtifactIDs = []string{"a1", "a2"}
				r.Lookup = map[string]composition.Artifact{"a1": source, "a2": punctuation}
			}, composition.CodeNoProgress},
			{"a wrong expected hash", func(r *composition.Request) {
				wrong := strings.Repeat("0", 64)
				r.ExpectedHash = &wrong
			}, composition.CodeHashMismatch},
			{"a stored receipt that no longer re-hashes", func(r *composition.Request) {
				tampered := *composed
				tampered.TextHash = strings.Repeat("0", 64)
				r.Stored = &tampered
			}, composition.CodeIntegrityFailed},
			{"a stored receipt with no algorithm", func(r *composition.Request) {
				headless := *composed
				headless.Algorithm = ""
				r.Stored = &headless
			}, composition.CodeIntegrityFailed},
			{"a selection that does not extend the stored one", func(r *composition.Request) {
				r.ArtifactIDs = []string{"a2"}
				r.Stored = composed
			}, composition.CodeRegression},
			{"a reordered selection", func(r *composition.Request) {
				r.ArtifactIDs = []string{"a1", "a2"}
				reordered := *composed
				reordered.TextHash = second.TextHash
				reordered.ArtifactIDs = []string{"a2"}
				reordered.Sources = []composition.SourceRef{{ArtifactID: "a2", Revision: 0, TextHash: second.TextHash}}
				r.Stored = &reordered
			}, composition.CodeRegression},
		}
		for _, testCase := range cases {
			request := base()
			testCase.mutate(&request)
			if receipt, err := composition.Compose(request); err == nil {
				t.Fatalf("%s: compose answered %v instead of a refusal", testCase.name, receipt)
			} else if refusal, ok := err.(*composition.Refusal); !ok || refusal.Code != testCase.want {
				t.Fatalf("%s: refused with %v; want %s", testCase.name, err, testCase.want)
			}
		}
		// The unmutated request composes and stores the hash of its own text.
		receipt, err := composition.Compose(base())
		if err != nil {
			t.Fatalf("a valid composition was refused: %v", err)
		}
		if receipt.Text != text || receipt.TextHash != productionCompositionDigest(receipt.Text) {
			t.Fatalf("a valid composition answered %v", receipt)
		}
		// Appending the second source joins the two visible texts, and the receipt
		// keeps the wire order of the selection. These candidates are shorter than the
		// overlap floor, so the repeated opening is not treated as a repeated tail.
		appended := base()
		appended.ArtifactIDs = []string{"a1", "a2"}
		grown, err := composition.Compose(appended)
		if err != nil {
			t.Fatalf("an appended composition was refused: %v", err)
		}
		if want := text + "\n\n" + added; grown.Text != want {
			t.Fatalf("the appended composition answered %q; want %q", grown.Text, want)
		}
		if len(grown.Sources) != 2 || grown.Sources[1].ArtifactID != "a2" {
			t.Fatalf("the appended composition answered sources %v", grown.Sources)
		}
	})
}
