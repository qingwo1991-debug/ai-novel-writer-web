package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

var (
	ErrFinalizationOwnerRequired = errors.New("FINALIZATION_OWNER_REQUIRED")
	ErrFinalizationInputInvalid = errors.New("FINALIZATION_INPUT_INVALID")
	ErrFinalizationCommitConflict = errors.New("FINALIZATION_COMMIT_CONFLICT")
	ErrFinalizationChapterMismatch = errors.New("FINALIZATION_COMMIT_CHAPTER_MISMATCH")
	ErrFinalizationOutboxMissing = errors.New("FINALIZATION_COMMIT_OUTBOX_MISSING")
	ErrFinalizationSuperseded = errors.New("FINALIZATION_COMMIT_SUPERSEDED")
	ErrFinalizationNotFound = errors.New("FINALIZATION_COMMIT_NOT_FOUND")
	ErrFinalizationSnapshotHashMismatch = errors.New("FINALIZATION_SNAPSHOT_HASH_MISMATCH")
	ErrFinalizationSchemaUnsupported = errors.New("FINALIZATION_SCHEMA_UNSUPPORTED")
)

// FinalizationScope is captured by the admitted-project owner, never resolved
// from request paths. Scope is not itself authorization.
type FinalizationScope struct {
	ProjectID string
	Epoch string
}

type FinalizationCommitInput struct {
	FinalizationID string `json:"finalizationId"`
	DraftID int64 `json:"draftId"`
	ChapterNumber int64 `json:"chapterNumber"`
	ChapterTitle string `json:"chapterTitle"`
	Content string `json:"content"`
	ExpectedContent *string `json:"expectedContent"`
	ContentHash string `json:"contentHash"`
	ContentRevision int64 `json:"contentRevision"`
	TargetFileName string `json:"targetFileName"`
}

type FinalizationRecord struct {
	FinalizationID string `json:"finalizationId"`
	DraftID int64 `json:"draftId"`
	ChapterNumber int64 `json:"chapterNumber"`
	ChapterTitle string `json:"chapterTitle"`
	ContentSnapshot string `json:"contentSnapshot"`
	ContentHash string `json:"contentHash"`
	ContentRevision int64 `json:"contentRevision"`
	TargetFileName string `json:"targetFileName"`
	KnowledgeDocumentID string `json:"knowledgeDocumentId"`
	PublicationStatus string `json:"publicationStatus"`
	LastError string `json:"lastError"`
	PublishedAt *string `json:"publishedAt"`
}

// FinalizationMutation is a value snapshot of exactly what is being authorized.
// Existing is nil only for a fresh commit. A replay is still owner-authorized.
type FinalizationMutation struct {
	Action string // commit, publication-pending, publication-published
	Input FinalizationCommitInput
	Existing *FinalizationRecord
	LastError string
}

// FinalizationOwnerProofVerifier MUST be injected by the trusted handler owner.
// It must bind opaque proof to this captured database/project/epoch, the exact
// operation and immutable source, enforce portable freeze and lifecycle/source
// guards, and reject renderer-manufactured proof. For publication-published it
// must verify the real publisher's receipt, not a client success boolean. It runs
// under the SAME write lock; it must use conn, not borrow another DB connection.
// No permissive/default implementation or proof minting API exists here.
type FinalizationOwnerProofVerifier interface {
	VerifyFinalizationOwnerProof(context.Context, *sql.Conn, FinalizationScope, FinalizationMutation, any) error
}

// FinalizationOutbox borrows an already admitted *sql.DB. This is an isolated
// storage foundation, NOT an RPC handler, generation owner or file publisher.
// No paths are opened, no schemas are migrated, and no publication is performed.
type FinalizationOutbox struct {
	db *sql.DB
	scope FinalizationScope
	owner FinalizationOwnerProofVerifier
}

