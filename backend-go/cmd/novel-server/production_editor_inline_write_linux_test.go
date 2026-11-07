//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the two editor-inline names production_editor_inline_write_linux.go
// binds, over the real HTTP handler and the frozen project session:
//
//	begin    freezes one run whose context and task are the shipped template
//	execute  dispatches that frozen task exactly once and settles the attempt
//
// The model is stubbed at the executor's dispatch seam (productionConfig.Dispatch),
// so no test dials the loopback origin the lane commits as its only endpoint; every
// loopback assertion below is lane.assertNoDial, the same witness the sibling write
// tests use. Everything asserted about the run is read back through a connection of
// the test's own - the ledger store or a read-only handle - never through the
// adapter's answer.
//
// The document is chosen so the digest cannot be faked: it carries an astral
// character (two UTF-16 units, one rune, four bytes), a blank line, and the
// characters < and &, which the standard Go encoder escapes and JSON.stringify does
// not. A lane that froze an HTML-escaped or key-sorted context would fail its own
// recovery read on this document, so begin succeeding here is the no-re-serialization
// proof - and the begin test asserts those characters are still raw in the stored
// binding.
const (
	// productionEditorInlineWriteDocument is 19 UTF-16 code units: 8 basic-plane
	// characters, U+1F600, two newlines, then 甲 & 乙, a space and <笑>.
	productionEditorInlineWriteDocument = "月色落在石阶上。😀\n\n甲&乙 <笑>"
	// productionEditorInlineWriteSelected is document.slice(12, 15): the code-unit
	// range that starts after the astral pair and the blank line.
	productionEditorInlineWriteSelected = "甲&乙"
	productionEditorInlineWriteFrom     = 12
	productionEditorInlineWriteTo       = 15
	// productionEditorInlineWriteGenerated is the text the stub answers with, so a
	// receipt that echoed the request instead of the dispatch is detectable.
	productionEditorInlineWriteGenerated = "生产内联：门后有人。"
	// productionEditorInlineWriteBeginNonce and its replay twin are the renderer's
	// uiActionNonce: one run per nonce, and a replay must answer that same run.
	productionEditorInlineWriteBeginNonce     = "production-editor-inline-write-begin-1"
	productionEditorInlineWriteForeignBegin   = "production-editor-inline-write-foreign-1"
	productionEditorInlineWriteOtherBegin     = "production-editor-inline-write-other-1"
	productionEditorInlineWriteOtherProjectID = "3ce1f7a2-5d84-4b6f-a0c9-000000000000"
	// productionEditorInlineWriteInstructNote is the author's own note, so the
	// instruct arm proves the renderer-sent instruction reaches the task beside the
	// shipped template rather than replacing it.
	productionEditorInlineWriteInstructNote = "把这两句改成第一人称。"
)

// productionEditorInlineWriteSeam is the executor's injectable dispatch seam for
// this family. It answers one visible-text completion and counts its calls, and it
// can read the ledger while the dispatch is in flight - which is how the transaction
// arm proves the attempt row was reserved and marked dispatched BEFORE the provider
// was called.
type productionEditorInlineWriteSeam struct {
	mu        sync.Mutex
	calls     int
	text      string
	ledger    *store.GenerationLedger
	rootID    string
	observed  string
	attemptID string
	watched   []string
	err       error
}

func (s *productionEditorInlineWriteSeam) dispatch(_ context.Context, _ any, callbacks generation.Callbacks) (generation.DispatchResult, error) {
	s.mu.Lock()
	s.calls++
	calls := s.calls
	ledger, rootID := s.ledger, s.rootID
	s.mu.Unlock()
	if calls > 1 {
		return generation.DispatchResult{}, errProductionExecuteDispatchedTwice
	}
	if ledger != nil && rootID != "" {
		status, attemptID, watched, err := productionEditorInlineWriteAttemptAtDispatch(ledger, rootID)
		s.mu.Lock()
		s.observed, s.attemptID, s.watched, s.err = status, attemptID, watched, err
		s.mu.Unlock()
	}
	if callbacks.OnVisible != nil {
		callbacks.OnVisible(generation.VisibleEvent{Kind: "cumulative", Text: s.text})
	}
	// The reduced lane plans an "unknown" reasoning policy, so the settled usage has
	// to satisfy total == prompt+completion and reasoning <= completion.
	return generation.DispatchResult{
		Usage:        map[string]any{"trusted": true, "promptTokens": 6, "completionTokens": 12, "reasoningTokens": 0, "totalTokens": 18},
		FinishReason: "stop",
	}, nil
}

// watch points the seam at one run's root, so its dispatch reads that run's attempt
// rows from the ledger while it runs.
func (s *productionEditorInlineWriteSeam) watch(ledger *store.GenerationLedger, rootID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger, s.rootID = ledger, rootID
}

