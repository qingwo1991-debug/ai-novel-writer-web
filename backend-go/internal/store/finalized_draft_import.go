package store

// The two author-manuscript import writes that had no store counterpart:
//
//	db:draft-import-finalized-batch  FinalizedDraftImportRepository.commit
//	                                 (:391-505)
//	db:import-run-author-preview     FinalizedDraftImportRepository.preview
//	                                 (:311-389)
//
// Ported from electron/repositories/finalized-draft-import-repository.ts (506
// lines) with the wire contract of src/shared/finalized-draft-import.ts:1-37,
// src/shared/author-manuscript-import.ts:1-33 and
// electron/controllers/db-controller.ts:367-383, :716-728. The helpers that
// cross into the transaction are ported with the methods that call them:
// authorityRows (:142-154), authorityFingerprint (:156-163) - through this
// package's draftAuthorityFingerprint (draft_authority.go:92), which reproduces
// it byte for byte - sequenceFromRows (:165-205), normalizeChapters (:60-90),
// requireNonEmptyOperationId (:52-58), requestPayloadHash (:92-113),
// requestPayloadHashCandidates (:115-131), manifestFingerprint (:133-140),
// finalizationId (:207-209), parseStoredReceipt (:211-232) and verifyStoredFacts
// (:234-281). getCommittedOperation (:292-309) is ported too, because the author
// lane's resumable short-circuit is the one caller importrun_lifecycle.go:26-34
// still names as missing.
//
// # The transaction boundary: one transaction, not one per chapter
//
// commit wraps its whole body in better-sqlite3's db.transaction (:400) and the
// doc comment above the class says why (:283-286): body, finalized draft and
// publication outbox are all-or-nothing. Everything - the replay probe, the two
// fingerprint guards, the already-finalized probe, every chapter's three INSERTs
// and the operation row - runs inside that one function, and an exception
// anywhere rolls all of it back. So "batch" means one atomic commit, NOT a
// commit per chapter: a refusal on chapter 7 leaves chapters 1-6 unwritten.
// Reproduced here as one BEGIN IMMEDIATE on one reserved connection (the shape
// of every neighbouring write, finalized_chapter_deletion.go:274-311), which
// only widens the lock the original always ended up taking.
//
// A consequence worth stating: `word_count` and chapter numbering are assigned
// inside the same transaction as the version probe, so no second writer can slip
// between MAX(version) (:442-446) and the INSERT (:447-455).
//
// # Statement order inside the transaction
//
// The reference's order is preserved exactly, because a batch import that
// reorders its chapter writes can renumber chapters differently:
//
//	:401-413  the operation row, and on a replay its receipt and stored facts
//	:415-421  the commit manifest fingerprint guard
//	:422-431  the authority fingerprint guard, then preview() itself
//	:433-438  the per-chapter already-finalized probe, short-circuiting
//	:441-489  per chapter, in ascending chapter number: MAX(version) -> INSERT
//	          contents -> INSERT drafts -> resolveManuscriptTarget (no SQL) ->
//	          INSERT finalization_outbox
//	:498-501  the operation row, whose receipt_json freezes the result
//
// # Every table and column these statements touch
//
//   - drafts: id, chapter_number, version, status, source, content_id,
//     word_count (read, and INSERT). (baseline_v7_ddl.go:205-218; the unique
//     index on (chapter_number, version) at :655-656 is what makes the MAX+1
//     version probe the correct one.)
//   - contents: id, body (INSERT of the chapter body; join read for the
//     authority and fact queries). (baseline_v7_ddl.go:195-199)
//   - finalization_outbox: finalization_id, draft_id, chapter_number,
//     chapter_title, content_hash, content_revision, content_snapshot,
//     target_file_name, publication_status, last_error (INSERT; join read).
//     (baseline_v7_ddl.go:218-234)
//   - finalized_draft_import_operations: operation_id, payload_hash,
//     receipt_json (read, and INSERT). (baseline_v7_ddl.go:235-240)
//   - continuity_projection_meta (baseline_v7_ddl.go:200-204): NOT touched.
//     The reference invalidates no continuity projection here - unlike the
//     chapter deletion lane, which must (finalized_chapter_deletion.go:733-751)
//     - so nothing below reads or writes it. The tests seed it to prove it stays
//     exactly as seeded.
//
// # What is deliberately NOT ported
//
//   - The filesystem arm of resolveManuscriptTarget
//     (electron/services/manuscript-publisher.ts:65-80): fs.existsSync at
//     :73-78, the collision suffix loop and MANUSCRIPT_TARGET_COLLISIONS at
//     :79. This package never opens a path (see sqlite.go:11-15), so it cannot
//     learn whether the preferred file name is already taken on disk.
//     Everything the name is made of IS ported
//     (finalizedImportTargetFileName below, the same reduction
//     internal/transport/finalization.go:564-601 already applies), so the frozen
//     target_file_name is the deterministic direct-child name; only the "this
//     file already exists, add the finalization-id marker" arm is absent. The
//     adapter owning the project root must resolve a taken name before it
//     publishes; nothing here weakens another guard to compensate.
//     containedDirectChild (:57-63) and MANUSCRIPT_CHAPTER_NUMBER_INVALID
//     (:66-67) go with it: sanitization leaves no separator in the name, so
//     the first can never fire, and normalizeChapters already refuses a chapter
//     number below one (:66-68), so the second is unreachable.
//   - The author-manuscript prepare() lane (import-run-repository.ts:2005-2138)
//     is not a repository this file owns; it stays unbound. CommittedOperation
//     below is the half of it that was genuinely missing.
//
// # The refusal strings
//
// Each is the reference's own literal, returned as a plain error like every
// sibling in this package, with the line it is thrown from above the guard.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
)

