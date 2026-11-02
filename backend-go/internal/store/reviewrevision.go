package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

var (
	ErrReviewRevisionProjectNotOpen = errors.New("PROJECT_NOT_OPEN")
	ErrContentOwnershipConflict = errors.New("CONTENT_OWNERSHIP_CONFLICT")
	ErrSourceDraftChanged = errors.New("SOURCE_DRAFT_CHANGED")
	ErrReviewCycleOwnerRequired = errors.New("REVIEW_CYCLE_OWNER_REQUIRED")
	ErrReviewRevisionInvalidBinding = errors.New("REVIEW_REVISION_INVALID_BINDING")
	ErrReviewRevisionInvalidInput = errors.New("REVIEW_REVISION_INPUT_INVALID")
	ErrRevisionNotFound = errors.New("REVISION_NOT_FOUND")
	ErrRevisionTargetMismatch = errors.New("REVISION_TARGET_MISMATCH")
	ErrRevisionAlreadyMerged = errors.New("REVISION_ALREADY_MERGED")
	ErrRevisionNotPending = errors.New("REVISION_NOT_PENDING")
	ErrRevisionTargetNotEditable = errors.New("REVISION_TARGET_NOT_EDITABLE")
	ErrRevisionTargetChanged = errors.New("REVISION_TARGET_CHANGED")
	ErrRevisionSourceMissing = errors.New("REVISION_SOURCE_MISSING")
	ErrRevisionSourceMismatch = errors.New("REVISION_SOURCE_MISMATCH")
	ErrRevisionMergeRequired = errors.New("REVISION_MERGE_REQUIRED")
)

// ReviewRevisions borrows an admitted database. It does not open paths, migrate,
// own the connection pool, or implement the review-cycle owner's state machine.
// Maps retain explicit JSON nulls for nullable metadata and absent source snapshots.
type ReviewRevisions struct{ db *sql.DB }

func NewReviewRevisions(db *sql.DB) *ReviewRevisions { return &ReviewRevisions{db: db} }

// Manual transactions must stay on one reserved connection. In particular, a
// cancelled request cannot suppress rollback and leak an open transaction.
func (r *ReviewRevisions) immediate(ctx context.Context, fn func(*sql.Conn) error) (retErr error) {
	if r == nil || r.db == nil { return ErrReviewRevisionProjectNotOpen }
	conn, err := r.db.Conn(ctx)
	if err != nil { return err }
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil { return err }
	committed := false
	defer func() {
		if committed { return }
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("rollback review/revision: %w", err))
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if err := fn(conn); err != nil { return err }
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil { return err }
	committed = true
	return nil
}

// Match JavaScript's safe-integer range and never coerce strings, booleans,
// fractions, NaN, or arbitrary Go objects into SQLite parameters.
func rrInteger(value any, minimum int64) (int64, bool) {
	var n int64
	switch v := value.(type) {
	case int: n = int64(v)
	case int64: n = v
	case int32: n = int64(v)
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Trunc(v) != v || math.Abs(v) > 9007199254740991 { return 0, false }
		n = int64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil { return 0, false }
		return rrInteger(f, minimum)
	default: return 0, false
	}
	return n, n >= minimum && n <= 9007199254740991
}

func rrID(input map[string]any, key string) (int64, error) {
	id, ok := rrInteger(input[key], 1)
	if !ok { return 0, ErrReviewRevisionInvalidInput }
	return id, nil
}

func rrText(input map[string]any, key string) (string, error) {
	value, ok := input[key].(string)
	if !ok { return "", ErrReviewRevisionInvalidInput }
	return value, nil
}

type rrQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// Query and body are read in one statement/snapshot, including missing-body
// fallback. No separate content lookup can observe a different author save.
func rrMaps(ctx context.Context, q rrQueryer, query string, args ...any) ([]map[string]any, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil { return nil, err }
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil { return nil, err }
	result := make([]map[string]any, 0)
	for rows.Next() {
		values, ptrs := make([]any, len(columns)), make([]any, len(columns))
		for i := range values { ptrs[i] = &values[i] }
		if err := rows.Scan(ptrs...); err != nil { return nil, err }
		row := make(map[string]any, len(columns))
		for i, key := range columns { row[key] = values[i] }
		result = append(result, row)
	}
	if err := rows.Err(); err != nil { return nil, err }
	if err := rows.Close(); err != nil { return nil, err }
	return result, nil
}

