//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/agentgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves agent-generation:begin and agent-generation:read - the run's
// own state, frozen from shipped prose and re-proved on every read, the way
// legacy-roster:begin/read are served. The family's other six names are served by
// production_agent_generation_tools_linux.go:
//
//	agent-generation:begin               served here
//	agent-generation:read                served here
//	agent-generation:resume              served (tools file)
//	agent-generation:round               served (tools file)
//	agent-generation:claim-tool          served (tools file)
//	agent-generation:finish-tool         served (tools file)
//	agent-generation:register-workflow   served (tools file)
//	agent-generation:commit-domain-tool  served (tools file)
//
// # Why begin and read are servable
//
// The old fence was "no Go file names PromptTemplate or renderPrompt". The
// identity template `assistant_writing_identity` ships in
// src/services/builtin-prompt-templates.ts:194 with its en-US overlay at :1472,
// and the prose buildAgentGenerationContext composes around it ships in
// electron/services/agent-generation-context.ts:39-54. Both are now a committed
// asset this binary embeds (internal/prompttemplate/agent-identity-v1.json),
// returned as the exact literals the desktop writes, so no prompt prose is
// authored in Go - the same port the finalization, editor-inline and legacy
// roster families received. The context is composed by internal/agentgen
// (BuildContext), a pure function over that prompt and the admitted project
// core, and frozen as JSON text beside its digest.
//
// The reduced lane's own prompt source (synthetic_generation_linux.go:645) still
// answers a synthesized placeholder instead of the template, so the binding's
// `prompt:assistant_writing_identity:<lang>` capability hashes that placeholder
// and not the shipped object. That is the one place this slice is narrower than
// the desktop, and it is why the *context* is what matters: the frozen
// agentContext carries the shipped template, the project core and the tool
// manifest, and read re-proves it byte for byte.
//
// # begin: what it freezes
//
// The answer is the reference's AgentGenerationRecovery
// (src/shared/agent-generation.ts:104-112): handle, modelId, context, rounds,
// sourceStatus, nextRound and run. begin spends the nonce exactly like the
// desktop does: key = sha256([projectId, uiActionNonce])
// (agent-generation.ts:147), the run's uiActionNonce is `agent:<key>:0`
// (:176), and the session is frozen as sourceManifest.agentSession with its
// context and digest beside it (:592-593). The ledger admits all three members
// for operation agent-round (internal/store/generation_ledger.go:428-434).
//
// A nonce this project already spent - any run whose manifest froze this session
// key, at any round index - is a replay when it froze the same input and model,
// and GENERATION_AGENT_NONCE_CONFLICT when it did not (:148-152).
//
// # read: the session's rounds
//
// read resolves the handle, re-proves the context digest against the exact
// stored bytes, then projects every run of the session in round order (:70-140).
// A round with no attempt is not-started; one whose attempt carries no parsed
// response is incomplete, or unknown when the invocation may be in flight; only
// a stop whose artifact text is the response's visible text is completed, and
// only its tool calls become actions. internal/agentgen owns that projection
// (ClassifyRound, NextRound) and read only feeds it.
//
// A run begun here that no round has been dispatched for yet reads back as one
// not-started round with nextRound 0: exactly what the reference answers for a
// freshly begun session.
//
// # The six tool names
//
// The six live in production_agent_generation_tools_linux.go. Each re-proves the
// session run, decides through internal/agentgen and writes its one receipt entry
// through the store's recordAgentReceipt port
// (internal/store/generation_ledger_agent.go:76), so the agent protocol never
// rides on the shared ordinary service's execute:
//
//   - resume re-admits a paused or completed run through the root-only activate
//     port and patches agentControlEpoch; it never rewrites the binding, because
//     that would re-encode the frozen agentContext and break its digest.
//   - round dispatches under a present agentToolNames manifest, admitted by
//     internal/generation for operation agent-round alone
//     (internal/generation/service.go:277), and the parse it stamps as
//     result.usage.agentResponse (:533-539) is what the next read projects.
//   - claim-tool, finish-tool, register-workflow and commit-domain-tool write the
//     action map, the workflow metadata and the domain effect through the same
//     receipt port, whose allowlist admits exactly agentActions,
//     agentAuthorSelections, agentSourceTransition and agentControlEpoch.

