//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves legacy-roster:execute: the six-step initial/repair loop at
// electron/services/main-generation-owner.ts:1143-1177, over the run
// legacy-roster:begin froze.
//
//	for (const stage of ['initial','repair'] as const)
//	  for (let ordinal = 0; ordinal < 3; ordinal++, total++)
//	    task = previousText === undefined ? stageTask : buildLegacyRosterReplacementTask(stageTask, previousText, language)
//	    ... dispatch, read the visible candidate, check composition eligibility ...
//	    if (finishReason === 'length') { previousText = candidate.text; continue }
//	    try { parseLegacyRosterJson(candidate.text); return result } catch {
//	      stageTask = buildLegacyRosterJsonRepairTask(candidate.text); break }
//
// Every prose fragment the loop composes now ships as a committed asset:
//
//   - the initial task is the run's own frozen legacyRosterTask, re-proved by
//     resolve against LEGACY_ROSTER_SYSTEM_PROMPT + CHARACTER_ROSTER_JSON_CONTRACT
//     (internal/prompttemplate/legacy-roster-v1.json), so the first dispatch
//     re-uses the shipped roster prompt rather than authoring it again;
//   - the repair task is CHARACTER_ROSTER_JSON_REPAIR_SYSTEM plus the
//     <invalid-json> wrapper around the unparsable answer, and the replacement
//     task is buildStructuredReplacementPrompt in both writing languages
//     (internal/prompttemplate/legacy-roster-loop-v1.json). Nothing in this file
//     writes prompt text: the two tasks are assembled from the loader, and the
//     lane's tests read the dispatched attempt's replayTask back out of the
//     ledger to prove it carries the asset.
//
// The dispatch/readback terms are the sibling families': the run is resolved
// through the family's own proof, the begin-time lease is re-resolved, the frozen
// sources are reproduced and compared to the manifest fingerprint, the task is
// planned by the shared executor, and one attempt is settled through the ordinary
// service under the reference's nonce (`legacy:<stage>:<ordinal>`). A replay is
// answered by the ledger's own findInvocation before the attempt-count guard, so
// re-posting the same handle re-reads the settled attempt instead of dialing a
// second model call.
//
// Two terms are deliberately stricter than the desktop's loop, and both are
// recorded here rather than hidden:
//
//   - the settle gate is parseLegacyRosterJson *and* the candidate closure
//     validateLegacyRosterCandidate enforces on a legacy_repair candidate
//     (electron/repositories/character-roster-repository.ts:307-311): a
//     schemaVersion-1 object with non-empty entries, known roles, unique identity
//     keys, no self-reference, unique relationships and every relationship target
//     naming another entry. The desktop applies that closure one step later, in
//     the artifact proof; this lane has no stage yet, so settling a candidate that
//     could never be proved would answer a run the browser can never advance.
//     Scalar field normalization (requiredText over every field, normalizeState)
//     is NOT ported here and remains the stage proof's job;
//   - the run is refused with GENERATION_LEGACY_EFFECT_SEALED when any of its
//     attempts already carries a legacyRosterProposal effect, which is
//     LegacyRosterGeneration.assertMutable (electron/services/legacy-roster-generation.ts:49-51).
//     Without it a desktop run that had already been staged could be re-dispatched
//     by this lane.
//
// legacy-roster:stage is untouched and stays refused; the artifact proof and the
// proposal writer it needs are still missing (production_legacy_roster_execute_linux.go).

const (
	// The reference's two stages and its three attempts per stage
	// (main-generation-owner.ts:1143, :1145).
	productionLegacyRosterInitialStage = "initial"
	productionLegacyRosterRepairStage  = "repair"
	productionLegacyRosterStageAttempts = 3
	// productionLegacyRosterRepairPurpose is buildLegacyRosterJsonRepairTask's
	// purpose (legacy-roster-generation-pure.ts:69) and its output contract
	// (:71).
	productionLegacyRosterRepairPurpose = "legacy-character-roster-json-repair"
	productionLegacyRosterRepairOutput  = "structured-data"
	productionLegacyRosterRepairReasoningStage = "planning"
	// productionLegacyRosterSchemaVersion is CHARACTER_ROSTER_SCHEMA_VERSION
	// (src/shared/character-roster.ts:10).
	productionLegacyRosterSchemaVersion = 1
	// The refusals this file raises itself. The first three are the reference's
	// own codes (main-generation-owner.ts:1160, :1167-1168); the candidate code is
	// validateLegacyRosterCandidate's boundary, and the sealed code is
	// assertMutable's.
	productionLegacyRosterAttemptConflict  = "GENERATION_LEGACY_ATTEMPT_CONFLICT"
	productionLegacyRosterArtifactInvalid  = "GENERATION_LEGACY_ARTIFACT_INVALID"
	productionLegacyRosterCandidateInvalid = "GENERATION_LEGACY_CANDIDATE_INVALID"
	productionLegacyRosterEffectSealed     = "GENERATION_LEGACY_EFFECT_SEALED"
	// productionLegacyRosterSourceChanged is the reduced lane's code for a
	// fingerprint the frozen sources no longer reproduce.
	productionLegacyRosterSourceChanged = "GENERATION_SOURCE_CHANGED"
	// The two finish reasons the loop distinguishes (:1163-1171).
	productionLegacyRosterFinishLength = "length"
	productionLegacyRosterFinishStop   = "stop"
	productionLegacyRosterStatusCompleted = "completed"
)