const rrReviewMeta = `r.id AS id, r.base_draft_id AS baseDraftId,
 r.review_index AS reviewIndex, r.content_id AS contentId, r.created_at AS createdAt`
const rrRevisionMeta = `r.id AS id, r.base_draft_id AS baseDraftId,
 r.revision_index AS revisionIndex, r.revision_type AS revisionType, r.status AS status,
 r.merged_to_draft_id AS mergedToDraftId, COALESCE(r.user_prompt,'') AS userPrompt,
 r.review_source_id AS reviewSourceId, r.content_id AS contentId, r.word_count AS wordCount,
 r.created_at AS createdAt, r.updated_at AS updatedAt`
const rrSourceColumns = `, r.source_draft_chapter_number AS sourceChapter,
 r.source_draft_version AS sourceVersion, r.source_draft_status AS sourceStatus,
 r.source_content AS sourceContent, COALESCE(c.body,'') AS content`

func rrFull(rows []map[string]any, err error) (map[string]any, error) {
	if err != nil || len(rows) == 0 { return nil, err }
	row := rows[0]
	row["sourceDraft"] = nil
	chapter, chapterOK := row["sourceChapter"], rrNumber(row["sourceChapter"])
	version, versionOK := row["sourceVersion"], rrNumber(row["sourceVersion"])
	status, statusOK := row["sourceStatus"].(string)
	body, bodyOK := row["sourceContent"].(string)
	if chapterOK && versionOK && statusOK && bodyOK {
		row["sourceDraft"] = map[string]any{"id": row["baseDraftId"], "chapterNumber": chapter,
			"version": version, "status": status, "content": body}
	}
	for _, key := range []string{"sourceChapter", "sourceVersion", "sourceStatus", "sourceContent"} { delete(row, key) }
	return row, nil
}

func (r *ReviewRevisions) ListReviews(ctx context.Context, baseID int64) ([]map[string]any, error) {
	if r == nil || r.db == nil { return []map[string]any{}, nil }
	return rrMaps(ctx, r.db, "SELECT "+rrReviewMeta+" FROM reviews r WHERE r.base_draft_id=? ORDER BY r.review_index ASC", baseID)
}

func (r *ReviewRevisions) LatestReview(ctx context.Context, baseID int64) (map[string]any, error) {
	if r == nil || r.db == nil { return nil, nil }
	return rrFull(rrMaps(ctx, r.db, "SELECT "+rrReviewMeta+rrSourceColumns+` FROM reviews r
 LEFT JOIN contents c ON c.id=r.content_id WHERE r.base_draft_id=? ORDER BY r.review_index DESC LIMIT 1`, baseID))
}

func (r *ReviewRevisions) FullReview(ctx context.Context, id int64) (map[string]any, error) {
	if r == nil || r.db == nil { return nil, nil }
	return rrFull(rrMaps(ctx, r.db, "SELECT "+rrReviewMeta+rrSourceColumns+` FROM reviews r
 LEFT JOIN contents c ON c.id=r.content_id WHERE r.id=?`, id))
}

func (r *ReviewRevisions) ListRevisions(ctx context.Context, baseID int64, pending bool) ([]map[string]any, error) {
	if r == nil || r.db == nil { return []map[string]any{}, nil }
	filter := ""
	if pending { filter = " AND r.status='pending'" }
	return rrMaps(ctx, r.db, "SELECT "+rrRevisionMeta+" FROM revisions r WHERE r.base_draft_id=?"+filter+" ORDER BY r.revision_index ASC", baseID)
}

func (r *ReviewRevisions) FullRevision(ctx context.Context, id int64) (map[string]any, error) {
	if r == nil || r.db == nil { return nil, nil }
	return rrFull(rrMaps(ctx, r.db, "SELECT "+rrRevisionMeta+rrSourceColumns+` FROM revisions r
 LEFT JOIN contents c ON c.id=r.content_id WHERE r.id=?`, id))
}

func (r *ReviewRevisions) NextReviewIndex(ctx context.Context, baseID int64) (int64, error) {
	if r == nil || r.db == nil { return 1, nil }
	var n sql.NullInt64
	err := r.db.QueryRowContext(ctx, "SELECT MAX(review_index) FROM reviews WHERE base_draft_id=?", baseID).Scan(&n)
	return n.Int64+1, err
}

