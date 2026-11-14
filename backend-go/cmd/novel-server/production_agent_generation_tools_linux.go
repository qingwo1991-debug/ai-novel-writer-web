//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"strconv"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/agentgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the six agent-generation:* tool names that were refused
// until the dispatch gate and the receipt writer landed. Each name re-proves the
// session run, decides through internal/agentgen, and writes its one receipt
// entry through the store's recordAgentReceipt port - never through the shared
// ordinary service's dispatch, so the agent protocol stays out of every other
// family's execute.

// The four refusal codes the reference throws that internal/agentgen does not
// carry as decisions; they are validations this file performs against the
// frozen context and the live lease before any decision is taken.
const (
	productionAgentGenerationConfirmationRequired    = "GENERATION_AGENT_CONFIRMATION_REQUIRED"
	productionAgentGenerationAuthorSelectionInvalid  = "GENERATION_AGENT_AUTHOR_SELECTION_INVALID"
	productionAgentGenerationAuthorSelectionConflict = "GENERATION_AGENT_AUTHOR_SELECTION_CONFLICT"
	productionAgentGenerationDomainToolNotAuthorized = "GENERATION_AGENT_DOMAIN_TOOL_NOT_AUTHORIZED"
	productionAgentGenerationDomainToolInvalid       = "GENERATION_AGENT_DOMAIN_TOOL_INVALID"
)

// operation is the per-name dispatch used by Bind in the serve file. It keeps
// the two begin/read handlers where they were and adds the six tool handlers
// without widening any other family's execute.
func (a *productionAgentGenerationServe) operation(name string) func(context.Context, transport.Caller, map[string]any, map[string]any) (any, error) {
	switch name {
	case productionAgentGenerationBegin:
		return a.begin
	case productionAgentGenerationRead:
		return a.read
	case productionAgentGenerationRound:
		return a.executeRound
	case productionAgentGenerationClaimTool:
		return a.claimTool
	case productionAgentGenerationFinishTool:
		return a.finishTool
	case productionAgentGenerationRegister:
		return a.registerWorkflow
	case productionAgentGenerationCommitDomain:
		return a.commitDomainTool
	case productionAgentGenerationResume:
		return a.resume
	}
	return nil
}

// productionAgentGenerationSafeInt reads an exact JavaScript-safe integer out of
// a decoded request member, refusing fractional, missing and out-of-range
// values the way Number.isSafeInteger does.
func productionAgentGenerationSafeInt(value any) (int64, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := number.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	case int64:
		return number, true
	case int:
		return int64(number), true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || number != math.Trunc(number) ||
			number > 9007199254740991 || number < -9007199254740991 {
			return 0, false
		}
		return int64(number), true
	}
	return 0, false
}

// productionAgentGenerationRoundRequest reads the reference's round envelope
// (electron/services/agent-generation.ts:180): a handle and a safe integer index.
func productionAgentGenerationRoundRequest(value map[string]any) (syntheticHandle, int64, error) {
	handle, err := productionAgentGenerationHandleRequest(value)
	if err != nil {
		return syntheticHandle{}, 0, err
	}
	raw, present := value["index"]
	if !present {
		return syntheticHandle{}, 0, errors.New(agentgen.CodeRoundInvalid)
	}
	index, ok := productionAgentGenerationSafeInt(raw)
	if !ok || index < 0 || index >= agentgen.MaxRounds {
		return syntheticHandle{}, 0, errors.New(agentgen.CodeRoundInvalid)
	}
	return handle, index, nil
}

// productionAgentGenerationRef reads a tool reference (src/shared/agent-generation.ts:44):
// the four-key handle, the attempt id and the tool call id.
func productionAgentGenerationRef(value any) (syntheticHandle, string, string, error) {
	object := syntheticMap(value)
	if object == nil {
		return syntheticHandle{}, "", "", errors.New(agentgen.CodeToolIdentityMismatch)
	}
	handle, err := productionAgentGenerationHandleRequest(object)
	if err != nil {
		return syntheticHandle{}, "", "", errors.New(agentgen.CodeToolIdentityMismatch)
	}
	attemptID := syntheticString(object["attemptId"])
	toolCallID := syntheticString(object["toolCallId"])
	if attemptID == "" || toolCallID == "" {
		return syntheticHandle{}, "", "", errors.New(agentgen.CodeToolIdentityMismatch)
	}
	return handle, attemptID, toolCallID, nil
}

