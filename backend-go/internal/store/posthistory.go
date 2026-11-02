package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
)

var (
	ErrPostProcessStepNotFound = errors.New("POST_PROCESS_STEP_NOT_FOUND")
	ErrPostProcessFinalizationSourceRequired = errors.New("POST_PROCESS_FINALIZATION_SOURCE_REQUIRED")
	ErrPostProcessFinalizationSourceChanged = errors.New("POST_PROCESS_FINALIZATION_SOURCE_CHANGED")
	ErrPostHistoryInvalidInput = errors.New("POST_HISTORY_INPUT_INVALID")
)

// PostHistory borrows an admitted database; it does not close, migrate, or open
// it. This is receipt/history storage only, not a post-process runner or a
// knowledge-store reader. Raw SQLite values (including BLOBs) remain raw.
type PostHistory struct {
	db *sql.DB
	newRunID func() (string, error)
}

func NewPostHistory(db *sql.DB) *PostHistory {
	return &PostHistory{db: db, newRunID: secureUUID}
}

// Match crypto.randomUUID's random RFC 4122 version-4 / variant-1 layout.
// Injection is deliberately private to store-package tests, never caller input.
func secureUUID() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil { return "", err }
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	text := hex.EncodeToString(bytes[:])
	return text[:8]+"-"+text[8:12]+"-"+text[12:16]+"-"+text[16:20]+"-"+text[20:], nil
}

func phText(input map[string]any, key string) (string, error) {
	value, ok := input[key].(string)
	if !ok { return "", ErrPostHistoryInvalidInput }
	return value, nil
}

// Number fields in the original LLM API are not restricted to nonnegative
// integers. Keep finite REALs and negatives, while rejecting unsafe bindings.
func phNumber(value any) (any, bool) {
	switch v := value.(type) {
	case int: return int64(v), true
	case int32: return int64(v), true
	case int64: return v, true
	case float64: return v, !math.IsNaN(v) && !math.IsInf(v, 0)
	case json.Number:
		n, err := v.Float64()
		return n, err == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
	}
	return nil, false
}

// JavaScript String(number) uses fixed notation for [1e-6,1e21) and
// unpadded exponents elsewhere. Source identity uses that exact chapter text.
func phNumberString(value any) string {
	var number float64
	switch v := value.(type) {
	case int64: number = float64(v)
	case float64: number = v
	default: return ""
	}
	if number == 0 { return "0" }
	absolute := math.Abs(number)
	if absolute >= 1e-6 && absolute < 1e21 { return strconv.FormatFloat(number, 'f', -1, 64) }
	text := strconv.FormatFloat(number, 'e', -1, 64)
	parts := strings.SplitN(text, "e", 2)
	exponent, _ := strconv.Atoi(parts[1])
	sign := "+"
	if exponent < 0 { sign = "-"; exponent = -exponent }
	return parts[0]+"e"+sign+strconv.Itoa(exponent)
}

func phTruthy(value any) bool {
	switch v := value.(type) {
	case nil: return false
	case bool: return v
	case string: return v != ""
	case int: return v != 0
	case int32: return v != 0
	case int64: return v != 0
	case float64: return v != 0 && !math.IsNaN(v)
	case json.Number:
		n, err := v.Float64()
		return err != nil || (n != 0 && !math.IsNaN(n))
	}
	return true
}

type phStep struct { key, label string; critical bool }

func phSteps(value any) ([]phStep, error) {
	var entries []any
	switch list := value.(type) {
	case []any: entries = list
	case []map[string]any:
		entries = make([]any, len(list))
		for i := range list { entries[i] = list[i] }
	default: return nil, ErrPostHistoryInvalidInput
	}
	steps := make([]phStep, 0, len(entries))
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok { return nil, ErrPostHistoryInvalidInput }
		key, err := phText(item, "key")
		if err != nil { return nil, err }
		label, err := phText(item, "label")
		if err != nil { return nil, err }
		critical, ok := item["critical"].(bool)
		if !ok { return nil, ErrPostHistoryInvalidInput }
		steps = append(steps, phStep{key: key, label: label, critical: critical})
	}
	return steps, nil
}