func (r *ReviewRevisions) NextRevisionIndex(ctx context.Context, baseID int64) (int64, error) {
	if r == nil || r.db == nil { return 1, nil }
	var n sql.NullInt64
	err := r.db.QueryRowContext(ctx, "SELECT MAX(revision_index) FROM revisions WHERE base_draft_id=?", baseID).Scan(&n)
	return n.Int64+1, err
}

type rrSource struct {
	id, chapter, version int64
	status, content string
}

func rrExpected(input map[string]any) (rrSource, error) {
	var source rrSource
	if input["expectedSource"] == nil { return source, ErrSourceDraftChanged }
	m, ok := input["expectedSource"].(map[string]any)
	if !ok { return source, ErrSourceDraftChanged }
	var err error
	if source.id, err = rrID(m, "id"); err != nil { return source, ErrSourceDraftChanged }
	if source.chapter, ok = rrInteger(m["chapterNumber"], 1); !ok { return source, ErrSourceDraftChanged }
	if source.version, ok = rrInteger(m["version"], 1); !ok { return source, ErrSourceDraftChanged }
	if source.status, err = rrText(m, "status"); err != nil { return source, ErrSourceDraftChanged }
	if source.content, err = rrText(m, "content"); err != nil { return source, ErrSourceDraftChanged }
	return source, nil
}

// Preserve SQLite storage classes: better-sqlite3 exposes BLOB as Buffer, not
// string. Scanning into string would silently authorize a non-string baseline.
func rrNumber(value any) bool {
	switch value.(type) { case int64, float64: return true }
	return false
}

func rrEqual(a, b any) bool {
	switch x := a.(type) {
	case nil: return b == nil
	case string: y, ok := b.(string); return ok && x == y
	case int64:
		switch y := b.(type) { case int64: return x == y; case float64: return float64(x) == y }
	case float64:
		switch y := b.(type) { case int64: return x == float64(y); case float64: return x == y }
	}
	return false
}

func rrAssertSource(ctx context.Context, conn *sql.Conn, baseID int64, expected rrSource) error {
	rows, err := rrMaps(ctx, conn, `SELECT d.id,d.chapter_number AS chapter,d.version,d.status,c.body
 FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=?`, baseID)
	if err != nil { return err }
	if len(rows) == 0 { return ErrSourceDraftChanged }
	actual := rows[0]
	if baseID != expected.id || !rrEqual(actual["id"], expected.id) || !rrEqual(actual["chapter"], expected.chapter) ||
		!rrEqual(actual["version"], expected.version) || !rrEqual(actual["status"], expected.status) || !rrEqual(actual["body"], expected.content) { return ErrSourceDraftChanged }
	return nil
}

func rrOwnerInput(input map[string]any) error {
	for _, key := range []string{"reviewCycleId", "generationRunHandle", "authorRecovery"} {
		if _, present := input[key]; present { return ErrReviewCycleOwnerRequired }
	}
	return nil
}

// Missing cycle tables are an explicit proof of no cycle binding on supported
// pre-cycle schemas. If the table exists but its query fails, fail closed.
func rrUnbound(ctx context.Context, conn *sql.Conn, revisionID, reviewID, baseID int64) error {
	// review_source_id is historically an unconstrained optional link: do not
	// invent existence/same-draft requirements or infer authority from it.
	// Still refuse links to either review role of a participating cycle.
	_ = baseID
	var exists int
	err := conn.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='review_cycles'").Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) { return nil }
	if err != nil { return err }
	err = conn.QueryRowContext(ctx, `SELECT 1 FROM review_cycles
 WHERE revision_id=? OR confirmation_review_id=? OR review_id=? LIMIT 1`, revisionID, reviewID, reviewID).Scan(&exists)
	if err == nil { return ErrReviewCycleOwnerRequired }
	if errors.Is(err, sql.ErrNoRows) { return nil }
	return err
}