func NewFinalizationOutbox(db *sql.DB, scope FinalizationScope, owner FinalizationOwnerProofVerifier) *FinalizationOutbox {
	return &FinalizationOutbox{db: db, scope: scope, owner: owner}
}

func (r *FinalizationOutbox) authorize(ctx context.Context, conn *sql.Conn, mutation FinalizationMutation, proof any) error {
	if r.owner == nil || proof == nil || trimJS(r.scope.ProjectID) == "" || trimJS(r.scope.Epoch) == "" { return ErrFinalizationOwnerRequired }
	// Do not let even a verifier accidentally rewrite the caller's CAS baseline
	// or returned record through shared pointers.
	if mutation.Input.ExpectedContent != nil { value := *mutation.Input.ExpectedContent; mutation.Input.ExpectedContent = &value }
	if mutation.Existing != nil { value := *mutation.Existing; if value.PublishedAt != nil { stamp := *value.PublishedAt; value.PublishedAt = &stamp }; mutation.Existing = &value }
	return r.owner.VerifyFinalizationOwnerProof(ctx, conn, r.scope, mutation, proof)
}

const finalizationSelect = `SELECT finalization_id,draft_id,chapter_number,chapter_title,
 content_snapshot,content_hash,content_revision,target_file_name,COALESCE(knowledge_document_id,''),
 publication_status,COALESCE(last_error,''),published_at FROM finalization_outbox`

func finalizationRecords(ctx context.Context, q rrQueryer, suffix string, args ...any) ([]FinalizationRecord, error) {
	rows, err := q.QueryContext(ctx, finalizationSelect+suffix, args...)
	if err != nil { return nil, err }
	defer rows.Close()
	result := make([]FinalizationRecord, 0)
	for rows.Next() {
		var row FinalizationRecord
		var body any
		var published sql.NullString
		if err := rows.Scan(&row.FinalizationID, &row.DraftID, &row.ChapterNumber, &row.ChapterTitle,
			&body, &row.ContentHash, &row.ContentRevision, &row.TargetFileName, &row.KnowledgeDocumentID,
			&row.PublicationStatus, &row.LastError, &published); err != nil { return nil, err }
		if body != nil { var ok bool; row.ContentSnapshot, ok = body.(string); if !ok { return nil, ErrFinalizationSnapshotHashMismatch } }
		if published.Valid { stamp := published.String; row.PublishedAt = &stamp }
		result = append(result, row)
	}
	if err := rows.Err(); err != nil { return nil, err }
	if err := rows.Close(); err != nil { return nil, err }
	return result, nil
}

func finalizationOne(rows []FinalizationRecord, err error) (*FinalizationRecord, error) {
	if err != nil || len(rows) == 0 { return nil, err }
	return &rows[0], nil
}

func (r *FinalizationOutbox) Get(ctx context.Context, id string) (*FinalizationRecord, error) {
	if r == nil || r.db == nil { return nil, ErrReviewRevisionProjectNotOpen }
	return finalizationOne(finalizationRecords(ctx, r.db, " WHERE finalization_id=?", id))
}

func (r *FinalizationOutbox) GetByDraftID(ctx context.Context, id int64) (*FinalizationRecord, error) {
	if r == nil || r.db == nil { return nil, ErrReviewRevisionProjectNotOpen }
	return finalizationOne(finalizationRecords(ctx, r.db, " WHERE draft_id=?", id))
}

// ListPending returns frozen projection jobs, including superseded jobs. The
// publisher owner must re-prove authority before publication; listing grants none.
func (r *FinalizationOutbox) ListPending(ctx context.Context) ([]FinalizationRecord, error) {
	if r == nil || r.db == nil { return nil, ErrReviewRevisionProjectNotOpen }
	return finalizationRecords(ctx, r.db, " WHERE publication_status='pending' ORDER BY chapter_number,draft_id,finalization_id")
}

