//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves generation:retry-draft-short-outline faithfully, not by
// authoring a new prompt. The reference (electron/services/main-generation-owner.ts:
// 178-200, :683-688) re-dispatches the task the failed attempt froze: it reads the
// attempt's usage receipt, takes usage.replayTask, proves it against the material
// decision's shortOutlinePromptHash and the stored request hash, then re-dispatches
// that exact task under a retry nonce.
//
// OrdinaryService.Execute now records the frozen task as the attempt's replayTask
// (internal/generationowner/ordinary.go), so the readback below is the same task
// the failed dispatch froze. The proof is the same two checks the reference makes:
// the single user prompt must hash to the material decision's short-outline hash,
// and the task must re-hash - together with the run, the original invocation nonce,
// the model lease and the re-derived plan - to the stored request hash. A tampered
// attempt therefore fails the second check and is refused, never re-dispatched.

// productionGenerationRetryName is the operation name this file serves.
const productionGenerationRetryName = "generation:retry-draft-short-outline"

// productionGenerationRequestHash reimplements generationowner.hashJSON for this
// file's proof: sha256 of deterministic JSON with the same HTML/U+2028/U+2029
// escaping the dispatch used when it computed the stored request hash.
func productionGenerationRequestHash(value any) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(value) != nil {
		return ""
	}
	raw := bytes.TrimSuffix(buf.Bytes(), []byte{'\n'})
	raw = bytes.ReplaceAll(raw, []byte(`\u2028`), []byte("\u2028"))
	raw = bytes.ReplaceAll(raw, []byte(`\u2029`), []byte("\u2029"))
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// RetryDraftShortOutline answers the retry name under the same committed gates as
// the rest of the generation write batch, then re-dispatches the proven frozen
// task through the shared ordinary service.
func (g *productionGeneration) RetryDraftShortOutline(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if err := g.mutation(); err != nil {
		return nil, err
	}
	return productionRetryDraftShortOutline(ctx, g.inner, caller, expected, arg)
}

// productionRetryDraftShortOutline is the whole read-prove-redispatch body. It is
// a free function over the reused synthetic owner so the production lane never
// constructs a second executor or a second admission path.
func productionRetryDraftShortOutline(ctx context.Context, s *syntheticGeneration, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	request := syntheticMap(arg)
	if request == nil {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	handle, err := syntheticParseHandle(request["handle"])
	if err != nil {
		return nil, err
	}
	failedAttemptID := syntheticString(request["failedAttemptId"])
	if failedAttemptID == "" {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	contextValue, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	run, _, manifest, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	if manifest["operation"] != "chapter-draft" {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	decision := syntheticMap(manifest["materialDecision"])
	if decision == nil || syntheticString(decision["shortOutlinePromptHash"]) == "" {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE")
	}
	// The availability gate is the same one the renderer applies before a retry:
	// the failed attempt must be the run's first, a settled short-outline attempt
	// with no failure code and a length (or empty) stop.
	if err := s.syntheticAssertShortOutlineFailure(ctx, ledger, db, run, failedAttemptID); err != nil {
		return nil, err
	}
	task, nonce, storedHash, err := productionRetryReadFrozenTask(ctx, db, failedAttemptID)
	if err != nil {
		return nil, err
	}
	if task.Purpose != syntheticRetryPurpose || task.Output != "visible-text" {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID")
	}
	var userPrompts []string
	for _, message := range task.Messages {
		if message.Role == "user" {
			userPrompts = append(userPrompts, message.Content)
		}
	}
	if len(userPrompts) != 1 || syntheticHash(userPrompts[0]) != syntheticString(decision["shortOutlinePromptHash"]) {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID")
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
	frozen, err := s.executor.Prepare(ctx, scope, leaseID, task, syntheticRootOutputTokens, bindingValue)
	if err != nil {
		return nil, err
	}
	// The second proof: the stored request hash. The dispatch hashed the same
	// struct (ordinary.go) over the original nonce and the frozen plan; rebuilding
	// both here proves the stored task is the one that was originally dispatched.
	proofHash := productionGenerationRequestHash(struct {
		RunID   string               `json:"runId"`
		Nonce   string               `json:"invocationNonce"`
		LeaseID string               `json:"modelLeaseId"`
		Binding string               `json:"sourceFingerprint"`
		Task    generationowner.Task `json:"task"`
		Plan    generationowner.Plan `json:"plan"`
	}{handle.RunID, nonce, leaseID, bindingValue.Fingerprint(), task, frozen.Plan()})
	if proofHash == "" || proofHash != storedHash {
		return nil, errors.New("GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID")
	}
	service, err := s.newService(ctx, caller, contextValue, ledger, db, scope, handle)
	if err != nil {
		return nil, err
	}
	receipt, err := service.Execute(ctx, frozen, handle.RunID, syntheticRetryNoncePrefix+failedAttemptID)
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

// productionRetryReadFrozenTask reads the failed attempt's usage receipt and its
// original invocation nonce, then parses the stored replayTask back into the
// frozen task it was written from.
func productionRetryReadFrozenTask(ctx context.Context, db *sql.DB, attemptID string) (generationowner.Task, string, string, error) {
	if db == nil {
		return generationowner.Task{}, "", "", errors.New("GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID")
	}
	var raw, nonce string
	err := db.QueryRowContext(ctx, "SELECT usage_receipt_json, invocation_nonce FROM generation_attempts WHERE attempt_id=?", attemptID).Scan(&raw, &nonce)
	if err != nil {
		return generationowner.Task{}, "", "", errors.New("GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID")
	}
	var usage map[string]any
	if err := json.Unmarshal([]byte(raw), &usage); err != nil {
		return generationowner.Task{}, "", "", errors.New("GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID")
	}
	if usage["replayTask"] == nil {
		return generationowner.Task{}, "", "", errors.New("GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID")
	}
	taskRaw, err := json.Marshal(usage["replayTask"])
	if err != nil {
		return generationowner.Task{}, "", "", errors.New("GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID")
	}
	task, err := generationowner.ParseTask(taskRaw)
	if err != nil {
		return generationowner.Task{}, "", "", errors.New("GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID")
	}
	return task, nonce, syntheticString(usage["requestHash"]), nil
}