const (
	// productionAgentGenerationVisibleText is the one output a session freezes
	// (electron/services/agent-generation.ts:178).
	productionAgentGenerationVisibleText = "visible-text"
	// productionAgentGenerationNoncePrefix is begin's uiActionNonce prefix
	// (agent-generation.ts:176): `agent:<sessionKey>:<roundIndex>`.
	productionAgentGenerationNoncePrefix = "agent:"
	// productionAgentGenerationNonceMax is the reference's bound on uiActionNonce
	// (agent-generation.ts:144).
	productionAgentGenerationNonceMax = 256
	// productionAgentGenerationStatusCurrent is the sourceStatus a run answers
	// when its frozen binding still reproduces (:139).
	productionAgentGenerationStatusCurrent = "current"
	// productionAgentGenerationStatusConflict is the other sourceStatus.
	productionAgentGenerationStatusConflict = "conflict"
	// productionAgentGenerationActionIDPrefix is the tool call identity
	// (agent-generation.ts:76): `agent-tool:<attemptId>:<index>`.
	productionAgentGenerationActionIDPrefix = "agent-tool:"
	// productionAgentGenerationRunMissing is the ledger's code for a run id this
	// database does not hold (electron/repositories/generation-run-repository.ts:126).
	productionAgentGenerationRunMissing = "GENERATION_RUN_MISSING"
)

// productionAgentGenerationServedOperations is the whole family, installed by
// this file's Bind through the per-name dispatch
// production_agent_generation_tools_linux.go adds.
// productionAgentGenerationOperations is now empty; the two lists stay disjoint
// by construction, because Bind refuses to run if a served name is also in the
// refusal table.
func productionAgentGenerationServedOperations() []string {
	return []string{
		productionAgentGenerationBegin,
		productionAgentGenerationRead,
		productionAgentGenerationRound,
		productionAgentGenerationClaimTool,
		productionAgentGenerationFinishTool,
		productionAgentGenerationRegister,
		productionAgentGenerationCommitDomain,
		productionAgentGenerationResume,
	}
}

// productionAgentGenerationServe serves the eight agent-generation:* names over
// the one generation owner, exactly like productionLegacyRosterServe and
// productionFinalizationServe do for their families: begin/read here, and the six
// tool names in production_agent_generation_tools_linux.go.
type productionAgentGenerationServe struct {
	owner *productionGeneration
}

func newProductionAgentGenerationServe(owner *productionGeneration) *productionAgentGenerationServe {
	return &productionAgentGenerationServe{owner: owner}
}

func (a *productionAgentGenerationServe) ready() bool {
	return a != nil && a.owner != nil && a.owner.open != nil && a.owner.inner != nil
}

// Bind installs all eight served names - begin and read from this file, the six
// tool names from production_agent_generation_tools_linux.go, through the
// per-name dispatch that file adds. A nil owner is not an error: the names keep
// failing closed at GO_OPERATION_NOT_MIGRATED, exactly like the sibling binders,
// and a nil server is refused like every other binder in this package.
//
// It is called from production_open_linux.go's owner block
// (production_open_linux.go:892), next to
// newProductionLegacyRosterServe(owner).Bind(server): begin mints a model lease
// and opens a ledger run, so the generation owner is the one port it cannot do
// without.
func (a *productionAgentGenerationServe) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if !a.ready() {
		return nil
	}
	for _, name := range productionAgentGenerationServedOperations() {
		if _, refused := productionAgentGenerationSeamFor(name); refused {
			return errors.New("PRODUCTION_AGENT_GENERATION_SEAM_DOUBLE_BOUND")
		}
	}
	for _, name := range productionAgentGenerationServedOperations() {
		if err := server.Bind(name, productionFinalizationServeChannel(a.operation(name))); err != nil {
			return err
		}
	}
	return nil
}

// productionAgentGenerationBeginRequest validates the reference's begin envelope
// (electron/services/agent-generation.ts:141-146): exactly the three documented
// members, a non-blank nonce of at most 256 characters, a non-blank model id, and
// an input its own validator accepts.
func productionAgentGenerationBeginRequest(value map[string]any) (modelID, nonce string, input agentgen.Input, err error) {
	invalid := errors.New(agentgen.CodeBeginInvalid)
	if value == nil {
		return "", "", agentgen.Input{}, invalid
	}
	for key := range value {
		switch key {
		case "uiActionNonce", "modelId", "input":
		default:
			return "", "", agentgen.Input{}, invalid
		}
	}
	rawNonce, ok := value["uiActionNonce"].(string)
	if !ok || strings.TrimSpace(rawNonce) == "" || utf8.RuneCountInString(rawNonce) > productionAgentGenerationNonceMax ||
		!utf8.ValidString(rawNonce) {
		return "", "", agentgen.Input{}, invalid
	}
	rawModel, ok := value["modelId"].(string)
	if !ok || strings.TrimSpace(rawModel) == "" || !utf8.ValidString(rawModel) {
		return "", "", agentgen.Input{}, invalid
	}
	parsed, err := agentgen.ParseInput(syntheticMap(value["input"]))
	if err != nil {
		return "", "", agentgen.Input{}, err
	}
	return rawModel, rawNonce, parsed, nil
}

