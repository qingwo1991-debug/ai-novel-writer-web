package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ImportRuns borrows an admitted author database. It never opens, closes or
// migrates one, and it owns no source reader: it persists the durable import
// run's bookkeeping (stage, execution lease, checkpoints) for runs an earlier
// app version already created.
//
// What is deliberately NOT here: run creation (prepare-inspection /
// finalize-parsing / restart), the effect receipts and the four effect
// commits. Those either need the in-memory inspection owner, the generation
// source proof, or the knowledge base, so they stay unbound rather than being
// approximated.
//
// Ported from electron/repositories/import-run-repository.ts. Every refusal
// code is the original's own symbolic string; the original never catches these
// (db-controller.ts:236-547), so they reach the browser as transport errors
// with no "Error: " prefix.
type ImportRuns struct{ db *sql.DB }

// NewImportRuns performs no I/O. The caller must have admitted the project and
// its schema separately.
func NewImportRuns(db *sql.DB) *ImportRuns { return &ImportRuns{db: db} }

// ImportRunExecution is the renderer-held execution lease minus its expiry: the
// original compares only owner and epoch against the row (assertExecutionAuthority),
// so an expired expiresAt on the caller's copy is irrelevant.
type ImportRunExecution struct {
	Owner string
	Epoch int64
}

const (
	// importRunLeaseMS mirrors DEFAULT_EXECUTION_LEASE_MS (15 minutes).
	importRunLeaseMS int64 = 15 * 60 * 1000
	// importRunMaxPageSize mirrors MAX_PAGE_SIZE.
	importRunMaxPageSize int64 = 100
	// importRunMaxChapterBytes mirrors MAX_CHAPTER_BYTES.
	importRunMaxChapterBytes int64 = 16 * 1024 * 1024
	// importRunKnowledgeBatchSize and importRunBlueprintBatchSize mirror
	// IMPORT_RUN_KNOWLEDGE_BATCH_SIZE and IMPORT_RUN_BLUEPRINT_BATCH_SIZE.
	importRunKnowledgeBatchSize  int64 = 10
	importRunBlueprintBatchSize  int64 = 5
	// importRunStageCompleted is the terminal stage; complete() is the only way
	// in, so it is never a next stage.
	importRunStageCompleted = "completed"
	// importRunAuthorInferenceStage/BatchID mark the frozen "also infer
	// settings, structure, characters and blueprints" option. It is an option,
	// never progress: completed batches deliberately skip it.
	importRunAuthorInferenceStage   = "prepared"
	importRunAuthorInferenceBatchID = "author-inference"
	importRunAuthorInferenceKey     = "option:author-inference"
)

var (
	importRunBatchCheckpointID = regexp.MustCompile(`^([1-9][0-9]*)-([1-9][0-9]*)-([a-f0-9]{8}(?:\.[a-f0-9]{8})*)$`)
	importRunAuthorCheckpoint  = regexp.MustCompile(`^chapter:([1-9][0-9]*)$`)
	importRunSHA256            = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// importRunQueryer is the borrow surface shared by *sql.DB, *sql.Tx and
// *sql.Conn, so every read runs in exactly one snapshot and every write in
// exactly one reserved connection.
type importRunQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

type importRunRow struct {
	ID                      string
	Purpose                 string
	RootRunID               string
	EffectNamespace         string
	SourceFingerprint       string
	ManifestFingerprint     string
	AuthorityFingerprint    string
	LegacySourceFingerprint string
	SourceDisplayJSON       string
	Locale                  string
	Stage                   string
	Status                  string
	CompletedBatchesJSON    string
	LastError               string
	Resumable               int64
	CancelRequested         int64
	ExecutionOwner          string
	ExecutionEpoch          int64
	LeaseExpiresAt          int64
	TotalChapters           int64
	TotalContentSize        int64
	ManifestChapterCount    int64
	ManifestContentSize     int64
	ManifestWordCount       int64
	CompletedChapters       int64
	BaseRunID               sql.NullString
	CreatedAt               string
	UpdatedAt               string
	CompletedAt             sql.NullString
}

const importRunRowColumns = `id, purpose, root_run_id, effect_namespace, source_fingerprint,
 manifest_fingerprint, authority_fingerprint, legacy_source_fingerprint, source_display_json,
 locale, stage, status, completed_batches_json, last_error, resumable, cancel_requested,
 execution_owner, execution_epoch, lease_expires_at, total_chapters, total_content_size,
 manifest_chapter_count, manifest_content_size, manifest_word_count, completed_chapters,
 base_run_id, created_at, updated_at, completed_at`

func readImportRunRow(ctx context.Context, q importRunQueryer, runID string) (importRunRow, bool, error) {
	var row importRunRow
	err := q.QueryRowContext(ctx, `SELECT `+importRunRowColumns+` FROM import_runs WHERE id = ?`, runID).Scan(
		&row.ID, &row.Purpose, &row.RootRunID, &row.EffectNamespace, &row.SourceFingerprint,
		&row.ManifestFingerprint, &row.AuthorityFingerprint, &row.LegacySourceFingerprint, &row.SourceDisplayJSON,
		&row.Locale, &row.Stage, &row.Status, &row.CompletedBatchesJSON, &row.LastError, &row.Resumable, &row.CancelRequested,
		&row.ExecutionOwner, &row.ExecutionEpoch, &row.LeaseExpiresAt, &row.TotalChapters, &row.TotalContentSize,
		&row.ManifestChapterCount, &row.ManifestContentSize, &row.ManifestWordCount, &row.CompletedChapters,
		&row.BaseRunID, &row.CreatedAt, &row.UpdatedAt, &row.CompletedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return importRunRow{}, false, nil
	}
	if err != nil {
		return importRunRow{}, false, err
	}
	return row, true, nil
}

// importRunHasLedger mirrors hasEffectLedger: a database upgraded past M04 reads
// its checkpoints from the ledger, an older one still trusts the JSON column.
func importRunHasLedger(ctx context.Context, q importRunQueryer) (bool, error) {
	var present int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='import_effect_ledger'`).Scan(&present)
	if err != nil {
		return false, err
	}
	return present > 0, nil
}

// importRunCompletedBatches is completedBatches: ledger evidence wins, the JSON
// column is only a fallback and is validated rather than trusted.
func importRunCompletedBatches(ctx context.Context, q importRunQueryer, row importRunRow) (map[string][]string, error) {
	result := map[string][]string{}
	ledger, err := importRunHasLedger(ctx, q)
	if err != nil {
		return nil, err
	}
	if ledger {
		rows, err := q.QueryContext(ctx, `SELECT stage, batch_id FROM import_effect_ledger
 WHERE run_id = ? AND state = 'complete' AND NOT (stage = ? AND batch_id = ?) ORDER BY rowid`,
			row.ID, importRunAuthorInferenceStage, importRunAuthorInferenceBatchID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var stage, batchID string
			if err := rows.Scan(&stage, &batchID); err != nil {
				return nil, err
			}
			result[stage] = append(result[stage], batchID)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return result, nil
	}
	var decoded any
	if err := json.Unmarshal([]byte(row.CompletedBatchesJSON), &decoded); err != nil {
		return nil, errors.New("IMPORT_RUN_CHECKPOINT_CORRUPT")
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New("IMPORT_RUN_CHECKPOINT_CORRUPT")
	}
	for stage, batches := range object {
		if !importRunStageKnown(stage) {
			return nil, errors.New("IMPORT_RUN_CHECKPOINT_CORRUPT")
		}
		list, ok := batches.([]any)
		if !ok {
			return nil, errors.New("IMPORT_RUN_CHECKPOINT_CORRUPT")
		}
		names := make([]string, 0, len(list))
		for _, entry := range list {
			name, ok := entry.(string)
			if !ok {
				return nil, errors.New("IMPORT_RUN_CHECKPOINT_CORRUPT")
			}
			names = append(names, name)
		}
		result[stage] = names
	}
	return result, nil
}

func importRunStageKnown(stage string) bool {
	switch stage {
	case "parsing", "prepared", "knowledge", "global", "style", "blueprints",
		"author-commit", "author-publish", "author-postprocess", "refresh", "completed":
		return true
	}
	return false
}

// importRunStagesFor mirrors importStagesFor: the author plan and the reference
// plan differ, and the frozen inference option appends the three inference
// stages after author-postprocess.
func importRunStagesFor(purpose string, inference bool) []string {
	if purpose == "reference" {
		return []string{"knowledge", "global", "style", "blueprints", "refresh", importRunStageCompleted}
	}
	if inference {
		return []string{"author-commit", "author-publish", "author-postprocess", "global", "style",
			"blueprints", "refresh", importRunStageCompleted}
	}
	return []string{"author-commit", "author-publish", "author-postprocess", "refresh", importRunStageCompleted}
}

func importRunNextStage(row importRunRow, inference bool, stage string) (string, bool) {
	plan := importRunStagesFor(row.Purpose, inference)
	for index, name := range plan {
		if name != stage {
			continue
		}
		if index+1 >= len(plan) || plan[index+1] == importRunStageCompleted {
			return "", false
		}
		return plan[index+1], true
	}
	return "", false
}

func importRunAuthorInference(ctx context.Context, q importRunQueryer, runID string) (bool, error) {
	ledger, err := importRunHasLedger(ctx, q)
	if err != nil || !ledger {
		return false, err
	}
	var present int64
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_effect_ledger WHERE run_id = ? AND stage = ? AND batch_id = ?`,
		runID, importRunAuthorInferenceStage, importRunAuthorInferenceBatchID).Scan(&present)
	if err != nil {
		return false, err
	}
	return present > 0, nil
}

