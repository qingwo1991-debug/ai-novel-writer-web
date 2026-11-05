package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// The three authoritative-export reads of the durable lane. They are pure
// reads over drafts, contents and finalization_outbox - no generation owner, no
// source project, no LanceDB, no filesystem - and mirror the original:
//
//	db:draft-authority-sequence       FinalizedDraftImportRepository.authoritySequence()
//	                                  (electron/repositories/finalized-draft-import-repository.ts:288,
//	                                   authorityRows :142-154, sequenceFromRows :165-205)
//	db:draft-export-snapshot          FinalizationRepository.listAuthoritativeForExport()
//	                                  (electron/repositories/finalization-repository.ts:207,
//	                                   selectAuthoritativeExportRows :121-176,
//	                                   readAuthoritativeExportRows :178-194)
//	db:draft-export-authority-current FinalizationRepository.matchesAuthoritativeExportReceipt()
//	                                  (electron/repositories/finalization-repository.ts:211-241)
//
// Every refusal below is the original's own code so the transport can reflect
// it unchanged.
var (
	ErrDraftExportDuplicateChapter = errors.New("FINALIZATION_EXPORT_DUPLICATE_CHAPTER")
	ErrDraftExportSnapshotInvalid  = errors.New("FINALIZATION_EXPORT_SNAPSHOT_INVALID")
	ErrDraftExportContentEmpty     = errors.New("FINALIZATION_EXPORT_CONTENT_EMPTY")
	ErrDraftExportIdentityMismatch = errors.New("FINALIZATION_EXPORT_IDENTITY_MISMATCH")
	ErrDraftExportContentMismatch  = errors.New("FINALIZATION_EXPORT_CONTENT_MISMATCH")
	ErrDraftExportSummaryMismatch  = errors.New("FINALIZATION_EXPORT_SUMMARY_MISMATCH")
)

// draftAuthorityFingerprintRow is one element of the JSON payload the original
// hashes. Field order is the object literal order in the TypeScript, so the
// marshaled bytes and therefore the fingerprint match it exactly.
type draftAuthorityFingerprintRow struct {
	DraftID       int64  `json:"draftId"`
	ChapterNumber int64  `json:"chapterNumber"`
	Version       int64  `json:"version"`
	BodyHash      string `json:"bodyHash"`
}

type draftAuthorityRow struct {
	draftID       int64
	chapterNumber int64
	version       int64
	body          string
}

