package store

// This file is the store port behind db:project-core-commit-generated, the last
// `db:*` name production_db_residual_linux.go refused for a reason that was about
// this package rather than about the reference: "store.ProjectCore.Update exists,
// but the child-effect stamp it must commit with has no writer or shared
// transaction". Both halves of that sentence are answered here - the shared
// transaction by reusing the handle another batch wrote in this same package, and
// the stamp by reading what the reference actually stamps for the runs this
// module can begin.
//
// # The reference
//
//	electron/controllers/db-controller.ts:197-209
//	  operations.handle('db:project-core-commit-generated', async (_event, request, expectedProjectPath) => {
//	    try {
//	      assertRequiredExpectedProjectPath(getCurrentProjectPath(), expectedProjectPath)
//	      const database = getProjectDb()
//	      if (!database || !request.generationRunHandle) throw new Error('GENERATION_OWNER_REQUIRED')
//	      database.transaction(() => withGenerationAgentChildEffect(request.generationRunHandle, () => {
//	        assertGenerationSourcesCurrent(request.generationRunHandle)
//	        ProjectCoreRepository.update(request.data)
//	      })).immediate()
//	      return { success: true }
//	    } catch (error) { return { success: false, error: String(error) } }
//	  })
//
// The row is ProjectCoreRepository.update (electron/repositories/project-core-repository.ts
// :156-210): a dynamic SET over the field names the caller sent, `updated_at =
// datetime('now')` (:204) and `WHERE id = ?` with 'main' (:208) - and no
// statement at all when the caller named no allowlisted field, because :201
// returns before updated_at is appended.
//
// # The child-effect stamp
//
// withGenerationAgentChildEffect (electron/controllers/generation-controller.ts
// :67-71) opens the transaction and hands the effect to
// MainGenerationOwner.withAgentChildEffect (electron/services/main-generation-owner.ts
// :1548), which is `requireRun(handle); return agents.withChildEffect(handle,
// effect)`. That body (electron/services/agent-generation.ts:344-349) is:
//
//	  if (!this.db.inTransaction) throw new Error('GENERATION_AGENT_TRANSACTION_REQUIRED')
//	  const child = this.runs.get(handle.runId)
//	  if (!isDeepStrictEqual(this.handle(child), handle)) throw new Error('GENERATION_AGENT_CHILD_NOT_AUTHORIZED')
//	  const id = child.binding.sourceManifest.agentWorkflowRegistrationId
//	  if (!id) return effect()
//
// so the receipt stamp (agentSourceTransition, written over the session's runs at
// :364-365) exists only for a child that carries an agentWorkflowRegistrationId.
// No run this module can begin carries that key: generation:begin parses a closed
// selection with no registration field (cmd/novel-server/synthetic_generation_linux.go
// :396-417), which is why the whole agent family is the eight refused names of
// cmd/novel-server/production_agent_generation_linux.go:73. For every run that is
// reachable here, the reference's own line `if (!id) return effect()` (:349) is
// the whole of withChildEffect: it proves the child's identity and then runs the
// effect, and it writes no receipt row. That is the write this port does not
// invent. Writing agentSourceTransition for a run the reference never stamps would
// be a receipt no projection produced, and internal/store/generation_ledger.go:588
// refuses that key by name, so every later ledger write on the same root would
// fail GENERATION_EFFECT_UNSUPPORTED. A run that does carry the key is refused by
// name instead of being served with a stamp this module cannot build.
//
// Everything else the reference does inside that transaction is kept: the run
// identity of requireRun (main-generation-owner.ts:156-164), the live epoch and
// the cancelled-root half of assertSourcesCurrent (:730-731), and the core row -
// all inside one BEGIN IMMEDIATE, so a refusal anywhere leaves the author's
// database file byte-identical.
//
// # Documented reductions, each one already recorded by a served sibling
//
//   - assertSourcesCurrent's last step rebuilds the binding and compares it with
//     the frozen one (GENERATION_SOURCE_CHANGED, main-generation-owner.ts:742-743).
//     rebuildBinding has no port in this module (internal/generationowner/binding.go
//     :50-54 says so), and internal/store/draft_commit_tx.go:54-58 records the
//     same reduction for generation:commit-draft: the epoch and cancelled-root
//     halves are enforced, the rebuilt-binding comparison is not.
//   - requireRun(execution) also calls assertBlueprintRecoveryWritable and
//     assertPlotOutlineWritable (generation-run-repository.ts:134-137, both
//     read-only). Neither a blueprint nor a plot outline is touched here.
//   - its policy/budget comparison (main-generation-owner.ts:151-153,
//     GENERATION_POLICY_BUDGET_MISMATCH) reads readMainGenerationPolicy, which no
//     Go file ports; the ledger froze that budget at begin, and generation:begin
//     already validated it there.
//
// # The one handle
//
// dbMaintenanceImmediate (internal/store/dbmaintenance.go:126-158) is this
// package's one hand-written reservation: a single *sql.Conn, BEGIN IMMEDIATE, a
// rollback under a fresh five-second context on every failure path, and a commit
// only after the body returned nil. This file reuses it rather than copying it a
// third time - the ledger borrows ReviewRevisions.immediate the same way
// (internal/store/generation_ledger.go:268) - so this transaction cannot diverge
// from the one that already carries the two destructive db writes. Because the
// reservation is taken before the first read, the identity/epoch/root proofs and
// the core UPDATE are serialized against every other writer instead of being a
// check-then-act across two connections.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