func finalizationInputValid(input FinalizationCommitInput) bool {
	for _, value := range []string{input.FinalizationID, input.ChapterTitle, input.Content, input.ContentHash, input.TargetFileName} {
		if !summarySupportedText(value) { return false }
	}
	if input.ExpectedContent != nil && !summarySupportedText(*input.ExpectedContent) { return false }
	if _, ok := rrInteger(input.DraftID, 1); !ok { return false }
	if _, ok := rrInteger(input.ChapterNumber, 1); !ok { return false }
	if _, ok := rrInteger(input.ContentRevision, 0); !ok { return false }
	// Stored target is a portable direct-child name, never an arbitrary path.
	name := input.TargetFileName
	if trimJS(input.FinalizationID) == "" || trimJS(name) == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\:<>\"|?*\x00") { return false }
	for _, c := range name { if c < 32 { return false } }
	return summaryHashValid(input.ContentHash) && summaryHash(input.Content) == input.ContentHash
}

func finalizationSame(existing *FinalizationRecord, input FinalizationCommitInput) bool {
	return existing.DraftID == input.DraftID && existing.ChapterNumber == input.ChapterNumber &&
		existing.ChapterTitle == input.ChapterTitle && existing.ContentHash == input.ContentHash &&
		existing.ContentRevision == input.ContentRevision && existing.ContentSnapshot == input.Content
}

