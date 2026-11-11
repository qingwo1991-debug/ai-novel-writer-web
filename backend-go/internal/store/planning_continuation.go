package store

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
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

// DirectoryRecoveryNeed is one directory run that readContext would put in the
// recovery list: a run whose blueprint recovery is unavailable (the reference's
// writeState:'unavailable' branch) or one that carries a proved planning
// continuation (the reference's continued branch). AuthorEdit, RemainingRange
// and TargetUnits are the values planningContinuation/readContext read for the
// continued branch; AuthorEdit is the receipt readBlueprintRecovery returns as
// authorEdit (generation-run-repository.ts:564).
type DirectoryRecoveryNeed struct {
	Run            DirectoryRecoveryRun
	Unavailable    bool
	Diagnostic     BlueprintRecoveryDiagnostic
	Continuation   *PlanningContinuationReceipt
	AuthorEdit     *BlueprintAuthorEditReceipt
	RemainingRange *BlueprintRange
	TargetUnits    int64
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
			// (main-generation-owner.ts:935-937). readPlanningContinuation's
			// invalid-receipt refusal is the reference's readContext throw, which
			// findBlueprintRecovery catches and skips (main-generation-owner.ts:1069).
			candidate, err := directoryRecoveryNeed(ctx, conn, run)
			if err != nil {
				continue
			}
			if candidate != nil {
				need = candidate
				return nil
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return need, nil
}

// ListDirectoryRecoveryNeeds is the planning bucket of listRecovery
// (main-generation-owner.ts:1021-1024): every chapter-blueprint-directory run of
// the project whose readContext would push a context. A per-run read failure is
// the reference's catch setting partial=true, not a failure of the list.
func ListDirectoryRecoveryNeeds(ctx context.Context, db *sql.DB, projectID string) ([]*DirectoryRecoveryNeed, bool, error) {
	needs := []*DirectoryRecoveryNeed{}
	partial := false
	if err := bpRead(ctx, db, func(conn *sql.Conn) error {
		runs, err := listDirectoryRecoveryRuns(ctx, conn, projectID)
		if err != nil {
			return err
		}
		for _, run := range runs {
			need, err := directoryRecoveryNeed(ctx, conn, run)
			if err != nil {
				partial = true
				continue
			}
			if need != nil {
				needs = append(needs, need)
			}
		}
		return nil
	}); err != nil {
		return nil, false, err
	}
	return needs, partial, nil
}

// directoryRecoveryNeed reduces one directory run the way readContext does. An
// unavailable blueprint recovery is the one non-continuation need. An available
// recovery without a proved continuation is skipped: readContext does not set
// blueprintRecovery for it (main-generation-owner.ts:938-943).
func directoryRecoveryNeed(ctx context.Context, conn *sql.Conn, run DirectoryRecoveryRun) (*DirectoryRecoveryNeed, error) {
	recovery, err := bpRecovery(ctx, conn, run.RunID, map[string]bool{})
	if err != nil {
		return nil, err
	}
	if recovery.kind != "available" {
		return &DirectoryRecoveryNeed{Run: run, Unavailable: true, Diagnostic: recovery.diagnostic}, nil
	}
	continuation, err := readPlanningContinuation(ctx, conn, run.RunID)
	if err != nil {
		return nil, err
	}
	if continuation == nil {
		return nil, nil
	}
	remaining, err := directoryPlanningRemainingRange(run, recovery)
	if err != nil {
		return nil, err
	}
	targetUnits, err := directoryPlanningTargetUnits(run)
	if err != nil {
		return nil, err
	}
	return &DirectoryRecoveryNeed{
		Run:            run,
		Continuation:   continuation,
		AuthorEdit:     recovery.authorEdit,
		RemainingRange: remaining,
		TargetUnits:    targetUnits,
	}, nil
}

// directoryPlanningRemainingRange is the remainingRange planningContinuation
// derives (main-generation-owner.ts:776-784): the author-edit receipt's own
// remainingRange when one is saved, else the single directory progress receipt's
// remaining range, else the requested scope itself when the run has no progress.
func directoryPlanningRemainingRange(run DirectoryRecoveryRun, recovery bpRecoveryResult) (*BlueprintRange, error) {
	if recovery.authorEdit != nil {
		return recovery.authorEdit.RemainingRange, nil
	}
	// planningContinuation refuses more than one progress receipt
	// (main-generation-owner.ts:779-780).
	if len(recovery.progress) > 1 {
		return nil, glError("GENERATION_DIRECTORY_PROGRESS_INVALID")
	}
	if len(recovery.progress) == 1 {
		remaining := recovery.progress[0].RemainingRange
		if remaining == nil {
			return nil, nil
		}
		return &BlueprintRange{From: remaining.StartChapter, To: remaining.EndChapter}, nil
	}
	for _, input := range run.AuthorInputs {
		if input.ID != "directory:requested-range" {
			continue
		}
		value, err := glParse(input.Text)
		if err != nil {
			return nil, err
		}
		scope, ok := bpRangeObject(value)
		if !ok {
			return nil, glError("GENERATION_DIRECTORY_PROGRESS_INVALID")
		}
		return &BlueprintRange{From: scope.StartChapter, To: scope.EndChapter}, nil
	}
	// Without a directory scope planningContinuation is undefined, so a run that
	// carries a continuation but no scope is not projectable.
	return nil, glError("GENERATION_DIRECTORY_PROGRESS_INVALID")
}

// directoryPlanningTargetUnits is parsePlanningTargetUnits on the frozen
// planning:target-units input (main-generation-owner.ts:785,
// src/shared/plot-outline-contract.ts:13-18): absent means 600, and anything
// that is not a safe integer in 1..1000 throws GENERATION_PLANNING_TARGET_INVALID.
func directoryPlanningTargetUnits(run DirectoryRecoveryRun) (int64, error) {
	for _, input := range run.AuthorInputs {
		if input.ID != "planning:target-units" {
			continue
		}
		value, err := strconv.ParseFloat(strings.TrimSpace(input.Text), 64)
		if err != nil || value != float64(int64(value)) || int64(value) < 1 || int64(value) > 1000 {
			return 0, glError("GENERATION_PLANNING_TARGET_INVALID")
		}
		return int64(value), nil
	}
	return 600, nil
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