type importRunProgress struct {
	completedSources  int64
	totalSources      int64
	completedChapters int64
	progressCompleted int64
	progressTotal     int64
}

func importRunSourceProgress(ctx context.Context, q importRunQueryer, runID string) (importRunProgress, error) {
	var progress importRunProgress
	err := q.QueryRowContext(ctx, `SELECT COUNT(*),
 COALESCE(SUM(CASE WHEN status = 'completed' THEN 1 ELSE 0 END), 0),
 COALESCE(SUM(CASE WHEN status = 'completed' THEN chapter_count ELSE 0 END), 0)
 FROM import_run_sources WHERE run_id = ?`, runID).Scan(&progress.totalSources, &progress.completedSources, &progress.completedChapters)
	if err != nil {
		return importRunProgress{}, err
	}
	return progress, nil
}

func importRunUnfinishedSourceDisplay(ctx context.Context, q importRunQueryer, runID string) ([]any, error) {
	rows, err := q.QueryContext(ctx, `SELECT display_json FROM import_run_sources
 WHERE run_id = ? AND status <> 'completed' ORDER BY source_index`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	display := make([]any, 0)
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			// Per-row fallback of the original's parseJson default argument.
			decoded = map[string]any{"displayName": "", "mediaType": "", "size": int64(0)}
		}
		display = append(display, decoded)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return display, nil
}

