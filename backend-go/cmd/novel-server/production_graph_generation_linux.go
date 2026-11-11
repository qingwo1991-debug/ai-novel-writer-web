//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's graph-generation read slice. Of the family's
// five names (electron/controllers/generation-controller.ts:228-232) it binds one;
// the other four are served elsewhere.
//
//	graph-generation:read      BOUND, READ, wire arity 2
//	graph-generation:execute   served by production_graph_generation_execute_linux.go
//	graph-generation:confirm   served by production_graph_generation_execute_linux.go
//
// Two more are served elsewhere: begin freezes the context beside the draft
// admission (production_generation_begin_linux.go:65) and cancel stops the graph
// root through the ordinary stop path (production_generation_write_linux.go:146).
// finalization-generation:read is the sibling slice
// (production_graph_reads_linux.go:14), not this file. Wire types are the browser's
// own (src/shared/graph-generation.ts:40-45): read and execute take `{ handle }`,
// confirm takes `{ handle, artifact, index }`.
//
// # Served: read
//
// A graph recovery is a projection of rows the shipped ledger already wrote, and the
// projection exists: syntheticGeneration.GraphRead (synthetic_graph_linux.go:19)
// re-admits the frozen three-key session, requires the run the handle names, and then
// graphRecovery (:56) re-proves the frozen context digest, the input frozen twice
// (inside the context and beside it), the origin epoch, the task and its digest before
// it answers a single field. It reserves no attempt, resolves no credential, opens no
// egress endpoint and dispatches to no provider - the same property that lets the two
// committed-state generation reads live in this lane's read slice
// (production_generation_reads_linux.go:8-17).
//
// The begin that freezes such a context is served
// (production_generation_begin_linux.go:65), and it is the population this read is owed:
// a plan run begun through graph-generation:begin in the same session, under the same
// live lease.
//
// That producer cannot open a run today, and it is not this file's defect. The frozen
// selection begin mints carries the prompt key `graph-plan`, and the source binding's
// prompt grammar (internal/generationowner/binding.go:35) admits only
// `[a-z][a-z0-9_]{0,63}` before the language suffix, so the hyphen is refused as
// GENERATION_SOURCE_UNSUPPORTED before any row is written. The original admits it -
// electron/services/generation-source-binding.ts:306 allows `-` in a prompt key, and
// :292 requires exactly `graph-${kind}` - and this lane's own prompt reader already
// accepts the three graph keys (synthetic_generation_linux.go:84), so the narrowed
// pattern is the deviation, and widening its alternation to admit
// `graph-(plot|plan|event)` is the fix that unblocks this begin - it is the first
// refusal, so treat it as the one to land before looking for a second. Until it lands
// this read has no run to answer, and its test fails on that gate's code rather than
// passing over a producer that does not exist.
//
// The answer is GraphGenerationRecovery - the run view with its
// handle, status, budget and diagnostics; the model id; the validated context with its
// input re-projected to the browser's fields; the attempt count; and the source status,
// which for a plan re-derives the blueprint, the total chapter count and the writing
// language and compares their digest with the frozen one instead of assuming they did
// not move.
//
// The handle is fenced before the lane resolves it: exactly four identity keys, the
// caller's project and the epoch the session holds, or the answer is
// GENERATION_RUN_IDENTITY_MISMATCH (productionGraphGenerationIdentity, mirroring
// electron/services/graph-generation.ts:25-30). This is not the isolation itself - the
// ledger the lane opens is the caller's own database, so another project's rows are
// unreachable by id in the first place - but a handle is the browser's, and without this
// fence one naming another book still resolves by run id inside this one and comes back
// projected - the renderer would refuse that answer client-side
// (src/services/graph-generation.ts:47-51), but only after the server had already
// projected a run for a handle it never issued. The fence
// runs here, before the lane re-admits the session, so a caller whose lease is stale and
// whose handle is its own still gets the session refusal: that is the only handle such a
// client holds, and the original asserts the session before the handle
// (electron/services/main-generation-owner.ts:1206-1210).
//
// The projection still omits two browser fields, and both stay omitted for the reason
// its own documentation gives (synthetic_graph_linux.go:41-50): effects can only exist
// once a confirmation is bound, and artifact/result can only be projected by qualifying
// the stored output against the graph reference, which no port in this lane does. So a
// begun run answers an empty effects list and no artifact or result - the truth about a
// run that has produced no output yet - rather than inventing either.
//
// The sibling graph read slice lends only the finalization slot read and leaves this
// name alone (production_graph_reads_linux.go:19-29), on the grounds that a resumed plan
// run is answered by begin's own replay. That covers the resume path, not the reload
// path: a client that already holds a handle and asks again after a crash or a remount
// calls this read first (src/services/graph-generation.ts:35), so the handle projection
// belongs to a binder. The same slice used to pin this name at 501 in its fixture, and
// that pin is retired with the begin that falsified it
// (production_graph_reads_linux_test.go:58-76).
//
// # What a graph run already reuses
//
// Almost all of it. A graph run begun in this lane goes through the same admission
// as any other write (syntheticGeneration.admit), the same single ledger open and
// budget (openRun, then the ledger's own open/budget methods), the same frozen binding,
// and it freezes beside that binding the whole graph state - the context, its digest,
// the input, the key, the origin epoch, the task and the task digest
// (synthetic_generation_linux.go:1155-1163). A dispatch goes through the same one
// generationowner.Executor and the one OrdinaryService, because
// syntheticGeneration.Execute (:1297) is the single model path in this lane.
//
// The two ends of a run - execute and confirm - are now served by
// production_graph_generation_execute_linux.go: execute dispatches the frozen graph
// task through that same lease and executor, and confirm re-proves the artifact,
// derives the plan candidates, writes the chosen plan and stamps the graph effect in
// one BEGIN IMMEDIATE transaction (internal/store/graph_generation_confirm.go). The
// seam that once refused both names was retired with them.

