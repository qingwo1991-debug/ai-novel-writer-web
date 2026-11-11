package store

import (
	"context"
	"database/sql"
	"regexp"
	"sort"
	"strings"
)

// This file DERIVES the batch and directory generation progress of one admitted
// project database. It is a pure read: every statement is a SELECT, every
// transaction is a deferred read transaction that is committed without a write,
// and no method here creates a row, updates a receipt or advances a run.
//
// # Where the model comes from
//
// There is no batch table. A batch is one generation root plus one run whose
// binding_json.sourceManifest.operation is "batch-chapters" and whose manifest
// carries the frozen batchIntent; the batch id IS that run's run_id
// (electron/services/generation-draft-effects.ts:104-107, reached from
// electron/services/main-generation-owner.ts:1522-1533). Per-chapter state is
// derived, never stored: absent -> drafted{draftCommit} -> finalized{pending
// FinalizationId} -> postProcessComplete -> finalizationId
// (generation-draft-effects.ts:134-137).
//
// The inputs are, in full:
//
//	generation_runs.binding_json
//	    $.projectId, $.epoch, $.sourceManifest.operation,
//	    $.sourceManifest.batchId, $.sourceManifest.batchIntent,
//	    $.sourceManifest.chapterNumber, $.sourceManifest.modelReceipt.modelId,
//	    $.sourceManifest.authorInputs, $.sourceManifest.selectedBlueprintChapterNumbers,
//	    $."$blueprintRetirement", $."$runEffects".blueprintAuthorEdit
//	generation_attempts.usage_receipt_json
//	    $.draftCommit, $.visibleComposition, $.artifactIdentity.epoch,
//	    $.directoryProgress
//	generation_attempts.run_id / attempt_id  and  generation_runs.root_action_id
//	drafts.id, drafts.version, drafts.chapter_number, drafts.status, contents.body
//	finalization_outbox.finalization_id, .content_hash, .content_snapshot,
//	    .chapter_number, .publication_status
//	post_process_runs.id, .all_critical_passed, .created_at
//	post_process_steps.step_key, .critical, .ok
//	blueprint_commit_operations.payload_hash, .start_chapter, .end_chapter
//
// # Refusal codes, preserved
//
// GENERATION_BATCH_IDENTITY_INVALID    wrong project, or the run is not a batch
// GENERATION_BATCH_INTENT_INVALID      the frozen intent is not a batch intent
// GENERATION_BATCH_LINEAGE_INVALID     a chapter commit belongs to another root
// GENERATION_BATCH_PROGRESS_INVALID    a gap, an out-of-range chapter, or two
//                                      uncommitted runs for one chapter
// GENERATION_DRAFT_RECEIPT_INVALID     a draftCommit receipt the rows contradict
// GENERATION_BATCH_DRAFT_REPLACED      the committed draft is no longer newest
// GENERATION_BATCH_FINALIZATION_CONFLICT  the outbox disagrees with the receipt
// GENERATION_BATCH_FINALIZATION_REQUIRED  confirm: the chapter is not finalized
// GENERATION_DIRECTORY_PROGRESS_INVALID   a directory receipt the rows contradict
// GENERATION_BLUEPRINT_AUTHOR_RECEIPT_INVALID  an author-edit receipt that lies
// GENERATION_RUN_MISSING               the batch/run id is not in this database
//
// The scoping note that motivated this file named a
// "GENERATION_BATCH_RECEIPT_INVALID" code. No such code exists anywhere in this
// repository; the receipt refusal the original raises is
// GENERATION_DRAFT_RECEIPT_INVALID (generation-draft-effects.ts:75) and that is
// the code reproduced here.

const (
	// bpMaxBatchChapters is MAX_BATCH_CHAPTERS (src/shared/generation-owner-contract.ts:224):
	// the intent admits a span of at most ten chapters, refused at exactly ten.
	bpMaxBatchChapters = 10
	// bpMaxBatchTargetUnits is the intent's targetUnits ceiling (:27).
	bpMaxBatchTargetUnits = 1_000_000
	// bpDirectoryRangeLimit bounds one directory request span (:527).
	bpDirectoryRangeLimit = 10_000

	bpBatchOperation     = "batch-chapters"
	bpDirectoryOperation = "chapter-blueprint-directory"
	bpDraftVisibleV1     = "draft-visible-v1"
	bpDraftVisibleV2     = "draft-visible-v2"
	bpReceiptInvalid     = "GENERATION_DRAFT_RECEIPT_INVALID"
)

// bpContentHash is isContentHash (src/shared/source-ref.ts:14): a lower-case hex
// sha256, never a length-plausible arbitrary string.
var bpContentHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

// BatchChapterRange is the closed chapter range a batch intent or a directory
// operation froze.
type BatchChapterRange struct {
	StartChapter int64 `json:"startChapter"`
	EndChapter   int64 `json:"endChapter"`
}

// BlueprintRange is the {from,to} range the planning contract uses
// (src/shared/plot-outline-contract.ts:39, generation-owner-contract.ts:176-184).
// It is deliberately not BatchChapterRange: a blueprint author-edit receipt and
// a planning continuation both spell their ranges from/to, and reusing the
// startChapter/endChapter accessor would drop a real receipt.
type BlueprintRange struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// BlueprintAuthorEditReceipt is BlueprintAuthorEditReceipt
// (src/shared/generation-owner-contract.ts:176-184): the frozen effect
// readBlueprintAuthorEdit proves and readBlueprintRecovery returns as its
// authorEdit (generation-run-repository.ts:194-214, :564). The recovery context
// projects it as blueprintRecovery.saved
// (main-generation-owner.ts:932, :941).
type BlueprintAuthorEditReceipt struct {
	Kind           string          `json:"kind"`
	OperationID    string          `json:"operationId"`
	SourceHandle   BatchRunHandle  `json:"sourceHandle"`
	RequestedRange BlueprintRange  `json:"requestedRange"`
	CommittedRange BlueprintRange  `json:"committedRange"`
	RemainingRange *BlueprintRange `json:"remainingRange"`
	PayloadHash    string          `json:"payloadHash"`
}

// BatchAuthorInput is one raw author input frozen into a run's source manifest.
type BatchAuthorInput struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// BatchRunHandle is the browser run handle: exactly the four keys the renderer
// sends back, with no project path and no lease.
type BatchRunHandle struct {
	ProjectID    string `json:"projectId"`
	Epoch        string `json:"epoch"`
	RootActionID string `json:"rootActionId"`
	RunID        string `json:"runId"`
}

// BatchCompletedChapter is one derived chapter of a batch. finalizationId means
// the outbox row is published; pendingFinalizationId means it exists but has not
// been published yet. Exactly one of the two is present when the chapter is
// finalized at all.
type BatchCompletedChapter struct {
	ChapterNumber         int64          `json:"chapterNumber"`
	DraftID               int64          `json:"draftId"`
	Version               int64          `json:"version"`
	ContentHash           string         `json:"contentHash"`
	SourceRunHandle       BatchRunHandle `json:"sourceRunHandle"`
	FinalizationID        *string        `json:"finalizationId,omitempty"`
	PendingFinalizationID *string        `json:"pendingFinalizationId,omitempty"`
	PostProcessComplete   bool           `json:"postProcessComplete"`
}

