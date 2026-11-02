// Package store provides repositories over an already admitted project database.
// It never creates tables, migrates schemas, or changes journal configuration.
package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrDraftContentConflict = errors.New("DRAFT_CONTENT_CONFLICT")
	ErrDraftNotFound        = errors.New("DRAFT_NOT_FOUND")
)

// IsWorkingDraftStatus deliberately fails closed for unknown statuses.
// Keep this allowlist in sync with src/shared/draft-status.ts.
func IsWorkingDraftStatus(status string) bool {
	return status == "draft" || status == "revised" || status == "reviewed"
}

type DraftSourceDependency struct {
	DraftID        int64  `json:"draftId"`
	ContentHash    string `json:"contentHash"`
	Kind           string `json:"kind,omitempty"`
	ChapterNumber  *int64 `json:"chapterNumber,omitempty"`
	FinalizationID string `json:"finalizationId,omitempty"`
}

type DraftMeta struct {
	ID                 int64                   `json:"id"`
	ChapterNumber      int64                   `json:"chapterNumber"`
	ChapterTitle       string                  `json:"chapterTitle,omitempty"`
	Version            int64                   `json:"version"`
	Status             string                  `json:"status"`
	Source             string                  `json:"source"`
	ContentID          int64                   `json:"contentId"`
	WordCount          int64                   `json:"wordCount"`
	SourceDependencies []DraftSourceDependency `json:"sourceDependencies"`
	DependenciesStale  bool                    `json:"dependenciesStale"`
	CreatedAt          string                  `json:"createdAt"`
	UpdatedAt          string                  `json:"updatedAt"`
}

type DraftFull struct {
	DraftMeta
	Content string `json:"content"`
}

// Drafts borrows its database; ownership and closing remain with the caller.
// NewDrafts performs no I/O. The caller must validate/admit the project schema
// separately; missing current-layout columns produce SQL errors, not migrations
// or a legacy-layout fallback.
type Drafts struct{ db *sql.DB }

func NewDrafts(db *sql.DB) *Drafts { return &Drafts{db: db} }

const draftMetaSelect = `SELECT d.id, d.chapter_number, d.version, d.status,
 d.source, d.content_id, d.word_count, d.source_dependencies,
 d.created_at, d.updated_at, f.chapter_title
 FROM drafts d LEFT JOIN finalization_outbox f
 ON d.status IN ('finalized', 'superseded') AND f.draft_id = d.id`

type draftRow struct {
	meta  DraftMeta
	valid bool
}

// readMetas uses a single read snapshot for metadata and transitive dependencies.
func readMetas(ctx context.Context, tx *sql.Tx, suffix string, args ...any) ([]DraftMeta, error) {
	rows, err := tx.QueryContext(ctx, draftMetaSelect+suffix, args...)
	if err != nil {
		return nil, err
	}
	parsed := make([]draftRow, 0)
	ids := make([]int64, 0)
	for rows.Next() {
		var row draftRow
		var status, source, dependencies, created, updated, title sql.NullString
		var count sql.NullInt64
		m := &row.meta
		err = rows.Scan(&m.ID, &m.ChapterNumber, &m.Version, &status, &source,
			&m.ContentID, &count, &dependencies, &created, &updated, &title)
		if err != nil {
			rows.Close()
			return nil, err
		}
		m.Status, m.Source, m.WordCount = status.String, source.String, count.Int64
		m.CreatedAt, m.UpdatedAt = created.String, updated.String
		if trimJS(title.String) != "" {
			m.ChapterTitle = title.String
		}
		m.SourceDependencies, row.valid = parseDependencies(dependencies)
		for _, dep := range m.SourceDependencies {
			ids = append(ids, dep.DraftID)
		}
		parsed = append(parsed, row)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	states, err := loadDependencyStates(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	metas := make([]DraftMeta, 0, len(parsed))
	memo := make(map[int64]bool)
	for _, row := range parsed {
		m := row.meta
		m.DependenciesStale = !row.valid || !dependenciesCurrent(m.SourceDependencies, states, memo)
		metas = append(metas, m)
	}
	return metas, nil
}

func (r *Drafts) list(ctx context.Context, suffix string, args ...any) ([]DraftMeta, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	metas, err := readMetas(ctx, tx, suffix, args...)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return metas, nil
}

func (r *Drafts) ListByChapter(ctx context.Context, chapterNumber int64) ([]DraftMeta, error) {
	return r.list(ctx, " WHERE d.chapter_number = ? ORDER BY d.version ASC", chapterNumber)
}

func (r *Drafts) ListAll(ctx context.Context) ([]DraftMeta, error) {
	return r.list(ctx, " ORDER BY d.chapter_number ASC, d.version ASC")
}

// GetMeta and GetFull return nil, nil when the draft does not exist.
func (r *Drafts) GetMeta(ctx context.Context, id int64) (*DraftMeta, error) {
	metas, err := r.list(ctx, " WHERE d.id = ?", id)
	if err != nil || len(metas) == 0 {
		return nil, err
	}
	return &metas[0], nil
}

func (r *Drafts) GetFull(ctx context.Context, id int64) (*DraftFull, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	metas, err := readMetas(ctx, tx, " WHERE d.id = ?", id)
	if err != nil {
		return nil, err
	}
	var full *DraftFull
	if len(metas) != 0 {
		full = &DraftFull{DraftMeta: metas[0]}
		var body sql.NullString
		err = tx.QueryRowContext(ctx, "SELECT body FROM contents WHERE id = ?", full.ContentID).Scan(&body)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		// Match ContentRepository.getBody(...) ?? '' for missing content rows.
		full.Content = body.String
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return full, nil
}

// UpdateContentIfCurrent is the only author-save path. A nil baseline is a
// conflict, never permission to read a replacement baseline from the server.
// Empty baselines are valid; equality is exact, without Unicode normalization.
// BEGIN IMMEDIATE, checks, and both writes use the SAME reserved connection.
// This does not depend on the caller remembering the driver's _txlock option.
func (r *Drafts) UpdateContentIfCurrent(ctx context.Context, id int64, content string, wordCount int64, expectedContent *string) (retErr error) {
	if expectedContent == nil {
		return ErrDraftContentConflict
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
		// Request cancellation must not prevent rollback or return a connection
		// with an open manual transaction to the pool.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("rollback draft save: %w", err))
			// A trigger may already have rolled back, but discard on any uncertain
			// rollback outcome rather than potentially leaking transaction state.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	var status sql.NullString
	var contentID int64
	var body string
	err = conn.QueryRowContext(ctx, `SELECT d.status, d.content_id, c.body
 FROM drafts d JOIN contents c ON c.id = d.content_id WHERE d.id = ?`, id).Scan(&status, &contentID, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrDraftNotFound
	}
	if err != nil {
		return err
	}
	if !IsWorkingDraftStatus(status.String) || body != *expectedContent {
		return ErrDraftContentConflict
	}
	if err := updateOne(ctx, conn, "UPDATE contents SET body = ? WHERE id = ?", content, contentID); err != nil {
		return err
	}
	if err := updateOne(ctx, conn, `UPDATE drafts SET word_count = ?, updated_at = datetime('now') WHERE id = ?`, wordCount, id); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

func updateOne(ctx context.Context, conn *sql.Conn, query string, args ...any) error {
	result, err := conn.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("draft save affected %d rows instead of one", count)
	}
	return nil
}

// ECMAScript trim includes BOM, unlike strings.TrimSpace.
func trimJS(value string) string {
	return strings.Trim(value, "\u0009\u000a\u000b\u000c\u000d\u0020\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff")
}
