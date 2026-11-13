package store

// The parsing lane's writes, and the two lifecycle writes that end or continue a
// durable import run. This file is the store half of the renderer operations that
// had no method behind them:
//
//	db:import-run-prepare-inspection   BeginParsing -> CommitParsedSource per
//	                                   source (FailParsedSource on a refused
//	                                   one) -> FinalizeParsing
//	db:import-run-finalize-parsing     FinalizeParsing
//	db:import-run-restart              Restart
//	db:import-run-fail                 Fail
//
// Ported from electron/repositories/import-run-repository.ts: beginParsing
// (:1570-1723), commitParsedSource (:1725-1796), failParsedSource (:1798-1817),
// finalizeParsing (:1819-2003), restart (:2575-2659) and fail (:2721-2735), with
// the helpers they call - normalizeDisplay (:778-793), normalizeSourceIds
// (:795-809), normalizeSourceFingerprints (:811-819), normalizeChapters
// (:821-861), canonicalManifest/hashManifest (:217-230), assignStableChapterNumbers
// (:1233-1264), adoptLegacyCompletedRun (:1266-1333), matchingResumableRun
// (:1335-1343), latestCompletedRun (:1412-1418), completedChapterManifest
// (:1200-1231), overlappingResumableSourceRun (:1420-1436),
// discardProvisionalParsingRun (:1404-1410) and createPreparationInspection
// (:105-141).
//
// What is deliberately NOT here: prepare() for the author-manuscript purpose
// (:2005-2138), so db:import-run-author-preview and the author branch of
// db:import-run-prepare-inspection stay unbound. Its first step is
// FinalizedDraftImportRepository.preview, which belongs to the finalized-draft
// lane, and its resumable short-circuit asks hasCommittedAuthorFinalizationReceipt
// (:1345-1354), which validates a committed receipt through rowToEffectReceipt
// (:564-603) - the receipt lane importrun.go leaves unbound on purpose (see that
// file's header). A run this file creates is therefore always the reference
// purpose: BeginParsing refuses anything else with the original's own code.
//
// Every refusal below is the original's own symbolic string, returned as a plain
// error like every sibling in importrun.go, and every write is one IMMEDIATE
// transaction that never reads then upgrades on a shared connection.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
)

const (
	// importRunZeroFingerprint is '0'.repeat(64): the manifest fingerprint a run
	// carries before it owns one, and the one a parsing restart resets to.
	importRunZeroFingerprint = "0000000000000000000000000000000000000000000000000000000000000000"
	// importRunMaxSourceFiles/importRunMaxChapters/importRunMaxTotalBytes mirror
	// MAX_IMPORT_SOURCE_FILES, MAX_IMPORT_CHAPTERS and MAX_IMPORT_TOTAL_BYTES
	// (src/shared/import-limits.ts). MAX_DISPLAY_SOURCES is MAX_IMPORT_SOURCE_FILES,
	// so the display bound reuses it.
	importRunMaxSourceFiles int64 = 5000
	importRunMaxChapters    int64 = 5000
	importRunMaxTotalBytes  int64 = 128 * 1024 * 1024
	// importRunPreparationPreviewLimit mirrors IMPORT_PREPARATION_PREVIEW_LIMIT.
	importRunPreparationPreviewLimit int64 = 8
	// importRunFailureReasonLimit is the original's error.slice(0, 2_000), counted
	// in UTF-16 code units like JavaScript.
	importRunFailureReasonLimit int64 = 2000
)

// importRunOpaqueSourceID mirrors OPAQUE_SOURCE_ID: a UUID-shaped source identity,
// never a path.
var importRunOpaqueSourceID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// ImportRunSourceDisplay is ImportSourceDisplayMetadata: the only thing this lane
// keeps of a source file for display. The original trims both strings and refuses
// a display name that looks like a path, so nothing here is taken on trust. The
// field tags are also the stored display_json shape.
type ImportRunSourceDisplay struct {
	DisplayName string `json:"displayName"`
	MediaType   string `json:"mediaType"`
	Size        int64  `json:"size"`
}

// ImportRunBeginParsingRequest is ImportRunBeginParsingRequest
// (src/shared/import-run.ts): the inspection's frozen source facts, not its
// chapters - parsing writes sources only, and CommitParsedSource brings the text.
//
// SourceIDs and LegacySourceFingerprints are the original's optional arrays: a nil
// slice is JavaScript's undefined and a non-nil empty slice is "sent empty", which
// the original refuses. A binder over JSON must keep that distinction.
//
// LegacyCollectionFingerprint has no "absent" state: undefined becomes "" there.
type ImportRunBeginParsingRequest struct {
	RunID                       string
	Purpose                     string
	SourceFingerprint           string
	SourceIDs                   []string
	SourceFingerprints          []string
	LegacySourceFingerprints    []string
	LegacyCollectionFingerprint string
	SourceDisplay               []ImportRunSourceDisplay
	Locale                      string
}

// ImportRunChapterInput is ImportRunChapterInput (src/shared/import-run.ts): one
// parsed chapter of one source. A zero SourceIndex is the original's `?? 0` and a
// zero SourceChapterNumber is its `?? number`; both are then validated, so no
// legal chapter is lost to the default.
type ImportRunChapterInput struct {
	Number              int64
	SourceIndex         int64
	SourceChapterNumber int64
	Title               string
	Content             string
	ContentFingerprint  string
	ContentSize         int64
}

// importRunParsingRequest is BeginParsing's normalized request, so the two re-entry
// branches can share it instead of re-validating the caller's arrays.
type importRunParsingRequest struct {
	purpose            string
	sourceFingerprint  string
	sourceIDs          []string
	sourceFingerprints []string
	legacyFingerprints []string
	legacyCollection   string
	sourceDisplay      []ImportRunSourceDisplay
	locale             string
}

// importRunNormalizedChapter is NormalizedImportRunChapter: a request chapter with
// the affiliation normalizeChapters resolved and the stable number
// assignStableChapterNumbers assigns.
type importRunNormalizedChapter struct {
	number              int64
	sourceIndex         int64
	sourceID            string
	sourceChapterNumber int64
	title               string
	contentFingerprint  string
	contentSize         int64
	content             string
}

// importRunParsedSource is one import_run_sources row this lane reads: the
// affiliation it froze, the display facts a re-inspection may refresh, and the
// parse bookkeeping finalizeParsing sums up.
type importRunParsedSource struct {
	sourceIndex             int64
	sourceID                string
	sourceFingerprint       string
	legacySourceFingerprint string
	displayJSON             string
	status                  string
	manifestFingerprint     string
	chapterCount            int64
	contentSize             int64
	wordCount               int64
	lastError               string
}

// importRunSourceChapterMeta is one row of finalizeParsing's metadata query: the
// frozen facts of a parsed chapter, in the order the stable numbering is derived
// from.
type importRunSourceChapterMeta struct {
	sourceIndex         int64
	sourceID            string
	sourceChapterNumber int64
	title               string
	contentFingerprint  string
	contentSize         int64
	wordCount           int64
}

// importRunLegacyChapter is one frozen chapter of a legacy run that
// adoptLegacyCompletedRun re-attributes.
type importRunLegacyChapter struct {
	sourceChapterNumber int64
	title               string
	contentFingerprint  string
	contentSize         int64
}

// importRunChapterMapping is one row assignStableChapterNumbers still has to
// insert into import_source_chapter_map.
type importRunChapterMapping struct {
	sourceID            string
	sourceChapterNumber int64
	chapterNumber       int64
}

// importRunPreviewChapter is one row of createPreparationInspection's preview: the
// first eight chapters, each with the disposition the confirmation dialog colours.
type importRunPreviewChapter struct {
	number      int64
	title       string
	wordCount   int64
	contentSize int64
}

// importRunManifestChapter is one element of canonicalManifest's payload. The field
// order is the object literal order in the TypeScript, so the marshaled bytes - and
// therefore the fingerprint - match it.
type importRunManifestChapter struct {
	Number              int64  `json:"number"`
	SourceID            string `json:"sourceId"`
	SourceChapterNumber int64  `json:"sourceChapterNumber"`
	Title               string `json:"title"`
	ContentFingerprint  string `json:"contentFingerprint"`
	ContentSize         int64  `json:"contentSize"`
}

type importRunManifest struct {
	Purpose  string                     `json:"purpose"`
	Chapters []importRunManifestChapter `json:"chapters"`
}