// draftAuthorityRows is authorityRows(): every finalized draft joined to its
// body, ordered by chapter then version then id ascending.
func (r *Drafts) draftAuthorityRows(ctx context.Context) ([]draftAuthorityRow, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT drafts.id, drafts.chapter_number, drafts.version,
 contents.body FROM drafts JOIN contents ON contents.id = drafts.content_id
 WHERE drafts.status = 'finalized'
 ORDER BY drafts.chapter_number ASC, drafts.version ASC, drafts.id ASC`)
	if err != nil {
		return nil, err
	}
	result := make([]draftAuthorityRow, 0)
	for rows.Next() {
		var row draftAuthorityRow
		if err := rows.Scan(&row.draftID, &row.chapterNumber, &row.version, &row.body); err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, row)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	return result, nil
}

func draftBodyHash(body string) string {
	digest := sha256.Sum256([]byte(body))
	return hex.EncodeToString(digest[:])
}

// draftAuthorityFingerprint is authorityFingerprint(): the sha256 of the JSON
// array of {draftId, chapterNumber, version, bodyHash}. The empty array must
// marshal to "[]" rather than "null", so the slice is never nil.
func draftAuthorityFingerprint(rows []draftAuthorityRow) (string, error) {
	payload := make([]draftAuthorityFingerprintRow, 0, len(rows))
	for _, row := range rows {
		payload = append(payload, draftAuthorityFingerprintRow{DraftID: row.draftID,
			ChapterNumber: row.chapterNumber, Version: row.version, BodyHash: draftBodyHash(row.body)})
	}
	encoded, err := jsonMarshalCompact(payload)
	if err != nil {
		return "", err
	}
	return draftBodyHash(encoded), nil
}

// AuthoritySequence is sequenceFromRows(): the continuity verdict the import
// pipeline and the draft tab both read. It reports empty, continuous or invalid
// with exactly the original's key set - the invalid verdict omits
// nextChapterNumber, and a contiguous chapter run omits firstGapChapterNumber.
func (r *Drafts) AuthoritySequence(ctx context.Context) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	rows, err := r.draftAuthorityRows(ctx)
	if err != nil {
		return nil, err
	}
	fingerprint, err := draftAuthorityFingerprint(rows)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return map[string]any{"status": "empty", "lastChapterNumber": int64(0),
			"nextChapterNumber": int64(1), "duplicateChapterNumbers": make([]int64, 0),
			"authorityFingerprint": fingerprint}, nil
	}
	counts := make(map[int64]int64)
	maxChapter := int64(0)
	for _, row := range rows {
		counts[row.chapterNumber]++
		if row.chapterNumber > maxChapter {
			maxChapter = row.chapterNumber
		}
	}
	duplicates := make([]int64, 0)
	for chapter, count := range counts {
		if count > 1 {
			duplicates = append(duplicates, chapter)
		}
	}
	// The original builds this list from Map iteration order, then only tests it
	// for emptiness; a stable order keeps the wire deterministic.
	sort.Slice(duplicates, func(i, j int) bool { return duplicates[i] < duplicates[j] })
	var firstGap *int64
	for chapter := int64(1); chapter <= maxChapter; chapter++ {
		if counts[chapter] == 0 {
			gap := chapter
			firstGap = &gap
			break
		}
	}
	if len(duplicates) != 0 || firstGap != nil {
		invalid := map[string]any{"status": "invalid", "lastChapterNumber": maxChapter,
			"duplicateChapterNumbers": duplicates, "authorityFingerprint": fingerprint}
		if firstGap != nil {
			invalid["firstGapChapterNumber"] = *firstGap
		}
		return invalid, nil
	}
	return map[string]any{"status": "continuous", "lastChapterNumber": maxChapter,
		"nextChapterNumber": maxChapter + 1, "duplicateChapterNumbers": duplicates,
		"authorityFingerprint": fingerprint}, nil
}

// DraftExportSnapshotRow is one authoritative export snapshot. FinalizationID
// is nil for a finalized draft that never got a finalization receipt, which is
// the original's finalizationId: null.
type DraftExportSnapshotRow struct {
	DraftID        int64
	ChapterNumber  int64
	Version        int64
	FinalizationID *string
	ContentHash    string
	Title          string
	Content        string
}

// ExportSnapshot is listAuthoritativeForExport(): one transactionally frozen,
// highest-version finalized fact per chapter, with the original's integrity
// checks. A finalized draft without an outbox row is accepted; one with an
// outbox row must agree with it on chapter identity, frozen body and hash.
func (r *Drafts) ExportSnapshot(ctx context.Context) ([]map[string]any, error) {
	rows, err := r.exportSnapshotRows(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var finalizationID any
		if row.FinalizationID != nil {
			finalizationID = *row.FinalizationID
		}
		result = append(result, map[string]any{"draftId": row.DraftID,
			"chapterNumber": row.ChapterNumber, "version": row.Version,
			"finalizationId": finalizationID, "contentHash": row.ContentHash,
			"title": row.Title, "content": row.Content})
	}
	return result, nil
}

// exportSnapshotRows is selectAuthoritativeExportRows() over
// readAuthoritativeExportRows(). It reads the ordered rows and applies the
// first-row-per-chapter selection with the original's validations in one
// read-only transaction.
func (r *Drafts) exportSnapshotRows(ctx context.Context) ([]DraftExportSnapshotRow, error) {
	if r == nil || r.db == nil {
		return nil, ErrProjectNotOpen
	}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT drafts.id, drafts.chapter_number, drafts.version,
 contents.body, finalization_outbox.finalization_id,
 finalization_outbox.chapter_number, finalization_outbox.chapter_title,
 finalization_outbox.content_hash, finalization_outbox.content_snapshot
 FROM drafts JOIN contents ON contents.id = drafts.content_id
 LEFT JOIN finalization_outbox ON finalization_outbox.draft_id = drafts.id
 WHERE drafts.status = 'finalized'
 ORDER BY drafts.chapter_number ASC, drafts.version DESC, drafts.id DESC`)
	if err != nil {
		return nil, err
	}
	type exportRow struct {
		draftID         int64
		chapterNumber   int64
		version         int64
		body            string
		finalizationID  sql.NullString
		outboxChapter   sql.NullInt64
		chapterTitle    sql.NullString
		contentHash     sql.NullString
		contentSnapshot sql.NullString
	}
	raw := make([]exportRow, 0)
	for rows.Next() {
		var row exportRow
		if err := rows.Scan(&row.draftID, &row.chapterNumber, &row.version, &row.body,
			&row.finalizationID, &row.outboxChapter, &row.chapterTitle, &row.contentHash,
			&row.contentSnapshot); err != nil {
			rows.Close()
			return nil, err
		}
		raw = append(raw, row)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	selected := make([]DraftExportSnapshotRow, 0, len(raw))
	selectedVersions := make(map[int64]int64)
	for _, row := range raw {
		if version, seen := selectedVersions[row.chapterNumber]; seen {
			if version == row.version {
				return nil, ErrDraftExportDuplicateChapter
			}
			continue
		}
		if row.draftID < 1 || row.chapterNumber < 1 || row.version < 1 {
			return nil, ErrDraftExportSnapshotInvalid
		}
		if trimJS(row.body) == "" {
			return nil, ErrDraftExportContentEmpty
		}
		contentHash := draftBodyHash(row.body)
		var finalizationID *string
		if row.finalizationID.Valid {
			if trimJS(row.finalizationID.String) == "" {
				return nil, ErrDraftExportSnapshotInvalid
			}
			if !row.outboxChapter.Valid || row.outboxChapter.Int64 != row.chapterNumber {
				return nil, ErrDraftExportIdentityMismatch
			}
			// A NULL frozen snapshot can never equal the body, exactly like the
			// original's `row.content_snapshot !== row.body`.
			if !row.contentSnapshot.Valid || row.contentSnapshot.String != row.body {
				return nil, ErrDraftExportContentMismatch
			}
			if !row.contentHash.Valid || row.contentHash.String != contentHash {
				return nil, ErrDraftExportSummaryMismatch
			}
			value := row.finalizationID.String
			finalizationID = &value
		}
		selectedVersions[row.chapterNumber] = row.version
		selected = append(selected, DraftExportSnapshotRow{DraftID: row.draftID,
			ChapterNumber: row.chapterNumber, Version: row.version,
			FinalizationID: finalizationID, ContentHash: contentHash,
			Title: trimJS(row.chapterTitle.String), Content: row.body})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return selected, nil
}

// DraftExportAuthorityItem is one validated element of the browser's export
// authority receipt. The transport has already proven every field shape, so a
// nil FinalizationID here is the original's explicit null.
type DraftExportAuthorityItem struct {
	DraftID        int64
	ChapterNumber  int64
	Version        int64
	FinalizationID *string
	ContentHash    string
}

// ExportAuthorityCurrent is matchesAuthoritativeExportReceipt(): the receipt is
// current only if it names exactly the same drafts, in the same order, with the
// same identities and body hashes. A read that refuses is `false`, never an
// error, exactly like the original's catch-all.
func (r *Drafts) ExportAuthorityCurrent(ctx context.Context, receipt []DraftExportAuthorityItem) (bool, error) {
	rows, err := r.exportSnapshotRows(ctx)
	if err != nil {
		return false, nil
	}
	if len(rows) != len(receipt) {
		return false, nil
	}
	for index, row := range rows {
		expected := receipt[index]
		if row.DraftID != expected.DraftID || row.ChapterNumber != expected.ChapterNumber ||
			row.Version != expected.Version || row.ContentHash != expected.ContentHash {
			return false, nil
		}
		switch {
		case row.FinalizationID == nil && expected.FinalizationID == nil:
		case row.FinalizationID == nil || expected.FinalizationID == nil:
			return false, nil
		case *row.FinalizationID != *expected.FinalizationID:
			return false, nil
		}
	}
	return true, nil
}

// jsonMarshalCompact is the JSON.stringify equivalent the fingerprint needs:
// compact output with no HTML escaping and no trailing newline.
func jsonMarshalCompact(value any) (string, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buffer.String(), "\n"), nil
}
