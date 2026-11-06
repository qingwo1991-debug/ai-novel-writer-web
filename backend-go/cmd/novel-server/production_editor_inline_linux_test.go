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
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the two editor-inline names production_editor_inline_linux.go
// binds: read-recovery answers a run this server really froze and refuses every
// run it cannot prove, and cancel performs the family's own stop. Both are driven
// over the real HTTP handler through a frozen session, and everything asserted
// afterwards is read back through a connection of the test's own - the ledger store
// over a read-only handle - never through the adapter's answer.
//
// The runs themselves are written by the shipped ledger's own "open" (never by
// hand), because the point of the fixture is the binding document that ledger
// writes: the desktop hashes JSON.stringify of an insertion-ordered context
// (electron/repositories/generation-run-repository.ts:92-93), so the test freezes
// two contexts that carry the SAME members in TWO DIFFERENT insertion orders, each
// with the digest of its own stored text. A reader that re-serializes the decoded
// maps before hashing answers the sorted order and therefore refuses both; the
// adapter under test hashes the stored text and serves both. The digests here are
// computed by this file's own sha256 oracle, never by borrowing the adapter's
// hashing to build the value the adapter is supposed to verify.
//
// The document is chosen so the code-unit arithmetic cannot be faked: it ends in an
// astral character followed by a basic-plane one, so a byte-based, rune-based or
// UTF-16-based reader answers three different selections for the same offsets.
const (
	// productionEditorInlineFixtureEpoch is deliberately NOT the epoch of the
	// session that reads these runs. The family's identity check is against the
	// run's binding (and the origin epoch frozen beside it), not against the
	// caller's live lease, so a recovery after a reload keeps answering; a reader
	// that compared the caller's lease instead would refuse every fixture here.
	productionEditorInlineFixtureEpoch = "lease-0000000009-editor-inline"
	// productionEditorInlineFixtureSnapshot is the context snapshot id both
	// fixtures freeze.
	productionEditorInlineFixtureSnapshot = "ci-editor-inline-snapshot-1"
	// productionEditorInlineFixtureModelID is the model the fixture runs name, and
	// the id read-recovery must hand back.
	productionEditorInlineFixtureModelID = productionTestModelID
	// productionEditorInlineFixtureDocument is 8 basic-plane characters, then
	// U+1F600 (two code units), then one more: 11 code units, 10 runes, 31 bytes.
	productionEditorInlineFixtureDocument = "月色落在石阶上。😀甲"
	// productionEditorInlineFixtureSelected is document.slice(10, 11): the
	// selection starts AFTER the astral character, so its rune index is 9 and its
	// code-unit index is 10.
	productionEditorInlineFixtureSelected = "甲"
	// productionEditorInlineFixtureAstral is document.slice(8, 10): the pair itself.
	productionEditorInlineFixtureAstral = "😀"
	// productionEditorInlineFixtureMidPair starts inside the surrogate pair. It is
	// a legal pair of code-unit offsets and an impossible selection.
	productionEditorInlineFixtureMidPairFrom = 9
	productionEditorInlineFixtureMidPairTo   = 10
)

// productionEditorInlineFixture is one frozen run plus the identity the browser
// would hold for it.
type productionEditorInlineFixture struct {
	Handle map[string]any
	RunID  string
	RootID string
}

// productionEditorInlineFixtures is the seeded set, keyed by the name the subtests
// ask for, so a missing run fails loudly instead of asserting nothing.
type productionEditorInlineFixtures struct {
	ProjectID string
	Epoch     string
	Runs      map[string]productionEditorInlineFixture
}

func (f productionEditorInlineFixtures) run(t *testing.T, name string) productionEditorInlineFixture {
	t.Helper()
	run, ok := f.Runs[name]
	if !ok {
		t.Fatalf("the fixture froze no run named %v", name)
	}
	return run
}

func (f productionEditorInlineFixtures) handle(t *testing.T, name string) map[string]any {
	t.Helper()
	return f.run(t, name).Handle
}

// productionEditorInlineDigest is this file's own sha256/hex oracle for the frozen
// digests.
func productionEditorInlineDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// productionEditorInlineContextOrdered freezes the context with kind first, the way
// an author-side builder fills it in.
func productionEditorInlineContextOrdered(document, selection string, from, to int64) string {
	return `{"kind":"author-draft","documentText":"` + document + `","documentHash":"` +
		productionEditorInlineDigest(document) + `","action":"refine","from":` + strconv.FormatInt(from, 10) +
		`,"to":` + strconv.FormatInt(to, 10) + `,"selectedText":"` + selection +
		`","writingLanguage":"zh-CN","template":{"key":"edit_selected_text","title":"润色选中文字"}}`
}