// importRunUTF16Length is the JavaScript `.length` of value: the original bounds
// run ids, display names, chapter titles and a failure reason in UTF-16 code units,
// not bytes.
func importRunUTF16Length(value string) int64 {
	length := int64(0)
	for _, r := range value {
		if r > 0xFFFF {
			length += 2
			continue
		}
		length++
	}
	return length
}

// importRunTruncateUTF16 is String.prototype.slice(0, limit) for a positive limit.
// It cuts on a code point boundary instead of emitting an unpaired surrogate the
// way the original can, because a lone surrogate is not text this database can hold.
func importRunTruncateUTF16(value string, limit int64) string {
	if importRunUTF16Length(value) <= limit {
		return value
	}
	length := int64(0)
	for index, r := range value {
		width := int64(1)
		if r > 0xFFFF {
			width = 2
		}
		if length+width > limit {
			return value[:index]
		}
		length += width
	}
	return value
}

// importRunEffectNamespace is effectNamespace: the per-run effect key prefix.
func importRunEffectNamespace(purpose, runID string) string {
	return "import:" + purpose + ":" + runID
}

// importRunSourceChapterKey is sourceChapterKey: the affiliation a chapter keeps
// across sources, in the decimal form the original's template literal produces.
func importRunSourceChapterKey(sourceID string, sourceChapterNumber int64) string {
	return sourceID + ":" + strconv.FormatInt(sourceChapterNumber, 10)
}

// importRunParseDisplay is parseJson<ImportSourceDisplayMetadata>(raw,
// {displayName:”,mediaType:”,size:0}): a row that does not decode is served as
// the empty display the original falls back to.
func importRunParseDisplay(raw string) ImportRunSourceDisplay {
	display := ImportRunSourceDisplay{}
	if err := json.Unmarshal([]byte(raw), &display); err != nil {
		return ImportRunSourceDisplay{}
	}
	return display
}

// importRunNormalizeDisplay is normalizeDisplay: one trimmed display record per
// source, with the original's bounds.
func importRunNormalizeDisplay(items []ImportRunSourceDisplay) ([]ImportRunSourceDisplay, error) {
	if len(items) == 0 || int64(len(items)) > importRunMaxSourceFiles {
		return nil, errors.New("IMPORT_RUN_SOURCE_DISPLAY_INVALID")
	}
	normalized := make([]ImportRunSourceDisplay, 0, len(items))
	for _, item := range items {
		displayName := strings.TrimSpace(item.DisplayName)
		mediaType := strings.TrimSpace(item.MediaType)
		if displayName == "" || importRunUTF16Length(displayName) > 255 || strings.ContainsAny(displayName, `/\`) {
			return nil, errors.New("IMPORT_RUN_SOURCE_DISPLAY_INVALID")
		}
		if mediaType == "" || importRunUTF16Length(mediaType) > 100 || item.Size < 0 {
			return nil, errors.New("IMPORT_RUN_SOURCE_DISPLAY_INVALID")
		}
		normalized = append(normalized, ImportRunSourceDisplay{DisplayName: displayName, MediaType: mediaType, Size: item.Size})
	}
	return normalized, nil
}

// importRunNormalizeSourceIDs is normalizeSourceIds. A nil slice is undefined and
// takes the single legacy identity the original mints from the collection
// fingerprint; a supplied array must be one opaque, unique id per display record.
func importRunNormalizeSourceIDs(items []string, sourceDisplay []ImportRunSourceDisplay, sourceFingerprint string) ([]string, error) {
	if items == nil {
		return []string{"legacy:" + sourceFingerprint}, nil
	}
	if len(items) == 0 || len(items) != len(sourceDisplay) {
		return nil, errors.New("IMPORT_RUN_SOURCE_IDS_INVALID")
	}
	normalized := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		trimmed := strings.TrimSpace(item)
		if !importRunOpaqueSourceID.MatchString(trimmed) || seen[trimmed] {
			return nil, errors.New("IMPORT_RUN_SOURCE_IDS_INVALID")
		}
		seen[trimmed] = true
		normalized = append(normalized, trimmed)
	}
	return normalized, nil
}

// importRunNormalizeSourceFingerprints is normalizeSourceFingerprints: undefined
// answers an empty list, which the caller's arity check then refuses.
func importRunNormalizeSourceFingerprints(items []string, sourceIDs []string) ([]string, error) {
	if items == nil {
		return []string{}, nil
	}
	if len(items) != len(sourceIDs) {
		return nil, errors.New("IMPORT_RUN_SOURCE_FINGERPRINT_INVALID")
	}
	for _, item := range items {
		if !importRunSHA256.MatchString(item) {
			return nil, errors.New("IMPORT_RUN_SOURCE_FINGERPRINT_INVALID")
		}
	}
	return items, nil
}

// importRunNormalizeChapters is normalizeChapters: every chapter is checked against
// its own frozen bytes before any row is written, and the aggregate is bounded.
func importRunNormalizeChapters(items []ImportRunChapterInput, sourceIDs []string) ([]importRunNormalizedChapter, error) {
	if len(items) == 0 || int64(len(items)) > importRunMaxChapters {
		return nil, errors.New("IMPORT_RUN_CHAPTERS_INVALID")
	}
	seen := make(map[string]bool, len(items))
	aggregateBytes := int64(0)
	normalized := make([]importRunNormalizedChapter, 0, len(items))
	for _, item := range items {
		sourceIndex := item.SourceIndex
		sourceChapterNumber := item.SourceChapterNumber
		if sourceChapterNumber == 0 {
			sourceChapterNumber = item.Number
		}
		if item.Number < 1 || sourceIndex < 0 || sourceIndex >= int64(len(sourceIDs)) || sourceChapterNumber < 1 {
			return nil, errors.New("IMPORT_RUN_CHAPTERS_INVALID")
		}
		sourceID := sourceIDs[sourceIndex]
		affiliation := importRunSourceChapterKey(sourceID, sourceChapterNumber)
		if seen[affiliation] {
			return nil, errors.New("IMPORT_RUN_CHAPTERS_INVALID")
		}
		seen[affiliation] = true
		bytes := int64(len(item.Content))
		if importRunUTF16Length(item.Title) > 500 || !importRunSHA256.MatchString(item.ContentFingerprint) ||
			item.ContentSize != bytes || bytes == 0 || bytes > importRunMaxChapterBytes {
			return nil, errors.New("IMPORT_RUN_CHAPTERS_INVALID")
		}
		aggregateBytes += bytes
		if aggregateBytes > importRunMaxTotalBytes {
			return nil, errors.New("IMPORT_RUN_CONTENT_TOO_LARGE")
		}
		normalized = append(normalized, importRunNormalizedChapter{number: item.Number, sourceIndex: sourceIndex,
			sourceID: sourceID, sourceChapterNumber: sourceChapterNumber, title: strings.TrimSpace(item.Title),
			contentFingerprint: item.ContentFingerprint, contentSize: item.ContentSize, content: item.Content})
	}
	return normalized, nil
}

// importRunManifestFingerprint is hashManifest: the sha256 of canonicalManifest, the
// frozen {purpose, chapters} document every later comparison is made against.
func importRunManifestFingerprint(purpose string, chapters []importRunNormalizedChapter) (string, error) {
	payload := importRunManifest{Purpose: purpose, Chapters: make([]importRunManifestChapter, 0, len(chapters))}
	for _, chapter := range chapters {
		payload.Chapters = append(payload.Chapters, importRunManifestChapter{Number: chapter.number,
			SourceID: chapter.sourceID, SourceChapterNumber: chapter.sourceChapterNumber, Title: chapter.title,
			ContentFingerprint: chapter.contentFingerprint, ContentSize: chapter.contentSize})
	}
	encoded, err := jsonMarshalCompact(payload)
	if err != nil {
		return "", err
	}
	// JSON.stringify emits the two line separators raw where the Go encoder
	// escapes them; unescaping them again keeps the digest identical.
	encoded = strings.ReplaceAll(encoded, `\u2028`, "\u2028")
	encoded = strings.ReplaceAll(encoded, `\u2029`, "\u2029")
	sum := sha256.Sum256([]byte(encoded))
	return hex.EncodeToString(sum[:]), nil
}

// importRunReadSources is the parsing lane's import_run_sources read, ordered the
// way the original's `ORDER BY source_index` returns them.
func importRunReadSources(ctx context.Context, q importRunQueryer, runID string) ([]importRunParsedSource, error) {
	rows, err := q.QueryContext(ctx, `SELECT source_index, source_id, source_fingerprint,
 legacy_source_fingerprint, display_json, status, manifest_fingerprint, chapter_count, content_size, word_count, last_error
 FROM import_run_sources WHERE run_id = ? ORDER BY source_index`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sources := make([]importRunParsedSource, 0)
	for rows.Next() {
		var source importRunParsedSource
		if err := rows.Scan(&source.sourceIndex, &source.sourceID, &source.sourceFingerprint,
			&source.legacySourceFingerprint, &source.displayJSON, &source.status, &source.manifestFingerprint,
			&source.chapterCount, &source.contentSize, &source.wordCount, &source.lastError); err != nil {
			return nil, err
		}
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return sources, nil
}

// importRunSourceChapterMetadata is finalizeParsing's metadata query: every frozen
// chapter of every source, ordered by the source's own index and then the chapter's
// number, which is the order the stable numbering is derived from.
func importRunSourceChapterMetadata(ctx context.Context, q importRunQueryer, runID string) ([]importRunSourceChapterMeta, error) {
	rows, err := q.QueryContext(ctx, `SELECT sources.source_index, chapters.source_id,
 chapters.source_chapter_number, chapters.title, chapters.content_fingerprint, chapters.content_size,
 chapters.word_count
 FROM import_run_source_chapters AS chapters
 JOIN import_run_sources AS sources
   ON sources.run_id = chapters.run_id AND sources.source_id = chapters.source_id
 WHERE chapters.run_id = ?
 ORDER BY sources.source_index, chapters.source_chapter_number`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	metadata := make([]importRunSourceChapterMeta, 0)
	for rows.Next() {
		var chapter importRunSourceChapterMeta
		if err := rows.Scan(&chapter.sourceIndex, &chapter.sourceID, &chapter.sourceChapterNumber, &chapter.title,
			&chapter.contentFingerprint, &chapter.contentSize, &chapter.wordCount); err != nil {
			return nil, err
		}
		metadata = append(metadata, chapter)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return metadata, nil
}

// importRunFilterChapters is the original's normalized.filter(chapter =>
// chapter.sourceIndex === index): the adoption step matches a source by its
// position in the ordered source list, not by the stored source_index.
func importRunFilterChapters(chapters []importRunNormalizedChapter, sourceIndex int64) []importRunNormalizedChapter {
	filtered := make([]importRunNormalizedChapter, 0, len(chapters))
	for _, chapter := range chapters {
		if chapter.sourceIndex == sourceIndex {
			filtered = append(filtered, chapter)
		}
	}
	return filtered
}

// importRunAssignStableChapterNumbers is assignStableChapterNumbers: a chapter keeps
// the number a previous import already mapped it to, and only an unseen affiliation
// consumes the next one. New mappings are returned, not written, so the caller
// stays in one statement order.
func importRunAssignStableChapterNumbers(ctx context.Context, q importRunQueryer, purpose string, chapters []importRunNormalizedChapter) ([]importRunNormalizedChapter, []importRunChapterMapping, error) {
	var next int64
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(chapter_number), 0) FROM import_source_chapter_map WHERE purpose = ?`, purpose).Scan(&next); err != nil {
		return nil, nil, err
	}
	mappings := make([]importRunChapterMapping, 0)
	numbered := make([]importRunNormalizedChapter, 0, len(chapters))
	for _, chapter := range chapters {
		var existing int64
		err := q.QueryRowContext(ctx, `SELECT chapter_number FROM import_source_chapter_map
 WHERE purpose = ? AND source_id = ? AND source_chapter_number = ?`,
			purpose, chapter.sourceID, chapter.sourceChapterNumber).Scan(&existing)
		mapped := err == nil
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, nil, err
		}
		number := existing
		if !mapped {
			next++
			number = next
			mappings = append(mappings, importRunChapterMapping{sourceID: chapter.sourceID,
				sourceChapterNumber: chapter.sourceChapterNumber, chapterNumber: number})
		}
		chapter.number = number
		numbered = append(numbered, chapter)
	}
	sort.SliceStable(numbered, func(left, right int) bool { return numbered[left].number < numbered[right].number })
	return numbered, mappings, nil
}

