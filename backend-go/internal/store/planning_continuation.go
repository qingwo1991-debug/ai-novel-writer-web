package store

import (
	"context"
	"database/sql"
)

// This file is the read-only planner-continuation arm of the recovery family.
// It reads the one receipt the previous recovery passes recorded as ABSENT:
// readPlanningContinuation (electron/repositories/generation-run-repository.ts:
// 179-193), which the owner's planningContinuation closure reads first
// (main-generation-owner.ts:786) and findBlueprintRecovery filters on (:1070).
//
// Nothing here writes: every statement is a SELECT inside the same deferred read
// transaction bpRead already uses, and the only rows touched are generation_runs
// read back verbatim.

// PlanningContinuationReceipt is PlanningContinuationReceipt
// (src/shared/generation-owner-contract.ts:170-175). The next handle's epoch is
// re-read from the child run's stored binding, exactly like the reference's
// `{ ...receipt.nextHandle, epoch: next.binding.epoch }`.
type PlanningContinuationReceipt struct {
	Kind         string         `json:"kind"`
	SourceHandle BatchRunHandle `json:"sourceHandle"`
	NextHandle   BatchRunHandle `json:"nextHandle"`
	RequestHash  string         `json:"requestHash"`
}

// DirectoryRecoveryRun is one chapter-blueprint-directory run reduced to what the
// find-blueprint-recovery projection reads out of its stored binding.
type DirectoryRecoveryRun struct {
	RunID        string
	RootActionID string
	ProjectID    string
	Epoch        string
	ModelID      string
	AuthorInputs []BatchAuthorInput
	SelectedBlueprintChapterNumbers []int64
}

// DirectoryRecoveryNeed is the find-blueprint-recovery scan result. Exactly one
// of Unavailable and Continuation is meaningful: a run whose blueprint recovery
// is unavailable (the reference's writeState:'unavailable' branch) or one that
// carries a proved planning continuation (the reference's continued branch).
type DirectoryRecoveryNeed struct {
	Run          DirectoryRecoveryRun
	Unavailable  bool
	Diagnostic   BlueprintRecoveryDiagnostic
	Continuation *PlanningContinuationReceipt
}

