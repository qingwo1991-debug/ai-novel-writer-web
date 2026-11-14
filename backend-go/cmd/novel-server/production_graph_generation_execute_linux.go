//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/graphgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the two ends of a graph run the read slice deliberately left
// unbound: graph-generation:execute dispatches one admitted plan run through the
// same model lease and executor as generation:execute, and graph-generation:confirm
// turns the settled artifact into one narrative-thread plan and stamps the graph
// effect, all in one BEGIN IMMEDIATE transaction.
//
// Both names are the reference's own
// (electron/services/main-generation-owner.ts:1211-1228): execute carries
// `{ handle }` and answers the MainGenerationExecuteReceipt, confirm carries
// `{ handle, artifact, index }` and answers the GraphGenerationEffect receipt.

// productionGraphExecuteNonce is the invocation nonce the reference mints for a
// graph run (electron/services/main-generation-owner.ts:1214). The frozen task
// and the frozen binding make the request hash deterministic, so a replay of the
// same handle answers the already-settled attempt through the ledger's own
// findInvocation instead of dispatching a second model call.
const productionGraphExecuteNonce = "graph:0"

// productionGraphGenerationWrites wraps the one generation owner this lane
// built, so execute shares generation:begin's registry, model leases and executor
// and confirm shares the same admitted author database. A nil owner installs
// nothing, exactly like productionGenerationWrites.
type productionGraphGenerationWrites struct {
	owner *productionGeneration
}

// newProductionGraphGenerationWrites builds the two names over the owner the
// lane already admitted.
func newProductionGraphGenerationWrites(owner *productionGeneration) *productionGraphGenerationWrites {
	return &productionGraphGenerationWrites{owner: owner}
}

// Bind installs graph-generation:execute and graph-generation:confirm. It is a
// plain server.Bind per name through the family's mutating adapter, the same one
// productionGenerationWrites uses for graph-generation:cancel.
func (g *productionGraphGenerationWrites) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if g == nil || g.owner == nil || g.owner.inner == nil {
		return nil
	}
	for _, bind := range []struct {
		name  string
		arity int
		call  func(context.Context, transport.Caller, map[string]any, []any) (any, error)
	}{
		{"graph-generation:execute", 1, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			handle, err := productionGraphGenerationHandleRequest(args[0])
			if err != nil {
				return nil, err
			}
			if err := productionGraphGenerationIdentity(handle, expected); err != nil {
				return nil, err
			}
			return g.execute(ctx, caller, expected, handle)
		}},
		{"graph-generation:confirm", 1, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			request := syntheticMap(args[0])
			if request == nil {
				return nil, errors.New("GENERATION_GRAPH_REQUEST_INVALID")
			}
			for key := range request {
				switch key {
				case "handle", "artifact", "index":
				default:
					return nil, errors.New("GENERATION_GRAPH_REQUEST_INVALID")
				}
			}
			if err := productionGraphConfirmIdentity(request["handle"], expected); err != nil {
				return nil, err
			}
			handle, err := syntheticParseHandle(request["handle"])
			if err != nil {
				return nil, err
			}
			artifact, err := parseProductionGraphArtifact(request["artifact"])
			if err != nil {
				return nil, err
			}
			index, err := productionGraphIndex(request["index"])
			if err != nil {
				return nil, err
			}
			return g.confirm(ctx, caller, expected, handle, artifact, index)
		}},
	} {
		if err := server.Bind(bind.name, productionGenerationMutation(bind.arity, bind.call)); err != nil {
			return err
		}
	}
	return nil
}

// execute answers graph-generation:execute: the reference's one dispatch of the
// frozen graph task through the committed lease and executor.
func (g *productionGraphGenerationWrites) execute(ctx context.Context, caller transport.Caller, expected map[string]any, handle any) (any, error) {
	if g == nil || g.owner == nil || g.owner.open == nil || g.owner.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := g.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	if g.owner.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
	}
	return g.owner.inner.GraphExecute(ctx, caller, expected, handle)
}

// confirm answers graph-generation:confirm without a provider call, so it needs no
// executor: the run it confirms only exists once execute dispatched it.
func (g *productionGraphGenerationWrites) confirm(ctx context.Context, caller transport.Caller, expected map[string]any, handle syntheticHandle, artifact productionGraphArtifact, index int) (any, error) {
	if g == nil || g.owner == nil || g.owner.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	return g.owner.inner.GraphConfirm(ctx, caller, expected, handle, artifact, index)
}