func (r *ReviewRevisions) CreateReview(ctx context.Context, input map[string]any) (map[string]any, error) {
	if r == nil || r.db == nil { return nil, ErrReviewRevisionProjectNotOpen }
	if err := rrOwnerInput(input); err != nil { return nil, err }
	baseID, err := rrID(input, "baseDraftId")
	if err != nil { return nil, err }
	source, err := rrExpected(input)
	if err != nil { return nil, err }
	body, err := rrText(input, "content")
	if err != nil { return nil, err }
	var output map[string]any
	err = r.immediate(ctx, func(conn *sql.Conn) error {
		if err := rrAssertSource(ctx, conn, baseID, source); err != nil { return err }
		var index int64
		if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(review_index),0)+1 FROM reviews WHERE base_draft_id=?", baseID).Scan(&index); err != nil { return err }
		content, err := conn.ExecContext(ctx, "INSERT INTO contents(body) VALUES(?)", body)
		if err != nil { return err }
		contentID, err := content.LastInsertId()
		if err != nil { return err }
		result, err := conn.ExecContext(ctx, `INSERT INTO reviews(base_draft_id,review_index,
 source_draft_chapter_number,source_draft_version,source_draft_status,source_content,content_id)
 VALUES(?,?,?,?,?,?,?)`, baseID, index, source.chapter, source.version, source.status, source.content, contentID)
		if err != nil { return err }
		id, err := result.LastInsertId()
		if err != nil { return err }
		output = map[string]any{"id": id, "reviewIndex": index}
		return nil
	})
	if err != nil { return nil, err }
	return output, nil
}

func (r *ReviewRevisions) CreateRevision(ctx context.Context, input map[string]any) (map[string]any, error) {
	return r.createRevision(ctx, input, false)
}

func (r *ReviewRevisions) ReplacePending(ctx context.Context, input map[string]any) (map[string]any, error) {
	return r.createRevision(ctx, input, true)
}

func (r *ReviewRevisions) createRevision(ctx context.Context, input map[string]any, replace bool) (map[string]any, error) {
	if r == nil || r.db == nil { return nil, ErrReviewRevisionProjectNotOpen }
	if err := rrOwnerInput(input); err != nil { return nil, err }
	baseID, err := rrID(input, "baseDraftId")
	if err != nil { return nil, err }
	source, err := rrExpected(input)
	if err != nil { return nil, err }
	body, err := rrText(input, "content")
	if err != nil { return nil, err }
	kind, err := rrText(input, "revisionType")
	if err != nil || (kind != "refine" && kind != "review-fix") { return nil, ErrReviewRevisionInvalidInput }
	count, ok := rrInteger(input["wordCount"], 0)
	if !ok { return nil, ErrReviewRevisionInvalidInput }
	prompt := ""
	if input["userPrompt"] != nil {
		if prompt, err = rrText(input, "userPrompt"); err != nil { return nil, err }
	}
	var reviewID int64
	var reviewValue any
	if input["reviewSourceId"] != nil {
		if reviewID, err = rrID(input, "reviewSourceId"); err != nil { return nil, err }
		reviewValue = reviewID
	}
	var output map[string]any
	err = r.immediate(ctx, func(conn *sql.Conn) error {
		if err := rrAssertSource(ctx, conn, baseID, source); err != nil { return err }
		if err := rrUnbound(ctx, conn, 0, reviewID, baseID); err != nil { return err }
		discarded := make([]int64, 0)
		if replace {
			rows, err := rrMaps(ctx, conn, "SELECT id, review_source_id AS reviewSourceId FROM revisions WHERE base_draft_id=? AND status='pending' ORDER BY id", baseID)
			if err != nil { return err }
			for _, row := range rows {
				id, ok := row["id"].(int64)
				if !ok { return ErrReviewRevisionInvalidBinding }
				oldReview, err := rrStoredReviewID(row["reviewSourceId"])
				if err != nil { return err }
				// Original reconcile would detach generated cycles here. Only the
				// cycle owner may do that; never discard and silently skip it.
				if err := rrUnbound(ctx, conn, id, oldReview, baseID); err != nil { return err }
				discarded = append(discarded, id)
			}
		}
		var index int64
		if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision_index),0)+1 FROM revisions WHERE base_draft_id=?", baseID).Scan(&index); err != nil { return err }
		content, err := conn.ExecContext(ctx, "INSERT INTO contents(body) VALUES(?)", body)
		if err != nil { return err }
		contentID, err := content.LastInsertId()
		if err != nil { return err }
		result, err := conn.ExecContext(ctx, `INSERT INTO revisions(base_draft_id,revision_index,revision_type,
 user_prompt,review_source_id,source_draft_chapter_number,source_draft_version,source_draft_status,source_content,content_id,word_count)
 VALUES(?,?,?,?,?,?,?,?,?,?,?)`, baseID, index, kind, prompt, reviewValue, source.chapter, source.version, source.status, source.content, contentID, count)
		if err != nil { return err }
		id, err := result.LastInsertId()
		if err != nil { return err }
		output = map[string]any{"id": id, "revisionIndex": index}
		if replace {
			if _, err := conn.ExecContext(ctx, `UPDATE revisions SET status='discarded',updated_at=datetime('now')
 WHERE base_draft_id=? AND status='pending' AND id<>?`, baseID, id); err != nil { return err }
			output["discardedRevisionIds"] = discarded
		}
		return nil
	})
	if err != nil { return nil, err }
	return output, nil
}