// typedRecovery projects the session's rounds into the internal/agentgen shape
// the decisions take, including the currency and the root's block/cancel state.
func (a *productionAgentGenerationServe) typedRecovery(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, selected *productionAgentGenerationRun) (*agentgen.Recovery, error) {
	records, err := productionAgentGenerationSessionRuns(ctx, ledger, db, scope.Project, selected.Session.Key)
	if err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, errors.New(agentgen.CodeRootMismatch)
	}
	root := syntheticString(selected.Run["rootActionId"])
	rounds := make([]agentgen.Round, 0, len(records))
	for _, record := range records {
		if syntheticString(record.Run["rootActionId"]) != root {
			return nil, errors.New(agentgen.CodeRootMismatch)
		}
		projected, err := a.round(ctx, ledger, db, record)
		if err != nil {
			return nil, err
		}
		rounds = append(rounds, projected.Agent)
	}
	budgetValue, err := syntheticCall(ctx, ledger, "budget", root)
	if err != nil {
		return nil, err
	}
	budget := syntheticMap(budgetValue)
	rootStatus := syntheticString(syntheticMap(budget["root"])["status"])
	return &agentgen.Recovery{
		Session:       selected.Session,
		RootActionID:  root,
		SourceCurrent: a.sourceStatus(ctx, scope, ledger, db, records[len(records)-1]) == productionAgentGenerationStatusCurrent,
		Blocked:       syntheticString(budget["blockedCode"]) != "",
		Cancelled:     rootStatus == "cancelled",
		Rounds:        rounds,
	}, nil
}