// productionGraphArtifact is the `{ artifactId, revision, textHash }` reference the
// confirm request carries (src/shared/graph-generation.ts:25).
type productionGraphArtifact struct {
	ArtifactID string
	Revision   int64
	TextHash   string
}

// parseProductionGraphArtifact validates one GraphGenerationArtifact: exactly three
// keys, a non-empty id, a non-negative safe-integer revision and a 64-character
// lowercase hex text hash.
func parseProductionGraphArtifact(value any) (productionGraphArtifact, error) {
	invalid := errors.New("GENERATION_GRAPH_ARTIFACT_INVALID")
	object := syntheticMap(value)
	if object == nil || len(object) != 3 {
		return productionGraphArtifact{}, invalid
	}
	for key := range object {
		switch key {
		case "artifactId", "revision", "textHash":
		default:
			return productionGraphArtifact{}, invalid
		}
	}
	artifactID := syntheticString(object["artifactId"])
	revision, revisionOK := syntheticNonNegative(object["revision"])
	textHash := syntheticString(object["textHash"])
	if artifactID == "" || !revisionOK || !syntheticHex64(textHash) {
		return productionGraphArtifact{}, invalid
	}
	return productionGraphArtifact{ArtifactID: artifactID, Revision: revision, TextHash: textHash}, nil
}

// productionGraphIndex validates the confirm index the way the reference does
// (electron/services/graph-generation.ts:103): a safe integer, not negative.
func productionGraphIndex(value any) (int, error) {
	number, ok := syntheticFloat64(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) ||
		number < 0 || number > 1<<53-1 {
		return 0, errors.New("GENERATION_GRAPH_SELECTION_INVALID")
	}
	return int(number), nil
}

// syntheticNonNegative reads a non-negative safe integer.
func syntheticNonNegative(value any) (int64, bool) {
	number, ok := syntheticFloat64(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) ||
		number < 0 || number > 1<<53-1 {
		return 0, false
	}
	return int64(number), true
}

// productionGraphConfirmIdentity is the confirm half of the handle fence, and it
// is deliberately looser than the read fence: the reference's require accepts a
// handle epoch that is the run's binding epoch or its origin epoch, not the live
// lease (electron/services/graph-generation.ts:25-30), because a confirmation may
// settle a run whose lease has since rotated. It still refuses a handle that is
// not exactly four keys or that names another project; the run's epoch is then
// compared against the handle inside GraphConfirm, after the live lease was
// already proved by the lane's admission.
func productionGraphConfirmIdentity(handle any, expected map[string]any) error {
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
	if syntheticString(object["projectId"]) != syntheticString(expected["projectId"]) {
		return errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
	}
	return nil
}

// GraphExecute dispatches one graph run through the same path generation:execute
// uses, with the frozen graph task instead of a browser task and the reference's
// fixed invocation nonce.
func (s *syntheticGeneration) GraphExecute(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	handle, err := syntheticParseHandle(arg)
	if err != nil {
		return nil, err
	}
	contextValue, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	_, _, manifest, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	_, task, err := syntheticGraphContext(manifest, scope.Project)
	if err != nil {
		return nil, err
	}
	ownerTask, err := syntheticGraphOwnerTask(task)
	if err != nil {
		return nil, err
	}
	leaseID := syntheticString(manifest["modelLeaseId"])
	if leaseID == "" {
		return nil, errors.New("GENERATION_LEASE_MISSING")
	}
	if _, err := s.registry.Resolve(ctx, leaseID, scope, syntheticGenerationPurpose); err != nil {
		return nil, err
	}
	set, err := s.rebuildSourcesForRun(ctx, db, manifest)
	if err != nil {
		return nil, err
	}
	bindingValue, err := s.registry.BindSources(ctx, scope, leaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, err
	}
	if bindingValue.Fingerprint() != syntheticString(manifest["goOwnerSourceFingerprint"]) {
		return nil, errors.New("GENERATION_SOURCE_CHANGED")
	}
	frozen, err := s.executor.Prepare(ctx, scope, leaseID, ownerTask, syntheticRootOutputTokens, bindingValue)
	if err != nil {
		return nil, err
	}
	service, err := s.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	receipt, err := service.Execute(context.WithoutCancel(ctx), frozen, handle.RunID, productionGraphExecuteNonce)
	if err != nil {
		return nil, err
	}
	outcome := syntheticOutcome(syntheticMap(receipt))
	view, err := s.viewOf(ctx, ledger, db, syntheticMap(receipt)["run"])
	if err != nil {
		return nil, err
	}
	return map[string]any{"outcome": outcome, "run": view}, nil
}