func (p *PostHistory) CreateRun(ctx context.Context, input map[string]any) (string, error) {
	if p == nil || p.db == nil { return "", ErrProjectNotOpen }
	kind, err := phText(input, "triggerSourceType")
	if err != nil { return "", err }
	sourceID, err := phText(input, "triggerSourceId")
	if err != nil { return "", err }
	label, err := phText(input, "sourceLabel")
	if err != nil { return "", err }
	steps, err := phSteps(input["steps"])
	if err != nil { return "", err }
	newID := p.newRunID
	if newID == nil { newID = secureUUID }
	runID, err := newID()
	if err != nil { return "", err }
	err = NewReviewRevisions(p.db).immediate(ctx, func(conn *sql.Conn) error {
		if !phTruthy(input["finalizedSource"]) {
			if strings.HasPrefix(sourceID, "finalization:") { return ErrPostProcessFinalizationSourceRequired }
		} else {
			finalizationID, err := phAssertSource(ctx, conn, kind, sourceID, input["finalizedSource"])
			if err != nil { return err }
			sourceID = "finalization:"+finalizationID
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO post_process_runs
 (id,trigger_source_type,trigger_source_id,source_label) VALUES(?,?,?,?)`, runID, kind, sourceID, label); err != nil { return err }
		for _, step := range steps {
			critical := 0
			if step.critical { critical = 1 }
			if _, err := conn.ExecContext(ctx, `INSERT INTO post_process_steps
 (run_id,step_key,label,critical) VALUES(?,?,?,?)`, runID, step.key, step.label, critical); err != nil { return err }
		}
		return nil
	})
	if err != nil { return "", err }
	return runID, nil
}

func phAssertSource(ctx context.Context, conn *sql.Conn, kind, sourceID string, value any) (string, error) {
	source, ok := value.(map[string]any)
	if !ok { return "", ErrPostProcessFinalizationSourceChanged }
	draftID, idOK := phNumber(source["draftId"])
	chapter, chapterOK := phNumber(source["chapterNumber"])
	finalizationID, finalOK := source["finalizationId"].(string)
	hash, hashOK := source["contentHash"].(string)
	if !idOK || !chapterOK || !finalOK || !hashOK || kind != "chapter_finalize" || sourceID != phNumberString(chapter) { return "", ErrPostProcessFinalizationSourceChanged }
	rows, err := rrMaps(ctx, conn, `SELECT d.id,d.chapter_number,d.status,c.body,
 o.finalization_id,o.content_hash,o.content_snapshot FROM drafts d
 JOIN contents c ON c.id=d.content_id JOIN finalization_outbox o ON o.draft_id=d.id WHERE d.id=?`, draftID)
	if err != nil { return "", err }
	latest, err := rrMaps(ctx, conn, `SELECT id FROM drafts WHERE chapter_number=? AND status='finalized'
 ORDER BY version DESC,id DESC LIMIT 1`, chapter)
	if err != nil { return "", err }
	if len(rows) == 0 || len(latest) == 0 { return "", ErrPostProcessFinalizationSourceChanged }
	row := rows[0]
	body, bodyOK := row["body"].(string)
	if !bodyOK || !rrEqual(row["id"], draftID) || !rrEqual(row["id"], latest[0]["id"]) || !rrEqual(row["chapter_number"], chapter) ||
		!rrEqual(row["status"], "finalized") || !rrEqual(row["finalization_id"], finalizationID) ||
		!rrEqual(row["content_hash"], hash) || !rrEqual(row["content_snapshot"], body) { return "", ErrPostProcessFinalizationSourceChanged }
	digest := sha256.Sum256([]byte(body))
	if hex.EncodeToString(digest[:]) != hash { return "", ErrPostProcessFinalizationSourceChanged }
	return finalizationID, nil
}

func phDigits(text string) bool {
	if text == "" { return false }
	for _, c := range text { if c < '0' || c > '9' { return false } }
	return true
}

func (p *PostHistory) LatestRun(ctx context.Context, kind, sourceID string) (map[string]any, error) {
	if p == nil || p.db == nil { return nil, nil }
	if kind == "chapter_finalize" && phDigits(sourceID) {
		tables, err := rrMaps(ctx, p.db, "SELECT 1 FROM sqlite_master WHERE type='table' AND name='finalization_outbox'")
		if err != nil { return nil, err }
		if len(tables) != 0 {
			// JS Number(sourceId) accepts leading zeros and rounds very large IDs.
			number, err := strconv.ParseFloat(sourceID, 64)
			if err != nil && !math.IsInf(number, 0) { return nil, err }
			current, err := rrMaps(ctx, p.db, `SELECT o.finalization_id FROM drafts d
 JOIN finalization_outbox o ON o.draft_id=d.id WHERE d.chapter_number=? AND d.status='finalized'
 ORDER BY d.version DESC,d.id DESC LIMIT 1`, number)
			if err != nil { return nil, err }
			if len(current) != 0 { if id, ok := current[0]["finalization_id"].(string); ok { sourceID = "finalization:"+id } }
		}
	}
	rows, err := rrMaps(ctx, p.db, `SELECT id,trigger_source_type AS triggerSourceType,
 trigger_source_id AS triggerSourceId,source_label AS sourceLabel,all_critical_passed AS allCriticalPassed,
 created_at AS createdAt,updated_at AS updatedAt FROM post_process_runs
 WHERE trigger_source_type=? AND trigger_source_id=? ORDER BY created_at DESC,rowid DESC LIMIT 1`, kind, sourceID)
	if err != nil || len(rows) == 0 { return nil, err }
	rows[0]["allCriticalPassed"] = rrEqual(rows[0]["allCriticalPassed"], int64(1))
	return rows[0], nil
}

func (p *PostHistory) Steps(ctx context.Context, runID string) ([]map[string]any, error) {
	if p == nil || p.db == nil { return []map[string]any{}, nil }
	rows, err := rrMaps(ctx, p.db, `SELECT id,run_id AS runId,step_key AS stepKey,label,
 critical,ok,error_msg AS errorMsg,attempt_count AS attemptCount,
 completed_at AS completedAt,last_attempt_at AS lastAttemptAt
 FROM post_process_steps WHERE run_id=? ORDER BY id ASC`, runID)
	if err != nil { return nil, err }
	for _, row := range rows {
		row["critical"] = rrEqual(row["critical"], int64(1))
		row["ok"] = rrEqual(row["ok"], int64(1))
		for _, key := range []string{"label", "errorMsg", "completedAt", "lastAttemptAt"} {
			if row[key] == nil { row[key] = "" }
		}
	}
	return rows, nil
}

func (p *PostHistory) MarkStepOK(ctx context.Context, runID, key string) error {
	return p.markStep(ctx, runID, key, "", true)
}

func (p *PostHistory) MarkStepFailed(ctx context.Context, runID, key, message string) error {
	return p.markStep(ctx, runID, key, message, false)
}

func (p *PostHistory) markStep(ctx context.Context, runID, key, message string, ok bool) error {
	if p == nil || p.db == nil { return nil }
	return NewReviewRevisions(p.db).immediate(ctx, func(conn *sql.Conn) error {
		query := `UPDATE post_process_steps SET ok=0,error_msg=?,completed_at='',
 last_attempt_at=datetime('now'),attempt_count=attempt_count+1 WHERE run_id=? AND step_key=?`
		args := []any{message, runID, key}
		if ok {
			query = `UPDATE post_process_steps SET ok=1,error_msg='',completed_at=datetime('now'),
 last_attempt_at=datetime('now'),attempt_count=attempt_count+1 WHERE run_id=? AND step_key=?`
			args = []any{runID, key}
		}
		result, err := conn.ExecContext(ctx, query, args...)
		if err := rrAffected(result, err, ErrPostProcessStepNotFound); err != nil { return err }
		var failedCritical int64
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM post_process_steps
 WHERE run_id=? AND critical=1 AND ok=0`, runID).Scan(&failedCritical); err != nil { return err }
		allPassed := 0
		if failedCritical == 0 { allPassed = 1 }
		_, err = conn.ExecContext(ctx, `UPDATE post_process_runs
 SET all_critical_passed=?,updated_at=datetime('now') WHERE id=?`, allPassed, runID)
		return err
	})
}