// productionEditorInlineContextReversed freezes the same nine members in a
// different insertion order, so the two digests cannot both be reproduced by any
// implementation that drops the order.
func productionEditorInlineContextReversed(document, selection string, from, to int64) string {
	return `{"writingLanguage":"zh-CN","template":{"key":"edit_selected_text","title":"润色选中文字"},"selectedText":"` +
		selection + `","to":` + strconv.FormatInt(to, 10) + `,"from":` + strconv.FormatInt(from, 10) +
		`,"action":"expand","documentText":"` + document + `","documentHash":"` +
		productionEditorInlineDigest(document) + `","kind":"author-draft"}`
}

// productionEditorInlineInputText is the frozen editorInlineInput beside a
// context, i.e. the five-field projection of it.
func productionEditorInlineInputText(action, document, selection string, from, to int64) string {
	return `{"action":"` + action + `","documentText":"` + document + `","from":` + strconv.FormatInt(from, 10) +
		`,"to":` + strconv.FormatInt(to, 10) + `,"selectedText":"` + selection + `"}`
}

// productionEditorInlineRequestText is one ledger "open" request as text, so the
// sourceManifest reaches the ledger ordered: the ledger writes binding_json from
// the parsed text, and that text is the only place the context's insertion order
// survives.
func productionEditorInlineRequestText(name, projectID, epoch, operation, contextText, inputText, digest string) string {
	hash := strings.Repeat("a", 64)
	return `{"projectId":"` + projectID + `","epoch":"` + epoch + `","operation":"` + operation +
		`","uiActionNonce":"production-editor-inline-` + name + `","frozenInputHash":"` + hash +
		`","fingerprint":{"chapterBriefHash":"` + hash + `","authorGuidanceHash":"` + hash +
		`","dependencyHash":"` + hash + `","contextSnapshotHash":"` + hash + `","templateHash":"` + hash +
		`","skillSnapshotHash":"` + hash + `","modelLeaseRevision":"` + hash + `","policyHash":"` + hash +
		`","outputContractHash":"` + hash + `"},"contextSnapshotId":"` + productionEditorInlineFixtureSnapshot +
		`","sourceManifest":{"operation":"` + operation + `","modelId":"` + productionEditorInlineFixtureModelID +
		`","editorInlineOriginEpoch":"` + epoch + `","editorInlineContext":` + contextText +
		`,"editorInlineContextHash":"` + digest + `","editorInlineInput":` + inputText +
		`},"sourceRefs":[],"budget":{"maxPhysicalRequests":3,"maxTokenLiability":4000,"maxOutputPerRequest":1000,"maxActiveElapsedMs":600000}}`
}

