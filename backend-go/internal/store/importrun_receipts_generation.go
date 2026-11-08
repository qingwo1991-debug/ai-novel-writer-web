package store

import (
	"context"
	"database/sql"
	"errors"
)

// This file is the ONE new read the import-run receipt assert needed, written
// where the read is legal: on the receipt transaction's own connection.
//
// # Why the read moved here
//
// The reference's source proof starts by reading the generation run
//
//	electron/services/main-generation-owner.ts:1454  const run = requireRun(handle)
//
// and it may, because its owner is a synchronous dispatcher over the one
// better-sqlite3 connection and the controller refuses the call unless the
// database is already in a transaction
// (electron/controllers/generation-controller.ts:74). This port had the same
// convention - the assert runs inside PrepareEffectReceipt's and
// CommitEffectReceipt's BEGIN IMMEDIATE (internal/store/importrun.go:948
// importRunImmediate) - but handed the caller no connection: the callback type
// carried (handle, slot) only, so a caller that wanted the run had to borrow one
// from the same pool the transaction had reserved. The author database is opened
// with exactly one connection (internal/projectfs/author_linux.go:121
// db.SetMaxOpenConns(1)), so that borrow was a deadlock rather than a proof
// (internal/store/character_proposal_pending.go:37) and it ended, measurably, in
// context.DeadlineExceeded.
//
// So the store performs the read itself, on the connection it already holds,
// and hands the RESULT to the assert. The assert keeps every decision: a run it
// cannot see, a run that belongs to another project, a run frozen for another
// slot and a run whose sources no longer reproduce are all refusals of the
// prover, not of this reader. This file only answers "what does the ledger say
// about the run this handle names".
//
// # What it hands over, member by member
//
//   - generation_runs.run_id, root_action_id and binding_json: requireRun's row
//     (electron/services/main-generation-owner.ts:156-167), byte-exact, so the
//     prover decodes the frozen binding itself and cannot be handed a
//     re-serialized one.
//   - generation_roots.action_json's status: the budget root the assert reads at
//     :1465 (repository.budget(rootActionId).root.status).
//   - every generation_attempts row of THIS run, with its usage_receipt_json
//     verbatim, and that attempt's generation_artifacts row (attempt_id is
//     UNIQUE, so it is at most one) with its status and artifact_json. The
//     reference filters the root's attempts down to the run's own at :1465-1471;
//     selecting them by run_id is that same set, and the attempt/usage/artifact
//     decoding stays with the prover.
//
// Everything is handed over raw. Nothing here hashes, decodes or compares a
// receipt, and nothing here decides a code.

// ImportRunGenerationAttempt is one generation_attempts row of the run the
// receipt's handle names, with the artifact that attempt settled. A nil
// UsageReceiptJSON or ArtifactJSON means the column was NULL, never that the
// value was empty: the prover refuses both.
type ImportRunGenerationAttempt struct {
	// AttemptID is generation_attempts.attempt_id, the row identity the
	// reference's receipt(attemptId) is keyed on.
	AttemptID string
	// UsageReceiptJSON is generation_attempts.usage_receipt_json verbatim. The
	// reference reads result.finishReason out of it (:1468).
	UsageReceiptJSON []byte
	// ArtifactPresent reports whether generation_artifacts has a row for this
	// attempt. The reference refuses an attempt with no artifact (:1468).
	ArtifactPresent bool
	// ArtifactID is generation_artifacts.artifact_id.
	ArtifactID string
	// ArtifactStatus is generation_artifacts.status, the column the reference's
	// own "status<>'discarded'" probe (:1469) filters on.
	ArtifactStatus string
	// ArtifactJSON is generation_artifacts.artifact_json verbatim: the frozen
	// text and the textHash the prover re-derives (:1468).
	ArtifactJSON []byte
}

// ImportRunGenerationEvidence is the generation ledger's answer about one run,
// read on the receipt transaction's own connection and handed to the assert as
// data. It is evidence, not a verdict: every field is the stored value, and the
// absent run is reported as RunPresent=false rather than as an error so the
// prover can answer the reference's own GENERATION_RUN_MISSING.
type ImportRunGenerationEvidence struct {
	// RunID is the run id the handle named, echoed even when nothing was found.
	RunID string
	// RunPresent reports whether generation_runs holds this run. requireRun's
	// repository.get answers GENERATION_RUN_MISSING when it does not
	// (electron/repositories/generation-run-repository.ts:118-126).
	RunPresent bool
	// RootActionID is generation_runs.root_action_id: the root the run belongs
	// to, which the handle must also name.
	RootActionID string
	// BindingJSON is generation_runs.binding_json verbatim.
	BindingJSON []byte
	// RootPresent reports whether generation_roots holds RootActionID.
	RootPresent bool
	// RootStatus is json_extract(action_json,'$.status') of that root.
	RootStatus string
	// Attempts is every attempt of THIS run, in row order.
	Attempts []ImportRunGenerationAttempt
}

// importRunGenerationHandleRunID is the run id a generationRunHandle names. The
// store already proved the handle's shape before the assert is reached
// (importRunReceiptHandle: exactly projectId, epoch, rootActionId and runId, all
// non-blank strings), so this is a lookup and not a second validation. A handle
// that still names no run is a run that does not exist, which is what the empty
// answer means here.
func importRunGenerationHandleRunID(handle any) string {
	object, ok := handle.(map[string]any)
	if !ok || object == nil {
		return ""
	}
	runID, ok := object["runId"].(string)
	if !ok {
		return ""
	}
	return importRunReceiptTrim(runID)
}