// The reference strips a Markdown fence before it parses (legacy-roster-generation-pure.ts:89).
var (
	productionLegacyRosterJSONFence = regexp.MustCompile("(?i)```json?\n?")
	productionLegacyRosterCodeFence = regexp.MustCompile("```\n?")
)

// productionLegacyRosterExecuteServe serves legacy-roster:execute over the one
// generation owner. It borrows the begin/read/cancel serve for the run proof and
// the source reproduction, so the two handlers cannot disagree about what a
// legacy-roster run is.
type productionLegacyRosterExecuteServe struct {
	owner  *productionGeneration
	roster *productionLegacyRosterServe
}

// newProductionLegacyRosterExecuteServe is the constructor
// production_open_linux.go wires beside newProductionLegacyRosterServe.
func newProductionLegacyRosterExecuteServe(owner *productionGeneration) *productionLegacyRosterExecuteServe {
	return &productionLegacyRosterExecuteServe{owner: owner, roster: newProductionLegacyRosterServe(owner)}
}

// ready reports whether a dispatch could leave at all: the family's admission, a
// model egress, and the executor the attempt is settled through.
func (e *productionLegacyRosterExecuteServe) ready() bool {
	return e != nil && e.owner != nil && e.owner.open != nil && e.owner.inner != nil && e.owner.inner.executor != nil
}

// Bind installs legacy-roster:execute. A nil owner installs nothing, so the name
// keeps failing closed at GO_OPERATION_NOT_MIGRATED, exactly like the sibling
// binders.
func (e *productionLegacyRosterExecuteServe) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if e == nil || e.owner == nil || e.owner.inner == nil {
		return nil
	}
	// The request is exactly `{ handle }` plus the frozen context, and the
	// reference refuses any other request key with GENERATION_LEGACY_REQUEST_INVALID
	// (main-generation-owner.ts:1139), which is the family's existing channel.
	return server.Bind(productionLegacyRosterExecuteName, productionLegacyRosterChannel(e.execute))
}

