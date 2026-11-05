package main

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/graphgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// GraphRead is the graph generation recovery projection: it returns the run
// view, the validated frozen context and the facts the UI needs to decide
// whether the run can still be continued.
//
// Two pieces of the browser contract are deliberately absent, and both are
// absent for the same reason - they can only be answered for a run that has
// produced output, and no operation that produces one is bound yet:
//
//   - effects: graph effects are recorded by confirmation, which is unbound.
//   - artifact/result: the Node projection qualifies the stored result against a
//     graph-specific reference and omits it when that qualification fails, so
//     raw unqualified output stays in the run view for copying. Re-projecting
//     the stored result without that check would present unverified text as a
//     result, which is exactly what the original refuses to do.
//
// sourceStatus currently reflects only the first half of assertFinalizationSources
// (a cancelled root is never current). The second half - rebuilding the source
// binding from current project state and comparing it with the frozen one - has
// to land before any operation that confirms or writes is bound.
func (s *syntheticGeneration) GraphRead(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	handle, err := syntheticParseHandle(arg)
	if err != nil {
		return nil, err
	}
	_, scope, ledger, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	run, _, manifest, err := syntheticRequireRun(ctx, ledger, scope, handle)
	if err != nil {
		return nil, err
	}
	contextValue, _, err := syntheticGraphContext(manifest, scope.Project)
	if err != nil {
		return nil, err
	}
	attempts, err := syntheticGraphAttemptIDs(ctx, db, handle.RunID)
	if err != nil {
		return nil, err
	}
	// A second attempt means the run was retried, and a recovery would have to
	// choose which raw output the author is looking at. The original refuses
	// instead of guessing.
	if len(attempts) > 1 {
		return nil, errors.New("GENERATION_GRAPH_ATTEMPT_CONFLICT")
	}
	view, err := s.viewOf(ctx, ledger, db, run)
	if err != nil {
		return nil, err
	}
	status, err := syntheticGraphSourceStatus(ctx, ledger, db, syntheticString(run["rootActionId"]), contextValue)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"view":         view,
		"modelId":      syntheticString(manifest["modelId"]),
		"context":      contextValue,
		"attemptCount": float64(len(attempts)),
		"sourceStatus": status,
		"effects":      []any{},
	}, nil
}