// GenerationBatchProgress is GenerationBatchProgress
// (src/shared/generation-owner-contract.ts:237-249).
type GenerationBatchProgress struct {
	Mode                    string                  `json:"mode"`
	Range                   BatchChapterRange       `json:"range"`
	TargetUnits             int64                   `json:"targetUnits"`
	ModelID                 string                  `json:"modelId"`
	BatchID                 string                  `json:"batchId"`
	RootHandle              BatchRunHandle          `json:"rootHandle"`
	CompletedChapters       []BatchCompletedChapter `json:"completedChapters"`
	CurrentChapterRunHandle *BatchRunHandle         `json:"currentChapterRunHandle,omitempty"`
	NextChapterNumber       *int64                  `json:"nextChapterNumber"`
	AuthorInputs            []BatchAuthorInput      `json:"authorInputs"`
}

// GenerationBatchHistory is GenerationBatchHistory (:248-254). inFlight is
// deliberately absent: the original's readBatchHistory never sets it, so a field
// that could only ever be false would be an invented shape.
type GenerationBatchHistory struct {
	GenerationBatchProgress
	SourceCurrent bool `json:"sourceCurrent"`
}

// DirectoryGenerationProgress is DirectoryGenerationProgress (:21-32). The
// continuation handle's epoch is re-read from the child run, exactly like
// generation-run-repository.ts:545, and authorInputs is the read projection the
// owner adds on the way out (main-generation-owner.ts:1550-1553): the SOURCE
// run's frozen array, never the receipt's. An empty frozen array is projected as
// absent, the one place where `omitempty` differs from `undefined`.
type DirectoryGenerationProgress struct {
	OperationID        string             `json:"operationId"`
	PayloadHash        string             `json:"payloadHash"`
	SourceHandle       BatchRunHandle     `json:"sourceHandle"`
	RequestedRange     BatchChapterRange  `json:"requestedRange"`
	CommittedRange     BatchChapterRange  `json:"committedRange"`
	RemainingRange     *BatchChapterRange `json:"remainingRange"`
	ContinuationHandle *BatchRunHandle    `json:"continuationHandle,omitempty"`
	AuthorInputs       []BatchAuthorInput `json:"authorInputs,omitempty"`
}

// BlueprintRecoveryDiagnostic is BlueprintRecoveryDiagnostic (:33-37).
type BlueprintRecoveryDiagnostic struct {
	RunID string `json:"runId"`
	Kind  string `json:"kind"`
	Code  string `json:"code"`
}

// DirectoryGenerationCatalog is DirectoryGenerationCatalog (:35-38).
type DirectoryGenerationCatalog struct {
	Progress    []DirectoryGenerationProgress `json:"progress"`
	Diagnostics []BlueprintRecoveryDiagnostic `json:"diagnostics"`
}

// ReadGenerationBatchProgress is generation:read-batch: the strict projection.
// A source conflict - a replaced draft, a receipt whose body no longer matches,
// an outbox that disagrees with the receipt - is refused here instead of being
// reported as progress, exactly like readBatch (:95-99).
func ReadGenerationBatchProgress(ctx context.Context, db *sql.DB, projectID, batchID string) (*GenerationBatchProgress, error) {
	var progress *GenerationBatchProgress
	if err := bpRead(ctx, db, func(conn *sql.Conn) error {
		commits, err := bpBatchCommits(ctx, conn, batchID)
		if err != nil {
			return err
		}
		snapshot, err := bpSnapshot(ctx, conn, projectID, batchID, commits)
		if err != nil {
			return err
		}
		if snapshot.conflict != "" {
			return glError(snapshot.conflict)
		}
		progress = snapshot.progress
		return nil
	}); err != nil {
		return nil, err
	}
	return progress, nil
}

