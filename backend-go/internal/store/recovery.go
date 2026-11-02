package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	ErrRecoveryProjectNotOpen     = errors.New("PROJECT_NOT_OPEN")
	ErrRecoveryIdentityInvalid    = errors.New("RECOVERY_CANDIDATE_IDENTITY_INVALID")
	ErrRecoveryChapterInvalid     = errors.New("RECOVERY_CANDIDATE_CHAPTER_INVALID")
	ErrRecoverySourceMismatch     = errors.New("RECOVERY_CANDIDATE_SOURCE_MISMATCH")
	ErrRecoverySourceDraftInvalid = errors.New("RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID")
	ErrRecoveryTextEmpty          = errors.New("RECOVERY_CANDIDATE_TEXT_EMPTY")
	ErrRecoveryReplacementInvalid = errors.New("RECOVERY_CANDIDATE_REPLACEMENT_INVALID")
	ErrRecoveryIntegrityFailed    = errors.New("RECOVERY_CANDIDATE_INTEGRITY_FAILED")
	ErrRecoveryNotPending         = errors.New("RECOVERY_CANDIDATE_NOT_PENDING")
	ErrRecoverySourceChanged      = errors.New("RECOVERY_CANDIDATE_SOURCE_CHANGED")
	ErrRecoveryActionInvalid      = errors.New("RECOVERY_CANDIDATE_ACTION_INVALID")
	ErrRecoveryInputUnsupported   = errors.New("RECOVERY_CANDIDATE_INPUT_UNSUPPORTED")
)

// Recovery borrows an admitted baseline-v7 database, never performs DDL, and
// owns no generation authority. Record's projectId must be injected by the
// validated session/HTTP parent, never copied from raw renderer RPC input.
// Portable/frozen projects are rejected by admission; no frozen-row exemption
// is inferred here from untrusted or historical row metadata.
type Recovery struct{ db *sql.DB }

func NewRecovery(db *sql.DB) *Recovery { return &Recovery{db: db} }

const recoveryLimit = 32 << 20

var recoveryThinking = regexp.MustCompile(`(?is)<think>.*?(?:</think>|$)`)
var recoveryClosing = regexp.MustCompile(`(?i)</think>`)
var recoveryTags = regexp.MustCompile(`(?i)</?think>`)

func recoveryVisible(text string) string {
	text = recoveryThinking.ReplaceAllString(text, "")
	if loc := recoveryClosing.FindStringIndex(text); loc != nil {
		text = text[loc[1]:]
	}
	return strings.TrimFunc(recoveryTags.ReplaceAllString(text, ""), narrativeSpace)
}
func recoveryText(value any) (string, error) {
	text, ok := value.(string)
	if !ok || !utf8.ValidString(text) || len(text) > recoveryLimit {
		return "", ErrRecoveryInputUnsupported
	}
	return text, nil
}
func recoveryIdentity(value any) (string, error) {
	text, err := recoveryText(value)
	if err != nil {
		return "", err
	}
	text = strings.TrimFunc(text, narrativeSpace)
	if text == "" || narrativeLength(text) > 160 {
		return "", ErrRecoveryIdentityInvalid
	}
	return text, nil
}

// JS slice counts UTF-16 code units. Splitting a surrogate pair is outside the
// native UTF-8 contract: reject, rather than silently substituting U+FFFD.
func recoverySlice(text string, limit int) (string, error) {
	units := 0
	for i, r := range text {
		width := 1
		if r > 0xffff {
			width = 2
		}
		if units+width > limit {
			if units < limit {
				return "", ErrRecoveryInputUnsupported
			}
			return text[:i], nil
		}
		units += width
	}
	return text, nil
}
func recoveryHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

type recoverySource struct {
	chapter                         int64
	title, role, purpose, keyEvents string
	characters                      []any
	suspenseHook, userGuidance      string
}