// productionEditorInlineSeedOne writes one run through the shipped ledger and then
// asserts, over the bytes the database actually holds, that the digest frozen in
// that row is the digest of that row's own context text. It rewrites nothing: the
// fixture is exactly what the ledger wrote.
//
// The digest is computed here from the fixture's literal before the insert (the row
// must carry a real value), so the post-insert byte comparison below is what makes
// it the digest of the stored row: the stored context must be the same literal, so
// the insertion order the digest was taken over is the order the row holds.
func productionEditorInlineSeedOne(t *testing.T, db *sql.DB, ledger *store.GenerationLedger, name, projectID, epoch, operation, contextText, inputText string, tamper bool) productionEditorInlineFixture {
	t.Helper()
	// The tampered row freezes a digest no context text can produce; every other row
	// freezes the digest of its own literal.
	mismatch := strings.Repeat("0", 64)
	digest := productionEditorInlineDigest(contextText)
	if tamper {
		digest = mismatch
	}
	request := productionEditorInlineRequestText(name, projectID, epoch, operation, contextText, inputText, digest)
	value, err := ledger.CallJSON(context.Background(), "open", json.RawMessage("["+request+"]"))
	if err != nil {
		t.Fatalf("seed %v open: %v", name, err)
	}
	run := syntheticMap(value)
	runID := syntheticString(run["runId"])
	rootID := syntheticString(run["rootActionId"])
	if runID == "" || rootID == "" {
		t.Fatalf("seed %v open returned no identity: %v", name, run)
	}
	var stored string
	if err := db.QueryRow(`SELECT binding_json FROM generation_runs WHERE run_id=?`, runID).Scan(&stored); err != nil {
		t.Fatalf("seed %v readback: %v", name, err)
	}
	// The exact bytes the row froze for the context, extracted the same way the read
	// extracts them. Nothing here re-serializes the row.
	raw, err := productionEditorInlineStoredContext(context.Background(), db, runID)
	if err != nil {
		t.Fatalf("seed %v stored context: %v", name, err)
	}
	if string(raw) != contextText {
		t.Fatalf("seed %v stored context %s, want the frozen literal %s: %s", name, raw, contextText, stored)
	}
	// The one assertion that matters: the hash frozen in this row is (or, for the
	// tampered fixture, is not) the digest of this row's stored context text.
	storedDigest := productionEditorInlineDigest(string(raw))
	if tamper {
		if strings.Contains(stored, storedDigest) {
			t.Fatalf("seed %v froze the true digest %v of its own context, so the row is not tampered: %s", name, storedDigest, stored)
		}
	} else if !strings.Contains(stored, storedDigest) {
		t.Fatalf("seed %v froze no digest of its own stored context %v: %s", name, storedDigest, stored)
	}
	return productionEditorInlineFixture{Handle: productionEditorInlineHandle(projectID, epoch, rootID, runID),
		RunID: runID, RootID: rootID}
}

// productionEditorInlineHandle is the four-key handle the browser holds, in the
// epoch the run was frozen under.
func productionEditorInlineHandle(projectID, epoch, rootID, runID string) map[string]any {
	return map[string]any{"projectId": projectID, "epoch": epoch, "rootActionId": rootID, "runId": runID}
}

// productionEditorInlineSeed freezes the five runs the read subtests need, before
// the lane takes its writer lease, and returns their identities.
func productionEditorInlineSeed(t *testing.T, projectPath string) productionEditorInlineFixtures {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return fmt.Sprintf("production-editor-inline-%02d", ordinal)
	})
	document := productionEditorInlineFixtureDocument
	projectID := productionEditorProjectID
	epoch := productionEditorInlineFixtureEpoch
	out := productionEditorInlineFixtures{ProjectID: projectID, Epoch: epoch,
		Runs: map[string]productionEditorInlineFixture{}}
	ordered := productionEditorInlineContextOrdered(document, productionEditorInlineFixtureSelected, 10, 11)
	reversed := productionEditorInlineContextReversed(document, productionEditorInlineFixtureAstral, 8, 10)
	orderedInput := productionEditorInlineInputText("refine", document, productionEditorInlineFixtureSelected, 10, 11)
	reversedInput := productionEditorInlineInputText("expand", document, productionEditorInlineFixtureAstral, 8, 10)
	out.Runs["ordered"] = productionEditorInlineSeedOne(t, db, ledger, "ordered", projectID, epoch,
		productionEditorInlineOperation, ordered, orderedInput, false)
	out.Runs["reversed"] = productionEditorInlineSeedOne(t, db, ledger, "reversed", projectID, epoch,
		productionEditorInlineOperation, reversed, reversedInput, false)
	// A run of another family that carries this family's keys: the ledger admits it
	// (its closed family list only refuses the keys it already names), so the read
	// must refuse it on the manifest's operation alone.
	out.Runs["wrong-operation"] = productionEditorInlineSeedOne(t, db, ledger, "wrong-operation", projectID, epoch,
		"chapter-draft", ordered, orderedInput, false)
	// A run whose manifest promises a digest its context cannot produce.
	out.Runs["tampered"] = productionEditorInlineSeedOne(t, db, ledger, "tampered", projectID, epoch,
		productionEditorInlineOperation, ordered, orderedInput, true)
	// A run whose frozen input starts inside the surrogate pair.
	shortInput := productionEditorInlineInputText("refine", document, productionEditorInlineFixtureAstral,
		productionEditorInlineFixtureMidPairFrom, productionEditorInlineFixtureMidPairTo)
	out.Runs["short-selection"] = productionEditorInlineSeedOne(t, db, ledger, "short-selection", projectID, epoch,
		productionEditorInlineOperation, ordered, shortInput, false)
	productionEditorCheckpoint(t, db, database)
	return out
}