var (
	// :55 - an empty or over-long operation id.
	ErrFinalizedImportOperationInvalid = errors.New("FINALIZED_IMPORT_OPERATION_INVALID")
	// :62, :67, :70, :74, :77, :80 - the chapter payload is not importable.
	ErrFinalizedImportChaptersInvalid = errors.New("FINALIZED_IMPORT_CHAPTERS_INVALID")
	// :216, :219, :229, :245, :255 - the stored receipt does not describe
	// itself or this request.
	ErrFinalizedImportReceiptCorrupt = errors.New("FINALIZED_IMPORT_RECEIPT_CORRUPT")
	// :278 - a committed draft's frozen facts moved since the receipt.
	ErrFinalizedImportFactsChanged = errors.New("FINALIZED_IMPORT_FACTS_CHANGED")
	// :408 - the operation id is already bound to a different payload.
	ErrFinalizedImportOperationConflict = errors.New("FINALIZED_IMPORT_OPERATION_CONFLICT")
	// :419, :425 - the manifest or authority the caller inspected has moved.
	ErrFinalizedImportPreviewStale = errors.New("FINALIZED_IMPORT_PREVIEW_STALE")
	// :429 - the authority is current but the chapters would not append.
	ErrFinalizedImportAuthorityConflict = errors.New("FINALIZED_IMPORT_AUTHORITY_CONFLICT")
	// :437 - a chapter number this batch targets is already finalized.
	ErrFinalizedImportChapterAlreadyFinalized = errors.New("FINALIZED_IMPORT_CHAPTER_ALREADY_FINALIZED")
)

// The absence refusal is the reference's own PROJECT_NOT_OPEN (:144, :239, :297,
// :393), which this package already exports as ErrProjectNotOpen
// (projectcore.go:12): nothing extra is invented for it.

// FinalizedDraftImportChapter is FinalizedDraftImportChapter
// (src/shared/finalized-draft-import.ts:1-6). WordCount is a pointer because the
// original distinguishes a missing/non-integer wordCount (:79
// `!Number.isInteger(chapter.wordCount)`) from a supplied one: a nil is that
// refusal, and a wrong value is the same refusal. The caller that binds this
// from JSON must keep the difference.
//
// ChapterNumber is int64, so `Number.isInteger` at :66 is the binder's
// obligation; everything else the original validates is validated below.
type FinalizedDraftImportChapter struct {
	ChapterNumber int64  `json:"chapterNumber"`
	Title         string `json:"title"`
	Content       string `json:"content"`
	WordCount     *int64 `json:"wordCount"`
}

// FinalizedDraftImportRequest is FinalizedDraftImportRequest
// (src/shared/finalized-draft-import.ts:8-17). A nil fingerprint is the
// original's `undefined`; the wire must not send an explicit null, which the
// original would compare as a value and refuse as stale.
type FinalizedDraftImportRequest struct {
	OperationID                       string                        `json:"operationId"`
	ExpectedAuthorityFingerprint      *string                       `json:"expectedAuthorityFingerprint"`
	ExpectedManifestFingerprint       *string                       `json:"expectedManifestFingerprint"`
	ExpectedCommitManifestFingerprint *string                       `json:"expectedCommitManifestFingerprint"`
	Chapters                          []FinalizedDraftImportChapter `json:"chapters"`
}

// FinalizedDraftImportDraftReceipt is FinalizedDraftImportDraftReceipt
// (src/shared/finalized-draft-import.ts:19-28). The field order is the object
// literal order of :480-488, so the stored receipt_json matches the original's
// JSON.stringify byte for byte.
type FinalizedDraftImportDraftReceipt struct {
	ChapterNumber     int64  `json:"chapterNumber"`
	DraftID           int64  `json:"draftId"`
	FinalizationID    string `json:"finalizationId"`
	ContentHash       string `json:"contentHash"`
	TargetFileName    string `json:"targetFileName"`
	Status            string `json:"status"`
	PublicationStatus string `json:"publicationStatus"`
}

// FinalizedDraftImportReceipt is FinalizedDraftImportReceipt
// (src/shared/finalized-draft-import.ts:30-36), in the literal order of
// :491-497.
type FinalizedDraftImportReceipt struct {
	OperationID    string                             `json:"operationId"`
	PayloadHash    string                             `json:"payloadHash"`
	ChapterNumbers []int64                            `json:"chapterNumbers"`
	Drafts         []FinalizedDraftImportDraftReceipt `json:"drafts"`
	Idempotent     bool                               `json:"idempotent"`
}

// AuthorManuscriptPreviewChapter is AuthorManuscriptPreviewChapter
// (src/shared/author-manuscript-import.ts:13-18).
type AuthorManuscriptPreviewChapter struct {
	Number      int64  `json:"number"`
	Title       string `json:"title"`
	WordCount   int64  `json:"wordCount"`
	Disposition string `json:"disposition"`
}

// AuthorManuscriptImportPreview is AuthorManuscriptImportPreview
// (src/shared/author-manuscript-import.ts:20-33). NextChapterNumber and
// FirstGapChapterNumber are pointers because the original omits them
// (:379, :384-386) instead of writing null; both fields are only ever omitted,
// never zero.
type AuthorManuscriptImportPreview struct {
	Classification          string                           `json:"classification"`
	AuthorityFingerprint    string                           `json:"authorityFingerprint"`
	ManifestFingerprint     string                           `json:"manifestFingerprint"`
	ChapterCount            int64                            `json:"chapterCount"`
	TargetStatus            string                           `json:"targetStatus"`
	NextChapterNumber       *int64                           `json:"nextChapterNumber,omitempty"`
	Chapters                []AuthorManuscriptPreviewChapter `json:"chapters"`
	NewChapterNumbers       []int64                          `json:"newChapterNumbers"`
	DuplicateChapterNumbers []int64                          `json:"duplicateChapterNumbers"`
	ConflictChapterNumbers  []int64                          `json:"conflictChapterNumbers"`
	FirstGapChapterNumber   *int64                           `json:"firstGapChapterNumber,omitempty"`
	AuthorityInvalid        bool                             `json:"authorityInvalid"`
}