// productionAgentGenerationHandleRequest reads `{ handle }`. The reference reads
// request.handle and does not inspect the rest of the envelope, so neither does
// this: a member this lane would have to ignore is not one it may honour, and a
// read that refused a shape the desktop sends would be a regression, not a gate.
func productionAgentGenerationHandleRequest(value map[string]any) (syntheticHandle, error) {
	raw, ok := value["handle"]
	if !ok {
		return syntheticHandle{}, errors.New(agentgen.CodeIdentityMismatch)
	}
	handle, err := syntheticParseHandle(raw)
	if err != nil {
		return syntheticHandle{}, errors.New(agentgen.CodeIdentityMismatch)
	}
	return handle, nil
}

// productionAgentGenerationWireHandle is the four-key handle a response carries
// (src/services/generation/generation-runtime.ts, MainGenerationRunHandle).
func productionAgentGenerationWireHandle(handle syntheticHandle) map[string]any {
	return map[string]any{
		"projectId":    handle.ProjectID,
		"epoch":        handle.Epoch,
		"rootActionId": handle.RootActionID,
		"runId":        handle.RunID,
	}
}

// begin answers agent-generation:begin. A malformed request is refused before a
// row is written; a spent nonce answers the session it already opened; anything
// else opens round 0 of a new one.
func (a *productionAgentGenerationServe) begin(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !a.ready() {
		return nil, errProductionGenerationUnavailable
	}
	// The gate generation:begin passes first. A lane with no committed model
	// boundary refuses before a root row exists rather than freezing a run whose
	// lease could never be minted.
	if _, err := a.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	// The reference asserts the session before it reads the request
	// (agent-generation.ts:142), so a malformed begin under a lease this server
	// never issued answers the session's refusal.
	_, scope, ledger, db, done, err := a.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	modelID, nonce, input, err := productionAgentGenerationBeginRequest(request)
	if err != nil {
		return nil, err
	}
	key, err := agentgen.SessionKey(scope.Project, nonce)
	if err != nil {
		return nil, err
	}
	prior, err := productionAgentGenerationSessionRuns(ctx, ledger, db, scope.Project, key)
	if err != nil {
		return nil, err
	}
	if len(prior) > 0 {
		// The replay is the session's first round: the desktop answers
		// read(handle of sessionRuns(key)[0]) (agent-generation.ts:151), and the
		// two terms it compares are the frozen input and the frozen model (:150).
		frozen, err := productionAgentGenerationFrozenInput(prior[0].Context)
		if err != nil {
			return nil, err
		}
		current, err := input.Text()
		if err != nil || current != frozen || prior[0].ModelID != modelID {
			return nil, errors.New(agentgen.CodeNonceConflict)
		}
		return a.recovery(ctx, scope, ledger, db, prior[0].Handle())
	}
	opened, err := a.openAgentRun(ctx, scope, ledger, db, input, modelID, key)
	if err != nil {
		return nil, err
	}
	handle, err := productionEditorInlineRunHandle(scope.Project, scope.Epoch, opened)
	if err != nil {
		return nil, err
	}
	return a.recovery(ctx, scope, ledger, db, handle)
}

// read answers agent-generation:read with the same envelope begin does, so the
// renderer validates one shape. It reserves no attempt, resolves no credential
// and opens no egress endpoint.
func (a *productionAgentGenerationServe) read(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !a.ready() {
		return nil, errProductionGenerationUnavailable
	}
	_, scope, ledger, db, done, err := a.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	handle, err := productionAgentGenerationHandleRequest(request)
	if err != nil {
		return nil, err
	}
	return a.recovery(ctx, scope, ledger, db, handle)
}

// productionAgentGenerationFrozenInput re-reads the input a session froze, in the
// same canonical projection a new request is compared against.
func productionAgentGenerationFrozenInput(frozen *agentgen.Context) (string, error) {
	if frozen == nil {
		return "", errors.New(agentgen.CodeContextInvalid)
	}
	return frozen.Input.Text()
}