const (
	// productionGraphGenerationRead is the one name this file binds. The wire request
	// is `{ handle }` and the answer is GraphGenerationRecovery
	// (src/shared/graph-generation.ts:42).
	productionGraphGenerationRead = "graph-generation:read"
	// productionGraphGenerationExecute is served by
	// production_graph_generation_execute_linux.go: the one model dispatch of the
	// frozen graph task.
	productionGraphGenerationExecute = "graph-generation:execute"
	// productionGraphGenerationConfirm is served by
	// production_graph_generation_execute_linux.go: the one transaction that writes the
	// plan and stamps the effect.
	productionGraphGenerationConfirm = "graph-generation:confirm"
)

// productionGraphGeneration is the graph reader. Of syntheticGeneration's fields only
// lifecycle is set, because a handle read touches nothing else.
type productionGraphGeneration struct {
	read func(context.Context, transport.Caller, map[string]any, any) (any, error)
}

// newProductionGraphGeneration builds the reader from the lane's durable open. A nil or
// closed runtime lends nothing, Bind then installs nothing, and the name keeps failing
// closed.
func newProductionGraphGeneration(durable *productionDurableOpen) *productionGraphGeneration {
	if durable == nil {
		return &productionGraphGeneration{}
	}
	durable.mu.Lock()
	lifecycle := durable.lifecycle
	closed := durable.closed
	durable.mu.Unlock()
	if closed || lifecycle == nil {
		return &productionGraphGeneration{}
	}
	reads := &syntheticGeneration{lifecycle: lifecycle, leaseOwner: productionLeaseOwner}
	return &productionGraphGeneration{read: reads.GraphRead}
}

// Bind installs graph-generation:read and nothing else. The seam check runs first, so a
// refusal table that drifted from the controller fails startup instead of quietly
// leaving a refused name unaccounted for.
func (g *productionGraphGeneration) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, operation := range productionGraphGenerationRefused() {
		seam, found := productionGraphGenerationSeamFor(operation)
		if !found || len(seam.Missing) == 0 {
			return errors.New("PRODUCTION_GRAPH_GENERATION_SEAM_INCOMPLETE")
		}
	}
	if g == nil || g.read == nil {
		return nil
	}
	read := g.read
	return server.Bind(productionGraphGenerationRead, func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		// Wire arity is the request arity plus the frozen expected context.
		if len(args) != 2 {
			return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		raw, ok := args[1].(map[string]any)
		if !ok || len(raw) != 3 {
			return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
		}
		expected := make(map[string]any, 3)
		for _, key := range []string{"projectId", "leaseId", "projectPath"} {
			text, ok := raw[key].(string)
			if !ok || text == "" || !utf8.ValidString(text) {
				return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
			}
			expected[key] = text
		}
		// The request is `{ handle }` and nothing else: any other key is refused before
		// the ledger is read, which is what the family binder does when it unwraps this
		// channel (internal/transport/graph_generation.go:33-37).
		handle, err := productionGraphGenerationHandleRequest(args[0])
		if err != nil {
			return productionGraphReadFailure(err)
		}
		// The handle's identity is fenced against the frozen session before the lane
		// resolves it, which is the order the original uses
		// (electron/services/graph-generation.ts:25-30).
		if err := productionGraphGenerationIdentity(handle, expected); err != nil {
			return productionGraphReadFailure(err)
		}
		if ctx == nil {
			return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		value, err := read(ctx, caller, expected, handle)
		if err != nil {
			return productionGraphReadFailure(err)
		}
		return value, nil
	})
}