// finalizedImportChapter is one element of normalizeChapters' output: the
// request's chapter after validation, in ascending chapter number order.
type finalizedImportChapter struct {
	chapterNumber int64
	title         string
	content       string
	wordCount     int64
}

// finalizedImportAuthorityRow is AuthorityRow (:40-46). Title is the
// finalization_outbox sub-query of :148, so it is NULL for a finalized draft
// that never got an outbox row - which preview treats as "any title" (:332).
type finalizedImportAuthorityRow struct {
	draftID       int64
	chapterNumber int64
	version       int64
	title         sql.NullString
	body          string
}

// finalizedImportSequence is AuthoritativeChapterSequence
// (src/shared/author-manuscript-import.ts:3-10) kept typed, so preview can test
// its parts instead of re-reading a wire map. nextChapterNumber is nil exactly
// where the original leaves it undefined: an 'invalid' sequence.
type finalizedImportSequence struct {
	status                  string
	lastChapterNumber       int64
	nextChapterNumber       *int64
	firstGapChapterNumber   *int64
	duplicateChapterNumbers []int64
	authorityFingerprint    string
}

// finalizedImportOperationRow is ImportOperationRow (:21-25).
type finalizedImportOperationRow struct {
	operationID string
	payloadHash string
	receiptJSON string
}

// finalizedImportPayloadChapter is the chapter object the payload hash hashes
// (:82-87). Field order is load-bearing: JSON.stringify writes these keys in
// this order and the digest is taken over those bytes.
type finalizedImportPayloadChapter struct {
	ChapterNumber int64  `json:"chapterNumber"`
	Title         string `json:"title"`
	Content       string `json:"content"`
	WordCount     int64  `json:"wordCount"`
}

// finalizedImportBarePayload is :97 `{ operationId, chapters }`.
type finalizedImportBarePayload struct {
	OperationID string                          `json:"operationId"`
	Chapters    []finalizedImportPayloadChapter `json:"chapters"`
}

// finalizedImportExpectedPayload is :99-104, where `?? null` is a nil pointer.
type finalizedImportExpectedPayload struct {
	OperationID                  string                          `json:"operationId"`
	ExpectedAuthorityFingerprint *string                         `json:"expectedAuthorityFingerprint"`
	ExpectedManifestFingerprint  *string                         `json:"expectedManifestFingerprint"`
	Chapters                     []finalizedImportPayloadChapter `json:"chapters"`
}

// finalizedImportCommittedPayload is :106-112.
type finalizedImportCommittedPayload struct {
	OperationID                       string                          `json:"operationId"`
	ExpectedAuthorityFingerprint      *string                         `json:"expectedAuthorityFingerprint"`
	ExpectedManifestFingerprint       *string                         `json:"expectedManifestFingerprint"`
	ExpectedCommitManifestFingerprint *string                         `json:"expectedCommitManifestFingerprint"`
	Chapters                          []finalizedImportPayloadChapter `json:"chapters"`
}

// finalizedImportManifestChapter is one element of manifestFingerprint's payload
// (:134-139).
type finalizedImportManifestChapter struct {
	ChapterNumber int64  `json:"chapterNumber"`
	Title         string `json:"title"`
	ContentHash   string `json:"contentHash"`
	WordCount     int64  `json:"wordCount"`
}

// finalizedImportJSONDigest is sha256(JSON.stringify(value)) as the reference
// computes it: compact JSON with no HTML escaping. encoding/json escapes the two
// line separators where JSON.stringify emits them raw, so they are unescaped
// again - the same correction importRunManifestFingerprint makes
// (importrun_lifecycle.go:395-400).
func finalizedImportJSONDigest(value any) (string, error) {
	encoded, err := jsonMarshalCompact(value)
	if err != nil {
		return "", err
	}
	encoded = strings.ReplaceAll(encoded, `\u2028`, "\u2028")
	encoded = strings.ReplaceAll(encoded, `\u2029`, "\u2029")
	return draftBodyHash(encoded), nil
}

// finalizedImportRequireOperationID is requireNonEmptyOperationId (:52-58): the
// id is trimmed first, and the bound is 200 UTF-16 code units, which is what
// `.length` counts.
func finalizedImportRequireOperationID(operationID string) (string, error) {
	normalized := trimJS(operationID)
	if normalized == "" || importRunUTF16Length(normalized) > 200 {
		// :55
		return "", ErrFinalizedImportOperationInvalid
	}
	return normalized, nil
}

// finalizedImportNormalizeChapters is normalizeChapters (:60-90). Every refusal
// in it is the same code, checked in the reference's own order, and the result
// is sorted by chapter number - a stable sort, like Array.prototype.sort with
// the comparator at :89, so an unordered request is numbered ascending.
func finalizedImportNormalizeChapters(items []FinalizedDraftImportChapter) ([]finalizedImportChapter, error) {
	if len(items) == 0 {
		// :61-63
		return nil, ErrFinalizedImportChaptersInvalid
	}
	seen := make(map[int64]bool, len(items))
	normalized := make([]finalizedImportChapter, 0, len(items))
	for _, item := range items {
		if item.ChapterNumber < 1 {
			// :66-68
			return nil, ErrFinalizedImportChaptersInvalid
		}
		if seen[item.ChapterNumber] {
			// :69-72
			return nil, ErrFinalizedImportChaptersInvalid
		}
		seen[item.ChapterNumber] = true
		// :73-75 - the title must be a string. The field cannot hold anything
		// else here, so the binder owes this check, not the store.
		if trimJS(item.Content) == "" {
			// :76-78
			return nil, ErrFinalizedImportChaptersInvalid
		}
		if item.WordCount == nil || *item.WordCount != int64(draftalg.CountDraftUnits(item.Content)) {
			// :79-81 - the claimed count must be the visible-prose count of the
			// bytes that are about to be frozen.
			return nil, ErrFinalizedImportChaptersInvalid
		}
		normalized = append(normalized, finalizedImportChapter{chapterNumber: item.ChapterNumber,
			title: item.Title, content: item.Content, wordCount: *item.WordCount})
	}
	sort.SliceStable(normalized, func(left, right int) bool {
		return normalized[left].chapterNumber < normalized[right].chapterNumber
	})
	return normalized, nil
}