// FinalizationRead is the finalization-generation:read projection: the recovery
// of the run that currently owns a finalization slot, or JSON null when no run
// owns it yet.
//
// null is a real answer here, not a stand-in. The renderer asks this before
// begin (src/services/finalization-generation.ts:38) and reads null as "nothing
// to resume", so refusing the call would break the first step of every
// finalization instead of protecting anything. The slot is validated exactly
// like finalizationSlotKey and its key is matched against the frozen manifest of
// the runs this project's ledger holds, so an absent answer is the result of a
// real lookup rather than a constant.
//
// A matching row is refused instead of projected. That key is stamped by the
// finalization source binding alone
// (electron/services/generation-source-binding.ts:589), which this lane never
// reaches because it does not bind finalization begin, so such a row can only
// come from another writer. Recovering it would mean proving textHash(JSON.stringify(context))
// against the stored finalizationGenerationContextHash, and JSON.stringify
// writes property insertion order while every digest this lane derives is
// canonical (syntheticGraphDigest): either that comparison is skipped or a
// context the lane cannot prove is handed to the renderer. It refuses with the
// code the original raises for an unprovable context, and a row whose binding
// names another project is the original's slot conflict
// (electron/services/finalization-generation.ts:27).
func (s *syntheticGeneration) FinalizationRead(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	key, err := syntheticFinalizationSlotKey(arg)
	if err != nil {
		return nil, err
	}
	_, scope, _, db, done, err := s.admit(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	defer done()
	rows, err := db.QueryContext(ctx,
		`SELECT json_extract(binding_json,'$.projectId') FROM generation_runs
		 WHERE json_extract(binding_json,'$.sourceManifest.finalizationGenerationSlotKey')=? ORDER BY rowid`, key)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	found := false
	for rows.Next() {
		// A missing projectId (NULL) is the original's `undefined !== projectId`
		// conflict, so NullString covers both the foreign row and the shapeless one.
		var owner sql.NullString
		if err := rows.Scan(&owner); err != nil {
			return nil, err
		}
		if !owner.Valid || owner.String != scope.Project {
			return nil, errors.New("GENERATION_FINALIZATION_SLOT_CONFLICT")
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	// The rows all belong to this project, so the original would go on to compare
	// each stored context with the slot; that comparison is what this lane cannot
	// reproduce, and it refuses rather than guesses.
	return nil, errors.New("GENERATION_FINALIZATION_CONTEXT_INVALID")
}

// syntheticFinalizationSlotKey validates one FinalizationGenerationSlot and
// returns the original's finalizationSlotKey
// (electron/services/finalization-generation-source.ts:12):
//
//	textHash(JSON.stringify([draftId, finalizationId, chapterNumber, contentHash, stepKey]))
//
// The hashed value is an array of primitives, so the insertion-order ambiguity
// that forces this lane's object digests onto canonical encoding cannot arise:
// graphgen.EncodeJSON reproduces JSON.stringify for exactly this shape, and the
// key matches the one a desktop build stores for the same slot.
func syntheticFinalizationSlotKey(value any) (string, error) {
	invalid := errors.New("GENERATION_FINALIZATION_SLOT_INVALID")
	slot := syntheticMap(value)
	if slot == nil {
		return "", invalid
	}
	for key := range slot {
		if key != "source" && key != "stepKey" {
			return "", invalid
		}
	}
	stepKey := syntheticString(slot["stepKey"])
	if stepKey != "chapter_notes" && stepKey != "character_cards" {
		return "", invalid
	}
	source := syntheticMap(slot["source"])
	if source == nil {
		return "", invalid
	}
	for key := range source {
		switch key {
		case "draftId", "chapterNumber", "finalizationId", "contentHash":
		default:
			return "", invalid
		}
	}
	draftID, draftOK := syntheticSlotID(source["draftId"])
	chapterNumber, chapterOK := syntheticSlotID(source["chapterNumber"])
	finalizationID, finalizationOK := source["finalizationId"].(string)
	contentHash, contentOK := source["contentHash"].(string)
	if !draftOK || !chapterOK || !finalizationOK || !contentOK ||
		strings.TrimSpace(finalizationID) == "" || !syntheticHex64(contentHash) {
		return "", invalid
	}
	encoded, err := graphgen.EncodeJSON([]any{draftID, finalizationID, chapterNumber, contentHash, stepKey})
	if err != nil {
		return "", invalid
	}
	return graphgen.TextHash(encoded), nil
}

// syntheticSlotID accepts exactly what the original's Number.isSafeInteger guard
// accepts: a JSON number, integral, positive and inside the safe-integer range.
// A numeric string is refused, unlike this lane's looser ledger coercion,
// because the original refuses it too.
func syntheticSlotID(value any) (int64, bool) {
	number, ok := syntheticFloat64(value)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) ||
		number != math.Trunc(number) || number < 1 || number > 1<<53-1 {
		return 0, false
	}
	return int64(number), true
}

// syntheticGraphAttemptIDs lists a run's attempts in dispatch order.
func syntheticGraphAttemptIDs(ctx context.Context, db *sql.DB, runID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT attempt_id FROM generation_attempts WHERE run_id=? ORDER BY rowid", runID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	attempts := make([]string, 0, 1)
	for rows.Next() {
		var attemptID string
		if err := rows.Scan(&attemptID); err != nil {
			return nil, err
		}
		attempts = append(attempts, attemptID)
	}
	return attempts, rows.Err()
}

// syntheticGraphSourceStatus reports whether the run's sources are still the ones
// it was frozen from.
//
// The default is "conflict", not "current". This value gates confirmation, and
// confirmation writes finalized text over what the author already has, so an
// unverifiable source state must read as changed rather than as fine. Only a kind
// whose current state can actually be re-read gets to answer "current"; today
// that is the event kind, whose context freezes the exact
// draftId/finalizationId/contentHash triple the finalization outbox stores.
//
// The plot and plan kinds still need their own re-derivation, and neither is a
// shortcut away: the frozen plot baseline is only a CAS over the raw
// plot_tree_snapshot column, so a run whose other plot sources moved would read
// "current" on it. Until the binding rebuild exists, a run of either kind reports
// "conflict" - visibly wrong rather than invisibly permissive, which is the right
// way round for a value this one gates.
func syntheticGraphSourceStatus(ctx context.Context, ledger *store.GenerationLedger, db *sql.DB, rootActionID string, contextValue map[string]any) (string, error) {
	budgetValue, err := syntheticCall(ctx, ledger, "budget", rootActionID)
	if err != nil {
		return "", err
	}
	root := syntheticMap(syntheticMap(budgetValue)["root"])
	if strings.TrimSpace(syntheticString(root["status"])) == "cancelled" {
		return "conflict", nil
	}
	if syntheticString(contextValue["kind"]) != "event" {
		return syntheticGraphFreshness("", "", ""), nil
	}
	source := syntheticMap(contextValue["source"])
	if source == nil || syntheticString(source["contentHash"]) == "" {
		return syntheticGraphFreshness("event", "", ""), nil
	}
	var current string
	err = db.QueryRowContext(ctx,
		"SELECT content_hash FROM finalization_outbox WHERE draft_id=? AND finalization_id=?",
		syntheticNumber(source["draftId"]), syntheticString(source["finalizationId"]),
	).Scan(&current)
	if err != nil {
		// A missing row means the finalized chapter is no longer the one the run
		// was frozen from, which is exactly a changed source.
		if errors.Is(err, sql.ErrNoRows) {
			return "conflict", nil
		}
		return "", err
	}
	return syntheticGraphFreshness("event", current, syntheticString(source["contentHash"])), nil
}

// syntheticGraphFreshness is the decision rule on its own, so the fail-closed
// direction can be pinned by a test that needs no database. `kind` is empty for
// the kinds whose current state is not re-derived yet.
func syntheticGraphFreshness(kind, currentHash, frozenHash string) string {
	if kind != "event" || currentHash == "" || frozenHash == "" {
		return "conflict"
	}
	if currentHash == frozenHash {
		return "current"
	}
	return "conflict"
}