// importRunAdoptLegacyCompletedRun is adoptLegacyCompletedRun: when a completed run
// of the same purpose already holds this exact chapter list under a legacy source
// identity, the old mapping and rows are re-attributed to the real source ids
// instead of being imported twice.
func importRunAdoptLegacyCompletedRun(ctx context.Context, conn *sql.Conn, purpose, legacyFingerprint string, candidates []importRunNormalizedChapter) error {
	if len(candidates) == 0 {
		return nil
	}
	legacySourceID := "legacy:" + legacyFingerprint
	var legacyRunID string
	err := conn.QueryRowContext(ctx, `SELECT id FROM import_runs
 WHERE purpose = ? AND source_fingerprint = ? AND status = 'completed'
 ORDER BY completed_at DESC, rowid DESC LIMIT 1`, purpose, legacyFingerprint).Scan(&legacyRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	legacyChapters, err := importRunLegacyChapters(ctx, conn, legacyRunID, legacySourceID)
	if err != nil {
		return err
	}
	ordered := append([]importRunNormalizedChapter(nil), candidates...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if ordered[left].number != ordered[right].number {
			return ordered[left].number < ordered[right].number
		}
		return ordered[left].sourceChapterNumber < ordered[right].sourceChapterNumber
	})
	if len(legacyChapters) != len(ordered) {
		return nil
	}
	for index, chapter := range legacyChapters {
		candidate := ordered[index]
		if chapter.title != candidate.title || chapter.contentFingerprint != candidate.contentFingerprint ||
			chapter.contentSize != candidate.contentSize {
			return nil
		}
	}
	for index, chapter := range legacyChapters {
		candidate := ordered[index]
		if _, err := conn.ExecContext(ctx, `UPDATE import_source_chapter_map
 SET source_id = ?, source_chapter_number = ?
 WHERE purpose = ? AND source_id = ? AND source_chapter_number = ?`,
			candidate.sourceID, candidate.sourceChapterNumber, purpose, legacySourceID,
			chapter.sourceChapterNumber); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE import_run_chapters
 SET source_id = ?, source_chapter_number = ?
 WHERE source_id = ? AND source_chapter_number = ?
   AND run_id IN (SELECT id FROM import_runs WHERE purpose = ? AND source_fingerprint = ?)`,
			candidate.sourceID, candidate.sourceChapterNumber, legacySourceID,
			chapter.sourceChapterNumber, purpose, legacyFingerprint); err != nil {
			return err
		}
	}
	return nil
}

// importRunLegacyChapters reads the chapters the legacy run froze under its own
// legacy source identity, ordered the way the comparison needs them.
func importRunLegacyChapters(ctx context.Context, q importRunQueryer, runID, sourceID string) ([]importRunLegacyChapter, error) {
	rows, err := q.QueryContext(ctx, `SELECT source_chapter_number, title, content_fingerprint, content_size
 FROM import_run_chapters WHERE run_id = ? AND source_id = ? ORDER BY chapter_number`, runID, sourceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chapters := make([]importRunLegacyChapter, 0)
	for rows.Next() {
		var chapter importRunLegacyChapter
		if err := rows.Scan(&chapter.sourceChapterNumber, &chapter.title, &chapter.contentFingerprint,
			&chapter.contentSize); err != nil {
			return nil, err
		}
		chapters = append(chapters, chapter)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chapters, nil
}

// importRunCompletedChapter is one entry of completedChapterManifest: what a
// completed run already committed for an affiliation.
type importRunCompletedChapter struct {
	title              string
	contentFingerprint string
	contentSize        int64
}

// importRunCompletedChapterManifest is completedChapterManifest: every chapter a
// completed run of this purpose already adopted for one of these sources, keyed by
// affiliation. A source that was never imported simply has no entries.
func importRunCompletedChapterManifest(ctx context.Context, q importRunQueryer, purpose string, sourceIDs []string) (map[string]importRunCompletedChapter, error) {
	manifest := make(map[string]importRunCompletedChapter)
	if len(sourceIDs) == 0 {
		return manifest, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(sourceIDs)), ", ")
	args := make([]any, 0, len(sourceIDs)+1)
	args = append(args, purpose)
	for _, sourceID := range sourceIDs {
		args = append(args, sourceID)
	}
	rows, err := q.QueryContext(ctx, `SELECT source_map.chapter_number, chapters.source_id,
 chapters.source_chapter_number, chapters.title, chapters.content_fingerprint, chapters.content_size
 FROM import_run_chapters AS chapters
 JOIN import_runs AS runs ON runs.id = chapters.run_id
 JOIN import_source_chapter_map AS source_map
   ON source_map.purpose = runs.purpose
   AND source_map.source_id = chapters.source_id
   AND source_map.source_chapter_number = chapters.source_chapter_number
 WHERE runs.purpose = ? AND runs.status = 'completed'
   AND chapters.source_id IN (`+placeholders+`)
 ORDER BY runs.completed_at ASC, runs.rowid ASC, chapters.chapter_number ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var chapterNumber, sourceChapterNumber, contentSize int64
		var sourceID, title, fingerprint string
		if err := rows.Scan(&chapterNumber, &sourceID, &sourceChapterNumber, &title, &fingerprint, &contentSize); err != nil {
			return nil, err
		}
		manifest[importRunSourceChapterKey(sourceID, sourceChapterNumber)] = importRunCompletedChapter{
			title: title, contentFingerprint: fingerprint, contentSize: contentSize}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return manifest, nil
}

// importRunReadRunByQuery runs one id query and then reads the whole row, so a
// single-row scan of the 29 columns exists only once in the package.
func importRunReadRunByQuery(ctx context.Context, q importRunQueryer, query string, args ...any) (importRunRow, bool, error) {
	var runID string
	err := q.QueryRowContext(ctx, query, args...).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return importRunRow{}, false, nil
	}
	if err != nil {
		return importRunRow{}, false, err
	}
	return readImportRunRow(ctx, q, runID)
}