// finalizedImportPayloadChapters is :82-87 for the hash payloads.
func finalizedImportPayloadChapters(chapters []finalizedImportChapter) []finalizedImportPayloadChapter {
	payload := make([]finalizedImportPayloadChapter, 0, len(chapters))
	for _, chapter := range chapters {
		payload = append(payload, finalizedImportPayloadChapter{ChapterNumber: chapter.chapterNumber,
			Title: chapter.title, Content: chapter.content, WordCount: chapter.wordCount})
	}
	return payload
}

// finalizedImportPayloadHash is requestPayloadHash (:92-113): the fingerprint
// binds the operation id, the chapters, and whichever expectations the caller
// supplied - the three shapes are three different digests.
func finalizedImportPayloadHash(request FinalizedDraftImportRequest, chapters []finalizedImportChapter) (string, error) {
	payload := finalizedImportPayloadChapters(chapters)
	if request.ExpectedAuthorityFingerprint == nil && request.ExpectedManifestFingerprint == nil &&
		request.ExpectedCommitManifestFingerprint == nil {
		// :97
		return finalizedImportJSONDigest(finalizedImportBarePayload{OperationID: request.OperationID,
			Chapters: payload})
	}
	if request.ExpectedCommitManifestFingerprint == nil {
		// :98-105
		return finalizedImportJSONDigest(finalizedImportExpectedPayload{OperationID: request.OperationID,
			ExpectedAuthorityFingerprint: request.ExpectedAuthorityFingerprint,
			ExpectedManifestFingerprint:  request.ExpectedManifestFingerprint,
			Chapters:                     payload})
	}
	// :106-112
	return finalizedImportJSONDigest(finalizedImportCommittedPayload{OperationID: request.OperationID,
		ExpectedAuthorityFingerprint:      request.ExpectedAuthorityFingerprint,
		ExpectedManifestFingerprint:       request.ExpectedManifestFingerprint,
		ExpectedCommitManifestFingerprint: request.ExpectedCommitManifestFingerprint,
		Chapters:                          payload})
}

// finalizedImportPayloadHashCandidates is requestPayloadHashCandidates
// (:115-131): a receipt written by an older counter must still replay, so the
// two historical counters are accepted beside the current one. The second is
// `content.length`, which JavaScript counts in UTF-16 code units - not bytes.
func finalizedImportPayloadHashCandidates(request FinalizedDraftImportRequest, chapters []finalizedImportChapter) (map[string]bool, error) {
	candidates := make(map[string]bool, 3)
	primary, err := finalizedImportPayloadHash(request, chapters)
	if err != nil {
		return nil, err
	}
	candidates[primary] = true
	counters := []func(string) int64{
		func(content string) int64 { return int64(draftalg.CountLegacyDraftUnitsV1(content)) },
		importRunUTF16Length,
	}
	for _, counter := range counters {
		legacy := make([]finalizedImportChapter, 0, len(chapters))
		for _, chapter := range chapters {
			chapter.wordCount = counter(chapter.content)
			legacy = append(legacy, chapter)
		}
		digest, err := finalizedImportPayloadHash(request, legacy)
		if err != nil {
			return nil, err
		}
		candidates[digest] = true
	}
	return candidates, nil
}

// finalizedImportManifestFingerprint is manifestFingerprint (:133-140): the
// identity of the chapter set itself, which the preview hands back and the
// commit rechecks.
func finalizedImportManifestFingerprint(chapters []finalizedImportChapter) (string, error) {
	payload := make([]finalizedImportManifestChapter, 0, len(chapters))
	for _, chapter := range chapters {
		payload = append(payload, finalizedImportManifestChapter{ChapterNumber: chapter.chapterNumber,
			Title: chapter.title, ContentHash: draftBodyHash(chapter.content),
			WordCount: chapter.wordCount})
	}
	return finalizedImportJSONDigest(payload)
}