var (
	// The reference's own codes, spelled the way the controller publishes them
	// after `Error: `.
	ErrProjectCoreGeneratedRunMissing  = errors.New("GENERATION_RUN_MISSING")
	ErrProjectCoreGeneratedIdentity    = errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
	ErrProjectCoreGeneratedEpochStale  = errors.New("GENERATION_EPOCH_STALE")
	ErrProjectCoreGeneratedCancelled   = errors.New("GENERATION_ACTION_CANCELLED")
	ErrProjectCoreGeneratedRootMissing = errors.New("GENERATION_ROOT_MISSING")

	// ErrProjectCoreGeneratedBinding refuses a binding_json or action_json this
	// package cannot read. The reference's JSON.parse throws a non-symbolic
	// SyntaxError there; the ledger's own code for an unusable binding is what
	// this lane publishes instead.
	ErrProjectCoreGeneratedBinding = errors.New("GENERATION_BINDING_INVALID")

	// ErrProjectCoreGeneratedChildEffect is this lane's own refusal, not the
	// reference's. The reference stamps agentSourceTransition for every run of an
	// agent session when the child carries an agentWorkflowRegistrationId; this
	// module has neither the projection behind that stamp nor an agent begin that
	// could mint the key, so the request is refused by name rather than served
	// with a stamp that would poison the ledger's receipt allowlist
	// (internal/store/generation_ledger.go:588).
	ErrProjectCoreGeneratedChildEffect = errors.New("GENERATION_AGENT_CHILD_EFFECT_UNPORTED")
)

// ProjectCoreGeneratedRequest is one db:project-core-commit-generated call. Data
// is the renderer's Partial<ProjectCoreData>; Handle is the generation run handle
// the request carries (MainGenerationRunHandle, src/services/generation/
// generation-runtime.ts:77-82); ProjectID and LiveEpoch are the authenticated
// project and the live writer lease the frozen session proves, i.e. deps.projectId
// and deps.epoch in requireRun/assertSourcesCurrent.
type ProjectCoreGeneratedRequest struct {
	Data      map[string]any
	Handle    BatchRunHandle
	ProjectID string
	LiveEpoch string
}

// projectCoreGeneratedBinding is the part of the frozen RunBinding this port
// reads: the two identity fields requireRun compares with the handle, and the one
// manifest key withChildEffect branches on.
type projectCoreGeneratedBinding struct {
	ProjectID      string `json:"projectId"`
	Epoch          string `json:"epoch"`
	SourceManifest struct {
		AgentWorkflowRegistrationID string `json:"agentWorkflowRegistrationId"`
	} `json:"sourceManifest"`
}

// CommitGenerated is db:project-core-commit-generated: one BEGIN IMMEDIATE that
// proves the generation run and writes the project core row, or refuses and
// writes nothing. The success answer carries no receipt - the controller returns
// `{ success: true }` (db-controller.ts:208) - so this method answers an error and
// the adapter in cmd/novel-server owns the envelope.
func (r *ProjectCore) CommitGenerated(ctx context.Context, request ProjectCoreGeneratedRequest) error {
	if r == nil || r.db == nil {
		return ErrProjectNotOpen
	}
	// A nil context is not a cancelled request, and Conn would panic on it. The
	// repository cannot run without one, which is the same answer it gives when it
	// has no database.
	if ctx == nil {
		return ErrProjectNotOpen
	}
	return NewDatabaseMaintenance(r.db).dbMaintenanceImmediate(ctx, func(conn *sql.Conn) error {
		binding, err := projectCoreGeneratedRun(ctx, conn, request)
		if err != nil {
			return err
		}
		// withChildEffect, first arm (:348-349): a registered agent child is the
		// only run the reference stamps, and this lane cannot build that stamp.
		if binding.SourceManifest.AgentWorkflowRegistrationID != "" {
			return ErrProjectCoreGeneratedChildEffect
		}
		// assertSourcesCurrent through requireRun(handle, true): the run's frozen
		// epoch must be the lease this call holds (:164), and its root must not
		// have been cancelled (:731). The rebuilt-binding comparison is the
		// documented reduction, not a silently skipped check.
		if request.Handle.Epoch != request.LiveEpoch {
			return ErrProjectCoreGeneratedEpochStale
		}
		status, err := projectCoreGeneratedRootStatus(ctx, conn, request.Handle.RootActionID)
		if err != nil {
			return err
		}
		if status == "cancelled" {
			return ErrProjectCoreGeneratedCancelled
		}
		// :206 ProjectCoreRepository.update(request.data), on this connection and
		// inside this transaction.
		return projectCoreGeneratedWrite(ctx, conn, request.Data)
	})
}