// productionGraphGenerationHandleRequest is the `{ handle }` unwrap of
// internal/transport/graph_generation.go with the same single-key rule and the same
// refusal code.
func productionGraphGenerationHandleRequest(value any) (any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("GENERATION_GRAPH_REQUEST_INVALID")
	}
	handle, present := object["handle"]
	if !present || len(object) != 1 {
		return nil, errors.New("GENERATION_GRAPH_REQUEST_INVALID")
	}
	return handle, nil
}

// productionGraphGenerationIdentity is the handle fence, and it is the one the original
// applies in GraphGeneration.require (electron/services/graph-generation.ts:25-30): the
// handle is exactly four keys, its project is the browser's project, and its epoch is the
// epoch that run was frozen under. A violation is GENERATION_RUN_IDENTITY_MISMATCH, the
// same code the composition lane's read uses for the same check
// (production_composition_linux.go:458).
//
// This is not redundant with the lane's own admission. That admission proves the stored
// run against the live lease, which is what stops another project's rows from being
// reached at all - the ledger is opened on the caller's own database. What it does not
// do is compare the handle with that scope, and the handle is the browser's: without
// this fence a handle whose projectId and epoch name another book still resolves by run
// id inside this book and comes back projected, so the caller learns that a handle it
// was never given is answerable. Refusing it here is also what makes the answer's own
// handle trustworthy, since the projection echoes the handle it was asked about.
func productionGraphGenerationIdentity(handle any, expected map[string]any) error {
	object := syntheticMap(handle)
	if object == nil {
		return errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
	}
	for key := range object {
		switch key {
		case "projectId", "epoch", "rootActionId", "runId":
		default:
			return errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
		}
	}
	if syntheticString(object["projectId"]) != syntheticString(expected["projectId"]) ||
		syntheticString(object["epoch"]) != syntheticString(expected["leaseId"]) {
		return errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
	}
	return nil
}

// productionGraphGenerationLine is one declaration a served or refused name depends on:
// the file, the line inside it, a marker that must be on that line, and a note saying
// what the declaration does for that name, so a seam-test failure is actionable rather
// than a bare line-number complaint.
type productionGraphGenerationLine struct {
	Path   string
	Line   int
	Marker string
	Note   string
}

// productionGraphGenerationSeam is one refused name and the missing pieces that keep it
// unbound.
type productionGraphGenerationSeam struct {
	Operation string
	Missing   []productionGraphGenerationLine
}

// productionGraphGenerationRefused is empty: this slice no longer leaves any name of
// the graph family unbound. The two names it once refused are served by
// production_graph_generation_execute_linux.go.
func productionGraphGenerationRefused() []string {
	return nil
}

// productionGraphGenerationServed is the answer graph-generation:read gives, anchored to
// the code that produces it. Every row must still hold: the projection, the re-proof
// that runs before it, the browser wire shape, and the two browser fields it
// deliberately omits.
func productionGraphGenerationServed() []productionGraphGenerationLine {
	return []productionGraphGenerationLine{
		{
			Path: "synthetic_graph_linux.go", Line: 19, Marker: "func (s *syntheticGeneration) GraphRead",
			Note: "the served projection: re-admits the session, requires the run, reserves nothing",
		},
		{
			Path: "synthetic_graph_linux.go", Line: 56, Marker: "func (s *syntheticGeneration) graphRecovery",
			Note: "re-proves the frozen context digest, input, origin epoch and task before answering",
		},
		{
			Path:   "../../../src/shared/graph-generation.ts", Line: 42, Marker: "'graph-generation:read': { args: [{ handle: MainGenerationRunHandle }]; return: GraphGenerationRecovery }",
			Note:   "the wire shape the browser sends and the type it decodes",
		},
		{
			Path:   "../../../electron/services/graph-generation.ts", Line: 25, Marker: "require(handle: MainGenerationRunHandle): DurableGenerationRun {",
			Note:   "the handle fence this binder applies: four keys, the browser's project, the run's epoch",
		},
		{
			Path: "synthetic_graph_linux.go", Line: 45, Marker: "effects: graph effects are recorded by confirmation, which is unbound.",
			Note: "the read projection still answers an empty effects list; confirmation is served elsewhere",
		},
		{
			Path: "synthetic_graph_linux.go", Line: 46, Marker: "artifact/result: the Node projection qualifies",
			Note: "the read projection still omits artifact and result; the qualification is served elsewhere",
		},
	}
}

// productionGraphGenerationSeams is empty: every name this slice once refused is now
// served, so there is no gap left to document.
func productionGraphGenerationSeams() []productionGraphGenerationSeam {
	return nil
}

// productionGraphGenerationSeamFor looks one refused name up, so a typo in the binder
// refuses at startup instead of being silently served or silently forgotten.
func productionGraphGenerationSeamFor(operation string) (productionGraphGenerationSeam, bool) {
	for _, seam := range productionGraphGenerationSeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionGraphGenerationSeam{}, false
}
