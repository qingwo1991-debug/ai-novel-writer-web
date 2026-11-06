//go:build linux

package main

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

const (
	// productionGraphGenerationRunNonce is the action nonce the one begun graph run
	// spends, so the same test always either finds its run or freezes a first one.
	productionGraphGenerationRunNonce = "graph-generation-served-plan"
	// productionGraphGenerationDraftNonce begins a second, non-graph run in the same
	// lane, so the kind check is asserted against a handle this session really owns
	// rather than against a made-up one.
	productionGraphGenerationDraftNonce = "graph-generation-served-draft"
)

// TestProductionLaneServesGraphGenerationRead is the positive half of this slice. It
// begins a plan run through the served graph-generation:begin, reads it back through
// graph-generation:read, and asserts the answer is the projection the renderer
// consumes (src/shared/graph-generation.ts:30-39): the begun handle, the plan
// operation, a running run with nothing in flight, the frozen model, an attempt count
// of zero, the plan source status the lane re-derives, and the empty effects list a run
// with no confirmation has.
//
// It also asserts the two properties a projection can silently get wrong. The input is
// answered in the browser's key order, because the renderer compares
// JSON.stringify(recovery.context.input) with the request it just sent
// (src/services/graph-generation.ts:56) and a stored map would answer number-first. And
// artifact/result stay absent rather than invented: execute is unbound, so nothing has
// produced output to qualify.
//
// The refusal arms follow: an unknown run, another project's handle, a run of another
// kind, a lease this session no longer holds, a request carrying a key the family does
// not define, and a call with no frozen context at all. Every one of them must be a
// symbolic refusal, and none of them may write.
func TestProductionLaneServesGraphGenerationRead(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	handle := productionGraphGenerationBegin(t, lane)
	// A second run of another kind, so the kind arm is a real handle of this session.
	draft := lane.begin(t, productionEditorBeginRequest(productionGraphGenerationDraftNonce))

	// The fixture seeds one settled run of its own, so the counts below are a
	// before/after comparison rather than an absolute: a read may add no row, and the
	// two begun runs are what a graph read has to see in front of it.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var runsBefore, attemptsBefore int
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_runs", nil, &runsBefore)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_attempts", nil, &attemptsBefore)
	before := productionMCPDatabaseDigest(t, lane.projectPath)

	response := lane.author.call(t, productionGraphGenerationRead, []any{map[string]any{"handle": handle}, lane.frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("%v = %d %s, want the served projection", productionGraphGenerationRead, response.Code, response.Body.String())
	}
	answer := productionGraphGenerationEnvelope(t, response.Body.Bytes())
	view := productionGraphGenerationMap(t, "view", answer["view"])
	served := productionGraphGenerationMap(t, "view.handle", view["handle"])
	for _, key := range []string{"runId", "rootActionId"} {
		if served[key] != handle[key] {
			t.Fatalf("view.handle carries %v for %s, want the begun run's %v", served[key], key, handle[key])
		}
	}
	if served["projectId"] != lane.frozen["projectId"] || served["epoch"] != lane.frozen["leaseId"] {
		t.Fatalf("view.handle does not name this session's binding: %v %v", served["projectId"], served["epoch"])
	}
	// The plan operation and the running status the begin wrote, unchanged by a read.
	if view["operation"] != "narrative-thread-plan-candidate" || view["status"] != "running" {
		t.Fatalf("view is %v/%v, want the begun plan run", view["operation"], view["status"])
	}
	// Nothing is in flight and the run is still replayable: a read reserves no attempt.
	if view["inFlight"] != false || view["nonReplayable"] != false {
		t.Fatalf("a read reported an in-flight or non-replayable run: %v %v", view["inFlight"], view["nonReplayable"])
	}
	// One model, frozen at begin, answered as the browser's modelId.
	if answer["modelId"] != productionTestModelID {
		t.Fatalf("modelId = %v, want the frozen model", answer["modelId"])
	}
	// Zero attempts, and the source status the lane can prove rather than assume: a plan
	// re-derives the blueprint, the chapter count and the writing language and compares
	// their digest with the frozen one.
	if answer["attemptCount"] != float64(0) {
		t.Fatalf("attemptCount = %v after a begin with no dispatch", answer["attemptCount"])
	}
	if answer["sourceStatus"] != "current" {
		t.Fatalf("sourceStatus = %v for an unchanged plan source", answer["sourceStatus"])
	}
	// A run with no confirmation has no effect, and the projection says so with an empty
	// list instead of omitting the key.
	effects, ok := answer["effects"].([]any)
	if !ok || len(effects) != 0 {
		t.Fatalf("effects = %v, want the empty list of a run with no confirmation", answer["effects"])
	}
	// The two fields only a qualified result can fill stay absent, which is what the
	// refusal of execute means for this answer.
	for _, key := range []string{"artifact", "result"} {
		if _, present := answer[key]; present {
			t.Fatalf("the read invented %s before any dispatch", key)
		}
	}
	// canExecuteGraph also requires an empty candidate and artifact list, which is what
	// a begun run with no attempt has.
	for _, key := range []string{"artifacts", "candidates"} {
		list, ok := view[key].([]any)
		if !ok || len(list) != 0 {
			t.Fatalf("view.%s = %v, want the empty list of a run with no attempt", key, view[key])
		}
	}
	contextValue := productionGraphGenerationMap(t, "context", answer["context"])
	if contextValue["kind"] != "plan" || contextValue["projectId"] != lane.frozen["projectId"] ||
		contextValue["originEpoch"] != lane.frozen["leaseId"] {
		t.Fatalf("the served context is not this session's plan context: %v %v %v",
			contextValue["kind"], contextValue["projectId"], contextValue["originEpoch"])
	}
	// The input is the browser's two-field document, validated structurally: the
	// renderer checks context.kind against context.input.kind and each handle field,
	// never the key order (src/services/graph-generation.ts:27). This lane cannot
	// promise an order here in any case - the public response boundary re-encodes every
	// struct as an object, so a served document reads lexically - which is why the
	// projection keeps building the input in the browser's field order for callers that
	// encode it directly (TestProductionGraphGenerationInputOrder) rather than for the
	// wire.
	input, ok := contextValue["input"].(map[string]any)
	if !ok || len(input) != 2 || input["kind"] != "plan" || input["chapterNumber"] != float64(1) {
		t.Fatalf("the served context input is not the browser's plan document: %v", contextValue["input"])
	}
	if contextValue["totalChapters"] != float64(12) {
		t.Fatalf("the served context dropped the re-derived chapter count: %v", contextValue["totalChapters"])
	}

	// An unknown run is refused as a missing row rather than projected.
	productionGraphGenerationRefusal(t, lane, "an unknown run", productionGraphGenerationRead, []any{map[string]any{
		"handle": map[string]any{
			"projectId":    lane.frozen["projectId"],
			"epoch":        lane.frozen["leaseId"],
			"rootActionId": "graph-generation-absent-root",
			"runId":        "graph-generation-absent-run",
		},
	}, lane.frozen}, "GENERATION_RUN_NOT_FOUND")
	// Another project's handle carries this session's lease and real run ids, and is
	// still refused: the binding, not the id alone, is what the read trusts.
	productionGraphGenerationRefusal(t, lane, "another project's handle", productionGraphGenerationRead, []any{map[string]any{
		"handle": map[string]any{
			"projectId":    productionGraphReadsForeignProjectID,
			"epoch":        lane.frozen["leaseId"],
			"rootActionId": handle["rootActionId"],
			"runId":        handle["runId"],
		},
	}, lane.frozen}, "GENERATION_RUN_NOT_FOUND")
	// A run of another kind has no graph context to project, and is refused with the
	// context code rather than answered as a plan.
	productionGraphGenerationRefusal(t, lane, "a draft run", productionGraphGenerationRead,
		[]any{map[string]any{"handle": draft}, lane.frozen}, "GENERATION_GRAPH_CONTEXT_INVALID")
	// A lease this session no longer holds never reaches the projection.
	productionEditorRefused(t, lane.author, "stale lease", productionGraphGenerationRead,
		[]any{map[string]any{"handle": handle}, lane.stale()})
	// The family request is `{ handle }` and nothing else.
	productionGraphGenerationRefusal(t, lane, "an extra request key", productionGraphGenerationRead,
		[]any{map[string]any{"handle": handle, "artifact": nil}, lane.frozen}, "GENERATION_GRAPH_REQUEST_INVALID")
	// And the frozen context is not optional: without it this is not the family's call.
	productionGraphGenerationRefusal(t, lane, "a missing frozen context", productionGraphGenerationRead,
		[]any{map[string]any{"handle": handle}}, "INVALID_ARGUMENTS")

	// A read writes nothing: not a byte of the file, not a row.
	after := productionMCPDatabaseDigest(t, lane.projectPath)
	if before != after {
		t.Fatal("a graph-generation read moved a byte of the author database")
	}
	var runsAfter, attemptsAfter int
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_runs", nil, &runsAfter)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_attempts", nil, &attemptsAfter)
	if runsAfter != runsBefore || attemptsAfter != attemptsBefore {
		t.Fatalf("a read added rows: runs %d, attempts %d", runsAfter, attemptsAfter)
	}
	// No attempt means no dispatch, and the lane's origin listener proves it.
	lane.assertNoDial(t)
}

