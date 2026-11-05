package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// The three author draft mutations the durable lane serves beyond the body CAS.
// They are the plain SQLite writes of electron/repositories/draft-repository.ts
// (create :314-359, updateStatus :464-487, delete :542-572) and each keeps the
// original's own refusals as sentinels so the transport can reflect exactly the
// code the original raised. None of them needs the generation owner, a source
// project, LanceDB or a filesystem path.
var (
	ErrDraftFinalizedReadOnly          = errors.New("DRAFT_FINALIZED_READ_ONLY")
	ErrDraftFinalizationRequiresCommit = errors.New("DRAFT_FINALIZATION_REQUIRES_COMMIT")
	ErrFinalizedDraftDeleteRequired    = errors.New("FINALIZED_DRAFT_DELETE_REQUIRED")
	ErrSupersededDraftDeleteForbidden  = errors.New("SUPERSEDED_DRAFT_DELETE_FORBIDDEN")
	ErrDraftDependencyInvalid          = errors.New("GENERATION_DRAFT_DEPENDENCY_INVALID")
	ErrDraftDependencyStale            = errors.New("GENERATION_DRAFT_DEPENDENCY_STALE")
)

// receiptedDraftStatus is RECEIPTED_DRAFT_STATUSES in
// src/shared/draft-status.ts: only these two statuses carry a finalization
// receipt and are therefore read-only for status changes.
func receiptedDraftStatus(status string) bool {
	return status == "finalized" || status == "superseded"
}

// DraftCreateParams carries one db:draft-create request. Dependencies is the
// browser's raw sourceDependencies value (nil when absent or null), not a
// pre-parsed slice: the original stringifies the request value and re-parses
// that exact JSON, so the store must see the same bytes to reject the same
// shapes. Version is deliberately absent - the original ignores the requested
// version and allocates MAX(version)+1 inside its transaction.
type DraftCreateParams struct {
	ChapterNumber int64
	Source        string
	Content       string
	WordCount     int64
	Dependencies  any
}

const draftCreateSQL = `INSERT INTO drafts(chapter_number, version, source, content_id, word_count,
 source_dependencies) VALUES(?,?,?,?,?,?)`

// Create mirrors DraftRepository.create: one transaction allocates the version
// atomically, validates the frozen dependency list, then writes contents and
// drafts. A dependency that is stale or malformed is refused before either
// INSERT, exactly like the original, which throws inside the same transaction.
//
// The transaction is deferred rather than BEGIN IMMEDIATE because the version
// allocation reads first and dependency states have to be read on the same
// transaction, whose connection the dependency loader requires; the durable
// lane already holds the exclusive legacy writer lock, so no second writer can
// interleave between the read and the write.
func (r *Drafts) Create(ctx context.Context, params DraftCreateParams) (int64, error) {
	if r == nil || r.db == nil {
		return 0, ErrProjectNotOpen
	}
	serialized := "[]"
	if params.Dependencies != nil {
		encoded, err := json.Marshal(params.Dependencies)
		if err != nil {
			return 0, ErrDraftDependencyInvalid
		}
		serialized = string(encoded)
	}
	dependencies, valid := parseDependencies(sql.NullString{String: serialized, Valid: true})
	if !valid {
		return 0, ErrDraftDependencyInvalid
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	ids := make([]int64, 0, len(dependencies))
	for _, dependency := range dependencies {
		ids = append(ids, dependency.DraftID)
	}
	states, err := loadDependencyStates(ctx, tx, ids)
	if err != nil {
		return 0, err
	}
	if !dependenciesCurrent(dependencies, states, make(map[int64]bool)) {
		return 0, ErrDraftDependencyStale
	}
	var maxVersion sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		"SELECT MAX(version) FROM drafts WHERE chapter_number = ?", params.ChapterNumber).Scan(&maxVersion); err != nil {
		return 0, err
	}
	inserted, err := tx.ExecContext(ctx, "INSERT INTO contents(body) VALUES(?)", params.Content)
	if err != nil {
		return 0, err
	}
	contentID, err := inserted.LastInsertId()
	if err != nil {
		return 0, err
	}
	inserted, err = tx.ExecContext(ctx, draftCreateSQL, params.ChapterNumber, maxVersion.Int64+1,
		params.Source, contentID, params.WordCount, serialized)
	if err != nil {
		return 0, err
	}
	id, err := inserted.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// UpdateStatus mirrors DraftRepository.updateStatus: an unknown draft is a
// silent no-op, a receipted draft may not change status at all, and a working
// draft may not become receipted (finalization must commit that fact instead).
// wordCount nil is the original's `undefined`: the stored count is untouched.
func (r *Drafts) UpdateStatus(ctx context.Context, id int64, status string, wordCount *int64) error {
	if r == nil || r.db == nil {
		return ErrProjectNotOpen
	}
	meta, err := r.GetMeta(ctx, id)
	if err != nil {
		return err
	}
	if meta == nil {
		return nil
	}
	if receiptedDraftStatus(meta.Status) && status != meta.Status {
		return ErrDraftFinalizedReadOnly
	}
	if !receiptedDraftStatus(meta.Status) && receiptedDraftStatus(status) {
		return ErrDraftFinalizationRequiresCommit
	}
	if wordCount != nil {
		_, err = r.db.ExecContext(ctx,
			"UPDATE drafts SET status = ?, word_count = ?, updated_at = datetime('now') WHERE id = ?",
			status, *wordCount, id)
		return err
	}
	_, err = r.db.ExecContext(ctx,
		"UPDATE drafts SET status = ?, updated_at = datetime('now') WHERE id = ?", status, id)
	return err
}

// Delete mirrors DraftRepository.delete: the status guard, the draft DELETE and
// the best-effort content DELETE share one transaction, and a row that is
// already gone is a silent success. The content DELETE is deliberately allowed
// to fail, because the original swallows exactly that failure when a review or
// revision still references the row (SQLite RESTRICT).
func (r *Drafts) Delete(ctx context.Context, id int64) error {
	if r == nil || r.db == nil {
		return ErrProjectNotOpen
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status sql.NullString
	var contentID sql.NullInt64
	err = tx.QueryRowContext(ctx, "SELECT status, content_id FROM drafts WHERE id = ?", id).Scan(&status, &contentID)
	if errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if status.String == "finalized" {
		return ErrFinalizedDraftDeleteRequired
	}
	if status.String == "superseded" {
		return ErrSupersededDraftDeleteForbidden
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM drafts WHERE id = ?", id); err != nil {
		return err
	}
	if contentID.Valid {
		// Foreign-key guarded content is left behind on purpose.
		_, _ = tx.ExecContext(ctx, "DELETE FROM contents WHERE id = ?", contentID.Int64)
	}
	return tx.Commit()
}
