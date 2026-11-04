package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"

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
	status, err := syntheticGraphSourceStatus(ctx, ledger, syntheticString(run["rootActionId"]))
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

// syntheticGraphSourceStatus reports whether the run's sources are still the
// ones it was frozen from.
//
// Only cancellation is checked today. That is the half of the check the ledger
// can answer on its own; the other half rebuilds the source binding from current
// project state, and until it exists this must not claim more than it can prove.
// Note the direction of the remaining risk: this can report "current" for a run
// whose sources moved, so no operation that writes may be bound on the strength
// of this value alone.
func syntheticGraphSourceStatus(ctx context.Context, ledger *store.GenerationLedger, rootActionID string) (string, error) {
	budgetValue, err := syntheticCall(ctx, ledger, "budget", rootActionID)
	if err != nil {
		return "", err
	}
	root := syntheticMap(syntheticMap(budgetValue)["root"])
	if strings.TrimSpace(syntheticString(root["status"])) == "cancelled" {
		return "conflict", nil
	}
	return "current", nil
}