// productionAgentGenerationToolNames is the validated tool manifest a round
// dispatches: the frozen input's own tool names, in frozen order. It is always
// non-nil so the dispatch service sees a protocol request even when the input
// declared no tools.
func productionAgentGenerationToolNames(context *agentgen.Context) []string {
	names := make([]string, 0, len(context.Input.Tools))
	for _, tool := range context.Input.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// productionAgentGenerationRoundMessages is the round's frozen task: the
// context's initial messages plus every prior round's assistant protocol and
// application tool results (electron/services/agent-generation.ts:200-204).
func productionAgentGenerationRoundMessages(context *agentgen.Context, rounds []agentgen.Round, index int64) ([]llm.Message, error) {
	messages := make([]llm.Message, 0, len(context.Messages)+len(rounds)*2)
	for _, message := range context.Messages {
		messages = append(messages, llm.Message{Role: message.Role, Content: message.Content})
	}
	for _, round := range rounds {
		if round.Index >= index {
			continue
		}
		protocol, err := agentgen.Protocol(round)
		if err != nil {
			return nil, err
		}
		for _, message := range protocol {
			messages = append(messages, llm.Message{Role: message.Role, Content: message.Content})
		}
	}
	return messages, nil
}

// productionAgentGenerationRequestHash is the deterministic idempotency key for
// one round dispatch: the same run, nonce, lease, fingerprint, task and plan
// always answer the same hash, so a replay finds its prior attempt.
func productionAgentGenerationRequestHash(run map[string]any, nonce, leaseID, fingerprint string, task generationowner.Task, plan generationowner.Plan) (string, error) {
	text, err := syntheticCanonical(struct {
		RunID   string               `json:"runId"`
		Nonce   string               `json:"invocationNonce"`
		LeaseID string               `json:"modelLeaseId"`
		Binding string               `json:"sourceFingerprint"`
		Task    generationowner.Task `json:"task"`
		Plan    generationowner.Plan `json:"plan"`
	}{syntheticString(run["runId"]), nonce, leaseID, fingerprint, task, plan})
	if err != nil {
		return "", errors.New("GENERATION_REQUEST_HASH_INVALID")
	}
	return syntheticHash(text), nil
}

// dispatchRound is the round's own dispatch: a dedicated generation.Service over
// the run's ledger and executor, never the shared ordinary service. The tool
// manifest is the only field a round sets that an ordinary execute cannot.
func (a *productionAgentGenerationServe) dispatchRound(ctx context.Context, caller transport.Caller, contextValue projectsession.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, run *productionAgentGenerationRun, messages []llm.Message, toolNames []string, index int64) error {
	manifest := run.Manifest
	leaseID := syntheticString(manifest["modelLeaseId"])
	if leaseID == "" {
		return errors.New("GENERATION_LEASE_MISSING")
	}
	if _, err := a.owner.inner.registry.Resolve(ctx, leaseID, scope, syntheticGenerationPurpose); err != nil {
		return err
	}
	set, err := a.owner.inner.rebuildSourcesForRun(ctx, db, manifest)
	if err != nil {
		return err
	}
	bindingValue, err := a.owner.inner.registry.BindSources(ctx, scope, leaseID, set.Sources, set.Selection)
	if err != nil {
		return err
	}
	if bindingValue.Fingerprint() != syntheticString(manifest["goOwnerSourceFingerprint"]) {
		return errors.New(agentgen.CodeSourceChanged)
	}
	task := generationowner.Task{
		Purpose:        "agent",
		Output:         productionAgentGenerationVisibleText,
		Messages:       messages,
		ReasoningStage: "general",
	}
	frozen, err := a.owner.inner.executor.Prepare(ctx, scope, leaseID, task, syntheticRootOutputTokens, bindingValue)
	if err != nil {
		return err
	}
	plan := frozen.Plan()
	usage, err := json.Marshal(plan.UsagePolicy)
	if err != nil {
		return errors.New("GENERATION_RESERVATION_INVALID")
	}
	var policy map[string]any
	if err := json.Unmarshal(usage, &policy); err != nil {
		return errors.New("GENERATION_RESERVATION_INVALID")
	}
	nonce := "agent-round:" + strconv.FormatInt(index, 10)
	taskRaw, err := json.Marshal(task)
	if err != nil {
		return errors.New("GENERATION_SEMANTIC_TASK_INVALID")
	}
	hash, err := productionAgentGenerationRequestHash(run.Run, nonce, leaseID, bindingValue.Fingerprint(), task, plan)
	if err != nil {
		return err
	}
	service := generation.New(generation.Dependencies{
		Ledger:   ledger,
		Dispatch: a.owner.inner.executor.Dispatch,
		OnSnapshot: func(snapshot generation.Snapshot) {
			a.owner.inner.publishSnapshot(caller, contextValue, run.Handle(), ledger, db, snapshot)
		},
	})
	_, err = service.Execute(context.WithoutCancel(ctx), generation.Request{
		RunID:                     syntheticString(run.Run["runId"]),
		InvocationNonce:           nonce,
		RequestHash:               hash,
		ProviderRequest:           frozen,
		ReservedTokens:            plan.Reserved,
		RequestedOutputTokens:     plan.Output,
		InputUpperBoundTokens:     plan.Input,
		ReasoningUpperBoundTokens: plan.Reasoning,
		UsagePolicy:               policy,
		Purpose:                   "agent",
		AgentToolNames:            toolNames,
		ReplayTask:                json.RawMessage(taskRaw),
		BudgetDecision:            plan.BudgetDecision,
	})
	return err
}

// openAgentRound opens a fresh round run (index > 0) under the session's root,
// freezing the same context the session began with and the parent root that
// keeps the round under one budget (electron/services/agent-generation.ts:193-198).
func (a *productionAgentGenerationServe) openAgentRound(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, input agentgen.Input, modelID, key string, roundIndex int64, parentRootActionID string) (map[string]any, error) {
	selection, err := syntheticParseSelection(map[string]any{
		"operation":                       agentgen.Operation,
		"uiActionNonce":                   productionAgentGenerationNoncePrefix + key + ":" + strconv.FormatInt(roundIndex, 10),
		"modelId":                         modelID,
		"parentRootActionId":              parentRootActionID,
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
		productionEditorInlineField{"parentRootActionId", productionEditorInlineQuote(parentRootActionID)},
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

// executeRound answers agent-generation:round. It re-proves the session,
// decides the round through internal/agentgen, and dispatches only an execute
// decision - with the frozen tool manifest - through this family's own service.
func (a *productionAgentGenerationServe) executeRound(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !a.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := a.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	if a.owner.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
	}
	contextValue, scope, ledger, db, done, err := a.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	handle, index, err := productionAgentGenerationRoundRequest(request)
	if err != nil {
		return nil, err
	}
	selected, err := a.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	recovery, err := a.typedRecovery(ctx, scope, ledger, db, selected)
	if err != nil {
		return nil, err
	}
	decision, err := agentgen.DecideRound(*recovery, index)
	if err != nil {
		return nil, err
	}
	if decision.Kind == agentgen.KindReplay {
		return a.recovery(ctx, scope, ledger, db, handle)
	}
	run := selected
	if index >= int64(len(recovery.Rounds)) {
		// A fresh round binds the next round index under the session's root
		// without another parent budget (agent-generation.ts:196-198).
		opened, err := a.openAgentRound(ctx, scope, ledger, db, selected.Context.Input, selected.ModelID, selected.Session.Key, index, selected.Handle().RootActionID)
		if err != nil {
			return nil, err
		}
		newHandle, err := productionEditorInlineRunHandle(scope.Project, scope.Epoch, opened)
		if err != nil {
			return nil, err
		}
		resolved, err := a.resolve(ctx, scope, ledger, db, newHandle)
		if err != nil {
			return nil, err
		}
		run = resolved
		handle = newHandle
	}
	messages, err := productionAgentGenerationRoundMessages(selected.Context, recovery.Rounds, index)
	if err != nil {
		return nil, err
	}
	if err := a.dispatchRound(ctx, caller, contextValue, scope, ledger, db, run, messages, productionAgentGenerationToolNames(selected.Context), index); err != nil {
		return nil, err
	}
	return a.recovery(ctx, scope, ledger, db, handle)
}

// productionAgentGenerationTool is one resolved action plus its stored state.
type productionAgentGenerationTool struct {
	Run        *productionAgentGenerationRun
	AttemptID  string
	ToolCallID string
	Action     agentgen.Action
	Usage      map[string]any
	State      map[string]any
}

// productionAgentGenerationStoredUsage reads one attempt's full usage receipt
// (the top-level object, where agentActions, agentAuthorSelections and
// agentControlEpoch live) over the lane's own connection. The receipt projection
// does not surface those top-level agent keys, so the tool names read them here.
func productionAgentGenerationStoredUsage(ctx context.Context, db *sql.DB, attemptID string) (map[string]any, error) {
	if db == nil {
		return nil, errors.New(agentgen.CodeToolIdentityMismatch)
	}
	var raw sql.NullString
	if err := db.QueryRowContext(ctx, "SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?", attemptID).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New(agentgen.CodeToolIdentityMismatch)
		}
		return nil, err
	}
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	var usage map[string]any
	if err := json.Unmarshal([]byte(raw.String), &usage); err != nil {
		return nil, errors.New(agentgen.CodeToolIdentityMismatch)
	}
	return usage, nil
}

// toolAction resolves a tool reference against the named run's completed round
// and merges the stored action-map state into the action the decisions take.
func (a *productionAgentGenerationServe) toolAction(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, refValue any) (*productionAgentGenerationTool, error) {
	handle, attemptID, toolCallID, err := productionAgentGenerationRef(refValue)
	if err != nil {
		return nil, err
	}
	run, err := a.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	attempts, err := productionAgentGenerationAttempts(ctx, db, syntheticString(run.Run["runId"]))
	if err != nil {
		return nil, err
	}
	if len(attempts) != 1 || attempts[0] != attemptID {
		return nil, errors.New(agentgen.CodeToolIdentityMismatch)
	}
	projected, err := a.round(ctx, ledger, db, run)
	if err != nil {
		return nil, err
	}
	if projected.Status != agentgen.RoundCompleted {
		return nil, errors.New(agentgen.CodeToolIdentityMismatch)
	}
	var action agentgen.Action
	found := false
	for index, candidate := range projected.Agent.Actions {
		if productionAgentGenerationActionIDPrefix+attemptID+":"+strconv.Itoa(index) == toolCallID {
			action = candidate
			found = true
			break
		}
	}
	if !found {
		return nil, errors.New(agentgen.CodeToolIdentityMismatch)
	}
	usage, err := productionAgentGenerationStoredUsage(ctx, db, attemptID)
	if err != nil {
		return nil, err
	}
	state := productionAgentGenerationActionState(usage, toolCallID)
	if state != nil {
		if status := syntheticString(state["status"]); status != "" {
			action.Status = status
		}
		if observation := syntheticString(state["observation"]); observation != "" {
			action.Observation = observation
		}
		if workflow := syntheticMap(state["workflow"]); workflow != nil {
			action.Registration = productionAgentGenerationRegistration(workflow)
		}
		if syntheticMap(state["domainEffect"]) != nil {
			action.HasDomainEffect = true
		}
	}
	return &productionAgentGenerationTool{Run: run, AttemptID: attemptID, ToolCallID: toolCallID, Action: action, Usage: usage, State: state}, nil
}

// productionAgentGenerationRegistration reads the stored workflow wire back into
// the internal registration the decisions compare.
func productionAgentGenerationRegistration(stored map[string]any) *agentgen.Registration {
	registration := &agentgen.Registration{
		ID:                 syntheticString(stored["registrationId"]),
		Workflow:           syntheticString(stored["workflow"]),
		ParentRootActionID: syntheticString(syntheticMap(stored["parentHandle"])["rootActionId"]),
		ModelID:            syntheticString(stored["modelId"]),
		State:              syntheticString(stored["state"]),
		ChildRunIDs:        syntheticStringList(stored["childHandles"]),
	}
	if value, present := stored["chapterNumber"]; present {
		registration.ChapterNumber = syntheticNumber(value)
		registration.HasChapterNumber = true
	}
	if planning := syntheticMap(stored["planning"]); planning != nil {
		registration.Planning = &agentgen.Planning{
			From:        syntheticNumber(planning["from"]),
			To:          syntheticNumber(planning["to"]),
			TargetUnits: syntheticNumber(planning["targetUnits"]),
		}
	}
	return registration
}

// productionAgentGenerationWorkflowWire renders a registration back to the wire
// shape register-workflow answers with (src/shared/agent-generation.ts:72).
func productionAgentGenerationWorkflowWire(registration agentgen.Registration) map[string]any {
	wire := map[string]any{
		"registrationId": registration.ID,
		"workflow":       registration.Workflow,
		"parentHandle":   map[string]any{"rootActionId": registration.ParentRootActionID},
		"modelId":        registration.ModelID,
		"state":          registration.State,
		"childHandles":   []any{},
	}
	if registration.HasChapterNumber {
		wire["chapterNumber"] = registration.ChapterNumber
	}
	if registration.Planning != nil {
		wire["planning"] = map[string]any{
			"from":        registration.Planning.From,
			"to":          registration.Planning.To,
			"targetUnits": registration.Planning.TargetUnits,
		}
	}
	return wire
}

// productionAgentGenerationToolWire renders one action to the wire shape the
// tool names answer with (src/shared/agent-generation.ts:49-56).
func productionAgentGenerationToolWire(handle syntheticHandle, attemptID, toolCallID string, action agentgen.Action) map[string]any {
	wire := map[string]any{
		"ref": map[string]any{
			"handle":     productionAgentGenerationWireHandle(handle),
			"attemptId":  attemptID,
			"toolCallId": toolCallID,
		},
		"name":      action.Name,
		"arguments": action.Arguments,
		"status":    action.Status,
	}
	if action.Observation != "" {
		wire["observation"] = action.Observation
	}
	if action.Registration != nil {
		wire["workflow"] = productionAgentGenerationWorkflowWire(*action.Registration)
	}
	return wire
}

// productionAgentGenerationMergeAction folds one action's next state into the
// full stored action map, preserving the entries the reference preserves
// (domainEffect and any planningArguments) that the wire action does not carry.
func productionAgentGenerationMergeAction(usage map[string]any, toolCallID string, next map[string]any) map[string]any {
	actions := syntheticMap(usage["agentActions"])
	if actions == nil {
		actions = map[string]any{}
	}
	merged := make(map[string]any, len(actions)+1)
	for key, value := range actions {
		merged[key] = value
	}
	merged[toolCallID] = next
	return merged
}

// productionAgentGenerationRootGates reads the live gates the claim, finish,
// register and commit decisions share: the root epoch against the caller's
// lease, the root's active status and the source currency.
func (a *productionAgentGenerationServe) productionAgentGenerationRootGates(ctx context.Context, scope generationowner.Scope, ledger *store.GenerationLedger, db *sql.DB, run *productionAgentGenerationRun) (epochCurrent, rootActive, sourcesCurrent bool, err error) {
	budgetValue, err := syntheticCall(ctx, ledger, "budget", syntheticString(run.Run["rootActionId"]))
	if err != nil {
		return false, false, false, err
	}
	budget := syntheticMap(budgetValue)
	root := syntheticMap(budget["root"])
	return syntheticString(root["epoch"]) == scope.Epoch, syntheticString(root["status"]) == "active",
		a.sourceStatus(ctx, scope, ledger, db, run) == productionAgentGenerationStatusCurrent, nil
}

// productionAgentGenerationToolKnown reports whether the frozen input registered
// a tool by name (agent-generation.ts:252).
func productionAgentGenerationToolKnown(context *agentgen.Context, name string) bool {
	for _, tool := range context.Input.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

// productionAgentGenerationBuiltinConfirmedTool reports whether the frozen input
// registered a tool as builtin, requiring confirmation and not read-only.
func productionAgentGenerationBuiltinConfirmedTool(context *agentgen.Context, name string) bool {
	for _, tool := range context.Input.Tools {
		if tool.Name == name && tool.Source == "builtin" && tool.RequiresConfirmation && !tool.IsReadOnly {
			return true
		}
	}
	return false
}

// productionAgentGenerationApply writes one action's next state into the action
// map on the run's receipt, in one effect transaction.
func (a *productionAgentGenerationServe) productionAgentGenerationApply(ctx context.Context, ledger *store.GenerationLedger, resolved *productionAgentGenerationTool, next map[string]any) (map[string]any, error) {
	actions := productionAgentGenerationMergeAction(resolved.Usage, resolved.ToolCallID, next)
	_, err := syntheticCall(ctx, ledger, "recordAgentReceipt",
		syntheticString(resolved.Run.Run["runId"]), resolved.AttemptID,
		map[string]any{"agentActions": actions}, []string{"agentActions"})
	if err != nil {
		return nil, err
	}
	return actions, nil
}

// claimTool answers agent-generation:claim-tool (agent-generation.ts:230-258).
func (a *productionAgentGenerationServe) claimTool(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !a.ready() {
		return nil, errProductionGenerationUnavailable
	}
	_, scope, ledger, db, done, err := a.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	confirmed, ok := request["confirmed"].(bool)
	if !ok {
		return nil, errors.New(productionAgentGenerationConfirmationRequired)
	}
	resolved, err := a.toolAction(ctx, scope, ledger, db, request["ref"])
	if err != nil {
		return nil, err
	}
	// The author's blueprint selections, when offered, are validated and written
	// beside the action map (agent-generation.ts:234-248).
	if raw, present := request["authorBlueprintProposals"]; present {
		selections, err := productionAgentGenerationAuthorSelections(raw)
		if err != nil {
			return nil, err
		}
		if resolved.Action.Name != "propose_novel_config" || !confirmed ||
			!productionAgentGenerationBuiltinConfirmedTool(resolved.Run.Context, "propose_chapter_blueprint") {
			return nil, errors.New(productionAgentGenerationAuthorSelectionInvalid)
		}
		var prior any
		if stored := syntheticMap(resolved.Usage["agentAuthorSelections"]); stored != nil {
			prior = stored[resolved.ToolCallID]
		}
		if (prior != nil && !productionAgentGenerationSame(prior, selections)) ||
			(prior == nil && resolved.Action.Status != agentgen.ActionPending) {
			return nil, errors.New(productionAgentGenerationAuthorSelectionConflict)
		}
		selectionsMap := syntheticMap(resolved.Usage["agentAuthorSelections"])
		if selectionsMap == nil {
			selectionsMap = map[string]any{}
		}
		selectionsMap[resolved.ToolCallID] = selections
		if _, err := syntheticCall(ctx, ledger, "recordAgentReceipt",
			syntheticString(resolved.Run.Run["runId"]), resolved.AttemptID,
			map[string]any{"agentAuthorSelections": selectionsMap}, []string{"agentAuthorSelections"}); err != nil {
			return nil, err
		}
	}
	epochCurrent, rootActive, sourcesCurrent, err := a.productionAgentGenerationRootGates(ctx, scope, ledger, db, resolved.Run)
	if err != nil {
		return nil, err
	}
	kind, err := agentgen.DecideClaim(resolved.Action, agentgen.ClaimRequest{
		Confirmed:      confirmed,
		EpochCurrent:   epochCurrent,
		SourcesCurrent: sourcesCurrent,
		RootActive:     rootActive,
		ToolKnown:      productionAgentGenerationToolKnown(resolved.Run.Context, resolved.Action.Name),
	})
	if err != nil {
		return nil, err
	}
	execute := false
	switch kind {
	case agentgen.ClaimNoop:
	case agentgen.ClaimUnknownTool:
		text := "Unknown tool: " + resolved.Action.Name
		if resolved.Run.Context.WritingLanguage == "en-US" {
			text = "Unknown tool: " + resolved.Action.Name
		} else {
			text = "未知工具：" + resolved.Action.Name
		}
		if _, err := a.productionAgentGenerationApply(ctx, ledger, resolved, map[string]any{"status": agentgen.ActionFailed, "observation": text}); err != nil {
			return nil, err
		}
	case agentgen.ClaimExecute:
		if _, err := a.productionAgentGenerationApply(ctx, ledger, resolved, map[string]any{"status": agentgen.ActionRunning}); err != nil {
			return nil, err
		}
		execute = true
	case agentgen.ClaimDeclined:
		if _, err := a.productionAgentGenerationApply(ctx, ledger, resolved, map[string]any{"status": agentgen.ActionDeclined, "observation": "The author declined this action."}); err != nil {
			return nil, err
		}
	}
	action := resolved.Action
	if kind == agentgen.ClaimExecute {
		action.Status = agentgen.ActionRunning
	} else if kind == agentgen.ClaimDeclined {
		action.Status = agentgen.ActionDeclined
		action.Observation = "The author declined this action."
	} else if kind == agentgen.ClaimUnknownTool {
		action.Status = agentgen.ActionFailed
		if resolved.Run.Context.WritingLanguage == "en-US" {
			action.Observation = "Unknown tool: " + resolved.Action.Name
		} else {
			action.Observation = "未知工具：" + resolved.Action.Name
		}
	}
	return map[string]any{
		"action":  productionAgentGenerationToolWire(resolved.Run.Handle(), resolved.AttemptID, resolved.ToolCallID, action),
		"execute": execute,
	}, nil
}

// finishTool answers agent-generation:finish-tool (agent-generation.ts:259-275).
func (a *productionAgentGenerationServe) finishTool(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !a.ready() {
		return nil, errProductionGenerationUnavailable
	}
	_, scope, ledger, db, done, err := a.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	status := syntheticString(request["status"])
	observation := syntheticString(request["observation"])
	if (status != agentgen.ActionCompleted && status != agentgen.ActionFailed && status != agentgen.StatusUnknown) ||
		len(observation) > agentgen.MaxObservationBytes {
		return nil, errors.New(agentgen.CodeToolResultInvalid)
	}
	resolved, err := a.toolAction(ctx, scope, ledger, db, request["ref"])
	if err != nil {
		return nil, err
	}
	epochCurrent, _, _, err := a.productionAgentGenerationRootGates(ctx, scope, ledger, db, resolved.Run)
	if err != nil {
		return nil, err
	}
	decision, err := agentgen.DecideFinish(resolved.Action, agentgen.FinishRequest{Status: status, Observation: observation, EpochCurrent: epochCurrent})
	if err != nil {
		return nil, err
	}
	if decision.Kind == agentgen.KindReplay {
		return productionAgentGenerationToolWire(resolved.Run.Handle(), resolved.AttemptID, resolved.ToolCallID, resolved.Action), nil
	}
	next := map[string]any{"status": status, "observation": observation}
	if resolved.Action.Registration != nil {
		next["workflow"] = productionAgentGenerationWorkflowWire(*resolved.Action.Registration)
	}
	if _, err := a.productionAgentGenerationApply(ctx, ledger, resolved, next); err != nil {
		return nil, err
	}
	resolved.Action.Status = status
	resolved.Action.Observation = observation
	return productionAgentGenerationToolWire(resolved.Run.Handle(), resolved.AttemptID, resolved.ToolCallID, resolved.Action), nil
}

// registerWorkflow answers agent-generation:register-workflow
// (agent-generation.ts:276-293).
func (a *productionAgentGenerationServe) registerWorkflow(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !a.ready() {
		return nil, errProductionGenerationUnavailable
	}
	_, scope, ledger, db, done, err := a.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	resolved, err := a.toolAction(ctx, scope, ledger, db, request["ref"])
	if err != nil {
		return nil, err
	}
	epochCurrent, rootActive, sourcesCurrent, err := a.productionAgentGenerationRootGates(ctx, scope, ledger, db, resolved.Run)
	if err != nil {
		return nil, err
	}
	registration, created, err := agentgen.Register(resolved.Action, resolved.ToolCallID, resolved.Run.ModelID,
		syntheticString(resolved.Run.Run["rootActionId"]), epochCurrent, sourcesCurrent, rootActive)
	if err != nil {
		return nil, err
	}
	if created {
		next := map[string]any{
			"status":   resolved.Action.Status,
			"workflow": productionAgentGenerationWorkflowWire(registration),
		}
		if resolved.Action.Observation != "" {
			next["observation"] = resolved.Action.Observation
		}
		if _, err := a.productionAgentGenerationApply(ctx, ledger, resolved, next); err != nil {
			return nil, err
		}
	}
	return productionAgentGenerationWorkflowWire(registration), nil
}

// commitDomainTool answers agent-generation:commit-domain-tool
// (agent-generation.ts:377-404). The reduced lane has no author-domain writer,
// so it stamps the domain-effect receipt entry without mutating author data.
func (a *productionAgentGenerationServe) commitDomainTool(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !a.ready() {
		return nil, errProductionGenerationUnavailable
	}
	_, scope, ledger, db, done, err := a.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	resolved, err := a.toolAction(ctx, scope, ledger, db, request["ref"])
	if err != nil {
		return nil, err
	}
	if existing := syntheticMap(resolved.State["domainEffect"]); existing != nil {
		return map[string]any{
			"kind":       syntheticString(existing["kind"]),
			"toolCallId": syntheticString(existing["toolCallId"]),
			"changes":    existing["changes"],
			"beforeHash": syntheticString(existing["beforeHash"]),
			"afterHash":  syntheticString(existing["afterHash"]),
			"current":    true,
		}, nil
	}
	if resolved.Action.Name != "propose_novel_config" && resolved.Action.Name != "propose_chapter_blueprint" {
		return nil, errors.New(productionAgentGenerationDomainToolNotAuthorized)
	}
	epochCurrent, rootActive, sourcesCurrent, err := a.productionAgentGenerationRootGates(ctx, scope, ledger, db, resolved.Run)
	if err != nil {
		return nil, err
	}
	if resolved.Action.Status != agentgen.ActionRunning || !epochCurrent || !rootActive || !sourcesCurrent ||
		!productionAgentGenerationBuiltinConfirmedTool(resolved.Run.Context, resolved.Action.Name) {
		return nil, errors.New(productionAgentGenerationDomainToolNotAuthorized)
	}
	effect, err := productionAgentGenerationDomainEffect(resolved.Action, resolved.ToolCallID,
		syntheticString(resolved.Run.Manifest["goOwnerSourceFingerprint"]))
	if err != nil {
		return nil, err
	}
	next := map[string]any{"status": resolved.Action.Status, "domainEffect": effect}
	if resolved.Action.Observation != "" {
		next["observation"] = resolved.Action.Observation
	}
	if resolved.Action.Registration != nil {
		next["workflow"] = productionAgentGenerationWorkflowWire(*resolved.Action.Registration)
	}
	if _, err := a.productionAgentGenerationApply(ctx, ledger, resolved, next); err != nil {
		return nil, err
	}
	return effect, nil
}

// productionAgentGenerationSame compares two decoded values by their canonical
// JSON, which is the deep equality the reference uses for author selections.
func productionAgentGenerationSame(a, b any) bool {
	aText, aErr := syntheticCanonical(a)
	bText, bErr := syntheticCanonical(b)
	return aErr == nil && bErr == nil && aText == bText
}

// productionAgentGenerationAuthorSelections validates the author's blueprint
// selections (agent-generation.ts:236-240): an array of at most 100 proposals,
// each exactly {name: propose_chapter_blueprint, arguments: object}, under the
// reference's byte ceiling.
func productionAgentGenerationAuthorSelections(raw any) ([]map[string]any, error) {
	values := syntheticSlice(raw)
	if values == nil || len(values) > 100 {
		return nil, errors.New(productionAgentGenerationAuthorSelectionInvalid)
	}
	selections := make([]map[string]any, 0, len(values))
	for _, item := range values {
		object := syntheticMap(item)
		if object == nil || len(object) != 2 || syntheticString(object["name"]) != "propose_chapter_blueprint" {
			return nil, errors.New(productionAgentGenerationAuthorSelectionInvalid)
		}
		if _, ok := object["name"].(string); !ok {
			return nil, errors.New(productionAgentGenerationAuthorSelectionInvalid)
		}
		if _, ok := object["arguments"].(map[string]any); !ok {
			return nil, errors.New(productionAgentGenerationAuthorSelectionInvalid)
		}
		selections = append(selections, object)
	}
	encoded, err := syntheticCanonical(selections)
	if err != nil || len(encoded) > 256*1024 {
		return nil, errors.New(productionAgentGenerationAuthorSelectionInvalid)
	}
	return selections, nil
}

// productionAgentGenerationDomainEffect builds the domain-effect receipt the
// commit stamps. No author data is written, so before and after share the
// run's own source fingerprint.
func productionAgentGenerationDomainEffect(action agentgen.Action, toolCallID, fingerprint string) (map[string]any, error) {
	changes := syntheticMap(action.Arguments["changes"])
	if changes == nil || len(changes) == 0 {
		return nil, errors.New(productionAgentGenerationDomainToolInvalid)
	}
	kind := "config"
	if action.Name == "propose_chapter_blueprint" {
		kind = "blueprint"
	}
	effect := map[string]any{
		"kind":       kind,
		"toolCallId": toolCallID,
		"changes":    changes,
		"beforeHash": fingerprint,
		"afterHash":  fingerprint,
		"current":    true,
	}
	if kind == "blueprint" {
		chapter := syntheticNumber(action.Arguments["chapter_number"])
		if chapter < 1 {
			return nil, errors.New(productionAgentGenerationDomainToolInvalid)
		}
		effect["chapterNumber"] = chapter
	}
	return effect, nil
}

// resume answers agent-generation:resume (agent-generation.ts:156-174): it
// re-admits the paused run through the root-only activate port (never the
// ordinary binding-rewriting resume) and, when the last round already
// completed, patches the control epoch onto the receipt.
func (a *productionAgentGenerationServe) resume(ctx context.Context, caller transport.Caller, expected map[string]any, request map[string]any) (any, error) {
	if !a.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := a.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	if a.owner.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
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
	recovery, err := a.recovery(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	wire := syntheticMap(recovery)
	if syntheticString(wire["sourceStatus"]) != productionAgentGenerationStatusCurrent {
		return nil, errors.New(agentgen.CodeSourceChanged)
	}
	runStatus := syntheticString(syntheticMap(wire["run"])["status"])
	rounds := syntheticSlice(wire["rounds"])
	lastCompleted := false
	attemptID := ""
	if len(rounds) > 0 {
		last := syntheticMap(rounds[len(rounds)-1])
		lastCompleted = syntheticString(last["status"]) == agentgen.RoundCompleted
		attemptID = syntheticString(last["attemptId"])
	}
	// A finished response resumes control without rewriting its historical input
	// binding (agent-generation.ts:160-170); an unfinished round resumes only a
	// paused run. Both re-admit through the root-only port: the ordinary resume
	// re-encodes the binding_json and would break the frozen agent context digest.
	if !lastCompleted && runStatus != "paused" {
		return nil, errors.New("GENERATION_RESUME_UNAVAILABLE")
	}
	if runStatus == "paused" {
		if _, err := syntheticCall(ctx, ledger, "activateRoot", handle.RootActionID); err != nil {
			return nil, err
		}
	}
	if lastCompleted {
		if attemptID == "" {
			return nil, errors.New(agentgen.CodeRoundAmbiguous)
		}
		if _, err := syntheticCall(ctx, ledger, "recordAgentReceipt",
			handle.RunID, attemptID, map[string]any{"agentControlEpoch": scope.Epoch}, []string{"agentControlEpoch"}); err != nil {
			return nil, err
		}
	}
	return a.recovery(ctx, scope, ledger, db, handle)
}