// CommitWithOwnerProof atomically freezes author-confirmed prose and the outbox.
// ExpectedContent is the acknowledged full persisted body, NOT a hash or new
// server-read baseline. Existing exact frozen retries return their original ID
// and filename, ignoring replacement candidates, even after live-body drift.
func (r *FinalizationOutbox) CommitWithOwnerProof(ctx context.Context, input FinalizationCommitInput, proof any) (*FinalizationRecord, error) {
	if r == nil || r.db == nil { return nil, ErrReviewRevisionProjectNotOpen }
	if !finalizationInputValid(input) { return nil, ErrFinalizationInputInvalid }
	if input.ExpectedContent != nil { value := *input.ExpectedContent; input.ExpectedContent = &value }
	var output *FinalizationRecord
	err := NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		existing, err := finalizationOne(finalizationRecords(ctx, conn, " WHERE draft_id=?", input.DraftID))
		if err != nil { return err }
		if err := r.authorize(ctx, conn, FinalizationMutation{Action: "commit", Input: input, Existing: existing}, proof); err != nil { return err }
		if existing != nil {
			if !finalizationSame(existing, input) { return ErrFinalizationCommitConflict }
			output = existing
			return nil
		}
		rows, err := rrMaps(ctx, conn, `SELECT d.chapter_number AS chapter,d.status,d.content_id AS contentId,c.body
 FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=?`, input.DraftID)
		if err != nil { return err }
		if len(rows) == 0 { return ErrDraftNotFound }
		draft := rows[0]
		if !rrEqual(draft["chapter"], input.ChapterNumber) { return ErrFinalizationChapterMismatch }
		if draft["status"] == "finalized" { return ErrFinalizationOutboxMissing }
		if draft["status"] == "superseded" { return ErrFinalizationSuperseded }
		status, ok := draft["status"].(string)
		if !ok || !IsWorkingDraftStatus(status) || input.ExpectedContent == nil || !rrEqual(draft["body"], *input.ExpectedContent) { return ErrFinalizationCommitConflict }
		if err := rrExclusiveDraftContent(ctx, conn, input.DraftID, draft["contentId"]); err != nil { return err }
		// Observe required modern schema; never synthesize it with native DDL.
		var generation int64
		if err := conn.QueryRowContext(ctx, "SELECT generation FROM continuity_projection_meta WHERE id='main'").Scan(&generation); err != nil { return errors.Join(ErrFinalizationSchemaUnsupported, err) }
		replaced, err := conn.ExecContext(ctx, `UPDATE drafts SET status='superseded',updated_at=datetime('now')
 WHERE chapter_number=? AND status='finalized' AND id<>?`, input.ChapterNumber, input.DraftID)
		if err != nil { return err }
		n, err := replaced.RowsAffected()
		if err != nil { return err }
		if n > 0 {
			result, err := conn.ExecContext(ctx, `UPDATE continuity_projection_meta SET generation=generation+1,
 stale_from_chapter=CASE WHEN stale_from_chapter IS NULL OR stale_from_chapter>? THEN ? ELSE stale_from_chapter END WHERE id='main'`, input.ChapterNumber, input.ChapterNumber)
			if err := rrAffected(result, err, ErrFinalizationSchemaUnsupported); err != nil { return err }
		}
		result, err := conn.ExecContext(ctx, "UPDATE contents SET body=? WHERE id=? AND body=?", input.Content, draft["contentId"], *input.ExpectedContent)
		if err := rrAffected(result, err, ErrFinalizationCommitConflict); err != nil { return err }
		result, err = conn.ExecContext(ctx, `UPDATE drafts SET status='finalized',word_count=?,updated_at=datetime('now')
 WHERE id=? AND chapter_number=? AND status=? AND content_id=?`, finalizationDraftUnits(input.Content), input.DraftID, input.ChapterNumber, status, draft["contentId"])
		if err := rrAffected(result, err, ErrFinalizationCommitConflict); err != nil { return err }
		_, err = conn.ExecContext(ctx, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,
 content_hash,content_revision,content_snapshot,target_file_name,publication_status,last_error) VALUES(?,?,?,?,?,?,?,?,'pending','')`,
			input.FinalizationID, input.DraftID, input.ChapterNumber, input.ChapterTitle, input.ContentHash, input.ContentRevision, input.Content, input.TargetFileName)
		if err != nil { return err }
		if err := finalizationFreezeCharacters(ctx, conn, input); err != nil { return err }
		output, err = finalizationOne(finalizationRecords(ctx, conn, " WHERE draft_id=?", input.DraftID))
		if err != nil { return err }
		if output == nil { return ErrFinalizationNotFound }
		return nil
	})
	if err != nil { return nil, err }
	return output, nil
}

// These methods record a trusted publisher's outcome only. They neither publish
// files nor claim that any public/native finalization operation is complete.
func (r *FinalizationOutbox) MarkPublicationPendingWithOwnerProof(ctx context.Context, id, lastError string, proof any) (*FinalizationRecord, error) {
	return r.markProjection(ctx, id, lastError, false, proof)
}
func (r *FinalizationOutbox) MarkPublishedWithOwnerProof(ctx context.Context, id string, proof any) (*FinalizationRecord, error) {
	return r.markProjection(ctx, id, "", true, proof)
}
func (r *FinalizationOutbox) markProjection(ctx context.Context, id, lastError string, published bool, proof any) (*FinalizationRecord, error) {
	if r == nil || r.db == nil { return nil, ErrReviewRevisionProjectNotOpen }
	if !summarySupportedText(id) || !summarySupportedText(lastError) { return nil, ErrFinalizationInputInvalid }
	var output *FinalizationRecord
	err := NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		existing, err := finalizationOne(finalizationRecords(ctx, conn, " WHERE finalization_id=?", id))
		if err != nil { return err }
		if existing == nil { return ErrFinalizationNotFound }
		if summaryHash(existing.ContentSnapshot) != existing.ContentHash { return ErrFinalizationSnapshotHashMismatch }
		action := "publication-pending"
		if published { action = "publication-published" }
		if err := r.authorize(ctx, conn, FinalizationMutation{Action: action, Existing: existing, LastError: lastError}, proof); err != nil { return err }
		query := "UPDATE finalization_outbox SET publication_status='pending',last_error=?,updated_at=datetime('now') WHERE finalization_id=?"
		args := []any{lastError, id}
		if published { query = "UPDATE finalization_outbox SET publication_status='published',last_error='',published_at=datetime('now'),updated_at=datetime('now') WHERE finalization_id=?"; args = []any{id} }
		result, err := conn.ExecContext(ctx, query, args...)
		if err := rrAffected(result, err, ErrFinalizationNotFound); err != nil { return err }
		output, err = finalizationOne(finalizationRecords(ctx, conn, " WHERE finalization_id=?", id))
		return err
	})
	if err != nil { return nil, err }
	return output, nil
}

func finalizationFreezeCharacters(ctx context.Context, conn *sql.Conn, input FinalizationCommitInput) error {
	var exists int
	err := conn.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='character_identity_meta'").Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) { return nil } // Actual pre-M02 original behavior.
	if err != nil { return err }
	var revision int64
	if err := conn.QueryRowContext(ctx, "SELECT revision FROM character_identity_meta WHERE id='main'").Scan(&revision); err != nil { return err }
	if _, ok := rrInteger(revision, 0); !ok { return ErrFinalizationSchemaUnsupported }
	rows, err := rrMaps(ctx, conn, "SELECT character_id,name FROM characters WHERE retired=0 ORDER BY character_id")
	if err != nil { return err }
	characters := []any{}
	for _, row := range rows {
		id, idOK := row["character_id"].(string)
		name, nameOK := row["name"].(string)
		if !idOK || !nameOK || !utf8.ValidString(id) || !utf8.ValidString(name) { return ErrFinalizationSchemaUnsupported }
		aliases, err := rrMaps(ctx, conn, `SELECT DISTINCT name FROM character_aliases WHERE character_id=?
 AND valid_from<=? AND (valid_through IS NULL OR valid_through>=?) ORDER BY name`, id, revision, revision)
		if err != nil { return err }
		names := []any{}
		for _, alias := range aliases { value, ok := alias["name"].(string); if !ok || !utf8.ValidString(value) { return ErrFinalizationSchemaUnsupported }; names = append(names, value) }
		characters = append(characters, plotObject{{"characterId", id}, {"displayNameSnapshot", name}, {"aliases", names}})
	}
	source := plotObject{{"draftId", input.DraftID}, {"finalizationId", input.FinalizationID}, {"chapterNumber", input.ChapterNumber}, {"contentHash", input.ContentHash}}
	raw, err := plotJSON(plotObject{{"kind", "finalized-character-snapshot:v1"}, {"source", source}, {"identityRevision", revision}, {"characters", characters}})
	if err != nil { return err }
	_, err = conn.ExecContext(ctx, `INSERT INTO character_identity_proposals(proposal_id,source_key,source_hash,raw_value,candidate_ids_json)
 VALUES(?,'finalized-character-snapshot:v1',?,?,'[]')`, "fcs:"+input.FinalizationID, summaryHash(raw), raw)
	return err
}

// Native counterpart of draft-units.ts v3. NFC affects counts only; persisted
// body, hash and CAS always retain the author's exact unnormalized UTF-8 bytes.
func finalizationDraftUnits(text string) int64 {
	runes := []rune(norm.NFC.String(text))
	var count int64
	for i := 0; i < len(runes); {
		c := runes[i]
		if unicode.Is(unicode.Han, c) { count++; i++; continue }
		if unicode.IsLetter(c) {
			count++; i++
			for i < len(runes) {
				c = runes[i]
				if !unicode.Is(unicode.Han, c) && (unicode.IsLetter(c) || unicode.IsMark(c)) { i++; continue }
				if (c == '\'' || c == '’') && i+1 < len(runes) && unicode.IsLetter(runes[i+1]) && !unicode.Is(unicode.Han, runes[i+1]) { i++; continue }
				break
			}
			continue
		}
		if !narrativeSpace(c) && !unicode.IsPunct(c) && !unicode.IsSymbol(c) { count++ }
		i++
	}
	return count
}