// openAgentRun is the family's one run admission: the reference's begin selection
// (agent-generation.ts:175-178), the lease, the frozen sources, the nine derived
// hashes, the canonical budget, and the ledger's own open - with the session and
// its context frozen as ordered JSON text.
//
// It mirrors productionLegacyRosterServe.openLegacyRosterRun and differs in the
// family's own selection and members. The frozen prompt key is the identity
// template because that is the key the reference freezes.
func (a *productionAgentGenerationServe) openAgentRun(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, input agentgen.Input, modelID, key string) (map[string]any, error) {
	selection, err := syntheticParseSelection(map[string]any{
		"operation":                       agentgen.Operation,
		"uiActionNonce":                   productionAgentGenerationNoncePrefix + key + ":0",
		"modelId":                         modelID,
		"selectedDraftIds":                []any{},
		"selectedFinalizedDraftIds":       []any{},
		"selectedBlueprintChapterNumbers": []any{},
		"promptKeys":                      []any{prompttemplate.AgentIdentityKey},
		"skillStages":                     []any{},
		"authorInputs":                    []any{},
		"output":                          productionAgentGenerationVisibleText,
	})
	if err != nil {
		return nil, err
	}
	receipt, err := a.owner.inner.registry.Begin(ctx, scope, selection.ModelID, syntheticGenerationPurpose)
	if err != nil {
		return nil, err
	}
	set, err := syntheticBuildSources(ctx, db, selection)
	if err != nil {
		return nil, err
	}
	language := productionEditorInlineLanguage(set.Core)
	prompt, err := prompttemplate.AgentIdentityPrompt(language)
	if err != nil {
		return nil, errors.New(agentgen.CodePromptInvalid)
	}
	contextValue, err := agentgen.BuildContext(input, set.CoreText, language, productionAgentGenerationPrompt(prompt))
	if err != nil {
		return nil, err
	}
	contextText, err := contextValue.Text()
	if err != nil {
		return nil, err
	}
	binding, err := a.owner.inner.registry.BindSources(ctx, scope, receipt.LeaseID, set.Sources, set.Selection)
	if err != nil {
		return nil, err
	}
	plan, err := a.owner.inner.syntheticCanonicalPlan(ctx, scope, receipt.LeaseID, selection)
	if err != nil {
		return nil, err
	}
	budget, err := syntheticBudget(plan)
	if err != nil {
		return nil, err
	}
	fingerprint, err := syntheticFingerprint(ctx, db, scope, binding, set, selection, receipt, budget)
	if err != nil {
		return nil, err
	}
	frozenInput, err := syntheticCanonical([]any{fingerprint, set.CoreText, syntheticOutputContract(selection)})
	if err != nil {
		return nil, errors.New(agentgen.CodeBeginInvalid)
	}
	refs, err := syntheticRefsAsAny(binding.Refs())
	if err != nil {
		return nil, errors.New(agentgen.CodeBeginInvalid)
	}
	fingerprintText, err := productionEditorInlineJSON(fingerprint)
	if err != nil {
		return nil, errors.New(agentgen.CodeBeginInvalid)
	}
	budgetText, err := productionEditorInlineJSON(budget)
	if err != nil {
		return nil, errors.New(agentgen.CodeBeginInvalid)
	}
	refsText, err := productionEditorInlineJSON(refs)
	if err != nil {
		return nil, errors.New(agentgen.CodeBeginInvalid)
	}
	request := productionEditorInlineObject(
		productionEditorInlineField{"projectId", productionEditorInlineQuote(scope.Project)},
		productionEditorInlineField{"epoch", productionEditorInlineQuote(scope.Epoch)},
		productionEditorInlineField{"fingerprint", fingerprintText},
		productionEditorInlineField{"contextSnapshotId", productionEditorInlineQuote(syntheticHash(set.CoreText))},
		productionEditorInlineField{"sourceManifest", productionAgentGenerationManifestText(selection, receipt.LeaseID, binding.Fingerprint(), key, contextText)},
		productionEditorInlineField{"sourceRefs", refsText},
		productionEditorInlineField{"operation", productionEditorInlineQuote(selection.Operation)},
		productionEditorInlineField{"uiActionNonce", productionEditorInlineQuote(selection.UIActionNonce)},
		productionEditorInlineField{"frozenInputHash", productionEditorInlineQuote(syntheticHash(frozenInput))},
		productionEditorInlineField{"budget", budgetText},
	)
	value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+request+"]"))
	if err != nil {
		return nil, err
	}
	run := syntheticMap(value)
	if run == nil {
		return nil, errors.New(productionAgentGenerationRunMissing)
	}
	return run, nil
}

// productionAgentGenerationPrompt is the boundary between the shipped asset and
// the protocol: the prompt is read once, by internal/prompttemplate, and placed
// here without a literal of its own.
func productionAgentGenerationPrompt(prompt prompttemplate.AgentIdentity) agentgen.IdentityPrompt {
	return agentgen.IdentityPrompt{
		SystemRole:             prompt.Template.SystemRole,
		Content:                prompt.Template.Content,
		TaskGuidance:           prompt.Template.TaskGuidance,
		SystemSuffix:           prompt.Template.SystemSuffix,
		SystemContract:         prompt.SystemContract,
		ModePlanning:           prompt.ModePlanning,
		ModeFast:               prompt.ModeFast,
		ToolHeading:            prompt.ToolHeading,
		ToolInstruction:        prompt.ToolInstruction,
		AuthorGuidanceHeading:  prompt.AuthorGuidanceHeading,
		ProjectSettingsHeading: prompt.ProjectSettingsHeading,
		EditorContextHeading:   prompt.EditorContextHeading,
	}
}