// TestProductionLaneRefusesGraphGenerationExecuteAndConfirm pins the two names this
// slice leaves unbound at the registry's fail-closed 501, over the same live-lease plan
// run the read test serves. The run exists, so the refusals are asserted against a
// population a dispatch or a confirmation would really have, not against an empty
// database - and neither name may write, dispatch or add a row.
func TestProductionLaneRefusesGraphGenerationExecuteAndConfirm(t *testing.T) {
	lane := productionGenerationWriteOpen(t, nil, nil)
	handle := productionGraphGenerationBegin(t, lane)
	artifact := map[string]any{"artifactId": "graph-generation-artifact-0", "revision": float64(1),
		"textHash": strings.Repeat("a", 64)}

	// The fixture seeds a settled run of its own, so the count is a before/after
	// comparison: a refused name may add no row at all.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var runsBefore int
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_runs", nil, &runsBefore)
	before := productionMCPDatabaseDigest(t, lane.projectPath)
	productionGraphGenerationUnbound(t, lane.author, productionGraphGenerationExecute,
		[]any{map[string]any{"handle": handle}, lane.frozen})
	productionGraphGenerationUnbound(t, lane.author, productionGraphGenerationConfirm,
		[]any{map[string]any{"handle": handle, "artifact": artifact, "index": float64(0)}, lane.frozen})
	after := productionMCPDatabaseDigest(t, lane.projectPath)
	if before != after {
		t.Fatal("a refused graph-generation name moved a byte of the author database")
	}
	var runs, attempts int
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_runs", nil, &runs)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_attempts", nil, &attempts)
	if runs != runsBefore || attempts != 0 {
		t.Fatalf("a refused name added rows: runs %d, attempts %d", runs, attempts)
	}
	lane.assertNoDial(t)
}