// execute answers legacy-roster:execute. It resolves the run, takes the task it
// froze, and walks the reference's two stages: a fresh dispatch per ordinal, a
// replacement prompt after a length stop, and a repair task after an answer the
// candidate gate refuses.
func (e *productionLegacyRosterExecuteServe) execute(ctx context.Context, caller transport.Caller, expected map[string]any, handle syntheticHandle) (any, error) {
	if !e.ready() {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := e.owner.open.ModelEgress(); err != nil {
		return nil, err
	}
	contextValue, scope, ledger, db, done, err := e.owner.inner.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	resolved, err := e.roster.resolve(ctx, scope, ledger, db, handle)
	if err != nil {
		return nil, err
	}
	taskText, err := productionLegacyRosterStoredMember(ctx, db, handle.RunID, "legacyRosterTask")
	if err != nil {
		return nil, err
	}
	initial, err := generationowner.ParseTask(taskText)
	if err != nil {
		return nil, errors.New(productionLegacyRosterTaskInvalid)
	}
	loop, err := prompttemplate.LegacyRosterLoopTemplate()
	if err != nil {
		return nil, errors.New(productionLegacyRosterTaskInvalid)
	}
	language := productionLegacyRosterWritingLanguage(syntheticMap(resolved.Context["source"])["writingLanguage"])
	// The begin-time binding is re-derived, not trusted: a run whose frozen sources
	// no longer reproduce must not dispatch, exactly like the sibling executes.
	binding, fingerprint, err := e.roster.reproduce(ctx, scope, db, resolved)
	if err != nil {
		return nil, err
	}
	if fingerprint != syntheticString(resolved.Manifest["goOwnerSourceFingerprint"]) {
		return nil, errors.New(productionLegacyRosterSourceChanged)
	}
	leaseID := syntheticString(resolved.Manifest["modelLeaseId"])
	if leaseID == "" {
		return nil, errors.New(productionEditorInlineLeaseMissing)
	}
	service, err := e.owner.inner.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	stageTask := initial
	var repairTask generationowner.Task
	hasRepair := false
	// iterations counts loop iterations, not attempts: the reference's `total` is
	// incremented by the for-update, so a replayed ordinal counts too and the
	// attempt-count guard still lines up after a partial replay.
	iterations := 0
	for stageIndex, stage := range []string{productionLegacyRosterInitialStage, productionLegacyRosterRepairStage} {
		if stageIndex > 0 {
			if !hasRepair {
				return nil, errors.New(productionLegacyRosterAttemptConflict)
			}
			stageTask = repairTask
		}
		previous := ""
		hasPrevious := false
		for ordinal := 0; ordinal < productionLegacyRosterStageAttempts; ordinal++ {
			task := stageTask
			if hasPrevious {
				replacement, err := loop.Replacement(language)
				if err != nil {
					return nil, errors.New(productionLegacyRosterTaskInvalid)
				}
				rendered, err := productionLegacyRosterReplacementTask(stageTask, replacement.Render(productionLegacyRosterTaskUserPrompt(stageTask), previous))
				if err != nil {
					return nil, err
				}
				task = rendered
			}
			nonce := productionLegacyRosterInvocationNonce(stage, ordinal)
			frozen, err := e.owner.inner.executor.Prepare(ctx, scope, leaseID, task, syntheticRootOutputTokens, binding)
			if err != nil {
				return nil, err
			}
			requestHash, err := productionLegacyRosterRequestHash(handle.RunID, nonce, leaseID, fingerprint, task, frozen.Plan())
			if err != nil {
				return nil, err
			}
			prior, err := syntheticCall(ctx, ledger, "findInvocation", handle.RunID, nonce, requestHash)
			if err != nil {
				return nil, err
			}
			if prior == nil {
				// Only a fresh attempt is sealed-guarded and counted; a replayed
				// ordinal already owns its attempt row.
				if err := productionLegacyRosterAssertMutable(ctx, db, handle.RunID); err != nil {
					return nil, err
				}
				attempts, err := syntheticCall(ctx, ledger, "attemptCount", handle.RunID)
				if err != nil {
					return nil, err
				}
				used, ok := syntheticFloat64(attempts)
				if !ok || int(used) != iterations {
					return nil, errors.New(productionLegacyRosterAttemptConflict)
				}
			}
			receiptValue, err := service.Execute(ctx, frozen, handle.RunID, nonce)
			if err != nil {
				return nil, err
			}
			iterations++
			receipt := syntheticMap(receiptValue)
			if receipt == nil {
				return nil, errors.New(productionLegacyRosterArtifactInvalid)
			}
			outcome := syntheticOutcome(receipt)
			answer, err := e.answer(ctx, ledger, db, receipt)
			if err != nil {
				return nil, err
			}
			snapshot, eligible := e.owner.inner.snapshotOf(ctx, db, receipt, syntheticMap(resolved.Run["binding"]))
			if !eligible {
				return answer, nil
			}
			text := syntheticString(snapshot["text"])
			if syntheticHash(text) != syntheticString(snapshot["textHash"]) {
				return nil, errors.New(productionLegacyRosterArtifactInvalid)
			}
			finish := syntheticString(outcome["finishReason"])
			if finish == productionLegacyRosterFinishLength {
				if ordinal == productionLegacyRosterStageAttempts-1 {
					return answer, nil
				}
				previous = text
				hasPrevious = true
				continue
			}
			if finish != productionLegacyRosterFinishStop || syntheticString(outcome["status"]) != productionLegacyRosterStatusCompleted {
				return answer, nil
			}
			if err := productionLegacyRosterCandidateFromText(text); err != nil {
				if stage == productionLegacyRosterRepairStage {
					return answer, nil
				}
				built, err := productionLegacyRosterRepairTask(loop, text)
				if err != nil {
					return nil, err
				}
				repairTask = built
				hasRepair = true
				break
			}
			return answer, nil
		}
	}
	return nil, errors.New(productionLegacyRosterAttemptConflict)
}

// answer projects one settled attempt into the browser's
// MainGenerationExecuteReceipt: the outcome and the run view, exactly like the
// sibling families.
func (e *productionLegacyRosterExecuteServe) answer(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, receipt map[string]any) (any, error) {
	outcome := syntheticOutcome(receipt)
	view, err := e.owner.inner.viewOf(ctx, ledger, db, syntheticMap(receipt["run"]))
	if err != nil {
		return nil, err
	}
	return map[string]any{"outcome": outcome, "run": view}, nil
}

// productionLegacyRosterTaskUserPrompt is the base task's second message, the
// text buildLegacyRosterReplacementTask hands to buildStructuredReplacementPrompt.
func productionLegacyRosterTaskUserPrompt(task generationowner.Task) string {
	if len(task.Messages) < 2 {
		return ""
	}
	return task.Messages[1].Content
}

// productionLegacyRosterReplacementTask is buildLegacyRosterReplacementTask
// (legacy-roster-generation-pure.ts:79-85): the base task with its second message
// replaced, every other message unchanged.
func productionLegacyRosterReplacementTask(base generationowner.Task, prompt string) (generationowner.Task, error) {
	if len(base.Messages) < 2 {
		return generationowner.Task{}, errors.New(productionLegacyRosterTaskInvalid)
	}
	messages := make([]llm.Message, len(base.Messages))
	copy(messages, base.Messages)
	messages[1].Content = prompt
	base.Messages = messages
	return base, nil
}

// productionLegacyRosterRepairTask is buildLegacyRosterJsonRepairTask
// (legacy-roster-generation-pure.ts:67-77): the shipped repair system prompt and
// the shipped wrapper around the answer that did not parse.
func productionLegacyRosterRepairTask(loop prompttemplate.LegacyRosterLoopPrompt, rawText string) (generationowner.Task, error) {
	user, err := loop.Repair(rawText)
	if err != nil {
		return generationowner.Task{}, errors.New(productionLegacyRosterTaskInvalid)
	}
	if loop.RepairSystem == "" {
		return generationowner.Task{}, errors.New(productionLegacyRosterTaskInvalid)
	}
	return generationowner.Task{
		Purpose:        productionLegacyRosterRepairPurpose,
		ReasoningStage: productionLegacyRosterRepairReasoningStage,
		Output:         productionLegacyRosterRepairOutput,
		Messages: []llm.Message{
			{Role: "system", Content: loop.RepairSystem},
			{Role: "user", Content: user},
		},
	}, nil
}

// productionLegacyRosterInvocationNonce is the reference's per-iteration nonce
// (main-generation-owner.ts:1147), which is what makes a replay land on the
// attempt it already settled.
func productionLegacyRosterInvocationNonce(stage string, ordinal int) string {
	return "legacy:" + stage + ":" + strconv.Itoa(ordinal)
}

// productionLegacyRosterRequestHash is the hash generationowner.OrdinaryService
// stores on the attempt, rebuilt here so the loop can ask findInvocation whether
// this ordinal already settled.
func productionLegacyRosterRequestHash(runID, nonce, leaseID, fingerprint string, task generationowner.Task, plan generationowner.Plan) (string, error) {
	hash := productionGenerationRequestHash(struct {
		RunID   string               `json:"runId"`
		Nonce   string               `json:"invocationNonce"`
		LeaseID string               `json:"modelLeaseId"`
		Binding string               `json:"sourceFingerprint"`
		Task    generationowner.Task `json:"task"`
		Plan    generationowner.Plan `json:"plan"`
	}{runID, nonce, leaseID, fingerprint, task, plan})
	if hash == "" {
		return "", errors.New(productionLegacyRosterTaskInvalid)
	}
	return hash, nil
}

// productionLegacyRosterAssertMutable is LegacyRosterGeneration.assertMutable
// (electron/services/legacy-roster-generation.ts:49-51): a run whose attempt
// already carries the legacyRosterProposal effect is sealed and must not be
// re-dispatched.
func productionLegacyRosterAssertMutable(ctx context.Context, db *sql.DB, runID string) error {
	if db == nil {
		return errors.New(productionLegacyRosterEffectSealed)
	}
	rows, err := db.QueryContext(ctx, `SELECT usage_receipt_json FROM generation_attempts WHERE run_id=?`, runID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var usage map[string]any
		if err := json.Unmarshal([]byte(raw), &usage); err != nil {
			return errors.New(productionLegacyRosterEffectSealed)
		}
		if _, sealed := usage["legacyRosterProposal"]; sealed {
			return errors.New(productionLegacyRosterEffectSealed)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

// productionLegacyRosterCandidateFromText is the loop's settle gate:
// parseLegacyRosterJson first, then the candidate closure.
func productionLegacyRosterCandidateFromText(text string) error {
	value, err := productionLegacyRosterParseJSON(text)
	if err != nil {
		return err
	}
	return productionLegacyRosterCandidate(value)
}

// productionLegacyRosterParseJSON is parseLegacyRosterJson
// (legacy-roster-generation-pure.ts:87-103) without its localized message: a
// refusal is the symbolic code the lane maps, never prose.
func productionLegacyRosterParseJSON(text string) (any, error) {
	clean := productionLegacyRosterJSONFence.ReplaceAllString(text, "")
	clean = productionLegacyRosterCodeFence.ReplaceAllString(clean, "")
	clean = strings.TrimSpace(clean)
	firstBrace := strings.Index(clean, "{")
	firstBracket := strings.Index(clean, "[")
	lastBrace := strings.LastIndex(clean, "}")
	lastBracket := strings.LastIndex(clean, "]")
	switch {
	case firstBrace != -1 && lastBrace != -1 && (firstBracket == -1 || firstBrace < firstBracket):
		clean = clean[firstBrace : lastBrace+1]
	case firstBracket != -1 && lastBracket != -1:
		clean = clean[firstBracket : lastBracket+1]
	}
	var value any
	if err := json.Unmarshal([]byte(clean), &value); err != nil {
		return nil, errors.New(productionLegacyRosterCandidateInvalid)
	}
	return value, nil
}

// productionLegacyRosterCandidate is the candidate closure
// validateLegacyRosterCandidate (electron/repositories/character-roster-repository.ts:
// 307-311) applies with intent legacy_repair: the schema version, a non-empty
// entries array, and per entry the name, role, relationship shape and the local
// closure checks normalizeEntry and normalizeRequest make. The scalar text
// normalizer is not repeated here; the artifact proof owns it.
func productionLegacyRosterCandidate(value any) error {
	invalid := errors.New(productionLegacyRosterCandidateInvalid)
	object := syntheticMap(value)
	if object == nil {
		return invalid
	}
	if version, ok := syntheticFloat64(object["schemaVersion"]); !ok || version != productionLegacyRosterSchemaVersion {
		return invalid
	}
	entries := syntheticSlice(object["entries"])
	if len(entries) == 0 {
		return invalid
	}
	names := make(map[string]bool, len(entries))
	targets := make([][]string, 0, len(entries))
	for _, rawEntry := range entries {
		entry := syntheticMap(rawEntry)
		if entry == nil {
			return invalid
		}
		name, ok := entry["name"].(string)
		if !ok {
			return invalid
		}
		name = strings.TrimSpace(name)
		if name == "" {
			return invalid
		}
		key := strings.ToLower(name)
		if names[key] {
			return invalid
		}
		names[key] = true
		if !productionLegacyRosterRoleAllowed(entry["role"]) {
			return invalid
		}
		relationships := syntheticSlice(entry["relationships"])
		if relationships == nil {
			return invalid
		}
		seen := make(map[string]bool, len(relationships))
		entryTargets := make([]string, 0, len(relationships))
		for _, rawRelationship := range relationships {
			relationship := syntheticMap(rawRelationship)
			if relationship == nil {
				return invalid
			}
			target, ok := relationship["target"].(string)
			if !ok {
				return invalid
			}
			target = strings.TrimSpace(target)
			relation, ok := relationship["relation"].(string)
			if !ok {
				return invalid
			}
			relation = strings.TrimSpace(relation)
			if target == "" || relation == "" {
				return invalid
			}
			if target == name {
				return invalid
			}
			pair := target + "\x00" + relation
			if seen[pair] {
				return invalid
			}
			seen[pair] = true
			entryTargets = append(entryTargets, target)
		}
		targets = append(targets, entryTargets)
	}
	for _, entryTargets := range targets {
		for _, target := range entryTargets {
			if !names[strings.ToLower(target)] {
				return invalid
			}
		}
	}
	return nil
}

// productionLegacyRosterRoleAllowed is isRosterRole: the four shipped role values.
func productionLegacyRosterRoleAllowed(value any) bool {
	role, ok := value.(string)
	if !ok {
		return false
	}
	switch role {
	case "protagonist", "antagonist", "supporting", "minor":
		return true
	default:
		return false
	}
}