// importRunGenerationTable reports whether one ledger table exists. A database
// that never wrote a generation run has no such table, and that is the same
// answer as a table with no row for this run: no run. The probe is a read, so it
// is legal inside the receipt transaction, and it keeps the absent-run answer
// from becoming a driver error in a project that never generated anything.
func importRunGenerationTable(ctx context.Context, q importRunQueryer, name string) (bool, error) {
	var present int64
	err := q.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&present)
	if err != nil {
		return false, err
	}
	return present > 0, nil
}

// importRunGenerationEvidenceRead is requireRun's read, performed on the receipt
// transaction's own connection. It never opens a second connection and never
// returns a refusal of its own: an absent table, an absent run and an absent root
// are reported as absent, and a malformed stored value is handed over verbatim
// for the prover to refuse.
func importRunGenerationEvidenceRead(ctx context.Context, q importRunQueryer, runID string) (ImportRunGenerationEvidence, error) {
	evidence := ImportRunGenerationEvidence{RunID: runID}
	if runID == "" {
		return evidence, nil
	}
	runs, err := importRunGenerationTable(ctx, q, "generation_runs")
	if err != nil {
		return ImportRunGenerationEvidence{RunID: runID}, err
	}
	if !runs {
		return evidence, nil
	}
	var bindingJSON string
	err = q.QueryRowContext(ctx,
		`SELECT run_id, root_action_id, binding_json FROM generation_runs WHERE run_id = ?`, runID).
		Scan(&evidence.RunID, &evidence.RootActionID, &bindingJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return ImportRunGenerationEvidence{RunID: runID}, nil
	}
	if err != nil {
		return ImportRunGenerationEvidence{RunID: runID}, err
	}
	evidence.RunPresent = true
	evidence.BindingJSON = []byte(bindingJSON)
	if evidence.RootActionID == "" {
		return evidence, nil
	}
	if err := importRunGenerationEvidenceRoot(ctx, q, &evidence); err != nil {
		return ImportRunGenerationEvidence{RunID: runID}, err
	}
	if err := importRunGenerationEvidenceAttempts(ctx, q, &evidence); err != nil {
		return ImportRunGenerationEvidence{RunID: runID}, err
	}
	return evidence, nil
}

// importRunGenerationEvidenceRoot adds the root's status: the one column the
// assert reads off the budget root (:1465).
func importRunGenerationEvidenceRoot(ctx context.Context, q importRunQueryer, evidence *ImportRunGenerationEvidence) error {
	if evidence == nil {
		return nil
	}
	roots, err := importRunGenerationTable(ctx, q, "generation_roots")
	if err != nil || !roots {
		return err
	}
	var status sql.NullString
	err = q.QueryRowContext(ctx,
		`SELECT json_extract(action_json,'$.status') FROM generation_roots WHERE root_action_id = ?`,
		evidence.RootActionID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	evidence.RootPresent = true
	evidence.RootStatus = status.String
	return nil
}

// importRunGenerationEvidenceAttempts adds the run's attempts and, for each one,
// the artifact it settled. Both reads are on the same connection, in the same
// transaction, so the attempt list and the artifacts it reports are one snapshot.
func importRunGenerationEvidenceAttempts(ctx context.Context, q importRunQueryer, evidence *ImportRunGenerationEvidence) error {
	if evidence == nil {
		return nil
	}
	attempts, err := importRunGenerationTable(ctx, q, "generation_attempts")
	if err != nil || !attempts {
		return err
	}
	artifacts, err := importRunGenerationTable(ctx, q, "generation_artifacts")
	if err != nil {
		return err
	}
	rows, err := q.QueryContext(ctx,
		`SELECT attempt_id, usage_receipt_json FROM generation_attempts WHERE run_id = ? ORDER BY rowid`,
		evidence.RunID)
	if err != nil {
		return err
	}
	for rows.Next() {
		attempt := ImportRunGenerationAttempt{}
		var usage sql.NullString
		if err := rows.Scan(&attempt.AttemptID, &usage); err != nil {
			_ = rows.Close()
			return err
		}
		if usage.Valid {
			attempt.UsageReceiptJSON = []byte(usage.String)
		}
		evidence.Attempts = append(evidence.Attempts, attempt)
	}
	// The rows are closed BEFORE the artifact reads start. One connection carried
	// this whole transaction, and database/sql reserves it for an open Rows: a
	// second query issued underneath this cursor would wait for the same connection
	// the receipt transaction is holding - the exact shape this file exists to avoid.
	iterationErr := rows.Err()
	closeErr := rows.Close()
	if iterationErr != nil {
		return iterationErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !artifacts {
		return nil
	}
	for index := range evidence.Attempts {
		attempt := &evidence.Attempts[index]
		if attempt.AttemptID == "" {
			continue
		}
		var artifactID, status, artifactJSON string
		err := q.QueryRowContext(ctx,
			`SELECT artifact_id, status, artifact_json FROM generation_artifacts WHERE attempt_id = ?`,
			attempt.AttemptID).Scan(&artifactID, &status, &artifactJSON)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		attempt.ArtifactPresent = true
		attempt.ArtifactID = artifactID
		attempt.ArtifactStatus = status
		attempt.ArtifactJSON = []byte(artifactJSON)
	}
	return nil
}