// productionGraphGenerationBegin opens the one graph run this file reads back, through
// the name this lane already serves, and returns its browser handle.
func productionGraphGenerationBegin(t *testing.T, lane *productionGenerationWriteLane) map[string]any {
	t.Helper()
	request := map[string]any{
		"input":         map[string]any{"kind": "plan", "chapterNumber": float64(1)},
		"modelId":       productionTestModelID,
		"uiActionNonce": productionGraphGenerationRunNonce,
	}
	result := productionStoryStateWriteCall(t, lane.author, "graph-generation:begin", []any{request, lane.frozen})
	view, ok := result["view"].(map[string]any)
	if !ok {
		t.Fatalf("graph-generation:begin returned no view: %v", result)
	}
	handle, ok := view["handle"].(map[string]any)
	if !ok || syntheticString(handle["runId"]) == "" || syntheticString(handle["rootActionId"]) == "" {
		t.Fatalf("graph-generation:begin returned no run handle: %v", result)
	}
	contextValue, ok := result["context"].(map[string]any)
	if !ok || syntheticString(contextValue["kind"]) != "plan" {
		t.Fatalf("graph-generation:begin froze no plan context: %v", result)
	}
	return handle
}

// productionGraphGenerationEnvelope unwraps the frozen session's result envelope.
func productionGraphGenerationEnvelope(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(body, &wire); err != nil || wire.Result == nil {
		t.Fatalf("the answer carries no result: %v", err)
	}
	return wire.Result
}

// productionGraphGenerationMap narrows one map field, naming it on failure so a shape
// change is reported where it happened instead of as a nil dereference.
func productionGraphGenerationMap(t *testing.T, label string, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %v", label, value)
	}
	return object
}

