package store

// The author half of ImportRunRepository.prepare (electron/repositories/
// import-run-repository.ts:2005-2138): the store method importrun_lifecycle.go
// left out on purpose (its header, :26-34), and the one
// db:import-run-prepare-inspection needs before the name can be served.
//
// # What the reference does, step by step
//
//	:2006-2008  runId trimmed, bounded at 160 UTF-16 units, sourceFingerprint a digest
//	:2012       locale is one of the two the renderer speaks
//	:2014-2018  display, source ids and source fingerprints normalized by the same
//	            helpers beginParsing uses (:778-819)
//	:2019       chapters normalized by the same normalizeChapters (:821-861)
//	:2021-2024  an author prepare without both fingerprints is
//	            IMPORT_RUN_AUTHOR_PREVIEW_REQUIRED
//	:2025-2028  FinalizedDraftImportRepository.preview over the chapters; a manifest
//	            that is not the one the caller inspected is
//	            IMPORT_RUN_AUTHOR_MANIFEST_MISMATCH
//	:2029-2034  matchingResumableRun, then an authority that moved is
//	            AuthorImportPreviewStaleError (AUTHOR_IMPORT_PREVIEW_STALE)
//	:2035-2047  a resumable run answers itself when it already owns a committed
//	            author-finalized-batch receipt or already froze this authority;
//	            otherwise fenceUncommittedAuthorRun retires it
//	:2048-2058  a conflict is the reference's locale guidance, an exact duplicate
//	            writes nothing
//	:2059-2061  a taken run id is IMPORT_RUN_ID_CONFLICT
//	:2062-2104  one INSERT with stage 'author-commit' and status 'ready', the
//	            authorInference ledger row, and one import_run_chapters row per new
//	            chapter
//	:2105-2138  read the row back and answer rowToSnapshot
//
// The preview is the finalized-draft lane's own function, not a re-derivation:
// finalizedImportPreviewBody is what Drafts.FinalizedImportPreview calls, so this
// prepare and db:import-run-author-preview cannot disagree about a classification,
// a gap or either fingerprint. It runs INSIDE the write's BEGIN IMMEDIATE, so the
// authority a run freezes is the authority the same transaction observed.
//
// # The three refusals that are prose in the reference and codes here
//
// The reference throws a localized sentence for a conflict (:2050), for a previous
// import that is still running (:1380-1384) and for a fence that changed under it
// (:1391-1396). This lane publishes symbolic codes, so the three sentences become
// IMPORT_RUN_AUTHOR_CONFLICT, IMPORT_RUN_AUTHOR_PREVIOUS_RUNNING and
// IMPORT_RUN_AUTHOR_FENCE_CHANGED, and the same code is what the fenced row keeps
// in last_error. Nothing else is invented: the cause of a conflict is already on
// the wire before this call - the renderer previews first and the preview carries
// classification, firstGapChapterNumber and conflictChapterNumbers - and the two
// fence states are the caller's own run, not hidden data.
//
// # What this file deliberately does not port
//
// The reference's legacy bridge (legacySourceFingerprints,
// legacyCollectionFingerprint) exists so import_run_sources can record an old
// stable identity. An author run writes no import_run_sources row at all - the
// reference's own author branch does not either - so the two fields have no column
// to reach here. They are resolved by the caller for the reference half, which
// this file does not implement (BeginParsing refuses any purpose but reference).

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
)

// ErrImportRunAuthorPreviewStale is AuthorImportPreviewStaleError
// (electron/controllers/db-controller.ts:280-283): the authority moved between the
// preview the caller saw and this prepare. It is NOT a refusal code - the
// controller answers `{success:false, errorCode: AUTHOR_IMPORT_PREVIEW_STALE}` with
// HTTP 200 - so the caller distinguishes it with errors.Is instead of by text.
var ErrImportRunAuthorPreviewStale = errors.New("AUTHOR_IMPORT_PREVIEW_STALE")

// ImportRunPrepareAuthorRequest is the author-manuscript arm of
// ImportRunPrepareFromInspectionRequest (src/shared/import-run.ts:296-304) plus the
// source facts the controller resolves before it calls prepare
// (electron/controllers/db-controller.ts:284-292): the resolved source ids,
// fingerprints, display records and the frozen chapters. The fields are the ones
// the reference's prepare reads for this purpose, with the two the author branch
// never touches left out (see the header).
type ImportRunPrepareAuthorRequest struct {
	RunID                       string
	SourceFingerprint           string
	SourceIDs                   []string
	SourceFingerprints          []string
	SourceDisplay               []ImportRunSourceDisplay
	Locale                      string
	AuthorityFingerprint        string
	ExpectedManifestFingerprint string
	AuthorInference             bool
	Chapters                    []ImportRunChapterInput
}