// productionEditorInlineAnswer posts one channel request and returns the bare result
// object: both served names answer a value the browser reads verbatim, never the
// mutation family's {success,error} DTO.
func productionEditorInlineAnswer(t *testing.T, label string, client *productionDurableClient, operation string, request, frozen map[string]any) map[string]any {
	t.Helper()
	response := client.call(t, operation, []any{request, frozen})
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%v is still the lane's fail-closed 501 (the binder is not installed): %s", label, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%v: HTTP %v %s", label, response.Code, response.Body.String())
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		t.Fatalf("%v: %v %s", label, err, response.Body.String())
	}
	return wire.Result
}

// productionEditorInlineRefused asserts one request was refused, that the binder is
// installed at all, and that the refusal carried exactly the symbolic code the read
// envelope promises.
func productionEditorInlineRefused(t *testing.T, label string, response *httptest.ResponseRecorder, want string) {
	t.Helper()
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%v is still the lane's fail-closed 501 (the binder is not installed): %s", label, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%v answered 200 where this lane must refuse with %v: %s", label, want, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %v %s", label, err, response.Body.String())
	}
	if body.Error.Code != want {
		t.Fatalf("%v refused with %v, want %v: %s", label, body.Error.Code, want, response.Body.String())
	}
}

// productionEditorInlineLeaseRefusal asserts a request refused before admission,
// through one of the frozen session's own codes.
func productionEditorInlineLeaseRefusal(t *testing.T, label string, response *httptest.ResponseRecorder) {
	t.Helper()
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%v is still the lane's fail-closed 501 (the binder is not installed): %s", label, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%v answered 200 under a lease the server never issued: %s", label, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %v %s", label, err, response.Body.String())
	}
	switch body.Error.Code {
	case "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE", "PROJECT_NOT_OPEN",
		"AUTHOR_WRITE_STALE", "PROJECT_SESSION_REQUIRED", "PROJECT_SESSION_CONTEXT_REQUIRED":
	default:
		t.Fatalf("%v refused with %v, which is not a frozen-session refusal: %s", label, body.Error.Code, response.Body.String())
	}
}

// TestProductionLaneServesEditorInlineRecoveryAndCancel is the family's proof.
func TestProductionLaneServesEditorInlineRecoveryAndCancel(t *testing.T) {
	t.Run("read-recovery answers the runs it can prove", testProductionEditorInlineReadServes)
	t.Run("read-recovery refuses the runs it cannot prove", testProductionEditorInlineReadRefuses)
	t.Run("read-recovery refuses a lease the server never issued", testProductionEditorInlineReadStaleLease)
	t.Run("cancel stops the family root and keeps it stopped", testProductionEditorInlineCancel)
}