func recoveryNormalizeSource(input map[string]any) (recoverySource, error) {
	var source recoverySource
	var ok bool
	source.chapter, ok = rrInteger(input["chapterNumber"], 1)
	if !ok {
		return source, ErrRecoveryInputUnsupported
	}
	for _, field := range []struct {
		key    string
		target *string
	}{{"title", &source.title}, {"role", &source.role}, {"purpose", &source.purpose}, {"keyEvents", &source.keyEvents}, {"suspenseHook", &source.suspenseHook}, {"userGuidance", &source.userGuidance}} {
		value := input[field.key]
		if value == nil && (field.key == "suspenseHook" || field.key == "userGuidance") {
			value = ""
		}
		text, err := recoveryText(value)
		if err != nil {
			return source, err
		}
		*field.target = text
	}
	switch values := input["characters"].(type) {
	case []string:
		source.characters = make([]any, len(values))
		for i, value := range values {
			source.characters[i] = value
		}
	case []any:
		source.characters = values
	default:
		return source, ErrRecoveryInputUnsupported
	}
	total := 0
	for _, value := range source.characters {
		text, err := recoveryText(value)
		if err != nil {
			return source, err
		}
		total += len(text)
		if total > recoveryLimit {
			return source, ErrRecoveryInputUnsupported
		}
	}
	return source, nil
}
func (s recoverySource) serialized() (string, error) {
	encoded, err := plotJSON(plotObject{{"chapterNumber", s.chapter}, {"title", s.title}, {"role", s.role}, {"purpose", s.purpose}, {"keyEvents", s.keyEvents}, {"characters", s.characters}, {"suspenseHook", s.suspenseHook}, {"userGuidance", s.userGuidance}})
	if err != nil {
		return "", err
	}
	if len(encoded) > recoveryLimit {
		return "", ErrRecoveryInputUnsupported
	}
	return encoded, nil
}
func recoveryCurrent(ctx context.Context, conn *sql.Conn, row map[string]any) (bool, error) {
	if !rrEqual(row["source_draft_identity_captured"], int64(1)) {
		return false, nil
	}
	rows, err := blueprintRows(ctx, conn, "WHERE chapter_number=?", row["chapter_number"])
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	source, err := recoveryNormalizeSource(rows[0])
	if err != nil {
		return false, err
	}
	serialized, err := source.serialized()
	if err != nil {
		return false, err
	}
	if recoveryHash(serialized) != row["source_hash"] {
		return false, nil
	}
	// DraftRepository.getLatestByChapter returns metadata only. A body/status
	// edit on the same id/version does NOT invalidate this original identity check.
	drafts, err := rrMaps(ctx, conn, "SELECT id,version FROM drafts WHERE chapter_number=? ORDER BY version DESC LIMIT 1", row["chapter_number"])
	if err != nil {
		return false, err
	}
	id, version := row["source_draft_id"], row["source_draft_version"]
	if id == nil || version == nil {
		return id == nil && version == nil && len(drafts) == 0, nil
	}
	return len(drafts) > 0 && rrEqual(drafts[0]["id"], id) && rrEqual(drafts[0]["version"], version), nil
}
func recoveryCandidate(ctx context.Context, conn *sql.Conn, row map[string]any) (map[string]any, error) {
	for _, pair := range [][2]string{{"source_snapshot", "source_hash"}, {"visible_text", "content_hash"}} {
		text, ok := row[pair[0]].(string)
		if !ok || !utf8.ValidString(text) || recoveryHash(text) != row[pair[1]] {
			return nil, ErrRecoveryIntegrityFailed
		}
	}
	current, err := recoveryCurrent(ctx, conn, row)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"sourceCurrent": current}
	for _, pair := range [][2]string{{"candidateId", "candidate_id"}, {"runId", "run_id"}, {"stepId", "step_id"}, {"projectId", "project_id"}, {"chapterNumber", "chapter_number"}, {"chapterTitle", "chapter_title"}, {"sourceHash", "source_hash"}, {"visibleText", "visible_text"}, {"contentHash", "content_hash"}, {"failureCode", "failure_code"}, {"failureReason", "failure_reason"}, {"status", "status"}, {"replacesCandidateId", "replaces_candidate_id"}, {"createdAt", "created_at"}, {"resolvedAt", "resolved_at"}} {
		out[pair[0]] = row[pair[1]]
	}
	return out, nil
}
func recoveryRow(ctx context.Context, conn *sql.Conn, id string, pending bool) (map[string]any, error) {
	suffix := ""
	if pending {
		suffix = " AND status='pending'"
	}
	rows, err := rrMaps(ctx, conn, "SELECT * FROM recovery_candidates WHERE candidate_id=?"+suffix, id)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, ErrRecoveryNotPending
	}
	return rows[0], nil
}
func (r *Recovery) Record(ctx context.Context, input map[string]any) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, ErrRecoveryProjectNotOpen
	}
	run, err := recoveryIdentity(input["runId"])
	if err != nil {
		return nil, err
	}
	step, err := recoveryIdentity(input["stepId"])
	if err != nil {
		return nil, err
	}
	project, err := recoveryIdentity(input["projectId"])
	if err != nil {
		return nil, err
	}
	chapter, ok := rrInteger(input["chapterNumber"], 1)
	if !ok {
		return nil, ErrRecoveryChapterInvalid
	}
	sourceInput, ok := input["source"].(map[string]any)
	if !ok {
		return nil, ErrRecoveryInputUnsupported
	}
	sourceChapter, ok := rrInteger(sourceInput["chapterNumber"], 1)
	if !ok {
		return nil, ErrRecoveryInputUnsupported
	}
	if sourceChapter != chapter {
		return nil, ErrRecoverySourceMismatch
	}
	var draftID, draftVersion any
	draft, present := input["sourceDraft"]
	if !present {
		return nil, ErrRecoverySourceDraftInvalid
	}
	if draft != nil {
		m, ok := draft.(map[string]any)
		if !ok {
			return nil, ErrRecoverySourceDraftInvalid
		}
		id, idOK := rrInteger(m["id"], 1)
		version, versionOK := rrInteger(m["version"], 1)
		if !idOK || !versionOK {
			return nil, ErrRecoverySourceDraftInvalid
		}
		draftID, draftVersion = id, version
	}
	text, err := recoveryText(input["visibleText"])
	if err != nil {
		return nil, err
	}
	text = recoveryVisible(text)
	if text == "" {
		return nil, ErrRecoveryTextEmpty
	}
	source, err := recoveryNormalizeSource(sourceInput)
	if err != nil {
		return nil, err
	}
	serialized, err := source.serialized()
	if err != nil {
		return nil, err
	}
	title, err := recoveryText(input["chapterTitle"])
	if err != nil {
		return nil, err
	}
	title = strings.TrimFunc(title, narrativeSpace)
	code, err := recoveryText(input["failureCode"])
	if err != nil {
		return nil, err
	}
	code, err = recoverySlice(strings.TrimFunc(code, narrativeSpace), 160)
	if err != nil {
		return nil, err
	}
	reason, err := recoveryText(input["failureReason"])
	if err != nil {
		return nil, err
	}
	reason, err = recoverySlice(strings.TrimFunc(reason, narrativeSpace), 2000)
	if err != nil {
		return nil, err
	}
	var replacement any
	if input["replacesCandidateId"] != nil {
		value, err := recoveryText(input["replacesCandidateId"])
		if err != nil {
			return nil, err
		}
		replacement = value
	}
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return nil, err
	}
	bytes[6] = (bytes[6] & 15) | 64
	bytes[8] = (bytes[8] & 63) | 128
	id := fmt.Sprintf("%x-%x-%x-%x-%x", bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
	var output map[string]any
	err = NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		if replacement != nil && replacement != "" {
			rows, err := rrMaps(ctx, conn, "SELECT run_id,project_id,chapter_number FROM recovery_candidates WHERE candidate_id=?", replacement)
			if err != nil {
				return err
			}
			if len(rows) == 0 || rows[0]["run_id"] != run || rows[0]["project_id"] != project || !rrEqual(rows[0]["chapter_number"], chapter) {
				return ErrRecoveryReplacementInvalid
			}
		}
		_, err := conn.ExecContext(ctx, `INSERT INTO recovery_candidates(candidate_id,run_id,step_id,project_id,chapter_number,chapter_title,source_snapshot,source_hash,source_draft_id,source_draft_version,source_draft_identity_captured,visible_text,content_hash,failure_code,failure_reason,replaces_candidate_id) VALUES(?,?,?,?,?,?,?,?,?,?,1,?,?,?,?,?)`, id, run, step, project, chapter, title, serialized, recoveryHash(serialized), draftID, draftVersion, text, recoveryHash(text), code, reason, replacement)
		if err != nil {
			return err
		}
		row, err := recoveryRow(ctx, conn, id, false)
		if err != nil {
			return err
		}
		expected := map[string]any{"candidate_id": id, "run_id": run, "step_id": step, "project_id": project, "chapter_number": chapter, "chapter_title": title, "source_snapshot": serialized, "source_hash": recoveryHash(serialized), "source_draft_id": draftID, "source_draft_version": draftVersion, "source_draft_identity_captured": int64(1), "visible_text": text, "content_hash": recoveryHash(text), "failure_code": code, "failure_reason": reason, "replaces_candidate_id": replacement, "status": "pending", "resolved_at": nil}
		for key, value := range expected {
			if !rrEqual(row[key], value) {
				return ErrRecoveryIntegrityFailed
			}
		}
		output, err = recoveryCandidate(ctx, conn, row)
		return err
	})
	if err != nil {
		return nil, err
	}
	return output, nil
}
func (r *Recovery) ListPending(ctx context.Context) ([]map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, ErrRecoveryProjectNotOpen
	}
	output := make([]map[string]any, 0)
	err := NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		rows, err := rrMaps(ctx, conn, "SELECT * FROM recovery_candidates WHERE status='pending' ORDER BY rowid ASC")
		if err != nil {
			return err
		}
		for _, row := range rows {
			candidate, err := recoveryCandidate(ctx, conn, row)
			if err != nil {
				return err
			}
			output = append(output, candidate)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return output, nil
}
func (r *Recovery) UpdatePending(ctx context.Context, id, text string) (map[string]any, error) {
	if r == nil || r.db == nil {
		return nil, ErrRecoveryProjectNotOpen
	}
	id, err := recoveryIdentity(id)
	if err != nil {
		return nil, err
	}
	var output map[string]any
	err = NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		row, err := recoveryRow(ctx, conn, id, true)
		if err != nil {
			return err
		}
		candidate, err := recoveryCandidate(ctx, conn, row)
		if err != nil {
			return err
		}
		if candidate["sourceCurrent"] != true {
			return ErrRecoverySourceChanged
		}
		before := row
		text, err := recoveryText(text)
		if err != nil {
			return err
		}
		text = recoveryVisible(text)
		if text == "" {
			return ErrRecoveryTextEmpty
		}
		result, err := conn.ExecContext(ctx, "UPDATE recovery_candidates SET visible_text=?,content_hash=? WHERE candidate_id=? AND status='pending'", text, recoveryHash(text), id)
		if err := rrAffected(result, err, ErrRecoveryNotPending); err != nil {
			return err
		}
		row, err = recoveryRow(ctx, conn, id, true)
		if err != nil {
			return err
		}
		if row["visible_text"] != text || row["content_hash"] != recoveryHash(text) {
			return ErrRecoveryIntegrityFailed
		}
		for key, value := range before {
			if key != "visible_text" && key != "content_hash" && !rrEqual(row[key], value) {
				return ErrRecoveryIntegrityFailed
			}
		}
		output, err = recoveryCandidate(ctx, conn, row)
		if err != nil {
			return err
		}
		if output["sourceCurrent"] != true {
			return ErrRecoverySourceChanged
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return output, nil
}
func (r *Recovery) Resolve(ctx context.Context, id, status string) error {
	if status != "continued" && status != "discarded" {
		return ErrRecoveryActionInvalid
	}
	if r == nil || r.db == nil {
		return ErrRecoveryProjectNotOpen
	}
	if _, err := recoveryText(id); err != nil {
		return err
	}
	return NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
		// Deliberately NOT toCandidate/integrity: original continued checks only
		// source identity; discarded permits stale or corrupt candidates for cleanup.
		if status == "continued" {
			row, err := recoveryRow(ctx, conn, id, true)
			if err != nil {
				return err
			}
			current, err := recoveryCurrent(ctx, conn, row)
			if err != nil {
				return err
			}
			if !current {
				return ErrRecoverySourceChanged
			}
		}
		result, err := conn.ExecContext(ctx, "UPDATE recovery_candidates SET status=?,resolved_at=datetime('now') WHERE candidate_id=? AND status='pending'", status, id)
		if err := rrAffected(result, err, ErrRecoveryNotPending); err != nil {
			return err
		}
		row, err := recoveryRow(ctx, conn, id, false)
		if err != nil {
			return err
		}
		if row["status"] != status || row["resolved_at"] == nil {
			return ErrRecoveryNotPending
		}
		return nil
	})
}