// importRunChaptersMissingBlueprint: an author run only infers blueprints for
// chapters that have none; chapters that already own one are not this run's.
func importRunChaptersMissingBlueprint(ctx context.Context, q importRunQueryer, runID string) (int64, error) {
	var missing int64
	err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_run_chapters AS chapters
 WHERE chapters.run_id = ? AND NOT EXISTS (
  SELECT 1 FROM blueprints WHERE blueprints.chapter_number = chapters.chapter_number
 )`, runID).Scan(&missing)
	if err != nil {
		return 0, err
	}
	return missing, nil
}

type importRunCheckpointChapter struct {
	chapterNumber       int64
	sourceID            string
	sourceChapterNumber int64
	contentFingerprint  string
}

func importRunChapterRowsForRange(ctx context.Context, q importRunQueryer, runID string, startChapter, endChapter int64) ([]importRunCheckpointChapter, error) {
	rows, err := q.QueryContext(ctx, `SELECT chapter_number, source_id, source_chapter_number, content_fingerprint
 FROM import_run_chapters WHERE run_id = ? AND chapter_number BETWEEN ? AND ? ORDER BY chapter_number ASC`,
		runID, startChapter, endChapter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chapters := make([]importRunCheckpointChapter, 0)
	for rows.Next() {
		var chapter importRunCheckpointChapter
		if err := rows.Scan(&chapter.chapterNumber, &chapter.sourceID, &chapter.sourceChapterNumber, &chapter.contentFingerprint); err != nil {
			return nil, err
		}
		chapters = append(chapters, chapter)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chapters, nil
}

// importRunKnowledgeReceipt is one committed reference-knowledge receipt. Only
// a real knowledge import writes these rows, so a run whose chapters lack them
// can never be checkpointed as imported.
type importRunKnowledgeReceipt struct {
	chapterNumber       int64
	purpose             string
	sourceID            string
	sourceChapterNumber int64
	contentFingerprint  string
	documentID          string
	state               string
}

func importRunKnowledgeReceipts(ctx context.Context, q importRunQueryer, runID string, startChapter, endChapter int64) ([]importRunKnowledgeReceipt, error) {
	rows, err := q.QueryContext(ctx, `SELECT chapter_number, purpose, source_id, source_chapter_number,
 content_fingerprint, document_id, state
 FROM import_run_knowledge_receipts WHERE run_id = ? AND chapter_number BETWEEN ? AND ?
 ORDER BY chapter_number ASC`, runID, startChapter, endChapter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	receipts := make([]importRunKnowledgeReceipt, 0)
	for rows.Next() {
		var receipt importRunKnowledgeReceipt
		if err := rows.Scan(&receipt.chapterNumber, &receipt.purpose, &receipt.sourceID,
			&receipt.sourceChapterNumber, &receipt.contentFingerprint, &receipt.documentID, &receipt.state); err != nil {
			return nil, err
		}
		receipts = append(receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return receipts, nil
}

type importRunBatchCheckpoint struct {
	startChapter              int64
	endChapter                int64
	contentFingerprintPrefixes []string
}

// parseImportRunChapterBatchCheckpointID mirrors
// parseImportRunChapterBatchCheckpointId; nil means unparseable.
func parseImportRunChapterBatchCheckpointID(batchID string) *importRunBatchCheckpoint {
	match := importRunBatchCheckpointID.FindStringSubmatch(batchID)
	if match == nil {
		return nil
	}
	start, startErr := strconv.ParseInt(match[1], 10, 64)
	end, endErr := strconv.ParseInt(match[2], 10, 64)
	if startErr != nil || endErr != nil || start > 9007199254740991 || end > 9007199254740991 {
		return nil
	}
	prefixes := strings.Split(match[3], ".")
	if end < start || int64(len(prefixes)) != end-start+1 {
		return nil
	}
	return &importRunBatchCheckpoint{startChapter: start, endChapter: end, contentFingerprintPrefixes: prefixes}
}

// importRunCheckpointChapterNumbers validates that the chapters the batch id
// names are exactly the persisted ones, and — for the knowledge stage — that
// each has a committed knowledge receipt bound to that exact affiliation.
func importRunCheckpointChapterNumbers(ctx context.Context, q importRunQueryer, row importRunRow, stage, batchID string) ([]int64, error) {
	maxBatch := importRunKnowledgeBatchSize
	if stage == "blueprints" {
		maxBatch = importRunBlueprintBatchSize
	} else if stage != "knowledge" {
		return nil, errors.New("IMPORT_RUN_BATCH_ID_INVALID")
	}
	parsed := parseImportRunChapterBatchCheckpointID(batchID)
	if parsed == nil || int64(len(parsed.contentFingerprintPrefixes)) > maxBatch {
		return nil, errors.New("IMPORT_RUN_BATCH_ID_INVALID")
	}
	chapters, err := importRunChapterRowsForRange(ctx, q, row.ID, parsed.startChapter, parsed.endChapter)
	if err != nil {
		return nil, err
	}
	if len(chapters) != len(parsed.contentFingerprintPrefixes) {
		return nil, errors.New("IMPORT_RUN_BATCH_ID_INVALID")
	}
	for index, chapter := range chapters {
		if chapter.chapterNumber != parsed.startChapter+int64(index) ||
			!strings.HasPrefix(chapter.contentFingerprint, parsed.contentFingerprintPrefixes[index]) {
			return nil, errors.New("IMPORT_RUN_BATCH_ID_INVALID")
		}
	}
	if stage != "knowledge" {
		numbers := make([]int64, 0, len(chapters))
		for _, chapter := range chapters {
			numbers = append(numbers, chapter.chapterNumber)
		}
		return numbers, nil
	}
	// A reference checkpoint is only legal evidence if the knowledge base
	// really holds one committed document per chapter, bound to the same
	// source affiliation and the same frozen content.
	receipts, err := importRunKnowledgeReceipts(ctx, q, row.ID, parsed.startChapter, parsed.endChapter)
	if err != nil {
		return nil, err
	}
	if len(receipts) != len(chapters) {
		return nil, errors.New("IMPORT_RUN_REFERENCE_RECEIPT_INCOMPLETE")
	}
	numbers := make([]int64, 0, len(chapters))
	for index, receipt := range receipts {
		chapter := chapters[index]
		if receipt.chapterNumber != chapter.chapterNumber || receipt.purpose != row.Purpose ||
			receipt.sourceID != chapter.sourceID || receipt.sourceChapterNumber != chapter.sourceChapterNumber ||
			receipt.contentFingerprint != chapter.contentFingerprint ||
			!importRunSHA256.MatchString(receipt.documentID) || receipt.state != "committed" {
			return nil, errors.New("IMPORT_RUN_REFERENCE_RECEIPT_INCOMPLETE")
		}
		numbers = append(numbers, chapter.chapterNumber)
	}
	return numbers, nil
}

func importRunCompletedCheckpointChapters(ctx context.Context, q importRunQueryer, row importRunRow, stage string) (int64, error) {
	batches, err := importRunCompletedBatches(ctx, q, row)
	if err != nil {
		return 0, err
	}
	covered := map[int64]bool{}
	for _, batchID := range batches[stage] {
		numbers, err := importRunCheckpointChapterNumbers(ctx, q, row, stage, batchID)
		if err != nil {
			return 0, err
		}
		for _, number := range numbers {
			covered[number] = true
		}
	}
	return int64(len(covered)), nil
}

func importRunPersistedProgress(ctx context.Context, q importRunQueryer, row importRunRow) (importRunProgress, error) {
	sources, err := importRunSourceProgress(ctx, q, row.ID)
	if err != nil {
		return importRunProgress{}, err
	}
	progress := sources
	if row.Stage == "parsing" {
		progress.progressCompleted = sources.completedSources
		progress.progressTotal = sources.totalSources
		return progress, nil
	}
	// The persisted progress column is NOT what the snapshot reports: the
	// original derives it from the stage, so a run resumed mid-stage never
	// advertises chapters it has not checkpointed.
	completedChapters := row.TotalChapters
	switch row.Stage {
	case "prepared":
		completedChapters = 0
	case "knowledge":
		completedChapters, err = importRunCompletedCheckpointChapters(ctx, q, row, "knowledge")
		if err != nil {
			return importRunProgress{}, err
		}
	}
	progress.completedChapters = completedChapters
	if row.Stage == "knowledge" || row.Stage == "prepared" {
		progress.progressCompleted = completedChapters
		progress.progressTotal = row.TotalChapters
		return progress, nil
	}
	if row.Stage == "blueprints" {
		completed, err := importRunCompletedCheckpointChapters(ctx, q, row, "blueprints")
		if err != nil {
			return importRunProgress{}, err
		}
		progress.progressCompleted = completed
		if row.Purpose == "author-manuscript" {
			missing, err := importRunChaptersMissingBlueprint(ctx, q, row.ID)
			if err != nil {
				return importRunProgress{}, err
			}
			progress.progressTotal = completed + missing
			return progress, nil
		}
		progress.progressTotal = row.TotalChapters
		return progress, nil
	}
	if row.Stage == "global" || row.Stage == "style" || row.Stage == "refresh" {
		batches, err := importRunCompletedBatches(ctx, q, row)
		if err != nil {
			return importRunProgress{}, err
		}
		progress.progressCompleted = 0
		for _, batchID := range batches[row.Stage] {
			if batchID == "done" {
				progress.progressCompleted = 1
			}
		}
		progress.progressTotal = 1
		return progress, nil
	}
	progress.progressCompleted = row.TotalChapters
	progress.progressTotal = row.TotalChapters
	return progress, nil
}

// importRunSnapshot is rowToSnapshot. Field presence follows the original's
// spread: manifest facts exist only for author runs, authorityFingerprint and
// the two nullable timestamps are omitted when absent.
func importRunSnapshot(ctx context.Context, q importRunQueryer, row importRunRow) (map[string]any, error) {
	progress, err := importRunPersistedProgress(ctx, q, row)
	if err != nil {
		return nil, err
	}
	batches, err := importRunCompletedBatches(ctx, q, row)
	if err != nil {
		return nil, err
	}
	display := make([]any, 0)
	if err := json.Unmarshal([]byte(row.SourceDisplayJSON), &display); err != nil {
		display = make([]any, 0)
	}
	if display == nil {
		display = make([]any, 0)
	}
	unfinished, err := importRunUnfinishedSourceDisplay(ctx, q, row.ID)
	if err != nil {
		return nil, err
	}
	snapshot := map[string]any{
		"id":                     row.ID,
		"purpose":                row.Purpose,
		"rootRunId":              row.RootRunID,
		"effectNamespace":        row.EffectNamespace,
		"sourceDisplay":          display,
		"unfinishedSourceDisplay": unfinished,
		"locale":                 row.Locale,
		"stage":                  row.Stage,
		"status":                 row.Status,
		"completedBatches":       batches,
		"lastError":              row.LastError,
		"resumable":              row.Resumable == 1,
		"cancelRequested":        row.CancelRequested == 1,
		"totalChapters":          row.TotalChapters,
		"totalContentSize":       row.TotalContentSize,
		"manifestChapterCount":   row.ManifestChapterCount,
		"manifestContentSize":    row.ManifestContentSize,
		"manifestWordCount":      row.ManifestWordCount,
		"completedChapters":      progress.completedChapters,
		"completedSources":       progress.completedSources,
		"totalSources":           progress.totalSources,
		"progressCompleted":      progress.progressCompleted,
		"progressTotal":          progress.progressTotal,
		"createdAt":              row.CreatedAt,
		"updatedAt":              row.UpdatedAt,
	}
	if row.Purpose == "author-manuscript" {
		snapshot["manifestFingerprint"] = row.ManifestFingerprint
		if row.AuthorityFingerprint != "" {
			snapshot["authorityFingerprint"] = row.AuthorityFingerprint
		}
		inference, err := importRunAuthorInference(ctx, q, row.ID)
		if err != nil {
			return nil, err
		}
		snapshot["authorInference"] = inference
	}
	if row.BaseRunID.Valid {
		snapshot["baseRunId"] = row.BaseRunID.String
	}
	if row.CompletedAt.Valid {
		snapshot["completedAt"] = row.CompletedAt.String
	}
	return snapshot, nil
}

// importRunChapterSnapshot re-hashes the frozen chapter body, so a row edited
// behind this lane's back is refused instead of served.
func importRunChapterSnapshot(chapterNumber, contentSize int64, title, fingerprint, content string) (map[string]any, error) {
	sum := sha256.Sum256([]byte(content))
	if contentSize < 1 || contentSize > importRunMaxChapterBytes || int64(len(content)) != contentSize ||
		!importRunSHA256.MatchString(fingerprint) || hex.EncodeToString(sum[:]) != fingerprint {
		return nil, errors.New("IMPORT_RUN_SNAPSHOT_CORRUPT")
	}
	return map[string]any{
		"number":             chapterNumber,
		"title":              title,
		"contentFingerprint": fingerprint,
		"contentSize":        contentSize,
		"content":            content,
	}, nil
}

// importRunAssertExecutionAuthority is assertExecutionAuthority: only the exact
// running owner with the exact epoch inside the lease may mutate the run.
func importRunAssertExecutionAuthority(ctx context.Context, q importRunQueryer, runID string, execution ImportRunExecution, now int64) (importRunRow, error) {
	row, present, err := readImportRunRow(ctx, q, runID)
	if err != nil {
		return importRunRow{}, err
	}
	if !present || row.Status != "running" || execution.Owner == "" ||
		row.ExecutionOwner != execution.Owner || row.ExecutionEpoch != execution.Epoch ||
		row.LeaseExpiresAt <= now {
		return importRunRow{}, errors.New("IMPORT_RUN_EXECUTION_STALE")
	}
	return row, nil
}

func importRunAssertKnownEffectHistory(ctx context.Context, q importRunQueryer, row importRunRow, stage, batchID string) error {
	ledger, err := importRunHasLedger(ctx, q)
	if err != nil || !ledger {
		return err
	}
	var unknown int64
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_effect_ledger
 WHERE run_id = ? AND stage = ? AND state = 'unknown' AND (batch_id = ? OR batch_id LIKE '__legacy_%')`,
		row.ID, stage, batchID).Scan(&unknown)
	if err != nil {
		return err
	}
	if unknown > 0 {
		return errors.New("IMPORT_RUN_EFFECT_HISTORY_UNKNOWN")
	}
	return nil
}