// importRunMatchingParsingRun is the parsing run near-duplicate lookup of
// beginParsing: the same purpose and collection fingerprint, still parsing, still
// resumable, most recently touched first.
func importRunMatchingParsingRun(ctx context.Context, q importRunQueryer, purpose, sourceFingerprint string) (importRunRow, bool, error) {
	return importRunReadRunByQuery(ctx, q, `SELECT id FROM import_runs
 WHERE purpose = ? AND source_fingerprint = ? AND stage = 'parsing' AND resumable = 1
 ORDER BY updated_at DESC, rowid DESC LIMIT 1`, purpose, sourceFingerprint)
}

// importRunMatchingResumableRun is matchingResumableRun: a run of the same purpose
// and fingerprints that already left parsing and can still be resumed.
func importRunMatchingResumableRun(ctx context.Context, q importRunQueryer, purpose, sourceFingerprint, manifestFingerprint string) (importRunRow, bool, error) {
	return importRunReadRunByQuery(ctx, q, `SELECT id FROM import_runs
 WHERE purpose = ? AND source_fingerprint = ? AND manifest_fingerprint = ?
   AND stage <> 'parsing' AND resumable = 1
   AND status IN ('ready', 'running', 'failed', 'cancelled')
 ORDER BY created_at DESC, rowid DESC LIMIT 1`, purpose, sourceFingerprint, manifestFingerprint)
}

// importRunLatestCompletedRun is latestCompletedRun: the run whose committed
// chapters this import becomes the incremental continuation of.
func importRunLatestCompletedRun(ctx context.Context, q importRunQueryer, purpose, sourceFingerprint string) (importRunRow, bool, error) {
	return importRunReadRunByQuery(ctx, q, `SELECT id FROM import_runs
 WHERE purpose = ? AND source_fingerprint = ? AND status = 'completed'
 ORDER BY completed_at DESC, rowid DESC LIMIT 1`, purpose, sourceFingerprint)
}