// finalizedImportAuthorityRows is authorityRows (:142-154): every finalized
// draft with its body and its frozen title, in the order the sequence verdict
// and the preview's last-row-wins map both depend on.
func finalizedImportAuthorityRows(ctx context.Context, q importRunQueryer) ([]finalizedImportAuthorityRow, error) {
	rows, err := q.QueryContext(ctx, `SELECT drafts.id, drafts.chapter_number, drafts.version,
 contents.body,
 (SELECT chapter_title FROM finalization_outbox WHERE draft_id = drafts.id) AS title
 FROM drafts
 JOIN contents ON contents.id = drafts.content_id
 WHERE drafts.status = 'finalized'
 ORDER BY drafts.chapter_number ASC, drafts.version ASC, drafts.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	authority := make([]finalizedImportAuthorityRow, 0)
	for rows.Next() {
		var row finalizedImportAuthorityRow
		if err := rows.Scan(&row.draftID, &row.chapterNumber, &row.version, &row.body, &row.title); err != nil {
			return nil, err
		}
		authority = append(authority, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return authority, nil
}

// finalizedImportSequenceFromRows is sequenceFromRows (:165-205). The
// fingerprint is draftAuthorityFingerprint's, which is authorityFingerprint
// (:156-163) byte for byte; this port only adds the typed parts preview tests.
func finalizedImportSequenceFromRows(rows []finalizedImportAuthorityRow) (finalizedImportSequence, error) {
	plain := make([]draftAuthorityRow, 0, len(rows))
	for _, row := range rows {
		plain = append(plain, draftAuthorityRow{draftID: row.draftID, chapterNumber: row.chapterNumber,
			version: row.version, body: row.body})
	}
	fingerprint, err := draftAuthorityFingerprint(plain)
	if err != nil {
		return finalizedImportSequence{}, err
	}
	if len(rows) == 0 {
		// :167-175
		next := int64(1)
		return finalizedImportSequence{status: "empty", lastChapterNumber: 0, nextChapterNumber: &next,
			duplicateChapterNumbers: make([]int64, 0), authorityFingerprint: fingerprint}, nil
	}
	counts := make(map[int64]int64, len(rows))
	maxChapter := int64(0)
	for _, row := range rows {
		counts[row.chapterNumber]++
		if row.chapterNumber > maxChapter {
			maxChapter = row.chapterNumber
		}
	}
	// :178-188 - duplicates first, then the first gap in 1..max. Both make the
	// sequence 'invalid', and the original only tests the lists for emptiness;
	// a stable order keeps the answer deterministic.
	duplicates := make([]int64, 0)
	for chapter, count := range counts {
		if count > 1 {
			duplicates = append(duplicates, chapter)
		}
	}
	sort.Slice(duplicates, func(left, right int) bool { return duplicates[left] < duplicates[right] })
	var firstGap *int64
	for chapter := int64(1); chapter <= maxChapter; chapter++ {
		if counts[chapter] == 0 {
			gap := chapter
			firstGap = &gap
			break
		}
	}
	if len(duplicates) > 0 || firstGap != nil {
		// :189-197
		return finalizedImportSequence{status: "invalid", lastChapterNumber: maxChapter,
			firstGapChapterNumber: firstGap, duplicateChapterNumbers: duplicates,
			authorityFingerprint: fingerprint}, nil
	}
	// :198-204
	next := maxChapter + 1
	return finalizedImportSequence{status: "continuous", lastChapterNumber: maxChapter,
		nextChapterNumber: &next, duplicateChapterNumbers: duplicates,
		authorityFingerprint: fingerprint}, nil
}

// finalizedImportFinalizationID is finalizationId (:207-209): the receipt identity
// an import mints per chapter, deterministic from the operation so a replay names
// the same one.
func finalizedImportFinalizationID(operationID string, chapterNumber int64) string {
	digest := draftBodyHash(operationID + ":" + strconv.FormatInt(chapterNumber, 10))
	return "import-" + digest[:32]
}

// finalizedImportFileNamePart is sanitizeWindowsFileNamePart
// (electron/services/manuscript-publisher.ts:30-43) in the original's order:
// the illegal characters and the control characters become '_', then trim, then
// trailing dots and spaces, then the reserved-device suffix. The two replace
// passes are merged because their character sets are disjoint and both write
// '_'. The store keeps its own copy because it must not import the transport
// package (which owns the same reduction at finalization.go:566-590): the frozen
// name is a database fact, not a filesystem one.
func finalizedImportFileNamePart(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character <= 0x1f || strings.ContainsRune(`<>:"/\|?*`, character) {
			builder.WriteByte('_')
			continue
		}
		builder.WriteRune(character)
	}
	part := strings.TrimRight(trimJS(builder.String()), ". ")
	if part == "" {
		return ""
	}
	if finalizedImportReservedName(part) {
		return part + "_"
	}
	return part
}

// finalizedImportReservedName is WINDOWS_RESERVED_FILE_NAME
// (manuscript-publisher.ts:6, :39-41): the device base name, with or without a
// suffix after the first dot.
func finalizedImportReservedName(part string) bool {
	base := strings.ToLower(part)
	if offset := strings.IndexByte(base, '.'); offset >= 0 {
		base = base[:offset]
	}
	switch base {
	case "con", "prn", "aux", "nul",
		"com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9",
		"lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
		return true
	}
	return false
}

// finalizedImportTargetFileName is manuscriptFileName (manuscript-publisher.ts:52-55):
// the portable direct-child name the outbox freezes, so a later publish or retry
// never accepts a renderer path. See this file's header for the collision arm
// that is not ported.
func finalizedImportTargetFileName(chapterNumber int64, chapterTitle string) string {
	title := finalizedImportFileNamePart(chapterTitle)
	if title == "" {
		return fmt.Sprintf("第%d章.txt", chapterNumber)
	}
	return fmt.Sprintf("第%d章 %s.txt", chapterNumber, title)
}

// finalizedImportReadOperation is the operation probe of :300-304 and :401-405.
func finalizedImportReadOperation(ctx context.Context, q importRunQueryer, operationID string) (finalizedImportOperationRow, bool, error) {
	var row finalizedImportOperationRow
	err := q.QueryRowContext(ctx, `SELECT operation_id, payload_hash, receipt_json
 FROM finalized_draft_import_operations WHERE operation_id = ?`, operationID).
		Scan(&row.operationID, &row.payloadHash, &row.receiptJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return finalizedImportOperationRow{}, false, nil
	}
	if err != nil {
		return finalizedImportOperationRow{}, false, err
	}
	return row, true, nil
}

// finalizedImportParseReceipt is parseStoredReceipt (:211-232). A stored value
// that is not a JSON object, or that does not describe the row it sits in, is
// corrupt rather than empty - the receipt is the only record of what was
// written, so a damaged one must never read as "nothing to replay".
func finalizedImportParseReceipt(row finalizedImportOperationRow) (*FinalizedDraftImportReceipt, error) {
	var candidate any
	if err := json.Unmarshal([]byte(row.receiptJSON), &candidate); err != nil {
		// :214-217
		return nil, ErrFinalizedImportReceiptCorrupt
	}
	// :218-220 - `!candidate || typeof candidate !== 'object'`. A JSON array is
	// also corrupt: it cannot carry operationId, which :222 then refuses.
	if _, ok := candidate.(map[string]any); !ok {
		return nil, ErrFinalizedImportReceiptCorrupt
	}
	receipt := &FinalizedDraftImportReceipt{}
	if err := json.Unmarshal([]byte(row.receiptJSON), receipt); err != nil {
		// A key of the wrong JSON type is the original's `undefined`/string
		// mismatch at :222-230, refused with the same code.
		return nil, ErrFinalizedImportReceiptCorrupt
	}
	// :222-230 - the two arrays must exist (a JSON null decodes to a nil slice,
	// which is `!Array.isArray`) and describe one draft per chapter.
	if receipt.OperationID != row.operationID || receipt.PayloadHash != row.payloadHash ||
		receipt.ChapterNumbers == nil || receipt.Drafts == nil ||
		len(receipt.ChapterNumbers) != len(receipt.Drafts) {
		return nil, ErrFinalizedImportReceiptCorrupt
	}
	return receipt, nil
}