// productionAgentGenerationManifestText is the frozen source manifest: the
// reduced lane's members in syntheticManifest order
// (synthetic_generation_linux.go:736-762), then the session, its context and the
// context digest the reference freezes beside it
// (electron/services/generation-source-binding.ts:592-593).
func productionAgentGenerationManifestText(selection syntheticSelection, leaseID, fingerprint, key, contextText string) string {
	return productionEditorInlineObject(
		productionEditorInlineField{"operation", productionEditorInlineQuote(selection.Operation)},
		productionEditorInlineField{"modelId", productionEditorInlineQuote(selection.ModelID)},
		productionEditorInlineField{"modelLeaseId", productionEditorInlineQuote(leaseID)},
		productionEditorInlineField{"goOwnerSourceFingerprint", productionEditorInlineQuote(fingerprint)},
		productionEditorInlineField{"purpose", productionEditorInlineQuote(syntheticPurpose(selection.Operation))},
		productionEditorInlineField{"output", productionEditorInlineQuote(selection.Output)},
		productionEditorInlineField{"promptKeys", productionEditorInlineStringArray(selection.PromptKeys)},
		productionEditorInlineField{"skillStages", "[]"},
		productionEditorInlineField{"authorInputs", "[]"},
		productionEditorInlineField{"selectedDraftIds", productionEditorInlineIDArray(selection.SelectedDraftIDs)},
		productionEditorInlineField{"selectedFinalizedDraftIds", productionEditorInlineIDArray(selection.SelectedFinalizedDraftIDs)},
		productionEditorInlineField{"selectedBlueprintChapterNumbers", productionEditorInlineIDArray(selection.SelectedBlueprintChapters)},
		productionEditorInlineField{"agentSession", productionEditorInlineObject(
			productionEditorInlineField{"key", productionEditorInlineQuote(key)},
			productionEditorInlineField{"roundIndex", "0"},
		)},
		productionEditorInlineField{"agentContext", contextText},
		productionEditorInlineField{"agentContextHash", productionEditorInlineQuote(syntheticHash(contextText))},
	)
}

// productionAgentGenerationRun is one proved session run: the ledger row, its
// manifest, the frozen session, the model it froze and the context it composed.
type productionAgentGenerationRun struct {
	Run      map[string]any
	Manifest map[string]any
	ModelID  string
	Session  agentgen.Session
	Context  *agentgen.Context
}

// Handle is the run's own identity, which is what a recovery answers with.
func (r *productionAgentGenerationRun) Handle() syntheticHandle {
	binding := syntheticMap(r.Run["binding"])
	return syntheticHandle{
		ProjectID:    syntheticString(binding["projectId"]),
		Epoch:        syntheticString(binding["epoch"]),
		RootActionID: syntheticString(r.Run["rootActionId"]),
		RunID:        syntheticString(r.Run["runId"]),
	}
}

// productionAgentGenerationRecord proves one run's frozen context and session:
// the digest is over the exact stored text of the context member, because that is
// the only place its insertion order still exists (agent-generation.ts:58-69).
func productionAgentGenerationRecord(ctx context.Context, db *sql.DB, run map[string]any) (*productionAgentGenerationRun, error) {
	binding := syntheticMap(run["binding"])
	if binding == nil {
		return nil, errors.New(productionAgentGenerationRunMissing)
	}
	manifest := syntheticMap(binding["sourceManifest"])
	if manifest == nil {
		return nil, errors.New(productionAgentGenerationRunMissing)
	}
	digest := syntheticString(manifest["agentContextHash"])
	if digest == "" {
		return nil, errors.New(agentgen.CodeContextInvalid)
	}
	raw, err := productionAgentGenerationStoredContext(ctx, db, syntheticString(run["runId"]))
	if err != nil {
		return nil, err
	}
	if syntheticHash(string(raw)) != digest {
		return nil, errors.New(agentgen.CodeContextInvalid)
	}
	frozen, err := agentgen.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	session, err := agentgen.ParseSession(syntheticMap(manifest["agentSession"]))
	if err != nil {
		return nil, err
	}
	return &productionAgentGenerationRun{Run: run, Manifest: manifest,
		ModelID: syntheticString(manifest["modelId"]), Session: session, Context: &frozen}, nil
}

// resolve proves one session run in the reference's order: the handle against the
// run (agent-generation.ts:50-57), then the context and the navigation
// (:58-69).
func (a *productionAgentGenerationServe) resolve(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, handle syntheticHandle) (*productionAgentGenerationRun, error) {
	if handle.ProjectID != scope.Project {
		return nil, errors.New(agentgen.CodeIdentityMismatch)
	}
	value, err := syntheticCall(ctx, ledger, "get", handle.RunID)
	if err != nil {
		return nil, err
	}
	run := syntheticMap(value)
	binding := syntheticMap(run["binding"])
	if run == nil || binding == nil {
		return nil, errors.New(productionAgentGenerationRunMissing)
	}
	// requireRun's identity terms. The session is not one of them - the reference
	// compares the handle first - so nothing is invented when the manifest is not
	// a session run: MatchIdentity refuses the operation straight away.
	if _, err := agentgen.MatchIdentity(agentgen.Handle{ProjectID: handle.ProjectID, Epoch: handle.Epoch,
		RootActionID: handle.RootActionID, RunID: handle.RunID}, agentgen.Frozen{
		ProjectID: syntheticString(binding["projectId"]), Epoch: syntheticString(binding["epoch"]),
		RootActionID: syntheticString(run["rootActionId"]), RunID: syntheticString(run["runId"]),
		Operation: syntheticString(syntheticMap(binding["sourceManifest"])["operation"]),
	}); err != nil {
		return nil, err
	}
	return productionAgentGenerationRecord(ctx, db, run)
}