// importRunOverlappingResumableSourceRun is overlappingResumableSourceRun: any other
// resumable run past parsing that already froze one of this run's sources.
func importRunOverlappingResumableSourceRun(ctx context.Context, q importRunQueryer, runID, purpose string) (string, bool, error) {
	var otherID string
	err := q.QueryRowContext(ctx, `SELECT DISTINCT other_runs.id
 FROM import_runs AS other_runs
 JOIN import_run_sources AS other_sources ON other_sources.run_id = other_runs.id
 JOIN import_run_sources AS current_sources
   ON current_sources.run_id = ?
   AND current_sources.source_fingerprint = other_sources.source_fingerprint
 WHERE other_runs.id <> ?
   AND other_runs.purpose = ?
   AND other_runs.stage <> 'parsing'
   AND other_runs.resumable = 1
   AND other_runs.status IN ('ready', 'running', 'failed', 'cancelled')
 ORDER BY other_runs.updated_at DESC, other_runs.rowid DESC
 LIMIT 1`, runID, runID, purpose).Scan(&otherID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return otherID, true, nil
}

// importRunDiscardProvisionalParsingRun is discardProvisionalParsingRun: a parsing
// run that turned out to be a duplicate or a conflict owns nothing, so it is
// dropped whole and the cascade takes its sources and chapters with it.
func importRunDiscardProvisionalParsingRun(ctx context.Context, conn *sql.Conn, runID string) error {
	result, err := conn.ExecContext(ctx, `DELETE FROM import_runs
 WHERE id = ? AND stage = 'parsing' AND status IN ('ready', 'failed')`, runID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return errors.New("IMPORT_RUN_PARSING_DISCARD_FAILED")
	}
	return nil
}

// importRunPreparationInspection is createPreparationInspection: the bounded summary
// the confirmation dialog shows, derived only from rows the transaction read.
func importRunPreparationInspection(inspectionID, purpose string, sourceDisplay []ImportRunSourceDisplay, chapters []importRunPreviewChapter, newNumbers, conflictNumbers, duplicateNumbers []int64) map[string]any {
	conflicts := make(map[int64]bool, len(conflictNumbers))
	for _, number := range conflictNumbers {
		conflicts[number] = true
	}
	duplicates := make(map[int64]bool, len(duplicateNumbers))
	for _, number := range duplicateNumbers {
		duplicates[number] = true
	}
	fresh := make(map[int64]bool, len(newNumbers))
	for _, number := range newNumbers {
		fresh[number] = true
	}
	preview := make([]any, 0, importRunPreparationPreviewLimit)
	for index, chapter := range chapters {
		if int64(index) >= importRunPreparationPreviewLimit {
			break
		}
		targetStatus := "duplicate"
		if conflicts[chapter.number] {
			targetStatus = "conflict"
		} else if duplicates[chapter.number] {
			targetStatus = "duplicate"
		} else if fresh[chapter.number] {
			targetStatus = "new"
		}
		preview = append(preview, map[string]any{"number": chapter.number, "title": chapter.title,
			"wordCount": chapter.wordCount, "targetStatus": targetStatus})
	}
	displayNames := make([]string, 0, len(sourceDisplay))
	totalWords := int64(0)
	totalBytes := int64(0)
	for _, source := range sourceDisplay {
		displayNames = append(displayNames, source.DisplayName)
	}
	for _, chapter := range chapters {
		totalWords += chapter.wordCount
		totalBytes += chapter.contentSize
	}
	return map[string]any{
		"inspectionId":       inspectionID,
		"purpose":            purpose,
		"sourceCount":        int64(len(sourceDisplay)),
		"sourceDisplayNames": displayNames,
		"chapterCount":       int64(len(chapters)),
		"totalWords":         totalWords,
		"totalBytes":         totalBytes,
		"preview":            preview,
		"previewRemaining":   int64(len(chapters) - len(preview)),
	}
}

// BeginParsing is db:import-run-prepare-inspection's first write: it creates the
// parsing run and one source row per inspected source.
//
// It re-enters an existing run instead of creating a second one, and only when the
// request describes exactly the sources that run already froze:
//   - the exact run id this request names, still parsing and resumable, refreshes
//     that run's display facts and legacy identities (:1590-1644);
//   - otherwise the newest parsing run of the same purpose and collection
//     fingerprint, whose sources must match one for one, is re-indexed in place
//     (:1647-1689). The unique (run_id, source_index) index makes an in-place
//     rewrite impossible, so every index is first parked one page beyond the table.
//
// Only the reference purpose reaches this lane; see this file's header for the
// author purpose.
func (r *ImportRuns) BeginParsing(ctx context.Context, candidate ImportRunBeginParsingRequest) (map[string]any, error) {
	runID := strings.TrimSpace(candidate.RunID)
	if runID == "" || importRunUTF16Length(runID) > 160 || !importRunSHA256.MatchString(candidate.SourceFingerprint) {
		return nil, errors.New("IMPORT_RUN_ID_INVALID")
	}
	if candidate.Purpose != "reference" {
		return nil, errors.New("IMPORT_RUN_PURPOSE_INVALID")
	}
	if candidate.Locale != "zh-CN" && candidate.Locale != "en-US" {
		return nil, errors.New("IMPORT_RUN_LOCALE_INVALID")
	}
	display, err := importRunNormalizeDisplay(candidate.SourceDisplay)
	if err != nil {
		return nil, err
	}
	sourceIDs, err := importRunNormalizeSourceIDs(candidate.SourceIDs, display, candidate.SourceFingerprint)
	if err != nil {
		return nil, err
	}
	fingerprints, err := importRunNormalizeSourceFingerprints(candidate.SourceFingerprints, sourceIDs)
	if err != nil {
		return nil, err
	}
	if len(fingerprints) != len(sourceIDs) {
		return nil, errors.New("IMPORT_RUN_SOURCE_FINGERPRINT_INVALID")
	}
	legacyFingerprints := candidate.LegacySourceFingerprints
	if legacyFingerprints == nil {
		legacyFingerprints = make([]string, len(sourceIDs))
	}
	if len(legacyFingerprints) != len(sourceIDs) {
		return nil, errors.New("IMPORT_RUN_SOURCE_FINGERPRINT_INVALID")
	}
	for _, legacyFingerprint := range legacyFingerprints {
		if legacyFingerprint != "" && !importRunSHA256.MatchString(legacyFingerprint) {
			return nil, errors.New("IMPORT_RUN_SOURCE_FINGERPRINT_INVALID")
		}
	}
	if candidate.LegacyCollectionFingerprint != "" && !importRunSHA256.MatchString(candidate.LegacyCollectionFingerprint) {
		return nil, errors.New("IMPORT_RUN_SOURCE_FINGERPRINT_INVALID")
	}
	request := importRunParsingRequest{purpose: candidate.Purpose, sourceFingerprint: candidate.SourceFingerprint,
		sourceIDs: sourceIDs, sourceFingerprints: fingerprints, legacyFingerprints: legacyFingerprints,
		legacyCollection: candidate.LegacyCollectionFingerprint, sourceDisplay: display, locale: candidate.Locale}
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response map[string]any
	err = r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if present {
			response, err = importRunReauthorizeParsing(ctx, conn, row, request)
			return err
		}
		existing, found, err := importRunMatchingParsingRun(ctx, conn, request.purpose, request.sourceFingerprint)
		if err != nil {
			return err
		}
		if found {
			response, err = importRunReindexParsing(ctx, conn, existing, request)
			return err
		}
		// The original asks again whether this id is taken (:1690). The explicit
		// row above already answered for it, so this refusal cannot fire; it stays
		// because the insert keeps the original's own guard.
		if _, taken, err := readImportRunRow(ctx, conn, runID); err != nil {
			return err
		} else if taken {
			return errors.New("IMPORT_RUN_ID_CONFLICT")
		}
		displayJSON, err := jsonMarshalCompact(request.sourceDisplay)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO import_runs (
 id, purpose, root_run_id, effect_namespace, source_fingerprint, manifest_fingerprint, legacy_source_fingerprint,
 source_display_json, locale, stage, status, total_chapters, total_content_size,
 manifest_chapter_count, manifest_content_size, manifest_word_count, completed_chapters
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'parsing', 'ready', 0, 0, 0, 0, 0, 0)`,
			runID, request.purpose, runID, importRunEffectNamespace(request.purpose, runID),
			request.sourceFingerprint, importRunZeroFingerprint, request.legacyCollection,
			displayJSON, request.locale); err != nil {
			return err
		}
		for index, sourceID := range request.sourceIDs {
			sourceDisplayJSON, err := jsonMarshalCompact(request.sourceDisplay[index])
			if err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO import_run_sources (
 run_id, source_index, source_id, source_fingerprint, legacy_source_fingerprint, display_json
) VALUES (?, ?, ?, ?, ?, ?)`, runID, int64(index), sourceID, request.sourceFingerprints[index],
				request.legacyFingerprints[index], sourceDisplayJSON); err != nil {
				return err
			}
		}
		created, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		response, err = importRunSnapshot(ctx, conn, created)
		return err
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// importRunReauthorizeParsing is beginParsing's exact-id re-entry (:1590-1644): the
// run must still be the same purpose, stage, lease and language, and every source it
// already completed is frozen - only the display facts and legacy identities of the
// unfinished ones may be refreshed.
func importRunReauthorizeParsing(ctx context.Context, conn *sql.Conn, row importRunRow, request importRunParsingRequest) (map[string]any, error) {
	if row.Purpose != request.purpose || row.Stage != "parsing" || row.Resumable != 1 || row.Locale != request.locale {
		return nil, errors.New("IMPORT_RUN_REAUTHORIZE_UNAVAILABLE")
	}
	sources, err := importRunReadSources(ctx, conn, row.ID)
	if err != nil {
		return nil, err
	}
	positions := make(map[string]int, len(sources))
	for position, source := range sources {
		positions[source.sourceID] = position
	}
	for index, sourceID := range request.sourceIDs {
		position, ok := positions[sourceID]
		if !ok || sources[position].status == "completed" ||
			sources[position].sourceFingerprint != request.sourceFingerprints[index] {
			return nil, errors.New("IMPORT_RUN_REAUTHORIZE_SOURCES_MISMATCH")
		}
	}
	for index, sourceID := range request.sourceIDs {
		displayJSON, err := jsonMarshalCompact(request.sourceDisplay[index])
		if err != nil {
			return nil, err
		}
		result, err := conn.ExecContext(ctx, `UPDATE import_run_sources
 SET legacy_source_fingerprint = ?, display_json = ?, updated_at = datetime('now')
 WHERE run_id = ? AND source_id = ? AND source_fingerprint = ? AND status <> 'completed'`,
			request.legacyFingerprints[index], displayJSON, row.ID, sourceID, request.sourceFingerprints[index])
		if err != nil {
			return nil, err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return nil, err
		} else if changed != 1 {
			return nil, errors.New("IMPORT_RUN_REAUTHORIZE_SOURCES_MISMATCH")
		}
		sources[positions[sourceID]].legacySourceFingerprint = request.legacyFingerprints[index]
		sources[positions[sourceID]].displayJSON = displayJSON
	}
	// The run's own display list keeps every source it holds, in index order, with
	// the refreshed rows already folded in.
	fullDisplay := make([]ImportRunSourceDisplay, 0, len(sources))
	for _, source := range sources {
		fullDisplay = append(fullDisplay, importRunParseDisplay(source.displayJSON))
	}
	encoded, err := jsonMarshalCompact(fullDisplay)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE import_runs SET source_display_json = ?, updated_at = datetime('now') WHERE id = ?`,
		encoded, row.ID); err != nil {
		return nil, err
	}
	updated, present, err := readImportRunRow(ctx, conn, row.ID)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, errors.New("IMPORT_RUN_STATE_CONFLICT")
	}
	return importRunSnapshot(ctx, conn, updated)
}

// importRunReindexParsing is beginParsing's near-duplicate re-entry (:1647-1689): the
// run already froze exactly these sources under the same collection fingerprint, so
// re-selecting the same files must reuse it rather than mint a second run.
func importRunReindexParsing(ctx context.Context, conn *sql.Conn, row importRunRow, request importRunParsingRequest) (map[string]any, error) {
	sources, err := importRunReadSources(ctx, conn, row.ID)
	if err != nil {
		return nil, err
	}
	if len(sources) != len(request.sourceIDs) {
		return nil, errors.New("IMPORT_RUN_REAUTHORIZE_SOURCES_MISMATCH")
	}
	byID := make(map[string]importRunParsedSource, len(sources))
	for _, source := range sources {
		byID[source.sourceID] = source
	}
	for index, sourceID := range request.sourceIDs {
		source, ok := byID[sourceID]
		if !ok || source.sourceFingerprint != request.sourceFingerprints[index] {
			return nil, errors.New("IMPORT_RUN_REAUTHORIZE_SOURCES_MISMATCH")
		}
	}
	if _, err := conn.ExecContext(ctx, `UPDATE import_run_sources
 SET source_index = source_index + ? WHERE run_id = ?`, importRunMaxSourceFiles, row.ID); err != nil {
		return nil, err
	}
	for index, sourceID := range request.sourceIDs {
		displayJSON, err := jsonMarshalCompact(request.sourceDisplay[index])
		if err != nil {
			return nil, err
		}
		result, err := conn.ExecContext(ctx, `UPDATE import_run_sources
 SET source_index = ?, legacy_source_fingerprint = ?, display_json = ?, updated_at = datetime('now')
 WHERE run_id = ? AND source_id = ? AND source_fingerprint = ?`,
			int64(index), request.legacyFingerprints[index], displayJSON, row.ID, sourceID,
			request.sourceFingerprints[index])
		if err != nil {
			return nil, err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return nil, err
		} else if changed != 1 {
			return nil, errors.New("IMPORT_RUN_REAUTHORIZE_SOURCES_MISMATCH")
		}
	}
	encoded, err := jsonMarshalCompact(request.sourceDisplay)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE import_runs SET source_display_json = ?, updated_at = datetime('now') WHERE id = ?`,
		encoded, row.ID); err != nil {
		return nil, err
	}
	updated, present, err := readImportRunRow(ctx, conn, row.ID)
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, errors.New("IMPORT_RUN_STATE_CONFLICT")
	}
	return importRunSnapshot(ctx, conn, updated)
}