// syntheticGraphOwnerTask converts the frozen graph task into the owner task the
// executor plans. The two structs are different packages but the same wire, so the
// conversion is a JSON round-trip through the owner's own strict parser.
func syntheticGraphOwnerTask(task graphgen.Task) (generationowner.Task, error) {
	raw, err := json.Marshal(task)
	if err != nil {
		return generationowner.Task{}, errors.New("GENERATION_GRAPH_TASK_INVALID")
	}
	ownerTask, err := generationowner.ParseTask(raw)
	if err != nil {
		return generationowner.Task{}, err
	}
	return ownerTask, nil
}

// syntheticGraphConfirmRun resolves the handle against the committed run for a
// confirmation. It drops the live-lease epoch equality syntheticRequireRun keeps:
// a confirm may settle a run whose lease rotated after it was frozen, exactly as
// the reference's require accepts the binding epoch or the origin epoch
// (electron/services/graph-generation.ts:25-30). The live lease is still proved
// by the admission before this is reached, and the handle's epoch is compared
// against the run's frozen epoch here.
func syntheticGraphConfirmRun(ctx context.Context, ledger *store.GenerationLedger, scope generationowner.Scope, handle syntheticHandle) (map[string]any, map[string]any, map[string]any, error) {
	args, err := syntheticArgs(handle.RunID)
	if err != nil {
		return nil, nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	value, err := ledger.CallJSON(ctx, "get", args)
	if err != nil {
		return nil, nil, nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	binding := syntheticMap(run["binding"])
	manifest := syntheticMap(binding["sourceManifest"])
	if run["rootActionId"] != handle.RootActionID || run["runId"] != handle.RunID ||
		binding["projectId"] != scope.Project || binding["epoch"] != handle.Epoch ||
		handle.ProjectID != scope.Project || manifest == nil {
		return nil, nil, nil, errors.New("GENERATION_RUN_NOT_FOUND")
	}
	return run, binding, manifest, nil
}

// GraphConfirm answers graph-generation:confirm for the plan kind: the admission
// and context re-proof happen here, and the one transaction that writes the plan
// and stamps the effect lives in the store.
func (s *syntheticGeneration) GraphConfirm(ctx context.Context, caller transport.Caller, expected map[string]any, handle syntheticHandle, artifact productionGraphArtifact, index int) (any, error) {
	_, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	_, _, manifest, err := syntheticGraphConfirmRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	contextValue, _, err := syntheticGraphContext(manifest, scope.Project)
	if err != nil {
		return nil, err
	}
	if syntheticString(contextValue["kind"]) != "plan" {
		return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
	}
	receipt, err := store.ConfirmGraphPlan(ctx, db, store.GraphConfirmRequest{
		RunID:         handle.RunID,
		RootActionID:  handle.RootActionID,
		ProjectID:     scope.Project,
		Artifact:      store.GraphConfirmArtifact{ArtifactID: artifact.ArtifactID, Revision: artifact.Revision, TextHash: artifact.TextHash},
		Index:         index,
		TotalChapters: int(syntheticNumber(contextValue["totalChapters"])),
		ContextHash:   syntheticString(manifest["graphGenerationContextHash"]),
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

// productionGraphGenerationExecuteServed anchors the two served names to the code
// that produces them, so the seam test keeps proving the answer did not drift.
func productionGraphGenerationExecuteServed() []productionGraphGenerationLine {
	return []productionGraphGenerationLine{
		{
			Path: "production_graph_generation_execute_linux.go", Line: 1, Marker: "func (s *syntheticGeneration) GraphExecute",
			Note: "execute dispatches the frozen graph task through the same lease and executor as generation:execute",
		},
		{
			Path: "../../../electron/services/graph-generation.ts", Line: 38, Marker: "private proof(run: DurableGenerationRun",
			Note: "the artifact qualification confirm re-proves before it writes the plan",
		},
		{
			Path: "../../../electron/services/graph-generation.ts", Line: 130, Marker: "graphEffects: [...(usage.graphEffects as StoredGraphEffect[] ?? []), effect]",
			Note: "the effect stamp confirm appends inside the same transaction",
		},
		{
			Path: "../../internal/store/graph_generation_confirm.go", Line: 1, Marker: "func ConfirmGraphPlan",
			Note: "the one BEGIN IMMEDIATE transaction that writes the plan and stamps the effect",
		},
		{
			Path: "../../internal/graphgen/parse.go", Line: 104, Marker: "func ParseNarrativeThreadPlanCandidates",
			Note: "the derivation the seam claimed had no port, which now serves confirm",
		},
	}
}