// PrepareAuthor is db:import-run-prepare-inspection's author half. It answers the
// reference's ImportRunPreparationResult as a map (src/shared/import-run.ts:358-367):
// classification plus the run snapshot, the new, conflicting and duplicate chapter
// numbers. `run` is omitted when the reference's own answer has no run, because
// JSON.stringify drops an undefined property and a Go nil would be written as null.
//
// Every write is inside one BEGIN IMMEDIATE, and every read that decides a write
// runs on the same reserved connection, so a resumed run is never adopted after a
// committed receipt appeared behind it.
func (r *ImportRuns) PrepareAuthor(ctx context.Context, candidate ImportRunPrepareAuthorRequest) (map[string]any, error) {
	runID := strings.TrimSpace(candidate.RunID)
	if runID == "" || importRunUTF16Length(runID) > 160 || !importRunSHA256.MatchString(candidate.SourceFingerprint) {
		// :2006-2008
		return nil, errors.New("IMPORT_RUN_ID_INVALID")
	}
	if candidate.Locale != "zh-CN" && candidate.Locale != "en-US" {
		// :2012
		return nil, errors.New("IMPORT_RUN_LOCALE_INVALID")
	}
	display, err := importRunNormalizeDisplay(candidate.SourceDisplay)
	if err != nil {
		// :2014
		return nil, err
	}
	sourceIDs, err := importRunNormalizeSourceIDs(candidate.SourceIDs, display, candidate.SourceFingerprint)
	if err != nil {
		// :2015
		return nil, err
	}
	if _, err := importRunNormalizeSourceFingerprints(candidate.SourceFingerprints, sourceIDs); err != nil {
		// :2016 - validated for both purposes by the reference, then used only by
		// the parsing lane, which is why the result is deliberately dropped.
		return nil, err
	}
	normalized, err := importRunNormalizeChapters(candidate.Chapters, sourceIDs)
	if err != nil {
		// :2019
		return nil, err
	}
	if !importRunSHA256.MatchString(candidate.AuthorityFingerprint) ||
		!importRunSHA256.MatchString(candidate.ExpectedManifestFingerprint) {
		// :2021-2024
		return nil, errors.New("IMPORT_RUN_AUTHOR_PREVIEW_REQUIRED")
	}
	// :2025-2028 - the preview normalizes too, and its own guard (a duplicate
	// chapter number, a blank body, a count that is not the body's own) is
	// FINALIZED_IMPORT_CHAPTERS_INVALID for this purpose as well.
	items := make([]FinalizedDraftImportChapter, 0, len(normalized))
	for _, chapter := range normalized {
		count := int64(draftalg.CountDraftUnits(chapter.content))
		items = append(items, FinalizedDraftImportChapter{ChapterNumber: chapter.number,
			Title: chapter.title, Content: chapter.content, WordCount: &count})
	}
	previewChapters, err := finalizedImportNormalizeChapters(items)
	if err != nil {
		return nil, err
	}
	displayJSON, err := jsonMarshalCompact(display)
	if err != nil {
		return nil, err
	}
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response map[string]any
	err = r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		preview, err := finalizedImportPreviewBody(ctx, conn, previewChapters)
		if err != nil {
			return err
		}
		if preview.ManifestFingerprint != candidate.ExpectedManifestFingerprint {
			// :2026-2028
			return errors.New("IMPORT_RUN_AUTHOR_MANIFEST_MISMATCH")
		}
		resumable, found, err := importRunMatchingResumableRun(ctx, conn, "author-manuscript",
			candidate.SourceFingerprint, preview.ManifestFingerprint)
		if err != nil {
			return err
		}
		if preview.AuthorityFingerprint != candidate.AuthorityFingerprint {
			// :2030-2032
			return ErrImportRunAuthorPreviewStale
		}
		if found {
			committed, err := importRunAuthorFinalizationCommitted(ctx, conn, resumable)
			if err != nil {
				return err
			}
			if committed || resumable.AuthorityFingerprint == preview.AuthorityFingerprint {
				// :2035-2045
				snapshot, err := importRunSnapshot(ctx, conn, resumable)
				if err != nil {
					return err
				}
				response = importRunAuthorPreparation("resumable", snapshot,
					[]int64{}, importRunAuthorChapterNumbers(previewChapters), []int64{})
				return nil
			}
			if err := importRunFenceAuthorRun(ctx, conn, resumable); err != nil {
				// :2046-2047
				return err
			}
		}
		switch preview.Classification {
		case "conflict":
			// :2048-2050 - see the header for the prose this code replaces.
			return errors.New("IMPORT_RUN_AUTHOR_CONFLICT")
		case "exact-duplicate":
			// :2051-2058 - nothing is written and the answer carries no run.
			response = importRunAuthorPreparation("exact-duplicate", nil,
				[]int64{}, []int64{}, preview.DuplicateChapterNumbers)
			return nil
		}
		if _, present, err := readImportRunRow(ctx, conn, runID); err != nil {
			return err
		} else if present {
			// :2059-2061
			return errors.New("IMPORT_RUN_ID_CONFLICT")
		}
		newNumbers := make(map[int64]bool, len(preview.NewChapterNumbers))
		for _, number := range preview.NewChapterNumbers {
			newNumbers[number] = true
		}
		chaptersToPersist := make([]importRunNormalizedChapter, 0, len(newNumbers))
		for _, chapter := range normalized {
			if newNumbers[chapter.number] {
				chaptersToPersist = append(chaptersToPersist, chapter)
			}
		}
		sort.SliceStable(chaptersToPersist, func(left, right int) bool {
			return chaptersToPersist[left].number < chaptersToPersist[right].number
		})
		totalContentSize := int64(0)
		for _, chapter := range chaptersToPersist {
			totalContentSize += chapter.contentSize
		}
		manifestContentSize := int64(0)
		manifestWordCount := int64(0)
		for _, chapter := range normalized {
			manifestContentSize += chapter.contentSize
			manifestWordCount += int64(draftalg.CountDraftUnits(chapter.content))
		}
		// :2062-2085 - the run row. The stage is the author lane's first stage and
		// the status is ready: this method creates the run, it does not start it.
		if _, err := conn.ExecContext(ctx, `INSERT INTO import_runs (
 id, purpose, root_run_id, effect_namespace, source_fingerprint, manifest_fingerprint,
 authority_fingerprint, source_display_json, locale, stage, status,
 total_chapters, total_content_size, completed_chapters, base_run_id,
 manifest_chapter_count, manifest_content_size, manifest_word_count
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'author-commit', 'ready', ?, ?, 0, NULL, ?, ?, ?)`,
			runID, "author-manuscript", runID, importRunEffectNamespace("author-manuscript", runID),
			candidate.SourceFingerprint, preview.ManifestFingerprint, candidate.AuthorityFingerprint,
			displayJSON, candidate.Locale, int64(len(chaptersToPersist)), totalContentSize,
			int64(len(normalized)), manifestContentSize, manifestWordCount); err != nil {
			return err
		}
		if candidate.AuthorInference {
			// :2086-2094 - evidence_kind is the direct-checkpoint value the M04
			// vocabulary allows (internal/store/importrun.go:59-64).
			if _, err := conn.ExecContext(ctx, `INSERT INTO import_effect_ledger(
 run_id, stage, batch_id, effect_namespace, effect_key, state, evidence_kind, diagnostic_code
) VALUES (?, ?, ?, ?, ?, 'complete', 'legacy-checkpoint', '')`, runID,
				importRunAuthorInferenceStage, importRunAuthorInferenceBatchID,
				importRunEffectNamespace("author-manuscript", runID), importRunAuthorInferenceKey); err != nil {
				return err
			}
		}
		// :2095-2104
		for _, chapter := range chaptersToPersist {
			if _, err := conn.ExecContext(ctx, `INSERT INTO import_run_chapters (
 run_id, chapter_number, source_id, source_chapter_number, title,
 content_fingerprint, content_size, content_snapshot
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, runID, chapter.number, chapter.sourceID,
				chapter.sourceChapterNumber, chapter.title, chapter.contentFingerprint,
				chapter.contentSize, chapter.content); err != nil {
				return err
			}
		}
		created, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			// :2106-2107
			return errors.New("IMPORT_RUN_CREATE_FAILED")
		}
		snapshot, err := importRunSnapshot(ctx, conn, created)
		if err != nil {
			return err
		}
		response = importRunAuthorPreparation("new", snapshot, preview.NewChapterNumbers,
			[]int64{}, preview.DuplicateChapterNumbers)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// importRunAuthorPreparation is one ImportRunPreparationResult
// (src/shared/import-run.ts:358-367). run is omitted when the reference's own
// answer has no run, because JSON.stringify drops an undefined property.
func importRunAuthorPreparation(classification string, run map[string]any,
	newChapterNumbers, conflictChapterNumbers, duplicateChapterNumbers []int64) map[string]any {
	preparation := map[string]any{
		"classification":          classification,
		"newChapterNumbers":       newChapterNumbers,
		"conflictChapterNumbers":  conflictChapterNumbers,
		"duplicateChapterNumbers": duplicateChapterNumbers,
	}
	if run != nil {
		preparation["run"] = run
	}
	return preparation
}

// importRunAuthorChapterNumbers is authorChapters.map(chapter => chapter.chapterNumber)
// (:2042): the chapters the caller already holds, in the preview's own order.
func importRunAuthorChapterNumbers(chapters []finalizedImportChapter) []int64 {
	numbers := make([]int64, 0, len(chapters))
	for _, chapter := range chapters {
		numbers = append(numbers, chapter.chapterNumber)
	}
	return numbers
}

// importRunAuthorFinalizationCommitted is hasCommittedAuthorFinalizationReceipt
// (:1345-1354): the run owns a committed author-finalized-batch receipt, and that
// receipt still describes itself.
//
// The reference selects by (run_id, kind, state). The store writes exactly one row
// per (run, stage, batch) and derives that pair from the kind
// (importrun_receipts.go:474-490), so the author-finalized-batch row of a run is
// (author-commit, done): reading that key is the same row, without a query that
// could pick a second copy. rowToEffectReceipt's try/catch is
// importRunReceiptValidate, and a receipt that no longer validates is the corrupt
// receipt it says it is.
func importRunAuthorFinalizationCommitted(ctx context.Context, q importRunQueryer, run importRunRow) (bool, error) {
	row, present, err := readImportRunReceiptRow(ctx, q, run.ID, "author-commit", "done")
	if err != nil || !present {
		return false, err
	}
	if row.Kind != "author-finalized-batch" || row.State != "committed" {
		return false, nil
	}
	if _, err := importRunReceiptValidate(row, run); err != nil {
		return false, err
	}
	return true, nil
}

// importRunFenceAuthorRun is fenceUncommittedAuthorRun (:1374-1400): a previous
// author run that is genuinely still running refuses, and every other resumable
// state is retired in place so the caller's new preview can create its own run.
//
// The UPDATE is the reference's own predicate, including the NOT EXISTS guard that
// keeps a run whose receipt committed from being retired between the two reads. A
// fence that changes anything but one row is the reference's second sentence and
// is published as IMPORT_RUN_AUTHOR_FENCE_CHANGED; the text it writes into
// last_error is that same code, because this lane stores codes rather than prose.
func importRunFenceAuthorRun(ctx context.Context, q importRunQueryer, run importRunRow) error {
	now := time.Now().UnixMilli()
	if run.Status == "running" && run.ExecutionOwner != "" && run.LeaseExpiresAt > now {
		// :1376-1380
		return errors.New("IMPORT_RUN_AUTHOR_PREVIOUS_RUNNING")
	}
	result, err := q.ExecContext(ctx, `UPDATE import_runs
 SET resumable = 0, cancel_requested = 0, last_error = ?,
  execution_owner = '', execution_epoch = execution_epoch + 1, lease_expires_at = 0,
  updated_at = datetime('now')
 WHERE id = ? AND purpose = 'author-manuscript' AND resumable = 1
  AND status IN ('ready', 'running', 'failed', 'cancelled')
  AND (status != 'running' OR execution_owner = '' OR lease_expires_at <= ?)
  AND NOT EXISTS (
   SELECT 1 FROM import_run_receipts
   WHERE run_id = import_runs.id
    AND kind = 'author-finalized-batch' AND state = 'committed'
  )`, "IMPORT_RUN_AUTHOR_FENCE_CHANGED", run.ID, now)
	if err != nil {
		return err
	}
	changes, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changes != 1 {
		// :1391-1396
		return errors.New("IMPORT_RUN_AUTHOR_FENCE_CHANGED")
	}
	return nil
}