// CommitParsedSource is db:import-run-prepare-inspection's second write: it freezes
// one source's parsed chapters, marks the source completed and re-sums the run's
// totals from the completed sources only.
//
// The re-hash is not optional: the frozen bytes must produce the fingerprint the
// caller claims, so a chapter edited in flight is refused instead of stored
// (IMPORT_RUN_PARSED_SOURCE_MISMATCH). A source that is already completed is a
// replay, and only an identical manifest may replay it.
func (r *ImportRuns) CommitParsedSource(ctx context.Context, runID, sourceID string, chapters []ImportRunChapterInput) (map[string]any, error) {
	if !importRunOpaqueSourceID.MatchString(sourceID) {
		return nil, errors.New("IMPORT_RUN_PARSED_SOURCE_INVALID")
	}
	if len(chapters) == 0 {
		return nil, errors.New("IMPORT_RUN_PARSED_SOURCE_EMPTY")
	}
	for _, chapter := range chapters {
		sum := sha256.Sum256([]byte(chapter.Content))
		if hex.EncodeToString(sum[:]) != chapter.ContentFingerprint {
			return nil, errors.New("IMPORT_RUN_PARSED_SOURCE_MISMATCH")
		}
	}
	normalized, err := importRunNormalizeChapters(chapters, []string{sourceID})
	if err != nil {
		return nil, err
	}
	// One source's manifest is hashed over its chapters renumbered from 1, which is
	// the shape a later finalizeParsing compares against.
	numbered := make([]importRunNormalizedChapter, 0, len(normalized))
	for index, chapter := range normalized {
		chapter.number = int64(index) + 1
		numbered = append(numbered, chapter)
	}
	manifestFingerprint, err := importRunManifestFingerprint("reference", numbered)
	if err != nil {
		return nil, err
	}
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response map[string]any
	err = r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present || row.Purpose != "reference" || row.Stage != "parsing" ||
			(row.Status != "ready" && row.Status != "failed") {
			return errors.New("IMPORT_RUN_PARSING_CLOSED")
		}
		var sourceStatus, sourceManifest string
		err = conn.QueryRowContext(ctx, `SELECT status, manifest_fingerprint FROM import_run_sources
 WHERE run_id = ? AND source_id = ?`, runID, sourceID).Scan(&sourceStatus, &sourceManifest)
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("IMPORT_RUN_PARSED_SOURCE_INVALID")
		}
		if err != nil {
			return err
		}
		if sourceStatus == "completed" {
			if sourceManifest != manifestFingerprint {
				return errors.New("IMPORT_RUN_REAUTHORIZE_CONTENT_CHANGED")
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
		}
		if _, err := conn.ExecContext(ctx, `DELETE FROM import_run_source_chapters WHERE run_id = ? AND source_id = ?`,
			runID, sourceID); err != nil {
			return err
		}
		contentSize := int64(0)
		wordCount := int64(0)
		for _, chapter := range normalized {
			words := int64(draftalg.CountDraftUnits(chapter.content))
			contentSize += chapter.contentSize
			wordCount += words
			if _, err := conn.ExecContext(ctx, `INSERT INTO import_run_source_chapters (
 run_id, source_id, source_chapter_number, title, content_fingerprint, content_size, word_count, content_snapshot
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, runID, sourceID, chapter.sourceChapterNumber, chapter.title,
				chapter.contentFingerprint, chapter.contentSize, words, chapter.content); err != nil {
				return err
			}
		}
		if _, err := conn.ExecContext(ctx, `UPDATE import_run_sources
 SET status = 'completed', manifest_fingerprint = ?, chapter_count = ?, content_size = ?, word_count = ?,
     last_error = '', updated_at = datetime('now')
 WHERE run_id = ? AND source_id = ?`, manifestFingerprint, int64(len(normalized)), contentSize,
			wordCount, runID, sourceID); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE import_runs
 SET status = 'ready', last_error = '',
     total_chapters = (SELECT COALESCE(SUM(chapter_count), 0) FROM import_run_sources WHERE run_id = ? AND status = 'completed'),
     total_content_size = (SELECT COALESCE(SUM(content_size), 0) FROM import_run_sources WHERE run_id = ? AND status = 'completed'),
     updated_at = datetime('now')
 WHERE id = ?`, runID, runID, runID); err != nil {
			return err
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

// FailParsedSource is db:import-run-prepare-inspection's failure write: one source
// that could not be parsed is marked failed with the reason the caller already
// localized, and the run becomes a resumable failure. A source that is already
// completed is untouched, so a late failure never rewrites adopted text.
func (r *ImportRuns) FailParsedSource(ctx context.Context, runID, sourceID string, reason any) (map[string]any, error) {
	text, ok := reason.(string)
	if !ok || strings.TrimSpace(text) == "" {
		return nil, errors.New("IMPORT_RUN_FAILURE_REASON_INVALID")
	}
	stored := importRunTruncateUTF16(text, importRunFailureReasonLimit)
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response map[string]any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present || row.Stage != "parsing" || (row.Status != "ready" && row.Status != "failed") {
			return errors.New("IMPORT_RUN_PARSING_CLOSED")
		}
		result, err := conn.ExecContext(ctx, `UPDATE import_run_sources
 SET status = 'failed', last_error = ?, updated_at = datetime('now')
 WHERE run_id = ? AND source_id = ? AND status <> 'completed'`, stored, runID, sourceID)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return errors.New("IMPORT_RUN_PARSING_CLOSED")
		}
		if _, err := conn.ExecContext(ctx, `UPDATE import_runs
 SET status = 'failed', last_error = ?, resumable = 1, updated_at = datetime('now')
 WHERE id = ?`, stored, runID); err != nil {
			return err
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

// FinalizeParsing is db:import-run-finalize-parsing, and the last step of
// db:import-run-prepare-inspection: it turns a fully parsed run into a prepared one,
// adopting every chapter that a completed run has not already imported.
//
// It is NOT idempotent, because the original is not: the first call moves the run
// off 'parsing', so a replay is refused with IMPORT_RUN_PARSING_CLOSED rather than
// answering the same preparation twice.
//
// Statements, in order: BEGIN IMMEDIATE; the run row; its sources; their frozen
// chapters; the legacy adoptions (which re-attribute old mappings); the stable
// numbering; the resumable-run probe; the overlapping-source probe; the latest
// completed run and the chapter manifest it committed; then exactly one of - a
// delete of this provisional run (resumable duplicate or conflict), a delete for an
// exact duplicate, or the mapping inserts, the chapter copies and the prepared
// update. The response omits run in the two branches where the original leaves it
// undefined: a JSON envelope cannot carry undefined.
func (r *ImportRuns) FinalizeParsing(ctx context.Context, runID string) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response map[string]any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		if !present || row.Stage != "parsing" || (row.Status != "ready" && row.Status != "failed") {
			return errors.New("IMPORT_RUN_PARSING_CLOSED")
		}
		sources, err := importRunReadSources(ctx, conn, runID)
		if err != nil {
			return err
		}
		if len(sources) == 0 {
			return errors.New("IMPORT_RUN_PARSING_INCOMPLETE")
		}
		declaredChapters := int64(0)
		declaredBytes := int64(0)
		declaredWords := int64(0)
		for _, source := range sources {
			if source.status != "completed" {
				return errors.New("IMPORT_RUN_PARSING_INCOMPLETE")
			}
			declaredChapters += source.chapterCount
			declaredBytes += source.contentSize
			declaredWords += source.wordCount
		}
		metadata, err := importRunSourceChapterMetadata(ctx, conn, runID)
		if err != nil {
			return err
		}
		if len(metadata) == 0 || int64(len(metadata)) > importRunMaxChapters ||
			int64(len(metadata)) != declaredChapters || declaredBytes > importRunMaxTotalBytes {
			return errors.New("IMPORT_RUN_MANIFEST_INVALID")
		}
		normalized := make([]importRunNormalizedChapter, 0, len(metadata))
		for index, chapter := range metadata {
			normalized = append(normalized, importRunNormalizedChapter{number: int64(index) + 1,
				sourceIndex: chapter.sourceIndex, sourceID: chapter.sourceID,
				sourceChapterNumber: chapter.sourceChapterNumber, title: chapter.title,
				contentFingerprint: chapter.contentFingerprint, contentSize: chapter.contentSize})
		}
		for sourceIndex, source := range sources {
			if source.legacySourceFingerprint == "" {
				continue
			}
			if err := importRunAdoptLegacyCompletedRun(ctx, conn, row.Purpose, source.legacySourceFingerprint,
				importRunFilterChapters(normalized, int64(sourceIndex))); err != nil {
				return err
			}
		}
		if row.LegacySourceFingerprint != "" {
			if err := importRunAdoptLegacyCompletedRun(ctx, conn, row.Purpose, row.LegacySourceFingerprint, normalized); err != nil {
				return err
			}
		}
		assigned, newMappings, err := importRunAssignStableChapterNumbers(ctx, conn, row.Purpose, normalized)
		if err != nil {
			return err
		}
		sourceDisplay := make([]ImportRunSourceDisplay, 0, len(sources))
		for _, source := range sources {
			sourceDisplay = append(sourceDisplay, importRunParseDisplay(source.displayJSON))
		}
		wordCounts := make(map[string]int64, len(metadata))
		for _, chapter := range metadata {
			wordCounts[importRunSourceChapterKey(chapter.sourceID, chapter.sourceChapterNumber)] = chapter.wordCount
		}
		previewChapters := make([]importRunPreviewChapter, 0, len(assigned))
		allNumbers := make([]int64, 0, len(assigned))
		for _, chapter := range assigned {
			previewChapters = append(previewChapters, importRunPreviewChapter{number: chapter.number,
				title:       chapter.title,
				wordCount:   wordCounts[importRunSourceChapterKey(chapter.sourceID, chapter.sourceChapterNumber)],
				contentSize: chapter.contentSize})
			allNumbers = append(allNumbers, chapter.number)
		}
		manifestFingerprint, err := importRunManifestFingerprint(row.Purpose, assigned)
		if err != nil {
			return err
		}
		resumable, resumableFound, err := importRunMatchingResumableRun(ctx, conn, row.Purpose,
			row.SourceFingerprint, manifestFingerprint)
		if err != nil {
			return err
		}
		if resumableFound && resumable.ID != runID {
			if err := importRunDiscardProvisionalParsingRun(ctx, conn, runID); err != nil {
				return err
			}
			snapshot, err := importRunSnapshot(ctx, conn, resumable)
			if err != nil {
				return err
			}
			inspection := importRunPreparationInspection(runID, row.Purpose, sourceDisplay, previewChapters,
				make([]int64, 0), make([]int64, 0), allNumbers)
			response = map[string]any{
				"classification":          "resumable",
				"run":                     snapshot,
				"newChapterNumbers":       make([]int64, 0),
				"conflictChapterNumbers":  make([]int64, 0),
				"duplicateChapterNumbers": allNumbers,
				"inspection":              inspection,
			}
			return nil
		}
		if _, spanningFound, err := importRunOverlappingResumableSourceRun(ctx, conn, runID, row.Purpose); err != nil {
			return err
		} else if spanningFound {
			// The original throws a sentence the renderer shows as written, in the
			// run's own language, instead of a symbolic code.
			if row.Locale == "en-US" {
				return errors.New("Another resumable import already contains the same source. Complete or cancel that import, then try again.")
			}
			return errors.New("另一个可恢复导入已包含相同来源，请先完成或取消该导入后重试")
		}
		sourceIDs := make([]string, 0, len(sources))
		for _, source := range sources {
			sourceIDs = append(sourceIDs, source.sourceID)
		}
		completed, completedFound, err := importRunLatestCompletedRun(ctx, conn, row.Purpose, row.SourceFingerprint)
		if err != nil {
			return err
		}
		completedManifest, err := importRunCompletedChapterManifest(ctx, conn, row.Purpose, sourceIDs)
		if err != nil {
			return err
		}
		conflictNumbers := make([]int64, 0)
		duplicateNumbers := make([]int64, 0)
		newChapters := make([]importRunNormalizedChapter, 0, len(assigned))
		newNumbers := make([]int64, 0, len(assigned))
		for _, chapter := range assigned {
			previous, ok := completedManifest[importRunSourceChapterKey(chapter.sourceID, chapter.sourceChapterNumber)]
			if !ok {
				newChapters = append(newChapters, chapter)
				newNumbers = append(newNumbers, chapter.number)
				continue
			}
			if previous.title != chapter.title || previous.contentFingerprint != chapter.contentFingerprint ||
				previous.contentSize != chapter.contentSize {
				conflictNumbers = append(conflictNumbers, chapter.number)
				continue
			}
			duplicateNumbers = append(duplicateNumbers, chapter.number)
		}
		if len(conflictNumbers) > 0 {
			if err := importRunDiscardProvisionalParsingRun(ctx, conn, runID); err != nil {
				return err
			}
			inspection := importRunPreparationInspection(runID, row.Purpose, sourceDisplay, previewChapters,
				newNumbers, conflictNumbers, duplicateNumbers)
			response = map[string]any{
				"classification":          "conflict",
				"newChapterNumbers":       newNumbers,
				"conflictChapterNumbers":  conflictNumbers,
				"duplicateChapterNumbers": duplicateNumbers,
				"inspection":              inspection,
			}
			return nil
		}
		if len(newChapters) == 0 {
			if _, err := conn.ExecContext(ctx, `DELETE FROM import_runs WHERE id = ?`, runID); err != nil {
				return err
			}
			inspection := importRunPreparationInspection(runID, row.Purpose, sourceDisplay, previewChapters,
				make([]int64, 0), make([]int64, 0), duplicateNumbers)
			response = map[string]any{
				"classification":          "exact-duplicate",
				"newChapterNumbers":       make([]int64, 0),
				"conflictChapterNumbers":  make([]int64, 0),
				"duplicateChapterNumbers": duplicateNumbers,
				"inspection":              inspection,
			}
			return nil
		}
		for _, mapping := range newMappings {
			if _, err := conn.ExecContext(ctx, `INSERT INTO import_source_chapter_map (
 purpose, source_id, source_chapter_number, chapter_number
) VALUES (?, ?, ?, ?)`, row.Purpose, mapping.sourceID, mapping.sourceChapterNumber,
				mapping.chapterNumber); err != nil {
				return err
			}
		}
		newContentSize := int64(0)
		for _, chapter := range newChapters {
			newContentSize += chapter.contentSize
			if _, err := conn.ExecContext(ctx, `INSERT INTO import_run_chapters (
 run_id, chapter_number, source_id, source_chapter_number,
 title, content_fingerprint, content_size, content_snapshot
)
 SELECT ?, ?, source_id, source_chapter_number, title, content_fingerprint, content_size, content_snapshot
 FROM import_run_source_chapters
 WHERE run_id = ? AND source_id = ? AND source_chapter_number = ?`, runID, chapter.number,
				runID, chapter.sourceID, chapter.sourceChapterNumber); err != nil {
				return err
			}
		}
		var baseRunID any
		if completedFound {
			baseRunID = completed.ID
		}
		if _, err := conn.ExecContext(ctx, `UPDATE import_runs
 SET stage = 'prepared', status = 'ready', manifest_fingerprint = ?,
     total_chapters = ?, total_content_size = ?, manifest_chapter_count = ?,
     manifest_content_size = ?, manifest_word_count = ?, completed_chapters = 0,
     base_run_id = ?, last_error = '', updated_at = datetime('now')
 WHERE id = ? AND stage = 'parsing'`, manifestFingerprint, int64(len(newChapters)), newContentSize,
			int64(len(assigned)), declaredBytes, declaredWords, baseRunID, runID); err != nil {
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
		inspection := importRunPreparationInspection(runID, row.Purpose, sourceDisplay, previewChapters,
			newNumbers, make([]int64, 0), duplicateNumbers)
		response = map[string]any{
			"classification":          "new",
			"run":                     snapshot,
			"newChapterNumbers":       newNumbers,
			"conflictChapterNumbers":  make([]int64, 0),
			"duplicateChapterNumbers": duplicateNumbers,
			"inspection":              inspection,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// Restart is db:import-run-restart: it fences the run the user gave up on and
// creates the next run as a copy of that run's frozen facts.
//
// The previous run is neither deleted nor re-staged. It keeps its stage and status,
// loses resumable, keeps 'Restarted by user' as its last error, has its lease
// retired and its epoch advanced - so any in-flight executor of the old run is
// fenced out by the epoch check. The new run starts 'ready' at the same stage and
// carries root_run_id, purpose, effect namespace, both fingerprints, the legacy
// collection fingerprint, the source display and the locale.
//
// A run that never left 'parsing' restarts with zeroed manifest bookkeeping, a zero
// manifest fingerprint, a null base_run_id, pending source rows and no chapters:
// nothing it parsed was ever adopted. Any later stage restarts with its source rows,
// its chapters and its manifest counters copied, because those are the facts it
// already committed.
func (r *ImportRuns) Restart(ctx context.Context, runID, nextRunID string) (map[string]any, error) {
	normalizedNextID := strings.TrimSpace(nextRunID)
	if normalizedNextID == "" || importRunUTF16Length(normalizedNextID) > 160 {
		return nil, errors.New("IMPORT_RUN_ID_INVALID")
	}
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response map[string]any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		now := time.Now().UnixMilli()
		source, present, err := readImportRunRow(ctx, conn, runID)
		if err != nil {
			return err
		}
		restartable := present && source.Resumable == 1 && (source.Status == "failed" ||
			source.Status == "cancelled" || (source.Status == "running" && source.LeaseExpiresAt <= now))
		if !restartable {
			return errors.New("IMPORT_RUN_NOT_RESTARTABLE")
		}
		if source.Purpose == "author-manuscript" {
			return errors.New("IMPORT_RUN_AUTHOR_RESTART_FORBIDDEN")
		}
		if _, taken, err := readImportRunRow(ctx, conn, normalizedNextID); err != nil {
			return err
		} else if taken {
			return errors.New("IMPORT_RUN_ID_CONFLICT")
		}
		result, err := conn.ExecContext(ctx, `UPDATE import_runs
 SET resumable = 0, cancel_requested = 0, last_error = 'Restarted by user',
     execution_owner = '', execution_epoch = execution_epoch + 1, lease_expires_at = 0,
     updated_at = datetime('now')
 WHERE id = ? AND execution_epoch = ?`, runID, source.ExecutionEpoch)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed != 1 {
			return errors.New("IMPORT_RUN_EXECUTOR_BUSY")
		}
		restartingParsing := source.Stage == "parsing"
		manifestFingerprint := source.ManifestFingerprint
		totalChapters := source.TotalChapters
		totalContentSize := source.TotalContentSize
		manifestChapters := source.ManifestChapterCount
		manifestSize := source.ManifestContentSize
		manifestWords := source.ManifestWordCount
		var baseRunID any
		if source.BaseRunID.Valid {
			baseRunID = source.BaseRunID.String
		}
		if restartingParsing {
			manifestFingerprint = importRunZeroFingerprint
			totalChapters = 0
			totalContentSize = 0
			manifestChapters = 0
			manifestSize = 0
			manifestWords = 0
			baseRunID = nil
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO import_runs (
 id, purpose, root_run_id, effect_namespace, source_fingerprint, manifest_fingerprint,
 authority_fingerprint, legacy_source_fingerprint, source_display_json, locale,
 stage, status, total_chapters, total_content_size, completed_chapters, base_run_id,
 manifest_chapter_count, manifest_content_size, manifest_word_count
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ready', ?, ?, 0, ?, ?, ?, ?)`,
			normalizedNextID, source.Purpose, source.RootRunID, importRunEffectNamespace(source.Purpose, normalizedNextID),
			source.SourceFingerprint, manifestFingerprint, source.AuthorityFingerprint,
			source.LegacySourceFingerprint, source.SourceDisplayJSON, source.Locale, source.Stage,
			totalChapters, totalContentSize, baseRunID, manifestChapters, manifestSize, manifestWords); err != nil {
			return err
		}
		if restartingParsing {
			if _, err := conn.ExecContext(ctx, `INSERT INTO import_run_sources (
 run_id, source_index, source_id, source_fingerprint, legacy_source_fingerprint, display_json, status,
 manifest_fingerprint, chapter_count, content_size, word_count, last_error
)
 SELECT ?, source_index, source_id, source_fingerprint, legacy_source_fingerprint, display_json,
        'pending', '', 0, 0, 0, ''
 FROM import_run_sources WHERE run_id = ? ORDER BY source_index`, normalizedNextID, runID); err != nil {
				return err
			}
		} else {
			if _, err := conn.ExecContext(ctx, `INSERT INTO import_run_sources (
 run_id, source_index, source_id, source_fingerprint, legacy_source_fingerprint, display_json, status,
 manifest_fingerprint, chapter_count, content_size, word_count, last_error
)
 SELECT ?, source_index, source_id, source_fingerprint, legacy_source_fingerprint, display_json, status,
        manifest_fingerprint, chapter_count, content_size, word_count, last_error
 FROM import_run_sources WHERE run_id = ? ORDER BY source_index`, normalizedNextID, runID); err != nil {
				return err
			}
			if _, err := conn.ExecContext(ctx, `INSERT INTO import_run_chapters (
 run_id, chapter_number, source_id, source_chapter_number,
 title, content_fingerprint, content_size, content_snapshot
)
 SELECT ?, chapter_number, source_id, source_chapter_number,
        title, content_fingerprint, content_size, content_snapshot
 FROM import_run_chapters WHERE run_id = ? ORDER BY chapter_number`, normalizedNextID, runID); err != nil {
				return err
			}
		}
		created, present, err := readImportRunRow(ctx, conn, normalizedNextID)
		if err != nil {
			return err
		}
		if !present {
			return errors.New("IMPORT_RUN_STATE_CONFLICT")
		}
		response, err = importRunSnapshot(ctx, conn, created)
		return err
	})
	if err != nil {
		return nil, err
	}
	return response, nil
}

// Fail is db:import-run-fail: it records the run's own failure at the stage the
// executor was in, and retires the lease so the run can be resumed or restarted.
//
// reason is `any` because the original refuses a non-string outright, and the check
// runs after the stage comparison: a run that is not running that stage answers
// IMPORT_RUN_STAGE_MISMATCH before its reason is ever looked at.
//
// Nothing is translated here. The caller has already turned a symbolic code into a
// sentence in the run's own language (electron/controllers/import-run-failure.ts:40-42,
// persistedImportRunFailure), and the original stores that text as given, cut at
// 2 000 UTF-16 code units.
func (r *ImportRuns) Fail(ctx context.Context, runID, stage string, reason any, execution ImportRunExecution) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	var response map[string]any
	err := r.importRunImmediate(ctx, func(conn *sql.Conn) error {
		row, err := importRunAssertExecutionAuthority(ctx, conn, runID, execution, time.Now().UnixMilli())
		if err != nil {
			return err
		}
		if row.Status != "running" || row.Stage != stage {
			return errors.New("IMPORT_RUN_STAGE_MISMATCH")
		}
		text, ok := reason.(string)
		if !ok {
			return errors.New("IMPORT_RUN_FAILURE_REASON_INVALID")
		}
		result, err := conn.ExecContext(ctx, `UPDATE import_runs
 SET status = 'failed', last_error = ?, resumable = 1,
     execution_owner = '', execution_epoch = execution_epoch + 1, lease_expires_at = 0,
     updated_at = datetime('now')
 WHERE id = ? AND stage = ? AND status = 'running'`,
			importRunTruncateUTF16(text, importRunFailureReasonLimit), runID, stage)
		if err != nil {
			return err
		}
		if changed, err := result.RowsAffected(); err != nil {
			return err
		} else if changed == 0 {
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