func importRunAuthorCheckpointChapter(ctx context.Context, q importRunQueryer, row importRunRow, batchID string) (int64, error) {
	match := importRunAuthorCheckpoint.FindStringSubmatch(batchID)
	if match == nil {
		return 0, errors.New("IMPORT_RUN_BATCH_ID_INVALID")
	}
	chapterNumber, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || chapterNumber > 9007199254740991 {
		return 0, errors.New("IMPORT_RUN_BATCH_ID_INVALID")
	}
	var present int64
	err = q.QueryRowContext(ctx, `SELECT COUNT(*) FROM import_run_chapters WHERE run_id = ? AND chapter_number = ?`,
		row.ID, chapterNumber).Scan(&present)
	if err != nil {
		return 0, err
	}
	if present == 0 {
		return 0, errors.New("IMPORT_RUN_BATCH_ID_INVALID")
	}
	return chapterNumber, nil
}

// importRunAssertCheckpointCanApply is assertCheckpointCanApply: the stage
// decides whether a receipt or a direct checkpoint is the only legal evidence,
// and two checkpoints may never cover the same chapters.
func importRunAssertCheckpointCanApply(ctx context.Context, q importRunQueryer, row importRunRow, stage, batchID, source string) error {
	if row.Status != "running" || row.Stage != stage {
		return errors.New("IMPORT_RUN_BATCH_STAGE_MISMATCH")
	}
	if stage == "parsing" || stage == "prepared" || stage == "completed" {
		return errors.New("IMPORT_RUN_BATCH_ID_INVALID")
	}
	if err := importRunAssertKnownEffectHistory(ctx, q, row, stage, batchID); err != nil {
		return err
	}
	receiptStage := stage == "global" || stage == "style" || stage == "blueprints" || stage == "author-commit"
	if receiptStage && source != "receipt" {
		return errors.New("IMPORT_RUN_RECEIPT_REQUIRED")
	}
	if !receiptStage && source != "direct" {
		return errors.New("IMPORT_RUN_RECEIPT_NOT_ACCEPTED")
	}
	if stage == "global" || stage == "style" || stage == "author-commit" || stage == "refresh" {
		if batchID != "done" {
			return errors.New("IMPORT_RUN_BATCH_ID_INVALID")
		}
		return nil
	}
	if stage == "author-publish" || stage == "author-postprocess" {
		chapterNumber, err := importRunAuthorCheckpointChapter(ctx, q, row, batchID)
		if err != nil {
			return err
		}
		batches, err := importRunCompletedBatches(ctx, q, row)
		if err != nil {
			return err
		}
		for _, existing := range batches[stage] {
			if existing == batchID {
				continue
			}
			existingChapter, err := importRunAuthorCheckpointChapter(ctx, q, row, existing)
			if err != nil {
				return err
			}
			if existingChapter == chapterNumber {
				return errors.New("IMPORT_RUN_CHECKPOINT_OVERLAP")
			}
		}
		return nil
	}
	if stage != "knowledge" && stage != "blueprints" {
		return errors.New("IMPORT_RUN_BATCH_ID_INVALID")
	}
	if _, err := importRunCheckpointChapterNumbers(ctx, q, row, stage, batchID); err != nil {
		return err
	}
	parsed := parseImportRunChapterBatchCheckpointID(batchID)
	batches, err := importRunCompletedBatches(ctx, q, row)
	if err != nil {
		return err
	}
	for _, existing := range batches[stage] {
		if existing == batchID {
			continue
		}
		existingRange := parseImportRunChapterBatchCheckpointID(existing)
		if existingRange == nil {
			return errors.New("IMPORT_RUN_BATCH_ID_INVALID")
		}
		if parsed.startChapter <= existingRange.endChapter && existingRange.startChapter <= parsed.endChapter {
			return errors.New("IMPORT_RUN_CHECKPOINT_OVERLAP")
		}
	}
	return nil
}