func (p *PostHistory) AllPassed(ctx context.Context, kind, sourceID string) (bool, error) {
	run, err := p.LatestRun(ctx, kind, sourceID)
	if err != nil || run == nil { return false, err }
	passed, _ := run["allCriticalPassed"].(bool)
	return passed, nil
}

func (p *PostHistory) FailedStepLabels(ctx context.Context, runID string) ([]any, error) {
	if p == nil || p.db == nil { return []any{}, nil }
	rows, err := rrMaps(ctx, p.db, "SELECT label FROM post_process_steps WHERE run_id=? AND critical=1 AND ok=0", runID)
	if err != nil { return nil, err }
	labels := make([]any, 0, len(rows))
	for _, row := range rows { labels = append(labels, row["label"]) }
	return labels, nil
}

func (p *PostHistory) LogCall(ctx context.Context, input map[string]any) error {
	if p == nil || p.db == nil { return nil }
	values := make([]any, 0, 9)
	for _, key := range []string{"modelId", "modelName", "purpose"} {
		value, err := phText(input, key)
		if err != nil { return err }
		values = append(values, value)
	}
	for _, key := range []string{"promptTokens", "completionTokens", "totalTokens", "durationMs"} {
		if input[key] == nil && key != "durationMs" { values = append(values, nil); continue }
		value, ok := phNumber(input[key])
		if !ok { return ErrPostHistoryInvalidInput }
		values = append(values, value)
	}
	success, ok := input["success"].(bool)
	if !ok { return ErrPostHistoryInvalidInput }
	flag := 0
	if success { flag = 1 }
	values = append(values, flag)
	message := ""
	if input["errorMessage"] != nil {
		var err error
		message, err = phText(input, "errorMessage")
		if err != nil { return err }
	}
	values = append(values, message)
	_, err := p.db.ExecContext(ctx, `INSERT INTO llm_calls(model_id,model_name,purpose,prompt_tokens,
 completion_tokens,total_tokens,duration_ms,success,error_message) VALUES(?,?,?,?,?,?,?,?,?)`, values...)
	return err
}