func rrStoredReviewID(value any) (int64, error) {
	if value == nil { return 0, nil }
	id, ok := rrInteger(value, 1)
	if !ok { return 0, ErrReviewRevisionInvalidBinding }
	return id, nil
}

func rrReceipt(revisionID, targetID, count int64, target map[string]any, replay bool) map[string]any {
	return map[string]any{"revisionId": revisionID, "targetDraftId": targetID, "status": "revised",
		"wordCount": count, "idempotent": replay, "chapterNumber": target["chapter"], "version": target["version"]}
}

func (r *ReviewRevisions) MergeRevision(ctx context.Context, input map[string]any) (map[string]any, error) {
	if r == nil || r.db == nil { return nil, ErrReviewRevisionProjectNotOpen }
	if err := rrOwnerInput(input); err != nil { return nil, err }
	revisionID, err := rrID(input, "revisionId")
	if err != nil { return nil, err }
	targetID, err := rrID(input, "targetDraftId")
	if err != nil { return nil, err }
	expected, err := rrText(input, "expectedDraftContent")
	if err != nil { return nil, err }
	merged, err := rrText(input, "mergedContent")
	if err != nil { return nil, err }
	count, ok := rrInteger(input["wordCount"], 0)
	if !ok { return nil, ErrReviewRevisionInvalidInput }
	var output map[string]any
	err = r.immediate(ctx, func(conn *sql.Conn) error {
		targets, err := rrMaps(ctx, conn, `SELECT d.chapter_number AS chapter,d.version,d.status,
 d.word_count AS wordCount,d.content_id AS contentId,c.body FROM drafts d
 JOIN contents c ON c.id=d.content_id WHERE d.id=?`, targetID)
		if err != nil { return err }
		if len(targets) == 0 { return ErrDraftNotFound }
		target := targets[0]
		revisions, err := rrMaps(ctx, conn, `SELECT base_draft_id AS baseDraftId,status,merged_to_draft_id AS mergedToDraftId,
 source_draft_chapter_number AS sourceChapter,source_draft_version AS sourceVersion,
 source_draft_status AS sourceStatus,source_content AS sourceContent,review_source_id AS reviewSourceId
 FROM revisions WHERE id=?`, revisionID)
		if err != nil { return err }
		if len(revisions) == 0 { return ErrRevisionNotFound }
		revision := revisions[0]
		if !rrEqual(revision["baseDraftId"], targetID) { return ErrRevisionTargetMismatch }
		replay := revision["status"] == "merged"
		if replay {
			if !rrEqual(revision["mergedToDraftId"], targetID) || !rrEqual(target["body"], merged) || !rrEqual(target["status"], "revised") || !rrEqual(target["wordCount"], count) {
				return ErrRevisionAlreadyMerged
			}
		} else {
			if revision["status"] != "pending" { return ErrRevisionNotPending }
			status, _ := target["status"].(string)
			if !IsWorkingDraftStatus(status) { return ErrRevisionTargetNotEditable }
			if !rrEqual(target["body"], expected) { return ErrRevisionTargetChanged }
			if revision["sourceChapter"] == nil || revision["sourceVersion"] == nil || revision["sourceStatus"] == nil || revision["sourceContent"] == nil { return ErrRevisionSourceMissing }
			if !rrEqual(target["chapter"], revision["sourceChapter"]) || !rrEqual(target["version"], revision["sourceVersion"]) || !rrEqual(target["status"], revision["sourceStatus"]) || !rrEqual(target["body"], revision["sourceContent"]) { return ErrRevisionSourceMismatch }
		}
		reviewID, err := rrStoredReviewID(revision["reviewSourceId"])
		if err != nil { return err }
		// Explicitly prove the commitMergedRevision(bound:false) case. The
		// table's mere presence is NOT a reason to disable unbound revisions.
		if err := rrUnbound(ctx, conn, revisionID, reviewID, targetID); err != nil { return err }
		if !replay {
			// Content-pool aliases are legal in the historical schema. Refuse
			// mutation rather than overwriting another draft or saved report.
			if err := rrExclusiveDraftContent(ctx, conn, targetID, target["contentId"]); err != nil { return err }
			result, err := conn.ExecContext(ctx, "UPDATE contents SET body=? WHERE id=? AND body=?", merged, target["contentId"], expected)
			if err := rrAffected(result, err, ErrRevisionTargetChanged); err != nil { return err }
			result, err = conn.ExecContext(ctx, `UPDATE drafts SET status='revised',word_count=?,updated_at=datetime('now')
 WHERE id=? AND chapter_number=? AND version=? AND status=? AND content_id=?`, count, targetID, target["chapter"], target["version"], target["status"], target["contentId"])
			if err := rrAffected(result, err, ErrRevisionTargetChanged); err != nil { return err }
			result, err = conn.ExecContext(ctx, `UPDATE revisions SET status='merged',merged_to_draft_id=?,updated_at=datetime('now')
 WHERE id=? AND base_draft_id=? AND status='pending'`, targetID, revisionID, targetID)
			if err := rrAffected(result, err, ErrRevisionNotPending); err != nil { return err }
		}
		output = rrReceipt(revisionID, targetID, count, target, replay)
		return nil
	})
	if err != nil { return nil, err }
	return output, nil
}