// productionGraphGenerationRefused asserts one arm of the read is refused with the
// symbolic code the lane documents. A 501 is reported as its own failure, because that
// would mean the name went back to being unbound.
func productionGraphGenerationRefusal(t *testing.T, lane *productionGenerationWriteLane, label, operation string, args []any, code string) {
	t.Helper()
	response := lane.author.call(t, operation, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s: %v is unbound: %s", label, operation, response.Body.String())
	}
	if response.Code == http.StatusOK {
		t.Fatalf("%s: %v answered a projection: %s", label, operation, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	if body.Error.Code != code {
		t.Fatalf("%s: %v refused with %s, want %s: %s", label, operation, body.Error.Code, code, response.Body.String())
	}
}

// productionGraphGenerationUnbound asserts one name still answers the registry's
// fail-closed 501, which is what an inventoried but unbound operation must do.
func productionGraphGenerationUnbound(t *testing.T, client *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%v = %d %s, want 501 GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
	}
}

// TestProductionGraphGenerationInputOrder pins the shape of the projection's input,
// because the two documents involved are ordered differently: the frozen context stores
// its input as a Go map, which json.Marshal sorts, so the stored document reads
// chapterNumber first, while the browser's own document reads kind first
// (src/services/narrative-thread-candidate-generator.ts:30). graphRecovery therefore
// re-projects the input through a struct whose fields are declared in the browser's
// order, and this test proves both orders rather than remembering them.
//
// The order does not survive the public response boundary, which re-encodes every
// struct as an object and so hands the browser a lexically ordered document. That is
// why the served read is asserted structurally: the renderer validates the input by its
// fields (src/services/graph-generation.ts:27), and the field order only reaches a
// caller that encodes the projection directly.
func TestProductionGraphGenerationInputOrder(t *testing.T) {
	frozen := map[string]any{"kind": "plan", "chapterNumber": float64(1)}
	sorted, err := json.Marshal(frozen)
	if err != nil {
		t.Fatal(err)
	}
	if string(sorted) != `{"chapterNumber":1,"kind":"plan"}` {
		t.Fatalf("a stored input map no longer sorts number-first: %s", sorted)
	}
	// syntheticGraphContextInput projects a whole frozen context, so the input has to
	// sit under its own key exactly as the ledger stores it.
	projected, err := json.Marshal(syntheticGraphContextInput(map[string]any{"input": frozen}))
	if err != nil {
		t.Fatal(err)
	}
	if string(projected) != `{"kind":"plan","chapterNumber":1}` {
		t.Fatalf("the browser-order projection is no longer kind-first: %s", projected)
	}
}

// TestProductionGraphGenerationSeams re-reads the code every served and seam row points
// at. The marker is searched for across the whole file rather than only at the recorded
// line: a row that is still documented but has been shifted by an unrelated edit is not
// a regression, while a row that was deleted is. The recorded line is still reported, so
// a reader finds it quickly.
func TestProductionGraphGenerationSeams(t *testing.T) {
	served := productionGraphGenerationServed()
	if len(served) == 0 {
		t.Fatal("the served table documents nothing")
	}
	productionGraphGenerationRows(t, productionGraphGenerationRead, served)

	refused := productionGraphGenerationRefused()
	seen := make(map[string]bool, len(refused))
	for _, operation := range refused {
		if seen[operation] {
			t.Fatalf("%v is listed twice", operation)
		}
		seen[operation] = true
		seam, found := productionGraphGenerationSeamFor(operation)
		if !found || len(seam.Missing) == 0 {
			t.Fatalf("%v has no documented seam", operation)
		}
		productionGraphGenerationRows(t, operation, seam.Missing)
	}
	// The two families do not overlap: the name this file serves is not also refused,
	// and a name another lane serves is not claimed here.
	for _, operation := range refused {
		if operation == productionGraphGenerationRead {
			t.Fatal("the served name is also listed as refused")
		}
	}
	if _, found := productionGraphGenerationSeamFor("finalization-generation:read"); found {
		t.Fatal("the seam table claims a name another lane serves")
	}
	// The binder installs nothing without a runtime and cannot be wired to a nil server.
	if err := newProductionGraphGeneration(nil).Bind(nil); err == nil || err.Error() != "TRANSPORT_SERVER_REQUIRED" {
		t.Fatalf("the binder accepted a nil server: %v", err)
	}
}

// productionGraphGenerationRows checks one table against the files it quotes.
func productionGraphGenerationRows(t *testing.T, operation string, rows []productionGraphGenerationLine) {
	t.Helper()
	for _, line := range rows {
		if line.Line < 1 || line.Marker == "" {
			t.Fatalf("%v has an incomplete row for %v", operation, line.Path)
		}
		raw, err := os.ReadFile(line.Path)
		if err != nil {
			t.Fatalf("%v: %v: %v", operation, line.Path, err)
		}
		if !strings.Contains(string(raw), line.Marker) {
			if line.Note != "" {
				t.Fatalf("%v no longer documents %v anywhere in %v (recorded at line %d): %v",
					operation, line.Marker, line.Path, line.Line, line.Note)
			}
			t.Fatalf("%v no longer documents %v anywhere in %v (recorded at line %d)",
				operation, line.Marker, line.Path, line.Line)
		}
	}
}