// productionAgentGenerationStoredContext returns the exact stored text of the
// agentContext member inside one run's binding document.
func productionAgentGenerationStoredContext(ctx context.Context, db *sql.DB, runID string) ([]byte, error) {
	if db == nil {
		return nil, errors.New(agentgen.CodeContextInvalid)
	}
	var stored string
	err := db.QueryRowContext(ctx, `SELECT binding_json FROM generation_runs WHERE run_id=?`, runID).Scan(&stored)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New(productionAgentGenerationRunMissing)
		}
		return nil, err
	}
	manifest, ok := productionEditorInlineRawMember([]byte(stored), "sourceManifest")
	if !ok {
		return nil, errors.New(agentgen.CodeContextInvalid)
	}
	raw, ok := productionEditorInlineRawMember(manifest, "agentContext")
	if !ok || len(raw) == 0 || raw[0] != '{' {
		return nil, errors.New(agentgen.CodeContextInvalid)
	}
	return raw, nil
}

// productionAgentGenerationSessionRuns is the reference's sessionRuns
// (agent-generation.ts:70-74): every run of this project whose manifest froze
// this session key, in round order. The binding is scanned and filtered here
// rather than asked of json_extract, for the same reason the editor-inline
// look-up is: this read must not depend on an extension the schema does not
// declare.
func productionAgentGenerationSessionRuns(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, projectID, key string) ([]*productionAgentGenerationRun, error) {
	if db == nil {
		return nil, errors.New(productionAgentGenerationRunMissing)
	}
	rows, err := db.QueryContext(ctx, `SELECT run_id FROM generation_runs ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	var runIDs []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	records := make([]*productionAgentGenerationRun, 0, len(runIDs))
	for _, runID := range runIDs {
		value, err := syntheticCall(ctx, ledger, "get", runID)
		if err != nil {
			// A row another build wrote may not decode as a run; it can never be a
			// run this family froze, so it is skipped rather than refusing the read.
			continue
		}
		run := syntheticMap(value)
		binding := syntheticMap(run["binding"])
		manifest := syntheticMap(binding["sourceManifest"])
		if manifest == nil || syntheticString(binding["projectId"]) != projectID ||
			syntheticString(syntheticMap(manifest["agentSession"])["key"]) != key {
			continue
		}
		record, err := productionAgentGenerationRecord(ctx, db, run)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	// The reference sorts by round index and then requires rounds[i].index === i
	// (agent-generation.ts:73, :135); NextRound performs the second half.
	sort.SliceStable(records, func(left, right int) bool {
		return records[left].Session.RoundIndex < records[right].Session.RoundIndex
	})
	return records, nil
}

// recovery assembles the reference's AgentGenerationRecovery: the session's
// rounds in round order, the root they share, the context of the first, the
// currency of the latest and the view of the newest run.
func (a *productionAgentGenerationServe) recovery(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, handle syntheticHandle) (any, error) {
	selected, err := a.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	records, err := productionAgentGenerationSessionRuns(ctx, ledger, db, scope.Project, selected.Session.Key)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errors.New(agentgen.CodeRootMismatch)
	}
	root := syntheticString(selected.Run["rootActionId"])
	for _, record := range records {
		// Every round of a session is a run under one root (:133).
		if syntheticString(record.Run["rootActionId"]) != root {
			return nil, errors.New(agentgen.CodeRootMismatch)
		}
	}
	rounds := make([]map[string]any, 0, len(records))
	classified := make([]agentgen.Round, 0, len(records))
	for _, record := range records {
		round, err := a.round(ctx, ledger, db, record)
		if err != nil {
			return nil, err
		}
		rounds = append(rounds, round.wire())
		classified = append(classified, round.Agent)
	}
	// NextRound refuses a gap and an empty session, and answers the only index a
	// caller may still execute.
	next, err := agentgen.NextRound(classified)
	if err != nil {
		return nil, err
	}
	latest := records[len(records)-1]
	view, err := a.owner.inner.viewOf(ctx, ledger, db, latest.Run)
	if err != nil {
		return nil, err
	}
	contextText, err := productionAgentGenerationStoredContext(ctx, db, syntheticString(records[0].Run["runId"]))
	if err != nil {
		return nil, err
	}
	value := map[string]any{
		"handle":       productionAgentGenerationWireHandle(latest.Handle()),
		"modelId":      syntheticString(records[0].Manifest["modelId"]),
		"context":      json.RawMessage(contextText),
		"rounds":       rounds,
		"sourceStatus": a.sourceStatus(ctx, scope, ledger, db, latest),
		"run":          view,
	}
	if next == nil {
		value["nextRound"] = nil
	} else {
		value["nextRound"] = *next
	}
	return value, nil
}

// productionAgentGenerationRoundWire is one projectable round: the wire shape a read
// answers with, and the pure projection internal/agentgen decided.
type productionAgentGenerationRoundWire struct {
	Index        int64
	Handle       syntheticHandle
	AttemptID    string
	Status       string
	VisibleText  string
	ProtocolText string
	Actions      []map[string]any
	Agent        agentgen.Round
}

// wire renders the round exactly as AgentGenerationRound is declared
// (src/shared/agent-generation.ts:84-96): index, handle, attemptId when an
// attempt exists, status, visibleText, protocolText and actions.
func (r productionAgentGenerationRoundWire) wire() map[string]any {
	round := map[string]any{
		"index":        r.Index,
		"handle":       productionAgentGenerationWireHandle(r.Handle),
		"status":       r.Status,
		"visibleText":  r.VisibleText,
		"protocolText": r.ProtocolText,
		"actions":      r.Actions,
	}
	if r.AttemptID != "" {
		round["attemptId"] = r.AttemptID
	}
	return round
}

// round projects one session run in the reference's order
// (agent-generation.ts:83-130): no attempt is not-started, more than one is
// ROUND_AMBIGUOUS, and an attempt is classified from what it settled.
func (a *productionAgentGenerationServe) round(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, record *productionAgentGenerationRun) (productionAgentGenerationRoundWire, error) {
	projected := productionAgentGenerationRoundWire{Index: record.Session.RoundIndex, Handle: record.Handle()}
	attempts, err := productionAgentGenerationAttempts(ctx, db, syntheticString(record.Run["runId"]))
	if err != nil {
		return projected, err
	}
	if len(attempts) > 1 {
		return projected, errors.New(agentgen.CodeRoundAmbiguous)
	}
	if len(attempts) == 0 {
		projected.Status = agentgen.RoundNotStarted
		projected.Actions = []map[string]any{}
		projected.Agent = agentgen.Round{Index: projected.Index, Status: agentgen.RoundNotStarted}
		return projected, nil
	}
	value, err := syntheticCall(ctx, ledger, "receipt", attempts[0])
	if err != nil {
		return projected, err
	}
	receipt := syntheticMap(value)
	attempt := productionAgentGenerationAttempt(receipt)
	projected.AttemptID = attempts[0]
	round := agentgen.ClassifyRound(projected.Index, attempt)
	projected.Status = round.Status
	projected.VisibleText = round.VisibleText
	if round.Status != agentgen.RoundCompleted {
		projected.Actions = []map[string]any{}
		projected.Agent = round
		return projected, nil
	}
	// The stored action state lives on the top-level usage receipt (agentActions),
	// not inside result.usage; read it back so a settled claim/finish/register is
	// reported rather than pretended to be still pending.
	usage, err := productionAgentGenerationStoredUsage(ctx, db, attempts[0])
	if err != nil {
		return projected, err
	}
	actions := make([]map[string]any, 0, len(round.Actions))
	for index, action := range round.Actions {
		toolCallID := productionAgentGenerationActionIDPrefix + attempts[0] + ":" + strconv.Itoa(index)
		state := productionAgentGenerationActionState(usage, toolCallID)
		status := action.Status
		if stored := syntheticString(state["status"]); stored != "" {
			status = stored
		}
		wire := map[string]any{
			"ref": map[string]any{
				"handle":     productionAgentGenerationWireHandle(projected.Handle),
				"attemptId":  attempts[0],
				"toolCallId": toolCallID,
			},
			"name":      action.Name,
			"arguments": action.Arguments,
			"status":    status,
		}
		if observation := syntheticString(state["observation"]); observation != "" {
			wire["observation"] = observation
			round.Actions[index].Observation = observation
		}
		if workflow := syntheticMap(state["workflow"]); workflow != nil {
			wire["workflow"] = workflow
			round.Actions[index].Registration = productionAgentGenerationRegistration(workflow)
		}
		round.Actions[index].Status = status
		actions = append(actions, wire)
	}
	// protocolText is exactly the assistant message the next round would carry
	// (agent-generation.ts:129, :200-204), so the loop sees what it said.
	if messages, err := agentgen.Protocol(round); err != nil {
		return projected, err
	} else if len(messages) > 0 {
		projected.ProtocolText = messages[0].Content
	}
	projected.Actions = actions
	projected.Agent = round
	return projected, nil
}

// productionAgentGenerationAttempts reads one run's attempt ids in row order. A
// second one is the reference's ROUND_AMBIGUOUS, not a round to choose from.
func productionAgentGenerationAttempts(ctx context.Context, db *sql.DB, runID string) ([]string, error) {
	if db == nil {
		return nil, errors.New(productionAgentGenerationRunMissing)
	}
	rows, err := db.QueryContext(ctx, `SELECT attempt_id FROM generation_attempts WHERE run_id=? ORDER BY rowid`, runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var attempts []string
	for rows.Next() {
		var attemptID string
		if err := rows.Scan(&attemptID); err != nil {
			return nil, err
		}
		attempts = append(attempts, attemptID)
	}
	return attempts, rows.Err()
}

// productionAgentGenerationAttempt is the round's attempt projection: the status
// and finish reason it settled, and - when it carries one - the parsed agent
// response the dispatch stamped.
func productionAgentGenerationAttempt(receipt map[string]any) *agentgen.Attempt {
	attempt := syntheticMap(receipt["attempt"])
	if attempt == nil {
		return nil
	}
	result := syntheticMap(receipt["result"])
	artifact := syntheticMap(receipt["artifact"])
	projected := &agentgen.Attempt{
		Status:       syntheticString(attempt["status"]),
		FinishReason: syntheticString(result["finishReason"]),
		ArtifactText: syntheticString(artifact["text"]),
	}
	response := syntheticMap(syntheticMap(result["usage"])["agentResponse"])
	if response == nil {
		return projected
	}
	projected.HasResponse = true
	projected.ResponseVersion = syntheticNumber(response["version"])
	projected.ResponseText = syntheticString(response["visibleText"])
	for _, raw := range syntheticSlice(response["toolCalls"]) {
		call := syntheticMap(raw)
		if call == nil {
			continue
		}
		projected.ToolCalls = append(projected.ToolCalls, agentgen.ToolCall{
			Name:      syntheticString(call["name"]),
			Arguments: syntheticMap(call["arguments"]),
		})
	}
	return projected
}

// productionAgentGenerationActionState reads the stored state of one tool call,
// or nil when the receipt carries none.
func productionAgentGenerationActionState(usage map[string]any, toolCallID string) map[string]any {
	if usage == nil {
		return nil
	}
	return syntheticMap(syntheticMap(usage["agentActions"])[toolCallID])
}

// sourceStatus is the reference's currency answer (:137-139): current only when
// the root is neither cancelled nor blocked, and the frozen sources still
// reproduce the fingerprint the run froze.
func (a *productionAgentGenerationServe) sourceStatus(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, resolved *productionAgentGenerationRun) string {
	if a.owner.inner.registry == nil {
		log.Printf("novel-server: agent-generation sourceStatus=%v step=registry-missing", productionAgentGenerationStatusConflict)
		return productionAgentGenerationStatusConflict
	}
	budget, err := syntheticCall(ctx, ledger, "budget", syntheticString(resolved.Run["rootActionId"]))
	if err != nil {
		log.Printf("novel-server: agent-generation sourceStatus=%v step=budget-read", productionAgentGenerationStatusConflict)
		return productionAgentGenerationStatusConflict
	}
	if syntheticString(syntheticMap(syntheticMap(budget)["root"])["status"]) == "cancelled" ||
		syntheticString(syntheticMap(budget)["blockedCode"]) != "" {
		log.Printf("novel-server: agent-generation sourceStatus=%v step=root-stopped", productionAgentGenerationStatusConflict)
		return productionAgentGenerationStatusConflict
	}
	fingerprint := syntheticString(resolved.Manifest["goOwnerSourceFingerprint"])
	if fingerprint == "" {
		log.Printf("novel-server: agent-generation sourceStatus=%v step=fingerprint-missing", productionAgentGenerationStatusConflict)
		return productionAgentGenerationStatusConflict
	}
	runScope := scope
	if binding := syntheticMap(resolved.Run["binding"]); binding != nil {
		runScope.Project = syntheticString(binding["projectId"])
		runScope.Epoch = syntheticString(binding["epoch"])
	}
	leaseID := syntheticString(resolved.Manifest["modelLeaseId"])
	if leaseID == "" {
		log.Printf("novel-server: agent-generation sourceStatus=%v step=lease-missing", productionAgentGenerationStatusConflict)
		return productionAgentGenerationStatusConflict
	}
	set, err := a.owner.inner.rebuildSourcesForRun(ctx, db, resolved.Manifest)
	if err != nil {
		log.Printf("novel-server: agent-generation sourceStatus=%v step=rebuild", productionAgentGenerationStatusConflict)
		return productionAgentGenerationStatusConflict
	}
	reproduced, err := a.owner.inner.registry.ReproduceBinding(ctx, runScope, leaseID, set.Sources, set.Selection)
	if err != nil || reproduced == nil || reproduced.Fingerprint() != fingerprint {
		log.Printf("novel-server: agent-generation sourceStatus=%v step=reproduce", productionAgentGenerationStatusConflict)
		return productionAgentGenerationStatusConflict
	}
	return productionAgentGenerationStatusCurrent
}