// finalizedImportVerifyStoredFacts is verifyStoredFacts (:234-281): the receipt
// is not trusted. Every draft it names must still exist under the finalization
// identity it froze, with the same chapter, an accepted status, the same word
// count, body, frozen snapshot, content hash, target file and a publication
// status that is still pending or published.
//
// The fact columns are read as plain strings because the DDL declares every one
// of them NOT NULL (baseline_v7_ddl.go:205-234); a NULL can only come from a
// row that cannot exist, and the JOIN then answers no rows, which is the
// original's `!fact` arm.
func finalizedImportVerifyStoredFacts(ctx context.Context, q importRunQueryer, receipt *FinalizedDraftImportReceipt, chapters []finalizedImportChapter) error {
	if len(receipt.ChapterNumbers) != len(chapters) || len(receipt.Drafts) != len(chapters) {
		// :240-246
		return ErrFinalizedImportReceiptCorrupt
	}
	for index, number := range receipt.ChapterNumbers {
		if number != chapters[index].chapterNumber {
			// :243 - the receipt names a different chapter in this position.
			return ErrFinalizedImportReceiptCorrupt
		}
	}
	for index, draft := range receipt.Drafts {
		chapter := chapters[index]
		if draft.ChapterNumber != chapter.chapterNumber || draft.Status != "finalized" ||
			draft.PublicationStatus != "pending" || draft.ContentHash != draftBodyHash(chapter.content) {
			// :249-256
			return ErrFinalizedImportReceiptCorrupt
		}
		var factDraftID, factChapterNumber, factWordCount int64
		var factStatus, factBody, factFinalizationID, factContentHash string
		var factContentSnapshot, factTargetFileName, factPublicationStatus string
		err := q.QueryRowContext(ctx, `SELECT drafts.id, drafts.chapter_number, drafts.status,
 drafts.word_count, contents.body, finalization_outbox.finalization_id,
 finalization_outbox.content_hash, finalization_outbox.content_snapshot,
 finalization_outbox.target_file_name, finalization_outbox.publication_status
 FROM drafts
 JOIN contents ON contents.id = drafts.content_id
 JOIN finalization_outbox ON finalization_outbox.draft_id = drafts.id
 WHERE drafts.id = ? AND finalization_outbox.finalization_id = ?`,
			draft.DraftID, draft.FinalizationID).Scan(&factDraftID, &factChapterNumber, &factStatus,
			&factWordCount, &factBody, &factFinalizationID, &factContentHash, &factContentSnapshot,
			&factTargetFileName, &factPublicationStatus)
		if errors.Is(err, sql.ErrNoRows) {
			// :267-279 - the draft or its outbox row is gone.
			return ErrFinalizedImportFactsChanged
		}
		if err != nil {
			return err
		}
		if factChapterNumber != chapter.chapterNumber || !bpReceiptedStatus(factStatus) ||
			factWordCount != chapter.wordCount || factBody != chapter.content ||
			factContentSnapshot != chapter.content || factContentHash != draft.ContentHash ||
			factTargetFileName != draft.TargetFileName ||
			(factPublicationStatus != "pending" && factPublicationStatus != "published") {
			// :267-279 - isReceiptedDraftStatus accepts 'finalized' and
			// 'superseded', so a replaced draft replays and an edited one does not.
			return ErrFinalizedImportFactsChanged
		}
	}
	return nil
}

// finalizedImportPreviewBody is preview (:311-389) over a queryer, so the commit
// can run the identical function inside its own transaction (:427). chapters are
// already normalized and sorted.
func finalizedImportPreviewBody(ctx context.Context, q importRunQueryer, chapters []finalizedImportChapter) (*AuthorManuscriptImportPreview, error) {
	rows, err := finalizedImportAuthorityRows(ctx, q)
	if err != nil {
		return nil, err
	}
	sequence, err := finalizedImportSequenceFromRows(rows)
	if err != nil {
		return nil, err
	}
	// :315 - `new Map(rows.map(...))`: the LAST row of a chapter wins, which for
	// the ordering above is its highest version.
	existing := make(map[int64]finalizedImportAuthorityRow, len(rows))
	for _, row := range rows {
		existing[row.chapterNumber] = row
	}
	conflicts := make([]int64, 0)
	duplicates := make([]int64, 0)
	fresh := make([]int64, 0)
	previewChapters := make([]AuthorManuscriptPreviewChapter, 0, len(chapters))
	for _, chapter := range chapters {
		disposition := "new"
		row, present := existing[chapter.chapterNumber]
		switch {
		case !present:
			// :321-329
			fresh = append(fresh, chapter.chapterNumber)
		case draftBodyHash(row.body) == draftBodyHash(chapter.content) &&
			(!row.title.Valid || row.title.String == chapter.title):
			// :330-341 - same bytes and the same title (or no frozen title at
			// all) is a duplicate, not a conflict.
			disposition = "duplicate"
			duplicates = append(duplicates, chapter.chapterNumber)
		default:
			// :342-348
			disposition = "conflict"
			conflicts = append(conflicts, chapter.chapterNumber)
		}
		previewChapters = append(previewChapters, AuthorManuscriptPreviewChapter{Number: chapter.chapterNumber,
			Title: chapter.title, WordCount: chapter.wordCount, Disposition: disposition})
	}
	// :351-363 - the new chapters must append without leaving a hole: the first
	// one that is not the expected next number is the candidate gap.
	var candidateGap *int64
	if sequence.nextChapterNumber != nil && len(fresh) > 0 {
		ordered := append([]int64(nil), fresh...)
		sort.Slice(ordered, func(left, right int) bool { return ordered[left] < ordered[right] })
		expected := *sequence.nextChapterNumber
		for _, chapterNumber := range ordered {
			if chapterNumber != expected {
				gap := expected
				candidateGap = &gap
				break
			}
			expected++
		}
	}
	// :365-372
	classification := "ready"
	authorityInvalid := sequence.status == "invalid"
	switch {
	case authorityInvalid || len(conflicts) > 0 || candidateGap != nil:
		classification = "conflict"
	case len(fresh) == 0:
		classification = "exact-duplicate"
	}
	manifest, err := finalizedImportManifestFingerprint(chapters)
	if err != nil {
		return nil, err
	}
	preview := &AuthorManuscriptImportPreview{Classification: classification,
		AuthorityFingerprint: sequence.authorityFingerprint, ManifestFingerprint: manifest,
		ChapterCount: int64(len(chapters)), TargetStatus: "finalized", Chapters: previewChapters,
		NewChapterNumbers: fresh, DuplicateChapterNumbers: duplicates,
		ConflictChapterNumbers: conflicts, AuthorityInvalid: authorityInvalid}
	if classification != "conflict" {
		// :370-372
		next := sequence.lastChapterNumber + int64(len(fresh)) + 1
		preview.NextChapterNumber = &next
	}
	// :384-386 - the sequence's own gap wins over the candidate gap.
	if sequence.firstGapChapterNumber != nil {
		preview.FirstGapChapterNumber = sequence.firstGapChapterNumber
	} else if candidateGap != nil {
		preview.FirstGapChapterNumber = candidateGap
	}
	return preview, nil
}