// testProductionEditorInlineReadServes is the happy path, in both key orders.
func testProductionEditorInlineReadServes(t *testing.T) {
	var fixtures productionEditorInlineFixtures
	lane := productionGenerationWriteOpen(t, nil, func(t *testing.T, projectPath string) {
		fixtures = productionEditorInlineSeed(t, projectPath)
	})
	ledger := productionLifecycleLedger(t, lane.projectPath)
	for _, name := range []string{"ordered", "reversed"} {
		run := fixtures.run(t, name)
		answer := productionEditorInlineAnswer(t, "read-recovery "+name, lane.author,
			productionEditorInlineReadRecoveryName, map[string]any{"handle": run.Handle}, lane.frozen)
		if got := syntheticString(answer["sourceStatus"]); got != "conflict" {
			t.Fatalf("read-recovery %v answered sourceStatus %v; a run this lane did not open freezes no source fingerprint, so the reference's own conflict fallback is the only honest answer", name, got)
		}
		if got := syntheticString(answer["modelId"]); got != productionEditorInlineFixtureModelID {
			t.Fatalf("read-recovery %v answered modelId %v, want %v", name, got, productionEditorInlineFixtureModelID)
		}
		view := syntheticMap(answer["view"])
		if view == nil {
			t.Fatalf("read-recovery %v answered no view: %v", name, answer)
		}
		if got := syntheticString(view["status"]); got != "running" {
			t.Fatalf("read-recovery %v projected status %v; want running", name, got)
		}
		if got := syntheticString(view["operation"]); got != productionEditorInlineOperation {
			t.Fatalf("read-recovery %v projected operation %v", name, got)
		}
		if view["nonReplayable"] != false {
			t.Fatalf("read-recovery %v projected nonReplayable=%v for a running, unblocked, idle run", name, view["nonReplayable"])
		}
		answered := syntheticMap(view["handle"])
		if got := syntheticString(answered["runId"]); got != run.RunID {
			t.Fatalf("read-recovery %v projected runId %v, want %v", name, got, run.RunID)
		}
		// The projected handle carries the FROZEN epoch, not the caller's lease:
		// the family's identity is the run's own.
		if got := syntheticString(answered["epoch"]); got != productionEditorInlineFixtureEpoch {
			t.Fatalf("read-recovery %v projected epoch %v; want the frozen %v", name, got, productionEditorInlineFixtureEpoch)
		}
		contextValue := syntheticMap(answer["context"])
		if contextValue == nil {
			t.Fatalf("read-recovery %v answered no context: %v", name, answer)
		}
		if got := syntheticString(contextValue["documentText"]); got != productionEditorInlineFixtureDocument {
			t.Fatalf("read-recovery %v answered documentText %v", name, got)
		}
		if got := syntheticString(contextValue["documentHash"]); got != productionEditorInlineDigest(productionEditorInlineFixtureDocument) {
			t.Fatalf("read-recovery %v answered documentHash %v", name, got)
		}
		if got := syntheticString(contextValue["kind"]); got != "author-draft" {
			t.Fatalf("read-recovery %v answered kind %v", name, got)
		}
		// The selection is the document sliced at the frozen code-unit offsets: 甲
		// for the ordered fixture (10,11), the astral pair for the reversed one
		// (8,10). A rune- or byte-based reader answers different text here.
		from, fromOK := contextValue["from"].(float64)
		to, toOK := contextValue["to"].(float64)
		if !fromOK || !toOK {
			t.Fatalf("read-recovery %v answered non-integer offsets: %v %v", name, contextValue["from"], contextValue["to"])
		}
		wantSelection := "甲"
		wantFrom, wantTo := float64(10), float64(11)
		if name == "reversed" {
			wantSelection, wantFrom, wantTo = productionEditorInlineFixtureAstral, 8, 10
		}
		if from != wantFrom || to != wantTo {
			t.Fatalf("read-recovery %v answered offsets %v..%v, want %v..%v", name, from, to, wantFrom, wantTo)
		}
		if got := syntheticString(contextValue["selectedText"]); got != wantSelection {
			t.Fatalf("read-recovery %v answered selectedText %v, want %v", name, got, wantSelection)
		}
		if got := productionLifecycleRunStatus(t, ledger, run.RunID); got != "running" {
			t.Fatalf("read-recovery %v moved the run row to %v", name, got)
		}
	}
	// The reference drops every request key but handle (the controller hands the
	// owner request.handle), so the envelope is closed on the handle and open on the
	// wrapper. This asserts the port keeps that, instead of inventing a stricter
	// request shape the desktop would never send.
	ordered := fixtures.run(t, "ordered")
	answer := productionEditorInlineAnswer(t, "read-recovery with an unused request key", lane.author,
		productionEditorInlineReadRecoveryName,
		map[string]any{"handle": ordered.Handle, "cancelledAt": float64(1)}, lane.frozen)
	if got := syntheticString(syntheticMap(answer["view"])["status"]); got != "running" {
		t.Fatalf("read-recovery with an unused request key answered status %v", got)
	}
	lane.assertNoDial(t)
}