func (s *productionEditorInlineWriteSeam) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *productionEditorInlineWriteSeam) atDispatch() (string, string, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.observed, s.attemptID, s.watched, s.err
}

// productionEditorInlineWriteAttemptAtDispatch reads the root's attempt rows from a
// connection of the test's own while the provider call is in flight. It reports the
// status of the single attempt it expects, the attempt id, and every status it saw,
// so a lane that dispatched before reserving (or never marked dispatched at all) is
// visible from inside the dispatch rather than inferred afterwards.
func productionEditorInlineWriteAttemptAtDispatch(ledger *store.GenerationLedger, rootID string) (string, string, []string, error) {
	value, err := ledger.Call(context.Background(), "budget", []any{rootID})
	if err != nil {
		return "", "", nil, err
	}
	attempts := syntheticSlice(syntheticMap(value)["attempts"])
	statuses := make([]string, 0, len(attempts))
	for _, item := range attempts {
		statuses = append(statuses, syntheticString(syntheticMap(item)["status"]))
	}
	if len(attempts) != 1 {
		return "", "", statuses, fmt.Errorf("the dispatch saw %d attempt rows for the run, want the one it reserved", len(attempts))
	}
	attempt := syntheticMap(attempts[0])
	return syntheticString(attempt["status"]), syntheticString(attempt["attemptId"]), statuses, nil
}

// productionEditorInlineWriteDatabase opens the committed author database on a
// read-only handle of the test's own, so every stored byte asserted below is read
// without the lane's admitted connection.
func productionEditorInlineWriteDatabase(t *testing.T, projectPath string) *sql.DB {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro&_busy_timeout=5000"
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
	return db
}

// productionEditorInlineWriteStored returns the exact stored text of one member of
// one run's frozen manifest, extracted the way the adapter extracts it: from the
// binding document's own bytes, never from a decoded map.
func productionEditorInlineWriteStored(t *testing.T, db *sql.DB, runID, member string) string {
	t.Helper()
	var stored string
	if err := db.QueryRow(`SELECT binding_json FROM generation_runs WHERE run_id=?`, runID).Scan(&stored); err != nil {
		t.Fatalf("read the committed binding of run %v: %v", runID, err)
	}
	manifest, ok := productionEditorInlineRawMember([]byte(stored), "sourceManifest")
	if !ok {
		t.Fatalf("the committed run %v holds no source manifest", runID)
	}
	raw, ok := productionEditorInlineRawMember(manifest, member)
	if !ok {
		t.Fatalf("the committed run %v froze no %v member", runID, member)
	}
	return string(raw)
}

// productionEditorInlineWriteCount reads one scalar count from the committed
// database.
func productionEditorInlineWriteCount(t *testing.T, db *sql.DB, query, argument string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query, argument).Scan(&count); err != nil {
		t.Fatalf("%v", err)
	}
	return count
}

// productionEditorInlineWriteBeginRequest is the renderer's begin request: an
// EditorInlineInput, the model main resolved, and the one-shot uiActionNonce.
func productionEditorInlineWriteBeginRequest(nonce, action string) map[string]any {
	input := map[string]any{
		"action":       action,
		"documentText": productionEditorInlineWriteDocument,
		"from":         productionEditorInlineWriteFrom,
		"to":           productionEditorInlineWriteTo,
		"selectedText": productionEditorInlineWriteSelected,
	}
	return map[string]any{"input": input, "modelId": productionTestModelID, "uiActionNonce": nonce}
}

// productionEditorInlineWriteInstructRequest is the same request for the one action
// that carries an author note.
func productionEditorInlineWriteInstructRequest(nonce, instruction string) map[string]any {
	request := productionEditorInlineWriteBeginRequest(nonce, "instruct")
	syntheticMap(request["input"])["instruction"] = instruction
	return request
}