// ListGenerationBatchHistory is generation:list-batches: every run of this
// project whose manifest carries a batchIntent, newest first. A source conflict
// is reported as sourceCurrent=false instead of a refusal (:100-103), but every
// other refusal still stops the list - the original maps without recovering.
func ListGenerationBatchHistory(ctx context.Context, db *sql.DB, projectID string) ([]GenerationBatchHistory, error) {
	history := make([]GenerationBatchHistory, 0)
	if err := bpRead(ctx, db, func(conn *sql.Conn) error {
		groups, _, err := bpAllCommits(ctx, conn)
		if err != nil {
			return err
		}
		rows, err := rrMaps(ctx, conn, "SELECT run_id,root_action_id,binding_json FROM generation_runs ORDER BY created_at_ms DESC, rowid DESC")
		if err != nil {
			return err
		}
		for _, row := range rows {
			binding, err := glStored(row["binding_json"])
			if err != nil {
				return err
			}
			if glText(binding["projectId"]) != projectID {
				continue
			}
			// runSummaries marks a batch root by the presence of a batch intent,
			// not by a batch id: chapter-draft children carry a batchId only
			// (main-generation-owner.ts:351).
			if !phTruthy(glMap(binding["sourceManifest"])["batchIntent"]) {
				continue
			}
			batchID := glText(row["run_id"])
			commits := groups[batchID]
			if commits == nil {
				commits = []bpCommitRow{}
			}
			snapshot, err := bpSnapshot(ctx, conn, projectID, batchID, commits)
			if err != nil {
				return err
			}
			history = append(history, GenerationBatchHistory{
				GenerationBatchProgress: *snapshot.progress,
				SourceCurrent:           snapshot.conflict == "",
			})
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return history, nil
}

// ConfirmGenerationBatchFinalization is generation:confirm-batch-finalization.
// It is a READ that confirms a finalization which already happened: the chapter
// must be published (a finalizationId, never a pending one) AND its post-process
// run must be complete. Anything else is refused with
// GENERATION_BATCH_FINALIZATION_REQUIRED (main-generation-owner.ts:1534-1538),
// and no row is ever written.
func ConfirmGenerationBatchFinalization(ctx context.Context, db *sql.DB, projectID, batchID string, chapterNumber int64, finalizationID string) (*GenerationBatchProgress, error) {
	progress, err := ReadGenerationBatchProgress(ctx, db, projectID, batchID)
	if err != nil {
		return nil, err
	}
	for _, chapter := range progress.CompletedChapters {
		if chapter.ChapterNumber != chapterNumber || chapter.FinalizationID == nil {
			continue
		}
		if *chapter.FinalizationID == finalizationID && chapter.PostProcessComplete {
			return progress, nil
		}
	}
	return nil, glError("GENERATION_BATCH_FINALIZATION_REQUIRED")
}

// ListDirectoryGenerationProgress is generation:list-directory-progress. The
// catalog is per project database, so it needs no project argument: the run set
// is exactly the directory runs of this database plus every run carrying a
// retirement marker, an author-edit effect or a directory receipt
// (generation-run-repository.ts:599-618).
func ListDirectoryGenerationProgress(ctx context.Context, db *sql.DB) (*DirectoryGenerationCatalog, error) {
	catalog := &DirectoryGenerationCatalog{
		Progress:    []DirectoryGenerationProgress{},
		Diagnostics: []BlueprintRecoveryDiagnostic{},
	}
	if err := bpRead(ctx, db, func(conn *sql.Conn) error {
		rows, err := rrMaps(ctx, conn, `SELECT r.run_id FROM generation_runs r
 WHERE json_extract(r.binding_json,'$.sourceManifest.operation')='chapter-blueprint-directory'
 OR json_type(r.binding_json,'$."$blueprintRetirement"') IS NOT NULL
 OR json_type(r.binding_json,'$."$runEffects".blueprintAuthorEdit') IS NOT NULL
 OR EXISTS (SELECT 1 FROM generation_attempts t WHERE t.run_id=r.run_id
   AND json_type(t.usage_receipt_json,'$.directoryProgress') IS NOT NULL)
 ORDER BY r.rowid`)
		if err != nil {
			return err
		}
		for _, row := range rows {
			runID := glText(row["run_id"])
			recovery, err := bpRecovery(ctx, conn, runID, map[string]bool{})
			if err != nil {
				return err
			}
			if recovery.kind == "available" {
				catalog.Progress = append(catalog.Progress, recovery.progress...)
				continue
			}
			catalog.Diagnostics = append(catalog.Diagnostics, recovery.diagnostic)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return catalog, nil
}

// bpRead runs one derivation on a borrowed connection inside a deferred read
// transaction, using the same BEGIN/COMMIT discipline as ListRuns so a pooled
// connection never keeps a transaction open.
func bpRead(ctx context.Context, db *sql.DB, read func(*sql.Conn) error) error {
	if db == nil {
		return glError("GENERATION_DATABASE_NOT_READY")
	}
	if ctx == nil {
		return glError("GENERATION_INPUT_INVALID")
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
	}()
	if err := read(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// bpRun is one generation_runs row with its stored binding parsed.
type bpRun struct {
	runID   string
	rootID  string
	status  string
	binding map[string]any
}

func (r *bpRun) manifest() map[string]any { return glMap(r.binding["sourceManifest"]) }

func bpHandleOf(run *bpRun) BatchRunHandle {
	return BatchRunHandle{
		ProjectID:    glText(run.binding["projectId"]),
		Epoch:        glText(run.binding["epoch"]),
		RootActionID: run.rootID,
		RunID:        run.runID,
	}
}

func bpLoadRun(ctx context.Context, conn *sql.Conn, runID string) (*bpRun, error) {
	rows, err := rrMaps(ctx, conn, "SELECT run_id,root_action_id,binding_json,status FROM generation_runs WHERE run_id=?", runID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, glError("GENERATION_RUN_MISSING")
	}
	binding, err := glStored(rows[0]["binding_json"])
	if err != nil {
		return nil, err
	}
	return &bpRun{
		runID:   glText(rows[0]["run_id"]),
		rootID:  glText(rows[0]["root_action_id"]),
		status:  glText(rows[0]["status"]),
		binding: binding,
	}, nil
}

// bpCommitRow is one attempt of the batch, reduced to what the derivation needs.
type bpCommitRow struct {
	attemptID string
	runID     string
	usage     any
}

func bpBatchCommits(ctx context.Context, conn *sql.Conn, batchID string) ([]bpCommitRow, error) {
	rows, err := rrMaps(ctx, conn, `SELECT attempt_id,run_id,usage_receipt_json FROM generation_attempts
 WHERE json_extract(usage_receipt_json,'$.draftCommit.batchId')=? ORDER BY rowid`, batchID)
	if err != nil {
		return nil, err
	}
	return bpCommitRows(rows), nil
}

// bpAllCommits is batchCommits() with no batch id: every receipt that names a
// batch, grouped in first-seen order with the rows of each group in attempt
// order (generation-draft-effects.ts:47-53).
func bpAllCommits(ctx context.Context, conn *sql.Conn) (map[string][]bpCommitRow, []string, error) {
	rows, err := rrMaps(ctx, conn, `SELECT attempt_id,run_id,usage_receipt_json,
 json_extract(usage_receipt_json,'$.draftCommit.batchId') AS batch_id FROM generation_attempts
 WHERE json_extract(usage_receipt_json,'$.draftCommit.batchId') IS NOT NULL ORDER BY rowid`)
	if err != nil {
		return nil, nil, err
	}
	groups := make(map[string][]bpCommitRow)
	order := make([]string, 0)
	for _, row := range rows {
		batchID := glText(row["batch_id"])
		if _, seen := groups[batchID]; !seen {
			order = append(order, batchID)
		}
		groups[batchID] = append(groups[batchID], bpCommitRow{
			attemptID: glText(row["attempt_id"]),
			runID:     glText(row["run_id"]),
			usage:     row["usage_receipt_json"],
		})
	}
	return groups, order, nil
}

func bpCommitRows(rows []map[string]any) []bpCommitRow {
	out := make([]bpCommitRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, bpCommitRow{
			attemptID: glText(row["attempt_id"]),
			runID:     glText(row["run_id"]),
			usage:     row["usage_receipt_json"],
		})
	}
	return out
}

type bpSnapshotResult struct {
	progress *GenerationBatchProgress
	conflict string
}

// bpSnapshot is readBatchSnapshot (generation-draft-effects.ts:104-153). Nothing
// in it writes; the two receipt keys it reads are projections of rows that exist
// elsewhere in this same database.
func bpSnapshot(ctx context.Context, conn *sql.Conn, projectID, batchID string, commits []bpCommitRow) (*bpSnapshotResult, error) {
	root, err := bpLoadRun(ctx, conn, batchID)
	if err != nil {
		return nil, err
	}
	manifest := root.manifest()
	mode, start, end, target, err := bpIntent(manifest["batchIntent"])
	if err != nil {
		return nil, err
	}
	// The identity refusal is checked after the intent, exactly like the
	// original: a run that is not a batch is a wrong intent, not a wrong project.
	if glText(root.binding["projectId"]) != projectID || glText(manifest["operation"]) != bpBatchOperation {
		return nil, glError("GENERATION_BATCH_IDENTITY_INVALID")
	}
	// The original dereferences modelReceipt.modelId without a guard, so a batch
	// root without one throws instead of being served. Refuse it through the
	// identity code rather than emitting a model id this database never froze.
	modelID := glText(glMap(manifest["modelReceipt"])["modelId"])
	if modelID == "" {
		return nil, glError("GENERATION_BATCH_IDENTITY_INVALID")
	}
	conflict := ""
	chapters := make([]BatchCompletedChapter, 0, len(commits))
	for _, row := range commits {
		saved, err := bpVerifyCommit(ctx, conn, row)
		if err != nil {
			return nil, err
		}
		if saved == nil {
			continue
		}
		if !saved.matchesBody && conflict == "" {
			conflict = bpReceiptInvalid
		}
		if saved.rootActionID != root.rootID {
			return nil, glError("GENERATION_BATCH_LINEAGE_INVALID")
		}
		latest, ok, err := bpLatestDraftID(ctx, conn, saved.chapterNumber)
		if err != nil {
			return nil, err
		}
		if !ok || latest != saved.draftID || saved.draftStatus == "superseded" {
			if conflict == "" {
				conflict = "GENERATION_BATCH_DRAFT_REPLACED"
			}
		}
		outbox, err := bpOutbox(ctx, conn, saved.draftID)
		if err != nil {
			return nil, err
		}
		if outbox != nil {
			if outbox.contentHash != recoveryHash(outbox.contentSnapshot) || outbox.chapterNumber != saved.chapterNumber {
				return nil, glError("GENERATION_BATCH_FINALIZATION_CONFLICT")
			}
			// The outbox is the immutable finalization snapshot: when it disagrees
			// with the receipt, only a receipted draft whose body IS that snapshot
			// can reconcile the two, and then the finalization is spent.
			if outbox.contentSnapshot != saved.content {
				if !saved.draftPresent || !bpReceiptedStatus(saved.draftStatus) ||
					!saved.bodyOK || saved.body != outbox.contentSnapshot {
					return nil, glError("GENERATION_BATCH_FINALIZATION_CONFLICT")
				}
				if conflict == "" {
					conflict = "GENERATION_BATCH_FINALIZATION_CONFLICT"
				}
				outbox = nil
			}
		}
		postProcessComplete := false
		if outbox != nil {
			postProcessComplete, err = bpPostProcessComplete(ctx, conn, outbox.finalizationID)
			if err != nil {
				return nil, err
			}
		}
		chapter := BatchCompletedChapter{
			ChapterNumber:       saved.chapterNumber,
			DraftID:             saved.draftID,
			Version:             saved.version,
			ContentHash:         saved.contentHash,
			SourceRunHandle:     saved.handle,
			PostProcessComplete: postProcessComplete,
		}
		if outbox != nil {
			finalizationID := outbox.finalizationID
			if outbox.publicationStatus == "published" {
				chapter.FinalizationID = &finalizationID
			} else {
				chapter.PendingFinalizationID = &finalizationID
			}
		}
		chapters = append(chapters, chapter)
	}
	// A receipt whose content disagrees with the draft body is a conflict the
	// strict read refuses; it is still counted here so list-batches reports the
	// chapter with sourceCurrent=false instead of dropping it.
	sort.SliceStable(chapters, func(i, j int) bool { return chapters[i].ChapterNumber < chapters[j].ChapterNumber })

	// nextChapterNumber is a cursor over the completed run: it starts at the first
	// chapter of the range, advances one chapter per completed chapter, and is
	// absent once the range is finished. A gap, a duplicate or a chapter beyond
	// the range is a progress conflict (:126-145).
	cursor := start
	for index, chapter := range chapters {
		if chapter.ChapterNumber != start+int64(index) || chapter.ChapterNumber > end {
			return nil, glError("GENERATION_BATCH_PROGRESS_INVALID")
		}
		if chapter.ChapterNumber != cursor {
			return nil, glError("GENERATION_BATCH_PROGRESS_INVALID")
		}
		// auto_finalize stops at the first chapter whose projection is not
		// complete, and the cursor must stay on it so the pending run for exactly
		// that chapter is the one reported.
		if mode == "auto_finalize" && (chapter.FinalizationID == nil || !chapter.PostProcessComplete) {
			break
		}
		cursor = chapter.ChapterNumber + 1
	}
	var next *int64
	if cursor <= end {
		next = &cursor
	}
	pending, err := bpPendingRuns(ctx, conn, root.rootID, batchID, next)
	if err != nil {
		return nil, err
	}
	if len(pending) > 1 {
		return nil, glError("GENERATION_BATCH_PROGRESS_INVALID")
	}
	progress := &GenerationBatchProgress{
		Mode:              mode,
		Range:             BatchChapterRange{StartChapter: start, EndChapter: end},
		TargetUnits:       target,
		ModelID:           modelID,
		BatchID:           batchID,
		RootHandle:        bpHandleOf(root),
		CompletedChapters: chapters,
		NextChapterNumber: next,
		AuthorInputs:      bpAuthorInputs(manifest["authorInputs"]),
	}
	if len(pending) == 1 {
		handle := bpHandleOf(pending[0])
		progress.CurrentChapterRunHandle = &handle
	}
	return &bpSnapshotResult{progress: progress, conflict: conflict}, nil
}

// bpPendingRuns is the pending filter of :146-149: a run under the same root
// that names this batch and the NEXT chapter but has not saved a draft yet.
func bpPendingRuns(ctx context.Context, conn *sql.Conn, rootID, batchID string, next *int64) ([]*bpRun, error) {
	if next == nil {
		return nil, nil
	}
	rows, err := rrMaps(ctx, conn, "SELECT run_id,root_action_id,binding_json,status FROM generation_runs WHERE root_action_id=?", rootID)
	if err != nil {
		return nil, err
	}
	pending := make([]*bpRun, 0, 1)
	for _, row := range rows {
		binding, err := glStored(row["binding_json"])
		if err != nil {
			return nil, err
		}
		run := &bpRun{runID: glText(row["run_id"]), rootID: glText(row["root_action_id"]), status: glText(row["status"]), binding: binding}
		manifest := run.manifest()
		if glText(manifest["batchId"]) != batchID {
			continue
		}
		chapterNumber, ok := rrInteger(manifest["chapterNumber"], -9007199254740991)
		if !ok || chapterNumber != *next {
			continue
		}
		saved, err := bpSaveState(ctx, conn, run.runID)
		if err != nil {
			return nil, err
		}
		if saved {
			continue
		}
		pending = append(pending, run)
	}
	return pending, nil
}

// bpSaveState is readSaveState (:78-82): at most one draftCommit receipt, and
// any existence at all means the chapter run already saved.
func bpSaveState(ctx context.Context, conn *sql.Conn, runID string) (bool, error) {
	rows, err := rrMaps(ctx, conn, "SELECT attempt_id,run_id,usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid", runID)
	if err != nil {
		return false, err
	}
	count := 0
	for _, row := range rows {
		saved, err := bpVerifyCommit(ctx, conn, bpCommitRow{attemptID: glText(row["attempt_id"]), runID: runID, usage: row["usage_receipt_json"]})
		if err != nil {
			return false, err
		}
		if saved != nil {
			count++
		}
	}
	if count > 1 {
		return false, glError(bpReceiptInvalid)
	}
	return count == 1, nil
}

type bpVerifiedCommit struct {
	chapterNumber int64
	draftID       int64
	version       int64
	contentHash   string
	content       string
	handle        BatchRunHandle
	rootActionID  string
	matchesBody   bool
	draftPresent  bool
	draftStatus   string
	body          string
	bodyOK        bool
}

// bpVerifyCommit is verifiedCommit (:65-77) with one documented reduction: the
// original recomputes the visible composition from the run's artifacts and
// compares both hashes. This store does not carry a port of
// sanitizeDraftText/composeVisibleContinuation, so it compares the receipt
// against the composition hash the run's own compose-visible receipt froze,
// against sha256 of the receipt's content, and against the draft row the receipt
// names. A receipt whose content disagrees with any of those is refused; a
// tampered ARTIFACT is not re-derived here.
func bpVerifyCommit(ctx context.Context, conn *sql.Conn, row bpCommitRow) (*bpVerifiedCommit, error) {
	if row.usage == nil {
		return nil, nil
	}
	usage, err := glStored(row.usage)
	if err != nil {
		return nil, err
	}
	stored := glMap(usage["draftCommit"])
	if stored == nil {
		// An absent or JSON-null receipt means "no commit"; a truthy non-object
		// is exactly the receipt the original refuses to read.
		if phTruthy(usage["draftCommit"]) {
			return nil, glError(bpReceiptInvalid)
		}
		return nil, nil
	}
	run, err := bpLoadRun(ctx, conn, row.runID)
	if err != nil {
		return nil, err
	}
	manifest := run.manifest()
	success, _ := stored["success"].(bool)
	chapterNumber, chapterOK := rrInteger(stored["chapterNumber"], -9007199254740991)
	draftID, draftOK := rrInteger(stored["id"], -9007199254740991)
	version, versionOK := rrInteger(stored["version"], -9007199254740991)
	contentHash, hashOK := stored["contentHash"].(string)
	content, contentOK := stored["content"].(string)
	batchID, batchOK := stored["batchId"].(string)
	handle := glMap(stored["handle"])
	identity := glMap(usage["artifactIdentity"])
	manifestChapter, manifestChapterOK := rrInteger(manifest["chapterNumber"], -9007199254740991)
	manifestBatch, manifestBatchOK := manifest["batchId"].(string)
	compositionHash, err := bpCompositionHash(ctx, conn, run.runID)
	if err != nil {
		return nil, err
	}
	expected := BatchRunHandle{
		ProjectID:    glText(run.binding["projectId"]),
		Epoch:        glText(identity["epoch"]),
		RootActionID: run.rootID,
		RunID:        run.runID,
	}
	if !success || !chapterOK || !draftOK || !versionOK || !hashOK || !contentOK || !batchOK ||
		len(handle) != 4 || identity == nil || !manifestChapterOK || !manifestBatchOK ||
		chapterNumber != manifestChapter || batchID != manifestBatch ||
		compositionHash == "" || contentHash != recoveryHash(content) || contentHash != compositionHash ||
		glText(handle["projectId"]) != expected.ProjectID || glText(handle["epoch"]) != expected.Epoch ||
		glText(handle["rootActionId"]) != expected.RootActionID || glText(handle["runId"]) != expected.RunID {
		return nil, glError(bpReceiptInvalid)
	}
	draft, err := bpDraft(ctx, conn, draftID)
	if err != nil {
		return nil, err
	}
	if draft != nil && (chapterNumber != draft.chapterNumber || version != draft.version) {
		return nil, glError(bpReceiptInvalid)
	}
	saved := &bpVerifiedCommit{
		chapterNumber: chapterNumber,
		draftID:       draftID,
		version:       version,
		contentHash:   contentHash,
		content:       content,
		handle:        expected,
		rootActionID:  glText(handle["rootActionId"]),
		draftStatus:   "",
	}
	if draft != nil {
		saved.draftStatus = draft.status
		saved.body = draft.body
		saved.bodyOK = draft.bodyOK
		saved.draftPresent = true
		saved.matchesBody = draft.bodyOK && content == draft.body
	}
	return saved, nil
}

// bpCompositionHash is the textHash of the newest stored visible composition of
// one run (generation-run-repository.ts:648-664). A run with no draft-visible
// composition receipt has no usable composition, exactly like a null or
// non-draft-visible one: the caller refuses the receipt.
func bpCompositionHash(ctx context.Context, conn *sql.Conn, runID string) (string, error) {
	rows, err := rrMaps(ctx, conn, `SELECT json_extract(usage_receipt_json,'$.visibleComposition') AS composition
 FROM generation_attempts WHERE run_id=? AND json_type(usage_receipt_json,'$.visibleComposition') IS NOT NULL
 ORDER BY rowid DESC LIMIT 1`, runID)
	if err != nil || len(rows) == 0 {
		return "", err
	}
	text, ok := rows[0]["composition"].(string)
	if !ok {
		return "", nil
	}
	value, err := glParse(text)
	if err != nil {
		return "", err
	}
	composition := glMap(value)
	if composition == nil {
		return "", nil
	}
	algorithm := glText(composition["algorithm"])
	if algorithm != bpDraftVisibleV1 && algorithm != bpDraftVisibleV2 {
		return "", nil
	}
	textHash, ok := composition["textHash"].(string)
	if !ok || textHash == "" {
		return "", nil
	}
	return textHash, nil
}

type bpDraftRow struct {
	id            int64
	version       int64
	chapterNumber int64
	status        string
	body          string
	bodyOK        bool
}

func bpDraft(ctx context.Context, conn *sql.Conn, draftID int64) (*bpDraftRow, error) {
	rows, err := rrMaps(ctx, conn, `SELECT d.id,d.version,d.chapter_number,d.status,c.body
 FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=?`, draftID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	id, idOK := rrInteger(rows[0]["id"], -9007199254740991)
	version, versionOK := rrInteger(rows[0]["version"], -9007199254740991)
	chapterNumber, chapterOK := rrInteger(rows[0]["chapter_number"], -9007199254740991)
	if !idOK || !versionOK || !chapterOK {
		return nil, glError(bpReceiptInvalid)
	}
	body, bodyOK := rows[0]["body"].(string)
	return &bpDraftRow{id: id, version: version, chapterNumber: chapterNumber, status: glText(rows[0]["status"]), body: body, bodyOK: bodyOK}, nil
}

func bpLatestDraftID(ctx context.Context, conn *sql.Conn, chapterNumber int64) (int64, bool, error) {
	rows, err := rrMaps(ctx, conn, "SELECT id FROM drafts WHERE chapter_number=? ORDER BY version DESC,id DESC LIMIT 1", chapterNumber)
	if err != nil || len(rows) == 0 {
		return 0, false, err
	}
	id, ok := rrInteger(rows[0]["id"], -9007199254740991)
	return id, ok, nil
}

// bpReceiptedStatus is isReceiptedDraftStatus (src/shared/draft-status.ts:31-33).
func bpReceiptedStatus(status string) bool {
	return status == "finalized" || status == "superseded"
}

type bpOutboxRow struct {
	finalizationID    string
	contentHash       string
	contentSnapshot   string
	chapterNumber     int64
	publicationStatus string
}

func bpOutbox(ctx context.Context, conn *sql.Conn, draftID int64) (*bpOutboxRow, error) {
	rows, err := rrMaps(ctx, conn, `SELECT finalization_id,content_hash,content_snapshot,chapter_number,publication_status
 FROM finalization_outbox WHERE draft_id=?`, draftID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	chapterNumber, ok := rrInteger(rows[0]["chapter_number"], -9007199254740991)
	if !ok {
		return nil, glError("GENERATION_BATCH_FINALIZATION_CONFLICT")
	}
	return &bpOutboxRow{
		finalizationID:    glText(rows[0]["finalization_id"]),
		contentHash:       glText(rows[0]["content_hash"]),
		contentSnapshot:   glText(rows[0]["content_snapshot"]),
		chapterNumber:     chapterNumber,
		publicationStatus: glText(rows[0]["publication_status"]),
	}, nil
}

// bpPostProcessComplete is the postProcessComplete expression of :128-133: the
// newest chapter_finalize run for the outbox row, all critical steps passed, the
// two named steps present and passing, and no declared step failing.
func bpPostProcessComplete(ctx context.Context, conn *sql.Conn, finalizationID string) (bool, error) {
	rows, err := rrMaps(ctx, conn, `SELECT id,all_critical_passed FROM post_process_runs
 WHERE trigger_source_type='chapter_finalize' AND trigger_source_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1`,
		"finalization:"+finalizationID)
	if err != nil || len(rows) == 0 {
		return false, err
	}
	if !rrEqual(rows[0]["all_critical_passed"], int64(1)) {
		return false, nil
	}
	runID, ok := rows[0]["id"].(string)
	if !ok || runID == "" {
		return false, nil
	}
	steps, err := rrMaps(ctx, conn, "SELECT step_key,critical,ok FROM post_process_steps WHERE run_id=?", runID)
	if err != nil {
		return false, err
	}
	passed := 0
	for _, key := range []string{"kb_import", "chapter_notes"} {
		for _, step := range steps {
			if glText(step["step_key"]) == key && rrEqual(step["critical"], int64(1)) && rrEqual(step["ok"], int64(1)) {
				passed++
				break
			}
		}
	}
	for _, step := range steps {
		if !rrEqual(step["ok"], int64(1)) {
			return false, nil
		}
	}
	return passed == 2, nil
}

// bpIntent is assertGenerationBatchIntent (generation-draft-effects.ts:22-29).
func bpIntent(value any) (string, int64, int64, int64, error) {
	intent := glMap(value)
	if intent == nil {
		return "", 0, 0, 0, glError("GENERATION_BATCH_INTENT_INVALID")
	}
	mode := glText(intent["mode"])
	if mode != "draft_review" && mode != "auto_finalize" {
		return "", 0, 0, 0, glError("GENERATION_BATCH_INTENT_INVALID")
	}
	chapterRange := glMap(intent["range"])
	if chapterRange == nil {
		return "", 0, 0, 0, glError("GENERATION_BATCH_INTENT_INVALID")
	}
	start, startOK := rrInteger(chapterRange["startChapter"], 1)
	end, endOK := rrInteger(chapterRange["endChapter"], -9007199254740991)
	target, targetOK := rrInteger(intent["targetUnits"], 1)
	if !startOK || !endOK || !targetOK || end < start || end-start >= bpMaxBatchChapters || target > bpMaxBatchTargetUnits {
		return "", 0, 0, 0, glError("GENERATION_BATCH_INTENT_INVALID")
	}
	return mode, start, end, target, nil
}

// bpAuthorInputs reads a manifest's frozen author inputs, dropping entries whose
// id or text is not a string, like the original's JSON.parse-then-spread would
// have carried them.
func bpAuthorInputs(value any) []BatchAuthorInput {
	values, ok := plotArray(glPlain(value))
	if !ok {
		return []BatchAuthorInput{}
	}
	inputs := make([]BatchAuthorInput, 0, len(values))
	for _, entry := range values {
		object := glMap(entry)
		if object == nil {
			continue
		}
		id, idOK := object["id"].(string)
		text, textOK := object["text"].(string)
		if !idOK || !textOK {
			continue
		}
		inputs = append(inputs, BatchAuthorInput{ID: id, Text: text})
	}
	return inputs
}

// --- directory progress -----------------------------------------------------

type bpRecoveryResult struct {
	kind       string
	progress   []DirectoryGenerationProgress
	authorEdit *BlueprintAuthorEditReceipt
	diagnostic BlueprintRecoveryDiagnostic
}

func bpDiagnostic(runID, kind, code string) bpRecoveryResult {
	return bpRecoveryResult{kind: kind, diagnostic: BlueprintRecoveryDiagnostic{RunID: runID, Kind: kind, Code: code}}
}

// bpRecovery is readBlueprintRecovery (generation-run-repository.ts:605-... ,
// reached from listDirectoryProgress :599-618). Every refusal the original
// expresses as a thrown code becomes a diagnostic here, because the catalog
// reports per-run diagnostics instead of failing the whole list.
func bpRecovery(ctx context.Context, conn *sql.Conn, runID string, visiting map[string]bool) (bpRecoveryResult, error) {
	if visiting[runID] {
		return bpDiagnostic(runID, "invalid", "GENERATION_DIRECTORY_PROGRESS_INVALID"), nil
	}
	visiting[runID] = true
	defer delete(visiting, runID)
	run, err := bpLoadRun(ctx, conn, runID)
	if err != nil {
		// GENERATION_RUN_MISSING and an unparsable binding are both the
		// original's invalid-diagnostic branch (:581-583).
		return bpDiagnostic(runID, "invalid", "GENERATION_DIRECTORY_PROGRESS_INVALID"), nil
	}
	if marker, present := run.binding["$blueprintRetirement"]; present && marker != nil {
		if glText(run.manifest()["operation"]) != bpDirectoryOperation || glText(marker) != "blueprints-cleared" {
			return bpDiagnostic(runID, "invalid", "GENERATION_DIRECTORY_PROGRESS_INVALID"), nil
		}
		return bpDiagnostic(runID, "retired", "GENERATION_BLUEPRINTS_CLEARED"), nil
	}
	rows, err := rrMaps(ctx, conn, `SELECT run_id,usage_receipt_json FROM generation_attempts
 WHERE run_id=? AND json_type(usage_receipt_json,'$.directoryProgress') IS NOT NULL ORDER BY rowid`, runID)
	if err != nil {
		return bpRecoveryResult{}, err
	}
	progress := make([]DirectoryGenerationProgress, 0, len(rows))
	for _, row := range rows {
		proof, err := bpDirectoryProof(ctx, conn, run, row["usage_receipt_json"])
		if err != nil {
			return bpDiagnostic(runID, "invalid", "GENERATION_DIRECTORY_PROGRESS_INVALID"), nil
		}
		progress = append(progress, proof)
	}
	if code := bpAuthorEditCode(ctx, conn, run); code != "" {
		return bpDiagnostic(runID, "invalid", code), nil
	}
	// readBlueprintRecovery itself returns the proved receipt as authorEdit
	// (:564). The validation above proves it; this read exposes the value the
	// recovery context projects as blueprintRecovery.saved.
	authorEdit := bpBlueprintAuthorEdit(run)
	parents, err := rrMaps(ctx, conn, `SELECT DISTINCT t.run_id FROM generation_attempts t
 JOIN generation_runs r ON r.run_id=t.run_id WHERE r.root_action_id=?
 AND json_extract(t.usage_receipt_json,'$.directoryProgress.continuationHandle.runId')=?`, run.rootID, runID)
	if err != nil {
		return bpRecoveryResult{}, err
	}
	if len(parents) > 1 {
		return bpDiagnostic(runID, "invalid", "GENERATION_DIRECTORY_PROGRESS_INVALID"), nil
	}
	for _, parent := range parents {
		recovery, err := bpRecovery(ctx, conn, glText(parent["run_id"]), visiting)
		if err != nil {
			return bpRecoveryResult{}, err
		}
		if recovery.kind != "available" {
			return bpDiagnostic(runID, recovery.diagnostic.Kind, recovery.diagnostic.Code), nil
		}
		matching, rootOK := 0, false
		for _, item := range recovery.progress {
			if item.ContinuationHandle != nil && item.ContinuationHandle.RunID == runID {
				matching++
				if item.SourceHandle.RootActionID == run.rootID {
					rootOK = true
				}
			}
		}
		if matching != 1 || !rootOK {
			return bpDiagnostic(runID, "invalid", "GENERATION_DIRECTORY_PROGRESS_INVALID"), nil
		}
	}
	return bpRecoveryResult{kind: "available", progress: progress, authorEdit: authorEdit}, nil
}

// bpDirectoryProof is readDirectoryProgressProof (generation-run-repository.ts:
// 514-547): the receipt must agree with the run it hangs off, with the
// blueprint commit operation it names, and with the child run its continuation
// handle names.
func bpDirectoryProof(ctx context.Context, conn *sql.Conn, run *bpRun, usageValue any) (DirectoryGenerationProgress, error) {
	var zero DirectoryGenerationProgress
	usage, err := glStored(usageValue)
	if err != nil {
		return zero, glError("GENERATION_DIRECTORY_PROGRESS_INVALID")
	}
	stored := glMap(usage["directoryProgress"])
	refuse := glError("GENERATION_DIRECTORY_PROGRESS_INVALID")
	if stored == nil {
		return zero, refuse
	}
	operationID, operationOK := stored["operationId"].(string)
	payloadHash, payloadOK := stored["payloadHash"].(string)
	sourceHandle, handleOK := bpCompleteHandle(stored["sourceHandle"])
	identity := glMap(usage["artifactIdentity"])
	requested, requestedOK := bpRangeObject(stored["requestedRange"])
	committed, committedOK := bpRangeObject(stored["committedRange"])
	if !operationOK || !payloadOK || !bpContentHash.MatchString(payloadHash) || !handleOK ||
		!requestedOK || !committedOK ||
		requested.StartChapter < 1 || requested.EndChapter < 1 ||
		committed.StartChapter < 1 || committed.EndChapter < 1 ||
		sourceHandle.RunID != run.runID || glText(identity["epoch"]) != sourceHandle.Epoch ||
		requested.EndChapter < requested.StartChapter ||
		requested.EndChapter-requested.StartChapter >= bpDirectoryRangeLimit ||
		committed.EndChapter < committed.StartChapter {
		return zero, refuse
	}
	// The requested range must be a subset of the chapter numbers the run froze.
	selected, selectedOK := plotArray(glPlain(run.manifest()["selectedBlueprintChapterNumbers"]))
	if !selectedOK {
		return zero, refuse
	}
	for chapter := requested.StartChapter; chapter <= requested.EndChapter; chapter++ {
		found := false
		for _, value := range selected {
			if number, ok := plotNumber(value); ok && int64(number) == chapter && number == float64(chapter) {
				found = true
				break
			}
		}
		if !found {
			return zero, refuse
		}
	}
	committedRows, err := rrMaps(ctx, conn, `SELECT payload_hash,start_chapter,end_chapter FROM blueprint_commit_operations
 WHERE operation_id=?`, operationID)
	if err != nil {
		return zero, err
	}
	if len(committedRows) == 0 {
		return zero, refuse
	}
	row := committedRows[0]
	rowPayload, rowPayloadOK := row["payload_hash"].(string)
	rowStart, rowStartOK := rrInteger(row["start_chapter"], -9007199254740991)
	rowEnd, rowEndOK := rrInteger(row["end_chapter"], -9007199254740991)
	remaining, remainingPresent := stored["remainingRange"]
	if !rowPayloadOK || !rowStartOK || !rowEndOK || rowPayload != payloadHash ||
		!remainingPresent ||
		sourceHandle.RootActionID != run.rootID || sourceHandle.ProjectID != glText(run.binding["projectId"]) ||
		rowStart != committed.StartChapter || rowEnd != committed.EndChapter ||
		requested.StartChapter != rowStart || requested.EndChapter < rowEnd ||
		!bpSameRemaining(remaining, committed.EndChapter < requested.EndChapter, requested, committed.EndChapter) {
		return zero, refuse
	}
	proof := DirectoryGenerationProgress{
		OperationID:    operationID,
		PayloadHash:    payloadHash,
		SourceHandle:   sourceHandle,
		RequestedRange: requested,
		CommittedRange: committed,
		RemainingRange: bpRemainingRange(committed.EndChapter < requested.EndChapter, requested, committed.EndChapter),
	}
	if phTruthy(stored["continuationHandle"]) {
		continuation, ok := bpCompleteHandle(stored["continuationHandle"])
		if !ok || continuation.RunID == "" {
			return zero, refuse
		}
		child, err := bpLoadRun(ctx, conn, continuation.RunID)
		if err != nil {
			return zero, refuse
		}
		if child.rootID != run.rootID || glText(child.binding["projectId"]) != glText(run.binding["projectId"]) ||
			continuation.RootActionID != child.rootID ||
			glText(child.binding["projectId"]) != continuation.ProjectID || child.runID == run.runID {
			return zero, refuse
		}
		continuation.Epoch = glText(child.binding["epoch"])
		proof.ContinuationHandle = &continuation
	}
	// The receipt itself never carries authorInputs; the owner projects the
	// source run's frozen array over it (main-generation-owner.ts:1550-1553), so
	// the value travels only when this run froze one.
	if value, present := run.manifest()["authorInputs"]; present && value != nil {
		proof.AuthorInputs = bpAuthorInputs(value)
	}
	return proof, nil
}

// bpRangeObject requires both ends to be safe integers, mirroring
// Number.isSafeInteger on each field.

// bpFromToRange reads a {from,to} range object, the shape the reference writes
// for a blueprint author-edit receipt's requestedRange/committedRange
// (main-generation-owner.ts:895), unlike the {startChapter,endChapter} shape of
// the directory commit receipt.
func bpFromToRange(value any) (BatchChapterRange, bool) {
	object := glMap(value)
	if object == nil || len(object) != 2 {
		return BatchChapterRange{}, false
	}
	start, startOK := rrInteger(object["from"], -9007199254740991)
	end, endOK := rrInteger(object["to"], -9007199254740991)
	if !startOK || !endOK {
		return BatchChapterRange{}, false
	}
	return BatchChapterRange{StartChapter: start, EndChapter: end}, true
}

func bpRangeObject(value any) (BatchChapterRange, bool) {
	object := glMap(value)
	if object == nil {
		return BatchChapterRange{}, false
	}
	start, startOK := rrInteger(object["startChapter"], -9007199254740991)
	end, endOK := rrInteger(object["endChapter"], -9007199254740991)
	if !startOK || !endOK {
		return BatchChapterRange{}, false
	}
	return BatchChapterRange{StartChapter: start, EndChapter: end}, true
}

// bpSameRemaining is isDeepStrictEqual on a directory receipt's remainingRange:
// an explicit null is only equal to "nothing remains", and an object must carry
// exactly the two derived ends.
func bpSameRemaining(value any, hasRemaining bool, requested BatchChapterRange, committedEnd int64) bool {
	if !hasRemaining {
		return value == nil
	}
	want := BatchChapterRange{StartChapter: committedEnd + 1, EndChapter: requested.EndChapter}
	object := glMap(value)
	if object == nil || len(object) != 2 {
		return false
	}
	got, ok := bpRangeObject(object)
	return ok && got == want
}

func bpRemainingRange(hasRemaining bool, requested BatchChapterRange, committedEnd int64) *BatchChapterRange {
	if !hasRemaining {
		return nil
	}
	return &BatchChapterRange{StartChapter: committedEnd + 1, EndChapter: requested.EndChapter}
}

// bpCompleteHandle is completeHandle (generation-run-repository.ts:96-98): four
// string keys, each non-blank after trimming.
func bpCompleteHandle(value any) (BatchRunHandle, bool) {
	object := glMap(value)
	if object == nil || len(object) != 4 {
		return BatchRunHandle{}, false
	}
	handle := BatchRunHandle{
		ProjectID:    glText(object["projectId"]),
		Epoch:        glText(object["epoch"]),
		RootActionID: glText(object["rootActionId"]),
		RunID:        glText(object["runId"]),
	}
	for _, field := range []string{handle.ProjectID, handle.Epoch, handle.RootActionID, handle.RunID} {
		if strings.TrimSpace(field) == "" {
			return BatchRunHandle{}, false
		}
	}
	return handle, true
}

// bpAuthorEditCode validates the frozen blueprint author-edit effect of one run
// (generation-run-repository.ts:194-214) and returns "" when there is none or it
// is intact. The receipt itself is not part of the catalog; it is validated
// because the original turns a lying receipt into a diagnostic.
func bpAuthorEditCode(ctx context.Context, conn *sql.Conn, run *bpRun) string {
	const code = "GENERATION_BLUEPRINT_AUTHOR_RECEIPT_INVALID"
	effects := glMap(run.binding["$runEffects"])
	if effects == nil {
		return ""
	}
	value, present := effects["blueprintAuthorEdit"]
	if !present || value == nil {
		return ""
	}
	receipt := glMap(value)
	if receipt == nil || glText(receipt["kind"]) != "blueprint-author-edit" {
		return code
	}
	manifest := run.manifest()
	if glText(manifest["operation"]) != bpDirectoryOperation {
		return code
	}
	rangeText := ""
	for _, input := range bpAuthorInputs(manifest["authorInputs"]) {
		if input.ID == "directory:requested-range" {
			// The original's Array.find takes the FIRST matching input.
			rangeText = input.Text
			break
		}
	}
	if rangeText == "" {
		return code
	}
	rangeValue, err := glParse(rangeText)
	if err != nil {
		// The original parses this JSON straight into the caller's frame, so a
		// malformed freeze is its SyntaxError branch, not a receipt refusal.
		return "GENERATION_DIRECTORY_PROGRESS_INVALID"
	}
	requested, ok := bpRangeObject(rangeValue)
	if !ok {
		return code
	}
	sourceHandle, handleOK := bpCompleteHandle(receipt["sourceHandle"])
	payloadHash, payloadOK := receipt["payloadHash"].(string)
	committed, committedOK := bpFromToRange(receipt["committedRange"])
	requestedObject := glMap(receipt["requestedRange"])
	if !handleOK || sourceHandle.RunID != run.runID || sourceHandle.RootActionID != run.rootID ||
		sourceHandle.ProjectID != glText(run.binding["projectId"]) ||
		!payloadOK || !bpContentHash.MatchString(payloadHash) || !committedOK ||
		requestedObject == nil || len(requestedObject) != 2 ||
		requested.StartChapter != bpFirstInt(requestedObject, "from", -1) ||
		requested.EndChapter != bpFirstInt(requestedObject, "to", -1) ||
		committed.StartChapter < requested.StartChapter ||
		committed.EndChapter < committed.StartChapter ||
		committed.EndChapter > requested.EndChapter {
		return code
	}
	remaining, remainingOK := receipt["remainingRange"]
	if !remainingOK || !bpSameAuthorRemaining(remaining, committed.EndChapter < requested.EndChapter, movedRange(requested, committed.EndChapter)) {
		return code
	}
	for key := range receipt {
		switch key {
		case "kind", "operationId", "sourceHandle", "requestedRange", "committedRange", "remainingRange", "payloadHash":
		default:
			return code
		}
	}
	operationID, operationOK := receipt["operationId"].(string)
	if !operationOK || operationID == "" {
		return code
	}
	rows, err := rrMaps(ctx, conn, `SELECT payload_hash,start_chapter,end_chapter FROM blueprint_commit_operations
 WHERE operation_id=?`, operationID)
	if err != nil || len(rows) == 0 {
		return code
	}
	rowPayload, rowPayloadOK := rows[0]["payload_hash"].(string)
	rowStart, rowStartOK := rrInteger(rows[0]["start_chapter"], -9007199254740991)
	rowEnd, rowEndOK := rrInteger(rows[0]["end_chapter"], -9007199254740991)
	if !rowPayloadOK || !rowStartOK || !rowEndOK || rowPayload != payloadHash ||
		rowStart != committed.StartChapter || rowEnd != committed.EndChapter {
		return code
	}
	return ""
}

// bpBlueprintAuthorEdit returns the receipt bpAuthorEditCode just proved. It is
// the value half of readBlueprintAuthorEdit (generation-run-repository.ts:194-214),
// which readBlueprintRecovery returns as its authorEdit (:564) and readContext
// projects as blueprintRecovery.saved (main-generation-owner.ts:932, :941).
// Callers must run bpAuthorEditCode first; this function only decodes what that
// proof accepted.
func bpBlueprintAuthorEdit(run *bpRun) *BlueprintAuthorEditReceipt {
	effects := glMap(run.binding["$runEffects"])
	if effects == nil {
		return nil
	}
	value, present := effects["blueprintAuthorEdit"]
	if !present || value == nil {
		return nil
	}
	receipt := glMap(value)
	if receipt == nil {
		return nil
	}
	sourceHandle, _ := bpCompleteHandle(receipt["sourceHandle"])
	requested, _ := bpFromToRange(receipt["requestedRange"])
	committed, _ := bpFromToRange(receipt["committedRange"])
	edit := &BlueprintAuthorEditReceipt{
		Kind:           "blueprint-author-edit",
		OperationID:    glText(receipt["operationId"]),
		SourceHandle:   sourceHandle,
		RequestedRange: BlueprintRange{From: requested.StartChapter, To: requested.EndChapter},
		CommittedRange: BlueprintRange{From: committed.StartChapter, To: committed.EndChapter},
		PayloadHash:    glText(receipt["payloadHash"]),
	}
	if remaining := glMap(receipt["remainingRange"]); remaining != nil {
		edit.RemainingRange = &BlueprintRange{
			From: bpFirstInt(remaining, "from", -1),
			To:   bpFirstInt(remaining, "to", -1),
		}
	}
	return edit
}

// bpFirstInt reads one integer field of a {from,to} range object without
// coercing it.
func bpFirstInt(value any, key string, missing int64) int64 {
	object := glMap(value)
	if object == nil {
		return missing
	}
	number, ok := rrInteger(object[key], -9007199254740991)
	if !ok {
		return missing
	}
	return number
}

// movedRange shapes the author-edit receipt's {from,to} vocabulary into the
// chapter range the receipt must promise.
func movedRange(requested BatchChapterRange, committedEnd int64) BatchChapterRange {
	return BatchChapterRange{StartChapter: committedEnd + 1, EndChapter: requested.EndChapter}
}

// bpSameAuthorRemaining is isDeepStrictEqual on the author-edit receipt's
// remainingRange, whose keys are from/to rather than startChapter/endChapter.
func bpSameAuthorRemaining(value any, hasRemaining bool, want BatchChapterRange) bool {
	if !hasRemaining {
		return value == nil
	}
	object := glMap(value)
	if object == nil || len(object) != 2 {
		return false
	}
	return bpFirstInt(object, "from", -1) == want.StartChapter && bpFirstInt(object, "to", -1) == want.EndChapter
}