// FinalizedImportPreview is db:import-run-author-preview: FinalizedDraftImportRepository.preview
// (:311-389). It is a pure read - the classification, the two fingerprints and
// every chapter disposition come from rows already committed - so the caller can
// show it before it commits anything.
//
// The refusal at :297-298 is ErrProjectNotOpen, which is also this package's
// absence refusal: without an admitted project database there is no authority to
// preview against. The author lane additionally requires the manifest and
// authority fingerprints it hands to the commit (import-run-repository.ts:2021-2029);
// that binding is the caller's, and the commit re-checks both.
func (r *Drafts) FinalizedImportPreview(ctx context.Context, items []FinalizedDraftImportChapter) (*AuthorManuscriptImportPreview, error) {
	if r == nil || r.db == nil {
		// :296-297
		return nil, ErrProjectNotOpen
	}
	chapters, err := finalizedImportNormalizeChapters(items)
	if err != nil {
		return nil, err
	}
	return finalizedImportPreviewBody(ctx, r.db, chapters)
}

// FinalizedImportCommittedOperation is getCommittedOperation (:292-309), the
// replay probe of the author lane: an operation that already committed answers
// its own receipt, and only after every fact it froze is proven unchanged. A
// missing operation is (nil, nil), the original's null.
func (r *Drafts) FinalizedImportCommittedOperation(ctx context.Context, operationID string, items []FinalizedDraftImportChapter) (*FinalizedDraftImportReceipt, error) {
	if r == nil || r.db == nil {
		// :296-297
		return nil, ErrProjectNotOpen
	}
	normalizedOperationID, err := finalizedImportRequireOperationID(operationID)
	if err != nil {
		return nil, err
	}
	chapters, err := finalizedImportNormalizeChapters(items)
	if err != nil {
		return nil, err
	}
	row, present, err := finalizedImportReadOperation(ctx, r.db, normalizedOperationID)
	if err != nil {
		return nil, err
	}
	if !present {
		// :305 - null, not a refusal.
		return nil, nil
	}
	// :306-308
	receipt, err := finalizedImportParseReceipt(row)
	if err != nil {
		return nil, err
	}
	if err := finalizedImportVerifyStoredFacts(ctx, r.db, receipt, chapters); err != nil {
		return nil, err
	}
	return receipt, nil
}

