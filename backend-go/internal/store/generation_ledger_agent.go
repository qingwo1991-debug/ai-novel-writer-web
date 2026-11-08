package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// This file is the agent-round receipt writer: the one ledger port the four
// tool names and the resume name write through. It patches exactly the closed
// set of agent receipt keys, is gated to operation agent-round, and is
// idempotent in the reference's way - replaying the same [key, value] leaves the
// row untouched.
//
// The agent action map lives on the attempt's usage receipt, not on the run
// binding: every tool operation of the family is a receipt patch
// (electron/services/agent-generation.ts:215-232 claim, :259-275 finish,
// :276-293 register, :377-404 commit, :163-170 resume), and no ledger method
// outside settle could patch a receipt key before this file
// (internal/store/generation_ledger.go:601 refused the keys, and recordEffect's
// allowlist was effect-only at :1228). This method is deliberately NOT
// recordEffect: the action map is keyed by tool call id and merged by the
// caller, so the ledger only proves the operation owns the keys and stores them
// in one effect transaction.

// glAgentReceiptKey is the closed vocabulary this writer admits. Every other
// top-level usage key is refused, exactly like recordEffect refuses every key
// the operation does not own.
func glAgentReceiptKey(key string) bool {
	switch key {
	case "agentActions", "agentAuthorSelections", "agentSourceTransition", "agentControlEpoch":
		return true
	}
	return false
}

// glPatchAgent is glPatch widened to append more than one new key, in the exact
// order the caller declares. Existing keys keep their stored position; a patch
// key neither present nor declared in order is an ambiguity, not a silent sort.
func glPatchAgent(text any, patch map[string]any, order []string) (string, error) {
	value, err := glParse(glText(text))
	if err != nil {
		return "", err
	}
	object, ok := value.(plotObject)
	if !ok {
		return "", glError("GENERATION_JSON_INVALID")
	}
	used := map[string]bool{}
	for index, field := range object {
		if replacement, present := patch[field.key]; present {
			object[index].value = replacement
			used[field.key] = true
		}
	}
	for _, key := range order {
		if _, present := patch[key]; !present || used[key] {
			continue
		}
		object = append(object, plotField{key, patch[key]})
		used[key] = true
	}
	for key := range patch {
		if !used[key] {
			return "", glError("GENERATION_JSON_ORDER_REQUIRED")
		}
	}
	return plotJSON(object)
}

// recordAgentReceipt patches one attempt's usage receipt with the agent keys the
// caller already decided. It is gated to operation agent-round, scopes the
// attempt by run id, refuses a key outside the agent vocabulary, and returns the
// attempt receipt. The patch is a complete value, not a merge: the caller owns
// the action-map merge, the ledger only proves and stores it.
func (g *GenerationLedger) recordAgentReceipt(ctx context.Context, conn *sql.Conn, runID, attemptID string, patch map[string]any, order []string) (map[string]any, error) {
	run, err := g.get(ctx, conn, runID)
	if err != nil {
		return nil, err
	}
	binding := glMap(run["binding"])
	manifest := glMap(binding["sourceManifest"])
	if glText(manifest["operation"]) != "agent-round" {
		return nil, glError("GENERATION_OPERATION_UNSUPPORTED")
	}
	if err := g.mutableRun(ctx, conn, runID); err != nil {
		return nil, err
	}
	if len(patch) == 0 {
		return nil, glError("GENERATION_INPUT_INVALID")
	}
	for key := range patch {
		if !glAgentReceiptKey(key) {
			return nil, glError("GENERATION_EFFECT_UNSUPPORTED")
		}
	}
	// The attempt must belong to this run. The same scoping recordEffect uses:
	// a caller who may mutate one run cannot write an action into an attempt
	// belonging to another run.
	row, err := glOne(ctx, conn, "SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=? AND run_id=?", "GENERATION_ATTEMPT_MISSING", attemptID, runID)
	if err != nil {
		return nil, err
	}
	current, err := glStored(row["usage_receipt_json"])
	if err != nil {
		return nil, err
	}
	// Idempotency: a replay of the same values leaves the row untouched, which
	// is the reference's `isDeepStrictEqual` guard in updateAction.
	filtered := map[string]any{}
	changed := false
	for key, value := range patch {
		if existing, present := current[key]; present {
			before, beforeErr := json.Marshal(existing)
			after, afterErr := json.Marshal(value)
			if beforeErr == nil && afterErr == nil && string(before) == string(after) {
				continue
			}
		}
		filtered[key] = value
		changed = true
	}
	if !changed {
		return g.receipt(ctx, conn, attemptID, nil)
	}
	encoded, err := glPatchAgent(row["usage_receipt_json"], filtered, order)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=?", encoded, attemptID); err != nil {
		return nil, err
	}
	return g.receipt(ctx, conn, attemptID, nil)
}