// productionEditorInlineWriteAnswer posts one write request and returns the bare
// business result, failing when the name still answers the lane's fail-closed 501 or
// leaves the 200 envelope the browser reads verbatim.
func productionEditorInlineWriteAnswer(t *testing.T, label string, client *productionDurableClient, operation string, request, frozen map[string]any) map[string]any {
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

// productionEditorInlineWriteRefusal posts one write request and returns the raw
// refusal body, asserting the name is bound at all and that it refused through the
// family envelope with exactly the symbolic code expected.
func productionEditorInlineWriteRefusal(t *testing.T, label string, client *productionDurableClient, operation string, request, frozen map[string]any, want string) string {
	t.Helper()
	response := client.call(t, operation, []any{request, frozen})
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
	return response.Body.String()
}

// productionEditorInlineWriteSessionRefusal asserts one request was refused by the
// frozen session rather than by anything this family invented, and returns the raw
// body so the two served names can be compared byte for byte. Every honest session
// refusal - a lease the server never issued, a project the session does not own, a
// session that has moved - is in the set the family's read tests already accept, so
// this does not pin a code the admission is free to choose.
func productionEditorInlineWriteSessionRefusal(t *testing.T, label string, response *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%v is still the lane's fail-closed 501 (the binder is not installed): %s", label, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%v answered 200 under a context this server never issued: %s", label, response.Body.String())
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
	return response.Body.String(), body.Error.Code
}

// TestProductionLaneServesEditorInlineBeginAndExecute is the family's write proof.
func TestProductionLaneServesEditorInlineBeginAndExecute(t *testing.T) {
	t.Run("begin freezes the shipped template", testProductionEditorInlineWriteBegin)
	t.Run("begin refuses a spent nonce that froze another input", testProductionEditorInlineWriteBeginNonceConflict)
	t.Run("execute dispatches the frozen task once", testProductionEditorInlineWriteExecute)
	t.Run("both served names refuse the same way", testProductionEditorInlineWriteRefusals)
	t.Run("the frozen context and task are composed, not restated", testProductionEditorInlineWriteComposition)
}

// testProductionEditorInlineWriteBegin is the admitting half: one run whose frozen
// context is the shipped template and whose frozen input is the renderer's, answered
// with the same four fields the recovery read answers - and a replay of the same
// nonce that answers the same run instead of opening a second one.
func testProductionEditorInlineWriteBegin(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	db := productionEditorInlineWriteDatabase(t, lane.projectPath)
	template, err := prompttemplate.EditSelectedTextTemplate(prompttemplate.LanguageChinese)
	if err != nil {
		t.Fatalf("%v", err)
	}
	request := productionEditorInlineWriteBeginRequest(productionEditorInlineWriteBeginNonce, "refine")
	answer := productionEditorInlineWriteAnswer(t, "begin", lane.author, productionEditorInlineBeginName, request, lane.frozen)

	// The four fields of EditorInlineRecovery, in the reference's own terms.
	if got := syntheticString(answer["sourceStatus"]); got != "current" {
		t.Fatalf("begin answered sourceStatus %v; a run this lane just froze under the live lease must reproduce its own binding", got)
	}
	if got := syntheticString(answer["modelId"]); got != productionTestModelID {
		t.Fatalf("begin answered modelId %v, want %v", got, productionTestModelID)
	}
	view := syntheticMap(answer["view"])
	if view == nil {
		t.Fatalf("begin answered no view: %v", answer)
	}
	if got := syntheticString(view["operation"]); got != productionEditorInlineOperation {
		t.Fatalf("begin projected operation %v", got)
	}
	if got := syntheticString(view["status"]); got != "running" {
		t.Fatalf("begin projected status %v, want running", got)
	}
	handle := syntheticMap(view["handle"])
	runID := syntheticString(handle["runId"])
	rootID := syntheticString(handle["rootActionId"])
	if runID == "" || rootID == "" {
		t.Fatalf("begin projected no run identity: %v", handle)
	}
	if got := syntheticString(handle["epoch"]); got != syntheticString(lane.frozen["leaseId"]) {
		t.Fatalf("begin projected epoch %v, want the frozen lease the run was opened under", got)
	}

	// The context is the reference's, field by field.
	contextValue := syntheticMap(answer["context"])
	if contextValue == nil {
		t.Fatalf("begin answered no context: %v", answer)
	}
	if got := syntheticString(contextValue["kind"]); got != "author-draft" {
		t.Fatalf("the frozen context carries kind %v", got)
	}
	if got := syntheticString(contextValue["action"]); got != "refine" {
		t.Fatalf("the frozen context carries action %v", got)
	}
	if got := syntheticString(contextValue["documentText"]); got != productionEditorInlineWriteDocument {
		t.Fatalf("the frozen context carries another document: %v", got)
	}
	if got := syntheticString(contextValue["selectedText"]); got != productionEditorInlineWriteSelected {
		t.Fatalf("the frozen context carries selection %v, want %v", got, productionEditorInlineWriteSelected)
	}
	if got, ok := contextValue["from"].(float64); !ok || got != productionEditorInlineWriteFrom {
		t.Fatalf("the frozen context carries from %v", contextValue["from"])
	}
	if got, ok := contextValue["to"].(float64); !ok || got != productionEditorInlineWriteTo {
		t.Fatalf("the frozen context carries to %v", contextValue["to"])
	}
	if got := syntheticString(contextValue["documentHash"]); got != productionEditorInlineDigest(productionEditorInlineWriteDocument) {
		t.Fatalf("the frozen context carries documentHash %v", got)
	}
	if got := syntheticString(contextValue["writingLanguage"]); got != prompttemplate.LanguageChinese {
		t.Fatalf("the frozen context carries writingLanguage %v", got)
	}

	// The context the database holds is the text the reference hashes, and its
	// template member is the shipped asset byte for byte - not a Go re-encoding.
	storedContext := productionEditorInlineWriteStored(t, db, runID, "editorInlineContext")
	if !strings.Contains(storedContext, template.Text) {
		t.Fatalf("the frozen context does not carry the shipped template text: %s", storedContext)
	}
	if got := productionEditorInlineWriteStored(t, db, runID, "editorInlineContextHash"); got != `"`+productionEditorInlineDigest(storedContext)+`"` {
		t.Fatalf("the frozen context digest is %v, not the digest of the stored context", got)
	}
	// No re-serialization: the document's < and & are raw in the stored binding, and
	// the members are in the order the builder wrote them.
	if !strings.Contains(storedContext, productionEditorInlineWriteDocument) {
		t.Fatalf("the stored context does not carry the document verbatim: %s", storedContext)
	}
	if strings.Contains(storedContext, `\u003c`) || strings.Contains(storedContext, `\u0026`) {
		t.Fatalf("the stored context was HTML-escaped, so its digest is not a digest of the author's document: %s", storedContext)
	}
	if !strings.HasPrefix(storedContext, `{"action":"refine","documentText":`) {
		t.Fatalf("the stored context is not in the frozen member order: %s", storedContext)
	}
	// The shipped template is not written in Go: the body line that names the author's
	// selection is taken from the asset, and no Go file in this package carries it.
	lines := strings.Split(template.Content, "\n")
	if len(lines) < 4 || lines[3] == "" {
		t.Fatal("the shipped template body no longer has the section line this scan keys on")
	}
	marker := lines[3]
	for _, source := range productionEditorInlineWriteSources(t) {
		if productionSeamContains(source.body, marker) {
			t.Fatalf("%v writes the shipped template body in Go; it must come from the committed asset", source.name)
		}
	}

	// The frozen input is the renderer's projection, and the task is the shipped one.
	storedInput := productionEditorInlineWriteStored(t, db, runID, "editorInlineInput")
	if !strings.Contains(storedInput, `"action":"refine"`) ||
		!strings.Contains(storedInput, productionEditorInlineWriteSelected) ||
		strings.Contains(storedInput, "instruction") {
		t.Fatalf("the frozen input is not the projection of a fixed action: %s", storedInput)
	}
	storedTask := productionEditorInlineWriteStored(t, db, runID, "editorInlineTask")
	if got := productionEditorInlineWriteStored(t, db, runID, "editorInlineTaskHash"); got != `"`+productionEditorInlineDigest(storedTask)+`"` {
		t.Fatalf("the frozen task digest is %v, not the digest of the stored task", got)
	}
	var task struct {
		Purpose  string `json:"purpose"`
		Stage    string `json:"reasoningStage"`
		Output   string `json:"output"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(storedTask), &task); err != nil {
		t.Fatalf("the frozen task is not JSON: %v %s", err, storedTask)
	}
	if task.Purpose != "editor-ai-refine" || task.Stage != "review" || task.Output != "visible-text" || len(task.Messages) != 2 {
		t.Fatalf("the frozen task is not the reference's: %+v", task)
	}
	if task.Messages[0].Role != "system" || !strings.Contains(task.Messages[0].Content, template.SystemRole) ||
		!strings.Contains(task.Messages[0].Content, productionEditorInlineWriteContract(t, prompttemplate.LanguageChinese)) {
		t.Fatalf("the frozen system message is not the composed role: %s", task.Messages[0].Content)
	}
	if task.Messages[1].Role != "user" || !strings.Contains(task.Messages[1].Content, productionEditorInlineWriteSelected) ||
		!strings.Contains(task.Messages[1].Content, template.SystemSuffix) || strings.Contains(task.Messages[1].Content, "{{") {
		t.Fatalf("the frozen user message is not the rendered body: %s", task.Messages[1].Content)
	}

	// The run was opened by the ledger's own transaction: one root, one run.
	if got := productionEditorInlineWriteCount(t, db, `SELECT COUNT(*) FROM generation_runs WHERE run_id=?`, runID); got != 1 {
		t.Fatalf("begin wrote %d run rows", got)
	}
	if got := productionEditorInlineWriteCount(t, db, `SELECT COUNT(*) FROM generation_roots WHERE root_action_id=?`, rootID); got != 1 {
		t.Fatalf("begin wrote %d root rows", got)
	}

	// The renderer's own open() replays a retried begin: same nonce, same input, and
	// the answer must be the same run, not a second one.
	replay := productionEditorInlineWriteAnswer(t, "replayed begin", lane.author, productionEditorInlineBeginName, request, lane.frozen)
	if got := syntheticString(syntheticMap(syntheticMap(replay["view"])["handle"])["runId"]); got != runID {
		t.Fatalf("a replayed begin answered run %v, want %v", got, runID)
	}
	// One nonce, one root: the root action row carries the nonce, and the replay must
	// not have added a second one.
	if got := productionEditorInlineWriteCount(t, db, `SELECT COUNT(*) FROM generation_roots WHERE action_json LIKE ?`,
		"%"+productionEditorInlineWriteBeginNonce+"%"); got != 1 {
		t.Fatalf("two begins under one nonce left %d root rows", got)
	}
	if got := productionEditorInlineWriteCount(t, db, `SELECT COUNT(*) FROM generation_runs WHERE root_action_id=?`, rootID); got != 1 {
		t.Fatalf("the run's root holds %d run rows", got)
	}
	// begin dispatches nothing: the committed origin never saw a connection.
	lane.assertNoDial(t)
}

// testProductionEditorInlineWriteBeginNonceConflict proves the nonce is an identity,
// not a cache key: the same nonce with a different frozen input is the reference's
// conflict, and it writes nothing.
func testProductionEditorInlineWriteBeginNonceConflict(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	request := productionEditorInlineWriteBeginRequest(productionEditorInlineWriteBeginNonce, "refine")
	productionEditorInlineWriteAnswer(t, "begin", lane.author, productionEditorInlineBeginName, request, lane.frozen)
	db := productionEditorInlineWriteDatabase(t, lane.projectPath)
	baseline := lane.bytes(t)
	// The same nonce, a different action: the input is not the input that nonce froze.
	moved := productionEditorInlineWriteBeginRequest(productionEditorInlineWriteBeginNonce, "expand")
	productionEditorInlineWriteRefusal(t, "begin replay with another input", lane.author,
		productionEditorInlineBeginName, moved, lane.frozen, productionEditorInlineNonceConflict)
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused begin replay changed the committed database file")
	}
	// The instruct request is the one shape whose frozen input carries an instruction,
	// and it opens a run of its own.
	instruct := productionEditorInlineWriteInstructRequest("production-editor-inline-write-instruct-1", productionEditorInlineWriteInstructNote)
	answer := productionEditorInlineWriteAnswer(t, "instruct begin", lane.author, productionEditorInlineBeginName, instruct, lane.frozen)
	runID := syntheticString(syntheticMap(syntheticMap(answer["view"])["handle"])["runId"])
	contextValue := syntheticMap(answer["context"])
	if syntheticString(contextValue["action"]) != "instruct" ||
		syntheticString(contextValue["instruction"]) != productionEditorInlineWriteInstructNote {
		t.Fatalf("the instruct context lost the author note: %v", contextValue)
	}
	storedIntent := productionEditorInlineWriteStored(t, db, runID, "editorInlineTask")
	var task struct {
		Purpose  string `json:"purpose"`
		Stage    string `json:"reasoningStage"`
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(storedIntent), &task); err != nil {
		t.Fatalf("%v", err)
	}
	if task.Purpose != "editor-ai-instruct" || task.Stage != "drafting" {
		t.Fatalf("the instruct task is not the reference's: %+v", task)
	}
	if !strings.Contains(task.Messages[0].Content, productionEditorInlineWriteRule(t, prompttemplate.LanguageChinese)) ||
		!strings.Contains(task.Messages[1].Content, productionEditorInlineWriteInstructNote) {
		t.Fatalf("the instruct task lost the author note or its precedence rule: %s", storedIntent)
	}
	lane.assertNoDial(t)
}

// testProductionEditorInlineWriteExecute is the dispatch half: the frozen task goes
// through the one lease and executor, the attempt is reserved and marked dispatched
// before the provider is called and settled after it, and a replay of the same handle
// answers that same settled attempt instead of spending a second request.
func testProductionEditorInlineWriteExecute(t *testing.T) {
	seam := &productionEditorInlineWriteSeam{text: productionEditorInlineWriteGenerated}
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)
	db := productionEditorInlineWriteDatabase(t, lane.projectPath)
	ledger := store.NewGenerationLedger(db, nil, nil)

	begin := productionEditorInlineWriteAnswer(t, "begin", lane.author, productionEditorInlineBeginName,
		productionEditorInlineWriteBeginRequest(productionEditorInlineWriteBeginNonce, "refine"), lane.frozen)
	handle := syntheticMap(syntheticMap(begin["view"])["handle"])
	runID := syntheticString(handle["runId"])
	rootID := syntheticString(handle["rootActionId"])
	seam.watch(ledger, rootID)

	answer := productionEditorInlineWriteAnswer(t, "execute", lane.author, productionEditorInlineExecuteName,
		map[string]any{"handle": handle}, lane.frozen)
	outcome := syntheticMap(answer["outcome"])
	if outcome == nil || syntheticString(outcome["status"]) != "completed" ||
		syntheticString(outcome["finishReason"]) != "stop" ||
		syntheticString(outcome["content"]) != productionEditorInlineWriteGenerated {
		t.Fatalf("execute answered %v", answer["outcome"])
	}
	if seam.callCount() != 1 {
		t.Fatalf("the run dispatched %d times through the seam; want exactly 1", seam.callCount())
	}

	// The transaction arm: while the probe was in flight the ledger already held one
	// attempt row, marked dispatched, and it is the row that settled below.
	status, attemptID, watched, watchErr := seam.atDispatch()
	if watchErr != nil {
		t.Fatalf("the dispatch could not read the ledger it was dispatched for: %v", watchErr)
	}
	if status != "dispatch-marked" || attemptID == "" {
		t.Fatalf("the attempt was %v at dispatch (%v), want one row marked dispatched", status, watched)
	}

	run := syntheticMap(answer["run"])
	if run == nil || syntheticString(run["operation"]) != productionEditorInlineOperation || syntheticString(run["status"]) != "running" {
		t.Fatalf("execute projected the wrong run: %v", run)
	}
	diagnostics := syntheticSlice(run["budgetDiagnostics"])
	if len(diagnostics) != 1 {
		t.Fatalf("execute reported %d attempt diagnostics, want the one it dispatched: %v", len(diagnostics), diagnostics)
	}
	attempt := syntheticMap(diagnostics[0])
	if syntheticString(attempt["attemptId"]) != attemptID {
		t.Fatalf("the settled attempt %v is not the attempt the dispatch saw %v", attempt["attemptId"], attemptID)
	}
	if syntheticString(attempt["actualState"]) != "settled" || attempt["failureCode"] != nil ||
		syntheticString(attempt["finishReason"]) != "stop" {
		t.Fatalf("the dispatched attempt did not settle: %v", attempt)
	}
	if syntheticNumber(attempt["requestedOutputTokens"]) == 0 || syntheticNumber(attempt["reservedTokens"]) == 0 {
		t.Fatalf("the settled attempt reserved nothing: %v", attempt)
	}

	// The settled row, read back through the ledger: one attempt for the run, the
	// inline task's own purpose, and the text the stub answered.
	budgetValue, err := ledger.Call(context.Background(), "budget", []any{rootID})
	if err != nil {
		t.Fatalf("ledger budget: %v", err)
	}
	attempts := syntheticSlice(syntheticMap(budgetValue)["attempts"])
	if len(attempts) != 1 {
		t.Fatalf("the run holds %d attempt rows; want one", len(attempts))
	}
	settled := syntheticMap(attempts[0])
	if syntheticString(settled["status"]) != "settled" || syntheticString(settled["attemptId"]) != attemptID {
		t.Fatalf("the committed attempt row is %v", settled)
	}
	if purpose := productionEditorInlineWriteAttemptPurpose(t, db, runID); purpose != "editor-ai-refine" {
		t.Fatalf("the dispatched attempt carries purpose %v, so it was not the frozen inline task", purpose)
	}
	receipt, err := ledger.Call(context.Background(), "receipt", []any{attemptID})
	if err != nil {
		t.Fatalf("ledger receipt: %v", err)
	}
	artifact := syntheticMap(syntheticMap(receipt)["artifact"])
	if artifact == nil || syntheticString(artifact["text"]) != productionEditorInlineWriteGenerated {
		t.Fatalf("the settled artifact is not the dispatched text: %v", artifact)
	}

	// A replay of the same handle: same invocation nonce, same frozen task, so the
	// ledger answers the settled attempt and nothing is dispatched again.
	replay := productionEditorInlineWriteAnswer(t, "replayed execute", lane.author, productionEditorInlineExecuteName,
		map[string]any{"handle": handle}, lane.frozen)
	replayRun := syntheticMap(replay["run"])
	replayedAttempts := syntheticSlice(replayRun["budgetDiagnostics"])
	if len(replayedAttempts) != 1 || syntheticString(syntheticMap(replayedAttempts[0])["attemptId"]) != attemptID {
		t.Fatalf("a replayed execute answered another attempt: %v", replayRun["budgetDiagnostics"])
	}
	if seam.callCount() != 1 {
		t.Fatalf("a replayed execute dispatched %d times; want the one admitted dispatch", seam.callCount())
	}
	if got := productionEditorInlineWriteCount(t, db, `SELECT COUNT(*) FROM generation_attempts WHERE run_id=?`, runID); got != 1 {
		t.Fatalf("a replayed execute left %d attempt rows", got)
	}
	lane.assertNoDial(t)
}

// testProductionEditorInlineWriteRefusals proves both served names are behind the
// same fences and answer them identically: a foreign project context and a lease the
// server never issued are refused byte for byte by begin and by execute, a handle
// naming another project is the family identity mismatch, a run of another family is
// the context refusal, a malformed begin request is the input refusal, and none of
// them writes a byte or dispatches.
func testProductionEditorInlineWriteRefusals(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	db := productionEditorInlineWriteDatabase(t, lane.projectPath)
	// A run of another family, opened by the lane's own generation:begin, so the
	// refusal below is the manifest's operation and not a missing row.
	foreignHandle := lane.begin(t, productionEditorBeginRequest(productionEditorInlineWriteForeignBegin))
	baseline := lane.bytes(t)

	beginRequest := productionEditorInlineWriteBeginRequest(productionEditorInlineWriteOtherBegin, "refine")
	executeRequest := map[string]any{"handle": foreignHandle}
	foreignProject := map[string]any{
		"projectId":   productionEditorInlineWriteOtherProjectID,
		"projectPath": lane.projectPath,
		"leaseId":     lane.frozen["leaseId"],
	}
	for _, c := range []struct {
		label  string
		frozen map[string]any
	}{
		{"a foreign project", foreignProject},
		{"a lease the server never issued", lane.stale()},
	} {
		beginBody, beginCode := productionEditorInlineWriteSessionRefusal(t, "begin under "+c.label,
			lane.author.call(t, productionEditorInlineBeginName, []any{beginRequest, c.frozen}))
		executeBody, executeCode := productionEditorInlineWriteSessionRefusal(t, "execute under "+c.label,
			lane.author.call(t, productionEditorInlineExecuteName, []any{executeRequest, c.frozen}))
		if beginBody != executeBody || beginCode != executeCode {
			t.Fatalf("%v: begin and execute answered different refusals: %s / %s", c.label, beginBody, executeBody)
		}
	}

	// The family's own identity and context refusals, on an admitted session.
	movedProject := productionEditorInlineHandle(productionEditorInlineWriteOtherProjectID,
		syntheticString(foreignHandle["epoch"]), syntheticString(foreignHandle["rootActionId"]), syntheticString(foreignHandle["runId"]))
	productionEditorInlineWriteRefusal(t, "execute of a handle naming another project", lane.author,
		productionEditorInlineExecuteName, map[string]any{"handle": movedProject}, lane.frozen, productionEditorInlineIdentityMismatch)
	productionEditorInlineWriteRefusal(t, "execute of a run of another family", lane.author,
		productionEditorInlineExecuteName, executeRequest, lane.frozen, productionEditorInlineContextInvalid)
	missing := productionEditorInlineHandle(syntheticString(lane.frozen["projectId"]), syntheticString(lane.frozen["leaseId"]),
		"production-editor-inline-write-no-root", "production-editor-inline-write-no-run")
	productionEditorInlineWriteRefusal(t, "execute of a run the database does not hold", lane.author,
		productionEditorInlineExecuteName, map[string]any{"handle": missing}, lane.frozen, productionEditorInlineRunMissing)

	// A begin request of the wrong shape: one extra key, and an input whose selection
	// is not the document's slice.
	widened := productionEditorInlineWriteBeginRequest(productionEditorInlineWriteOtherBegin, "refine")
	widened["task"] = map[string]any{"purpose": "chapter-draft"}
	productionEditorInlineWriteRefusal(t, "begin with an extra request key", lane.author,
		productionEditorInlineBeginName, widened, lane.frozen, productionEditorInlineRequestInvalid)
	broken := productionEditorInlineWriteBeginRequest(productionEditorInlineWriteOtherBegin, "refine")
	syntheticMap(broken["input"])["from"] = productionEditorInlineWriteTo
	productionEditorInlineWriteRefusal(t, "begin with an inverted selection", lane.author,
		productionEditorInlineBeginName, broken, lane.frozen, productionEditorInlineRequestInvalid)
	// An execute request with no handle at all is the handle's own identity refusal.
	productionEditorInlineWriteRefusal(t, "execute with no handle", lane.author,
		productionEditorInlineExecuteName, map[string]any{}, lane.frozen, productionEditorInlineIdentityMismatch)

	// Nine refusals, no write and no dispatch.
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a refused editor-inline write changed the committed database file")
	}
	if got := productionEditorInlineWriteCount(t, db, `SELECT COUNT(*) FROM generation_attempts WHERE run_id=?`, syntheticString(foreignHandle["runId"])); got != 0 {
		t.Fatalf("a refused execute wrote %d attempt rows", got)
	}
	lane.assertNoDial(t)
}

// testProductionEditorInlineWriteComposition is the renderer's own arithmetic, checked
// directly: the quoting JSON.stringify would produce, the single-pass fill, the
// blank-line prune, and a frozen context that carries the shipped template text and a
// document digest computed independently.
func testProductionEditorInlineWriteComposition(t *testing.T) {
	for _, text := range []string{"", "plain", `quote " and \\ backslash`, "<a> & </b>", "line\n\n\n\nbreak", "月\u2028甲😀", "\x00\x1f"} {
		quoted := productionEditorInlineQuote(text)
		var round any
		if err := json.Unmarshal([]byte(quoted), &round); err != nil {
			t.Fatalf("the quoter produced invalid JSON for one text: %v", err)
		}
		if got, ok := round.(string); !ok || got != text {
			t.Fatalf("the quoter did not round-trip a text, it answered: %s", got)
		}
		// JSON.stringify never HTML-escapes and never escapes U+2028/U+2029.
		for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`, `\u2028`, `\u2029`} {
			if strings.Contains(quoted, escaped) {
				t.Fatalf("the quoter escaped %v, which JSON.stringify leaves literal: %s", escaped, quoted)
			}
		}
	}
	if got := productionEditorInlinePrune("\nalpha\n\n\n\nbeta\n\n"); got != "alpha\n\nbeta" {
		t.Fatalf("the prune answered %s", got)
	}
	if got := productionEditorInlineFill("{{a}}/{{b}}/{{missing}}", map[string]string{"a": "{{b}}", "b": "$&"}); got != "{{b}}/$&/{{missing}}" {
		t.Fatalf("the fill is not one literal pass, it answered: %s", got)
	}
	template, err := prompttemplate.EditSelectedTextTemplate(prompttemplate.LanguageChinese)
	if err != nil {
		t.Fatalf("%v", err)
	}
	input := productionEditorInlineInput{
		Action: "refine", DocumentText: productionEditorInlineWriteDocument,
		From: productionEditorInlineWriteFrom, To: productionEditorInlineWriteTo,
		SelectedText: productionEditorInlineWriteSelected,
	}
	contextText, err := productionEditorInlineContextText(input, prompttemplate.LanguageChinese, template.Text)
	if err != nil {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(contextText, template.Text) {
		t.Fatal("the frozen context text does not carry the shipped template text")
	}
	var decoded struct {
		DocumentHash string `json:"documentHash"`
		Template     struct {
			Key string `json:"key"`
		} `json:"template"`
	}
	if err := json.Unmarshal([]byte(contextText), &decoded); err != nil {
		t.Fatalf("%v", err)
	}
	if decoded.DocumentHash != productionEditorInlineDigest(productionEditorInlineWriteDocument) {
		t.Fatalf("the frozen context carries documentHash %v", decoded.DocumentHash)
	}
	if decoded.Template.Key != productionEditorInlinePromptKey {
		t.Fatalf("the frozen context carries template %v", decoded.Template.Key)
	}
}

// productionEditorInlineWriteSource is one Go file of this package.
type productionEditorInlineWriteSource struct {
	name string
	body string
}

// productionEditorInlineWriteSources reads every Go file of this package, so a test
// can assert that a shipped sentence is not restated in one of them.
func productionEditorInlineWriteSources(t *testing.T) []productionEditorInlineWriteSource {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("%v", err)
	}
	if len(paths) == 0 {
		t.Fatal("this package has no Go files, so the scan would prove nothing")
	}
	sources := make([]productionEditorInlineWriteSource, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v", err)
		}
		sources = append(sources, productionEditorInlineWriteSource{name: path, body: string(raw)})
	}
	return sources
}

// productionEditorInlineWriteContract is the shipped system contract for one writing
// language, read through the asset rather than restated here.
func productionEditorInlineWriteContract(t *testing.T, language string) string {
	t.Helper()
	edit, err := prompttemplate.EditSelectedTextEdit(language)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return edit.SystemContract
}

// productionEditorInlineWriteRule is the shipped author-note precedence rule.
func productionEditorInlineWriteRule(t *testing.T, language string) string {
	t.Helper()
	edit, err := prompttemplate.EditSelectedTextEdit(language)
	if err != nil {
		t.Fatalf("%v", err)
	}
	return edit.InstructRule
}

// productionEditorInlineWriteAttemptPurpose reads the purpose the reserve recorded
// for a run's one attempt, which is the purpose of the task the executor planned.
func productionEditorInlineWriteAttemptPurpose(t *testing.T, db *sql.DB, runID string) string {
	t.Helper()
	var usage string
	if err := db.QueryRow(`SELECT usage_receipt_json FROM generation_attempts WHERE run_id=?`, runID).Scan(&usage); err != nil {
		t.Fatalf("%v", err)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(usage), &value); err != nil {
		t.Fatalf("%v", err)
	}
	return syntheticString(value["purpose"])
}