// testProductionEditorInlineReadRefuses is the refusal set: every one of them is a
// run this lane cannot prove, and none of them may write a byte.
func testProductionEditorInlineReadRefuses(t *testing.T) {
	var fixtures productionEditorInlineFixtures
	lane := productionGenerationWriteOpen(t, nil, func(t *testing.T, projectPath string) {
		fixtures = productionEditorInlineSeed(t, projectPath)
	})
	baseline := lane.bytes(t)
	refuse := func(label string, request map[string]any, want string) {
		t.Helper()
		response := lane.author.call(t, productionEditorInlineReadRecoveryName, []any{request, lane.frozen})
		productionEditorInlineRefused(t, label, response, want)
	}
	refuse("a run of another family", map[string]any{"handle": fixtures.handle(t, "wrong-operation")},
		productionEditorInlineContextInvalid)
	refuse("a tampered context digest", map[string]any{"handle": fixtures.handle(t, "tampered")},
		productionEditorInlineContextInvalid)
	refuse("a frozen input that starts inside a surrogate pair",
		map[string]any{"handle": fixtures.handle(t, "short-selection")}, productionEditorInlineInputInvalid)
	// A run the database does not hold: the ledger answers GENERATION_RUN_MISSING and
	// the adapter forwards that code instead of inventing a different one.
	refuse("a run the database does not hold", map[string]any{"handle": productionEditorInlineHandle(
		fixtures.ProjectID, fixtures.Epoch, "production-editor-inline-no-root", "production-editor-inline-no-run")},
		productionEditorInlineRunMissing)
	// The epoch term: a handle that names the run but not the epoch it was frozen
	// under is an identity mismatch, even though the caller's live lease is valid.
	movedEpoch := productionEditorInlineHandle(fixtures.ProjectID, "lease-0000000001-history",
		fixtures.run(t, "ordered").RootID, fixtures.run(t, "ordered").RunID)
	refuse("a handle with another epoch", map[string]any{"handle": movedEpoch},
		productionEditorInlineIdentityMismatch)
	// The project term: a handle naming another project never reaches a row.
	movedProject := productionEditorInlineHandle("3ce1f7a2-5d84-4b6f-a0c9-000000000000", fixtures.Epoch,
		fixtures.run(t, "ordered").RootID, fixtures.run(t, "ordered").RunID)
	refuse("a handle naming another project", map[string]any{"handle": movedProject},
		productionEditorInlineIdentityMismatch)
	// The handle is closed: a bare run id, an extra key and a missing epoch are all
	// the same identity mismatch the reference reports.
	refuse("a bare run id instead of a handle", map[string]any{"handle": fixtures.run(t, "ordered").RunID},
		productionEditorInlineIdentityMismatch)
	extra := map[string]any{}
	for key, value := range fixtures.run(t, "ordered").Handle {
		extra[key] = value
	}
	extra["cancelledAt"] = float64(1)
	refuse("a handle carrying an extra key", map[string]any{"handle": extra},
		productionEditorInlineIdentityMismatch)
	noEpoch := map[string]any{"projectId": fixtures.ProjectID, "rootActionId": fixtures.run(t, "ordered").RootID,
		"runId": fixtures.run(t, "ordered").RunID}
	refuse("a handle with no epoch", map[string]any{"handle": noEpoch}, productionEditorInlineIdentityMismatch)
	refuse("no handle at all", map[string]any{}, productionEditorInlineIdentityMismatch)
	// Ten refusals, no write: a failed read leaves the committed database exactly
	// as it was.
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused read-recovery changed the committed database file")
	}
	ledger := productionLifecycleLedger(t, lane.projectPath)
	if got := productionLifecycleRunStatus(t, ledger, fixtures.run(t, "ordered").RunID); got != "running" {
		t.Fatalf("a refused read-recovery moved the proved run to %v", got)
	}
	lane.assertNoDial(t)
}

// testProductionEditorInlineReadStaleLease proves the read is behind the same
// single-writer admission as every other production read: a lease the server never
// issued never reaches the ledger, even for a run that does exist.
func testProductionEditorInlineReadStaleLease(t *testing.T) {
	var fixtures productionEditorInlineFixtures
	lane := productionGenerationWriteOpen(t, nil, func(t *testing.T, projectPath string) {
		fixtures = productionEditorInlineSeed(t, projectPath)
	})
	baseline := lane.bytes(t)
	request := map[string]any{"handle": fixtures.handle(t, "ordered")}
	productionEditorInlineLeaseRefusal(t, "read-recovery under a stale lease",
		lane.author.call(t, productionEditorInlineReadRecoveryName, []any{request, lane.stale()}))
	// The context, not just the session, is three keys: a context missing one is
	// refused before admission too.
	productionEditorInlineLeaseRefusal(t, "read-recovery with a truncated context",
		lane.author.call(t, productionEditorInlineReadRecoveryName, []any{request, map[string]any{"projectId": lane.frozen["projectId"]}}))
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused read-recovery changed the committed database file")
	}
	ledger := productionLifecycleLedger(t, lane.projectPath)
	if got := productionLifecycleRunStatus(t, ledger, fixtures.run(t, "ordered").RunID); got != "running" {
		t.Fatalf("a stale-lease read moved the run to %v", got)
	}
	lane.assertNoDial(t)
}