// FinalizedImportCommit is db:draft-import-finalized-batch:
// FinalizedDraftImportRepository.commit (:391-505), the author's manuscript
// becoming finalized chapters.
//
// One BEGIN IMMEDIATE covers the whole batch (this file's header), so a refusal
// anywhere leaves the database byte-identical and no chapter half-written. A
// replay of a committed operation is answered from its receipt (:406-413), and
// only an identical payload - under any of the three historical word counters -
// may replay it (:407-409).
//
// The two expectation guards are the ones the preview hands over: the commit
// manifest must still be this exact chapter set (:417-421) and the authority must
// still be the one that was previewed (:422-431), after which the preview itself
// must still classify the batch 'ready'. Only then is a chapter that is already
// finalized refused (:437).
func (r *Drafts) FinalizedImportCommit(ctx context.Context, request FinalizedDraftImportRequest) (*FinalizedDraftImportReceipt, error) {
	if r == nil || r.db == nil {
		// :392-393 - no live project database behind the call.
		return nil, ErrProjectNotOpen
	}
	// :394-398 - the id and the payload are validated before the transaction, so
	// a malformed batch never even takes the write lock.
	operationID, err := finalizedImportRequireOperationID(request.OperationID)
	if err != nil {
		return nil, err
	}
	chapters, err := finalizedImportNormalizeChapters(request.Chapters)
	if err != nil {
		return nil, err
	}
	normalized := request
	normalized.OperationID = operationID
	payloadHash, err := finalizedImportPayloadHash(normalized, chapters)
	if err != nil {
		return nil, err
	}
	accepted, err := finalizedImportPayloadHashCandidates(normalized, chapters)
	if err != nil {
		return nil, err
	}
	var receipt *FinalizedDraftImportReceipt
	err = NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		// :401-405 - the replay probe, inside the transaction so a concurrent
		// commit of the same operation cannot slip past it.
		existing, present, err := finalizedImportReadOperation(ctx, conn, operationID)
		if err != nil {
			return err
		}
		if present {
			if !accepted[existing.payloadHash] {
				// :407-409
				return ErrFinalizedImportOperationConflict
			}
			stored, err := finalizedImportParseReceipt(existing)
			if err != nil {
				// :410
				return err
			}
			if err := finalizedImportVerifyStoredFacts(ctx, conn, stored, chapters); err != nil {
				// :411
				return err
			}
			// :412 - the receipt answers idempotent, and nothing is written.
			replay := *stored
			replay.Idempotent = true
			receipt = &replay
			return nil
		}
		// :415-416 - the commit fingerprint is the preview's own manifest; the
		// older expectedManifestFingerprint is the fallback of an older caller.
		expectedCommit := request.ExpectedCommitManifestFingerprint
		if expectedCommit == nil {
			expectedCommit = request.ExpectedManifestFingerprint
		}
		if expectedCommit != nil {
			manifest, err := finalizedImportManifestFingerprint(chapters)
			if err != nil {
				return err
			}
			if *expectedCommit != manifest {
				// :418-420
				return ErrFinalizedImportPreviewStale
			}
		}
		if request.ExpectedAuthorityFingerprint != nil {
			// :423 - the authority is read again here, inside the transaction.
			rows, err := finalizedImportAuthorityRows(ctx, conn)
			if err != nil {
				return err
			}
			sequence, err := finalizedImportSequenceFromRows(rows)
			if err != nil {
				return err
			}
			if *request.ExpectedAuthorityFingerprint != sequence.authorityFingerprint {
				// :424-426
				return ErrFinalizedImportPreviewStale
			}
			// :427-430 - the reference re-runs its whole preview here, so a
			// batch the preview classified as anything but 'ready' is refused
			// even though the fingerprints matched.
			preview, err := finalizedImportPreviewBody(ctx, conn, chapters)
			if err != nil {
				return err
			}
			if preview.Classification != "ready" {
				return ErrFinalizedImportAuthorityConflict
			}
		}
		// :433-438 - a chapter this batch targets is already finalized. The
		// original short-circuits at the first one, so this loop refuses at the
		// same statement count.
		for _, chapter := range chapters {
			var found int64
			err := conn.QueryRowContext(ctx, `SELECT 1 FROM drafts
 WHERE chapter_number = ? AND status = 'finalized' LIMIT 1`, chapter.chapterNumber).Scan(&found)
			if err == nil {
				return ErrFinalizedImportChapterAlreadyFinalized
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		drafts := make([]FinalizedDraftImportDraftReceipt, 0, len(chapters))
		chapterNumbers := make([]int64, 0, len(chapters))
		for _, chapter := range chapters {
			// :442-446 - the next version of this chapter, assigned inside the
			// transaction so the unique (chapter_number, version) index cannot be
			// raced by a second writer.
			var maxVersion sql.NullInt64
			if err := conn.QueryRowContext(ctx, `SELECT MAX(version) FROM drafts WHERE chapter_number = ?`,
				chapter.chapterNumber).Scan(&maxVersion); err != nil {
				return err
			}
			// :445-446
			contentResult, err := conn.ExecContext(ctx, `INSERT INTO contents (body) VALUES (?)`, chapter.content)
			if err != nil {
				return err
			}
			contentID, err := contentResult.LastInsertId()
			if err != nil {
				return err
			}
			// :447-456 - the imported chapter is born finalized, as 'write', with
			// the count the caller's payload was validated against.
			draftResult, err := conn.ExecContext(ctx, `INSERT INTO drafts
 (chapter_number, version, status, source, content_id, word_count)
 VALUES (?, ?, 'finalized', 'write', ?, ?)`,
				chapter.chapterNumber, maxVersion.Int64+1, contentID, chapter.wordCount)
			if err != nil {
				return err
			}
			draftID, err := draftResult.LastInsertId()
			if err != nil {
				return err
			}
			// :457-464 - the frozen identity and the frozen target name. The
			// reference resolves the target against the project root here; see
			// this file's header for the arm that is not ported.
			frozenContentHash := draftBodyHash(chapter.content)
			frozenFinalizationID := finalizedImportFinalizationID(operationID, chapter.chapterNumber)
			targetFileName := finalizedImportTargetFileName(chapter.chapterNumber, chapter.title)
			// :466-480 - the publication outbox row, pending and unretried.
			if _, err := conn.ExecContext(ctx, `INSERT INTO finalization_outbox (
 finalization_id, draft_id, chapter_number, chapter_title,
 content_hash, content_revision, content_snapshot, target_file_name,
 publication_status, last_error
) VALUES (?, ?, ?, ?, ?, 1, ?, ?, 'pending', '')`,
				frozenFinalizationID, draftID, chapter.chapterNumber, chapter.title,
				frozenContentHash, chapter.content, targetFileName); err != nil {
				return err
			}
			// :480-488
			chapterNumbers = append(chapterNumbers, chapter.chapterNumber)
			drafts = append(drafts, FinalizedDraftImportDraftReceipt{ChapterNumber: chapter.chapterNumber,
				DraftID: draftID, FinalizationID: frozenFinalizationID, ContentHash: frozenContentHash,
				TargetFileName: targetFileName, Status: "finalized", PublicationStatus: "pending"})
		}
		built := &FinalizedDraftImportReceipt{OperationID: operationID, PayloadHash: payloadHash,
			ChapterNumbers: chapterNumbers, Drafts: drafts, Idempotent: false}
		// :498-501 - the receipt is frozen only after every chapter succeeded,
		// and in the same transaction as them.
		encoded, err := jsonMarshalCompact(built)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO finalized_draft_import_operations
 (operation_id, payload_hash, receipt_json) VALUES (?, ?, ?)`,
			operationID, payloadHash, encoded); err != nil {
			return err
		}
		receipt = built
		return nil
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}