// FindDirectoryRecoveryNeed walks the chapter-blueprint-directory runs of one
// project, newest first, and returns the first run that needs recovery, or nil
// when none does. A run whose planning continuation is absent and whose
// blueprint recovery is available is skipped, exactly like the reference: its
// readContext never sets the blueprintRecovery field without a continuation
// (main-generation-owner.ts:938-943), so find never answers it (:1070).
func FindDirectoryRecoveryNeed(ctx context.Context, db *sql.DB, projectID string) (*DirectoryRecoveryNeed, error) {
	var need *DirectoryRecoveryNeed
	if err := bpRead(ctx, db, func(conn *sql.Conn) error {
		runs, err := listDirectoryRecoveryRuns(ctx, conn, projectID)
		if err != nil {
			return err
		}
		for _, run := range runs {
			// The unavailable branch wins over the continuation branch, exactly as
			// readContext checks blueprint.kind !== 'available' first
			// (main-generation-owner.ts:935-937).
			kind, diagnostic, err := readDirectoryRecoveryKind(ctx, conn, run.RunID)
			if err != nil {
				return err
			}
			if kind != "available" {
				need = &DirectoryRecoveryNeed{Run: run, Unavailable: true, Diagnostic: diagnostic}
				return nil
			}
			// readPlanningContinuation's invalid-receipt refusal is the reference's
			// readContext throw, which findBlueprintRecovery catches and skips
			// (main-generation-owner.ts:1069). A proved continuation is the
			// continued branch, whose writeState needs the unbuildable comparison.
			continuation, err := readPlanningContinuation(ctx, conn, run.RunID)
			if err != nil {
				continue
			}
			if continuation != nil {
				need = &DirectoryRecoveryNeed{Run: run, Continuation: continuation}
				return nil
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return need, nil
}

// readPlanningContinuation reads binding_json['$runEffects']['planningContinuation']
// for one run and proves it the way the reference does
// (generation-run-repository.ts:179-193). It returns (nil, nil) when the run
// carries no such receipt - which is every run this lane can read, because the
// ledger refuses to open any glPlanning operation that could write one
// (generation_ledger.go) - and refuses a receipt that does not prove out.
func readPlanningContinuation(ctx context.Context, conn *sql.Conn, runID string) (*PlanningContinuationReceipt, error) {
	run, err := bpLoadRun(ctx, conn, runID)
	if err != nil {
		return nil, err
	}
	effects := glMap(run.binding["$runEffects"])
	if effects == nil {
		return nil, nil
	}
	value, present := effects["planningContinuation"]
	if !present || value == nil {
		return nil, nil
	}
	receipt := glMap(value)
	if receipt == nil {
		return nil, glError("GENERATION_PLANNING_CONTINUATION_INVALID")
	}
	kind := glText(receipt["kind"])
	sourceHandle, sourceOK := bpCompleteHandle(receipt["sourceHandle"])
	nextHandle, nextOK := bpCompleteHandle(receipt["nextHandle"])
	requestHash := glText(receipt["requestHash"])
	if kind != "planning-continuation" || !sourceOK || !nextOK ||
		!bpContentHash.MatchString(requestHash) {
		return nil, glError("GENERATION_PLANNING_CONTINUATION_INVALID")
	}
	for key := range receipt {
		switch key {
		case "kind", "sourceHandle", "nextHandle", "requestHash":
		default:
			return nil, glError("GENERATION_PLANNING_CONTINUATION_INVALID")
		}
	}
	projectID := glText(run.binding["projectId"])
	if sourceHandle.RunID != runID || sourceHandle.RootActionID != run.rootID ||
		sourceHandle.ProjectID != projectID || nextHandle.ProjectID != projectID ||
		nextHandle.RootActionID == run.rootID {
		return nil, glError("GENERATION_PLANNING_CONTINUATION_INVALID")
	}
	next, err := bpLoadRun(ctx, conn, nextHandle.RunID)
	if err != nil {
		return nil, err
	}
	if next.rootID != nextHandle.RootActionID || glText(next.binding["projectId"]) != projectID {
		return nil, glError("GENERATION_PLANNING_CONTINUATION_INVALID")
	}
	// The reference re-reads the child's epoch over the receipt's own.
	nextHandle.Epoch = glText(next.binding["epoch"])
	return &PlanningContinuationReceipt{
		Kind:         kind,
		SourceHandle: sourceHandle,
		NextHandle:   nextHandle,
		RequestHash:  requestHash,
	}, nil
}

// listDirectoryRecoveryRuns is the run set findBlueprintRecovery walks
// (main-generation-owner.ts:1067): only chapter-blueprint-directory runs, newest
// first, filtered to this project so a shared database cannot leak another
// project's runs.
func listDirectoryRecoveryRuns(ctx context.Context, conn *sql.Conn, projectID string) ([]DirectoryRecoveryRun, error) {
	rows, err := rrMaps(ctx, conn, `SELECT run_id,root_action_id,binding_json FROM generation_runs
 WHERE json_extract(binding_json,'$.sourceManifest.operation')='chapter-blueprint-directory'
 AND json_extract(binding_json,'$.projectId')=?
 ORDER BY created_at_ms DESC, rowid DESC`, projectID)
	if err != nil {
		return nil, err
	}
	runs := make([]DirectoryRecoveryRun, 0, len(rows))
	for _, row := range rows {
		binding, err := glStored(row["binding_json"])
		if err != nil {
			return nil, err
		}
		manifest := glMap(binding["sourceManifest"])
		run := DirectoryRecoveryRun{
			RunID:        glText(row["run_id"]),
			RootActionID: glText(row["root_action_id"]),
			ProjectID:    glText(binding["projectId"]),
			Epoch:        glText(binding["epoch"]),
			ModelID:      directoryModelID(manifest),
			AuthorInputs: bpAuthorInputs(manifest["authorInputs"]),
			SelectedBlueprintChapterNumbers: directorySelectedChapters(manifest),
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// readDirectoryRecoveryKind reports whether one directory run's blueprint
// recovery is available, reusing bpRecovery so the two names cannot disagree
// about which receipt is readable.
func readDirectoryRecoveryKind(ctx context.Context, conn *sql.Conn, runID string) (string, BlueprintRecoveryDiagnostic, error) {
	recovery, err := bpRecovery(ctx, conn, runID, map[string]bool{})
	if err != nil {
		return "", BlueprintRecoveryDiagnostic{}, err
	}
	return recovery.kind, recovery.diagnostic, nil
}

// directoryModelID reads the model a directory run froze, the same way the
// editor-inline family reads both spellings (production_editor_inline_linux.go).
func directoryModelID(manifest map[string]any) string {
	if receipt := glMap(manifest["modelReceipt"]); receipt != nil {
		if modelID := glText(receipt["modelId"]); modelID != "" {
			return modelID
		}
	}
	return glText(manifest["modelId"])
}

// directorySelectedChapters reads the frozen selected chapter numbers, dropping
// entries that are not whole numbers like the reference's spread would.
func directorySelectedChapters(manifest map[string]any) []int64 {
	values, ok := plotArray(glPlain(manifest["selectedBlueprintChapterNumbers"]))
	if !ok {
		return []int64{}
	}
	chapters := make([]int64, 0, len(values))
	for _, value := range values {
		number, ok := plotNumber(value)
		if !ok || number != float64(int64(number)) {
			continue
		}
		chapters = append(chapters, int64(number))
	}
	return chapters
}