// projectCoreGeneratedRun is requireRun(handle) (main-generation-owner.ts:156-164)
// and the identity half of withChildEffect (:346-347) in one read: the run must
// exist, its binding must be readable, and the handle must equal the run's own
// identity - projectId, epoch, rootActionId and runId - on both the scope and the
// wire side. A run of another project, another lease or another root therefore
// refuses with the reference's GENERATION_RUN_IDENTITY_MISMATCH, and an absent one
// with the repository's GENERATION_RUN_MISSING (generation-run-repository.ts:126).
func projectCoreGeneratedRun(ctx context.Context, conn *sql.Conn, request ProjectCoreGeneratedRequest) (projectCoreGeneratedBinding, error) {
	var binding projectCoreGeneratedBinding
	rows, err := rrMaps(ctx, conn, "SELECT run_id,root_action_id,binding_json FROM generation_runs WHERE run_id=?",
		request.Handle.RunID)
	if err != nil {
		return binding, err
	}
	if len(rows) == 0 {
		return binding, ErrProjectCoreGeneratedRunMissing
	}
	if err := json.Unmarshal([]byte(glText(rows[0]["binding_json"])), &binding); err != nil {
		return binding, ErrProjectCoreGeneratedBinding
	}
	if glText(rows[0]["run_id"]) != request.Handle.RunID ||
		glText(rows[0]["root_action_id"]) != request.Handle.RootActionID ||
		binding.ProjectID != request.Handle.ProjectID || binding.Epoch != request.Handle.Epoch ||
		binding.ProjectID != request.ProjectID || request.Handle.ProjectID != request.ProjectID {
		return binding, ErrProjectCoreGeneratedIdentity
	}
	return binding, nil
}

// projectCoreGeneratedRootStatus reads the status assertSourcesCurrent checks
// before the effect (:731). An absent root is the ledger's own
// GENERATION_ROOT_MISSING, the code this database already answers for a budget
// read of a root it does not hold.
func projectCoreGeneratedRootStatus(ctx context.Context, conn *sql.Conn, rootID string) (string, error) {
	rows, err := rrMaps(ctx, conn, "SELECT action_json FROM generation_roots WHERE root_action_id=?", rootID)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", ErrProjectCoreGeneratedRootMissing
	}
	var action struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(glText(rows[0]["action_json"])), &action); err != nil {
		return "", ErrProjectCoreGeneratedBinding
	}
	return action.Status, nil
}

// projectCoreGeneratedWrite runs the reference's UPDATE on the transaction's own
// connection. Nothing is written when the caller named no allowlisted field,
// because projectCoreUpdateStatement answers the empty statement for that request
// exactly as :201 returns before the timestamp.
func projectCoreGeneratedWrite(ctx context.Context, conn *sql.Conn, data map[string]any) error {
	normalized, err := projectCoreGeneratedData(data)
	if err != nil {
		return err
	}
	statement, values, err := projectCoreUpdateStatement(normalized)
	if err != nil || statement == "" {
		return err
	}
	_, err = conn.ExecContext(ctx, statement, values...)
	return err
}

// projectCoreGeneratedData is the request data with the wire decoder's
// json.Number turned into the numeric types the core builder binds. The HTTP body
// is decoded with UseNumber (internal/security/body.go:69), so a renderer's
// totalChapters arrives as a json.Number while the same value is a JavaScript
// number in the desktop and better-sqlite3 binds it as one; refusing it here would
// refuse a real save the reference accepts. Nothing else is coerced: a boolean, an
// object or an array still reaches projectCoreBinding and is still refused.
func projectCoreGeneratedData(data map[string]any) (map[string]any, error) {
	normalized := make(map[string]any, len(data))
	for key, value := range data {
		number, ok := value.(json.Number)
		if !ok {
			normalized[key] = value
			continue
		}
		if integer, err := number.Int64(); err == nil {
			normalized[key] = integer
			continue
		}
		if real, err := number.Float64(); err == nil {
			normalized[key] = real
			continue
		}
		return nil, ErrProjectCoreInvalidBinding
	}
	return normalized, nil
}