// importRunAssertStageCheckpointComplete is assertStageCheckpointComplete: a
// stage may only be left once every chapter it owns is checkpointed.
func importRunAssertStageCheckpointComplete(ctx context.Context, q importRunQueryer, row importRunRow, stage string) error {
	batches, err := importRunCompletedBatches(ctx, q, row)
	if err != nil {
		return err
	}
	completed := batches[stage]
	if stage == "global" || stage == "style" || stage == "author-commit" || stage == "refresh" {
		if len(completed) != 1 || completed[0] != "done" {
			return errors.New("IMPORT_RUN_STAGE_INCOMPLETE")
		}
		return nil
	}
	if stage == "author-publish" || stage == "author-postprocess" {
		chapters, err := importRunChapterRowsForRange(ctx, q, row.ID, 1, 9007199254740991)
		if err != nil {
			return err
		}
		covered := map[int64]bool{}
		for _, batchID := range completed {
			chapterNumber, err := importRunAuthorCheckpointChapter(ctx, q, row, batchID)
			if err != nil {
				return err
			}
			covered[chapterNumber] = true
		}
		if len(covered) != len(chapters) {
			return errors.New("IMPORT_RUN_STAGE_INCOMPLETE")
		}
		for _, chapter := range chapters {
			if !covered[chapter.chapterNumber] {
				return errors.New("IMPORT_RUN_STAGE_INCOMPLETE")
			}
		}
		return nil
	}
	if stage == "blueprints" && row.Purpose == "author-manuscript" {
		missing, err := importRunChaptersMissingBlueprint(ctx, q, row.ID)
		if err != nil {
			return err
		}
		if missing != 0 {
			return errors.New("IMPORT_RUN_STAGE_INCOMPLETE")
		}
		return nil
	}
	if stage != "knowledge" && stage != "blueprints" {
		return errors.New("IMPORT_RUN_STAGE_INCOMPLETE")
	}
	chapters, err := importRunChapterRowsForRange(ctx, q, row.ID, 1, 9007199254740991)
	if err != nil {
		return err
	}
	covered := map[int64]bool{}
	for _, batchID := range completed {
		numbers, err := importRunCheckpointChapterNumbers(ctx, q, row, stage, batchID)
		if err != nil {
			return err
		}
		for _, number := range numbers {
			if covered[number] {
				return errors.New("IMPORT_RUN_CHECKPOINT_OVERLAP")
			}
			covered[number] = true
		}
	}
	if len(covered) != len(chapters) {
		return errors.New("IMPORT_RUN_STAGE_INCOMPLETE")
	}
	for _, chapter := range chapters {
		if !covered[chapter.chapterNumber] {
			return errors.New("IMPORT_RUN_STAGE_INCOMPLETE")
		}
	}
	return nil
}