// rrExclusiveDraftContent also protects the author-save CAS. Some supported
// synthetic/pre-review layouts have no report tables; only confirmed absence
// permits skipping those tables. All queries run under the caller's write lock.
func rrExclusiveDraftContent(ctx context.Context, conn *sql.Conn, draftID int64, contentID any) error {
	var aliases int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM drafts WHERE content_id=? AND id<>?", contentID, draftID).Scan(&aliases); err != nil { return err }
	if aliases != 0 { return ErrContentOwnershipConflict }
	for _, table := range []string{"reviews", "revisions"} {
		var exists int
		err := conn.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) { continue }
		if err != nil { return err }
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE content_id=?", contentID).Scan(&aliases); err != nil { return err }
		if aliases != 0 { return ErrContentOwnershipConflict }
	}
	return nil
}

func rrAffected(result sql.Result, err error, conflict error) error {
	if err != nil { return err }
	n, err := result.RowsAffected()
	if err != nil { return err }
	if n != 1 { return conflict }
	return nil
}

func (r *ReviewRevisions) MarkDiscarded(ctx context.Context, id int64) error {
	return r.mark(ctx, id, 0, false)
}

func (r *ReviewRevisions) MarkMerged(ctx context.Context, id, targetID int64) error {
	return r.mark(ctx, id, targetID, true)
}

func (r *ReviewRevisions) mark(ctx context.Context, id, targetID int64, merged bool) error {
	if r == nil || r.db == nil { return ErrReviewRevisionProjectNotOpen }
	if _, ok := rrInteger(id, 1); !ok { return ErrReviewRevisionInvalidInput }
	if merged { if _, ok := rrInteger(targetID, 1); !ok { return ErrReviewRevisionInvalidInput } }
	return r.immediate(ctx, func(conn *sql.Conn) error {
		// markMerged's cycle guard precedes its pending-status check in TS.
		if merged {
			if err := rrUnbound(ctx, conn, id, 0, 0); err != nil {
				if errors.Is(err, ErrReviewCycleOwnerRequired) { return ErrRevisionMergeRequired }
				return err
			}
		}
		rows, err := rrMaps(ctx, conn, "SELECT base_draft_id AS baseDraftId,status,review_source_id AS reviewSourceId FROM revisions WHERE id=?", id)
		if err != nil { return err }
		if len(rows) == 0 || rows[0]["status"] != "pending" { return ErrRevisionNotPending }
		baseID, ok := rows[0]["baseDraftId"].(int64)
		if !ok { return ErrReviewRevisionInvalidBinding }
		reviewID, err := rrStoredReviewID(rows[0]["reviewSourceId"])
		if err != nil { return err }
		if err := rrUnbound(ctx, conn, id, reviewID, baseID); err != nil { return err }
		if merged {
			result, err := conn.ExecContext(ctx, "UPDATE revisions SET status='merged',merged_to_draft_id=?,updated_at=datetime('now') WHERE id=? AND status='pending'", targetID, id)
			return rrAffected(result, err, ErrRevisionNotPending)
		}
		result, err := conn.ExecContext(ctx, "UPDATE revisions SET status='discarded',updated_at=datetime('now') WHERE id=? AND status='pending'", id)
		return rrAffected(result, err, ErrRevisionNotPending)
	})
}