func (p *PostHistory) Stats(ctx context.Context) (map[string]any, error) {
	if p == nil || p.db == nil {
		return map[string]any{"totalCalls": int64(0), "successfulCalls": int64(0), "failedCalls": int64(0),
			"knownUsageCalls": int64(0), "totalTokens": nil, "totalPromptTokens": nil, "totalCompletionTokens": nil}, nil
	}
	rows, err := rrMaps(ctx, p.db, `SELECT COUNT(*) AS totalCalls,
 SUM(CASE WHEN success=1 THEN 1 ELSE 0 END) AS successfulCalls,
 SUM(CASE WHEN success=0 THEN 1 ELSE 0 END) AS failedCalls,
 SUM(CASE WHEN total_tokens IS NOT NULL THEN 1 ELSE 0 END) AS knownUsageCalls,
 SUM(total_tokens) AS totalTokens,SUM(prompt_tokens) AS totalPromptTokens,
 SUM(completion_tokens) AS totalCompletionTokens FROM llm_calls`)
	if err != nil { return nil, err }
	row := rows[0]
	for _, key := range []string{"successfulCalls", "failedCalls", "knownUsageCalls"} {
		if row[key] == nil { row[key] = int64(0) }
	}
	return row, nil
}

func (p *PostHistory) History(ctx context.Context, limit int64) ([]map[string]any, error) {
	if p == nil || p.db == nil { return []map[string]any{}, nil }
	return rrMaps(ctx, p.db, `SELECT id,model_id AS modelId,model_name AS modelName,purpose,
 prompt_tokens AS promptTokens,completion_tokens AS completionTokens,total_tokens AS totalTokens,
 duration_ms AS durationMs,success,CASE
 WHEN success=1 THEN 'stop'
 WHEN error_message='finish:length' THEN 'length'
 WHEN error_message='finish:content_filter' THEN 'content_filter'
 WHEN error_message IN ('finish:cancelled','cancelled') THEN 'cancelled'
 WHEN error_message='finish:error' THEN 'error'
 WHEN error_message='finish:unknown' THEN 'unknown'
 ELSE NULL END AS finishReason,created_at AS createdAt
 FROM llm_calls ORDER BY id DESC LIMIT ?`, limit)
}