// testProductionEditorInlineCancel proves cancel is the family's own stop: it
// settles the root and the run, answers the post-stop view, and is idempotent.
func testProductionEditorInlineCancel(t *testing.T) {
	var fixtures productionEditorInlineFixtures
	lane := productionGenerationWriteOpen(t, nil, func(t *testing.T, projectPath string) {
		fixtures = productionEditorInlineSeed(t, projectPath)
	})
	ledger := productionLifecycleLedger(t, lane.projectPath)
	run := fixtures.run(t, "ordered")
	// A handle for another project is refused before anything is written.
	movedProject := productionEditorInlineHandle("3ce1f7a2-5d84-4b6f-a0c9-000000000000", fixtures.Epoch, run.RootID, run.RunID)
	baseline := lane.bytes(t)
	response := lane.author.call(t, productionEditorInlineCancelName, []any{map[string]any{"handle": movedProject}, lane.frozen})
	productionEditorInlineRefused(t, "cancel of another project's handle", response, productionEditorInlineIdentityMismatch)
	// A run of another family is refused by the same proof the read uses.
	response = lane.author.call(t, productionEditorInlineCancelName, []any{map[string]any{"handle": fixtures.handle(t, "wrong-operation")}, lane.frozen})
	productionEditorInlineRefused(t, "cancel of a run of another family", response, productionEditorInlineContextInvalid)
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused editor-inline:cancel changed the committed database file")
	}
	if got := productionLifecycleRootStatus(t, ledger, run.RootID); got != "active" {
		t.Fatalf("a refused editor-inline:cancel moved the root row to %v", got)
	}
	// The admitted stop.
	answer := productionEditorInlineAnswer(t, "cancel", lane.author, productionEditorInlineCancelName,
		map[string]any{"handle": run.Handle}, lane.frozen)
	if got := syntheticString(answer["status"]); got != "cancelled" {
		t.Fatalf("cancel answered status %v, want cancelled", got)
	}
	if got := syntheticString(syntheticMap(answer["handle"])["runId"]); got != run.RunID {
		t.Fatalf("cancel answered for run %v, want %v", got, run.RunID)
	}
	if got := productionLifecycleRunStatus(t, ledger, run.RunID); got != "cancelled" {
		t.Fatalf("cancel left the ledger run row at %v; want cancelled", got)
	}
	if got := productionLifecycleRootStatus(t, ledger, run.RootID); got != "cancelled" {
		t.Fatalf("cancel left the ledger root row at %v; want cancelled", got)
	}
	// A repeated cancel answers the cancelled view instead of a settled refusal:
	// the reference forwards whatever the stop projected, and the ledger's stop
	// returns before writing when the root is already cancelled.
	repeated := productionEditorInlineAnswer(t, "repeated cancel", lane.author, productionEditorInlineCancelName,
		map[string]any{"handle": run.Handle}, lane.frozen)
	if got := syntheticString(repeated["status"]); got != "cancelled" {
		t.Fatalf("a repeated cancel answered status %v, want cancelled", got)
	}
	// The context and the input survived the stop untouched: cancel settles the
	// control rows and rewrites neither.
	recovered := productionEditorInlineAnswer(t, "read-recovery after cancel", lane.author,
		productionEditorInlineReadRecoveryName, map[string]any{"handle": run.Handle}, lane.frozen)
	if got := syntheticString(syntheticMap(recovered["view"])["status"]); got != "cancelled" {
		t.Fatalf("read-recovery after cancel projected status %v", got)
	}
	if got := syntheticString(syntheticMap(recovered["context"])["selectedText"]); got != productionEditorInlineFixtureSelected {
		t.Fatalf("read-recovery after cancel answered selectedText %v", got)
	}
	// A lease the server never issued cannot stop anything, and writes nothing.
	stopped := lane.bytes(t)
	productionEditorInlineLeaseRefusal(t, "cancel under a stale lease",
		lane.author.call(t, productionEditorInlineCancelName, []any{map[string]any{"handle": run.Handle}, lane.stale()}))
	if !bytes.Equal(lane.bytes(t), stopped) {
		t.Fatal("a refused editor-inline:cancel changed the committed database file")
	}
	if got := productionLifecycleRunStatus(t, ledger, run.RunID); got != "cancelled" {
		t.Fatalf("a refused cancel moved the run row to %v", got)
	}
	lane.assertNoDial(t)
}