// importRunApplyBatchCheckpoint is applyBatchCheckpoint. A receipt checkpoint
// promotes its prepared ledger row; a direct checkpoint records its own
// evidence row. Both then apply the deferrable cancel to the run.
func importRunApplyBatchCheckpoint(ctx context.Context, q importRunQueryer, row importRunRow, stage, batchID, source string) (bool, bool, error) {
	if err := importRunAssertCheckpointCanApply(ctx, q, row, stage, batchID, source); err != nil {
		return false, false, err
	}
	ledger, err := importRunHasLedger(ctx, q)
	if err != nil {
		return false, false, err
	}
	batches, err := importRunCompletedBatches(ctx, q, row)
	if err != nil {
		return false, false, err
	}
	newlyCompleted := true
	for _, existing := range batches[stage] {
		if existing == batchID {
			newlyCompleted = false
		}
	}
	if ledger {
		var state, namespace, effectKey string
		err := q.QueryRowContext(ctx, `SELECT state, effect_namespace, effect_key FROM import_effect_ledger
 WHERE run_id = ? AND stage = ? AND batch_id = ?`, row.ID, stage, batchID).Scan(&state, &namespace, &effectKey)
		existing := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, false, err
		}
		if existing && state == "unknown" {
			return false, false, errors.New("IMPORT_RUN_EFFECT_HISTORY_UNKNOWN")
		}
		if source == "receipt" {
			var receiptState, receiptNamespace, receiptKey string
			receiptErr := q.QueryRowContext(ctx, `SELECT state, effect_namespace, effect_key FROM import_run_receipts
 WHERE run_id = ? AND stage = ? AND batch_id = ?`, row.ID, stage, batchID).Scan(&receiptState, &receiptNamespace, &receiptKey)
			if receiptErr != nil && !errors.Is(receiptErr, sql.ErrNoRows) {
				return false, false, receiptErr
			}
			if receiptErr != nil || receiptState != "prepared" || !existing || state != "partial" ||
				namespace != receiptNamespace || effectKey != receiptKey {
				return false, false, errors.New("IMPORT_RUN_EFFECT_LEDGER_MISMATCH")
			}
			if _, err := q.ExecContext(ctx, `UPDATE import_effect_ledger
 SET state='complete', diagnostic_code='', updated_at=datetime('now')
 WHERE run_id = ? AND stage = ? AND batch_id = ? AND state = 'partial'`, row.ID, stage, batchID); err != nil {
				return false, false, err
			}
		} else if !existing {
			evidence := "legacy-checkpoint"
			if stage == "knowledge" {
				evidence = "knowledge-receipt"
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO import_effect_ledger(
 run_id, stage, batch_id, effect_namespace, effect_key, state, evidence_kind, diagnostic_code
) VALUES(?, ?, ?, ?, ?, 'complete', ?, '')`, row.ID, stage, batchID, row.EffectNamespace,
				"checkpoint:"+stage+":"+batchID, evidence); err != nil {
				return false, false, err
			}
		}
	} else if newlyCompleted {
		batches[stage] = append(batches[stage], batchID)
	}
	cancelApplied := row.CancelRequested == 1
	if _, err := q.ExecContext(ctx, `UPDATE import_runs
 SET status = CASE WHEN ? = 1 THEN 'cancelled' ELSE status END,
     resumable = 1,
     execution_owner = CASE WHEN ? = 1 THEN '' ELSE execution_owner END,
     execution_epoch = execution_epoch + CASE WHEN ? = 1 THEN 1 ELSE 0 END,
     lease_expires_at = CASE WHEN ? = 1 THEN 0 ELSE lease_expires_at END,
     updated_at = datetime('now')
 WHERE id = ?`, importRunBool(cancelApplied), importRunBool(cancelApplied),
		importRunBool(cancelApplied), importRunBool(cancelApplied), row.ID); err != nil {
		return false, false, err
	}
	if !ledger {
		if err := importRunWriteCompletedBatches(ctx, q, row.ID, batches); err != nil {
			return false, false, err
		}
	}
	return newlyCompleted, cancelApplied, nil
}

func importRunWriteCompletedBatches(ctx context.Context, q importRunQueryer, runID string, batches map[string][]string) error {
	encoded, err := json.Marshal(batches)
	if err != nil {
		return err
	}
	_, err = q.ExecContext(ctx, `UPDATE import_runs SET completed_batches_json = ? WHERE id = ?`, string(encoded), runID)
	return err
}

func importRunBool(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

// importRunImmediate reserves one connection for a manual IMMEDIATE
// transaction so a cancelled request cannot leak an open transaction, and
// never runs a read-then-write upgrade on a shared connection.
func (r *ImportRuns) importRunImmediate(ctx context.Context, fn func(*sql.Conn) error) (retErr error) {
	if r == nil || r.db == nil {
		return ErrProjectNotOpen
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			retErr = errors.Join(retErr, err)
		}
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// importRunReadSnapshot runs a multi-query read in one read-only snapshot.
func (r *ImportRuns) importRunReadSnapshot(ctx context.Context, fn func(*sql.Tx) error) error {
	if r == nil || r.db == nil {
		return ErrProjectNotOpen
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Get is db:import-run-get. A missing run is null, never a refusal.
func (r *ImportRuns) Get(ctx context.Context, runID string) (any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var snapshot any
	err := r.importRunReadSnapshot(ctx, func(tx *sql.Tx) error {
		row, present, err := readImportRunRow(ctx, tx, runID)
		if err != nil || !present {
			return err
		}
		snapshot, err = importRunSnapshot(ctx, tx, row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

// ListResumable is db:import-run-list-resumable.
//
// The original also filters rows the portable runtime freeze pins
// (readPortableRuntimeFreeze(...).isFrozen('import_runs', id)). This lane cannot
// reach a frozen project at all: every admitted request re-runs
// projectfs.Project.VerifyNoPortableAuthority (projectaccess/admit_author.go:117,
// authorruntime/read_linux.go:138), which refuses ANY presence at the installed
// runtime-freeze name before the request is served. The filter is therefore
// provably empty here, and no run is ever hidden.
func (r *ImportRuns) ListResumable(ctx context.Context) (any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	result := make([]any, 0)
	err := r.importRunReadSnapshot(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT `+importRunRowColumns+` FROM import_runs
 WHERE resumable = 1 AND status IN ('ready', 'running', 'failed', 'cancelled')
 ORDER BY updated_at DESC, rowid DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var row importRunRow
			if err := rows.Scan(
				&row.ID, &row.Purpose, &row.RootRunID, &row.EffectNamespace, &row.SourceFingerprint,
				&row.ManifestFingerprint, &row.AuthorityFingerprint, &row.LegacySourceFingerprint, &row.SourceDisplayJSON,
				&row.Locale, &row.Stage, &row.Status, &row.CompletedBatchesJSON, &row.LastError, &row.Resumable, &row.CancelRequested,
				&row.ExecutionOwner, &row.ExecutionEpoch, &row.LeaseExpiresAt, &row.TotalChapters, &row.TotalContentSize,
				&row.ManifestChapterCount, &row.ManifestContentSize, &row.ManifestWordCount, &row.CompletedChapters,
				&row.BaseRunID, &row.CreatedAt, &row.UpdatedAt, &row.CompletedAt,
			); err != nil {
				return err
			}
			snapshot, err := importRunSnapshot(ctx, tx, row)
			if err != nil {
				return err
			}
			result = append(result, snapshot)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// ListChapterBatch is db:import-run-list-chapters. The limit is clamped exactly
// like MAX_PAGE_SIZE, and every page re-verifies the frozen body.
func (r *ImportRuns) ListChapterBatch(ctx context.Context, runID string, afterChapterNumber, limit int64) (any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	if limit < 1 {
		limit = 1
	}
	if limit > importRunMaxPageSize {
		limit = importRunMaxPageSize
	}
	result := make([]any, 0)
	err := r.importRunReadSnapshot(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT chapter_number, title, content_fingerprint, content_size, content_snapshot
 FROM import_run_chapters WHERE run_id = ? AND chapter_number > ?
 ORDER BY chapter_number ASC LIMIT ?`, runID, afterChapterNumber, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var chapterNumber, contentSize int64
			var title, fingerprint, content string
			if err := rows.Scan(&chapterNumber, &title, &fingerprint, &contentSize, &content); err != nil {
				return err
			}
			chapter, err := importRunChapterSnapshot(chapterNumber, contentSize, title, fingerprint, content)
			if err != nil {
				return err
			}
			result = append(result, chapter)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// StartOrResume is db:import-run-start-resume. It mints the execution lease and
// moves a prepared run into its first plan stage.
//
// The original also calls readPortableRuntimeFreeze(...).assertMutable before
// opening the transaction; see ListResumable for why a frozen run is
// unreachable in this lane.
func (r *ImportRuns) StartOrResume(ctx context.Context, runID, owner string) (any, error) {
	normalized := strings.TrimSpace(owner)
	if normalized == "" || len(normalized) > 160 {
		return nil, errors.New("IMPORT_RUN_EXECUTOR_INVALID")
	}
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		now := time.Now().UnixMilli()
		row, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present || row.Resumable != 1 || !importRunStartableStatus(row.Status) {
			if present && row.Resumable == 0 && row.LastError != "" {
				return errors.New(row.LastError)
			}
			return errors.New("IMPORT_RUN_NOT_STARTABLE")
		}
		if row.Stage == "parsing" {
			return errors.New("IMPORT_RUN_PARSING_INCOMPLETE")
		}
		var active string
		activeErr := conn.QueryRowContext(ctx, `SELECT id FROM import_runs
 WHERE id <> ? AND status = 'running' AND execution_owner <> '' AND lease_expires_at > ? LIMIT 1`,
			runID, now).Scan(&active)
		if activeErr == nil {
			return errors.New("IMPORT_RUN_ANOTHER_RUNNING")
		}
		if !errors.Is(activeErr, sql.ErrNoRows) {
			return activeErr
		}
		if row.ExecutionOwner != "" && row.ExecutionOwner != normalized && row.LeaseExpiresAt > now {
			return errors.New("IMPORT_RUN_EXECUTOR_BUSY")
		}
		epoch := row.ExecutionEpoch
		if !(row.ExecutionOwner == normalized && row.LeaseExpiresAt > now) {
			epoch++
		}
		expiresAt := now + importRunLeaseMS
		if _, err := conn.ExecContext(ctx, `UPDATE import_runs
 SET stage = CASE WHEN stage = 'prepared' THEN 'knowledge' ELSE stage END,
     status = 'running', resumable = 1, cancel_requested = 0, last_error = '',
     execution_owner = ?, execution_epoch = ?, lease_expires_at = ?,
     updated_at = datetime('now')
 WHERE id = ?`, normalized, epoch, expiresAt, runID); err != nil {
			return err
		}
		updated, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		snapshot, err := importRunSnapshot(ctx, conn, updated)
		if err != nil {
			return err
		}
		response = map[string]any{
			"run": snapshot,
			"execution": map[string]any{
				"owner":     normalized,
				"epoch":     epoch,
				"expiresAt": expiresAt,
			},
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

func importRunStartableStatus(status string) bool {
	return status == "ready" || status == "running" || status == "failed" || status == "cancelled"
}

// RenewExecution is db:import-run-renew-execution: it only extends the lease of
// the exact live owner and epoch.
func (r *ImportRuns) RenewExecution(ctx context.Context, runID string, execution ImportRunExecution) (any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		now := time.Now().UnixMilli()
		row, err := importRunAssertExecutionAuthority(ctx, conn, runID, execution, now)
		if err != nil {
			return err
		}
		expiresAt := now + importRunLeaseMS
		if _, err := conn.ExecContext(ctx, `UPDATE import_runs SET lease_expires_at = ?, updated_at = datetime('now')
 WHERE id = ?`, expiresAt, runID); err != nil {
			return err
		}
		response = map[string]any{
			"owner":     row.ExecutionOwner,
			"epoch":     row.ExecutionEpoch,
			"expiresAt": expiresAt,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// RequestCancel is db:import-run-request-cancel: it only marks the intent, and
// the boundary applies it.
func (r *ImportRuns) RequestCancel(ctx context.Context, runID string, execution ImportRunExecution) (any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := importRunAssertExecutionAuthority(ctx, conn, runID, execution, time.Now().UnixMilli()); err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, `UPDATE import_runs SET cancel_requested = 1, updated_at = datetime('now')
 WHERE id = ? AND status IN ('ready', 'running')`, runID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 0 {
			return errors.New("IMPORT_RUN_NOT_CANCELLABLE")
		}
		row, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		response, err = importRunSnapshot(ctx, conn, row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// CancelAtBoundary is db:import-run-cancel-at-boundary: it applies the cancel
// and retires the lease in one step.
func (r *ImportRuns) CancelAtBoundary(ctx context.Context, runID string, execution ImportRunExecution) (any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		if _, err := importRunAssertExecutionAuthority(ctx, conn, runID, execution, time.Now().UnixMilli()); err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, `UPDATE import_runs
 SET status = 'cancelled', resumable = 1, cancel_requested = 1,
     execution_owner = '', execution_epoch = execution_epoch + 1, lease_expires_at = 0,
     updated_at = datetime('now')
 WHERE id = ? AND status IN ('ready', 'running', 'failed')`, runID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 0 {
			return errors.New("IMPORT_RUN_NOT_CANCELLABLE")
		}
		row, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		response, err = importRunSnapshot(ctx, conn, row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// CompleteBatch is db:import-run-complete-batch for the direct checkpoint
// stages only (knowledge, author-publish, author-postprocess, refresh); the
// controller refuses every other stage before the repository runs.
func (r *ImportRuns) CompleteBatch(ctx context.Context, runID, stage, batchID string, execution ImportRunExecution) (map[string]any, error) {
	if strings.TrimSpace(batchID) == "" || len(batchID) > 160 {
		return nil, errors.New("IMPORT_RUN_BATCH_ID_INVALID")
	}
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response map[string]any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, err := importRunAssertExecutionAuthority(ctx, conn, runID, execution, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		newlyCompleted, cancelApplied, err := importRunApplyBatchCheckpoint(ctx, conn, row, stage, batchID, "direct")
		if err != nil {
			return err
		}
		updated, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		snapshot, err := importRunSnapshot(ctx, conn, updated)
		if err != nil {
			return err
		}
		response = map[string]any{
			"newlyCompleted": newlyCompleted,
			"cancelApplied":  cancelApplied,
			"run":            snapshot,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// AdvanceStage is db:import-run-advance-stage: the completed stage must be the
// run's own stage, the next stage must be the plan's next one, and the stage
// must be fully checkpointed first.
func (r *ImportRuns) AdvanceStage(ctx context.Context, runID, completedStage, nextStage string, execution ImportRunExecution) (any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, err := importRunAssertExecutionAuthority(ctx, conn, runID, execution, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if row.Status != "running" || row.Stage != completedStage {
			return errors.New("IMPORT_RUN_STAGE_MISMATCH")
		}
		inference, err := importRunAuthorInference(ctx, conn, runID)
		if err != nil {
			return err
		}
		expected, planned := importRunNextStage(row, inference, completedStage)
		if !planned || expected != nextStage {
			return errors.New("IMPORT_RUN_STAGE_MISMATCH")
		}
		if err := importRunAssertStageCheckpointComplete(ctx, conn, row, completedStage); err != nil {
			return err
		}
		completedChapters := row.CompletedChapters
		if completedStage == "blueprints" || completedStage == "author-postprocess" {
			completedChapters = row.TotalChapters
		}
		result, err := conn.ExecContext(ctx, `UPDATE import_runs
 SET stage = ?, completed_chapters = ?, status = 'running', last_error = '',
     resumable = 1, updated_at = datetime('now')
 WHERE id = ? AND stage = ? AND status = 'running'`, nextStage, completedChapters, runID, completedStage)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return errors.New("IMPORT_RUN_STAGE_MISMATCH")
		}
		updated, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		response, err = importRunSnapshot(ctx, conn, updated)
		return err
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// Complete is db:import-run-complete: only a running refresh stage whose
// checkpoint is 'done' may finish, and finishing retires the run and its lease.
func (r *ImportRuns) Complete(ctx context.Context, runID string, execution ImportRunExecution) (any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, err := importRunAssertExecutionAuthority(ctx, conn, runID, execution, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if row.Status != "running" || row.Stage != "refresh" {
			return errors.New("IMPORT_RUN_STAGE_MISMATCH")
		}
		if err := importRunAssertStageCheckpointComplete(ctx, conn, row, "refresh"); err != nil {
			return err
		}
		result, err := conn.ExecContext(ctx, `UPDATE import_runs
 SET stage = 'completed', status = 'completed', completed_chapters = total_chapters,
     resumable = 0, cancel_requested = 0, last_error = '',
     execution_owner = '', execution_epoch = execution_epoch + 1, lease_expires_at = 0,
     completed_at = datetime('now'), updated_at = datetime('now')
 WHERE id = ? AND stage = 'refresh' AND status = 'running'`, runID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		updated, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		response, err = importRunSnapshot(ctx, conn, updated)
		return err
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

